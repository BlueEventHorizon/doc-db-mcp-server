use anyhow::{Context, Result, ensure};
use rmcp::{
    RoleServer, ServerHandler,
    model::{
        CallToolRequestParams, CallToolResponse, CallToolResult, ErrorCode, ErrorData,
        Implementation, ListToolsResult, PaginatedRequestParams, ServerCapabilities, ServerConfig,
        Tool,
    },
    service::RequestContext,
};
use schemars::JsonSchema;
use serde::{Deserialize, Serialize, de::DeserializeOwned};
use serde_json::{Value, json};
use sha2::{Digest, Sha256};
use std::{
    collections::{HashMap, HashSet, VecDeque},
    path::Path,
    sync::{Arc, LazyLock, Mutex, Weak},
};
use time::{OffsetDateTime, format_description::well_known::Rfc3339};
use tokio::sync::{Mutex as AsyncMutex, OwnedMutexGuard, Semaphore, watch};

use crate::{
    VERSION, chunker, config,
    network::{self, Fetcher, OpenAi},
    search,
    store::{self, KeyState, Store},
};

type ToolResult = Result<Value, ErrorData>;
const MAX_RUNNING_SYNC_JOBS: usize = 8;

#[derive(Clone)]
pub struct Server(Arc<State>);

struct State {
    config: config::Config,
    store: Store,
    fetcher: Fetcher,
    openai: OpenAi,
    locks: Mutex<HashMap<String, Weak<AsyncMutex<()>>>>,
    jobs: Mutex<Jobs>,
    job_capacity: Arc<Semaphore>,
    shutdown: watch::Sender<bool>,
}

#[derive(Default)]
struct Jobs {
    states: HashMap<String, Job>,
    completed: VecDeque<String>,
}

#[derive(Clone, Serialize)]
struct Job {
    status: &'static str,
    processed: usize,
    skipped: usize,
    failed: usize,
    deleted_paths_marked: usize,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    errors: Vec<String>,
}

#[derive(Deserialize, JsonSchema)]
struct Document {
    /// KEY と series の中で文書を識別する path。
    path: String,
    /// Markdown 本文。url と local_path とは排他。
    #[serde(default)]
    content: String,
    /// 文書取得元の HTTP(S) URL。content と local_path とは排他。
    #[serde(default)]
    url: String,
    /// サーバーが読み取る絶対パス。通常ファイル、10 MiB 以下。content と url とは排他。
    #[serde(default)]
    local_path: String,
    #[serde(default, rename = "hash")]
    _hash: String,
}

#[derive(Deserialize, JsonSchema)]
struct DocumentsInput {
    key: String,
    series: String,
    documents: Vec<Document>,
}

#[derive(Deserialize, JsonSchema)]
struct KeyInput {
    key: String,
}

#[derive(Deserialize, JsonSchema)]
struct SeriesInput {
    key: String,
    series: String,
}

#[derive(Deserialize, JsonSchema)]
struct DeleteInput {
    key: String,
    series: String,
    paths: Vec<String>,
}

#[derive(Deserialize, JsonSchema)]
struct QueryInput {
    key: String,
    /// 自然言語、固有名詞、または識別子。
    query: String,
    /// 省略時は全 series を対象とする。
    #[serde(default)]
    series: String,
    /// all (既定), rerank, emb, lex, grep, hybrid のいずれか。
    #[serde(default)]
    mode: String,
    /// 最大結果数。省略時は 10。
    #[serde(default)]
    top_n: i64,
}

#[derive(Deserialize, JsonSchema)]
struct JobInput {
    job_id: String,
}

#[derive(Deserialize, JsonSchema)]
struct EmptyInput {}

#[derive(Default, Serialize)]
struct UpsertResult {
    processed: usize,
    skipped: usize,
    failed: usize,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    errors: Vec<DocumentError>,
}

#[derive(Serialize)]
struct DocumentError {
    path: String,
    error: String,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    skipped_chunks: Vec<usize>,
}

