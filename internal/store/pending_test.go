// pending_test.go は pending_deletions（削除予約）まわりの Store メソッドの単体テスト。
// DES-003 §3.3（メソッド仕様）・§6（テスト設計）、APP-003 SYN-03/04・GC-01〜04 に対応。
// sync_documents 経由の統合挙動は internal/mcp 側（TASK-013）で検証するため、ここでは扱わない。
package store

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// countRows は任意の COUNT クエリを実行して件数を返すテストヘルパー。
func countRows(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("countRows(%s): %v", query, err)
	}
	return n
}

// pendingRowCount は pending_deletions の該当行数を返す。
func pendingRowCount(t *testing.T, s *Store, key, series, path string) int {
	t.Helper()
	return countRows(t, s,
		`SELECT COUNT(*) FROM pending_deletions WHERE key=? AND series=? AND path=?`,
		key, series, path)
}

// recordAlive は record（本体・chunks・embeddings）の物理残存を検証する。
// wantChunks=0 は「物理削除済み（record も 0 件）」の検証に使う。
func recordAlive(t *testing.T, s *Store, recID int64, wantChunks int) {
	t.Helper()
	wantRecords := 1
	if wantChunks == 0 {
		wantRecords = 0
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM records WHERE id=?`, recID); n != wantRecords {
		t.Errorf("records(id=%d) count = %d, want %d", recID, n, wantRecords)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM chunks WHERE record_id=?`, recID); n != wantChunks {
		t.Errorf("chunks(record_id=%d) count = %d, want %d", recID, n, wantChunks)
	}
	if n := countRows(t, s,
		`SELECT COUNT(*) FROM embeddings e JOIN chunks c ON c.id=e.chunk_id WHERE c.record_id=?`,
		recID); n != wantChunks {
		t.Errorf("embeddings(record_id=%d) count = %d, want %d", recID, n, wantChunks)
	}
}

// docCount は ListKeys 経由で key の doc_count を読み戻す。
func docCount(t *testing.T, s *Store, key string) int {
	t.Helper()
	keys, err := s.ListKeys(context.Background())
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	for _, k := range keys {
		if k.Key == key {
			return k.DocCount
		}
	}
	t.Fatalf("key %q not found in ListKeys", key)
	return 0
}

// searchPaths は GetChunksForSearch の結果から path 集合を返す。
func searchPaths(t *testing.T, s *Store, key, series string) map[string]bool {
	t.Helper()
	chunks, err := s.GetChunksForSearch(context.Background(), key, series)
	if err != nil {
		t.Fatalf("GetChunksForSearch(%q, %q): %v", key, series, err)
	}
	got := map[string]bool{}
	for _, c := range chunks {
		got[c.Path] = true
	}
	return got
}

// -----------------------------------------------------------------------
// MarkSeriesForDeletion / MarkDocumentForDeletion — 冪等性（GC-01 / SYN-03）
// -----------------------------------------------------------------------

func TestMarkSeriesForDeletion_Idempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// 1 回目: 予約なし → alreadyScheduled=false
	already, err := s.MarkSeriesForDeletion(ctx, "K", "feature-x")
	if err != nil {
		t.Fatalf("MarkSeriesForDeletion 1回目: %v", err)
	}
	if already {
		t.Error("1回目の alreadyScheduled = true, want false")
	}

	// 2 回目: 既存予約あり → alreadyScheduled=true（冪等 upsert で行は増えない）
	already, err = s.MarkSeriesForDeletion(ctx, "K", "feature-x")
	if err != nil {
		t.Fatalf("MarkSeriesForDeletion 2回目: %v", err)
	}
	if !already {
		t.Error("2回目の alreadyScheduled = false, want true")
	}
	if n := pendingRowCount(t, s, "K", "feature-x", ""); n != 1 {
		t.Errorf("path='' センチネル行数 = %d, want 1", n)
	}
}

