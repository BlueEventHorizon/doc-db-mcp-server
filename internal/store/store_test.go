package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

// テスト用の固定次元（本番は 1536 だが可読性のためテストは小さい次元）
const testDim = 3

// newTestStore はテスト用に t.TempDir() に SQLite ファイルを作って Store を返す。
// Cleanup で自動 Close する。
func newTestStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := New(dbPath, testDim)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// makeChunks は ChunkInput のリストを生成する。vector は dim 次元の固定値。
func makeChunks(texts ...string) []ChunkInput {
	out := make([]ChunkInput, len(texts))
	for i, txt := range texts {
		out[i] = ChunkInput{
			ChunkIndex:  i,
			HeadingPath: "# H",
			Text:        txt,
			Vector:      []float32{float32(i + 1), 0.5, -0.5}, // dim=3
		}
	}
	return out
}

// -----------------------------------------------------------------------
// 初期化・スキーマ
// -----------------------------------------------------------------------

func TestNew_CreatesSchema(t *testing.T) {
	s := newTestStore(t)

	// 全テーブルが存在することを確認
	wantTables := []string{"keys", "records", "series_keys", "chunks", "embeddings", "bm25_stats", "bm25_df"}
	for _, name := range wantTables {
		var got string
		err := s.db.QueryRowContext(context.Background(),
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, name,
		).Scan(&got)
		if err != nil {
			t.Errorf("table %q not found: %v", name, err)
		}
	}
}

func TestNew_DimMismatch(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "dim.db")

	// 既存 DB に dim=3 のデータを入れる
	s1, err := New(dbPath, testDim)
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	ctx := context.Background()
	if _, err := s1.UpsertRecord(ctx, Record{
		Key: "K", Path: "p", ContentHash: "h", Series: "s",
		Chunks: makeChunks("hello"),
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	s1.Close()

	// 異なる dim で開くとエラー
	s2, err := New(dbPath, testDim+1)
	if err == nil {
		s2.Close()
		t.Fatal("expected dim mismatch error, got nil")
	}
}

// -----------------------------------------------------------------------
// UpsertRecord 基本
// -----------------------------------------------------------------------

func TestUpsertRecord_Basic(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	recID, err := s.UpsertRecord(ctx, Record{
		Key: "FNC-001", Path: "doc.md", ContentHash: "h1", Series: "s1",
		Chunks: makeChunks("hello world", "foo bar"),
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if recID == 0 {
		t.Fatal("recordID must be non-zero")
	}

	// chunks が 2 件
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM chunks WHERE record_id=?`, recID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("chunks count = %d, want 2", n)
	}

	// embeddings が 2 件
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings e JOIN chunks c ON c.id=e.chunk_id WHERE c.record_id=?`,
		recID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("embeddings count = %d, want 2", n)
	}

	// series_keys に s1 が入っている
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM series_keys WHERE record_id=? AND series=?`,
		recID, "s1").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("series_keys count = %d, want 1", n)
	}

	// bm25_df が記録されている（少なくとも 1 件）
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM bm25_df WHERE key=?`, "FNC-001").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Error("bm25_df should have entries")
	}
}

func TestFindRecord_FoundAndNotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	recID, err := s.UpsertRecord(ctx, Record{
		Key: "K", Path: "p", ContentHash: "h", Series: "s",
		Chunks: makeChunks("text"),
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.FindRecord(ctx, "K", "p", "h")
	if err != nil {
		t.Fatal(err)
	}
	if got != recID {
		t.Errorf("FindRecord = %d, want %d", got, recID)
	}

	// 不一致 → 0, nil
	got, err = s.FindRecord(ctx, "K", "p", "different")
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Errorf("FindRecord(nonexistent) = %d, want 0", got)
	}
}

// -----------------------------------------------------------------------
// GetChunksForSearch
// -----------------------------------------------------------------------