enum UpsertOutcome {
    Skipped,
    Processed(Option<DocumentError>),
}

fn invalid(message: impl Into<std::borrow::Cow<'static, str>>) -> ErrorData {
    ErrorData::invalid_params(message, None)
}

fn internal(error: anyhow::Error) -> ErrorData {
    tracing::error!(%error, "MCP ツールの処理に失敗しました");
    ErrorData::internal_error(
        "サーバー側の処理に失敗しました。サーバーログを確認してください",
        None,
    )
}

fn key_error(key: &str, code: &'static str, number: i32, detail: &str) -> ErrorData {
    ErrorData::new(
        ErrorCode(number),
        format!("{code}: key {key:?} {detail}"),
        Some(json!({"code": code, "key": key})),
    )
}

fn require_key(key: &str) -> Result<(), ErrorData> {
    if key.is_empty() {
        Err(invalid("key は必須です"))
    } else {
        Ok(())
    }
}

fn require_series(key: &str, series: &str) -> Result<(), ErrorData> {
    require_key(key)?;
    if series.is_empty() {
        Err(invalid("series は必須です"))
    } else {
        Ok(())
    }
}

impl Server {
    pub fn new(config: config::Config) -> Result<Self> {
        let openai = OpenAi::new(&config)?;
        Self::with_openai(config, openai)
    }

    pub(crate) fn with_openai(config: config::Config, openai: OpenAi) -> Result<Self> {
        let fetcher = Fetcher::new(&config.fetcher)?;
        let store = Store::open(&config.server.db_path, config.embedding.dim)?;
        let (shutdown, _) = watch::channel(false);
        Ok(Self(Arc::new(State {
            config,
            store,
            fetcher,
            openai,
            locks: Mutex::new(HashMap::new()),
            jobs: Mutex::new(Jobs::default()),
            job_capacity: Arc::new(Semaphore::new(MAX_RUNNING_SYNC_JOBS)),
            shutdown,
        })))
    }

    async fn key_lock(&self, key: &str) -> Result<OwnedMutexGuard<()>> {
        let lock = {
            let mut locks = self
                .0
                .locks
                .lock()
                .map_err(|_| anyhow::anyhow!("KEY ロックの取得に失敗しました"))?;
            locks.retain(|_, lock| lock.strong_count() > 0);
            if let Some(lock) = locks.get(key).and_then(Weak::upgrade) {
                lock
            } else {
                let lock = Arc::new(AsyncMutex::new(()));
                locks.insert(key.to_owned(), Arc::downgrade(&lock));
                lock
            }
        };
        Ok(lock.lock_owned().await)
    }

    async fn check_key(&self, key: &str, must_exist: bool) -> Result<(), ErrorData> {
        let owned = key.to_owned();
        match self
            .0
            .store
            .read(move |db| store::state(db, &owned))
            .await
            .map_err(internal)?
        {
            KeyState::Missing if must_exist => {
                Err(key_error(key, "KEY_NOT_FOUND", -31001, "が存在しません"))
            }
            KeyState::Trashed => Err(key_error(
                key,
                "KEY_TRASHED",
                -31002,
                "はゴミ箱にあります。restore_index で復活してください",
            )),
            _ => Ok(()),
        }
    }

    async fn mutation(&self, key: &str) -> Result<OwnedMutexGuard<()>, ErrorData> {
        self.check_key(key, false).await?;
        let guard = self.key_lock(key).await.map_err(internal)?;
        self.check_key(key, false).await?;
        Ok(guard)
    }

