// Package store は SQLite の読み書き・トランザクション管理を担う。
// WAL モード + Go 側 Mutex による並行アクセス制御。
// 書き込み操作: Mutex で直列化。読み取り操作: Mutex なし（WAL が担う）。
package store

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"runtime"
	"sync"

	_ "modernc.org/sqlite" // SQLite ドライバ登録
)

// -----------------------------------------------------------------------
// 型定義
// -----------------------------------------------------------------------

// ExpiryPolicy は KEY ごとの廃棄ポリシー設定を表す（JSON 表現で keys.expiry_policy に保存）。
type ExpiryPolicy struct {
	TTLDays   *int `json:"ttl_days,omitempty"`
	MaxChunks *int `json:"max_chunks,omitempty"`
}

// KeyInfo は ListKeys の戻り値要素。
type KeyInfo struct {
	Key            string        `json:"key"`
	Series         []string      `json:"series"`
	DocCount       int           `json:"doc_count"`
	LastUpdatedAt  string        `json:"last_updated_at"`
	LastAccessedAt string        `json:"last_accessed_at"`
	ExpiryPolicy   *ExpiryPolicy `json:"expiry_policy,omitempty"`
}

// Record は UpsertRecord に渡す入力データ。
type Record struct {
	Key         string
	Path        string
	ContentHash string // SHA-256 hex
	Series      string
	Chunks      []ChunkInput
}

// ChunkInput はチャンク分割後の1チャンク分データ。
type ChunkInput struct {
	ChunkIndex  int
	HeadingPath string
	Text        string
	Vector      []float32 // Embedder が生成したベクトル（nil の場合は Embedding 未生成）
}

// Chunk は検索パイプラインが必要とするデータを含む。
type Chunk struct {
	ID          int64
	RecordID    int64
	Key         string
	Path        string
	HeadingPath string
	Text        string
	Vector      []float32
	SeriesKeys  []string
}

// -----------------------------------------------------------------------
// Store
// -----------------------------------------------------------------------

// Store は SQLite の読み書き・トランザクション管理を行う。
// 書き込みは mu で直列化する。読み取りは mu を取得しない（WAL 並行読み取りを活用）。
type Store struct {
	db  *sql.DB
	mu  sync.Mutex
	dim int // Embedding ベクトル次元数（起動時に検証）
}

// New は Store を初期化して返す。
// dbPath はファイルパス（インメモリ ":memory:" も可）。
// expectedDim は環境変数 DOCDB_EMBED_DIM から渡される期待次元数。
func New(dbPath string, expectedDim int) (*Store, error) {
	dsn := buildDSN(dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store.New: open db: %w", err)
	}

	// 接続プール設定（DES-001 §4.2）
	// SetMaxOpenConns(1) は採用しない。最低 2 を保証する。
	n := runtime.GOMAXPROCS(0)
	if n < 2 {
		n = 2
	}
	db.SetMaxOpenConns(n)
	db.SetMaxIdleConns(n)

	s := &Store{db: db, dim: expectedDim}

	if err := s.initSchema(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("store.New: init schema: %w", err)
	}

	if err := s.checkDim(context.Background(), expectedDim); err != nil {
		db.Close()
		return nil, err
	}

	return s, nil
}

// buildDSN は modernc.org/sqlite の DSN 文字列を組み立てる。
// _pragma=foreign_keys(1) と _pragma=busy_timeout(5000) は per-connection。
// journal_mode=WAL は file-level で永続化されるが DSN に含めて確実に設定する。
func buildDSN(dbPath string) string {
	if dbPath == ":memory:" || dbPath == "file::memory:?cache=shared" {
		// インメモリはファイル URI 不要
		return dbPath +
			"?_pragma=foreign_keys(1)" +
			"&_pragma=busy_timeout(5000)"
	}
	return "file:" + dbPath +
		"?_pragma=foreign_keys(1)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)"
}

// Close は DB 接続を閉じる。
func (s *Store) Close() error {
	return s.db.Close()
}

// -----------------------------------------------------------------------
// スキーマ初期化
// -----------------------------------------------------------------------

