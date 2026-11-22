use anyhow::{Context, Result, ensure};
use rusqlite::{
    Connection, OpenFlags, OptionalExtension, Transaction, TransactionBehavior, params,
};
use serde::Serialize;
use std::{
    collections::{HashMap, HashSet},
    sync::{Arc, Mutex},
    time::Duration,
};

use crate::chunker;
use time::{OffsetDateTime, format_description::well_known::Rfc3339};
use tokio::sync::{Mutex as AsyncMutex, Semaphore};

#[derive(Clone)]
pub struct Store {
    writer: Arc<AsyncMutex<Connection>>,
    readers: Option<Arc<Readers>>,
}

struct Readers {
    connections: Mutex<Vec<Connection>>,
    available: Arc<Semaphore>,
}

#[derive(Serialize)]
pub struct Index {
    key: String,
    series: Option<Vec<String>>,
    doc_count: i64,
    chunk_count: i64,
    last_updated_at: String,
    last_accessed_at: String,
}

pub struct Chunk {
    pub path: String,
    pub heading_path: String,
    pub text: String,
    pub vector: Vec<f32>,
    pub series_keys: Option<Arc<[String]>>,
}

pub enum KeyState {
    Missing,
    Active,
    Trashed,
}

pub struct Pending {
    pub key: String,
    pub series: String,
    pub path: String,
    pub marked_at: String,
}

pub fn now() -> String {
    OffsetDateTime::now_utc()
        .replace_nanosecond(0)
        .expect("ナノ秒は固定の 0")
        .format(&Rfc3339)
        .expect("UTC の現在時刻を RFC3339 に整形")
}

impl Store {
    pub fn open(path: &str, dim: usize) -> Result<Self> {
        let mut connection = Connection::open(path)?;
        connection.busy_timeout(Duration::from_secs(5))?;
        connection.execute_batch(
            "PRAGMA foreign_keys=ON; PRAGMA trusted_schema=OFF; PRAGMA journal_mode=WAL;",
        )?;
        let tx = connection.transaction_with_behavior(TransactionBehavior::Immediate)?;
        tx.execute_batch(include_str!("schema.sql"))?;
        let has_trashed_column: bool = tx.query_row(
            "SELECT EXISTS(SELECT 1 FROM pragma_table_info('keys') WHERE name='trashed_at')",
            [],
            |row| row.get(0),
        )?;
        if !has_trashed_column {
            tx.execute("ALTER TABLE keys ADD COLUMN trashed_at TEXT", [])?;
        }
        let invalid_dim: bool = tx.query_row(
            "SELECT EXISTS(SELECT 1 FROM embeddings WHERE dim != ?1 OR length(vector) != dim * 4)",
            [i64::try_from(dim)?],
            |row| row.get(0),
        )?;
        ensure!(
            !invalid_dim,
            "既存の Embedding と設定の次元数または保存形式が一致しません"
        );
        tx.commit()?;
        let readers = if path == ":memory:" || path.starts_with("file:") {
            None
        } else {
            let count =
                std::thread::available_parallelism().map_or(2, |count| count.get().clamp(2, 4));
            let connections = (0..count)
                .map(|_| {
                    let reader = Connection::open_with_flags(
                        path,
                        OpenFlags::SQLITE_OPEN_READ_ONLY | OpenFlags::SQLITE_OPEN_NO_MUTEX,
                    )?;
                    reader.busy_timeout(Duration::from_secs(5))?;
                    reader.execute_batch("PRAGMA trusted_schema=OFF; PRAGMA query_only=ON;")?;
                    Ok(reader)
                })
                .collect::<rusqlite::Result<_>>()?;
            Some(Arc::new(Readers {
                connections: Mutex::new(connections),
                available: Arc::new(Semaphore::new(count)),
            }))
        };
        Ok(Self {
            writer: Arc::new(AsyncMutex::new(connection)),
            readers,
        })
    }

    pub async fn run<T, F>(&self, f: F) -> Result<T>
    where
        T: Send + 'static,
        F: FnOnce(&mut Connection) -> Result<T> + Send + 'static,
    {
        let mut connection = self.writer.clone().lock_owned().await;
        tokio::task::spawn_blocking(move || f(&mut connection))
            .await
            .context("SQLite の処理が中断されました")?
    }

