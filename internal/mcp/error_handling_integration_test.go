package mcp

// TASK-022 — エラーハンドリング検証 (DES-001 §11)
//
// 既存テストでカバー済みの項目（Embedder 部分失敗、URL fetch 失敗、Rerank フォールバック等）
// 以外の境界・エラー経路を追加で検証する:
//   - Fetcher が context.DeadlineExceeded を返した場合の failed カウント・errors 内訳
//   - Embedder 部分失敗時に「成功チャンク」が実際にストアへ保存されていること（M2）
//   - 全チャンク Embedding 失敗時の挙動（Processed=1, ベクトル無しで保存）

import (
	"context"
	"strings"
	"testing"
)

// Fetcher が DeadlineExceeded を返した場合、該当ドキュメントが Failed に計上され、
// 他のドキュメントは正常に処理される。
func TestErrorIntegration_FetcherTimeout(t *testing.T) {
	h := newHarness(t)
	h.fetcher.errs = map[string]error{
		"http://example.com/slow": context.DeadlineExceeded,
	}
	h.fetcher.contents = map[string]string{
		"http://example.com/ok": "# H\nok body",
	}
	ctx := context.Background()

	_, out, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
		Key: "K", Series: "s",
		Documents: []UpsertDocument{
			{Path: "ok.md", URL: "http://example.com/ok"},
			{Path: "slow.md", URL: "http://example.com/slow"},
		},
	})
	if err != nil {
		t.Fatalf("handleUpsert: %v", err)
	}
	if out.Processed != 1 || out.Failed != 1 {
		t.Errorf("counters: %+v, want Processed=1 Failed=1", out)
	}
	if len(out.Errors) != 1 || out.Errors[0].Path != "slow.md" {
		t.Errorf("errors should single out slow.md: %+v", out.Errors)
	}
	if !strings.Contains(out.Errors[0].Error, "fetch") {
		t.Errorf("error message should mention fetch: %q", out.Errors[0].Error)
	}
}

// M2: Embedder 部分失敗時に「成功チャンク」だけが ベクトル付きでストアへ保存される。
// 失敗チャンクはテキストのみ保存（vector 無し）で残り、語彙検索のみ対象になる。
func TestErrorIntegration_EmbedderPartialFailure_SuccessChunksStored(t *testing.T) {
	h := newHarness(t)
	// "# A\nalpha section" を失敗、"# B\nbeta section" を成功とする
	// Embedder には EmbedText (heading breadcrumb + prose) が渡されるため、
	// failTexts のキーもその形式に合わせる。
	h.embedder.failTexts = map[string]bool{
		"# A\n\nalpha section": true,
	}
	ctx := context.Background()

	md := "# A\nalpha section\n# B\nbeta section\n"
	if _, out, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
		Key: "K", Series: "s",
		Documents: []UpsertDocument{{Path: "p", Content: md}},
	}); err != nil || out.Processed != 1 {
		t.Fatalf("upsert: %+v err=%v", out, err)
	}

	// チャンク一覧を取得し、両方のテキストが保存されていることを確認
	chunks, err := h.store.GetChunksForSearch(ctx, "K", "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2 (both stored, one without vector)", len(chunks))
	}

	var alphaHasVec, betaHasVec bool
	for _, c := range chunks {
		hasVec := len(c.Vector) > 0
		switch {
		case strings.Contains(c.Text, "alpha"):
			alphaHasVec = hasVec
		case strings.Contains(c.Text, "beta"):
			betaHasVec = hasVec
		}
	}
	if alphaHasVec {
		t.Errorf("alpha chunk should NOT have vector (Embedder failed for it)")
	}
	if !betaHasVec {
		t.Errorf("beta chunk SHOULD have vector (Embedder succeeded)")
	}
}

// 全チャンク Embedding 失敗時: Processed=1（テキストは保存）+ errors に skipped_chunks 全件。
func TestErrorIntegration_EmbedderAllFail_TextStillStored(t *testing.T) {
	h := newHarness(t)
	// EmbedText 形式 (heading breadcrumb + "\n\n" + prose) で fail 対象を指定。
	// alpha は最初の chunk なので prose 継承無し、beta は前 chunk の prose も短いので継承無し。
	h.embedder.failTexts = map[string]bool{
		"# A\n\nalpha section": true,
		"# B\n\nbeta section":  true,
	}
	ctx := context.Background()

	md := "# A\nalpha section\n# B\nbeta section\n"
	_, out, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
		Key: "K", Series: "s",
		Documents: []UpsertDocument{{Path: "p", Content: md}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Processed != 1 {
		t.Errorf("Processed = %d, want 1 (text stored even when all chunks fail embedding)", out.Processed)
	}
	if len(out.Errors) == 0 || len(out.Errors[0].SkippedChunks) != 2 {
		t.Errorf("expected SkippedChunks to cover all chunks: %+v", out.Errors)
	}

	chunks, err := h.store.GetChunksForSearch(ctx, "K", "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 {
		t.Errorf("text chunks should remain stored: got %d", len(chunks))
	}
	for _, c := range chunks {
		if len(c.Vector) != 0 {
			t.Errorf("vector should be empty for failed-embedding chunk: %v", c.Vector)
		}
	}
}
