// pending.go は削除予約（pending_deletions テーブル）まわりの Store メソッドを実装する。
// DES-001 §4.1（スキーマ）・§4.5（メソッド仕様）、APP-001 FNC-006 SYN-03/04・GC-01〜04 に対応。
//
// pending_deletions.path の意味:
//   - path = ”（空文字列）: series 全体の削除予約（GC-01、schedule_delete_series 由来）。
//     起動時スイープで DeleteSeriesAll により物理削除される
//   - path が非空: 当該 path の orphan record（どの series からも参照されない record）の
//     物理削除予約（SYN-03、sync_documents の desired-state 切り離し由来）。
//     起動時スイープで DeleteOrphanRecords により回収される
package store

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// DetachSeriesFromPath は指定 key+path の record 群から当該 series の series_keys 行のみを
// 削除する（SYN-03 の即時切り離し。当該 series 指定の検索から直ちに消える）。
//
// record・chunks・embeddings は削除しない。これは既存の DeleteSeries / CleanOtherSeries が持つ
// 「series_keys が空になった record は即時物理削除」という不変条件の意図的な例外である
// （DES-001 §4.5）。record を残す目的は、SYN-04 の自己修復を Embedding 再計算なし
// （API 課金ゼロ）で成立させること。物理削除は削除予約スイープ（ListPendingDeletionsOlderThan +
// SweepOnePendingDeletion）まで遅延する。
//
// 戻り値 orphaned は、切り離し後に当該 key+path 配下にどの series からも参照されない record が
// 存在する場合に true を返す。呼び出し元（sync_documents）は orphaned=true の場合のみ
// MarkDocumentForDeletion で物理削除予約を記録する（他 series が残る record は
// その series の下で生き続けるため予約不要）。
//
// records 行は変化しないため doc_count の更新は行わない。
// 単一トランザクション + s.mu で直列化する。
func (s *Store) DetachSeriesFromPath(ctx context.Context, key, series, path string) (orphaned bool, retErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("store.DetachSeriesFromPath: begin tx: %w", err)
	}
	defer rollbackErrInto(tx, &retErr)

	// 当該 key+path の record 一覧を取得
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM records WHERE key=? AND path=?`, key, path,
	)
	if err != nil {
		return false, fmt.Errorf("store.DetachSeriesFromPath: list records: %w", err)
	}
	var recordIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return false, fmt.Errorf("store.DetachSeriesFromPath: scan record id: %w", err)
		}
		recordIDs = append(recordIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("store.DetachSeriesFromPath: iterate records: %w", err)
	}

	for _, rid := range recordIDs {
		// series の紐付けのみ除去する（record 本体・chunks・embeddings には触れない）
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM series_keys WHERE record_id=? AND series=?`, rid, series,
		); err != nil {
			return false, fmt.Errorf("store.DetachSeriesFromPath: remove series: %w", err)
		}

		// 切り離し後に orphan（series_keys 0 件）になったかを判定する。
		// 切り離し前から orphan だった record も true に含める（削除予約の記録は
		// 冪等 upsert のため、過剰検出しても安全側に倒れる）。
		var cnt int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM series_keys WHERE record_id=?`, rid,
		).Scan(&cnt); err != nil {
			return false, fmt.Errorf("store.DetachSeriesFromPath: count series: %w", err)
		}
		if cnt == 0 {
			orphaned = true
		}
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store.DetachSeriesFromPath: commit: %w", err)
	}
	return orphaned, nil
}

// ListPaths は指定 key+series に現在紐付いている path の一覧を返す（読み取り専用）。
// sync_documents の desired-state 判定（documents に含まれない既存 path の検出、SYN-03）に使う。
// series_keys を JOIN するため、DetachSeriesFromPath で切り離し済みの orphan record は含まれない。
// 読み取りのみのため s.mu は取得しない（KEY 単位の一貫性は呼び出し元の WithKeyLock が担保する）。
func (s *Store) ListPaths(ctx context.Context, key, series string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT r.path FROM records r
         JOIN series_keys sk ON sk.record_id = r.id
         WHERE r.key=? AND sk.series=?`,
		key, series,
	)
	if err != nil {
		return nil, fmt.Errorf("store.ListPaths: %w", err)
	}
	defer rows.Close()

	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("store.ListPaths: scan: %w", err)
		}
		paths = append(paths, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ListPaths: iterate: %w", err)
	}
	return paths, nil
}