    pub async fn read<T, F>(&self, f: F) -> Result<T>
    where
        T: Send + 'static,
        F: FnOnce(&mut Connection) -> Result<T> + Send + 'static,
    {
        let Some(readers) = self.readers.clone() else {
            return self.run(f).await;
        };
        let permit = readers.available.clone().acquire_owned().await?;
        tokio::task::spawn_blocking(move || {
            let _permit = permit;
            let mut connection = readers
                .connections
                .lock()
                .map_err(|_| anyhow::anyhow!("SQLite 読み取り接続を取得できません"))?
                .pop()
                .context("SQLite 読み取り接続がありません")?;
            let result = f(&mut connection);
            readers
                .connections
                .lock()
                .map_err(|_| anyhow::anyhow!("SQLite 読み取り接続を返却できません"))?
                .push(connection);
            result
        })
        .await
        .context("SQLite の読み取りが中断されました")?
    }
}

pub fn state(db: &Connection, key: &str) -> Result<KeyState> {
    Ok(
        match db
            .query_row("SELECT trashed_at FROM keys WHERE key=?1", [key], |row| {
                row.get::<_, Option<String>>(0)
            })
            .optional()?
        {
            None => KeyState::Missing,
            Some(None) => KeyState::Active,
            Some(Some(_)) => KeyState::Trashed,
        },
    )
}

fn require_writable(db: &Connection, key: &str) -> Result<()> {
    ensure!(
        !matches!(state(db, key)?, KeyState::Trashed),
        "ゴミ箱にある KEY を変更できません"
    );
    Ok(())
}

fn record_id(db: &Connection, key: &str, path: &str, hash: &str) -> Result<Option<i64>> {
    Ok(db
        .query_row(
            "SELECT id FROM records WHERE key=?1 AND path=?2 AND content_hash=?3",
            params![key, path, hash],
            |row| row.get(0),
        )
        .optional()?)
}

pub fn reuse(db: &mut Connection, key: &str, series: &str, path: &str, hash: &str) -> Result<bool> {
    let tx = db.transaction_with_behavior(TransactionBehavior::Immediate)?;
    require_writable(&tx, key)?;
    let Some(id) = record_id(&tx, key, path, hash)? else {
        return Ok(false);
    };
    attach(&tx, key, series, path, id)?;
    tx.commit()?;
    Ok(true)
}

pub fn save(
    db: &mut Connection,
    key: &str,
    series: &str,
    path: &str,
    hash: &str,
    chunks: Vec<chunker::Chunk>,
    vectors: Vec<Option<Vec<f32>>>,
) -> Result<()> {
    let tx = db.transaction_with_behavior(TransactionBehavior::Immediate)?;
    require_writable(&tx, key)?;
    let timestamp = now();
    let id: i64 = tx.query_row(
        "INSERT INTO records (key,path,content_hash,created_at,updated_at) VALUES (?1,?2,?3,?4,?4)
         ON CONFLICT (key,path,content_hash) DO UPDATE SET updated_at=excluded.updated_at
         RETURNING id",
        params![key, path, hash, timestamp],
        |row| row.get(0),
    )?;
    tx.execute("DELETE FROM chunks WHERE record_id=?1", [id])?;
    {
        let mut insert_chunk = tx.prepare_cached(
            "INSERT INTO chunks (record_id,chunk_index,heading_path,text) VALUES (?1,?2,?3,?4) RETURNING id"
        )?;
        let mut insert_vector =
            tx.prepare_cached("INSERT INTO embeddings (chunk_id,vector,dim) VALUES (?1,?2,?3)")?;
        for (index, (chunk, vector)) in chunks.into_iter().zip(vectors).enumerate() {
            let chunk_id: i64 = insert_chunk.query_row(
                params![id, i64::try_from(index)?, chunk.heading_path, chunk.text],
                |row| row.get(0),
            )?;
            if let Some(vector) = vector {
                let blob: Vec<u8> = vector
                    .iter()
                    .flat_map(|value| value.to_le_bytes())
                    .collect();
                insert_vector.execute(params![chunk_id, blob, i64::try_from(vector.len())?])?;
            }
        }
    }
    tx.execute(
        "INSERT INTO keys (key,doc_count,last_accessed_at,last_updated_at) VALUES (?1,0,?2,?2)
         ON CONFLICT(key) DO UPDATE SET last_updated_at=excluded.last_updated_at",
        params![key, timestamp],
    )?;
    attach(&tx, key, series, path, id)?;
    tx.commit()?;
    Ok(())
}

