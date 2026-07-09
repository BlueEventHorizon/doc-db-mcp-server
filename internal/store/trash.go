// trash.go は KEY 単位のゴミ箱状態（keys.trashed_at）まわりの Store メソッドを実装する。
// DES-003（KEY 削除の可視化・ユーザー主導化）・ADR-003・FNC-007〜FNC-013 に対応。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// TrashedKeyInfo は ListTrashedKeys の戻り値要素。
// 自動最終処分までの残り時間の計算には保持期間の設定値（trash.retention_days）が
// 必要だが、internal/store は設定値を持たないため RemainingSeconds は含めない
// （DES-003 §3.2 設計判断。呼び出し元が TrashedAt と設定値から算出する）。
type TrashedKeyInfo struct {
	Key       string
	TrashedAt string
}

// TrashKey は指定 KEY をゴミ箱状態にする（FNC-009）。
// 対象 KEY が存在しない場合、または既にゴミ箱に入っている場合はエラーを返す（多重投入防止）。
// DeleteKey と同様のトランザクション構造を踏襲する。
// WithKeyLock は内部で取得しない。KEY 単位排他が必要な呼び出し元が、対象 KEY への
// Store 呼び出し一式を 1 回の WithKeyLock で囲む（DES-001 §4.3）。
// Mutex を取得して直列化する。
// 戻り値 trashedAt は実際に DB へ保存した値（RFC3339）。呼び出し元が別途 time.Now() を
// 使うと DB の保存値と数秒単位でずれ得るため、保存した実値をそのまま返す。
func (s *Store) TrashKey(ctx context.Context, key string) (trashedAt string, retErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("store.TrashKey: begin tx: %w", err)
	}
	defer rollbackErrInto(tx, &retErr)

	var existingTrashedAt sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT trashed_at FROM keys WHERE key=?`, key).Scan(&existingTrashedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("store.TrashKey: key %q not found", key)
	}
	if err != nil {
		return "", fmt.Errorf("store.TrashKey: select: %w", err)
	}
	if existingTrashedAt.Valid {
		return "", fmt.Errorf("store.TrashKey: key %q is already trashed", key)
	}

	trashedAt = nowRFC3339()
	if _, err := tx.ExecContext(ctx,
		`UPDATE keys SET trashed_at=? WHERE key=?`, trashedAt, key,
	); err != nil {
		return "", fmt.Errorf("store.TrashKey: update: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", err
	}
	return trashedAt, nil
}

// RestoreKey はゴミ箱状態の KEY を利用可能な状態へ戻す（FNC-011）。
// 対象 KEY が存在しない場合、またはゴミ箱に入っていない場合はエラーを返す。
// DeleteKey と同様のトランザクション構造を踏襲する。
// WithKeyLock は内部で取得しない（TrashKey と同様、DES-001 §4.3）。
// Mutex を取得して直列化する。
func (s *Store) RestoreKey(ctx context.Context, key string) (retErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store.RestoreKey: begin tx: %w", err)
	}
	defer rollbackErrInto(tx, &retErr)

	var trashedAt sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT trashed_at FROM keys WHERE key=?`, key).Scan(&trashedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("store.RestoreKey: key %q not found", key)
	}
	if err != nil {
		return fmt.Errorf("store.RestoreKey: select: %w", err)
	}
	if !trashedAt.Valid {
		return fmt.Errorf("store.RestoreKey: key %q is not trashed", key)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE keys SET trashed_at=NULL WHERE key=?`, key,
	); err != nil {
		return fmt.Errorf("store.RestoreKey: update: %w", err)
	}

	return tx.Commit()
}

// ListTrashedKeys は現在ゴミ箱に入っている KEY の一覧を trashed_at 昇順（古い順）で返す（FNC-010）。
// 読み取り操作のため Mutex を取得しない。
func (s *Store) ListTrashedKeys(ctx context.Context) ([]TrashedKeyInfo, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT key, trashed_at FROM keys WHERE trashed_at IS NOT NULL ORDER BY trashed_at`,
	)
	if err != nil {
		return nil, fmt.Errorf("store.ListTrashedKeys: %w", err)
	}
	defer rows.Close()

	var result []TrashedKeyInfo
	for rows.Next() {
		var info TrashedKeyInfo
		if err := rows.Scan(&info.Key, &info.TrashedAt); err != nil {
			return nil, fmt.Errorf("store.ListTrashedKeys scan: %w", err)
		}
		result = append(result, info)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ListTrashedKeys: iterate: %w", err)
	}
	return result, nil
}

// IsTrashed は key がゴミ箱状態かどうかを返す。
// query / upsert_documents / sync_documents / delete_documents / schedule_delete_series の
// ゴミ箱状態判定に使う（DES-003 UC-7/UC-8）。
// key が存在しない場合は false を返す（存在確認は呼び出し元の KeyExists 等に委ねる）。
// 読み取り操作のため Mutex を取得しない。
func (s *Store) IsTrashed(ctx context.Context, key string) (bool, error) {
	var trashedAt sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT trashed_at FROM keys WHERE key=?`, key).Scan(&trashedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store.IsTrashed: %w", err)
	}
	return trashedAt.Valid, nil
}