// initSchema は起動時に CREATE TABLE IF NOT EXISTS を実行する。
func (s *Store) initSchema(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	const ddl = `
CREATE TABLE IF NOT EXISTS keys (
    key              TEXT PRIMARY KEY,
    doc_count        INTEGER NOT NULL DEFAULT 0,
    last_accessed_at TEXT NOT NULL,
    last_updated_at  TEXT NOT NULL,
    expiry_policy    TEXT
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

-- bm25_stats / bm25_df は廃止された（v0.1.2 で削除）。
-- reference doc-db SKILL と同方式で query 時に substring match で TF/DF を都度計算するため、
-- 事前 token 集計テーブルは不要になった。CASCADE 不要のため schema からも除去する。
-- 既存 DB に残っている場合は次の DROP で除去する（IF EXISTS で冪等）。
DROP TABLE IF EXISTS bm25_stats;
DROP TABLE IF EXISTS bm25_df;
`
	_, err := s.db.ExecContext(ctx, ddl)
	return err
}

// checkDim は起動時に embeddings テーブルの dim が expectedDim と一致することを確認する。
// テーブルが空（新規 DB）の場合はスキップする（DES-001 §4.1）。
func (s *Store) checkDim(ctx context.Context, expectedDim int) error {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT dim FROM embeddings`)
	if err != nil {
		return fmt.Errorf("store: checkDim: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var dim int
		if err := rows.Scan(&dim); err != nil {
			return fmt.Errorf("store: checkDim scan: %w", err)
		}
		if dim != expectedDim {
			return fmt.Errorf(
				"store: embedding dimension mismatch: DB has %d, expected %d. "+
					"モデル変更後の DB 再構築が必要です",
				dim, expectedDim,
			)
		}
	}
	return rows.Err()
}

// -----------------------------------------------------------------------
// 公開メソッド — 読み取り
// -----------------------------------------------------------------------

// FindRecord は同一 key+path+content_hash の record ID を返す。
// 存在しない場合は (0, nil) を返す。
func (s *Store) FindRecord(ctx context.Context, key, path, contentHash string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM records WHERE key=? AND path=? AND content_hash=?`,
		key, path, contentHash,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store.FindRecord: %w", err)
	}
	return id, nil
}

// KeyExists は key が存在するかを返す（query 開始時の存在確認用）。
// 読み取り操作のため Mutex を取得しない。
func (s *Store) KeyExists(ctx context.Context, key string) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM keys WHERE key = ?`, key,
	).Scan(&n); err != nil {
		return false, fmt.Errorf("store.KeyExists: %w", err)
	}
	return n > 0, nil
}

// HasRecord は key+path に records が 1 件でも存在するかを返す（delete_documents の DEL-02 用）。
// 読み取り操作のため Mutex を取得しない。
func (s *Store) HasRecord(ctx context.Context, key, path string) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM records WHERE key=? AND path=?`, key, path,
	).Scan(&n); err != nil {
		return false, fmt.Errorf("store.HasRecord: %w", err)
	}
	return n > 0, nil
}