    async fn upsert_one(&self, key: &str, series: &str, doc: Document) -> Result<UpsertOutcome> {
        ensure!(!doc.path.is_empty(), "path は必須です");
        let source_count = [&doc.content, &doc.url, &doc.local_path]
            .iter()
            .filter(|value| !value.is_empty())
            .count();
        ensure!(
            source_count == 1,
            "content / url / local_path のいずれか 1 つを指定してください"
        );
        let content = if !doc.url.is_empty() {
            self.0.fetcher.fetch(&doc.url).await.context("fetch")?
        } else if !doc.local_path.is_empty() {
            tokio::task::spawn_blocking(move || network::read_local(Path::new(&doc.local_path)))
                .await?
                .context("local_path")?
        } else {
            doc.content
        };
        let content = content
            .strip_prefix('\u{feff}')
            .unwrap_or(&content)
            .replace("\r\n", "\n")
            .replace('\r', "\n");
        // 現行と同じく、クライアントの hash に依存せず正規化後の本文から算出する。
        let mut hash = String::with_capacity(64);
        const HEX: &[u8; 16] = b"0123456789abcdef";
        for byte in Sha256::digest(content.as_bytes()) {
            hash.push(char::from(HEX[usize::from(byte >> 4)]));
            hash.push(char::from(HEX[usize::from(byte & 15)]));
        }
        let (key, series, path) = (key.to_owned(), series.to_owned(), doc.path);
        let existing = {
            let (key, series, path, hash) =
                (key.clone(), series.clone(), path.clone(), hash.clone());
            self.0
                .store
                .run(move |db| store::reuse(db, &key, &series, &path, &hash))
                .await?
        };
        if existing {
            return Ok(UpsertOutcome::Skipped);
        }
        let max_size = self.0.config.chunker.max_chunk_size;
        let chunks =
            tokio::task::spawn_blocking(move || chunker::split(&content, max_size)).await??;
        let mut vectors = Vec::with_capacity(chunks.len());
        let mut skipped_chunks = Vec::new();
        let mut error = String::new();
        for batch in chunks.chunks(100) {
            let input: Vec<_> = batch
                .iter()
                .map(|chunk| chunk.embed_text.as_str())
                .collect();
            match self.0.openai.embed(&input).await {
                Ok(batch) => vectors.extend(batch.into_iter().map(Some)),
                Err(batch_error) => {
                    tracing::warn!(error = %batch_error, "Embedding に失敗したチャンクを本文のみで保存します");
                    error = format!("embed: {batch_error}");
                    skipped_chunks.extend(vectors.len()..vectors.len() + batch.len());
                    vectors.resize_with(vectors.len() + batch.len(), || None);
                }
            }
        }
        let error = (!skipped_chunks.is_empty()).then(|| DocumentError {
            path: path.clone(),
            error,
            skipped_chunks,
        });
        self.0
            .store
            .run(move |db| store::save(db, &key, &series, &path, &hash, chunks, vectors))
            .await?;
        Ok(UpsertOutcome::Processed(error))
    }

    async fn upsert_into(&self, key: &str, series: &str, doc: Document, result: &mut UpsertResult) {
        let path = doc.path.clone();
        match self.upsert_one(key, series, doc).await {
            Ok(UpsertOutcome::Skipped) => result.skipped += 1,
            Ok(UpsertOutcome::Processed(error)) => {
                result.processed += 1;
                result.errors.extend(error);
            }
            Err(error) => {
                tracing::warn!(%error, "文書の登録に失敗しました");
                result.failed += 1;
                result.errors.push(DocumentError {
                    path,
                    error: format!("{error:#}"),
                    skipped_chunks: Vec::new(),
                });
            }
        }
    }

    async fn upsert(&self, input: DocumentsInput) -> ToolResult {
        require_series(&input.key, &input.series)?;
        if input.documents.is_empty() {
            return Err(invalid("documents が空です"));
        }
        let _guard = self.mutation(&input.key).await?;
        let mut result = UpsertResult::default();
        for doc in input.documents {
            self.upsert_into(&input.key, &input.series, doc, &mut result)
                .await;
        }
        to_value(result)
    }

