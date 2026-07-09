package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
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
	// 注: bm25_stats / bm25_df は v0.1.2 で廃止された（substring match に移行）
	wantTables := []string{"keys", "records", "series_keys", "chunks", "embeddings"}
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

	// bm25_df / bm25_stats は v0.1.2 で廃止（substring match で都度計算）
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

// TestListKeys_ChunkCount は FNC-008 の chunk_count 集計を検証する。
// K1 は 2 record（各 1 chunk）で合計 2、K2 は 1 record（1 chunk）で合計 1。
// K3 は record を持たない（chunk 0 件）KEY で、LEFT JOIN により ChunkCount=0 として
// 結果に含まれることを確認する（INNER JOIN 相当のロジックでは K3 が結果から
// 欠落してしまう点との違い）。
func TestListKeys_ChunkCount(t *testing.T) {
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

	// K3 は record 無しで keys テーブルにのみ存在させる（テスト用ヘルパーで直接 INSERT する）。
	if _, err := ExecForTest(ctx, s,
		`INSERT INTO keys (key, doc_count, last_accessed_at, last_updated_at) VALUES (?, 0, ?, ?)`,
		"K3", "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z",
	); err != nil {
		t.Fatal(err)
	}

	keys, err := s.ListKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}

	byKey := map[string]KeyInfo{}
	for _, k := range keys {
		byKey[k.Key] = k
	}

	if byKey["K1"].ChunkCount != 2 {
		t.Errorf("K1 chunk_count = %d, want 2", byKey["K1"].ChunkCount)
	}
	if byKey["K2"].ChunkCount != 1 {
		t.Errorf("K2 chunk_count = %d, want 1", byKey["K2"].ChunkCount)
	}
	k3, ok := byKey["K3"]
	if !ok {
		t.Fatalf("K3 (chunk 0件) が ListKeys の結果に含まれていない（LEFT JOIN 実装漏れの疑い）")
	}
	if k3.ChunkCount != 0 {
		t.Errorf("K3 chunk_count = %d, want 0", k3.ChunkCount)
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
//
// 注: bm25_stats / bm25_df テーブルは v0.1.2 で廃止された
// （reference doc-db SKILL と同方式の substring match に移行）。
// テストはレガシー削除済み。chunks の CASCADE 削除は
// TestDeleteSeries_RemovesAndPrunes / TestDeleteKey_RemovesEverything でカバー。
// -----------------------------------------------------------------------

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

func TestDeleteSeriesAll_MixedRemoveAndUpdate(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// path a: series [main, feature-x]
	recA, err := s.UpsertRecord(ctx, Record{
		Key: "K", Path: "a", ContentHash: "h_a", Series: "main",
		Chunks: makeChunks("x"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendSeries(ctx, recA, "feature-x"); err != nil {
		t.Fatal(err)
	}

	// path b: series [feature-x] のみ
	recB, err := s.UpsertRecord(ctx, Record{
		Key: "K", Path: "b", ContentHash: "h_b", Series: "feature-x",
		Chunks: makeChunks("y"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// path c: series [main] のみ (feature-x を持たない)
	if _, err := s.UpsertRecord(ctx, Record{
		Key: "K", Path: "c", ContentHash: "h_c", Series: "main",
		Chunks: makeChunks("z"),
	}); err != nil {
		t.Fatal(err)
	}

	// feature-x を全削除
	removed, updated, err := s.DeleteSeriesAll(ctx, "K", "feature-x")
	if err != nil {
		t.Fatal(err)
	}
	// path a: main が残るので保持 (updated=1)
	// path b: series が空になるので物理削除 (removed=1)
	// path c: feature-x 未保有なので触られない
	if removed != 1 || updated != 1 {
		t.Errorf("removed=%d, updated=%d, want removed=1, updated=1", removed, updated)
	}

	// path a はまだ存在 (main が残っている)
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM records WHERE id=?`, recA).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("path a should remain (main still attached), count=%d", n)
	}
	// path b は物理削除
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM records WHERE id=?`, recB).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("path b should be pruned, count=%d", n)
	}
	// path a に feature-x は残っていない
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM series_keys WHERE record_id=? AND series=?`,
		recA, "feature-x").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("feature-x should be removed from path a, count=%d", n)
	}
}

func TestDeleteSeriesAll_NonExistentSeries_Noop(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.UpsertRecord(ctx, Record{
		Key: "K", Path: "a", ContentHash: "h_a", Series: "main",
		Chunks: makeChunks("x"),
	}); err != nil {
		t.Fatal(err)
	}
	// 存在しない series を削除 → エラー無し、0 件処理
	removed, updated, err := s.DeleteSeriesAll(ctx, "K", "nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 || updated != 0 {
		t.Errorf("nonexistent series: removed=%d, updated=%d, want both 0", removed, updated)
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

// -----------------------------------------------------------------------
// WithKeyLock — KEY 単位排他ロック (DES-001 §4.3, SYN-08 / GC-05)
// -----------------------------------------------------------------------

// テストが hang した場合の全体保護タイムアウト。
const keyLockTestTimeout = 5 * time.Second

// keyLockRef は keyLocksMu を取得して key のエントリの参照カウントを返す。
// エントリが存在しない場合は (0, false)。
func keyLockRef(s *Store, key string) (ref int, ok bool) {
	s.keyLocksMu.Lock()
	defer s.keyLocksMu.Unlock()
	e, ok := s.keyLocks[key]
	if !ok {
		return 0, false
	}
	return e.ref, true
}

// waitForKeyLockRef は key の参照カウントが want になるまでポーリングで待機する。
// タイムアウトした場合はテストを Fatal で終了する。
// 注: ref が want に達しても goroutine が select（ロック待機）に入った瞬間そのものは
// 観測できないが、ref++ 後の goroutine は必ず select に進むため、
// キャンセルテストの前提（B が待機登録済み）としては十分。
func waitForKeyLockRef(t *testing.T, s *Store, key string, want int) {
	t.Helper()
	deadline := time.Now().Add(keyLockTestTimeout)
	for time.Now().Before(deadline) {
		ref, _ := keyLockRef(s, key)
		if ref == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	ref, ok := keyLockRef(s, key)
	t.Fatalf("keyLocks[%q] ref = %d (exists=%v), want %d (timeout %v)", key, ref, ok, want, keyLockTestTimeout)
}

// TestWithKeyLock_SameKeyBlocks は goroutine A がロック保持中、
// 同一 key の goroutine B が A の fn 完了までブロックされることを検証する（DES-001 §4.3 の核心）。
func TestWithKeyLock_SameKeyBlocks(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	aAcquired := make(chan struct{}) // A がロック取得して fn に入った通知
	aRelease := make(chan struct{})  // A の fn を終了させるトリガー
	aDone := make(chan error, 1)
	go func() {
		aDone <- s.WithKeyLock(ctx, "K", func() error {
			close(aAcquired)
			<-aRelease // 意図的にロックを保持し続ける
			return nil
		})
	}()

	select {
	case <-aAcquired:
	case <-time.After(keyLockTestTimeout):
		t.Fatal("goroutine A がロックを取得できなかった")
	}

	bEntered := make(chan struct{}) // B の fn が実行された通知
	bDone := make(chan error, 1)
	go func() {
		bDone <- s.WithKeyLock(ctx, "K", func() error {
			close(bEntered)
			return nil
		})
	}()

	// B がロック待機に入ったこと（ref=2）を確認したうえで、
	// A 保持中に B の fn が実行されないことをタイムアウト付きで検証する。
	waitForKeyLockRef(t, s, "K", 2)
	select {
	case <-bEntered:
		t.Fatal("A がロック保持中に B の fn が実行された（排他が効いていない）")
	case <-time.After(100 * time.Millisecond):
		// ブロックされている（期待どおり）
	}

	// A を解放すると B が実行される
	close(aRelease)
	select {
	case <-bEntered:
	case <-time.After(keyLockTestTimeout):
		t.Fatal("A 解放後も B の fn が実行されない")
	}

	if err := <-aDone; err != nil {
		t.Errorf("A の WithKeyLock がエラー: %v", err)
	}
	if err := <-bDone; err != nil {
		t.Errorf("B の WithKeyLock がエラー: %v", err)
	}
}

// TestWithKeyLock_DifferentKeysDoNotBlock は異なる key への WithKeyLock が
// 互いにブロックしないことを検証する（並行度が KEY 単位を超えて落ちていないこと。DES-001 §11）。
func TestWithKeyLock_DifferentKeysDoNotBlock(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	aAcquired := make(chan struct{})
	aRelease := make(chan struct{})
	aDone := make(chan error, 1)
	go func() {
		aDone <- s.WithKeyLock(ctx, "K1", func() error {
			close(aAcquired)
			<-aRelease
			return nil
		})
	}()

	select {
	case <-aAcquired:
	case <-time.After(keyLockTestTimeout):
		t.Fatal("goroutine A がロックを取得できなかった")
	}

	// K1 保持中でも K2 の WithKeyLock は即座に完了する
	bDone := make(chan error, 1)
	go func() {
		bDone <- s.WithKeyLock(ctx, "K2", func() error { return nil })
	}()
	select {
	case err := <-bDone:
		if err != nil {
			t.Errorf("K2 の WithKeyLock がエラー: %v", err)
		}
	case <-time.After(keyLockTestTimeout):
		t.Fatal("K2 の WithKeyLock が K1 のロックにブロックされた")
	}

	close(aRelease)
	if err := <-aDone; err != nil {
		t.Errorf("K1 の WithKeyLock がエラー: %v", err)
	}
}

// TestWithKeyLock_CancelWhileWaiting はロック待機中に ctx を cancel すると
// fn を実行せず即座に ctx.Err() が返ることを検証する（GC-05 の前提となる待機中キャンセル挙動）。
func TestWithKeyLock_CancelWhileWaiting(t *testing.T) {
	s := newTestStore(t)

	aAcquired := make(chan struct{})
	aRelease := make(chan struct{})
	aDone := make(chan error, 1)
	go func() {
		aDone <- s.WithKeyLock(context.Background(), "K", func() error {
			close(aAcquired)
			<-aRelease
			return nil
		})
	}()

	select {
	case <-aAcquired:
	case <-time.After(keyLockTestTimeout):
		t.Fatal("goroutine A がロックを取得できなかった")
	}

	// B は cancel 可能な ctx で同一 key を待機する
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	bEntered := make(chan struct{})
	bDone := make(chan error, 1)
	go func() {
		bDone <- s.WithKeyLock(ctxB, "K", func() error {
			close(bEntered)
			return nil
		})
	}()

	// B が待機登録済み（ref=2）になってから cancel する
	waitForKeyLockRef(t, s, "K", 2)
	cancelB()

	// B は fn を実行せず ctx.Err() を返して即座に戻る（A は解放していない）
	select {
	case err := <-bDone:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("B の戻り値 = %v, want context.Canceled", err)
		}
	case <-time.After(keyLockTestTimeout):
		t.Fatal("cancel 後も B の WithKeyLock が戻らない（待機中キャンセルが効いていない）")
	}
	select {
	case <-bEntered:
		t.Error("cancel されたのに B の fn が実行された")
	default:
	}

	// B 離脱後もエントリは A の分（ref=1）だけ残る
	waitForKeyLockRef(t, s, "K", 1)

	close(aRelease)
	if err := <-aDone; err != nil {
		t.Errorf("A の WithKeyLock がエラー: %v", err)
	}

	// A 終了後、エントリは map から削除される（キャンセル経路の release 込み）
	if ref, ok := keyLockRef(s, "K"); ok {
		t.Errorf("全 goroutine 終了後も keyLocks[K] が残存 (ref=%d)", ref)
	}
}

// TestWithKeyLock_EntryRemovedWhenRefZero は参照カウントが 0 になったエントリが
// keyLocks map から削除されることを検証する（メモリリーク防止の回帰テスト。DES-001 §4.3）。
func TestWithKeyLock_EntryRemovedWhenRefZero(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// 保持中（fn 実行中）はエントリが存在し ref=1
	err := s.WithKeyLock(ctx, "K", func() error {
		ref, ok := keyLockRef(s, "K")
		if !ok {
			t.Error("fn 実行中に keyLocks[K] が存在しない")
		} else if ref != 1 {
			t.Errorf("fn 実行中の ref = %d, want 1", ref)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithKeyLock: %v", err)
	}

	// 完了後はエントリが削除され、map は空
	if ref, ok := keyLockRef(s, "K"); ok {
		t.Errorf("完了後も keyLocks[K] が残存 (ref=%d)", ref)
	}
	s.keyLocksMu.Lock()
	total := len(s.keyLocks)
	s.keyLocksMu.Unlock()
	if total != 0 {
		t.Errorf("keyLocks map size = %d, want 0", total)
	}

	// fn がエラーを返す経路でもエントリは削除される
	wantErr := errors.New("fn error")
	if err := s.WithKeyLock(ctx, "K", func() error { return wantErr }); !errors.Is(err, wantErr) {
		t.Errorf("WithKeyLock = %v, want %v", err, wantErr)
	}
	if _, ok := keyLockRef(s, "K"); ok {
		t.Error("fn エラー後も keyLocks[K] が残存")
	}
}

// TestWithKeyLock_MutualExclusionStress は同一 key への多数 goroutine の
// WithKeyLock で相互排他が成立することを race detector 込みで検証する。
// counter は非 atomic な共有変数であり、排他が破れていれば -race が検出する。
func TestWithKeyLock_MutualExclusionStress(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	const N = 32
	var counter int // WithKeyLock の排他のみで保護される共有変数
	var wg sync.WaitGroup
	errs := make(chan error, N)

	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.WithKeyLock(ctx, "K", func() error {
				counter++
				return nil
			}); err != nil {
				errs <- err
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(keyLockTestTimeout):
		t.Fatal("同一 key への並行 WithKeyLock が完了しない（デッドロックの可能性）")
	}
	close(errs)
	for err := range errs {
		t.Errorf("WithKeyLock: %v", err)
	}

	if counter != N {
		t.Errorf("counter = %d, want %d（排他が破れている）", counter, N)
	}
	// 全 goroutine 終了後、エントリは残らない
	if _, ok := keyLockRef(s, "K"); ok {
		t.Error("全 goroutine 終了後も keyLocks[K] が残存")
	}
}
