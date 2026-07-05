// pending.go は削除予約（pending_deletions テーブル）まわりの Store メソッドを実装する。
// DES-003 §3.2（スキーマ）・§3.3（メソッド仕様）、APP-003 SYN-03/04・GC-01〜04 に対応。
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
)

// DetachSeriesFromPath は指定 key+path の record 群から当該 series の series_keys 行のみを
// 削除する（SYN-03 の即時切り離し。当該 series 指定の検索から直ちに消える）。
//
// record・chunks・embeddings は削除しない。これは既存の DeleteSeries / CleanOtherSeries が持つ
// 「series_keys が空になった record は即時物理削除」という不変条件の意図的な例外である
// （DES-003 §3.3）。record を残す目的は、SYN-04 の自己修復を Embedding 再計算なし
// （API 課金ゼロ）で成立させること。物理削除は起動時スイープ（SweepPendingDeletions）まで遅延する。
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
// sync_documents の fn 冒頭で呼び、補償 + 予約解除（DES-003 §3.3 [MANDATORY]）の対象 path と
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
// （stale な削除予約に対して呼ばれても live record を壊さない。DES-003 §3.3）。
//
// 呼び出し元は 2 箇所:
//   - sync_documents の fn 内で ClearPendingDeletion の直前（CleanOtherSeries の個別失敗の補償）
//   - SweepPendingDeletions の path 単位処理（起動時）
//
// record 削除を伴うため doc_count を更新する。
// WithKeyLock は内部で取得しない（呼び出し元が保持済み、または起動時専用で不要。DES-003 §3.5.2）。
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

// SweepPendingDeletions は pending_deletions の全行を処理する起動時スイープ（GC-02〜04）。
//
//   - path=” の行（series 全体予約）: 既存 DeleteSeriesAll を無改造で再利用する
//     （他 series が参照する record は保持する安全な不変条件込み）
//   - path が非空の行（path 単位予約）: DeleteOrphanRecords で orphan のみ回収する。
//     DeleteSeries は使わない: stale な予約行（Clear 失敗・upsert_documents 経由の復活等）が
//     残っていた場合、DeleteSeries の series 剥がしは復活済み record から series を剥がして
//     破壊し得る。DeleteOrphanRecords は series 紐付きが残る record に一切触れないため、
//     stale 行は 0 件処理の冪等動作で行だけが消える（DES-003 §3.3）
//
// 成功した行は pending_deletions から削除する。個別失敗はログ + errs に集約して処理を継続する
// （silent failure 禁止方針、GC-04）。行の消し忘れ・失敗行は次回起動時に再試行されるだけで
// 安全（両関数とも対象が既に無ければ 0 件処理で冪等）。
//
// [起動時専用] 本メソッドはサーバー起動時（MCP リクエストを受け付ける前、DB 統計表示より前。
// GC-03）にのみ呼ばれる前提であり、WithKeyLock を取得しない（並行する書き込みが存在しない
// 時間帯のため不要）。将来、起動時以外（手動トリガー等）でスイープを実行する変更を加える場合は、
// この前提が崩れるため各行ごとに WithKeyLock で囲むよう設計を見直すこと（DES-003 §3.3）。
func (s *Store) SweepPendingDeletions(ctx context.Context) (processed int, errs []error) {
	type pendingRow struct {
		key, series, path string
	}

	// 全予約行を読み取る（読み取りのみのため s.mu は不要。以降の各処理は
	// 呼び出す各メソッドが個別に s.mu を取得する）
	rows, err := s.db.QueryContext(ctx,
		`SELECT key, series, path FROM pending_deletions`,
	)
	if err != nil {
		return 0, []error{fmt.Errorf("store.SweepPendingDeletions: list: %w", err)}
	}
	var pending []pendingRow
	for rows.Next() {
		var r pendingRow
		if err := rows.Scan(&r.key, &r.series, &r.path); err != nil {
			rows.Close()
			return 0, []error{fmt.Errorf("store.SweepPendingDeletions: scan: %w", err)}
		}
		pending = append(pending, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, []error{fmt.Errorf("store.SweepPendingDeletions: iterate: %w", err)}
	}

	for _, r := range pending {
		var sweepErr error
		if r.path == "" {
			// series 全体予約（GC-01 由来）: 既存 DeleteSeriesAll を無改造で再利用
			_, _, sweepErr = s.DeleteSeriesAll(ctx, r.key, r.series)
		} else {
			// path 単位予約（SYN-03 由来）: orphan のみ回収（live record には不触）
			_, sweepErr = s.DeleteOrphanRecords(ctx, r.key, r.path)
		}
		if sweepErr != nil {
			slog.Error("store: 起動時スイープ個別失敗（次回起動時に再試行）",
				"key", r.key, "series", r.series, "path", r.path, "error", sweepErr)
			errs = append(errs, fmt.Errorf(
				"store.SweepPendingDeletions: key=%q series=%q path=%q: %w",
				r.key, r.series, r.path, sweepErr))
			continue
		}

		// 成功した行は予約を解除する。解除失敗は次回起動時の再試行で回収される
		// （両処理とも冪等）が、silent failure 禁止のためログ + errs に残す
		if err := s.ClearPendingDeletion(ctx, r.key, r.series, r.path); err != nil {
			slog.Error("store: 起動時スイープの予約行削除失敗（次回起動時に再試行）",
				"key", r.key, "series", r.series, "path", r.path, "error", err)
			errs = append(errs, fmt.Errorf(
				"store.SweepPendingDeletions: clear key=%q series=%q path=%q: %w",
				r.key, r.series, r.path, err))
			continue
		}
		processed++
	}
	return processed, errs
}