    async fn sync(&self, input: DocumentsInput) -> ToolResult {
        require_series(&input.key, &input.series)?;
        self.check_key(&input.key, false).await?;
        let permit = self
            .0
            .job_capacity
            .clone()
            .try_acquire_owned()
            .map_err(|_| invalid("同期ジョブの実行上限に達しました。後で再試行してください"))?;
        let job_id = uuid::Uuid::new_v4().simple().to_string();
        self.0
            .jobs
            .lock()
            .map_err(|_| ErrorData::internal_error("ジョブ状態を取得できません", None))?
            .states
            .insert(
                job_id.clone(),
                Job {
                    status: "running",
                    processed: 0,
                    skipped: 0,
                    failed: 0,
                    deleted_paths_marked: 0,
                    errors: Vec::new(),
                },
            );
        let server = self.clone();
        let id = job_id.clone();
        let mut shutdown = self.0.shutdown.subscribe();
        tokio::spawn(async move {
            let _permit = permit;
            let result = tokio::select! {
                _ = shutdown.wait_for(|stopped| *stopped) => Err(anyhow::anyhow!("サーバー終了のため同期を中断しました")),
                result = server.run_sync(&id, input) => result,
            };
            if let Err(error) = &result {
                tracing::warn!(job_id = id, %error, "同期ジョブが失敗しました");
            }
            match server.0.jobs.lock() {
                Ok(mut jobs) => {
                    if let Some(job) = jobs.states.get_mut(&id) {
                        job.status = if result.is_ok() { "done" } else { "failed" };
                        if let Err(error) = result {
                            job.errors.push(error.to_string());
                        }
                    }
                    jobs.completed.push_back(id);
                    if jobs.completed.len() > 100
                        && let Some(old) = jobs.completed.pop_front()
                    {
                        jobs.states.remove(&old);
                    }
                }
                Err(error) => tracing::error!(%error, "同期ジョブの結果を記録できません"),
            }
        });
        Ok(json!({"job_id": job_id}))
    }

    async fn run_sync(&self, id: &str, input: DocumentsInput) -> Result<()> {
        let _guard = self.key_lock(&input.key).await?;
        self.check_key(&input.key, false)
            .await
            .map_err(|error| anyhow::anyhow!("{}", error.message))?;
        let mut result = UpsertResult::default();
        let desired = input
            .documents
            .iter()
            .map(|doc| doc.path.clone())
            .collect::<HashSet<_>>();
        for doc in input.documents {
            self.upsert_into(&input.key, &input.series, doc, &mut result)
                .await;
            self.update_job(id, |job| {
                job.processed = result.processed;
                job.skipped = result.skipped;
                job.failed = result.failed;
                job.errors.extend(
                    result
                        .errors
                        .drain(..)
                        .map(|error| format!("path {:?}: {}", error.path, error.error)),
                );
            })?;
        }
        let marked = self
            .0
            .store
            .run(move |db| store::sync_paths(db, &input.key, &input.series, &desired))
            .await?;
        self.update_job(id, |job| job.deleted_paths_marked = marked)
    }

    fn update_job(&self, id: &str, update: impl FnOnce(&mut Job)) -> Result<()> {
        let mut jobs = self
            .0
            .jobs
            .lock()
            .map_err(|_| anyhow::anyhow!("ジョブ状態を取得できません"))?;
        update(
            jobs.states
                .get_mut(id)
                .context("ジョブ状態が見つかりません")?,
        );
        Ok(())
    }

    async fn get_status(&self, input: JobInput) -> ToolResult {
        let jobs = self
            .0
            .jobs
            .lock()
            .map_err(|_| ErrorData::internal_error("ジョブ状態を取得できません", None))?;
        to_value(
            jobs.states
                .get(&input.job_id)
                .ok_or_else(|| invalid("指定の job_id が見つかりません"))?,
        )
    }

