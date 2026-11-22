CREATE TABLE IF NOT EXISTS keys (
    key              TEXT PRIMARY KEY,
    doc_count        INTEGER NOT NULL DEFAULT 0,
    last_accessed_at TEXT NOT NULL,
    last_updated_at  TEXT NOT NULL,
    expiry_policy    TEXT,
    trashed_at       TEXT
);

CREATE TABLE IF NOT EXISTS records (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    key          TEXT NOT NULL,
    path         TEXT NOT NULL,
    content_hash TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,
    UNIQUE(key, path, content_hash)
);

CREATE TABLE IF NOT EXISTS series_keys (
    record_id INTEGER NOT NULL REFERENCES records(id) ON DELETE CASCADE,
    series    TEXT NOT NULL,
    PRIMARY KEY (record_id, series)
);

CREATE TABLE IF NOT EXISTS chunks (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    record_id    INTEGER NOT NULL REFERENCES records(id) ON DELETE CASCADE,
    chunk_index  INTEGER NOT NULL,
    heading_path TEXT NOT NULL,
    text         TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS embeddings (
    chunk_id INTEGER PRIMARY KEY REFERENCES chunks(id) ON DELETE CASCADE,
    vector   BLOB NOT NULL,
    dim      INTEGER NOT NULL
);

-- 削除予約テーブル（DES-001 §4.1、SYN-03/GC-01/GC-02）。
-- path = '' は「series 全体の削除予約」を表すセンチネル（GC-01 由来）。
-- path が非空の行は「当該 path の orphan record（どの series からも参照されない record）を
-- 起動時スイープで物理削除せよ」という予約（SYN-03 由来）。
-- SQLite の PRIMARY KEY は NULL の重複を許容するため、NULL ではなく空文字列で区別する。
CREATE TABLE IF NOT EXISTS pending_deletions (
    key       TEXT NOT NULL,
    series    TEXT NOT NULL,
    path      TEXT NOT NULL DEFAULT '',
    marked_at TEXT NOT NULL,
    PRIMARY KEY (key, series, path)
);

-- bm25_stats / bm25_df は廃止された（v0.1.2 で削除）。
-- reference doc-db SKILL と同方式で query 時に substring match で TF/DF を都度計算するため、
-- 事前 token 集計テーブルは不要になった。CASCADE 不要のため schema からも除去する。
-- 既存 DB に残っている場合は次の DROP で除去する（IF EXISTS で冪等）。
DROP TABLE IF EXISTS bm25_stats;
DROP TABLE IF EXISTS bm25_df;
