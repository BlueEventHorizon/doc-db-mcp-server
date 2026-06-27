package store

import (
	"context"
	"database/sql"
)

// ExecForTest は他パッケージのテストから内部 *sql.DB に対し直接 SQL を実行するための
// 補助関数。本番コードから呼び出してはならない（時刻フィールド等の不整合を生むため）。
//
// 用途例（test-only）:
//   - TTL/LRU テストで last_accessed_at を過去時刻に backdate する
//
// 内部実装の都合上 db フィールドは unexported のままにし、テスト時に必要な
// SQL 実行手段のみ通すための薄い窓口として提供する。
func ExecForTest(ctx context.Context, s *Store, query string, args ...any) (sql.Result, error) {
	return s.db.ExecContext(ctx, query, args...)
}
