use anyhow::{Context, Result, bail, ensure};
use reqwest::{
    Client, Response, Url,
    dns::{Addrs, Name, Resolve, Resolving},
    redirect::Policy,
};
use rustix::fs::{Mode, OFlags, open, openat};
use serde::Deserialize;
use serde_json::{Value, json};
use std::{
    fs::File,
    io::{self, Read},
    net::{IpAddr, Ipv4Addr, Ipv6Addr},
    path::{Component, Path},
    time::Duration,
};

use crate::{config, store};

const DOCUMENT_LIMIT: usize = 10 * 1024 * 1024;
const API_RESPONSE_LIMIT: usize = 32 * 1024 * 1024;

pub struct Fetcher {
    client: Client,
    allow_private: bool,
}

impl Fetcher {
    pub fn new(config: &config::Fetcher) -> Result<Self> {
        let allow_private = config.allow_private;
        let mut client = Client::builder()
            .no_proxy()
            .timeout(Duration::from_secs(config.timeout_seconds))
            .redirect(Policy::custom(move |attempt| {
                if attempt.previous().len() >= 5 {
                    attempt.error("リダイレクト上限を超えました")
                } else if let Err(error) = validate_target(attempt.url(), allow_private) {
                    attempt.error(error.to_string())
                } else {
                    attempt.follow()
                }
            }));
        if !allow_private {
            client = client.dns_resolver(PublicDns);
        }
        Ok(Self {
            client: client.build()?,
            allow_private,
        })
    }

    pub async fn fetch(&self, url: &str) -> Result<String> {
        let url = Url::parse(url).context("文書 URL が不正です")?;
        validate_target(&url, self.allow_private)?;
        let response = self
            .client
            .get(url)
            .send()
            .await
            .map_err(reqwest::Error::without_url)?;
        ensure!(
            response.status().is_success(),
            "文書取得に失敗しました (HTTP {})",
            response.status()
        );
        if let Some(content_type) = response.headers().get(reqwest::header::CONTENT_TYPE) {
            ensure!(
                content_type
                    .to_str()?
                    .to_ascii_lowercase()
                    .starts_with("text/"),
                "文書の Content-Type が text/ ではありません"
            );
        }
        let bytes = limited_body(response, DOCUMENT_LIMIT).await?;
        Ok(String::from_utf8_lossy(&bytes).into_owned())
    }
}

struct PublicDns;

impl Resolve for PublicDns {
    fn resolve(&self, name: Name) -> Resolving {
        Box::pin(async move {
            // reqwest が接続に使うアドレス自体を検査し、同じ DNS 応答を返す。
            let addresses: Vec<_> = tokio::net::lookup_host((name.as_str(), 0)).await?.collect();
            if addresses.is_empty() || addresses.iter().any(|address| !is_public(address.ip())) {
                return Err(io::Error::new(
                    io::ErrorKind::PermissionDenied,
                    "文書 URL の接続先が公開 IP ではありません",
                )
                .into());
            }
            Ok(Box::new(addresses.into_iter()) as Addrs)
        })
    }
}

fn validate_target(url: &Url, allow_private: bool) -> Result<()> {
    ensure!(
        matches!(url.scheme(), "http" | "https")
            && url.username().is_empty()
            && url.password().is_none(),
        "文書 URL は認証情報を含まない HTTP(S) URL を指定してください"
    );
    let host = url.host_str().context("文書 URL にホストがありません")?;
    if !allow_private && let Ok(ip) = host.trim_matches(['[', ']']).parse::<IpAddr>() {
        ensure!(is_public(ip), "文書 URL の接続先が公開 IP ではありません");
    }
    Ok(())
}

fn is_public(ip: IpAddr) -> bool {
    match ip {
        IpAddr::V4(ip) => public_ipv4(ip),
        IpAddr::V6(ip) => public_ipv6(ip),
    }
}