func TestGetChunksForSearch_All(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.UpsertRecord(ctx, Record{
		Key: "K", Path: "doc.md", ContentHash: "h", Series: "alpha",
		Chunks: makeChunks("a", "b", "c"),
	}); err != nil {
		t.Fatal(err)
	}

	chunks, err := s.GetChunksForSearch(ctx, "K", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 3 {
		t.Fatalf("len(chunks) = %d, want 3", len(chunks))
	}
	for i, c := range chunks {
		if c.Key != "K" || c.Path != "doc.md" {
			t.Errorf("chunk[%d]: key/path = %s/%s", i, c.Key, c.Path)
		}
		if len(c.Vector) != testDim {
			t.Errorf("chunk[%d]: vector dim = %d, want %d", i, len(c.Vector), testDim)
		}
		if len(c.SeriesKeys) != 1 || c.SeriesKeys[0] != "alpha" {
			t.Errorf("chunk[%d]: series_keys = %v", i, c.SeriesKeys)
		}
	}
}

func TestGetChunksForSearch_SeriesFilter(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.UpsertRecord(ctx, Record{
		Key: "K", Path: "a.md", ContentHash: "h_a", Series: "alpha",
		Chunks: makeChunks("a"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertRecord(ctx, Record{
		Key: "K", Path: "b.md", ContentHash: "h_b", Series: "beta",
		Chunks: makeChunks("b"),
	}); err != nil {
		t.Fatal(err)
	}

	chunks, err := s.GetChunksForSearch(ctx, "K", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 {
		t.Fatalf("series=alpha len = %d, want 1", len(chunks))
	}
	if chunks[0].Path != "a.md" {
		t.Errorf("path = %q, want a.md", chunks[0].Path)
	}
}

// -----------------------------------------------------------------------
// ListKeys
// -----------------------------------------------------------------------

func TestListKeys_AggregatesSeriesAndDocCount(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for _, r := range []Record{
		{Key: "K1", Path: "a", ContentHash: "h_a", Series: "s1", Chunks: makeChunks("a")},
		{Key: "K1", Path: "b", ContentHash: "h_b", Series: "s2", Chunks: makeChunks("b")},
		{Key: "K2", Path: "c", ContentHash: "h_c", Series: "s1", Chunks: makeChunks("c")},
	} {
		if _, err := s.UpsertRecord(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	keys, err := s.ListKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("len(keys) = %d, want 2", len(keys))
	}

	byKey := map[string]KeyInfo{}
	for _, k := range keys {
		byKey[k.Key] = k
	}
	if byKey["K1"].DocCount != 2 {
		t.Errorf("K1 doc_count = %d, want 2", byKey["K1"].DocCount)
	}
	if len(byKey["K1"].Series) != 2 {
		t.Errorf("K1 series = %v, want 2 entries", byKey["K1"].Series)
	}
	if byKey["K2"].DocCount != 1 {
		t.Errorf("K2 doc_count = %d, want 1", byKey["K2"].DocCount)
	}
}

// -----------------------------------------------------------------------
// DIF-02: 同一ハッシュ・新規 series
// -----------------------------------------------------------------------

func TestAppendAndCleanSeries_DIF02(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// 旧 record: 同一 key+path で series=s_old
	oldID, err := s.UpsertRecord(ctx, Record{
		Key: "K", Path: "p", ContentHash: "h_old", Series: "s_target",
		Chunks: makeChunks("old"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// 別 content の新 record（DIF-03 仮定の準備）
	newID, err := s.UpsertRecord(ctx, Record{
		Key: "K", Path: "p", ContentHash: "h_new", Series: "s_other",
		Chunks: makeChunks("new"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// AppendAndCleanSeries: newID に s_target を追加し、他 record から s_target を除去
	if err := s.AppendAndCleanSeries(ctx, newID, "K", "p", "s_target"); err != nil {
		t.Fatal(err)
	}

	// newID には s_target が追加されている
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM series_keys WHERE record_id=? AND series=?`,
		newID, "s_target").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("newID has s_target count = %d, want 1", n)
	}

	// 旧 record の series_keys が空になり物理削除されている
	var exists int
	err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM records WHERE id=?`, oldID).Scan(&exists)
	if err != nil {
		t.Fatal(err)
	}
	if exists != 0 {
		t.Errorf("oldID still exists (series_keys empty後の物理削除が走っていない)")
	}

	// 不変条件: 同一 key+path+series は常に高々 1 record
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM records r JOIN series_keys sk ON sk.record_id=r.id
		 WHERE r.key=? AND r.path=? AND sk.series=?`,
		"K", "p", "s_target").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("同一 key+path+series record count = %d, want 1", n)
	}
}

// -----------------------------------------------------------------------
// DIF-03: ハッシュ不一致・新規 record + CleanOtherSeries
// -----------------------------------------------------------------------

func TestCleanOtherSeries_DIF03(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	oldID, err := s.UpsertRecord(ctx, Record{
		Key: "K", Path: "p", ContentHash: "h_v1", Series: "main",
		Chunks: makeChunks("v1"),
	})
	if err != nil {
		t.Fatal(err)
	}

	newID, err := s.UpsertRecord(ctx, Record{
		Key: "K", Path: "p", ContentHash: "h_v2", Series: "main",
		Chunks: makeChunks("v2"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if oldID == newID {
		t.Fatal("new content should create new record")
	}

	// CleanOtherSeries: 新 record 以外の同 key+path から series=main を除去
	if err := s.CleanOtherSeries(ctx, "K", "p", "main", newID); err != nil {
		t.Fatal(err)
	}

	// 旧 record は series_keys が空になり物理削除
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM records WHERE id=?`, oldID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("旧 record が削除されていない")
	}

	// 不変条件
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM records r JOIN series_keys sk ON sk.record_id=r.id
		 WHERE r.key=? AND r.path=? AND sk.series=?`,
		"K", "p", "main").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("同一 key+path+series record count = %d, want 1", n)
	}
}

// -----------------------------------------------------------------------
// BM25 整合性
// -----------------------------------------------------------------------

func TestBM25_DfDecreasesOnRecordDelete(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// 2 record に共通の "shared" タームを含める
	if _, err := s.UpsertRecord(ctx, Record{
		Key: "K", Path: "a", ContentHash: "h_a", Series: "s",
		Chunks: makeChunks("shared apple"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertRecord(ctx, Record{
		Key: "K", Path: "b", ContentHash: "h_b", Series: "s",
		Chunks: makeChunks("shared banana"),
	}); err != nil {
		t.Fatal(err)
	}

	// "shared" の df は 2
	var df int
	if err := s.db.QueryRowContext(ctx,
		`SELECT df FROM bm25_df WHERE key=? AND term=?`, "K", "shared").Scan(&df); err != nil {
		t.Fatalf("shared df: %v", err)
	}
	if df != 2 {
		t.Errorf("df(shared) before delete = %d, want 2", df)
	}

	// path=a を DeleteSeries で削除
	if err := s.DeleteSeries(ctx, "K", "s", []string{"a"}); err != nil {
		t.Fatal(err)
	}

	// df が 1 に減っているはず
	if err := s.db.QueryRowContext(ctx,
		`SELECT df FROM bm25_df WHERE key=? AND term=?`, "K", "shared").Scan(&df); err != nil {
		t.Fatalf("after delete: %v", err)
	}
	if df != 1 {
		t.Errorf("df(shared) after delete = %d, want 1", df)
	}

	// "apple" は 1 record にしか無く df=1 → 削除後は df=0 で行ごと消える
	err := s.db.QueryRowContext(ctx,
		`SELECT df FROM bm25_df WHERE key=? AND term=?`, "K", "apple").Scan(&df)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("apple should be pruned (df<=0), got df=%d err=%v", df, err)
	}
}

func TestBM25_CascadeOnRecordDelete(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	recID, err := s.UpsertRecord(ctx, Record{
		Key: "K", Path: "p", ContentHash: "h", Series: "s",
		Chunks: makeChunks("hello world"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// bm25_stats に行があることを確認
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM bm25_stats WHERE chunk_id IN (SELECT id FROM chunks WHERE record_id=?)`,
		recID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("bm25_stats should have rows before delete")
	}

	// DeleteKey で全削除
	if err := s.DeleteKey(ctx, "K"); err != nil {
		t.Fatal(err)
	}

	// bm25_stats が CASCADE で消えている
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM bm25_stats`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("bm25_stats after DeleteKey = %d, want 0", n)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM bm25_df`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("bm25_df after DeleteKey = %d, want 0", n)
	}
}

// -----------------------------------------------------------------------
// DeleteSeries / DeleteKey
// -----------------------------------------------------------------------

func TestDeleteSeries_RemovesAndPrunes(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// 2 つの series を持つ record
	recID, err := s.UpsertRecord(ctx, Record{
		Key: "K", Path: "p", ContentHash: "h", Series: "s1",
		Chunks: makeChunks("x"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendSeries(ctx, recID, "s2"); err != nil {
		t.Fatal(err)
	}

	// s1 を削除しても record は残る（s2 がまだあるので）
	if err := s.DeleteSeries(ctx, "K", "s1", []string{"p"}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM records WHERE id=?`, recID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("record should remain (s2 still attached), got count=%d", n)
	}

	// s2 を削除すると record も消える
	if err := s.DeleteSeries(ctx, "K", "s2", []string{"p"}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM records WHERE id=?`, recID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("record should be pruned after last series removed, got count=%d", n)
	}
}

func TestDeleteKey_RemovesEverything(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.UpsertRecord(ctx, Record{
		Key: "K", Path: "a", ContentHash: "h_a", Series: "s",
		Chunks: makeChunks("x"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertRecord(ctx, Record{
		Key: "K", Path: "b", ContentHash: "h_b", Series: "s",
		Chunks: makeChunks("y"),
	}); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteKey(ctx, "K"); err != nil {
		t.Fatal(err)
	}

	for _, q := range []string{
		`SELECT COUNT(*) FROM records WHERE key='K'`,
		`SELECT COUNT(*) FROM keys WHERE key='K'`,
		`SELECT COUNT(*) FROM chunks`,
		`SELECT COUNT(*) FROM embeddings`,
	} {
		var n int
		if err := s.db.QueryRowContext(ctx, q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if n != 0 {
			t.Errorf("%s = %d, want 0", q, n)
		}
	}
}

// -----------------------------------------------------------------------
// 廃棄ポリシー用クエリ (TASK-012 サポート: §8.1 / §8.2 / §8.4)
// -----------------------------------------------------------------------

func TestListExpiredKeysByTTL_DefaultAndOverride(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// 3 つの KEY を投入
	for _, k := range []string{"FRESH", "OLD_DEFAULT", "OLD_OVERRIDE"} {
		if _, err := s.UpsertRecord(ctx, Record{
			Key: k, Path: "p", ContentHash: "h_" + k, Series: "s",
			Chunks: makeChunks("x"),
		}); err != nil {
			t.Fatal(err)
		}
	}

	// last_accessed_at を制御:
	//   FRESH        = now            （expire 対象外）
	//   OLD_DEFAULT  = -100 days      （default TTL=30 → expire）
	//   OLD_OVERRIDE = -10 days, ttl_days=5 override → expire
	if _, err := s.db.ExecContext(ctx, `
UPDATE keys SET last_accessed_at = CASE key
  WHEN 'OLD_DEFAULT'  THEN datetime('now', '-100 days')
  WHEN 'OLD_OVERRIDE' THEN datetime('now', '-10 days')
  ELSE last_accessed_at
END
`); err != nil {
		t.Fatal(err)
	}
	// OLD_OVERRIDE に ttl_days=5 のオーバーライドを設定
	if err := s.SetExpiryPolicy(ctx, "OLD_OVERRIDE", &ExpiryPolicy{TTLDays: intPtr(5)}); err != nil {
		t.Fatal(err)
	}

	keys, err := s.ListExpiredKeysByTTL(ctx, 30)
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]bool{}
	for _, k := range keys {
		got[k] = true
	}
	if got["FRESH"] {
		t.Error("FRESH should not be expired")
	}
	if !got["OLD_DEFAULT"] {
		t.Error("OLD_DEFAULT should be expired (now - 100d < now - 30d)")
	}
	if !got["OLD_OVERRIDE"] {
		t.Error("OLD_OVERRIDE should be expired (override ttl=5d, accessed 10d ago)")
	}
}

func TestTotalChunkCount(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if n, err := s.TotalChunkCount(ctx); err != nil || n != 0 {
		t.Errorf("initial: got (%d, %v), want (0, nil)", n, err)
	}

	if _, err := s.UpsertRecord(ctx, Record{
		Key: "K", Path: "p", ContentHash: "h", Series: "s",
		Chunks: makeChunks("a", "b", "c"),
	}); err != nil {
		t.Fatal(err)
	}

	if n, err := s.TotalChunkCount(ctx); err != nil || n != 3 {
		t.Errorf("after upsert: got (%d, %v), want (3, nil)", n, err)
	}
}

func TestListKeysByLRU_OrderedOldestFirst(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for _, k := range []string{"K1", "K2", "K3"} {
		if _, err := s.UpsertRecord(ctx, Record{
			Key: k, Path: "p", ContentHash: "h_" + k, Series: "s",
			Chunks: makeChunks("a", "b"), // 各 KEY 2 chunks
		}); err != nil {
			t.Fatal(err)
		}
	}

	// K1 を最古、K3 を最新に
	if _, err := s.db.ExecContext(ctx, `
UPDATE keys SET last_accessed_at = CASE key
  WHEN 'K1' THEN datetime('now', '-3 hours')
  WHEN 'K2' THEN datetime('now', '-2 hours')
  WHEN 'K3' THEN datetime('now', '-1 hours')
END WHERE key IN ('K1','K2','K3')`); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListKeysByLRU(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []KeyLRUInfo{{"K1", 2}, {"K2", 2}, {"K3", 2}}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestSetExpiryPolicy_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.UpsertRecord(ctx, Record{
		Key: "K", Path: "p", ContentHash: "h", Series: "s",
		Chunks: makeChunks("x"),
	}); err != nil {
		t.Fatal(err)
	}

	// ポリシー設定
	policy := &ExpiryPolicy{TTLDays: intPtr(7), MaxChunks: intPtr(500)}
	if err := s.SetExpiryPolicy(ctx, "K", policy); err != nil {
		t.Fatal(err)
	}

	// ListKeys で読み戻し
	keys, err := s.ListKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].ExpiryPolicy == nil {
		t.Fatalf("ExpiryPolicy not set: %+v", keys)
	}
	got := keys[0].ExpiryPolicy
	if got.TTLDays == nil || *got.TTLDays != 7 {
		t.Errorf("TTLDays = %v, want 7", got.TTLDays)
	}
	if got.MaxChunks == nil || *got.MaxChunks != 500 {
		t.Errorf("MaxChunks = %v, want 500", got.MaxChunks)
	}

	// nil で reset
	if err := s.SetExpiryPolicy(ctx, "K", nil); err != nil {
		t.Fatal(err)
	}
	keys, err = s.ListKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if keys[0].ExpiryPolicy != nil {
		t.Errorf("policy should be nil after reset, got %+v", keys[0].ExpiryPolicy)
	}
}

func TestSetExpiryPolicy_UnknownKey_Errors(t *testing.T) {
	s := newTestStore(t)
	err := s.SetExpiryPolicy(context.Background(), "NOTEXIST", &ExpiryPolicy{TTLDays: intPtr(1)})
	if err == nil {
		t.Fatal("want error for unknown key")
	}
}

func intPtr(v int) *int { return &v }

// -----------------------------------------------------------------------
// 並行書き込み
// -----------------------------------------------------------------------

func TestConcurrent_UpsertSerializedByMutex(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	const N = 8
	var wg sync.WaitGroup
	errs := make(chan error, N)

	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.UpsertRecord(ctx, Record{
				Key:         "K",
				Path:        "p" + string(rune('a'+i)),
				ContentHash: "h" + string(rune('a'+i)),
				Series:      "s",
				Chunks:      makeChunks("text"),
			})
			if err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent upsert: %v", err)
	}

	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM records WHERE key='K'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != N {
		t.Errorf("records count = %d, want %d", n, N)
	}
}