// MarkSeriesForDeletion は series 全体の削除予約を記録する（GC-01、schedule_delete_series 用）。
// path=” センチネル行を upsert する（ON CONFLICT DO UPDATE で冪等）。
// 既に同一 series の削除予約が存在した場合は alreadyScheduled=true を返す
// （schedule_delete_series の already_scheduled 出力に使用）。
// 単一トランザクション + s.mu で直列化する。
func (s *Store) MarkSeriesForDeletion(ctx context.Context, key, series string) (alreadyScheduled bool, retErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("store.MarkSeriesForDeletion: begin tx: %w", err)
	}
	defer rollbackErrInto(tx, &retErr)

	// 挿入前に既存行の有無を確認する（alreadyScheduled 判定）
	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_deletions WHERE key=? AND series=? AND path=''`,
		key, series,
	).Scan(&n); err != nil {
		return false, fmt.Errorf("store.MarkSeriesForDeletion: check existing: %w", err)
	}
	alreadyScheduled = n > 0

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO pending_deletions (key, series, path, marked_at) VALUES (?, ?, '', ?)
         ON CONFLICT(key, series, path) DO UPDATE SET marked_at=excluded.marked_at`,
		key, series, nowRFC3339(),
	); err != nil {
		return false, fmt.Errorf("store.MarkSeriesForDeletion: upsert: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store.MarkSeriesForDeletion: commit: %w", err)
	}
	return alreadyScheduled, nil
}

// MarkDocumentForDeletion は path 単位の削除予約を記録する（SYN-03）。
// DetachSeriesFromPath で orphan になった record の物理削除予約として、
// sync_documents から orphaned=true の場合に呼ばれる。upsert のため冪等。
// s.mu で直列化する。
func (s *Store) MarkDocumentForDeletion(ctx context.Context, key, series, path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO pending_deletions (key, series, path, marked_at) VALUES (?, ?, ?, ?)
         ON CONFLICT(key, series, path) DO UPDATE SET marked_at=excluded.marked_at`,
		key, series, path, nowRFC3339(),
	); err != nil {
		return fmt.Errorf("store.MarkDocumentForDeletion: %w", err)
	}
	return nil
}

// ClearPendingDeletion は削除予約行を解除する（SYN-04 の自己修復に使用）。
// path="" を渡すと series 全体の削除予約（GC-01 由来の path=” センチネル行）を解除する。
// 該当行が存在しない場合も何もせず成功する（冪等）。
// s.mu で直列化する。
func (s *Store) ClearPendingDeletion(ctx context.Context, key, series, path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM pending_deletions WHERE key=? AND series=? AND path=?`,
		key, series, path,
	); err != nil {
		return fmt.Errorf("store.ClearPendingDeletion: %w", err)
	}
	return nil
}