    async fn query(&self, input: QueryInput) -> ToolResult {
        require_key(&input.key)?;
        if input.query.is_empty() {
            return Err(invalid("query は必須です"));
        }
        let mode = search::Mode::parse(&input.mode).map_err(|error| invalid(error.to_string()))?;
        let top = if input.top_n <= 0 {
            10
        } else {
            input.top_n as usize
        };
        let guard = self.key_lock(&input.key).await.map_err(internal)?;
        self.check_key(&input.key, true).await?;
        let key = input.key.clone();
        let touch_error = self
            .0
            .store
            .run(move |db| store::touch(db, &key))
            .await
            .err();
        let chunks = self
            .0
            .store
            .read(move |db| store::chunks(db, &input.key, &input.series, mode.needs_embedding()))
            .await
            .map_err(internal)?;
        drop(guard);
        let mut result = search::run(
            chunks,
            input.query,
            mode,
            top,
            &self.0.config.bm25,
            &self.0.openai,
        )
        .await
        .map_err(internal)?;
        if let Some(error) = touch_error {
            tracing::warn!(%error, "最終アクセス時刻を更新できません");
            result.warnings.insert(
                0,
                "TouchKey failed: サーバーログを確認してください".to_owned(),
            );
        }
        to_value(result)
    }

    async fn delete_documents(&self, input: DeleteInput) -> ToolResult {
        require_series(&input.key, &input.series)?;
        if input.paths.is_empty() {
            return Err(invalid("paths は必須です"));
        }
        let _guard = self.mutation(&input.key).await?;
        let (deleted, missing) = self
            .0
            .store
            .run(move |db| store::delete_paths(db, &input.key, &input.series, &input.paths))
            .await
            .map_err(internal)?;
        if missing.is_empty() {
            Ok(json!({"deleted": deleted}))
        } else {
            let warnings: Vec<_> = missing
                .iter()
                .map(|path| format!("path {path:?} は存在しないためスキップ"))
                .collect();
            Ok(json!({"deleted": deleted, "warnings": warnings}))
        }
    }

    async fn delete_series(&self, input: SeriesInput) -> ToolResult {
        require_series(&input.key, &input.series)?;
        let _guard = self.mutation(&input.key).await?;
        let (removed, updated) = self
            .0
            .store
            .run(move |db| store::delete_series(db, &input.key, &input.series))
            .await
            .map_err(internal)?;
        Ok(json!({"removed_records": removed, "updated_records": updated}))
    }

    async fn schedule(&self, input: SeriesInput) -> ToolResult {
        require_series(&input.key, &input.series)?;
        let _guard = self.mutation(&input.key).await?;
        let (key, series) = (input.key.clone(), input.series.clone());
        let already = self
            .0
            .store
            .run(move |db| store::schedule(db, &key, &series))
            .await
            .map_err(internal)?;
        Ok(json!({"key": input.key, "series": input.series, "already_scheduled": already}))
    }

    async fn list_indexes(&self, _: EmptyInput) -> ToolResult {
        let indexes = self.0.store.read(store::indexes).await.map_err(internal)?;
        Ok(json!({"indexes": indexes}))
    }

    async fn trash(&self, input: KeyInput) -> ToolResult {
        require_key(&input.key)?;
        let _guard = self.key_lock(&input.key).await.map_err(internal)?;
        let key = input.key.clone();
        let timestamp = store::now();
        let stored_timestamp = timestamp.clone();
        let changed = self
            .0
            .store
            .run(move |db| store::set_trash(db, &key, Some(&stored_timestamp)))
            .await
            .map_err(internal)?;
        if !changed {
            return Err(invalid("指定の KEY が存在しないか、すでにゴミ箱にあります"));
        }
        Ok(json!({"key": input.key, "trashed": true, "trashed_at": timestamp}))
    }

    async fn restore(&self, input: KeyInput) -> ToolResult {
        require_key(&input.key)?;
        let _guard = self.key_lock(&input.key).await.map_err(internal)?;
        let key = input.key.clone();
        let changed = self
            .0
            .store
            .run(move |db| store::set_trash(db, &key, None))
            .await
            .map_err(internal)?;
        if !changed {
            return Err(invalid("指定の KEY がゴミ箱にありません"));
        }
        Ok(json!({"key": input.key, "restored": true}))
    }

