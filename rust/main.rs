mod chunker;
mod config;
mod network;
mod search;
mod server;
mod store;
#[cfg(test)]
mod tests;

use anyhow::{Context, Result};
use axum::Router;
use rmcp::transport::streamable_http_server::{
    StreamableHttpServerConfig, StreamableHttpService, session::local::LocalSessionManager,
};
use std::{
    fs::{self, OpenOptions},
    net::Ipv4Addr,
    os::unix::fs::OpenOptionsExt,
    path::Path,
    sync::Mutex,
    time::Duration,
};
use tokio::{
    net::TcpListener,
    signal::unix::{SignalKind, signal},
};
use tracing::Level;

const VERSION: &str = include_str!("../VERSION").trim_ascii();

#[tokio::main]
async fn main() {
    if let Err(error) = run().await {
        eprintln!("doc-db: {error:#}");
        std::process::exit(1);
    }
}

async fn run() -> Result<()> {
    let argument = std::env::args().nth(1);
    if matches!(argument.as_deref(), Some("--version" | "-v")) {
        println!("{VERSION}");
        return Ok(());
    }
    let (path, cfg) = config::Config::load()?;
    if argument.as_deref() == Some("--show-config") {
        println!("version:         {VERSION}");
        println!("config_path:     {}", path.display());
        println!("log.path:        {}", cfg.log.path);
        println!("log.level:       {}", cfg.log.level);
        println!("server.db_path:  {}", cfg.server.db_path);
        println!("server.port:     {}", cfg.server.port);
        println!("embedding.model: {}", cfg.embedding.model);
        return Ok(());
    }

    configure_logging(&cfg.log)?;
    // local_path を持つサーバーを利用者のネットワークへ公開しない。
    let addr = (Ipv4Addr::LOCALHOST, cfg.server.port);
    let listener = TcpListener::bind(addr)
        .await
        .context("MCP の待受を開始できません")?;
    let interval = Duration::from_secs(cfg.trash.interval_seconds);
    let mut interrupt = signal(SignalKind::interrupt())?;
    let mut terminate = signal(SignalKind::terminate())?;
    let server = server::Server::new(cfg)?;
    server.sweep(false).await;

    let transport = StreamableHttpServerConfig::default()
        .enforce_origin_validation()
        .with_max_request_body_bytes(64 * 1024 * 1024);
    let cancellation = transport.cancellation_token.clone();
    let app = router(server.clone(), transport);

    let worker_server = server.clone();
    let worker_cancel = cancellation.clone();
    let worker = tokio::spawn(async move {
        let mut timer = tokio::time::interval(interval);
        timer.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
        loop {
            tokio::select! {
                _ = worker_cancel.cancelled() => break,
                _ = timer.tick() => worker_server.sweep(true).await,
            }
        }
    });
    println!("doc-db v{VERSION} 起動 ({})", listener.local_addr()?);
    let signal_cancel = cancellation.clone();
    let stopping_server = server.clone();
    let result = axum::serve(listener, app)
        .with_graceful_shutdown(async move {
            tokio::select! {
                _ = interrupt.recv() => {},
                _ = terminate.recv() => {},
            }
            signal_cancel.cancel();
            stopping_server.begin_shutdown();
        })
        .await;
    cancellation.cancel();
    worker.abort();
    if let Err(error) = worker.await
        && !error.is_cancelled()
    {
        tracing::warn!(%error, "回収ワーカーが異常終了しました");
    }
    server.stop_jobs().await;
    result.context("MCP サーバーが終了しました")
}

fn router(server: server::Server, transport: StreamableHttpServerConfig) -> Router {
    let service: StreamableHttpService<server::Server, LocalSessionManager> =
        StreamableHttpService::new(move || Ok(server.clone()), Default::default(), transport);
    Router::new().route_service("/mcp", service)
}

fn configure_logging(cfg: &config::Log) -> Result<()> {
    let level = match cfg.level.as_str() {
        "debug" => Level::DEBUG,
        "warn" => Level::WARN,
        "error" => Level::ERROR,
        _ => Level::INFO,
    };
    let builder = tracing_subscriber::fmt()
        .with_ansi(false)
        .with_max_level(level);
    match cfg.path.as_str() {
        "stdout" => builder.with_writer(std::io::stdout).try_init(),
        "stderr" => builder.with_writer(std::io::stderr).try_init(),
        path => {
            if let Some(parent) = Path::new(path)
                .parent()
                .filter(|parent| !parent.as_os_str().is_empty())
            {
                fs::create_dir_all(parent).context("ログディレクトリを作成できません")?;
            }
            let file = OpenOptions::new()
                .append(true)
                .create(true)
                .mode(0o600)
                .open(path)
                .context("ログを開けません")?;
            builder.with_writer(Mutex::new(file)).try_init()
        }
    }
    .map_err(|error| anyhow::anyhow!("ログを初期化できません: {error}"))
}
