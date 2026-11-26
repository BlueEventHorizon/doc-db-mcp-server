use anyhow::{Result, ensure};
use regex::Regex;
use serde::Serialize;
use std::{
    collections::HashMap,
    sync::{Arc, LazyLock},
};
use unicode_normalization::UnicodeNormalization;

use crate::{config, network::OpenAi, store::Chunk};

#[derive(Clone, Copy, PartialEq)]
pub enum Mode {
    Emb,
    Lex,
    Grep,
    Hybrid,
    All,
    Rerank,
}

impl Mode {
    pub fn parse(value: &str) -> Result<Self> {
        Ok(match value {
            "emb" => Self::Emb,
            "lex" => Self::Lex,
            "grep" => Self::Grep,
            "hybrid" => Self::Hybrid,
            "" | "all" => Self::All,
            "rerank" => Self::Rerank,
            _ => anyhow::bail!("検索モードが不正です"),
        })
    }

    pub fn needs_embedding(self) -> bool {
        matches!(self, Self::Emb | Self::Hybrid | Self::All | Self::Rerank)
    }
}

#[derive(Default, Serialize)]
pub struct Scores {
    emb: f64,
    lex: f64,
    grep: f64,
    rrf: f64,
    rerank: f64,
}

#[derive(Default, Serialize)]
pub struct Stats {
    emb_candidates: usize,
    lex_candidates: usize,
    grep_candidates: usize,
    fused_candidates: usize,
    merged_candidates: usize,
    rerank_candidates: usize,
}

#[derive(Serialize)]
pub struct Hit {
    path: String,
    heading_path: String,
    text: String,
    score: f64,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    origin_signals: Vec<&'static str>,
    score_breakdown: Scores,
    series_keys: Option<Vec<String>>,
}

#[derive(Default, Serialize)]
pub struct SearchResult {
    results: Vec<Hit>,
    stage_stats: Stats,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub warnings: Vec<String>,
}