    async fn list_trashed(&self, _: EmptyInput) -> ToolResult {
        let entries = self
            .0
            .store
            .read(|db| store::trashed(db))
            .await
            .map_err(internal)?;
        let now = OffsetDateTime::now_utc().unix_timestamp();
        let retention = i64::from(self.0.config.trash.retention_days) * 86_400;
        let indexes: Vec<_> = entries
            .into_iter()
            .map(|(key, timestamp)| {
                let remaining = match OffsetDateTime::parse(&timestamp, &Rfc3339) {
                    Ok(at) => (at.unix_timestamp() + retention - now).max(0),
                    Err(error) => {
                        tracing::warn!(%error, "ゴミ箱投入時刻が不正です");
                        0
                    }
                };
                json!({"key": key, "trashed_at": timestamp, "remaining_seconds": remaining})
            })
            .collect();
        Ok(json!({"indexes": indexes}))
    }

    pub async fn sweep(&self, include_trash: bool) {
        let cutoff = OffsetDateTime::now_utc().unix_timestamp()
            - i64::from(self.0.config.trash.retention_days) * 86_400;
        if include_trash {
            match self.0.store.read(|db| store::trashed(db)).await {
                Ok(entries) => {
                    for (key, at) in entries {
                        if is_expired(&at, cutoff)
                            && let Err(error) = self.sweep_key(key, at).await
                        {
                            tracing::warn!(%error, "ゴミ箱の回収に失敗しました");
                        }
                    }
                }
                Err(error) => tracing::warn!(%error, "ゴミ箱一覧を取得できません"),
            }
        }
        match self.0.store.read(|db| store::pending(db)).await {
            Ok(entries) => {
                for entry in entries {
                    if is_expired(&entry.marked_at, cutoff)
                        && let Err(error) = self.sweep_entry(entry).await
                    {
                        tracing::warn!(%error, "削除予約の回収に失敗しました");
                    }
                }
            }
            Err(error) => tracing::warn!(%error, "削除予約一覧を取得できません"),
        }
    }

    async fn sweep_key(&self, key: String, at: String) -> Result<()> {
        let _guard = self.key_lock(&key).await?;
        self.0
            .store
            .run(move |db| store::delete_trashed(db, &key, &at))
            .await
    }

    async fn sweep_entry(&self, entry: store::Pending) -> Result<()> {
        let _guard = self.key_lock(&entry.key).await?;
        self.0
            .store
            .run(move |db| store::sweep_pending(db, entry))
            .await
    }

    pub fn begin_shutdown(&self) {
        self.0.job_capacity.close();
        self.0.shutdown.send_replace(true);
    }

    pub async fn stop_jobs(&self) {
        self.begin_shutdown();
        // 停止通知後に全ジョブが終了するまで待つ。
        let deadline = tokio::time::Instant::now() + std::time::Duration::from_secs(5);
        while self.0.job_capacity.available_permits() < MAX_RUNNING_SYNC_JOBS
            && tokio::time::Instant::now() < deadline
        {
            tokio::time::sleep(std::time::Duration::from_millis(10)).await;
        }
        if self.0.job_capacity.available_permits() < MAX_RUNNING_SYNC_JOBS {
            tracing::warn!("同期ジョブの終了待機がタイムアウトしました");
        }
    }
}

fn is_expired(timestamp: &str, cutoff: i64) -> bool {
    match OffsetDateTime::parse(timestamp, &Rfc3339) {
        Ok(at) => at.unix_timestamp() <= cutoff,
        Err(error) => {
            tracing::warn!(%error, "回収対象の時刻が不正です");
            false
        }
    }
}

fn to_value(value: impl Serialize) -> ToolResult {
    serde_json::to_value(value).map_err(|error| internal(error.into()))
}

fn arguments<T: DeserializeOwned>(
    arguments: Option<serde_json::Map<String, Value>>,
) -> Result<T, ErrorData> {
    serde_json::from_value(Value::Object(arguments.unwrap_or_default()))
        .map_err(|error| invalid(error.to_string()))
}

