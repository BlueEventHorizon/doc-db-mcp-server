use axum::{body::Bytes, routing::post};
use reqwest::{Client, Response, Url};
use rusqlite::{Connection, params};
use serde_json::{Value, json};
use std::{
    fs,
    path::{Path, PathBuf},
    sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    },
};
use tokio::{
    net::TcpListener,
    task::JoinHandle,
    time::{Duration, sleep},
};

use super::{
    Router, StreamableHttpServerConfig, config::Config, network::OpenAi, router, server::Server,
};

struct Workspace(PathBuf);

impl Workspace {
    fn new() -> Self {
        let path = Path::new(".temp").join(format!("rust-test-{}", uuid::Uuid::new_v4()));
        fs::create_dir_all(&path).unwrap();
        Self(path)
    }
}

impl Drop for Workspace {
    fn drop(&mut self) {
        if let Err(error) = fs::remove_dir_all(&self.0) {
            eprintln!("テストディレクトリの削除に失敗しました: {error}");
        }
    }
}

fn seed_legacy_sqlite(path: &Path, content: &str) {
    let db = Connection::open(path).unwrap();
    db.execute_batch(include_str!("schema.sql")).unwrap();
    db.execute_batch(
        "ALTER TABLE keys DROP COLUMN trashed_at;
        INSERT INTO keys VALUES ('test',1,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z',NULL);",
    )
    .unwrap();
    let hash = "eaf91445faaf2d667930e54d4c2d34f3e9584e160cf998d0ad4836315eeb2e9d";
    db.execute("INSERT INTO records VALUES (1,'test','guide.md',?1,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')", [hash]).unwrap();
    db.execute("INSERT INTO chunks VALUES (1,1,0,'見出し',?1)", [content])
        .unwrap();
    db.execute("INSERT INTO series_keys VALUES (1,'main')", [])
        .unwrap();
    let vector: Vec<u8> = [1.0_f32, 0.0, 0.0]
        .into_iter()
        .flat_map(f32::to_le_bytes)
        .collect();
    db.execute("INSERT INTO embeddings VALUES (1,?1,3)", params![vector])
        .unwrap();
}

async fn bind(app: Router) -> (Url, JoinHandle<()>) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let url = Url::parse(&format!("http://{}/", listener.local_addr().unwrap())).unwrap();
    let task = tokio::spawn(async move { axum::serve(listener, app).await.unwrap() });
    (url, task)
}

struct Mcp {
    client: Client,
    url: Url,
    session: Option<String>,
    id: u32,
}

impl Mcp {
    async fn post(&mut self, method: &str, params: Value) -> Response {
        self.id += 1;
        let payload = if method == "notifications/initialized" {
            json!({"jsonrpc": "2.0", "method": method})
        } else {
            json!({"jsonrpc": "2.0", "id": self.id, "method": method, "params": params})
        };
        let mut request = self
            .client
            .post(self.url.clone())
            .header("Accept", "application/json, text/event-stream")
            .json(&payload);
        if let Some(session) = &self.session {
            request = request.header("Mcp-Session-Id", session);
        }
        request.send().await.unwrap()
    }

    async fn call(&mut self, name: &str, args: Value) -> Value {
        let response = self
            .post("tools/call", json!({"name": name, "arguments": args}))
            .await;
        mcp_body(response).await
    }

    async fn tool(&mut self, name: &str, args: Value) -> Value {
        let response = self.call(name, args).await;
        assert!(response.get("error").is_none(), "{response}");
        response["result"]["structuredContent"].clone()
    }

    async fn sync(&mut self, documents: Value) -> Value {
        let result = self
            .tool(
                "sync_documents",
                json!({"key":"test","series":"branch","documents":documents}),
            )
            .await;
        let id = result["job_id"].as_str().unwrap();
        for _ in 0..100 {
            let status = self.tool("get_sync_status", json!({"job_id":id})).await;
            if status["status"] != "running" {
                assert_eq!(status["status"], "done", "{status}");
                return status;
            }
            sleep(Duration::from_millis(10)).await;
        }
        panic!("同期タイムアウト");
    }
}

async fn mcp_body(response: Response) -> Value {
    let status = response.status();
    let body = response.text().await.unwrap();
    assert!(status.is_success(), "{status} {body}");
    if let Ok(json) = serde_json::from_str(&body) {
        return json;
    }
    let data = body
        .lines()
        .filter_map(|line| line.strip_prefix("data:"))
        .find(|line| !line.trim().is_empty())
        .unwrap_or_else(|| panic!("{body}"));
    serde_json::from_str(data).unwrap()
}

