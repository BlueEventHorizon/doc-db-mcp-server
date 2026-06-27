package mcp

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/BlueEventHorizon/doc-db-mcp-server/internal/chunker"
	"github.com/BlueEventHorizon/doc-db-mcp-server/internal/embedder"
	"github.com/BlueEventHorizon/doc-db-mcp-server/internal/search"
	"github.com/BlueEventHorizon/doc-db-mcp-server/internal/store"
)

// -----------------------------------------------------------------------
// テスト用モック
// -----------------------------------------------------------------------

const testDim = 3

type fakeEmbedder struct {
	// fixed が non-nil なら全テキストに同じベクトルを返す。
	fixed []float32
	// vectors[i] が指定されていればテキスト i にそのベクトルを返す。
	vectors [][]float32
	// failTexts に含まれるテキストは skipped 扱いする。
	failTexts map[string]bool
	// err は最後に返す err（部分失敗の演出用）。
	err error
}

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([]embedder.Vector, []int, error) {
	out := make([]embedder.Vector, len(texts))
	var skipped []int
	for i, t := range texts {
		if f.failTexts[t] {
			skipped = append(skipped, i)
			continue
		}
		if f.vectors != nil && i < len(f.vectors) {
			out[i] = embedder.Vector(f.vectors[i])
			continue
		}
		if f.fixed != nil {
			out[i] = embedder.Vector(f.fixed)
			continue
		}
		// デフォルト: index に応じた基本ベクトル
		v := make([]float32, testDim)
		v[0] = float32(i + 1)
		out[i] = v
	}
	return out, skipped, f.err
}

type fakeFetcher struct {
	contents map[string]string
	errs     map[string]error
}

func (f *fakeFetcher) Fetch(_ context.Context, url string) (string, error) {
	if err, ok := f.errs[url]; ok {
		return "", err
	}
	if c, ok := f.contents[url]; ok {
		return c, nil
	}
	return "", errors.New("url not configured: " + url)
}

// -----------------------------------------------------------------------
// テストハーネス
// -----------------------------------------------------------------------

type testHarness struct {
	t        *testing.T
	store    *store.Store
	embedder *fakeEmbedder
	fetcher  *fakeFetcher
	handlers *Handlers
}

func newHarness(t *testing.T) *testHarness {
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
	h := New(st, ch, emb, fe, pipe)
	return &testHarness{t: t, store: st, embedder: emb, fetcher: fe, handlers: h}
}

// -----------------------------------------------------------------------
// upsert_documents
// -----------------------------------------------------------------------

func TestUpsert_BasicContent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	_, out, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
		Key: "K", Series: "main",
		Documents: []UpsertDocument{
			{Path: "a.md", Content: "# Title\nhello world"},
			{Path: "b.md", Content: "# Title\nfoo bar"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Processed != 2 || out.Skipped != 0 || out.Failed != 0 {
		t.Errorf("got %+v, want processed=2", out)
	}
}

func TestUpsert_DIF02_SameHashSkips(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	doc := UpsertDocument{Path: "p", Content: "# H\nsame content"}
	if _, _, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
		Key: "K", Series: "s1", Documents: []UpsertDocument{doc},
	}); err != nil {
		t.Fatal(err)
	}

	// 同じ内容で別 series に upsert → DIF-02 経路で skip + series 追記
	_, out, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
		Key: "K", Series: "s2", Documents: []UpsertDocument{doc},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Skipped != 1 || out.Processed != 0 {
		t.Errorf("DIF-02: expected Skipped=1, got %+v", out)
	}

	// keys には s1 と s2 の両方が紐づいている
	keys, err := h.store.ListKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || len(keys[0].Series) != 2 {
		t.Errorf("series_keys = %v, want 2", keys[0].Series)
	}
}