fn attach(tx: &Transaction<'_>, key: &str, series: &str, path: &str, id: i64) -> Result<()> {
    tx.execute(
        "INSERT OR IGNORE INTO series_keys (record_id,series) VALUES (?1,?2)",
        params![id, series],
    )?;
    tx.execute(
        "DELETE FROM series_keys WHERE series=?3 AND record_id IN
         (SELECT id FROM records WHERE key=?1 AND path=?2 AND id!=?4)",
        params![key, path, series, id],
    )?;
    delete_orphans(tx, key, path)?;
    tx.execute(
        "DELETE FROM pending_deletions WHERE key=?1 AND path=?2 AND series=?3",
        params![key, path, series],
    )?;
    update_count(tx, key)
}

fn delete_orphans(db: &Connection, key: &str, path: &str) -> Result<usize> {
    Ok(db.execute(
        "DELETE FROM records WHERE key=?1 AND path=?2 AND NOT EXISTS
         (SELECT 1 FROM series_keys WHERE record_id=records.id)",
        params![key, path],
    )?)
}

fn update_count(db: &Connection, key: &str) -> Result<()> {
    db.execute(
        "UPDATE keys SET doc_count=(SELECT COUNT(DISTINCT path) FROM records WHERE key=?1) WHERE key=?1",
        [key],
    )?;
    Ok(())
}

pub fn touch(db: &Connection, key: &str) -> Result<()> {
    db.execute(
        "UPDATE keys SET last_accessed_at=?2 WHERE key=?1",
        params![key, now()],
    )?;
    Ok(())
}

pub fn chunks(
    db: &mut Connection,
    key: &str,
    series: &str,
    with_vectors: bool,
) -> Result<Vec<Chunk>> {
    let tx = db.transaction()?;
    let mut series_by_record: HashMap<i64, Vec<String>> = HashMap::new();
    {
        let mut stmt = tx.prepare_cached(
            "SELECT s.record_id,s.series FROM series_keys s JOIN records r ON r.id=s.record_id
             WHERE r.key=?1 ORDER BY s.record_id,s.series",
        )?;
        for row in stmt.query_map([key], |row| Ok((row.get(0)?, row.get(1)?)))? {
            let (id, name) = row?;
            series_by_record.entry(id).or_default().push(name);
        }
    }
    let shared: HashMap<_, Arc<[String]>> = series_by_record
        .into_iter()
        .map(|(id, series)| (id, series.into()))
        .collect();
    let mut stmt = tx.prepare_cached(
        "SELECT r.id,r.path,c.heading_path,c.text,e.vector FROM records r JOIN chunks c ON c.record_id=r.id
         LEFT JOIN embeddings e ON e.chunk_id=c.id AND ?3
         WHERE r.key=?1 AND (?2='' OR EXISTS (SELECT 1 FROM series_keys s WHERE s.record_id=r.id AND s.series=?2))
         ORDER BY c.id"
    )?;
    let mut rows = stmt.query(params![key, series, with_vectors])?;
    let mut chunks = Vec::new();
    while let Some(row) = rows.next()? {
        let id = row.get::<_, i64>(0)?;
        let blob: Option<Vec<u8>> = row.get(4)?;
        let vector = blob.unwrap_or_default();
        let (words, remainder) = vector.as_chunks::<4>();
        ensure!(remainder.is_empty(), "Embedding BLOB の長さが不正です");
        chunks.push(Chunk {
            path: row.get(1)?,
            heading_path: row.get(2)?,
            text: row.get(3)?,
            vector: words.iter().copied().map(f32::from_le_bytes).collect(),
            series_keys: shared.get(&id).cloned(),
        });
    }
    Ok(chunks)
}