pub async fn run(
    chunks: Vec<Chunk>,
    query: String,
    mode: Mode,
    top: usize,
    cfg: &config::Bm25,
    api: &OpenAi,
) -> Result<SearchResult> {
    if chunks.is_empty() {
        return Ok(SearchResult::default());
    }

    // CPU で行う語彙検索と全文照合を Embedding API の待機中に実行する。
    let chunks = Arc::new(chunks);
    let lexical_chunks = chunks.clone();
    let lexical_query = query.clone();
    let (k1, b) = (cfg.k1, if cfg.b == 0.0 { 0.75 } else { cfg.b });
    let lexical = tokio::task::spawn_blocking(move || {
        lexical_scores(&lexical_chunks, &lexical_query, mode, k1, b)
    });
    let embedding = async {
        if mode.needs_embedding() {
            let vectors = api.embed(&[&query]).await?;
            ensure!(vectors.len() == 1, "検索クエリの Embedding がありません");
            Ok::<_, anyhow::Error>(vectors.into_iter().next())
        } else {
            Ok(None)
        }
    };
    let (lexical, embedding) = tokio::join!(lexical, embedding);
    let (lex, grep) = lexical?;
    let embedding = embedding?;
    let embedding_chunks = chunks.clone();
    let emb = tokio::task::spawn_blocking(move || match embedding {
        Some(vector) => embedding_chunks
            .iter()
            .map(|chunk| cosine(&vector, &chunk.vector))
            .collect(),
        None => vec![0.0; embedding_chunks.len()],
    })
    .await?;

    let mut stats = Stats {
        emb_candidates: count_nonzero(&emb),
        lex_candidates: count_nonzero(&lex),
        grep_candidates: count_nonzero(&grep),
        ..Stats::default()
    };
    let emb_rank = if mode.needs_embedding() {
        rank(&emb, false)
    } else {
        Vec::new()
    };
    let lex_rank = rank(&lex, true);
    let grep_rank = rank(&grep, true);
    let mut origins = vec![Vec::new(); chunks.len()];
    let mut rrf = vec![0.0; chunks.len()];
    let mut warnings = Vec::new();
    let mut order = match mode {
        Mode::Emb => {
            set_origins(&mut origins, &emb_rank, "emb");
            emb_rank
        }
        Mode::Lex => {
            set_origins(&mut origins, &lex_rank, "lex");
            lex_rank
        }
        Mode::Grep => {
            set_origins(&mut origins, &grep_rank, "grep");
            grep_rank
        }
        Mode::Hybrid => {
            let fallback = stats.emb_candidates > 0
                && stats.lex_candidates as f64 / (stats.emb_candidates as f64) < 0.05;
            let order = if fallback {
                warnings.push(format!(
                    "emb fallback triggered (lex_hits={} / emb_hits={} < 0.05)",
                    stats.lex_candidates, stats.emb_candidates
                ));
                rrf.clone_from(&emb);
                emb_rank.clone()
            } else {
                rrf = fuse(&emb_rank, &lex_rank, chunks.len());
                rank(&rrf, false)
            };
            for &index in &order {
                if emb[index] > 0.0 {
                    origins[index].push("emb");
                }
                if lex[index] > 0.0 {
                    origins[index].push("lex");
                }
            }
            stats.fused_candidates = order.len();
            order
        }
        Mode::All | Mode::Rerank => {
            let cap = top.max(30);
            let emb_end = emb_rank
                .iter()
                .take(cap)
                .take_while(|&&index| emb[index] > 0.0)
                .count();
            set_origins(&mut origins, &emb_rank[..emb_end], "emb");
            set_origins(&mut origins, &lex_rank[..lex_rank.len().min(cap)], "lex");
            set_origins(&mut origins, &grep_rank, "grep");
            let mut order: Vec<_> = (0..chunks.len())
                .filter(|&i| !origins[i].is_empty())
                .collect();
            order.sort_unstable_by(|&a, &b| {
                origins[b]
                    .len()
                    .cmp(&origins[a].len())
                    .then_with(|| emb[b].total_cmp(&emb[a]))
                    .then_with(|| a.cmp(&b))
            });
            stats.merged_candidates = order.len();
            order
        }
    };
    let mut rerank = vec![0.0; chunks.len()];
    if mode == Mode::Rerank && !order.is_empty() {
        let count = order.len().min(top.max(30));
        match api.rerank(&query, &chunks, &order[..count]).await {
            Ok(scores) => {
                for (&index, score) in order[..count].iter().zip(scores) {
                    rerank[index] = score;
                }
                order[..count].sort_unstable_by(|&a, &b| {
                    rerank[b]
                        .total_cmp(&rerank[a])
                        .then_with(|| emb[b].total_cmp(&emb[a]))
                        .then_with(|| a.cmp(&b))
                });
                stats.rerank_candidates = count;
            }
            Err(error) => {
                tracing::warn!(%error, "Rerank に失敗したため元の順序を保持します");
                warnings.push(format!("rerank fallback to RRF: {error}"));
            }
        }
    }

    let results = order
        .into_iter()
        .take(top)
        .map(|index| {
            let chunk = &chunks[index];
            let scores = Scores {
                emb: emb[index],
                lex: lex[index],
                grep: grep[index],
                rrf: rrf[index],
                rerank: rerank[index],
            };
            let score = if scores.rerank > 0.0 {
                scores.rerank
            } else {
                match mode {
                    Mode::Emb => scores.emb,
                    Mode::Lex => scores.lex,
                    Mode::Grep => scores.grep,
                    Mode::All => scores.emb.max(scores.lex).max(scores.grep),
                    Mode::Hybrid | Mode::Rerank => scores.rrf,
                }
            };
            Hit {
                path: chunk.path.clone(),
                heading_path: chunk.heading_path.clone(),
                text: chunk.text.clone(),
                score,
                origin_signals: std::mem::take(&mut origins[index]),
                score_breakdown: scores,
                series_keys: chunk.series_keys.as_ref().map(|keys| keys.to_vec()),
            }
        })
        .collect();
    Ok(SearchResult {
        results,
        stage_stats: stats,
        warnings,
    })
}