// ListPendingDeletions は当該 key+series の削除予約を 1 回で取得する。
//
// 戻り値:
//   - paths: path 単位予約（path が非空の行）の一覧
//   - seriesWide: series 全体予約（GC-01 由来、path=” 行）の有無
//
// sync_documents の fn 冒頭で呼び、補償 + 予約解除（DES-001 §4.5 [MANDATORY]）の対象 path と
// SYN-04 の series 全体予約解除の要否を判定する。読み取りのみのため s.mu は取得しない。
func (s *Store) ListPendingDeletions(ctx context.Context, key, series string) (paths []string, seriesWide bool, err error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT path FROM pending_deletions WHERE key=? AND series=?`,
		key, series,
	)
	if err != nil {
		return nil, false, fmt.Errorf("store.ListPendingDeletions: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, false, fmt.Errorf("store.ListPendingDeletions scan: %w", err)
		}
		if p == "" {
			seriesWide = true
		} else {
			paths = append(paths, p)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("store.ListPendingDeletions: iterate: %w", err)
	}
	return paths, seriesWide, nil
}

// DeleteOrphanRecords は指定 key+path の record のうち series_keys が 0 件のもののみを
// 物理削除する（chunks / embeddings は CASCADE、BM25 整合は deleteRecordWithBM25Tx に準拠）。
// series の紐付きが残る record には一切触れないため、冪等かつ常に安全である
// （stale な削除予約に対して呼ばれても live record を壊さない。DES-001 §4.5）。
//
// 呼び出し元は 2 箇所:
//   - sync_documents の fn 内で ClearPendingDeletion の直前（CleanOtherSeries の個別失敗の補償）
//   - SweepOnePendingDeletion の path 単位処理（起動時・定期スイープ共通）
//
// record 削除を伴うため doc_count を更新する。
// WithKeyLock は内部で取得しない（呼び出し元が保持済み。DES-001 §4.3）。
// 単一トランザクション + s.mu で直列化する。
func (s *Store) DeleteOrphanRecords(ctx context.Context, key, path string) (removed int, retErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store.DeleteOrphanRecords: begin tx: %w", err)
	}
	defer rollbackErrInto(tx, &retErr)

	// orphan（どの series からも参照されない record）のみを対象にする
	rows, err := tx.QueryContext(ctx,
		`SELECT r.id FROM records r
         WHERE r.key=? AND r.path=?
           AND NOT EXISTS (SELECT 1 FROM series_keys sk WHERE sk.record_id = r.id)`,
		key, path,
	)
	if err != nil {
		return 0, fmt.Errorf("store.DeleteOrphanRecords: list orphans: %w", err)
	}
	var recordIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("store.DeleteOrphanRecords: scan record id: %w", err)
		}
		recordIDs = append(recordIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("store.DeleteOrphanRecords: iterate: %w", err)
	}

	for _, rid := range recordIDs {
		if err := s.deleteRecordWithBM25Tx(ctx, tx, rid); err != nil {
			return 0, err
		}
		removed++
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store.DeleteOrphanRecords: commit: %w", err)
	}

	// doc_count を更新（派生値。Commit 後も s.mu を保持したまま実行: DES-001 §4.2）
	if removed > 0 {
		if err := s.updateDocCountLocked(ctx, key); err != nil {
			return removed, err
		}
	}
	return removed, nil
}

// PendingDeletionEntry は ListPendingDeletionsOlderThan が返す削除予約 1 件分の識別子。
// path="" は series 全体予約（GC-01 由来）、path が非空は path 単位予約（SYN-03 由来）を表す
// （pending_deletions.path の意味は本ファイル冒頭コメント参照）。
type PendingDeletionEntry struct {
	Key      string
	Series   string
	Path     string
	MarkedAt string // ゴミ箱投入・削除予約日時（RFC3339）。ログ記録用（DES-001 §8.5）
}

// ListPendingDeletionsOlderThan は marked_at が cutoff より前（marked_at < cutoff の
// RFC3339 文字列比較）の削除予約一覧を返す読み取り専用メソッド。
// 読み取りのみのため s.mu は取得しない（ListPendingDeletions と同じ方針）。
//
// 呼び出し元（internal/trash.Worker、cmd/docdb/main.go の起動時スイープ）は、
// 返された各エントリを WithKeyLock(entry.Key, ...) で囲んだ上で SweepOnePendingDeletion に
// 渡す（DES-001 §8.5 の分割方針）。
func (s *Store) ListPendingDeletionsOlderThan(ctx context.Context, cutoff time.Time) ([]PendingDeletionEntry, error) {
	cutoffStr := cutoff.UTC().Format(time.RFC3339)

	rows, err := s.db.QueryContext(ctx,
		`SELECT key, series, path, marked_at FROM pending_deletions WHERE marked_at < ?`,
		cutoffStr,
	)
	if err != nil {
		return nil, fmt.Errorf("store.ListPendingDeletionsOlderThan: %w", err)
	}
	defer rows.Close()

	var entries []PendingDeletionEntry
	for rows.Next() {
		var e PendingDeletionEntry
		if err := rows.Scan(&e.Key, &e.Series, &e.Path, &e.MarkedAt); err != nil {
			return nil, fmt.Errorf("store.ListPendingDeletionsOlderThan: scan: %w", err)
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ListPendingDeletionsOlderThan: iterate: %w", err)
	}
	return entries, nil
}

// SweepOnePendingDeletion は 1 件（1 KEY 分）の削除予約を物理削除し、予約を解除する。
//
//   - path=” の行（series 全体予約）: 既存 DeleteSeriesAll を無改造で再利用する
//     （他 series が参照する record は保持する安全な不変条件込み）
//   - path が非空の行（path 単位予約）: DeleteOrphanRecords で orphan のみ回収する。
//     DeleteSeries は使わない: stale な予約行（Clear 失敗・upsert_documents 経由の復活等）が
//     残っていた場合、DeleteSeries の series 剥がしは復活済み record から series を剥がして
//     破壊し得る。DeleteOrphanRecords は series 紐付きが残る record に一切触れないため、
//     stale 行は 0 件処理の冪等動作で行だけが消える（DES-001 §4.5）
//
// 成功した場合のみ pending_deletions から該当行を削除する。個別失敗は silent failure 禁止
// 方針（CLAUDE.md、GC-04）に従い slog.Error でログしてから error を返す。失敗行は予約が
// 保持されるため、次回のスイープ機会（起動時 or internal/trash.Worker の定期実行）で再試行される。
//
// [ロックは呼び出し元の責務] 本メソッドは WithKeyLock を内部で取得しない（DES-001 §4.3）。
// KEY 単位排他が必要な呼び出し元が、本メソッド呼び出し全体を 1 回の WithKeyLock(entry.Key, ...)
// で囲むこと（DES-001 §8.5）。
func (s *Store) SweepOnePendingDeletion(ctx context.Context, entry PendingDeletionEntry) error {
	var sweepErr error
	if entry.Path == "" {
		// series 全体予約（GC-01 由来）: 既存 DeleteSeriesAll を無改造で再利用
		_, _, sweepErr = s.DeleteSeriesAll(ctx, entry.Key, entry.Series)
	} else {
		// path 単位予約（SYN-03 由来）: orphan のみ回収（live record には不触）
		_, sweepErr = s.DeleteOrphanRecords(ctx, entry.Key, entry.Path)
	}
	if sweepErr != nil {
		slog.Error("store: 削除予約スイープ個別失敗（次回スイープで再試行）",
			"key", entry.Key, "series", entry.Series, "path", entry.Path, "error", sweepErr)
		return fmt.Errorf(
			"store.SweepOnePendingDeletion: key=%q series=%q path=%q: %w",
			entry.Key, entry.Series, entry.Path, sweepErr)
	}

	// 成功した行は予約を解除する。解除失敗は次回スイープの再試行で回収される
	// （両処理とも冪等）が、silent failure 禁止のためログしてから error を返す
	if err := s.ClearPendingDeletion(ctx, entry.Key, entry.Series, entry.Path); err != nil {
		slog.Error("store: 削除予約スイープの予約行削除失敗（次回スイープで再試行）",
			"key", entry.Key, "series", entry.Series, "path", entry.Path, "error", err)
		return fmt.Errorf(
			"store.SweepOnePendingDeletion: clear key=%q series=%q path=%q: %w",
			entry.Key, entry.Series, entry.Path, err)
	}
	return nil
}