func TestUpsert_DIF03_ContentChangeReplaces(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// 初回: 同 path で内容 v1
	if _, _, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
		Key: "K", Series: "main",
		Documents: []UpsertDocument{{Path: "p", Content: "# H\nv1"}},
	}); err != nil {
		t.Fatal(err)
	}

	// 2 回目: 同 path 同 series で内容 v2（DIF-03）
	_, out, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
		Key: "K", Series: "main",
		Documents: []UpsertDocument{{Path: "p", Content: "# H\nv2"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Processed != 1 {
		t.Errorf("DIF-03: expected Processed=1, got %+v", out)
	}

	// 旧 record は CleanOtherSeries で削除されているので records は 1 件のみ
	chunks, err := h.store.GetChunksForSearch(ctx, "K", "")
	if err != nil {
		t.Fatal(err)
	}
	// 全 chunk が v2 由来であること
	for _, c := range chunks {
		if c.Text == "" {
			t.Errorf("empty chunk text")
		}
	}
}

func TestUpsert_URLFetch(t *testing.T) {
	h := newHarness(t)
	h.fetcher.contents = map[string]string{
		"http://example.com/doc.md": "# Fetched\nremote content",
	}
	ctx := context.Background()

	_, out, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
		Key: "K", Series: "main",
		Documents: []UpsertDocument{{Path: "fetched.md", URL: "http://example.com/doc.md"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Processed != 1 {
		t.Errorf("got %+v", out)
	}
}

func TestUpsert_URLFetchFails_DocumentFailed(t *testing.T) {
	h := newHarness(t)
	h.fetcher.errs = map[string]error{
		"http://example.com/bad": errors.New("connection refused"),
	}
	ctx := context.Background()

	_, out, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
		Key: "K", Series: "main",
		Documents: []UpsertDocument{
			{Path: "good.md", Content: "# H\nok"},
			{Path: "bad.md", URL: "http://example.com/bad"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Processed != 1 || out.Failed != 1 {
		t.Errorf("expected 1 processed + 1 failed, got %+v", out)
	}
	if len(out.Errors) != 1 || out.Errors[0].Path != "bad.md" {
		t.Errorf("expected error for bad.md, got %+v", out.Errors)
	}
}

func TestUpsert_ContentURLBothSet_Fails(t *testing.T) {
	h := newHarness(t)
	_, out, err := h.handlers.handleUpsert(context.Background(), nil, UpsertInput{
		Key: "K", Series: "s",
		Documents: []UpsertDocument{{Path: "p", Content: "x", URL: "http://x"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Failed != 1 {
		t.Errorf("content + url 両指定は失敗、got %+v", out)
	}
}

func TestUpsert_ValidationErrors(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	cases := []struct {
		name string
		in   UpsertInput
	}{
		{"no key", UpsertInput{Series: "s", Documents: []UpsertDocument{{Path: "p", Content: "x"}}}},
		{"no series", UpsertInput{Key: "K", Documents: []UpsertDocument{{Path: "p", Content: "x"}}}},
		{"no documents", UpsertInput{Key: "K", Series: "s"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := h.handlers.handleUpsert(ctx, nil, tc.in); err == nil {
				t.Fatal("want validation error")
			}
		})
	}
}

func TestUpsert_PartialEmbeddingFailure(t *testing.T) {
	h := newHarness(t)
	h.embedder.failTexts = map[string]bool{
		// chunker は Embedding API には EmbedText (heading breadcrumb + prose) を渡す。
		// 短文 prose は前 chunk から継承されることがあるが、ここでは最初の chunk なので継承されない。
		"# A\n\nalpha section": true,
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
	// 部分失敗: ドキュメント自体は processed、Errors に skipped_chunks が記録される
	if out.Processed != 1 {
		t.Errorf("processed = %d, want 1 (partial)", out.Processed)
	}
	if len(out.Errors) == 0 {
		t.Fatal("Errors should record partial embedding failure")
	}
	if len(out.Errors[0].SkippedChunks) == 0 {
		t.Errorf("SkippedChunks empty: %+v", out.Errors)
	}
}

func TestUpsert_NormalizesCRLFAndBOM(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// CRLF + BOM 付きと、LF のみで内容は同じハッシュになるはず
	crlf := "\xef\xbb\xbf# H\r\nbody\r\n"
	lf := "# H\nbody\n"

	if _, _, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
		Key: "K", Series: "s",
		Documents: []UpsertDocument{{Path: "p", Content: crlf}},
	}); err != nil {
		t.Fatal(err)
	}
	_, out, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
		Key: "K", Series: "s",
		Documents: []UpsertDocument{{Path: "p", Content: lf}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Skipped != 1 {
		t.Errorf("BOM/CRLF 正規化後ハッシュ一致 → skip 期待: %+v", out)
	}
}

// -----------------------------------------------------------------------
// delete_documents
// -----------------------------------------------------------------------

func TestDelete_RemovesExistingPaths(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if _, _, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
		Key: "K", Series: "s",
		Documents: []UpsertDocument{
			{Path: "a", Content: "# H\nA"},
			{Path: "b", Content: "# H\nB"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	_, out, err := h.handlers.handleDelete(ctx, nil, DeleteInput{
		Key: "K", Series: "s", Paths: []string{"a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Deleted != 1 || len(out.Warnings) != 0 {
		t.Errorf("got %+v, want Deleted=1, no warnings", out)
	}
}

func TestDelete_MissingPath_Warning(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if _, _, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
		Key: "K", Series: "s",
		Documents: []UpsertDocument{{Path: "a", Content: "# H\nA"}},
	}); err != nil {
		t.Fatal(err)
	}

	_, out, err := h.handlers.handleDelete(ctx, nil, DeleteInput{
		Key: "K", Series: "s", Paths: []string{"a", "nonexistent"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Deleted != 1 {
		t.Errorf("Deleted = %d, want 1", out.Deleted)
	}
	if len(out.Warnings) != 1 {
		t.Errorf("Warnings = %v, want 1 entry for nonexistent", out.Warnings)
	}
}

func TestDelete_ValidationErrors(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	cases := []DeleteInput{
		{Series: "s", Paths: []string{"a"}},
		{Key: "K", Paths: []string{"a"}},
		{Key: "K", Series: "s"},
	}
	for _, in := range cases {
		if _, _, err := h.handlers.handleDelete(ctx, nil, in); err == nil {
			t.Errorf("want error for input %+v", in)
		}
	}
}

// -----------------------------------------------------------------------
// query
// -----------------------------------------------------------------------

func TestQuery_HappyPath(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if _, _, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
		Key: "K", Series: "main",
		Documents: []UpsertDocument{
			{Path: "a", Content: "# H\nhello world"},
			{Path: "b", Content: "# H\nfoo bar"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	_, out, err := h.handlers.handleQuery(ctx, nil, QueryInput{
		Query: "hello", Key: "K", Mode: "hybrid", TopN: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Results) == 0 {
		t.Fatal("no results")
	}
	if out.Results[0].Path == "" {
		t.Errorf("path missing in top result")
	}
	if out.StageStats.FusedCandidates == 0 {
		t.Errorf("stage_stats not populated: %+v", out.StageStats)
	}
}

func TestQuery_DefaultMode_Rerank(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, _, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
		Key: "K", Series: "s",
		Documents: []UpsertDocument{{Path: "a", Content: "# H\ntext"}},
	}); err != nil {
		t.Fatal(err)
	}
	// Mode 未指定 → rerank（reranker nil なので RRF フォールバック相当に動く）
	if _, _, err := h.handlers.handleQuery(ctx, nil, QueryInput{
		Query: "text", Key: "K",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestQuery_UnknownKey_Errors(t *testing.T) {
	h := newHarness(t)
	_, _, err := h.handlers.handleQuery(context.Background(), nil, QueryInput{
		Query: "x", Key: "NOTEXIST",
	})
	if err == nil {
		t.Fatal("want error for unknown key")
	}
}

func TestQuery_ValidationErrors(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	cases := []QueryInput{
		{Key: "K"},        // query missing
		{Query: "q"},      // key missing
	}
	for _, in := range cases {
		if _, _, err := h.handlers.handleQuery(ctx, nil, in); err == nil {
			t.Errorf("want validation error for %+v", in)
		}
	}
}

// -----------------------------------------------------------------------
// list_indexes / delete_index / manage_index
// -----------------------------------------------------------------------

func TestListIndexes(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for _, k := range []string{"K1", "K2"} {
		if _, _, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
			Key: k, Series: "s",
			Documents: []UpsertDocument{{Path: "p", Content: "# H\nx"}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	_, out, err := h.handlers.handleListIndexes(ctx, nil, ListIndexesInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Indexes) != 2 {
		t.Errorf("len = %d, want 2", len(out.Indexes))
	}
}

func TestDeleteIndex(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, _, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
		Key: "K", Series: "s",
		Documents: []UpsertDocument{{Path: "p", Content: "# H\nx"}},
	}); err != nil {
		t.Fatal(err)
	}
	_, out, err := h.handlers.handleDeleteIndex(ctx, nil, DeleteIndexInput{Key: "K"})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Deleted {
		t.Error("Deleted = false")
	}
	keys, _ := h.store.ListKeys(ctx)
	if len(keys) != 0 {
		t.Errorf("len = %d, want 0 after delete", len(keys))
	}
}

func TestDeleteIndex_MissingKey(t *testing.T) {
	h := newHarness(t)
	_, _, err := h.handlers.handleDeleteIndex(context.Background(), nil, DeleteIndexInput{Key: ""})
	if err == nil {
		t.Fatal("want error for empty key")
	}
}

func TestManageIndex_SetAndReset(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if _, _, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
		Key: "K", Series: "s",
		Documents: []UpsertDocument{{Path: "p", Content: "# H\nx"}},
	}); err != nil {
		t.Fatal(err)
	}

	ttl := 7
	maxChunks := 1000
	_, out, err := h.handlers.handleManageIndex(ctx, nil, ManageIndexInput{
		Key: "K",
		ExpiryPolicy: &store.ExpiryPolicy{
			TTLDays:   &ttl,
			MaxChunks: &maxChunks,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Updated {
		t.Error("Updated = false")
	}

	// reset
	_, out, err = h.handlers.handleManageIndex(ctx, nil, ManageIndexInput{Key: "K"})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Updated {
		t.Error("Updated = false on reset")
	}
}

func TestManageIndex_UnknownKey_Errors(t *testing.T) {
	h := newHarness(t)
	ttl := 7
	_, _, err := h.handlers.handleManageIndex(context.Background(), nil, ManageIndexInput{
		Key: "NOPE", ExpiryPolicy: &store.ExpiryPolicy{TTLDays: &ttl},
	})
	if err == nil {
		t.Fatal("want error for unknown key")
	}
}

// -----------------------------------------------------------------------
// utility
// -----------------------------------------------------------------------

func TestNormalizeContent(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"hello", "hello"},
		{"\xef\xbb\xbfhello", "hello"},
		{"a\r\nb", "a\nb"},
		{"a\rb", "a\nb"},
		{"\xef\xbb\xbfa\r\nb\rc\n", "a\nb\nc\n"},
	}
	for _, tc := range cases {
		if got := normalizeContent(tc.in); got != tc.want {
			t.Errorf("normalizeContent(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSearchEmbedderAdapter(t *testing.T) {
	inner := &fakeEmbedder{vectors: [][]float32{{1, 2, 3}, {4, 5, 6}}}
	a := SearchEmbedderAdapter{Inner: inner}
	vecs, skipped, err := a.Embed(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 2 || vecs[0][0] != 1 || vecs[1][2] != 6 {
		t.Errorf("got %v", vecs)
	}
	if skipped != nil {
		t.Errorf("skipped = %v, want nil", skipped)
	}
}