pub fn indexes(db: &mut Connection) -> Result<Vec<Index>> {
    let tx = db.transaction()?;
    let mut indexes = {
        let mut stmt = tx.prepare_cached(
            "SELECT k.key,k.doc_count,k.last_updated_at,k.last_accessed_at,COUNT(c.id)
             FROM keys k LEFT JOIN records r ON r.key=k.key LEFT JOIN chunks c ON c.record_id=r.id
             WHERE k.trashed_at IS NULL GROUP BY k.key ORDER BY k.key",
        )?;
        stmt.query_map([], |row| {
            Ok(Index {
                key: row.get(0)?,
                doc_count: row.get(1)?,
                last_updated_at: row.get(2)?,
                last_accessed_at: row.get(3)?,
                chunk_count: row.get(4)?,
                series: None,
            })
        })?
        .collect::<rusqlite::Result<Vec<_>>>()?
    };
    let mut stmt = tx.prepare_cached(
        "SELECT DISTINCT s.series FROM records r JOIN series_keys s ON s.record_id=r.id WHERE r.key=?1 ORDER BY s.series"
    )?;
    for index in &mut indexes {
        let series = stmt
            .query_map([&index.key], |row| row.get(0))?
            .collect::<rusqlite::Result<Vec<_>>>()?;
        if !series.is_empty() {
            index.series = Some(series);
        }
    }
    Ok(indexes)
}

pub fn delete_paths(
    db: &mut Connection,
    key: &str,
    series: &str,
    paths: &[String],
) -> Result<(usize, Vec<String>)> {
    let tx = db.transaction_with_behavior(TransactionBehavior::Immediate)?;
    require_writable(&tx, key)?;
    let mut present = Vec::new();
    let mut missing = Vec::new();
    for path in paths {
        let exists: bool = tx.query_row(
            "SELECT EXISTS(SELECT 1 FROM records WHERE key=?1 AND path=?2)",
            params![key, path],
            |row| row.get(0),
        )?;
        if !exists {
            missing.push(path.clone());
        } else {
            present.push(path);
        }
    }
    for path in &present {
        tx.execute(
            "DELETE FROM series_keys WHERE series=?3 AND record_id IN (SELECT id FROM records WHERE key=?1 AND path=?2)",
            params![key, path, series],
        )?;
        delete_orphans(&tx, key, path)?;
    }
    update_count(&tx, key)?;
    tx.commit()?;
    Ok((present.len(), missing))
}

fn delete_series_tx(tx: &Transaction<'_>, key: &str, series: &str) -> Result<(usize, usize)> {
    let removed = tx.execute(
        "DELETE FROM records WHERE key=?1
         AND EXISTS (SELECT 1 FROM series_keys WHERE record_id=records.id AND series=?2)
         AND NOT EXISTS (SELECT 1 FROM series_keys WHERE record_id=records.id AND series!=?2)",
        params![key, series],
    )?;
    let updated = tx.execute(
        "DELETE FROM series_keys WHERE series=?2 AND record_id IN (SELECT id FROM records WHERE key=?1)",
        params![key, series],
    )?;
    tx.execute(
        "DELETE FROM pending_deletions WHERE key=?1 AND series=?2 AND path=''",
        params![key, series],
    )?;
    update_count(tx, key)?;
    Ok((removed, updated))
}

pub fn delete_series(db: &mut Connection, key: &str, series: &str) -> Result<(usize, usize)> {
    let tx = db.transaction_with_behavior(TransactionBehavior::Immediate)?;
    require_writable(&tx, key)?;
    let counts = delete_series_tx(&tx, key, series)?;
    tx.commit()?;
    Ok(counts)
}

pub fn schedule(db: &Connection, key: &str, series: &str) -> Result<bool> {
    require_writable(db, key)?;
    Ok(db.execute(
        "INSERT INTO pending_deletions (key,series,path,marked_at) VALUES (?1,?2,'',?3) ON CONFLICT DO NOTHING",
        params![key, series, now()],
    )? == 0)
}

