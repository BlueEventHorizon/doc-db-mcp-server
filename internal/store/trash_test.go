package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// -----------------------------------------------------------------------
// TrashKey / RestoreKey / ListTrashedKeys / IsTrashed
// -----------------------------------------------------------------------

func TestTrashKey_Basic(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.UpsertRecord(ctx, Record{
		Key: "K", Path: "p", ContentHash: "h", Series: "s",
		Chunks: makeChunks("x"),
	}); err != nil {
		t.Fatal(err)
	}

	if err := s.TrashKey(ctx, "K"); err != nil {
		t.Fatalf("TrashKey: %v", err)
	}

	trashed, err := s.IsTrashed(ctx, "K")
	if err != nil {
		t.Fatal(err)
	}
	if !trashed {
		t.Error("IsTrashed = false after TrashKey, want true")
	}

	infos, err := s.ListTrashedKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 {
		t.Fatalf("ListTrashedKeys len = %d, want 1", len(infos))
	}
	if infos[0].Key != "K" || infos[0].TrashedAt == "" {
		t.Errorf("ListTrashedKeys[0] = %+v, want Key=K with non-empty TrashedAt", infos[0])
	}
}

func TestTrashKey_AlreadyTrashed_Errors(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.UpsertRecord(ctx, Record{
		Key: "K", Path: "p", ContentHash: "h", Series: "s",
		Chunks: makeChunks("x"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.TrashKey(ctx, "K"); err != nil {
		t.Fatal(err)
	}

	if err := s.TrashKey(ctx, "K"); err == nil {
		t.Fatal("want error for already-trashed key (多重投入防止)")
	}
}

func TestTrashKey_UnknownKey_Errors(t *testing.T) {
	s := newTestStore(t)

	if err := s.TrashKey(context.Background(), "NOTEXIST"); err == nil {
		t.Fatal("want error for unknown key")
	}
}

func TestRestoreKey_Basic(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.UpsertRecord(ctx, Record{
		Key: "K", Path: "p", ContentHash: "h", Series: "s",
		Chunks: makeChunks("x"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.TrashKey(ctx, "K"); err != nil {
		t.Fatal(err)
	}

	if err := s.RestoreKey(ctx, "K"); err != nil {
		t.Fatalf("RestoreKey: %v", err)
	}

	trashed, err := s.IsTrashed(ctx, "K")
	if err != nil {
		t.Fatal(err)
	}
	if trashed {
		t.Error("IsTrashed = true after RestoreKey, want false")
	}

	infos, err := s.ListTrashedKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 0 {
		t.Errorf("ListTrashedKeys after restore = %+v, want empty", infos)
	}

	// 復活後も再度ゴミ箱投入できる（ライフサイクル一巡）
	if err := s.TrashKey(ctx, "K"); err != nil {
		t.Fatalf("re-TrashKey after restore: %v", err)
	}
}

func TestRestoreKey_NotTrashed_Errors(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.UpsertRecord(ctx, Record{
		Key: "K", Path: "p", ContentHash: "h", Series: "s",
		Chunks: makeChunks("x"),
	}); err != nil {
		t.Fatal(err)
	}

	if err := s.RestoreKey(ctx, "K"); err == nil {
		t.Fatal("want error for non-trashed key")
	}
}

func TestRestoreKey_UnknownKey_Errors(t *testing.T) {
	s := newTestStore(t)

	if err := s.RestoreKey(context.Background(), "NOTEXIST"); err == nil {
		t.Fatal("want error for unknown key")
	}
}

func TestIsTrashed_UnknownKey_ReturnsFalse(t *testing.T) {
	s := newTestStore(t)

	trashed, err := s.IsTrashed(context.Background(), "NOTEXIST")
	if err != nil {
		t.Fatal(err)
	}
	if trashed {
		t.Error("IsTrashed = true for unknown key, want false")
	}
}

func TestIsTrashed_ExistingKeyNotTrashed_ReturnsFalse(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.UpsertRecord(ctx, Record{
		Key: "K", Path: "p", ContentHash: "h", Series: "s",
		Chunks: makeChunks("x"),
	}); err != nil {
		t.Fatal(err)
	}

	trashed, err := s.IsTrashed(ctx, "K")
	if err != nil {
		t.Fatal(err)
	}
	if trashed {
		t.Error("IsTrashed = true for existing non-trashed key, want false")
	}
}

func TestListTrashedKeys_MultipleKeys_OrderedByTrashedAt(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for _, key := range []string{"K1", "K2", "K3"} {
		if _, err := s.UpsertRecord(ctx, Record{
			Key: key, Path: "p", ContentHash: "h", Series: "s",
			Chunks: makeChunks("x"),
		}); err != nil {
			t.Fatal(err)
		}
	}

	// TrashKey の呼び出し順（K2, K1, K3）が trashed_at の順序になる。
	// 同一トランザクション内での time.Now() 解像度差を避けるため、
	// 呼び出し順に依存する ORDER BY trashed_at ASC の検証に主眼を置く。
	for _, key := range []string{"K2", "K1", "K3"} {
		if err := s.TrashKey(ctx, key); err != nil {
			t.Fatalf("TrashKey(%s): %v", key, err)
		}
	}

	infos, err := s.ListTrashedKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 3 {
		t.Fatalf("ListTrashedKeys len = %d, want 3", len(infos))
	}

	// trashed_at 昇順であることを検証する。
	for i := 1; i < len(infos); i++ {
		if infos[i-1].TrashedAt > infos[i].TrashedAt {
			t.Errorf("ListTrashedKeys not ordered by TrashedAt ascending: infos[%d]=%q > infos[%d]=%q",
				i-1, infos[i-1].TrashedAt, i, infos[i].TrashedAt)
		}
	}
}

// -----------------------------------------------------------------------
// マイグレーション: 既存 DB への trashed_at カラム追加
// -----------------------------------------------------------------------

func TestMigration_AddsTrashedAtColumn(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")

	// trashed_at カラム新設前の旧スキーマを模した DB を直接作成する。
	raw, err := sql.Open("sqlite", buildDSN(dbPath))
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := raw.Exec(`
CREATE TABLE keys (
    key              TEXT PRIMARY KEY,
    doc_count        INTEGER NOT NULL DEFAULT 0,
    last_accessed_at TEXT NOT NULL,
    last_updated_at  TEXT NOT NULL,
    expiry_policy    TEXT
);`); err != nil {
		t.Fatalf("create legacy keys table: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	// New (initSchema 経由) がマイグレーションを実行し trashed_at 列を追加するはず。
	s, err := New(dbPath, testDim)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	rows, err := s.db.Query(`PRAGMA table_info(keys)`)
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	defer rows.Close()

	found := false
	for rows.Next() {
		var (
			cid       int
			name      string
			ctype     string
			notNull   int
			dfltValue sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dfltValue, &pk); err != nil {
			t.Fatal(err)
		}
		if name == "trashed_at" {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Error("trashed_at column not found after migration")
	}

	// マイグレーション後、新規メソッドが問題なく動くことも確認する
	if err := s.TrashKey(context.Background(), "NOTEXIST"); err == nil {
		t.Fatal("want error for unknown key even on migrated DB")
	}
}