// GetChunksForSearch は key（series 指定時はフィルタ）の全チャンクを返す。
// 読み取り操作のため Mutex を取得しない。
func (s *Store) GetChunksForSearch(ctx context.Context, key string, series string) ([]Chunk, error) {
	var (
		query string
		args  []any
	)

	if series == "" {
		query = `
SELECT c.id, c.record_id, r.key, r.path, c.heading_path, c.text, e.vector
FROM chunks c
JOIN records r ON c.record_id = r.id
LEFT JOIN embeddings e ON e.chunk_id = c.id
WHERE r.key = ?
`
		args = []any{key}
	} else {
		query = `
SELECT c.id, c.record_id, r.key, r.path, c.heading_path, c.text, e.vector
FROM chunks c
JOIN records r ON c.record_id = r.id
LEFT JOIN embeddings e ON e.chunk_id = c.id
JOIN series_keys sk ON sk.record_id = r.id
WHERE r.key = ? AND sk.series = ?
`
		args = []any{key, series}
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store.GetChunksForSearch: %w", err)
	}
	defer rows.Close()

	var chunks []Chunk
	for rows.Next() {
		var c Chunk
		var vectorBlob []byte
		if err := rows.Scan(&c.ID, &c.RecordID, &c.Key, &c.Path, &c.HeadingPath, &c.Text, &vectorBlob); err != nil {
			return nil, fmt.Errorf("store.GetChunksForSearch scan: %w", err)
		}
		if len(vectorBlob) > 0 {
			c.Vector, err = decodeBlobToFloat32(vectorBlob)
			if err != nil {
				return nil, fmt.Errorf("store.GetChunksForSearch decode vector: %w", err)
			}
		}
		chunks = append(chunks, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// series_keys を取得して各チャンクに付与する
	for i := range chunks {
		skeys, err := s.fetchSeriesKeys(ctx, chunks[i].RecordID)
		if err != nil {
			return nil, err
		}
		chunks[i].SeriesKeys = skeys
	}

	return chunks, nil
}

// fetchSeriesKeys は record_id に紐づく series 一覧を返す（読み取り専用ヘルパー）。
func (s *Store) fetchSeriesKeys(ctx context.Context, recordID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT series FROM series_keys WHERE record_id = ?`, recordID,
	)
	if err != nil {
		return nil, fmt.Errorf("store.fetchSeriesKeys: %w", err)
	}
	defer rows.Close()
	var ss []string
	for rows.Next() {
		var series string
		if err := rows.Scan(&series); err != nil {
			return nil, err
		}
		ss = append(ss, series)
	}
	return ss, rows.Err()
}

// ListKeys は全 KEY の情報一覧を返す（MNG-01 対応）。
func (s *Store) ListKeys(ctx context.Context) ([]KeyInfo, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT key, doc_count, last_updated_at, last_accessed_at, expiry_policy FROM keys ORDER BY key`,
	)
	if err != nil {
		return nil, fmt.Errorf("store.ListKeys: %w", err)
	}
	defer rows.Close()

	var result []KeyInfo
	for rows.Next() {
		var ki KeyInfo
		var policyJSON sql.NullString
		if err := rows.Scan(&ki.Key, &ki.DocCount, &ki.LastUpdatedAt, &ki.LastAccessedAt, &policyJSON); err != nil {
			return nil, fmt.Errorf("store.ListKeys scan: %w", err)
		}
		if policyJSON.Valid && policyJSON.String != "" {
			ki.ExpiryPolicy = &ExpiryPolicy{}
			if err := json.Unmarshal([]byte(policyJSON.String), ki.ExpiryPolicy); err != nil {
				return nil, fmt.Errorf("store.ListKeys: parse expiry_policy: %w", err)
			}
		}
		result = append(result, ki)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// series 一覧を追加
	for i := range result {
		series, err := s.fetchSeriesForKey(ctx, result[i].Key)
		if err != nil {
			return nil, err
		}
		result[i].Series = series
	}

	return result, nil
}

// fetchSeriesForKey は key に紐づく series 一覧を返す（読み取り専用ヘルパー）。
func (s *Store) fetchSeriesForKey(ctx context.Context, key string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT sk.series
         FROM series_keys sk
         JOIN records r ON r.id = sk.record_id
         WHERE r.key = ?
         ORDER BY sk.series`,
		key,
	)
	if err != nil {
		return nil, fmt.Errorf("store.fetchSeriesForKey: %w", err)
	}
	defer rows.Close()
	var ss []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		ss = append(ss, s)
	}
	return ss, rows.Err()
}

// -----------------------------------------------------------------------
// 公開メソッド — 書き込み
// -----------------------------------------------------------------------

// AppendSeries は既存 record に series を追記する（DIF-02 経路）。
// Mutex を取得して直列化する。
func (s *Store) AppendSeries(ctx context.Context, recordID int64, series string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO series_keys (record_id, series) VALUES (?, ?)`,
		recordID, series,
	)
	if err != nil {
		return fmt.Errorf("store.AppendSeries: %w", err)
	}
	return nil
}

// CleanOtherSeries は同一 key+path+series を持つ exceptRecordID 以外の record から
// series を除去し、series_keys が空になった record を削除する（DES-001 §4.3）。
// DIF-02・DIF-03 の両経路で呼び出される。
// Mutex を取得して直列化する。
func (s *Store) CleanOtherSeries(ctx context.Context, key, path, series string, exceptRecordID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.cleanOtherSeriesLocked(ctx, key, path, series, exceptRecordID)
}

// AppendAndCleanSeries は DIF-02 経路で必要な AppendSeries + CleanOtherSeries を
// 単一の Mutex 取得内で原子的に実行する複合メソッド（DES-001 §4.2, §4.3）。
//
// AppendSeries と CleanOtherSeries を個別に呼ぶと 2 呼び出し間でロックが外れ、
// 別ゴルーチンが割り込んで「同一 key+path+series が複数 record に存在する」状態を
// 作れてしまう。本メソッドはその競合を排除する。
//
// 引数:
//   - recordID: series を追記する対象 record の ID
//   - key, path, series: CleanOtherSeries 用のフィルタ条件
func (s *Store) AppendAndCleanSeries(ctx context.Context, recordID int64, key, path, series string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// AppendSeries のロジック（Mutex 取得済みのため直接 ExecContext）
	if _, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO series_keys (record_id, series) VALUES (?, ?)`,
		recordID, series,
	); err != nil {
		return fmt.Errorf("store.AppendAndCleanSeries: append series: %w", err)
	}

	// CleanOtherSeries のロジック（Mutex 取得済みの内部実装を呼ぶ）
	return s.cleanOtherSeriesLocked(ctx, key, path, series, recordID)
}

// cleanOtherSeriesLocked は Mutex 取得済みの状態で呼ばれる内部実装。
// CleanOtherSeries（公開メソッド）から呼ばれる。
// DES-001 §6.2 の原子性要件を満たすため、全操作を単一トランザクションで実行する。
func (s *Store) cleanOtherSeriesLocked(ctx context.Context, key, path, series string, exceptRecordID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store.CleanOtherSeries: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// 対象 record_id 一覧を取得（except 以外の同一 key+path で series を持つ record）
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM records WHERE key=? AND path=? AND id!=?`,
		key, path, exceptRecordID,
	)
	if err != nil {
		return fmt.Errorf("store.CleanOtherSeries: list records: %w", err)
	}
	var targetIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		targetIDs = append(targetIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, rid := range targetIDs {
		// series を除去
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM series_keys WHERE record_id=? AND series=?`, rid, series,
		); err != nil {
			return fmt.Errorf("store.CleanOtherSeries: remove series: %w", err)
		}

		// series_keys が空になった record を削除（BM25 整合性を保ってから）
		var cnt int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM series_keys WHERE record_id=?`, rid,
		).Scan(&cnt); err != nil {
			return fmt.Errorf("store.CleanOtherSeries: count series: %w", err)
		}
		if cnt == 0 {
			if err := s.deleteRecordWithBM25Tx(ctx, tx, rid); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// UpsertRecord は key+path の embedding record を新規作成または更新する（DIF-03 経路）。
// 1トランザクションで records・series_keys・chunks・embeddings・bm25_stats/bm25_df・keys を操作する。
// Mutex を取得して直列化する。
func (s *Store) UpsertRecord(ctx context.Context, rec Record) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store.UpsertRecord: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	now := nowRFC3339()

	// records に upsert（UNIQUE(key, path, content_hash)）
	result, err := tx.ExecContext(ctx,
		`INSERT INTO records (key, path, content_hash, created_at, updated_at)
         VALUES (?, ?, ?, ?, ?)
         ON CONFLICT(key, path, content_hash) DO UPDATE SET updated_at=excluded.updated_at`,
		rec.Key, rec.Path, rec.ContentHash, now, now,
	)
	if err != nil {
		return 0, fmt.Errorf("store.UpsertRecord: insert record: %w", err)
	}

	recordID, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store.UpsertRecord: LastInsertId: %w", err)
	}
	if recordID == 0 {
		// ON CONFLICT の場合 LastInsertId が 0 になるため SELECT で取得
		if err2 := tx.QueryRowContext(ctx,
			`SELECT id FROM records WHERE key=? AND path=? AND content_hash=?`,
			rec.Key, rec.Path, rec.ContentHash,
		).Scan(&recordID); err2 != nil {
			return 0, fmt.Errorf("store.UpsertRecord: fetch record id: %w", err2)
		}
	}

	// series_keys に追記
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO series_keys (record_id, series) VALUES (?, ?)`,
		recordID, rec.Series,
	); err != nil {
		return 0, fmt.Errorf("store.UpsertRecord: insert series_key: %w", err)
	}

	// 既存チャンクを削除（再 upsert 時の入れ替え）してから新規挿入
	// 削除前に BM25 term を保存しておく
	if err := s.deleteChunksForRecordTx(ctx, tx, rec.Key, recordID); err != nil {
		return 0, err
	}

	// チャンクと埋め込みを挿入
	for _, chunk := range rec.Chunks {
		chunkResult, err := tx.ExecContext(ctx,
			`INSERT INTO chunks (record_id, chunk_index, heading_path, text) VALUES (?, ?, ?, ?)`,
			recordID, chunk.ChunkIndex, chunk.HeadingPath, chunk.Text,
		)
		if err != nil {
			return 0, fmt.Errorf("store.UpsertRecord: insert chunk: %w", err)
		}
		chunkID, err := chunkResult.LastInsertId()
		if err != nil {
			return 0, fmt.Errorf("store.UpsertRecord: chunk last insert id: %w", err)
		}

		// Embedding が存在する場合のみ保存（M2: 部分 record 保存）
		if len(chunk.Vector) > 0 {
			blob, err := encodeFloat32ToBlob(chunk.Vector)
			if err != nil {
				return 0, fmt.Errorf("store.UpsertRecord: encode vector: %w", err)
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT OR REPLACE INTO embeddings (chunk_id, vector, dim) VALUES (?, ?, ?)`,
				chunkID, blob, len(chunk.Vector),
			); err != nil {
				return 0, fmt.Errorf("store.UpsertRecord: insert embedding: %w", err)
			}
		}

		_ = chunkID // BM25 統計は廃止（v0.1.2: search 時に substring match で都度計算）
	}

	// keys テーブルを upsert
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO keys (key, doc_count, last_accessed_at, last_updated_at)
         VALUES (?, 1, ?, ?)
         ON CONFLICT(key) DO UPDATE SET
             doc_count = (SELECT COUNT(DISTINCT path) FROM records WHERE key=excluded.key),
             last_updated_at = excluded.last_updated_at`,
		rec.Key, now, now,
	); err != nil {
		return 0, fmt.Errorf("store.UpsertRecord: upsert key: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store.UpsertRecord: commit: %w", err)
	}

	return recordID, nil
}

// DeleteSeries は指定 key+paths から series を除去し、
// series_keys が空になった record を削除する（FNC-002 DEL-01）。
// DES-001 §6.2 の原子性要件を満たすため、削除ループ全体を単一トランザクションで実行する。
// doc_count 更新は Commit 後に Mutex を保持したまま実行する（DES-001 §4.2）。
// Mutex を取得して直列化する。
func (s *Store) DeleteSeries(ctx context.Context, key, series string, paths []string) error {
	s.mu.Lock()
	// defer ではなく明示的に Unlock する。updateDocCountLocked 呼び出しまで Mutex を保持する。
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store.DeleteSeries: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	for _, path := range paths {
		// path に対応する record 一覧を取得
		rows, err := tx.QueryContext(ctx,
			`SELECT id FROM records WHERE key=? AND path=?`, key, path,
		)
		if err != nil {
			return fmt.Errorf("store.DeleteSeries: list records for path %q: %w", path, err)
		}
		var recordIDs []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			recordIDs = append(recordIDs, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		for _, rid := range recordIDs {
			// series を除去
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM series_keys WHERE record_id=? AND series=?`, rid, series,
			); err != nil {
				return fmt.Errorf("store.DeleteSeries: remove series: %w", err)
			}

			// series_keys が空なら record 削除
			var cnt int
			if err := tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM series_keys WHERE record_id=?`, rid,
			).Scan(&cnt); err != nil {
				return fmt.Errorf("store.DeleteSeries: count series: %w", err)
			}
			if cnt == 0 {
				if err := s.deleteRecordWithBM25Tx(ctx, tx, rid); err != nil {
					return err
				}
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store.DeleteSeries: commit: %w", err)
	}

	// doc_count を更新（派生値。Commit 後も Mutex を保持したまま実行: DES-001 §4.2）
	if err := s.updateDocCountLocked(ctx, key); err != nil {
		return err
	}

	return nil
}

// DeleteKey は指定 KEY のすべてのデータを削除する（MNG-02 対応）。
// DES-001 §6.2 の原子性要件を満たすため、全操作を単一トランザクションで実行する。
// Mutex を取得して直列化する。
func (s *Store) DeleteKey(ctx context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store.DeleteKey: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// record 一覧を取得してから削除（BM25 整合性を保つため）
	rows, err := tx.QueryContext(ctx, `SELECT id FROM records WHERE key=?`, key)
	if err != nil {
		return fmt.Errorf("store.DeleteKey: list records: %w", err)
	}
	var recordIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		recordIDs = append(recordIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, rid := range recordIDs {
		if err := s.deleteRecordWithBM25Tx(ctx, tx, rid); err != nil {
			return err
		}
	}

	// keys レコードを削除
	if _, err := tx.ExecContext(ctx, `DELETE FROM keys WHERE key=?`, key); err != nil {
		return fmt.Errorf("store.DeleteKey: delete key: %w", err)
	}

	return tx.Commit()
}

// KeyLRUInfo は LRU 廃棄で使う KEY のチャンク数情報（DES-001 §8.2）。
type KeyLRUInfo struct {
	Key        string
	ChunkCount int
}

// ListExpiredKeysByTTL は最終アクセスが effective TTL を超えた KEY 名を返す（DES-001 §8.1 EXP-01）。
// effective TTL = COALESCE(keys.expiry_policy.ttl_days, defaultTTLDays)。
// 読み取り操作のため Mutex を取得しない。
func (s *Store) ListExpiredKeysByTTL(ctx context.Context, defaultTTLDays int) ([]string, error) {
	// JSON1 拡張で keys.expiry_policy.ttl_days を抽出し、未設定ならサーバーデフォルトを使う。
	// last_accessed_at は RFC3339 文字列で保存されているため SQLite の datetime() と比較可能。
	rows, err := s.db.QueryContext(ctx, `