fn public_ipv4(ip: Ipv4Addr) -> bool {
    !matches!(
        ip.octets(),
        [0 | 10 | 127 | 224..=255, ..]
            | [100, 64..=127, ..]
            | [169, 254, ..]
            | [172, 16..=31, ..]
            | [192, 0, 0 | 2, _]
            | [192, 88, 99, _]
            | [192, 168, ..]
            | [198, 18 | 19, ..]
            | [198, 51, 100, _]
            | [203, 0, 113, _]
    )
}

fn public_ipv6(ip: Ipv6Addr) -> bool {
    if let Some(mapped) = ip.to_ipv4_mapped() {
        return public_ipv4(mapped);
    }
    let [first, second, ..] = ip.segments();
    // グローバルユニキャストの中でも、トンネル・文書用・特殊用途を除く。
    (0x2000..=0x3fff).contains(&first)
        && !matches!(
            (first, second),
            (0x2001, 0..=0x1ff | 0xdb8) | (0x2002 | 0x3fff, _)
        )
}

async fn limited_body(mut response: Response, limit: usize) -> Result<Vec<u8>> {
    ensure!(
        response
            .content_length()
            .is_none_or(|length| length <= limit as u64),
        "HTTP 本文の上限を超えました"
    );
    let mut bytes = Vec::new();
    while let Some(chunk) = response
        .chunk()
        .await
        .map_err(reqwest::Error::without_url)?
    {
        ensure!(
            chunk.len() <= limit - bytes.len(),
            "HTTP 本文の上限を超えました"
        );
        bytes.extend_from_slice(&chunk);
    }
    Ok(bytes)
}

pub fn read_local(path: &Path) -> Result<String> {
    ensure!(
        path.is_absolute() && !path.components().any(|part| part == Component::ParentDir),
        "local_path は親参照を含まない絶対パスを指定してください"
    );
    let canonical = path.canonicalize().context("local_path を解決できません")?;
    let directories = OFlags::RDONLY | OFlags::DIRECTORY | OFlags::NOFOLLOW | OFlags::CLOEXEC;
    let mut directory = open("/", directories, Mode::empty())?;
    let mut components = canonical
        .components()
        .filter_map(|component| match component {
            Component::Normal(name) => Some(name),
            _ => None,
        })
        .peekable();
    // 解決済みパスの各階層をディレクトリ FD に対して開き、再解決時のすり替えを防ぐ。
    let file = loop {
        let name = components
            .next()
            .context("local_path がファイルを指定していません")?;
        if components.peek().is_none() {
            break File::from(openat(
                &directory,
                name,
                OFlags::RDONLY | OFlags::NONBLOCK | OFlags::NOFOLLOW | OFlags::CLOEXEC,
                Mode::empty(),
            )?);
        }
        directory = openat(&directory, name, directories, Mode::empty())?;
    };
    let metadata = file.metadata()?;
    ensure!(
        metadata.is_file() && metadata.len() <= DOCUMENT_LIMIT as u64,
        "local_path は 10 MiB 以下の通常ファイルを指定してください"
    );
    let mut bytes = Vec::new();
    file.take(DOCUMENT_LIMIT as u64 + 1)
        .read_to_end(&mut bytes)?;
    ensure!(
        bytes.len() <= DOCUMENT_LIMIT,
        "local_path のサイズ上限を超えました"
    );
    Ok(String::from_utf8_lossy(&bytes).into_owned())
}

/// OpenAI API が成功以外の HTTP 状態を返した。
#[derive(Debug)]
pub struct ApiStatus(pub u16);

impl std::fmt::Display for ApiStatus {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "OpenAI API が HTTP {} を返しました", self.0)
    }
}

impl std::error::Error for ApiStatus {}

/// 再試行を尽くしても OpenAI から Embedding を得られなかった。
/// 認証・通信・HTTP エラー・応答の不正を含む。MCP エラーの識別子 `EMBEDDING_FAILED` に対応する。
#[derive(Debug)]
pub struct EmbeddingFailed(anyhow::Error);

