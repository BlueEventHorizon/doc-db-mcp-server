package mcp

// TASK-018 — upsert_documents 統合テスト (DES-001 §11)
//
// 個別経路は mcp_test.go の各単体テストで確認済みのため、本ファイルでは
// 複数経路が同時に動く「結合シナリオ」を中心に検証する:
//   - 1 呼び出し内で 同一ハッシュ skip / 新規 / URL fetch / fetch 失敗 が混在する
//   - DIF-02 経路の Embedder 抑制（同ハッシュ既存時に Embedder が呼ばれないこと）
//   - DIF-03 経路で旧 record の series が新 record に切り替わること

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/BlueEventHorizon/doc-db-mcp-server/internal/embedder"
)

// spyEmbedder は内部 Embedder の呼び出し回数を観測する。
type spyEmbedder struct {
	calls int32
	inner embedder.Embedder
}

func (s *spyEmbedder) Embed(ctx context.Context, texts []string) ([]embedder.Vector, []int, error) {
	atomic.AddInt32(&s.calls, 1)
	return s.inner.Embed(ctx, texts)
}

// -----------------------------------------------------------------------
// 結合シナリオ
// -----------------------------------------------------------------------

func TestUpsertIntegration_MixedScenarios(t *testing.T) {
	h := newHarness(t)
	h.fetcher.contents = map[string]string{
		"http://example.com/ok.md": "# H\nremote ok",
	}
	h.fetcher.errs = map[string]error{
		"http://example.com/bad": errors.New("connection refused"),
	}
	ctx := context.Background()

	// 1 回目: a (content), c (url)
	if _, out, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
		Key: "K", Series: "s1",
		Documents: []UpsertDocument{
			{Path: "a.md", Content: "# H\nalpha"},
			{Path: "c.md", URL: "http://example.com/ok.md"},
		},
	}); err != nil || out.Processed != 2 {
		t.Fatalf("warmup: %+v err=%v", out, err)
	}

	// 2 回目: 同一内容 a（DIF-02 skip）/ 同一内容 c（DIF-02 skip）/ 新規 b / fetch 失敗 d
	_, out, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
		Key: "K", Series: "s2",
		Documents: []UpsertDocument{
			{Path: "a.md", Content: "# H\nalpha"},           // skip
			{Path: "c.md", URL: "http://example.com/ok.md"}, // skip (same hash from fetch)
			{Path: "b.md", Content: "# H\nbeta new"},        // processed
			{Path: "d.md", URL: "http://example.com/bad"},   // failed
		},
	})
	if err != nil {
		t.Fatalf("upsert err: %v", err)
	}
	if out.Processed != 1 || out.Skipped != 2 || out.Failed != 1 {
		t.Errorf("counters: %+v, want Processed=1 Skipped=2 Failed=1", out)
	}
	if len(out.Errors) != 1 || out.Errors[0].Path != "d.md" {
		t.Errorf("errors should single out d.md fetch failure: %+v", out.Errors)
	}

	// a.md / c.md の record は s1+s2 両方に紐づく
	keys, err := h.store.ListKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("len(keys)=%d, want 1", len(keys))
	}
	if len(keys[0].Series) != 2 {
		t.Errorf("series=%v, want both s1 and s2", keys[0].Series)
	}
}

// DIF-02 経路で同一ハッシュ既存時、Embedder が新規呼び出しされないことを確認する。
func TestUpsertIntegration_DIF02_DoesNotCallEmbedder(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// 1 回目で content "v1" を入れる
	doc := UpsertDocument{Path: "p", Content: "# H\nv1"}
	if _, _, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
		Key: "K", Series: "s1", Documents: []UpsertDocument{doc},
	}); err != nil {
		t.Fatal(err)
	}

	// Embedder を観測用にラップして差し替える
	spy := &spyEmbedder{inner: h.embedder}
	h.handlers.embedder = spy

	// DIF-02 経路: 同一内容なら Embedder 不要
	if _, out, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
		Key: "K", Series: "s2", Documents: []UpsertDocument{doc},
	}); err != nil || out.Skipped != 1 {
		t.Fatalf("expected skip: %+v err=%v", out, err)
	}
	if atomic.LoadInt32(&spy.calls) != 0 {
		t.Errorf("Embedder should not be called on DIF-02 path; calls=%d", spy.calls)
	}
}

// DIF-03 経路で旧 record の series が新 record に切り替わることを確認する。
func TestUpsertIntegration_DIF03_SeriesMigratesToNewRecord(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// 旧 record（v1）を series=s1 で投入
	if _, _, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
		Key: "K", Series: "s1",
		Documents: []UpsertDocument{{Path: "p", Content: "# H\nv1"}},
	}); err != nil {
		t.Fatal(err)
	}

	// 同 path 同 series で内容変更 → DIF-03
	if _, out, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
		Key: "K", Series: "s1",
		Documents: []UpsertDocument{{Path: "p", Content: "# H\nv2 — completely different"}},
	}); err != nil || out.Processed != 1 {
		t.Fatalf("DIF-03: %+v err=%v", out, err)
	}

	// 新 record（v2）のチャンクのみが series=s1 から見える
	chunks, err := h.store.GetChunksForSearch(ctx, "K", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) == 0 {
		t.Fatal("no chunks visible from series s1")
	}
	for _, c := range chunks {
		if strings.Contains(c.Text, "v1") && !strings.Contains(c.Text, "v2") {
			t.Errorf("series s1 still references old record text: %q", c.Text)
		}
	}
}
