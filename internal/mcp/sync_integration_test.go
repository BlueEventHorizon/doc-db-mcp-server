package mcp

// TASK-013 — sync_documents / get_sync_status / schedule_delete_series 統合テスト
// (DES-003 §6 統合テスト対象、APP-003 SYN-01〜08 / GC-01 / GC-05)
//
// 検証項目（計画書の 14 項目に対応）:
//    1. sync_documents が job_id を即座に返しブロックしない（SYN-05）
//    2. get_sync_status の進捗・完了反映と、存在しない / 期限切れ job_id のエラー（SYN-06）
//    3. 検索最新性の回帰テスト: sync 完了直後に削除 path が series 指定検索から消える（SYN-03）
//    4. orphan record にのみ削除予約が作られ、再度含まれると解除される（SYN-03/04）
//    5. 自己修復の API 課金ゼロ検証（Embedder spy、SYN-04）
//    6. orphan 非リーク検証: 別内容で復活すると旧 orphan が物理削除され record 数 1（DIF-03 経路）
//    7. 失敗 path の予約保持検証（DES-003 §3.3 ClearPendingDeletion の実行条件）
//    8. CleanOtherSeries 失敗補償の検証（人工 orphan の回収 → 予約解除）
//    9. schedule_delete_series の series 全体予約が sync_documents で解除される（自己修復）
//   10. schedule_delete_series が即座に削除せず already_scheduled を正しく返す（GC-01）
//   11. sync_documents 処理中の同一 KEY への upsert_documents / delete_index がブロックされる（SYN-08）
//   12. sync_documents 処理中の同一 KEY への TTL/LRU 相当（WithKeyLock+DeleteKey）がブロックされる（SYN-08）
//   13. root context cancel 済みで sync_documents を実行するとジョブが failed（GC-05）
//   14. MCP リクエスト context を cancel してもジョブは done まで完走する（GC-05 再発防止）
//
// 非同期ジョブの完了待ちは get_sync_status ポーリング + deadline（keyLockITTimeout）で行い、
// ロック保持中の停止には keylock_integration_test.go の gatedEmbedder / installGate を流用する。
// Embedder 呼び出し観測には upsert_integration_test.go の spyEmbedder を流用する。

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BlueEventHorizon/doc-db-mcp-server/internal/chunker"
	"github.com/BlueEventHorizon/doc-db-mcp-server/internal/search"
	"github.com/BlueEventHorizon/doc-db-mcp-server/internal/store"
)

// -----------------------------------------------------------------------
// ヘルパー
// -----------------------------------------------------------------------

// newRootCtxHarness は newHarness と同構成で rootCtx だけ差し替えたハーネスを作る。
// GC-05 系テスト（項目 13）で「サーバーシャットダウン済み」の rootCtx を注入するために使う
// （newHarness は context.Background() 固定のため rootCtx を制御できない）。
func newRootCtxHarness(t *testing.T, rootCtx context.Context) *testHarness {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.New(dbPath, testDim)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	emb := &fakeEmbedder{fixed: []float32{1, 0, 0}}
	fe := &fakeFetcher{}
	ch := chunker.New(1500)
	pipe := search.New(st, &SearchEmbedderAdapter{Inner: emb}, nil, search.Config{})
	h := New(rootCtx, st, ch, emb, fe, pipe)
	return &testHarness{t: t, store: st, embedder: emb, fetcher: fe, handlers: h}
}

// callSyncDocuments は sync_documents を呼び、job_id が即座に返ること（SYN-05）を
// タイムアウト付きで検証してから job_id を返す。実装がブロッキングに退行した場合、
// テスト全体が hang する前にここで fail する。
func callSyncDocuments(t *testing.T, h *testHarness, ctx context.Context, key, series string, docs []UpsertDocument) string {
	t.Helper()
	type outcome struct {
		out SyncResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		_, out, err := h.handlers.handleSyncDocuments(ctx, nil, SyncInput{
			Key: key, Series: series, Documents: docs,
		})
		done <- outcome{out: out, err: err}
	}()
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("sync_documents error: %v", res.err)
		}
		if res.out.JobID == "" {
			t.Fatal("sync_documents が空の job_id を返した")
		}
		return res.out.JobID
	case <-time.After(keyLockITTimeout):
		t.Fatal("sync_documents が即座に返らない（SYN-05 違反: 処理完了を待たず job_id を返すべき）")
		return "" // unreachable
	}
}

// getSyncStatus は get_sync_status を 1 回呼ぶ薄いラッパ。
func getSyncStatus(t *testing.T, h *testHarness, jobID string) GetSyncStatusResult {
	t.Helper()
	_, out, err := h.handlers.handleGetSyncStatus(context.Background(), nil, GetSyncStatusInput{JobID: jobID})
	if err != nil {
		t.Fatalf("get_sync_status(%s): %v", jobID, err)
	}
	return out
}