SELECT key
FROM keys
WHERE last_accessed_at < datetime('now', '-' || COALESCE(json_extract(expiry_policy, '$.ttl_days'), ?) || ' days')
`, defaultTTLDays)
	if err != nil {
		return nil, fmt.Errorf("store.ListExpiredKeysByTTL: %w", err)
	}
	defer rows.Close()

	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, fmt.Errorf("store.ListExpiredKeysByTTL scan: %w", err)
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// TotalChunkCount はシステム全体のチャンク総数を返す（DES-001 §8.2）。
// 読み取り操作のため Mutex を取得しない。
func (s *Store) TotalChunkCount(ctx context.Context) (int, error) {
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM chunks`).Scan(&total); err != nil {
		return 0, fmt.Errorf("store.TotalChunkCount: %w", err)
	}
	return total, nil
}

// ListKeysByLRU は KEY のチャンク数を last_accessed_at ASC（古い順）で返す（DES-001 §8.2）。
// チャンクが 0 件の KEY は含めない。読み取り操作のため Mutex を取得しない。
func (s *Store) ListKeysByLRU(ctx context.Context) ([]KeyLRUInfo, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT r.key, COUNT(c.id) AS chunk_count
FROM chunks c
JOIN records r ON c.record_id = r.id
GROUP BY r.key
ORDER BY (SELECT last_accessed_at FROM keys WHERE key = r.key) ASC
`)
	if err != nil {
		return nil, fmt.Errorf("store.ListKeysByLRU: %w", err)
	}
	defer rows.Close()

	var result []KeyLRUInfo
	for rows.Next() {
		var info KeyLRUInfo
		if err := rows.Scan(&info.Key, &info.ChunkCount); err != nil {
			return nil, fmt.Errorf("store.ListKeysByLRU scan: %w", err)
		}
		result = append(result, info)
	}
	return result, rows.Err()
}

// SetExpiryPolicy は KEY の廃棄ポリシーを更新する（DES-001 §8.4 EXP-04 / MNG-03）。
// policy が nil の場合は expiry_policy を NULL（サーバーデフォルト適用）にする。
// Mutex を取得して直列化する。
func (s *Store) SetExpiryPolicy(ctx context.Context, key string, policy *ExpiryPolicy) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var policyJSON any
	if policy != nil {
		b, err := json.Marshal(policy)
		if err != nil {
			return fmt.Errorf("store.SetExpiryPolicy: marshal policy: %w", err)
		}
		policyJSON = string(b)
	}

	res, err := s.db.ExecContext(ctx,
		`UPDATE keys SET expiry_policy = ? WHERE key = ?`,
		policyJSON, key,
	)
	if err != nil {
		return fmt.Errorf("store.SetExpiryPolicy: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store.SetExpiryPolicy: rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("store.SetExpiryPolicy: key %q not found", key)
	}
	return nil
}

// TouchKey は key の last_accessed_at を現在時刻に更新する（query 時に呼ぶ）。
// Mutex を取得して直列化する。
func (s *Store) TouchKey(ctx context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := nowRFC3339()
	_, err := s.db.ExecContext(ctx,
		`UPDATE keys SET last_accessed_at=? WHERE key=?`, now, key,
	)
	if err != nil {
		return fmt.Errorf("store.TouchKey: %w", err)
	}
	return nil
}

// -----------------------------------------------------------------------
// BM25 整合性維持ヘルパー（DES-001 §6.2）
// -----------------------------------------------------------------------

// dbExecer は *sql.DB と *sql.Tx の共通インターフェース。
type dbExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// deleteRecordWithBM25Tx は record を削除する共通ヘルパー。
// bm25_stats / bm25_df 廃止 (v0.1.2) 後は CASCADE のみで完結する。
// 命名は履歴上の互換性のため残す。
// 呼び出し元が既に Mutex を保持していること前提。
func (s *Store) deleteRecordWithBM25Tx(ctx context.Context, exec dbExecer, recordID int64) error {
	// CASCADE で chunks・embeddings・series_keys も削除される
	if _, err := exec.ExecContext(ctx,
		`DELETE FROM records WHERE id=?`, recordID,
	); err != nil {
		return fmt.Errorf("store.deleteRecord: %w", err)
	}
	return nil
}

// deleteChunksForRecordTx はトランザクション内で record のチャンクを削除する
// （UpsertRecord の既存チャンク入れ替え時に使用）。
// bm25_stats / bm25_df 廃止後は CASCADE で完結する。
func (s *Store) deleteChunksForRecordTx(ctx context.Context, tx *sql.Tx, key string, recordID int64) error {
	_ = key // bm25_df 廃止により未使用（API 互換性のため引数は残す）
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM chunks WHERE record_id=?`, recordID,
	); err != nil {
		return fmt.Errorf("store.deleteChunksForRecordTx: %w", err)
	}
	return nil
}