pub fn sync_paths(
    db: &mut Connection,
    key: &str,
    series: &str,
    desired: &HashSet<String>,
) -> Result<usize> {
    let tx = db.transaction_with_behavior(TransactionBehavior::Immediate)?;
    require_writable(&tx, key)?;
    let current = {
        let mut stmt = tx.prepare_cached(
            "SELECT DISTINCT r.path FROM records r JOIN series_keys s ON s.record_id=r.id WHERE r.key=?1 AND s.series=?2"
        )?;
        stmt.query_map(params![key, series], |row| row.get::<_, String>(0))?
            .collect::<rusqlite::Result<Vec<_>>>()?
    };
    let mut marked = 0;
    for path in current.iter().filter(|path| !desired.contains(*path)) {
        tx.execute(
            "DELETE FROM series_keys WHERE series=?3 AND record_id IN (SELECT id FROM records WHERE key=?1 AND path=?2)",
            params![key, path, series],
        )?;
        let orphaned: bool = tx.query_row(
            "SELECT EXISTS(SELECT 1 FROM records r WHERE r.key=?1 AND r.path=?2
             AND NOT EXISTS (SELECT 1 FROM series_keys WHERE record_id=r.id))",
            params![key, path],
            |row| row.get(0),
        )?;
        if orphaned {
            tx.execute(
                "INSERT INTO pending_deletions (key,series,path,marked_at) VALUES (?1,?2,?3,?4) ON CONFLICT DO NOTHING",
                params![key, series, path, now()],
            )?;
            marked += 1;
        }
    }
    tx.execute(
        "DELETE FROM pending_deletions WHERE key=?1 AND series=?2 AND path=''",
        params![key, series],
    )?;
    tx.commit()?;
    Ok(marked)
}

pub fn trashed(db: &Connection) -> Result<Vec<(String, String)>> {
    let mut stmt = db.prepare_cached(
        "SELECT key,trashed_at FROM keys WHERE trashed_at IS NOT NULL ORDER BY trashed_at,key",
    )?;
    Ok(stmt
        .query_map([], |row| Ok((row.get(0)?, row.get(1)?)))?
        .collect::<rusqlite::Result<_>>()?)
}

pub fn set_trash(db: &Connection, key: &str, timestamp: Option<&str>) -> Result<bool> {
    Ok(db.execute(
        "UPDATE keys SET trashed_at=?2 WHERE key=?1 AND (trashed_at IS NULL) != (?2 IS NULL)",
        params![key, timestamp],
    )? == 1)
}

pub fn delete_trashed(db: &mut Connection, key: &str, trashed_at: &str) -> Result<()> {
    let tx = db.transaction_with_behavior(TransactionBehavior::Immediate)?;
    let deleted = tx.execute(
        "DELETE FROM keys WHERE key=?1 AND trashed_at=?2",
        params![key, trashed_at],
    )?;
    if deleted != 0 {
        tx.execute("DELETE FROM records WHERE key=?1", [key])?;
        tx.execute("DELETE FROM pending_deletions WHERE key=?1", [key])?;
    }
    tx.commit()?;
    Ok(())
}

pub fn pending(db: &Connection) -> Result<Vec<Pending>> {
    let mut stmt = db.prepare_cached(
        "SELECT key,series,path,marked_at FROM pending_deletions ORDER BY marked_at",
    )?;
    Ok(stmt
        .query_map([], |row| {
            Ok(Pending {
                key: row.get(0)?,
                series: row.get(1)?,
                path: row.get(2)?,
                marked_at: row.get(3)?,
            })
        })?
        .collect::<rusqlite::Result<_>>()?)
}

pub fn sweep_pending(db: &mut Connection, entry: Pending) -> Result<()> {
    let tx = db.transaction_with_behavior(TransactionBehavior::Immediate)?;
    let params = params![entry.key, entry.series, entry.path, entry.marked_at];
    let active: bool = tx.query_row(
        "SELECT EXISTS(SELECT 1 FROM pending_deletions p WHERE p.key=?1 AND p.series=?2 AND p.path=?3 AND p.marked_at=?4
         AND NOT EXISTS (SELECT 1 FROM keys k WHERE k.key=p.key AND k.trashed_at IS NOT NULL))",
        params, |row| row.get(0),
    )?;
    if active {
        if entry.path.is_empty() {
            delete_series_tx(&tx, &entry.key, &entry.series)?;
        } else {
            delete_orphans(&tx, &entry.key, &entry.path)?;
            update_count(&tx, &entry.key)?;
        }
        tx.execute(
            "DELETE FROM pending_deletions WHERE key=?1 AND series=?2 AND path=?3 AND marked_at=?4",
            params,
        )?;
    }
    tx.commit()?;
    Ok(())
}