#[tokio::test]
async fn streamable_http_preserves_go_database_and_sync_contract() {
    let workspace = Workspace::new();
    let path = workspace.0.join("legacy.sqlite");
    let content = "# 見出し\nＡＰＰ-001 の仕様";
    seed_legacy_sqlite(&path, content);
    let mut cfg = Config::parse(include_bytes!("../doc-db.yaml.example"), Path::new("/")).unwrap();
    cfg.server.db_path = path.to_string_lossy().into_owned();
    cfg.embedding.dim = 3;

    let embed_calls = Arc::new(AtomicUsize::new(0));
    let calls = embed_calls.clone();
    let mock = Router::new()
        .route("/embeddings", post(move |body: Bytes| {
            let calls = calls.clone();
            async move {
                calls.fetch_add(1, Ordering::SeqCst);
                let body: Value = serde_json::from_slice(&body).unwrap();
                let inputs = body["input"].as_array().unwrap();
                let data: Vec<_> = (0..inputs.len()).rev().map(|index| json!({"index":index,"embedding":[1,0,0]})).collect();
                json!({"data":data}).to_string()
            }
        }))
        .route("/chat/completions", post(|| async {
            json!({"choices":[{"message":{"content":"{\"ranking\":[{\"id\":\"0\",\"score\":0.8}]}"}}]}).to_string()
        }));
    let (api_url, api_task) = bind(mock).await;
    let openai = OpenAi::mock(&cfg, api_url).unwrap();
    let server = Server::with_openai(cfg, openai).unwrap();
    let options = StreamableHttpServerConfig::default().enforce_origin_validation();
    let (url, task) = bind(router(server.clone(), options)).await;
    let mut mcp = Mcp {
        client: Client::builder()
            .no_proxy()
            .timeout(Duration::from_secs(8))
            .build()
            .unwrap(),
        url: url.join("mcp").unwrap(),
        id: 0,
        session: None,
    };
    let initialize = json!({"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1"}});
    let browser = mcp
        .client
        .post(mcp.url.clone())
        .header("Accept", "application/json, text/event-stream")
        .header("Origin", "https://example.com")
        .json(&json!({"jsonrpc":"2.0","id":1,"method":"initialize","params":initialize}))
        .send()
        .await
        .unwrap();
    assert_eq!(browser.status(), reqwest::StatusCode::FORBIDDEN);
    let response = mcp.post("initialize", initialize).await;
    mcp.session = response
        .headers()
        .get("Mcp-Session-Id")
        .map(|header| header.to_str().unwrap().to_owned());
    assert!(mcp.session.is_some());
    assert_eq!(
        mcp_body(response).await["result"]["protocolVersion"],
        "2025-03-26"
    );
    assert!(
        mcp.post("notifications/initialized", Value::Null)
            .await
            .status()
            .is_success()
    );
    assert_eq!(
        mcp_body(mcp.post("tools/list", json!({})).await).await["result"]["tools"]
            .as_array()
            .unwrap()
            .len(),
        11
    );

    let documents = json!([{"path":"guide.md","content":content}]);
    let inputs = json!({"key":"test","series":"branch","documents":documents});
    assert_eq!(mcp.tool("upsert_documents", inputs).await["skipped"], 1);
    assert_eq!(embed_calls.load(Ordering::SeqCst), 0);

    let query = |mode| json!({"key":"test","series":"branch","query":"APP-001","mode":mode});
    let result = mcp.tool("query", query("all")).await;
    assert_eq!(
        result["results"][0]["origin_signals"],
        json!(["emb", "lex", "grep"])
    );
    assert_eq!(
        result["results"][0]["series_keys"],
        json!(["branch", "main"])
    );
    let result = mcp.tool("query", query("rerank")).await;
    assert_eq!(result["results"][0]["score"], 0.8);
    for mode in ["emb", "lex", "grep", "hybrid"] {
        assert_eq!(
            mcp.tool("query", query(mode)).await["results"][0]["path"],
            "guide.md"
        );
    }

    let replacement = json!([{"path":"new.md","content":"新しい文書"}]);
    let result = mcp.sync(replacement).await;
    assert_eq!(result["processed"], 1);
    assert_eq!(result["deleted_paths_marked"], 0); // main が guide.md を参照している。
    assert_eq!(mcp.tool("query", query("grep")).await["results"], json!([]));
    let result = mcp.sync(json!([])).await;
    assert_eq!(result["deleted_paths_marked"], 1);
    let calls_before = embed_calls.load(Ordering::SeqCst);
    assert_eq!(
        mcp.sync(json!([{"path":"new.md","content":"新しい文書"}]))
            .await["skipped"],
        1
    );
    assert_eq!(embed_calls.load(Ordering::SeqCst), calls_before);

    let branches = json!({"key":"test","series":"branch"});
    assert_eq!(
        mcp.tool("schedule_delete_series", branches.clone()).await["already_scheduled"],
        false
    );
    assert_eq!(
        mcp.tool("schedule_delete_series", branches.clone()).await["already_scheduled"],
        true
    );
    assert_eq!(
        mcp.tool("trash_index", json!({"key":"test"})).await["trashed"],
        true
    );
    assert_eq!(
        mcp.call("query", query("grep")).await["error"]["data"]["code"],
        "KEY_TRASHED"
    );
    assert_eq!(
        mcp.tool("list_indexes", json!({})).await["indexes"],
        json!([])
    );
    assert_eq!(
        mcp.tool("list_trashed_indexes", json!({})).await["indexes"][0]["key"],
        "test"
    );
    assert_eq!(
        mcp.tool("restore_index", json!({"key":"test"})).await["restored"],
        true
    );
    assert_eq!(
        mcp.tool(
            "delete_documents",
            json!({"key":"test","series":"branch","paths":["new.md"]})
        )
        .await["deleted"],
        1
    );
    assert_eq!(
        mcp.tool("delete_series", branches).await["removed_records"],
        0
    );

    // 同梱 Python クライアントを変更せずに Go 版と同じ応答を読めることを確認する。
    let client_script = r#"
import sys
sys.path.insert(0, '.claude/skills/query-doc-db/scripts')
import docdb_client
client = docdb_client.Client(5)
client.url = sys.argv[1]
client.initialize()
assert client.call('list_indexes', {})['indexes'][0]['series'] == ['main']
"#;
    let url = mcp.url.to_string();
    let output = tokio::task::spawn_blocking(move || {
        std::process::Command::new("python3")
            .args(["-c", client_script, &url])
            .output()
            .unwrap()
    })
    .await
    .unwrap();
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );

    server.stop_jobs().await;
    task.abort();
    api_task.abort();
}