impl EmbeddingFailed {
    /// 原因に含まれる OpenAI API の HTTP 状態コード。
    pub fn http_status(&self) -> Option<u16> {
        self.0
            .chain()
            .find_map(|cause| cause.downcast_ref::<ApiStatus>())
            .map(|status| status.0)
    }
}

// 既存クライアントが読む `embed: ...` の文言を変えないため、原因の文言をそのまま表示する。
impl std::fmt::Display for EmbeddingFailed {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{:#}", self.0)
    }
}

impl std::error::Error for EmbeddingFailed {}

pub struct OpenAi {
    client: Client,
    base_url: Url,
    api_key: String,
    embed_model: String,
    embed_dim: usize,
    embed_timeout: Duration,
    rerank_model: String,
    rerank_timeout: Duration,
}

#[derive(Deserialize)]
struct Embeddings {
    data: Vec<EmbeddingRow>,
}

#[derive(Deserialize)]
struct EmbeddingRow {
    index: usize,
    embedding: Vec<f32>,
}

impl OpenAi {
    pub fn new(cfg: &config::Config) -> Result<Self> {
        let api_key = ["OPENAI_API_DOCDB_KEY", "OPENAI_API_KEY"]
            .into_iter()
            .filter_map(|name| std::env::var(name).ok())
            .find(|value| !value.is_empty())
            .context("OPENAI_API_DOCDB_KEY または OPENAI_API_KEY が必要です")?;
        Self::connect(cfg, api_key, Url::parse("https://api.openai.com/v1/")?)
    }

    fn connect(cfg: &config::Config, api_key: String, base_url: Url) -> Result<Self> {
        let client = Client::builder()
            .https_only(base_url.scheme() == "https")
            .redirect(Policy::none())
            .build()?;
        Ok(Self {
            client,
            base_url,
            api_key,
            embed_model: cfg.embedding.model.clone(),
            embed_dim: cfg.embedding.dim,
            embed_timeout: Duration::from_secs(cfg.embedding.timeout_seconds),
            rerank_model: cfg.rerank.model.clone(),
            rerank_timeout: Duration::from_secs(cfg.rerank.timeout_seconds),
        })
    }

    #[cfg(test)]
    pub fn mock(cfg: &config::Config, url: Url) -> Result<Self> {
        Self::connect(cfg, "test-credential".to_owned(), url)
    }

    async fn send(
        &self,
        endpoint: &'static str,
        payload: &Value,
        timeout: Duration,
    ) -> Result<Vec<u8>> {
        let response = self
            .client
            .post(self.base_url.join(endpoint)?)
            .bearer_auth(&self.api_key)
            .json(payload)
            .timeout(timeout)
            .send()
            .await?;
        if !response.status().is_success() {
            return Err(ApiStatus(response.status().as_u16()).into());
        }
        limited_body(response, API_RESPONSE_LIMIT).await
    }

    pub async fn embed(&self, texts: &[&str]) -> Result<Vec<Vec<f32>>> {
        let payload =
            json!({"model": self.embed_model, "input": texts, "encoding_format": "float"});
        let mut last_error = None;
        for attempt in 0..3 {
            if attempt > 0 {
                tokio::time::sleep(Duration::from_secs(1 << (attempt - 1))).await;
            }
            match self.embed_once(&payload, texts.len()).await {
                Ok(vectors) => return Ok(vectors),
                Err(error) => last_error = Some(error),
            }
        }
        let error =
            last_error.unwrap_or_else(|| anyhow::anyhow!("Embedding API を呼び出せませんでした"));
        Err(EmbeddingFailed(error).into())
    }