// updateDocCountLocked は key.doc_count を distinct path 数で更新する。
// 呼び出し元が s.mu を保持していること前提（DES-001 §4.2）。
func (s *Store) updateDocCountLocked(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE keys SET doc_count = (
             SELECT COUNT(DISTINCT path) FROM records WHERE key=?
         ) WHERE key=?`,
		key, key,
	)
	if err != nil {
		return fmt.Errorf("store.updateDocCountLocked: %w", err)
	}
	return nil
}

// -----------------------------------------------------------------------
// ユーティリティ
// -----------------------------------------------------------------------

// encodeFloat32ToBlob はリトルエンディアン float32 スライスを BLOB にエンコードする。
func encodeFloat32ToBlob(vec []float32) ([]byte, error) {
	buf := make([]byte, len(vec)*4)
	for i, v := range vec {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
	}
	return buf, nil
}

// decodeBlobToFloat32 は BLOB をリトルエンディアン float32 スライスにデコードする。
func decodeBlobToFloat32(blob []byte) ([]float32, error) {
	if len(blob)%4 != 0 {
		return nil, fmt.Errorf("store.decodeBlobToFloat32: invalid blob length %d", len(blob))
	}
	vec := make([]float32, len(blob)/4)
	for i := range vec {
		bits := binary.LittleEndian.Uint32(blob[i*4:])
		vec[i] = math.Float32frombits(bits)
	}
	return vec, nil
}

// nowRFC3339 は現在時刻を RFC3339 形式の文字列で返す。
func nowRFC3339() string {
	// time パッケージのインポートを最小化するため文字列変換はここで行う
	return timeNowRFC3339()
}
