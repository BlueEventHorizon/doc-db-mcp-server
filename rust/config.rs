use anyhow::{Context, Result, ensure};
use serde::Deserialize;
use std::{
    fs,
    path::{Path, PathBuf},
};

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Config {
    pub server: Server,
    pub embedding: Embedding,
    pub rerank: Rerank,
    pub chunker: Chunker,
    pub bm25: Bm25,
    pub fetcher: Fetcher,
    pub trash: Trash,
    #[serde(default)]
    pub log: Log,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Server {
    pub port: u16,
    pub db_path: String,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Embedding {
    pub model: String,
    pub dim: usize,
    pub timeout_seconds: u64,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Rerank {
    pub model: String,
    pub factor: usize,
    pub timeout_seconds: u64,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Chunker {
    pub max_chunk_size: usize,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Bm25 {
    pub k1: f64,
    #[serde(default)]
    pub b: f64,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Fetcher {
    pub timeout_seconds: u64,
    #[serde(default)]
    pub allow_private: bool,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Trash {
    pub retention_days: u32,
    pub interval_seconds: u64,
}

#[derive(Default, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct Log {
    pub path: String,
    pub level: String,
}

impl Config {
    pub fn load() -> Result<(PathBuf, Self)> {
        let home = std::env::home_dir().context("ホームディレクトリを解決できません")?;
        let path = home.join(".doc-db/doc-db.yaml");
        let yaml =
            fs::read(&path).with_context(|| format!("設定を読み込めません: {}", path.display()))?;
        let config = Self::parse(&yaml, &home)?;
        Ok((path, config))
    }

    pub(crate) fn parse(yaml: &[u8], home: &Path) -> Result<Self> {
        let input = std::str::from_utf8(yaml).context("設定ファイルが UTF-8 ではありません")?;
        let mut cfg: Self =
            serde_saphyr::from_str(input).context("設定ファイルの YAML が不正です")?;
        ensure!(
            cfg.server.port > 0 && !cfg.server.db_path.is_empty(),
            "server.port または server.db_path が不正です"
        );
        ensure!(
            !cfg.embedding.model.is_empty()
                && cfg.embedding.dim > 0
                && cfg.embedding.timeout_seconds > 0,
            "embedding の設定が不正です"
        );
        ensure!(
            !cfg.rerank.model.is_empty() && cfg.rerank.factor > 0 && cfg.rerank.timeout_seconds > 0,
            "rerank の設定が不正です"
        );
        ensure!(
            cfg.chunker.max_chunk_size > 0,
            "chunker.max_chunk_size が不正です"
        );
        ensure!(
            cfg.bm25.k1.is_finite() && cfg.bm25.k1 > 0.0 && (0.0..=1.0).contains(&cfg.bm25.b),
            "bm25 の設定が不正です"
        );
        ensure!(
            cfg.fetcher.timeout_seconds > 0,
            "fetcher.timeout_seconds が不正です"
        );
        ensure!(
            cfg.trash.retention_days > 0 && cfg.trash.interval_seconds > 0,
            "trash の設定が不正です"
        );

        if cfg.log.path.is_empty() {
            cfg.log.path = "~/.doc-db/doc-db.log".into();
        }
        if cfg.log.level.is_empty() {
            cfg.log.level = "info".into();
        }
        ensure!(
            matches!(cfg.log.level.as_str(), "debug" | "info" | "warn" | "error"),
            "log.level が不正です"
        );
        cfg.server.db_path = expand_home(&cfg.server.db_path, home)?;
        cfg.log.path = expand_home(&cfg.log.path, home)?;
        Ok(cfg)
    }
}

fn expand_home(path: &str, home: &Path) -> Result<String> {
    let expanded = match path.strip_prefix("~/") {
        Some(relative) => home.join(relative.trim_start_matches('/')),
        None if path == "~" => home.to_path_buf(),
        None => return Ok(path.to_owned()),
    };
    expanded
        .into_os_string()
        .into_string()
        .map_err(|_| anyhow::anyhow!("設定パスが UTF-8 ではありません"))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn example_and_unknown_fields() {
        let yaml = include_str!("../doc-db.yaml.example");
        assert!(Config::parse(yaml.as_bytes(), Path::new("/home/docdb")).is_ok());
        let unknown = format!("{yaml}\nunknown: true\n");
        assert!(Config::parse(unknown.as_bytes(), Path::new("/home/docdb")).is_err());
    }
}