fn tool<T: JsonSchema>(name: &'static str, description: &'static str) -> Tool {
    let mut schema = schemars::schema_for!(T);
    let schema = schema.ensure_object().clone();
    Tool::new(name, description, schema)
}

static TOOLS: LazyLock<Vec<Tool>> = LazyLock::new(|| {
    vec![
        tool::<DocumentsInput>(
            "upsert_documents",
            "Markdown 文書を追加・更新する。各文書の content、url、local_path は排他。同一ハッシュの文書は Embedding を再利用する。",
        ),
        tool::<DeleteInput>(
            "delete_documents",
            "指定の path を series から切り離す。どの series からも参照されなくなった対象レコードは物理削除する。",
        ),
        tool::<SeriesInput>(
            "delete_series",
            "KEY 内の全レコードから指定の series を取り除く。他の series から参照されない対象レコードは物理削除する。",
        ),
        tool::<QueryInput>(
            "query",
            "Embedding・BM25・全文一致で候補チャンクを検索する。origin_signals で各候補の検出経路を確認できる。rerank は候補を LLM で並べ替える。",
        ),
        tool::<EmptyInput>("list_indexes", "利用可能なインデックスを列挙する"),
        tool::<KeyInput>(
            "trash_index",
            "インデックスをゴミ箱に移す。保持期間が過ぎると自動で最終処分する。",
        ),
        tool::<EmptyInput>("list_trashed_indexes", "ゴミ箱のインデックスを列挙する"),
        tool::<KeyInput>("restore_index", "ゴミ箱からインデックスを復活する"),
        tool::<DocumentsInput>(
            "sync_documents",
            "KEY と series に属する文書の完全な一覧を非同期で同期する。一覧にない path はその series から切り離す。空一覧を指定すると全 path を切り離す。返された job_id で進捗を確認する。",
        ),
        tool::<JobInput>("get_sync_status", "同期ジョブの進捗を取得する"),
        tool::<SeriesInput>(
            "schedule_delete_series",
            "series の削除を予約する。保持期間後に物理削除し、同じ series を同期すると予約が解除される。",
        ),
    ]
});

impl ServerHandler for Server {
    fn get_info(&self) -> ServerConfig {
        ServerConfig::new(ServerCapabilities::builder().enable_tools().build())
            .with_server_info(Implementation::new("doc-db", VERSION))
    }

    async fn list_tools(
        &self,
        _: Option<PaginatedRequestParams>,
        _: RequestContext<RoleServer>,
    ) -> Result<ListToolsResult, ErrorData> {
        Ok(ListToolsResult::with_all_items(TOOLS.clone()))
    }

    fn get_tool(&self, name: &str) -> Option<Tool> {
        TOOLS.iter().find(|tool| tool.name == name).cloned()
    }

    async fn call_tool(
        &self,
        request: CallToolRequestParams,
        _: RequestContext<RoleServer>,
    ) -> Result<CallToolResponse, ErrorData> {
        let args = request.arguments;
        let value = match request.name.as_ref() {
            "upsert_documents" => self.upsert(arguments(args)?).await,
            "delete_documents" => self.delete_documents(arguments(args)?).await,
            "delete_series" => self.delete_series(arguments(args)?).await,
            "query" => self.query(arguments(args)?).await,
            "list_indexes" => self.list_indexes(arguments(args)?).await,
            "trash_index" => self.trash(arguments(args)?).await,
            "list_trashed_indexes" => self.list_trashed(arguments(args)?).await,
            "restore_index" => self.restore(arguments(args)?).await,
            "sync_documents" => self.sync(arguments(args)?).await,
            "get_sync_status" => self.get_status(arguments(args)?).await,
            "schedule_delete_series" => self.schedule(arguments(args)?).await,
            _ => Err(invalid("指定のツールが見つかりません")),
        }?;
        Ok(CallToolResult::structured(value).into())
    }
}