    async fn embed_once(&self, payload: &Value, count: usize) -> Result<Vec<Vec<f32>>> {
        let body = self.send("embeddings", payload, self.embed_timeout).await?;
        let reply: Embeddings = serde_json::from_slice(&body)?;
        let mut ordered = vec![Vec::new(); count];
        for row in reply.data {
            ensure!(
                row.embedding.len() == self.embed_dim
                    && row.embedding.iter().all(|value| value.is_finite()),
                "Embedding の値または次元数が不正です"
            );
            let target = ordered
                .get_mut(row.index)
                .context("Embedding 応答の index が範囲外です")?;
            ensure!(
                target.is_empty(),
                "Embedding 応答に重複する index があります"
            );
            *target = row.embedding;
        }
        ensure!(
            ordered.iter().all(|vector| !vector.is_empty()),
            "Embedding 応答に欠落があります"
        );
        Ok(ordered)
    }

    pub async fn rerank(
        &self,
        query: &str,
        chunks: &[store::Chunk],
        order: &[usize],
    ) -> Result<Vec<f64>> {
        if order.is_empty() {
            return Ok(Vec::new());
        }
        let candidates: Vec<_> = order
            .iter()
            .enumerate()
            .map(|(id, &index)| {
                let chunk = &chunks[index];
                let preview = format!("{}\n{}", chunk.heading_path, chunk.text);
                let preview = preview.trim();
                let tokens: Vec<_> = preview.split_ascii_whitespace().take(201).collect();
                let preview = if tokens.len() > 200 {
                    tokens[..200].join(" ")
                } else {
                    preview.to_owned()
                };
                json!({"id": id.to_string(), "preview": preview})
            })
            .collect();
        let input = json!({"query": query, "candidates": candidates}).to_string();
        let payload = json!({
            "model": self.rerank_model,
            "response_format": {"type": "json_object"},
            "temperature": 0,
            "messages": [
                {"role": "system", "content": "You rerank candidates by relevance to the query. Return JSON: {\"ranking\":[{\"id\":\"...\",\"score\":0..1}, ...]} with all ids included exactly once."},
                {"role": "user", "content": input}
            ]
        });
        let body = self
            .send("chat/completions", &payload, self.rerank_timeout)
            .await?;
        let reply: Value = serde_json::from_slice(&body)?;
        let Some(content) = reply
            .pointer("/choices/0/message/content")
            .and_then(Value::as_str)
        else {
            bail!("Rerank の応答にメッセージがありません");
        };
        let result: Value = serde_json::from_str(content)?;
        let rows = result
            .get("ranking")
            .and_then(Value::as_array)
            .context("Rerank の応答に ranking がありません")?;
        let mut scores = vec![-1.0; order.len()];
        for row in rows {
            let id = row["id"]
                .as_str()
                .and_then(|id| id.parse().ok())
                .or_else(|| row["id"].as_u64().and_then(|id| usize::try_from(id).ok()));
            let score = row["score"]
                .as_f64()
                .filter(|score| (0.0..=1.0).contains(score));
            match (id.and_then(|id| scores.get_mut(id)), score) {
                (Some(target), Some(score)) => *target = score,
                _ => tracing::warn!("Rerank が不正な候補識別子またはスコアを返しました"),
            }
        }
        Ok(scores)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn rejects_non_public_addresses_and_unsafe_urls() {
        for address in [
            "127.0.0.1",
            "100.64.0.1",
            "169.254.169.254",
            "::1",
            "::ffff:127.0.0.1",
            "fe80::1",
            "fc00::1",
            "2001:db8::1",
        ] {
            assert!(!is_public(address.parse().unwrap()), "{address}");
        }
        for address in ["8.8.8.8", "2606:4700::1111"] {
            assert!(is_public(address.parse().unwrap()), "{address}");
        }
        for url in [
            "file:///tmp/test",
            "http://user:password@example.com",
            "http://127.0.0.1",
        ] {
            assert!(validate_target(&Url::parse(url).unwrap(), false).is_err());
        }
    }
}