// waitSyncTerminal は get_sync_status を deadline 付きでポーリングし、
// running 以外（done / failed）になった時点の状態を返す。
func waitSyncTerminal(t *testing.T, h *testHarness, jobID string) GetSyncStatusResult {
	t.Helper()
	deadline := time.Now().Add(keyLockITTimeout)
	for {
		out := getSyncStatus(t, h, jobID)
		if out.Status != "running" {
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s が %v 以内に完了しない", jobID, keyLockITTimeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// syncAndWaitDone は sync_documents 投入 → done 完了待ちまでをまとめて行う。
func syncAndWaitDone(t *testing.T, h *testHarness, key, series string, docs []UpsertDocument) GetSyncStatusResult {
	t.Helper()
	jobID := callSyncDocuments(t, h, context.Background(), key, series, docs)
	out := waitSyncTerminal(t, h, jobID)
	if out.Status != "done" {
		t.Fatalf("job %s status=%q, want done (errors=%v)", jobID, out.Status, out.Errors)
	}
	return out
}

// chunkPathSet は GetChunksForSearch の結果から path 集合を作る。
func chunkPathSet(t *testing.T, h *testHarness, key, series string) map[string]bool {
	t.Helper()
	chunks, err := h.store.GetChunksForSearch(context.Background(), key, series)
	if err != nil {
		t.Fatalf("GetChunksForSearch(%s, %s): %v", key, series, err)
	}
	set := make(map[string]bool, len(chunks))
	for _, c := range chunks {
		set[c.Path] = true
	}
	return set
}

// pendingFor は ListPendingDeletions の結果を返す薄いラッパ。
func pendingFor(t *testing.T, h *testHarness, key, series string) (paths []string, seriesWide bool) {
	t.Helper()
	paths, seriesWide, err := h.store.ListPendingDeletions(context.Background(), key, series)
	if err != nil {
		t.Fatalf("ListPendingDeletions(%s, %s): %v", key, series, err)
	}
	return paths, seriesWide
}

// -----------------------------------------------------------------------
// 項目 1: SYN-05 — job_id の即時返却
// -----------------------------------------------------------------------

// TestSyncDocuments_SYN05_ReturnsJobIDImmediately は、ジョブ本体が Embed ゲートで
// 停止している間でも sync_documents ハンドラが job_id を即座に返し、その時点の
// ジョブ状態が running であることを検証する（SYN-05）。
func TestSyncDocuments_SYN05_ReturnsJobIDImmediately(t *testing.T) {
	h := newHarness(t)
	entered, release := installGate(t, h)

	// ゲート対象（マーカー入り新規内容）を含む sync を投入する。
	// callSyncDocuments 自体が「即座に返る」ことをタイムアウト付きで検証する。
	jobID := callSyncDocuments(t, h, context.Background(), "K", "s", []UpsertDocument{
		{Path: "gate.md", Content: "# H\n" + keyLockGateMarker + " body"},
	})

	// ジョブ goroutine がゲートに到達する（= まだ処理中）まで待つ
	select {
	case <-entered:
	case <-time.After(keyLockITTimeout):
		t.Fatal("sync ジョブがゲート（Embed）に到達しない")
	}

	// ハンドラ応答後もジョブは running（処理完了を待たずに返した証明）
	if got := getSyncStatus(t, h, jobID); got.Status != "running" {
		t.Errorf("ゲート保持中の status=%q, want running", got.Status)
	}

	release()
	out := waitSyncTerminal(t, h, jobID)
	if out.Status != "done" || out.Processed != 1 {
		t.Errorf("完了状態 = %+v, want done/Processed=1", out)
	}
}

// -----------------------------------------------------------------------
// 項目 2: SYN-06 — 進捗・完了の反映と job_id エラー
// -----------------------------------------------------------------------

// TestGetSyncStatus_SYN06_ReflectsProgressAndCompletion は、完了後の件数
// （processed / skipped / deleted_paths_marked）が正しく反映されることを検証する。
func TestGetSyncStatus_SYN06_ReflectsProgressAndCompletion(t *testing.T) {
	h := newHarness(t)

	// 1 回目: 2 件とも新規 → processed=2
	out := syncAndWaitDone(t, h, "K", "s", []UpsertDocument{
		{Path: "a.md", Content: "# H\nalpha"},
		{Path: "b.md", Content: "# H\nbeta"},
	})
	if out.Processed != 2 || out.Skipped != 0 || out.Failed != 0 || out.DeletedPathsMarked != 0 {
		t.Errorf("1回目 = %+v, want Processed=2 その他 0", out)
	}

	// 2 回目: a.md のみ（同一内容）→ skipped=1、b.md 欠落 → deleted_paths_marked=1
	out = syncAndWaitDone(t, h, "K", "s", []UpsertDocument{
		{Path: "a.md", Content: "# H\nalpha"},
	})
	if out.Processed != 0 || out.Skipped != 1 || out.Failed != 0 || out.DeletedPathsMarked != 1 {
		t.Errorf("2回目 = %+v, want Skipped=1 DeletedPathsMarked=1", out)
	}
}

// TestGetSyncStatus_SYN06_UnknownJobIDErrors は、存在しない job_id と空の job_id が
// エラーになることを検証する。
func TestGetSyncStatus_SYN06_UnknownJobIDErrors(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if _, _, err := h.handlers.handleGetSyncStatus(ctx, nil, GetSyncStatusInput{JobID: "no-such-job"}); err == nil {
		t.Error("存在しない job_id でエラーが返らない")
	}
	if _, _, err := h.handlers.handleGetSyncStatus(ctx, nil, GetSyncStatusInput{JobID: ""}); err == nil {
		t.Error("空の job_id でエラーが返らない")
	}
}

// TestGetSyncStatus_SYN06_EvictedJobIDErrors は、完了済みジョブが保持上限
// （maxCompletedSyncJobs）超過で追い出された後の job_id がエラーになることを検証する
// （SYN-06 の「保持期限切れ」）。createdAt を backdate して追い出し順を決定的にする。
func TestGetSyncStatus_SYN06_EvictedJobIDErrors(t *testing.T) {
	h := newHarness(t)

	// 最古の完了済みジョブを登録する（backdate で追い出し第一候補にする）
	h.handlers.registerSyncJob("oldest-job")
	h.handlers.updateSyncJob("oldest-job", func(st *SyncJobStatus) {
		st.Status = "done"
		st.createdAt = time.Now().Add(-time.Hour)
	})

	// 完了済みジョブを上限まで積むと、上限到達後の registerSyncJob が最古を追い出す
	for i := 0; i < maxCompletedSyncJobs; i++ {
		id := fmt.Sprintf("filler-%03d", i)
		h.handlers.registerSyncJob(id)
		h.handlers.updateSyncJob(id, func(st *SyncJobStatus) { st.Status = "done" })
	}

	if _, _, err := h.handlers.handleGetSyncStatus(context.Background(), nil,
		GetSyncStatusInput{JobID: "oldest-job"}); err == nil {
		t.Error("追い出し済み job_id でエラーが返らない（保持期限切れの検出漏れ）")
	}
	// 直近のジョブは保持されている
	if got := getSyncStatus(t, h, fmt.Sprintf("filler-%03d", maxCompletedSyncJobs-1)); got.Status != "done" {
		t.Errorf("直近ジョブの status=%q, want done", got.Status)
	}
}

// -----------------------------------------------------------------------
// 項目 3: SYN-03 — 検索最新性の回帰テスト
// -----------------------------------------------------------------------

// TestSyncDocuments_SYN03_DetachedPathDisappearsFromSeriesSearch は、desired-state から
// 欠落した path が sync 完了直後に当該 series 指定の検索から消えることを検証する。
// 「削除予約はされたが検索に残り続ける」実装（DES-003 1.5 以前の仕様）への退行を検出する。
func TestSyncDocuments_SYN03_DetachedPathDisappearsFromSeriesSearch(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	syncAndWaitDone(t, h, "K", "s", []UpsertDocument{
		{Path: "a.md", Content: "# H\nalpha"},
		{Path: "b.md", Content: "# H\nbeta"},
	})

	// b.md が desired-state から欠落した sync
	out := syncAndWaitDone(t, h, "K", "s", []UpsertDocument{
		{Path: "a.md", Content: "# H\nalpha"},
	})
	if out.DeletedPathsMarked != 1 {
		t.Errorf("DeletedPathsMarked = %d, want 1", out.DeletedPathsMarked)
	}

	// sync 完了直後、series 指定の検索経路に b.md の chunk が返らない
	paths := chunkPathSet(t, h, "K", "s")
	if paths["b.md"] {
		t.Error("削除された b.md の chunk が series 指定検索に残っている（SYN-03 退行）")
	}
	if !paths["a.md"] {
		t.Error("a.md の chunk が series 指定検索から消えている")
	}

	// orphan record 自体は自己修復（SYN-04 の API 課金ゼロ）のため物理的に残っている
	found, err := h.store.HasRecord(ctx, "K", "b.md")
	if err != nil {
		t.Fatalf("HasRecord: %v", err)
	}
	if !found {
		t.Error("b.md の orphan record が物理削除されている（起動時スイープまで保持されるべき）")
	}
}

// -----------------------------------------------------------------------
// 項目 4: SYN-03/04 — orphan record のみ削除予約 + 再包含での解除
// -----------------------------------------------------------------------

// TestSyncDocuments_SYN03_ReservationOnlyForOrphans は、desired-state から欠落した path の
// うち orphan になった record にのみ物理削除予約が作られ（他 series 参照 record には作られず）、
// 再度 documents に含まれると予約が解除されることを検証する。
func TestSyncDocuments_SYN03_ReservationOnlyForOrphans(t *testing.T) {
	h := newHarness(t)

	// series s: a.md / b.md / c.md、series s2: b.md（同一内容 → DIF-02 で record 共有）
	syncAndWaitDone(t, h, "K", "s", []UpsertDocument{
		{Path: "a.md", Content: "# H\nalpha"},
		{Path: "b.md", Content: "# H\nbeta"},
		{Path: "c.md", Content: "# H\ngamma"},
	})
	seedUpsert(t, h, "K", "s2", "b.md", "# H\nbeta")

	// s から b.md / c.md が欠落 → b.md は s2 が参照するので orphan にならず、c.md のみ orphan
	out := syncAndWaitDone(t, h, "K", "s", []UpsertDocument{
		{Path: "a.md", Content: "# H\nalpha"},
	})
	if out.DeletedPathsMarked != 1 {
		t.Errorf("DeletedPathsMarked = %d, want 1 (orphan の c.md のみ)", out.DeletedPathsMarked)
	}

	paths, seriesWide := pendingFor(t, h, "K", "s")
	if len(paths) != 1 || paths[0] != "c.md" {
		t.Errorf("pending paths = %v, want [c.md]（他 series 参照の b.md に予約を作ってはならない）", paths)
	}
	if seriesWide {
		t.Error("series 全体予約が誤って作られている")
	}

	// b.md は s2 の下で生き続ける
	if s2paths := chunkPathSet(t, h, "K", "s2"); !s2paths["b.md"] {
		t.Error("他 series (s2) が参照する b.md が s2 の検索から消えている")
	}

	// c.md を再度含めて sync すると予約が解除される（SYN-04）
	syncAndWaitDone(t, h, "K", "s", []UpsertDocument{
		{Path: "a.md", Content: "# H\nalpha"},
		{Path: "c.md", Content: "# H\ngamma"},
	})
	if paths, _ := pendingFor(t, h, "K", "s"); len(paths) != 0 {
		t.Errorf("再包含後も削除予約が残っている: %v", paths)
	}
}

// -----------------------------------------------------------------------
// 項目 5: SYN-04 — 自己修復の API 課金ゼロ検証
// -----------------------------------------------------------------------

// TestSyncDocuments_SYN04_SelfHealZeroEmbedderCalls は、切り離し・削除予約済みの path を
// 同一内容で再 sync すると (a) Embedder が呼ばれず（DIF-02 の record 再発見・再紐付け）、
// (b) 削除予約が解除され、(c) 当該 series 指定の検索に再び現れることを検証する。
func TestSyncDocuments_SYN04_SelfHealZeroEmbedderCalls(t *testing.T) {
	h := newHarness(t)

	syncAndWaitDone(t, h, "K", "s", []UpsertDocument{
		{Path: "a.md", Content: "# H\nalpha"},
		{Path: "c.md", Content: "# H\ngamma"},
	})
	// c.md を欠落させて orphan + 削除予約を作る
	syncAndWaitDone(t, h, "K", "s", []UpsertDocument{
		{Path: "a.md", Content: "# H\nalpha"},
	})
	if paths, _ := pendingFor(t, h, "K", "s"); len(paths) != 1 || paths[0] != "c.md" {
		t.Fatalf("前提: pending paths = %v, want [c.md]", paths)
	}

	// Embedder を spy に差し替えて、同一内容で c.md を復活させる
	spy := &spyEmbedder{inner: h.embedder}
	h.handlers.embedder = spy
	out := syncAndWaitDone(t, h, "K", "s", []UpsertDocument{
		{Path: "a.md", Content: "# H\nalpha"},
		{Path: "c.md", Content: "# H\ngamma"},
	})

	// (a) Embedder 呼び出しゼロ = API 課金ゼロ（両 path とも DIF-02 skip）
	if calls := atomic.LoadInt32(&spy.calls); calls != 0 {
		t.Errorf("自己修復で Embedder が %d 回呼ばれた（API 課金ゼロであるべき）", calls)
	}
	if out.Skipped != 2 || out.Processed != 0 || out.Failed != 0 {
		t.Errorf("復活 sync = %+v, want Skipped=2", out)
	}
	// (b) 削除予約が解除されている
	if paths, _ := pendingFor(t, h, "K", "s"); len(paths) != 0 {
		t.Errorf("自己修復後も削除予約が残っている: %v", paths)
	}
	// (c) series 指定の検索に再び現れる
	if paths := chunkPathSet(t, h, "K", "s"); !paths["c.md"] {
		t.Error("自己修復後も c.md が series 指定検索に現れない")
	}
}

// -----------------------------------------------------------------------
// 項目 6: orphan 非リーク検証 — 別内容での復活
// -----------------------------------------------------------------------

// TestSyncDocuments_OrphanDoesNotLeakOnDifferentContentResync は、切り離し・削除予約済みの
// path を別内容で再 sync すると、旧 orphan record が CleanOtherSeries（DIF-03 経路の既存機構）
// で物理削除され、同一 key+path の record 数が 1 になることを検証する（レビュー指摘の再発防止）。
func TestSyncDocuments_OrphanDoesNotLeakOnDifferentContentResync(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	syncAndWaitDone(t, h, "K", "s", []UpsertDocument{
		{Path: "a.md", Content: "# H\nalpha"},
		{Path: "c.md", Content: "# H\ngamma v1"},
	})
	// c.md を欠落させて orphan + 削除予約を作る
	syncAndWaitDone(t, h, "K", "s", []UpsertDocument{
		{Path: "a.md", Content: "# H\nalpha"},
	})

	// 別内容で復活（DIF-03 経路）
	out := syncAndWaitDone(t, h, "K", "s", []UpsertDocument{
		{Path: "a.md", Content: "# H\nalpha"},
		{Path: "c.md", Content: "# H\ngamma v2 — different"},
	})
	if out.Processed != 1 || out.Skipped != 1 {
		t.Errorf("復活 sync = %+v, want Processed=1 (c.md) Skipped=1 (a.md)", out)
	}

	// 同一 key+path の record 数が 1（旧 orphan が物理削除されている）
	chunks, err := h.store.GetChunksForSearch(ctx, "K", "")
	if err != nil {
		t.Fatalf("GetChunksForSearch: %v", err)
	}
	recordIDs := make(map[int64]bool)
	for _, c := range chunks {
		if c.Path == "c.md" {
			recordIDs[c.RecordID] = true
			if strings.Contains(c.Text, "v1") {
				t.Errorf("旧内容 (v1) の chunk が残っている: %q", c.Text)
			}
		}
	}
	if len(recordIDs) != 1 {
		t.Errorf("c.md の record 数 = %d, want 1（旧 orphan がリークしている）", len(recordIDs))
	}

	// 予約解除済みで、orphan も残っていない（DeleteOrphanRecords は冪等な 0 件処理になる）
	if paths, _ := pendingFor(t, h, "K", "s"); len(paths) != 0 {
		t.Errorf("復活後も削除予約が残っている: %v", paths)
	}
	removed, err := h.store.DeleteOrphanRecords(ctx, "K", "c.md")
	if err != nil {
		t.Fatalf("DeleteOrphanRecords: %v", err)
	}
	if removed != 0 {
		t.Errorf("orphan が %d 件リークしていた（sync 内で回収されているべき）", removed)
	}
}

// -----------------------------------------------------------------------
// 項目 7: 失敗 path の予約保持検証
// -----------------------------------------------------------------------

// TestSyncDocuments_FailedPathKeepsReservation は、documents に含まれる path の upsertOne が
// 失敗した場合、当該 path の削除予約が解除されずに残ることを検証する
// （DES-003 §3.3 ClearPendingDeletion の実行条件: 成功 path のみ解除）。
func TestSyncDocuments_FailedPathKeepsReservation(t *testing.T) {
	h := newHarness(t)

	syncAndWaitDone(t, h, "K", "s", []UpsertDocument{
		{Path: "a.md", Content: "# H\nalpha"},
		{Path: "c.md", Content: "# H\ngamma"},
	})
	// c.md を欠落させて orphan + 削除予約を作る
	syncAndWaitDone(t, h, "K", "s", []UpsertDocument{
		{Path: "a.md", Content: "# H\nalpha"},
	})

	// c.md を fetch 失敗する URL で復活させようとする → upsertOne が失敗する
	h.fetcher.errs = map[string]error{
		"http://example.com/c.md": fmt.Errorf("connection refused"),
	}
	out := syncAndWaitDone(t, h, "K", "s", []UpsertDocument{
		{Path: "a.md", Content: "# H\nalpha"},
		{Path: "c.md", URL: "http://example.com/c.md"},
	})
	if out.Failed != 1 {
		t.Errorf("sync = %+v, want Failed=1 (c.md の fetch 失敗)", out)
	}
	if len(out.Errors) == 0 {
		t.Error("失敗の詳細が errors に記録されていない（silent failure 禁止）")
	}

	// 失敗 path の削除予約は保持される（解除すると旧 orphan の回収手段が失われる）
	paths, _ := pendingFor(t, h, "K", "s")
	if len(paths) != 1 || paths[0] != "c.md" {
		t.Errorf("失敗 path の予約が保持されていない: pending = %v, want [c.md]", paths)
	}
}

// -----------------------------------------------------------------------
// 項目 8: CleanOtherSeries 失敗補償の検証
// -----------------------------------------------------------------------

// TestSyncDocuments_CompensatesOrphanBeforeClearingReservation は、削除予約中の path に
// store 層で直接 orphan record を人工的に用意した状態（CleanOtherSeries が掃除し損ねた状況の
// 再現）で当該 path を含む sync を実行すると、DeleteOrphanRecords により orphan が回収されて
// から予約が解除され、同一 key+path に orphan が残らないことを検証する（DES-003 §3.3 2 段階解除）。
func TestSyncDocuments_CompensatesOrphanBeforeClearingReservation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	syncAndWaitDone(t, h, "K", "s", []UpsertDocument{
		{Path: "a.md", Content: "# H\nalpha"},
		{Path: "c.md", Content: "# H\ngamma"},
	})
	// c.md を欠落させて orphan + 削除予約を作る
	syncAndWaitDone(t, h, "K", "s", []UpsertDocument{
		{Path: "a.md", Content: "# H\nalpha"},
	})

	// CleanOtherSeries が掃除し損ねた状況の再現: 同一 key+path に series_keys を持たない
	// 別 content_hash の record を直接注入する
	const staleHash = "stale-orphan-hash-cleanotherseries-missed"
	if _, err := store.ExecForTest(ctx, h.store,
		`INSERT INTO records (key, path, content_hash, created_at, updated_at)
		 VALUES ('K', 'c.md', ?, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		staleHash,
	); err != nil {
		t.Fatalf("人工 orphan の注入: %v", err)
	}

	// c.md を同一内容で復活させる sync → 予約解除の直前に orphan が回収される
	syncAndWaitDone(t, h, "K", "s", []UpsertDocument{
		{Path: "a.md", Content: "# H\nalpha"},
		{Path: "c.md", Content: "# H\ngamma"},
	})

	// 予約は解除されている
	if paths, _ := pendingFor(t, h, "K", "s"); len(paths) != 0 {
		t.Errorf("復活後も削除予約が残っている: %v", paths)
	}
	// 人工 orphan は回収済み
	if id, err := h.store.FindRecord(ctx, "K", "c.md", staleHash); err != nil {
		t.Fatalf("FindRecord: %v", err)
	} else if id != 0 {
		t.Error("人工 orphan record が回収されていない（DeleteOrphanRecords 補償の退行）")
	}
	// 同一 key+path に orphan が残っていない（冪等な 0 件処理で確認）
	removed, err := h.store.DeleteOrphanRecords(ctx, "K", "c.md")
	if err != nil {
		t.Fatalf("DeleteOrphanRecords: %v", err)
	}
	if removed != 0 {
		t.Errorf("予約解除後も orphan が %d 件残っていた", removed)
	}
	// 復活した live record は無傷（series 指定検索に現れる）
	if paths := chunkPathSet(t, h, "K", "s"); !paths["c.md"] {
		t.Error("復活した c.md が series 指定検索に現れない（live record が壊された可能性）")
	}
}

// -----------------------------------------------------------------------
// 項目 9/10: schedule_delete_series（GC-01 + SYN-04 自己修復）
// -----------------------------------------------------------------------

// TestScheduleDeleteSeries_GC01_NoImmediateDeleteAndAlreadyScheduled は、
// schedule_delete_series が実データに一切触れず（即時削除しない）、
// already_scheduled を正しく返すことを検証する（GC-01）。
func TestScheduleDeleteSeries_GC01_NoImmediateDeleteAndAlreadyScheduled(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	seedUpsert(t, h, "K", "s", "a.md", "# H\nalpha")

	// 1 回目: 予約が新規に作られる
	_, out, err := h.handlers.handleScheduleDeleteSeries(ctx, nil, ScheduleDeleteSeriesInput{
		Key: "K", Series: "s",
	})
	if err != nil {
		t.Fatalf("schedule_delete_series: %v", err)
	}
	if out.AlreadyScheduled {
		t.Error("初回呼び出しで already_scheduled=true が返った")
	}
	if out.Key != "K" || out.Series != "s" {
		t.Errorf("echo back = %+v, want key=K series=s", out)
	}

	// 即時削除されない: record も series 指定検索も無傷
	if found, err := h.store.HasRecord(ctx, "K", "a.md"); err != nil || !found {
		t.Errorf("予約後に record が消えた（即時削除された）: found=%v err=%v", found, err)
	}
	if paths := chunkPathSet(t, h, "K", "s"); !paths["a.md"] {
		t.Error("予約後に series 指定検索から a.md が消えた（GC-01 は遅延方式であるべき）")
	}
	if _, seriesWide := pendingFor(t, h, "K", "s"); !seriesWide {
		t.Error("series 全体の削除予約が記録されていない")
	}

	// 2 回目: 冪等呼び出しの判別として already_scheduled=true
	_, out, err = h.handlers.handleScheduleDeleteSeries(ctx, nil, ScheduleDeleteSeriesInput{
		Key: "K", Series: "s",
	})
	if err != nil {
		t.Fatalf("schedule_delete_series (2回目): %v", err)
	}
	if !out.AlreadyScheduled {
		t.Error("2 回目の呼び出しで already_scheduled=false が返った")
	}
}

// TestScheduleDeleteSeries_SYN04_ClearedBySyncDocuments は、series 全体を削除予約した後に
// 同一 key・series へ sync_documents を呼ぶと予約が解除されることを検証する（SYN-04 自己修復）。
func TestScheduleDeleteSeries_SYN04_ClearedBySyncDocuments(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	seedUpsert(t, h, "K", "s", "a.md", "# H\nalpha")

	if _, _, err := h.handlers.handleScheduleDeleteSeries(ctx, nil, ScheduleDeleteSeriesInput{
		Key: "K", Series: "s",
	}); err != nil {
		t.Fatalf("schedule_delete_series: %v", err)
	}
	if _, seriesWide := pendingFor(t, h, "K", "s"); !seriesWide {
		t.Fatal("前提: series 全体予約が記録されていない")
	}

	// 同一 key・series への sync_documents で予約が解除される
	syncAndWaitDone(t, h, "K", "s", []UpsertDocument{
		{Path: "a.md", Content: "# H\nalpha"},
	})
	if _, seriesWide := pendingFor(t, h, "K", "s"); seriesWide {
		t.Error("sync_documents 後も series 全体の削除予約が残っている（SYN-04 自己修復の退行）")
	}
}

// -----------------------------------------------------------------------
// 項目 11: SYN-08 — sync 処理中の同一 KEY への upsert / delete_index のブロック
// -----------------------------------------------------------------------

// startGatedSync はマーカー入り内容を含む sync_documents を投入し、ジョブ goroutine が
// WithKeyLock 保持中に Embed ゲートで停止した状態になるまで待ってから job_id を返す。
func startGatedSync(t *testing.T, h *testHarness, entered <-chan struct{}, key, series string, docs []UpsertDocument) string {
	t.Helper()
	jobID := callSyncDocuments(t, h, context.Background(), key, series, docs)
	select {
	case <-entered:
		// sync ジョブが KEY ロックを保持したまま Embed ゲートで停止した
	case <-time.After(keyLockITTimeout):
		t.Fatal("sync ジョブがゲート（Embed）に到達しない")
	}
	return jobID
}

// TestSyncIntegration_SYN08_SyncBlocksUpsert は、sync_documents 処理中に同一 KEY への
// upsert_documents がブロックされることを検証する（最重要回帰テスト）。
// ブロックされずに割り込むと、upsert が追加した path を sync の desired-state 判定が
// 「一覧に無い path」として誤って切り離し・削除予約するという再発防止対象の不具合が起きる。
func TestSyncIntegration_SYN08_SyncBlocksUpsert(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	seedUpsert(t, h, "K", "s", "keep.md", "# H\nkeep")
	entered, release := installGate(t, h)

	// A: sync ジョブが KEY ロックを保持したままゲートで停止する
	jobID := startGatedSync(t, h, entered, "K", "s", []UpsertDocument{
		{Path: "keep.md", Content: "# H\nkeep"},
		{Path: "gate.md", Content: "# H\n" + keyLockGateMarker + " body"},
	})

	// B: 同一 KEY への upsert_documents は sync 完了までブロックされる
	bDone := make(chan upsertOutcome, 1)
	go func() {
		_, out, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
			Key: "K", Series: "s",
			Documents: []UpsertDocument{{Path: "new.md", Content: "# H\nnew"}},
		})
		bDone <- upsertOutcome{out: out, err: err}
	}()
	select {
	case b := <-bDone:
		t.Fatalf("sync 処理中に upsert_documents が完了した（SYN-08 排他が効いていない）: %+v", b)
	case <-time.After(100 * time.Millisecond):
		// ブロックされている（期待どおり）
	}

	release()
	if out := waitSyncTerminal(t, h, jobID); out.Status != "done" {
		t.Fatalf("sync job status=%q, want done (errors=%v)", out.Status, out.Errors)
	}
	select {
	case b := <-bDone:
		if b.err != nil || b.out.Processed != 1 {
			t.Errorf("解放後の upsert = %+v err=%v, want Processed=1", b.out, b.err)
		}
	case <-time.After(keyLockITTimeout):
		t.Fatal("sync 完了後も upsert_documents が完了しない")
	}

	// 直列化の帰結: upsert は sync の desired-state 判定の後に実行されたため、
	// new.md は切り離されず series に残り、削除予約も作られていない。
	if paths := chunkPathSet(t, h, "K", "s"); !paths["new.md"] {
		t.Error("new.md が series から消えている（upsert が sync に割り込み、誤って切り離された）")
	}
	if paths, _ := pendingFor(t, h, "K", "s"); len(paths) != 0 {
		t.Errorf("削除予約が誤って作られている: %v", paths)
	}
}

// TestSyncIntegration_SYN08_SyncBlocksDeleteIndex は、sync_documents 処理中に同一 KEY への
// delete_index がブロックされることを検証する（最重要回帰テスト）。ブロックされずに割り込むと、
// 削除中の KEY へ sync が不整合データを書き込む・存在しない KEY への書き込みでエラーになる、
// のいずれかが再発する。
func TestSyncIntegration_SYN08_SyncBlocksDeleteIndex(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	seedUpsert(t, h, "K", "s", "keep.md", "# H\nkeep")
	entered, release := installGate(t, h)

	jobID := startGatedSync(t, h, entered, "K", "s", []UpsertDocument{
		{Path: "keep.md", Content: "# H\nkeep"},
		{Path: "gate.md", Content: "# H\n" + keyLockGateMarker + " body"},
	})

	// B: 同一 KEY への delete_index は sync 完了までブロックされる
	bDone := make(chan error, 1)
	go func() {
		_, _, err := h.handlers.handleDeleteIndex(ctx, nil, DeleteIndexInput{Key: "K"})
		bDone <- err
	}()
	select {
	case err := <-bDone:
		t.Fatalf("sync 処理中に delete_index が完了した（SYN-08 排他が効いていない）err=%v", err)
	case <-time.After(100 * time.Millisecond):
		// ブロックされている（期待どおり）
	}

	release()
	if out := waitSyncTerminal(t, h, jobID); out.Status != "done" {
		t.Fatalf("sync job status=%q, want done (errors=%v)", out.Status, out.Errors)
	}
	select {
	case err := <-bDone:
		if err != nil {
			t.Errorf("delete_index error: %v", err)
		}
	case <-time.After(keyLockITTimeout):
		t.Fatal("sync 完了後も delete_index が完了しない")
	}

	// 直列化の順序検証: sync（書き込み）→ delete_index の順で実行されたなら KEY ごと消えている
	exists, err := h.store.KeyExists(ctx, "K")
	if err != nil {
		t.Fatalf("KeyExists: %v", err)
	}
	if exists {
		t.Error("delete_index 完了後も KEY が存在する（sync の書き込みが delete に割り込んだ）")
	}
}

// -----------------------------------------------------------------------
// 項目 12: SYN-08 — sync 処理中の TTL/LRU 相当の KEY 削除のブロック
// -----------------------------------------------------------------------

// TestSyncIntegration_SYN08_SyncBlocksExpiryStyleDeleteKey は、sync_documents 処理中に
// 同一 KEY へ TTL/LRU 相当のロジック（internal/expiry の runTTL / runLRU が呼ぶ
// WithKeyLock(ctx, key, func() error { return DeleteKey(ctx, key) }) と同じ呼び出し）を
// 直接実行するとブロックされることを検証する（SYN-08、EXP-01/EXP-02 排他確認。
// 実装指示に従い internal/expiry は import せず、同等の呼び出しをテスト内で再現する）。
func TestSyncIntegration_SYN08_SyncBlocksExpiryStyleDeleteKey(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	seedUpsert(t, h, "K", "s", "keep.md", "# H\nkeep")
	entered, release := installGate(t, h)

	jobID := startGatedSync(t, h, entered, "K", "s", []UpsertDocument{
		{Path: "keep.md", Content: "# H\nkeep"},
		{Path: "gate.md", Content: "# H\n" + keyLockGateMarker + " body"},
	})

	// B: TTL/LRU 相当の KEY 削除（expiry.Worker.runTTL / runLRU と同じ形）は
	// sync 完了までブロックされる
	bDone := make(chan error, 1)
	go func() {
		bDone <- h.store.WithKeyLock(ctx, "K", func() error {
			return h.store.DeleteKey(ctx, "K")
		})
	}()
	select {
	case err := <-bDone:
		t.Fatalf("sync 処理中に TTL/LRU 相当の DeleteKey が完了した（SYN-08 排他が効いていない）err=%v", err)
	case <-time.After(100 * time.Millisecond):
		// ブロックされている（期待どおり）
	}

	release()
	if out := waitSyncTerminal(t, h, jobID); out.Status != "done" {
		t.Fatalf("sync job status=%q, want done (errors=%v)", out.Status, out.Errors)
	}
	select {
	case err := <-bDone:
		if err != nil {
			t.Errorf("TTL/LRU 相当の DeleteKey error: %v", err)
		}
	case <-time.After(keyLockITTimeout):
		t.Fatal("sync 完了後も TTL/LRU 相当の DeleteKey が完了しない")
	}

	// 直列化の順序検証: sync → DeleteKey の順なら KEY は消えている
	exists, err := h.store.KeyExists(ctx, "K")
	if err != nil {
		t.Fatalf("KeyExists: %v", err)
	}
	if exists {
		t.Error("DeleteKey 完了後も KEY が存在する（sync の書き込みが削除に割り込んだ）")
	}
}

// -----------------------------------------------------------------------
// 項目 13: GC-05 — root context cancel でジョブが failed になる
// -----------------------------------------------------------------------

// TestSyncDocuments_GC05_RootContextCanceledJobFails は、root context を cancel した状態
// （サーバーシャットダウン相当）で sync_documents を実行するとジョブが failed になることを
// 検証する（GC-05: 進行中のまま応答不能にならない）。
func TestSyncDocuments_GC05_RootContextCanceledJobFails(t *testing.T) {
	rootCtx, cancel := context.WithCancel(context.Background())
	h := newRootCtxHarness(t, rootCtx)

	// シャットダウン相当: rootCtx を cancel してから sync を投入する
	cancel()

	jobID := callSyncDocuments(t, h, context.Background(), "K", "s", []UpsertDocument{
		{Path: "a.md", Content: "# H\nalpha"},
	})

	out := waitSyncTerminal(t, h, jobID)
	if out.Status != "failed" {
		t.Errorf("status = %q, want failed（root context cancel 中のジョブ）", out.Status)
	}
	if len(out.Errors) == 0 {
		t.Error("failed の理由が errors に記録されていない（silent failure 禁止）")
	}
}

// -----------------------------------------------------------------------
// 項目 14: GC-05 — MCP リクエスト context 非依存の再発防止テスト
// -----------------------------------------------------------------------

// TestSyncDocuments_GC05_RequestContextCancelDoesNotAbortJob は、sync_documents が job_id を
// 返した直後に MCP リクエストの context を cancel しても（root context は生存させたまま）
// ジョブが done まで完走することを検証する。「request context を誤って goroutine に渡す」
// バグ（GC-05 で当初リスクとして指摘）の再発を直接検出する。
func TestSyncDocuments_GC05_RequestContextCancelDoesNotAbortJob(t *testing.T) {
	h := newHarness(t) // rootCtx = context.Background()（生存したまま）
	entered, release := installGate(t, h)

	// ゲートで停止するジョブを、cancel 可能な MCP リクエスト context で投入する
	reqCtx, cancelReq := context.WithCancel(context.Background())
	jobID := startGatedSync2(t, h, entered, reqCtx, "K", "s", []UpsertDocument{
		{Path: "gate.md", Content: "# H\n" + keyLockGateMarker + " body"},
	})

	// job_id 返却直後にリクエスト context を cancel する（クライアント切断相当）
	cancelReq()

	// cancel がジョブに波及していれば、ゲート解放前でも failed に遷移するはず。
	// 少し待ってから running のままであることを確認する。
	time.Sleep(50 * time.Millisecond)
	if got := getSyncStatus(t, h, jobID); got.Status != "running" {
		t.Fatalf("リクエスト context の cancel 後に status=%q（request context にジョブが依存している）", got.Status)
	}

	release()
	out := waitSyncTerminal(t, h, jobID)
	if out.Status != "done" || out.Processed != 1 {
		t.Errorf("完了状態 = %+v, want done/Processed=1（ジョブは完走すべき）", out)
	}
}

// startGatedSync2 は startGatedSync のリクエスト context 指定版（項目 14 用）。
func startGatedSync2(t *testing.T, h *testHarness, entered <-chan struct{}, reqCtx context.Context, key, series string, docs []UpsertDocument) string {
	t.Helper()
	jobID := callSyncDocuments(t, h, reqCtx, key, series, docs)
	select {
	case <-entered:
	case <-time.After(keyLockITTimeout):
		t.Fatal("sync ジョブがゲート（Embed）に到達しない")
	}
	return jobID
}

// -----------------------------------------------------------------------
// 外部レビュー対応（P2）: 空 documents を正当な desired-state として受理する
// -----------------------------------------------------------------------

// TestSyncDocuments_EmptyDesiredState は、documents の空リストが「この series に現存
// ファイルがない」という正当な desired-state として受理されることを検証する（SYN-01）。
// 空リスト sync で既存 path が全て series から切り離されて orphan 予約が作られ、
// その後同一内容で再 sync すると API 課金ゼロ（Embedder 呼び出し 0 回）で復活・
// 予約解除される自己修復の一連を確認する。空リスト拒否の実装への退行を検出する。
func TestSyncDocuments_EmptyDesiredState(t *testing.T) {
	h := newHarness(t)

	syncAndWaitDone(t, h, "K", "s", []UpsertDocument{
		{Path: "a.md", Content: "# H\nalpha"},
		{Path: "b.md", Content: "# H\nbeta"},
	})

	// 空 documents で再 sync → 拒否されず done になり、既存 2 path が全て切り離される
	out := syncAndWaitDone(t, h, "K", "s", []UpsertDocument{})
	if out.Processed != 0 || out.Skipped != 0 || out.Failed != 0 || out.DeletedPathsMarked != 2 {
		t.Errorf("空 sync = %+v, want DeletedPathsMarked=2 その他 0", out)
	}

	// series 指定の検索に両 path とも現れない
	paths := chunkPathSet(t, h, "K", "s")
	if paths["a.md"] || paths["b.md"] {
		t.Errorf("空 sync 後も series 指定検索に path が残っている: %v", paths)
	}

	// orphan 予約が両 path に作られている（series 全体予約ではない）
	pPaths, seriesWide := pendingFor(t, h, "K", "s")
	pSet := make(map[string]bool, len(pPaths))
	for _, p := range pPaths {
		pSet[p] = true
	}
	if len(pPaths) != 2 || !pSet["a.md"] || !pSet["b.md"] {
		t.Errorf("pending paths = %v, want [a.md b.md]", pPaths)
	}
	if seriesWide {
		t.Error("series 全体予約が誤って作られている")
	}

	// 同一内容で再 sync → Embedder 呼び出しゼロのまま両 path が復活し予約が解除される（SYN-04）
	spy := &spyEmbedder{inner: h.embedder}
	h.handlers.embedder = spy
	out = syncAndWaitDone(t, h, "K", "s", []UpsertDocument{
		{Path: "a.md", Content: "# H\nalpha"},
		{Path: "b.md", Content: "# H\nbeta"},
	})
	if calls := atomic.LoadInt32(&spy.calls); calls != 0 {
		t.Errorf("自己修復で Embedder が %d 回呼ばれた（API 課金ゼロであるべき）", calls)
	}
	if out.Skipped != 2 || out.Processed != 0 || out.Failed != 0 {
		t.Errorf("復活 sync = %+v, want Skipped=2", out)
	}
	paths = chunkPathSet(t, h, "K", "s")
	if !paths["a.md"] || !paths["b.md"] {
		t.Errorf("自己修復後も series 指定検索に path が現れない: %v", paths)
	}
	if pPaths, _ := pendingFor(t, h, "K", "s"); len(pPaths) != 0 {
		t.Errorf("自己修復後も削除予約が残っている: %v", pPaths)
	}
}

// -----------------------------------------------------------------------
// 外部レビュー対応（P1）: handleDelete の存在チェックを WithKeyLock 内に移動（TOCTOU）
// -----------------------------------------------------------------------

// TestDeleteDocuments_BlocksOnSyncCreatingPath は TOCTOU 回帰テスト:
// sync_documents が新規 path "late.md" を処理中（WithKeyLock 保持中）に同一 KEY へ
// handleDelete(late.md) を呼ぶと、sync 完了までブロックされた後に Deleted=1（warning
// なし）で完了することを検証する。旧実装は HasRecord 存在チェックがロック外にあった
// ため、sync が作成途中の path を「存在しない」と誤判定し、ブロックされずに warning +
// Deleted=0 で即完了して削除要求を取りこぼしていた。
func TestDeleteDocuments_BlocksOnSyncCreatingPath(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	seedUpsert(t, h, "K", "s", "keep.md", "# H\nkeep")
	entered, release := installGate(t, h)

	// A: sync ジョブが新規 path late.md の Embed でゲート停止する
	//（= late.md の record 作成前・WithKeyLock 保持中の状態）
	jobID := startGatedSync(t, h, entered, "K", "s", []UpsertDocument{
		{Path: "keep.md", Content: "# H\nkeep"},
		{Path: "late.md", Content: "# H\n" + keyLockGateMarker + " late body"},
	})

	// B: 同一 KEY への delete_documents(late.md) は sync 完了までブロックされる
	type deleteOutcome struct {
		out DeleteResult
		err error
	}
	bDone := make(chan deleteOutcome, 1)
	go func() {
		_, out, err := h.handlers.handleDelete(ctx, nil, DeleteInput{
			Key: "K", Series: "s", Paths: []string{"late.md"},
		})
		bDone <- deleteOutcome{out: out, err: err}
	}()
	// 「完了しないこと」の観測: 短い timer との select で行う。timer 側に落ちれば
	// ブロック中と判断する（旧実装ならロック外チェックが即座に warning + Deleted=0 を
	// 返すためここで fail する。false negative は release 後の Deleted=1 検証で補完）。
	select {
	case b := <-bDone:
		t.Fatalf("sync 処理中に delete_documents が完了した（ロック外チェックの TOCTOU 再発）: out=%+v err=%v", b.out, b.err)
	case <-time.After(100 * time.Millisecond):
		// ブロックされている（期待どおり）
	}

	// ゲート解放 → sync 完了 → delete が Deleted=1（warning なし）で完了する
	release()
	if out := waitSyncTerminal(t, h, jobID); out.Status != "done" {
		t.Fatalf("sync job status=%q, want done (errors=%v)", out.Status, out.Errors)
	}
	select {
	case b := <-bDone:
		if b.err != nil {
			t.Errorf("delete_documents error: %v", b.err)
		}
		if b.out.Deleted != 1 || len(b.out.Warnings) != 0 {
			t.Errorf("delete_documents out = %+v, want Deleted=1 Warnings なし（存在チェックが sync の書き込み後に実行されるべき）", b.out)
		}
	case <-time.After(keyLockITTimeout):
		t.Fatal("sync 完了後も delete_documents が完了しない")
	}

	// 直列化の帰結: sync が作成した late.md は delete により series から消え、keep.md は残る
	paths := chunkPathSet(t, h, "K", "s")
	if paths["late.md"] {
		t.Error("delete 完了後も late.md が series 指定検索に残っている（削除要求の取りこぼし）")
	}
	if !paths["keep.md"] {
		t.Error("無関係な keep.md まで series 指定検索から消えた")
	}
}