func TestMarkDocumentForDeletion_Idempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// 2 回呼んでもエラーなし・行は 1 件のまま（ON CONFLICT DO UPDATE）
	for i := 0; i < 2; i++ {
		if err := s.MarkDocumentForDeletion(ctx, "K", "main", "doc.md"); err != nil {
			t.Fatalf("MarkDocumentForDeletion %d回目: %v", i+1, err)
		}
	}
	if n := pendingRowCount(t, s, "K", "main", "doc.md"); n != 1 {
		t.Errorf("path 単位予約行数 = %d, want 1", n)
	}
}

// -----------------------------------------------------------------------
// DetachSeriesFromPath — SYN-03 の核心
// -----------------------------------------------------------------------

// TestDetachSeriesFromPath_OrphanedAndPhysicallyRetained は単独参照 record の切り離しで
// (a) 当該 series 指定の検索から直ちに消えること
// (b) record・chunks・embeddings が物理的に残存すること（orphan 保持）
// (c) orphaned=true が返ること — を検証する。
func TestDetachSeriesFromPath_OrphanedAndPhysicallyRetained(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// 切り離し対象（series=s1 単独参照、chunk 2 件）
	recID, err := s.UpsertRecord(ctx, Record{
		Key: "K", Path: "gone.md", ContentHash: "h_gone", Series: "s1",
		Chunks: makeChunks("alpha one", "alpha two"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// 同 series に残り続ける path（検索結果が空になるだけの検証を避ける）
	if _, err := s.UpsertRecord(ctx, Record{
		Key: "K", Path: "keep.md", ContentHash: "h_keep", Series: "s1",
		Chunks: makeChunks("keep"),
	}); err != nil {
		t.Fatal(err)
	}

	orphaned, err := s.DetachSeriesFromPath(ctx, "K", "s1", "gone.md")
	if err != nil {
		t.Fatalf("DetachSeriesFromPath: %v", err)
	}
	// (c) 単独参照 → orphaned=true
	if !orphaned {
		t.Error("orphaned = false, want true（単独参照 record の切り離し）")
	}

	// (a) series 指定検索に gone.md の chunk が現れない（keep.md は残る）
	got := searchPaths(t, s, "K", "s1")
	if got["gone.md"] {
		t.Error("切り離し後も series=s1 の検索に gone.md が現れる（SYN-03 違反）")
	}
	if !got["keep.md"] {
		t.Error("無関係な keep.md まで検索から消えた")
	}

	// (b) record・chunks・embeddings は物理的に残存（orphan 保持、SYN-04 の自己修復用）
	recordAlive(t, s, recID, 2)

	// 既知の制約（DES-003 §3.3）: series 未指定の KEY 全体検索には物理削除まで現れ得る
	if all := searchPaths(t, s, "K", ""); !all["gone.md"] {
		t.Error("KEY 全体検索（series 未指定）に orphan が現れない（想定と異なる挙動）")
	}
}

// TestDetachSeriesFromPath_OtherSeriesRemains は他 series が参照する record の切り離しで
// orphaned=false が返り、残る series の検索に引き続き現れることを検証する。
func TestDetachSeriesFromPath_OtherSeriesRemains(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	recID, err := s.UpsertRecord(ctx, Record{
		Key: "K", Path: "shared.md", ContentHash: "h", Series: "s1",
		Chunks: makeChunks("x"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendSeries(ctx, recID, "s2"); err != nil {
		t.Fatal(err)
	}

	orphaned, err := s.DetachSeriesFromPath(ctx, "K", "s1", "shared.md")
	if err != nil {
		t.Fatalf("DetachSeriesFromPath: %v", err)
	}
	if orphaned {
		t.Error("orphaned = true, want false（s2 の紐付けが残っている）")
	}

	// s1 の検索からは消え、s2 の検索には残る
	if searchPaths(t, s, "K", "s1")["shared.md"] {
		t.Error("s1 切り離し後も series=s1 の検索に現れる")
	}
	if !searchPaths(t, s, "K", "s2")["shared.md"] {
		t.Error("series=s2 の検索から消えた（他 series の紐付けが壊れた）")
	}
	recordAlive(t, s, recID, 1)
}

// -----------------------------------------------------------------------
// ListPendingDeletions — 4 状態（予約なし / path のみ / series 全体のみ / 両方）
// -----------------------------------------------------------------------

func TestListPendingDeletions_FourStates(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// 別 key・別 series の予約が混入しないことを確認するためのノイズ行
	if err := s.MarkDocumentForDeletion(ctx, "OTHER_KEY", "s1", "noise.md"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDocumentForDeletion(ctx, "K", "other_series", "noise2.md"); err != nil {
		t.Fatal(err)
	}

	// 状態 1: 予約なし
	paths, seriesWide, err := s.ListPendingDeletions(ctx, "K", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 0 || seriesWide {
		t.Errorf("予約なし: paths=%v seriesWide=%v, want 空/false", paths, seriesWide)
	}

	// 状態 2: path 単位のみ
	if err := s.MarkDocumentForDeletion(ctx, "K", "s1", "a.md"); err != nil {
		t.Fatal(err)
	}
	paths, seriesWide, err = s.ListPendingDeletions(ctx, "K", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "a.md" || seriesWide {
		t.Errorf("path のみ: paths=%v seriesWide=%v, want [a.md]/false", paths, seriesWide)
	}

	// 状態 3: series 全体のみ
	if err := s.ClearPendingDeletion(ctx, "K", "s1", "a.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkSeriesForDeletion(ctx, "K", "s1"); err != nil {
		t.Fatal(err)
	}
	paths, seriesWide, err = s.ListPendingDeletions(ctx, "K", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 0 || !seriesWide {
		t.Errorf("series 全体のみ: paths=%v seriesWide=%v, want 空/true", paths, seriesWide)
	}

	// 状態 4: 両方
	if err := s.MarkDocumentForDeletion(ctx, "K", "s1", "a.md"); err != nil {
		t.Fatal(err)
	}
	paths, seriesWide, err = s.ListPendingDeletions(ctx, "K", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "a.md" || !seriesWide {
		t.Errorf("両方: paths=%v seriesWide=%v, want [a.md]/true", paths, seriesWide)
	}
}

// -----------------------------------------------------------------------
// ClearPendingDeletion — 解除・冪等性・path='' センチネル
// -----------------------------------------------------------------------

func TestClearPendingDeletion_RemovesRowAndIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.MarkDocumentForDeletion(ctx, "K", "s1", "a.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkSeriesForDeletion(ctx, "K", "s1"); err != nil {
		t.Fatal(err)
	}

	// path 単位行を解除（series 全体行は残る）
	if err := s.ClearPendingDeletion(ctx, "K", "s1", "a.md"); err != nil {
		t.Fatalf("ClearPendingDeletion(path): %v", err)
	}
	if n := pendingRowCount(t, s, "K", "s1", "a.md"); n != 0 {
		t.Errorf("解除後も path 単位行が残存 (count=%d)", n)
	}
	if n := pendingRowCount(t, s, "K", "s1", ""); n != 1 {
		t.Errorf("path 解除で series 全体行まで消えた (count=%d)", n)
	}

	// 存在しない行への呼び出しは冪等（エラーなし）
	if err := s.ClearPendingDeletion(ctx, "K", "s1", "nonexistent.md"); err != nil {
		t.Errorf("存在しない行の解除がエラー: %v", err)
	}

	// path="" で series 全体の削除予約を解除できる
	if err := s.ClearPendingDeletion(ctx, "K", "s1", ""); err != nil {
		t.Fatalf("ClearPendingDeletion(series 全体): %v", err)
	}
	if n := pendingRowCount(t, s, "K", "s1", ""); n != 0 {
		t.Errorf("series 全体行が解除されていない (count=%d)", n)
	}
}

// -----------------------------------------------------------------------
// DeleteOrphanRecords — 補償の核心（DES-003 §3.3）
// -----------------------------------------------------------------------

// TestDeleteOrphanRecords_RemovesOrphanOnlyAndUpdatesDocCount は
// (a) series_keys 0 件の record のみ物理削除（chunks/embeddings 含む）・紐付き record には不触
// (b) orphan 不在時は 0 件処理で冪等
// (c) 削除後に doc_count が更新される — を検証する。
func TestDeleteOrphanRecords_RemovesOrphanOnlyAndUpdatesDocCount(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// path "a.md": live record（s1 紐付き）
	if _, err := s.UpsertRecord(ctx, Record{
		Key: "K", Path: "a.md", ContentHash: "h_a", Series: "s1",
		Chunks: makeChunks("a"),
	}); err != nil {
		t.Fatal(err)
	}
	// path "b.md": 旧 record（h_v1、s1 から切り離して orphan 化）と
	// 新 record（h_v2、s2 紐付きで live）が同居する状態を作る
	oldID, err := s.UpsertRecord(ctx, Record{
		Key: "K", Path: "b.md", ContentHash: "h_v1", Series: "s1",
		Chunks: makeChunks("v1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	newID, err := s.UpsertRecord(ctx, Record{
		Key: "K", Path: "b.md", ContentHash: "h_v2", Series: "s2",
		Chunks: makeChunks("v2"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if orphaned, err := s.DetachSeriesFromPath(ctx, "K", "s1", "b.md"); err != nil || !orphaned {
		t.Fatalf("Detach 準備: orphaned=%v err=%v", orphaned, err)
	}
	if got := docCount(t, s, "K"); got != 2 {
		t.Fatalf("前提: doc_count = %d, want 2", got)
	}

	// (a) orphan（旧 record）のみ物理削除され、series 紐付き record には触れない
	removed, err := s.DeleteOrphanRecords(ctx, "K", "b.md")
	if err != nil {
		t.Fatalf("DeleteOrphanRecords: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	recordAlive(t, s, oldID, 0) // 物理削除（chunks/embeddings 含む）
	recordAlive(t, s, newID, 1) // live record は無傷

	// (c) 削除後に doc_count が更新される（b.md には live record が残るため 2 のまま）
	if got := docCount(t, s, "K"); got != 2 {
		t.Errorf("orphan 回収後 doc_count = %d, want 2", got)
	}

	// (b) orphan 不在時は 0 件処理で冪等
	removed, err = s.DeleteOrphanRecords(ctx, "K", "b.md")
	if err != nil {
		t.Fatalf("DeleteOrphanRecords 2回目: %v", err)
	}
	if removed != 0 {
		t.Errorf("orphan 不在時の removed = %d, want 0", removed)
	}

	// (c) path 配下の record が全て消えるケースでは doc_count が減る
	if orphaned, err := s.DetachSeriesFromPath(ctx, "K", "s2", "b.md"); err != nil || !orphaned {
		t.Fatalf("Detach(s2) 準備: orphaned=%v err=%v", orphaned, err)
	}
	if removed, err = s.DeleteOrphanRecords(ctx, "K", "b.md"); err != nil || removed != 1 {
		t.Fatalf("DeleteOrphanRecords(3回目): removed=%d err=%v", removed, err)
	}
	if got := docCount(t, s, "K"); got != 1 {
		t.Errorf("b.md 全回収後 doc_count = %d, want 1", got)
	}
}

// -----------------------------------------------------------------------
// SweepPendingDeletions — 起動時スイープ（GC-02〜04）
// -----------------------------------------------------------------------

// TestSweepPendingDeletions_SeriesAndPathUnits は series 単位（DeleteSeriesAll）・
// path 単位（DeleteOrphanRecords）それぞれが正しく物理削除することを検証する。
func TestSweepPendingDeletions_SeriesAndPathUnits(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// 予約なしのスイープは 0 件処理・エラーなし
	if processed, errs := s.SweepPendingDeletions(ctx); processed != 0 || len(errs) != 0 {
		t.Fatalf("空スイープ: processed=%d errs=%v, want 0/空", processed, errs)
	}

	// series 単位予約の対象: path x は dead 単独、path y は dead+main の共有
	recX, err := s.UpsertRecord(ctx, Record{
		Key: "K", Path: "x.md", ContentHash: "h_x", Series: "dead",
		Chunks: makeChunks("x"),
	})
	if err != nil {
		t.Fatal(err)
	}
	recY, err := s.UpsertRecord(ctx, Record{
		Key: "K", Path: "y.md", ContentHash: "h_y", Series: "dead",
		Chunks: makeChunks("y"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendSeries(ctx, recY, "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkSeriesForDeletion(ctx, "K", "dead"); err != nil {
		t.Fatal(err)
	}

	// path 単位予約の対象: 切り離し済み orphan
	recZ, err := s.UpsertRecord(ctx, Record{
		Key: "K", Path: "z.md", ContentHash: "h_z", Series: "s1",
		Chunks: makeChunks("z"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if orphaned, err := s.DetachSeriesFromPath(ctx, "K", "s1", "z.md"); err != nil || !orphaned {
		t.Fatalf("Detach 準備: orphaned=%v err=%v", orphaned, err)
	}
	if err := s.MarkDocumentForDeletion(ctx, "K", "s1", "z.md"); err != nil {
		t.Fatal(err)
	}

	processed, errs := s.SweepPendingDeletions(ctx)
	if len(errs) != 0 {
		t.Fatalf("errs = %v, want 空", errs)
	}
	if processed != 2 {
		t.Errorf("processed = %d, want 2", processed)
	}

	// series 単位: dead 単独の x は物理削除、main 共有の y は保持（DEL-03 と同じ安全条件）
	recordAlive(t, s, recX, 0)
	recordAlive(t, s, recY, 1)
	if n := countRows(t, s,
		`SELECT COUNT(*) FROM series_keys WHERE record_id=? AND series='dead'`, recY); n != 0 {
		t.Errorf("y.md に dead の紐付けが残存 (count=%d)", n)
	}
	// path 単位: orphan z は物理削除
	recordAlive(t, s, recZ, 0)

	// 成功した予約行は全て除去されている
	if n := countRows(t, s, `SELECT COUNT(*) FROM pending_deletions`); n != 0 {
		t.Errorf("スイープ後も pending_deletions に %d 行残存", n)
	}
}

// TestSweepPendingDeletions_StaleRowKeepsLiveRecord は stale 予約行の無害性を検証する
// 最重要回帰テスト: path 単位の削除予約が残ったまま record が復活（series 再紐付け）した
// 状態でスイープしても、live record（chunks/embeddings 含む）が保持され予約行のみ除去される。
// DeleteSeries ベースのスイープ（series 剥がし + 空 record 削除）への退行を直接検出する。
func TestSweepPendingDeletions_StaleRowKeepsLiveRecord(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	recID, err := s.UpsertRecord(ctx, Record{
		Key: "K", Path: "revived.md", ContentHash: "h", Series: "s1",
		Chunks: makeChunks("body"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// 切り離し + 削除予約（sync の欠落検出相当）
	if orphaned, err := s.DetachSeriesFromPath(ctx, "K", "s1", "revived.md"); err != nil || !orphaned {
		t.Fatalf("Detach 準備: orphaned=%v err=%v", orphaned, err)
	}
	if err := s.MarkDocumentForDeletion(ctx, "K", "s1", "revived.md"); err != nil {
		t.Fatal(err)
	}
	// 予約解除を経ずに record が復活（DIF-02 経路の series 再紐付け相当）→ 予約行が stale 化
	if err := s.AppendSeries(ctx, recID, "s1"); err != nil {
		t.Fatal(err)
	}

	processed, errs := s.SweepPendingDeletions(ctx)
	if len(errs) != 0 {
		t.Fatalf("errs = %v, want 空", errs)
	}
	if processed != 1 {
		t.Errorf("processed = %d, want 1", processed)
	}

	// live record は chunks/embeddings 含めて保持され、検索にも現れる
	recordAlive(t, s, recID, 1)
	if !searchPaths(t, s, "K", "s1")["revived.md"] {
		t.Error("スイープ後、復活済み record が series=s1 の検索から消えた（live record が破壊された）")
	}
	// stale な予約行のみが除去される（0 件処理の冪等動作）
	if n := pendingRowCount(t, s, "K", "s1", "revived.md"); n != 0 {
		t.Errorf("stale 予約行が除去されていない (count=%d)", n)
	}
}

// TestSweepPendingDeletions_SharedHashRecordSurvives は最重要回帰テスト:
// 同一 content_hash の record を複数 series が参照している状態で片方だけ切り離し・
// スイープしても record が残ること（他 series の紐付きが守られること）を検証する。
func TestSweepPendingDeletions_SharedHashRecordSurvives(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// 同一 content_hash を s1 と s2 が共有する record（DIF-02 の series 追記状態）
	recID, err := s.UpsertRecord(ctx, Record{
		Key: "K", Path: "shared.md", ContentHash: "h_shared", Series: "s1",
		Chunks: makeChunks("shared body"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendSeries(ctx, recID, "s2"); err != nil {
		t.Fatal(err)
	}

	// s1 だけ切り離す。s2 が残るため orphaned=false（本来予約は不要だが、
	// stale 予約が紛れ込んだ最悪ケースを再現するため敢えて予約を記録してスイープする）
	if orphaned, err := s.DetachSeriesFromPath(ctx, "K", "s1", "shared.md"); err != nil || orphaned {
		t.Fatalf("Detach: orphaned=%v err=%v, want false/nil", orphaned, err)
	}
	if err := s.MarkDocumentForDeletion(ctx, "K", "s1", "shared.md"); err != nil {
		t.Fatal(err)
	}

	processed, errs := s.SweepPendingDeletions(ctx)
	if len(errs) != 0 {
		t.Fatalf("errs = %v, want 空", errs)
	}
	if processed != 1 {
		t.Errorf("processed = %d, want 1", processed)
	}

	// record は s2 の下で生き続ける
	recordAlive(t, s, recID, 1)
	if !searchPaths(t, s, "K", "s2")["shared.md"] {
		t.Error("スイープ後、s2 参照の record が検索から消えた（共有 record が破壊された）")
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM pending_deletions`); n != 0 {
		t.Errorf("スイープ後も pending_deletions に %d 行残存", n)
	}
}

// -----------------------------------------------------------------------
// DeleteKey / DeleteSeriesAll — pending_deletions の同一 tx 解除（stale 予約の残留防止）
// -----------------------------------------------------------------------

// TestDeleteKey_ClearsPendingDeletions は、DeleteKey が同一 tx で当該 key の
// pending_deletions（series 全体予約・path 単位予約の両方）を除去し、別 key の
// 予約行には触れないことを検証する。除去しない実装への退行は、KEY 再作成後の
// 起動時スイープが stale 予約で新データを破壊する事故につながる。
func TestDeleteKey_ClearsPendingDeletions(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// 削除対象 key の実データ + series 全体予約 + path 単位予約
	if _, err := s.UpsertRecord(ctx, Record{
		Key: "K", Path: "a.md", ContentHash: "h_a", Series: "s1",
		Chunks: makeChunks("a"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkSeriesForDeletion(ctx, "K", "s1"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDocumentForDeletion(ctx, "K", "s1", "a.md"); err != nil {
		t.Fatal(err)
	}
	// 別 key の予約（残るべきノイズ行）
	if _, err := s.MarkSeriesForDeletion(ctx, "OTHER", "s1"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDocumentForDeletion(ctx, "OTHER", "s1", "noise.md"); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteKey(ctx, "K"); err != nil {
		t.Fatalf("DeleteKey: %v", err)
	}

	// 当該 key の予約行は series 全体・path 単位とも 0 件
	if n := countRows(t, s, `SELECT COUNT(*) FROM pending_deletions WHERE key='K'`); n != 0 {
		t.Errorf("DeleteKey 後も key=K の予約行が %d 件残存（stale 予約の残留）", n)
	}
	// 別 key の予約行は無傷
	if n := countRows(t, s, `SELECT COUNT(*) FROM pending_deletions WHERE key='OTHER'`); n != 2 {
		t.Errorf("別 key の予約行数 = %d, want 2（DeleteKey が他 key の予約に触れた）", n)
	}
}

// TestDeleteSeriesAll_ClearsPendingDeletions は、DeleteSeriesAll が同一 tx で当該
// key+series の pending_deletions（series 全体予約・path 単位予約の両方）を除去し、
// 同一 key の別 series の予約行には触れないことを検証する。
func TestDeleteSeriesAll_ClearsPendingDeletions(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// s1 / s2 それぞれに実データ + series 全体予約 + path 単位予約
	for _, sr := range []string{"s1", "s2"} {
		if _, err := s.UpsertRecord(ctx, Record{
			Key: "K", Path: sr + ".md", ContentHash: "h_" + sr, Series: sr,
			Chunks: makeChunks(sr),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.MarkSeriesForDeletion(ctx, "K", sr); err != nil {
			t.Fatal(err)
		}
		if err := s.MarkDocumentForDeletion(ctx, "K", sr, sr+".md"); err != nil {
			t.Fatal(err)
		}
	}

	if _, _, err := s.DeleteSeriesAll(ctx, "K", "s1"); err != nil {
		t.Fatalf("DeleteSeriesAll: %v", err)
	}

	// s1 の予約行は series 全体・path 単位とも 0 件
	if n := countRows(t, s,
		`SELECT COUNT(*) FROM pending_deletions WHERE key='K' AND series='s1'`); n != 0 {
		t.Errorf("DeleteSeriesAll 後も series=s1 の予約行が %d 件残存（stale 予約の残留）", n)
	}
	// s2 の予約行は無傷
	if n := countRows(t, s,
		`SELECT COUNT(*) FROM pending_deletions WHERE key='K' AND series='s2'`); n != 2 {
		t.Errorf("series=s2 の予約行数 = %d, want 2（DeleteSeriesAll が別 series の予約に触れた）", n)
	}
}

// TestSweep_StaleSeriesWideReservation_DoesNotDestroyRecreatedData は最重要回帰テスト:
// series 全体の削除予約 → DeleteKey（delete_index 相当）→ 同一 key・series への record
// 再投入、の後に起動時スイープを実行しても再投入データが破壊されないことを検証する。
// 修正前は DeleteKey が pending_deletions を除去しなかったため、stale な series 全体予約が
// スイープ時に DeleteSeriesAll を発動し、再投入した新データを丸ごと消していた（データ消失）。
func TestSweep_StaleSeriesWideReservation_DoesNotDestroyRecreatedData(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// (a) 旧データ投入 → series 全体予約 → DeleteKey（delete_index 相当）
	if _, err := s.UpsertRecord(ctx, Record{
		Key: "K", Path: "doc.md", ContentHash: "h_v1", Series: "s",
		Chunks: makeChunks("v1"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkSeriesForDeletion(ctx, "K", "s"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteKey(ctx, "K"); err != nil {
		t.Fatalf("DeleteKey: %v", err)
	}

	// (b) 同一 key・series へ record を再投入（KEY の再作成）
	newID, err := s.UpsertRecord(ctx, Record{
		Key: "K", Path: "doc.md", ContentHash: "h_v2", Series: "s",
		Chunks: makeChunks("v2 one", "v2 two"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// (c) 起動時スイープ（stale 予約が残っていればここで新データが消える）
	processed, errs := s.SweepPendingDeletions(ctx)
	if len(errs) != 0 {
		t.Fatalf("errs = %v, want 空", errs)
	}
	if processed != 0 {
		t.Errorf("processed = %d, want 0（DeleteKey で予約が除去済みのはず）", processed)
	}

	// 再投入した record・chunks・embeddings が無傷で残り、検索にも現れる
	recordAlive(t, s, newID, 2)
	if !searchPaths(t, s, "K", "s")["doc.md"] {
		t.Error("スイープ後、再投入した doc.md が series=s の検索から消えた（stale 予約による新データ破壊）")
	}
	// pending_deletions は 0 件
	if n := countRows(t, s, `SELECT COUNT(*) FROM pending_deletions`); n != 0 {
		t.Errorf("pending_deletions に %d 行残存", n)
	}
}

// TestSweepPendingDeletions_PartialFailureContinues は個別失敗時にログ記録の上で
// 処理を継続することを検証する（silent failure 禁止方針、GC-04）。
// SQLite トリガーで特定 path の record 削除だけを失敗させ、他の予約行の処理と
// 失敗行の予約保持（次回起動時の再試行）を確認する。
func TestSweepPendingDeletions_PartialFailureContinues(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// slog 出力を捕捉する（ログ記録されることの検証用）
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// 失敗する予約: poison.md の orphan（トリガーで削除を失敗させる）
	poisonID, err := s.UpsertRecord(ctx, Record{
		Key: "K", Path: "poison.md", ContentHash: "h_p", Series: "s1",
		Chunks: makeChunks("p"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// 成功する予約: ok.md の orphan
	okID, err := s.UpsertRecord(ctx, Record{
		Key: "K", Path: "ok.md", ContentHash: "h_ok", Series: "s1",
		Chunks: makeChunks("ok"),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"poison.md", "ok.md"} {
		if orphaned, err := s.DetachSeriesFromPath(ctx, "K", "s1", p); err != nil || !orphaned {
			t.Fatalf("Detach(%s): orphaned=%v err=%v", p, orphaned, err)
		}
		if err := s.MarkDocumentForDeletion(ctx, "K", "s1", p); err != nil {
			t.Fatal(err)
		}
	}

	// poison.md の record 削除だけを失敗させる注入トリガー
	if _, err := s.db.ExecContext(ctx, `
CREATE TRIGGER poison_delete BEFORE DELETE ON records
WHEN OLD.path = 'poison.md'
BEGIN
    SELECT RAISE(ABORT, 'injected failure');
END`); err != nil {
		t.Fatalf("トリガー作成: %v", err)
	}

	processed, errs := s.SweepPendingDeletions(ctx)

	// 片方の失敗にもかかわらずもう片方は処理される（継続動作）
	if processed != 1 {
		t.Errorf("processed = %d, want 1", processed)
	}
	if len(errs) != 1 {
		t.Fatalf("len(errs) = %d, want 1 (errs=%v)", len(errs), errs)
	}
	if !strings.Contains(errs[0].Error(), "poison.md") {
		t.Errorf("errs[0] に失敗 path が含まれない: %v", errs[0])
	}
	if !strings.Contains(logBuf.String(), "起動時スイープ個別失敗") {
		t.Errorf("個別失敗がログに記録されていない: %q", logBuf.String())
	}

	// 成功行: record 物理削除 + 予約行除去
	recordAlive(t, s, okID, 0)
	if n := pendingRowCount(t, s, "K", "s1", "ok.md"); n != 0 {
		t.Errorf("成功した予約行が除去されていない (count=%d)", n)
	}
	// 失敗行: record は無傷（トランザクション rollback）+ 予約行保持（次回起動時に再試行）
	recordAlive(t, s, poisonID, 1)
	if n := pendingRowCount(t, s, "K", "s1", "poison.md"); n != 1 {
		t.Errorf("失敗した予約行が保持されていない (count=%d)", n)
	}
}