fn set_origins(origins: &mut [Vec<&'static str>], order: &[usize], signal: &'static str) {
    for &index in order {
        origins[index].push(signal);
    }
}

fn rank(scores: &[f64], positive: bool) -> Vec<usize> {
    let mut order: Vec<_> = (0..scores.len())
        .filter(|&i| !positive || scores[i] > 0.0)
        .collect();
    order.sort_unstable_by(|&a, &b| scores[b].total_cmp(&scores[a]).then_with(|| a.cmp(&b)));
    order
}

fn count_nonzero(scores: &[f64]) -> usize {
    scores.iter().filter(|&&score| score > 0.0).count()
}

fn fuse(emb_rank: &[usize], lex_rank: &[usize], len: usize) -> Vec<f64> {
    let mut scores = vec![0.0; len];
    for order in [emb_rank, lex_rank] {
        for (place, &index) in order.iter().enumerate() {
            scores[index] += 1.0 / (61 + place) as f64;
        }
    }
    let count = emb_rank.len().min(5);
    let top_emb = &emb_rank[..count];
    let order = rank(&scores, false);
    let top_fused = &order[..count];
    if let Some(&first_intruder) = top_fused.iter().find(|index| !top_emb.contains(index)) {
        let threshold = scores[first_intruder];
        for (place, &index) in top_emb.iter().enumerate() {
            if !top_fused.contains(&index) {
                scores[index] = threshold + (count - place) as f64 * 1e-9;
            }
        }
    }
    scores
}

fn cosine(left: &[f32], right: &[f32]) -> f64 {
    if left.len() != right.len() {
        return 0.0;
    }
    let (mut dot, mut a_norm, mut b_norm) = (0.0, 0.0, 0.0);
    for (&a, &b) in left.iter().zip(right) {
        let (a, b) = (f64::from(a), f64::from(b));
        dot += a * b;
        a_norm += a * a;
        b_norm += b * b;
    }
    let value = dot / (a_norm.sqrt() * b_norm.sqrt());
    if value.is_finite() { value } else { 0.0 }
}

static TOKENS: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"[a-z]+-[0-9]+|[a-z0-9_]+|[^\x00-\x7F]+").expect("固定トークン式")
});
static ID: LazyLock<Regex> = LazyLock::new(|| Regex::new(r"[a-z]+-[0-9]+").expect("固定 ID 式"));

fn normalize(value: &str) -> String {
    value
        .nfkc()
        .map(|character| character.to_lowercase().next().unwrap_or(character))
        .collect()
}

fn lexical_scores(
    chunks: &[Chunk],
    query: &str,
    mode: Mode,
    k1: f64,
    b: f64,
) -> (Vec<f64>, Vec<f64>) {
    let need_lex = matches!(mode, Mode::Lex | Mode::Hybrid | Mode::All | Mode::Rerank);
    let need_grep = matches!(mode, Mode::Grep | Mode::All | Mode::Rerank);
    let mut lex = vec![0.0; chunks.len()];
    let mut grep = vec![0.0; chunks.len()];
    if !need_lex && !need_grep {
        return (lex, grep);
    }
    let query = normalize(query);
    let literal = query.trim();
    let bodies: Vec<_> = chunks.iter().map(|chunk| normalize(&chunk.text)).collect();
    if need_grep && !literal.is_empty() {
        for (score, body) in grep.iter_mut().zip(&bodies) {
            *score = body.matches(literal).count() as f64;
        }
    }
    if need_lex {
        let terms: Vec<_> = TOKENS.find_iter(&query).map(|item| item.as_str()).collect();
        if terms.is_empty() {
            return (lex, grep);
        }
        let ids: Vec<_> = ID.find_iter(&query).map(|item| item.as_str()).collect();
        let lengths: Vec<_> = bodies.iter().map(|body| body.chars().count()).collect();
        let count = chunks.len() as f64;
        let avg = (lengths.iter().sum::<usize>() as f64 / count).max(f64::MIN_POSITIVE);
        let mut idf = HashMap::new();
        for &term in &terms {
            idf.entry(term).or_insert_with(|| {
                let df = bodies.iter().filter(|body| body.contains(term)).count() as f64;
                ((count - df + 0.5) / (df + 0.5) + 1.0).ln()
            });
        }
        for ((score, body), length) in lex.iter_mut().zip(&bodies).zip(lengths) {
            for &term in &terms {
                let tf = body.matches(term).count() as f64;
                if tf > 0.0 {
                    let normalized_tf =
                        tf * (k1 + 1.0) / (tf + k1 * (1.0 - b + b * length as f64 / avg));
                    *score += idf[term] * normalized_tf;
                }
            }
            *score += ids.iter().filter(|&&id| body.contains(id)).count() as f64 * 10.0;
            if body.contains(&query) {
                *score += 2.0;
            }
        }
    }
    (lex, grep)
}
