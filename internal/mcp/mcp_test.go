package mcp

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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

// testTrashRetentionDays はテストで使う trash.retention_days の固定値
// (list_trashed_indexes の remaining_seconds 算出テストで参照する)。
const testTrashRetentionDays = 3

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
	h := New(context.Background(), st, ch, emb, fe, pipe, testTrashRetentionDays)
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

// TestUpsert_DIF03_DiagnosticLogDistinguishesReason は、DIF-03 経路の診断ログが
// 「純新規 path」と「同一 path の内容変更 (hash 不一致)」を区別して出力することを
// 検証する（Issue #4: re-embedding の原因切り分け用）。
func TestUpsert_DIF03_DiagnosticLogDistinguishesReason(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// 1 回目: 純新規 path → "embedding new path"
	if _, _, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
		Key: "K", Series: "main",
		Documents: []UpsertDocument{{Path: "p", Content: "# H\nv1"}},
	}); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); !strings.Contains(got, "embedding new path") {
		t.Errorf("新規 path のログに 'embedding new path' が含まれない:\n%s", got)
	}

	// 2 回目: 同一 path で内容変更 → "re-embedding (content changed"
	buf.Reset()
	if _, _, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
		Key: "K", Series: "main",
		Documents: []UpsertDocument{{Path: "p", Content: "# H\nv2"}},
	}); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); !strings.Contains(got, "re-embedding (content changed") {
		t.Errorf("内容変更のログに 're-embedding (content changed' が含まれない:\n%s", got)
	}

	// 3 回目: 同一内容の別 series（DIF-02 skip）→ DIF-03 診断ログは出ない
	buf.Reset()
	if _, _, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
		Key: "K", Series: "s2",
		Documents: []UpsertDocument{{Path: "p", Content: "# H\nv2"}},
	}); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); strings.Contains(got, "embedding new path") || strings.Contains(got, "re-embedding") {
		t.Errorf("DIF-02 skip なのに DIF-03 診断ログが出力された:\n%s", got)
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

// -----------------------------------------------------------------------
// local_path (v0.1.8+): サーバー側でローカルファイルを読む経路
// -----------------------------------------------------------------------

func TestUpsert_LocalPath_ReadsFile(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "sample.md")
	if err := os.WriteFile(tmp, []byte("# H\nローカルファイルから読んだ内容"), 0644); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	_, out, err := h.handlers.handleUpsert(context.Background(), nil, UpsertInput{
		Key: "K", Series: "s",
		Documents: []UpsertDocument{{Path: "sample.md", LocalPath: tmp}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Processed != 1 {
		t.Fatalf("expected processed=1 via local_path, got %+v", out)
	}
	// 保存されたチャンクに実際の本文が含まれる
	chunks, err := h.store.GetChunksForSearch(context.Background(), "K", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) == 0 || !strings.Contains(chunks[0].Text, "ローカルファイルから") {
		t.Errorf("chunks should contain local file content: %v", chunks)
	}
}

func TestUpsert_LocalPath_RelativePathRejected(t *testing.T) {
	h := newHarness(t)
	_, out, err := h.handlers.handleUpsert(context.Background(), nil, UpsertInput{
		Key: "K", Series: "s",
		Documents: []UpsertDocument{{Path: "p", LocalPath: "docs/api.md"}}, // 相対パス
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Failed != 1 {
		t.Errorf("relative path should be rejected, got %+v", out)
	}
}

func TestUpsert_LocalPath_TraversalRejected(t *testing.T) {
	h := newHarness(t)
	_, out, err := h.handlers.handleUpsert(context.Background(), nil, UpsertInput{
		Key: "K", Series: "s",
		Documents: []UpsertDocument{{Path: "p", LocalPath: "/etc/../etc/passwd"}}, // .. 含む
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Failed != 1 {
		t.Errorf("path traversal should be rejected, got %+v", out)
	}
}

func TestUpsert_LocalPath_NotFound(t *testing.T) {
	h := newHarness(t)
	_, out, err := h.handlers.handleUpsert(context.Background(), nil, UpsertInput{
		Key: "K", Series: "s",
		Documents: []UpsertDocument{{Path: "p", LocalPath: "/tmp/nonexistent-doc-db-test-xxx.md"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Failed != 1 {
		t.Errorf("missing file should fail, got %+v", out)
	}
}

func TestUpsert_ThreeSourcesMutuallyExclusive(t *testing.T) {
	h := newHarness(t)
	// content + local_path 同時指定
	_, out, _ := h.handlers.handleUpsert(context.Background(), nil, UpsertInput{
		Key: "K", Series: "s",
		Documents: []UpsertDocument{{Path: "p", Content: "x", LocalPath: "/tmp/x.md"}},
	})
	if out.Failed != 1 {
		t.Errorf("content + local_path 併用は failed 扱いとなるべき, got %+v", out)
	}
	// url + local_path 同時指定
	_, out2, _ := h.handlers.handleUpsert(context.Background(), nil, UpsertInput{
		Key: "K", Series: "s",
		Documents: []UpsertDocument{{Path: "p", URL: "http://x", LocalPath: "/tmp/x.md"}},
	})
	if out2.Failed != 1 {
		t.Errorf("url + local_path 併用は failed 扱いとなるべき, got %+v", out2)
	}
	// 3 つ全部指定
	_, out3, _ := h.handlers.handleUpsert(context.Background(), nil, UpsertInput{
		Key: "K", Series: "s",
		Documents: []UpsertDocument{{Path: "p", Content: "x", URL: "http://x", LocalPath: "/tmp/x.md"}},
	})
	if out3.Failed != 1 {
		t.Errorf("三重指定は failed 扱いとなるべき, got %+v", out3)
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
		// v0.1.3: heading_path から `#` プレフィックスを除去したので、形式は "A\n\nalpha section"。
		"A\n\nalpha section": true,
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
// delete_series (branch cleanup, v0.1.9+)
// -----------------------------------------------------------------------

func TestDeleteSeries_RemovesFromMultiplePaths(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// main と feature-x に登録された 3 path 構成:
	//   a: main + feature-x
	//   b: feature-x のみ
	//   c: main のみ
	upsert := func(path, content, series string) {
		_, _, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
			Key: "K", Series: series,
			Documents: []UpsertDocument{{Path: path, Content: content}},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	upsert("a", "# A\naaa", "main")
	upsert("a", "# A\naaa", "feature-x") // 同一 hash → series 共有
	upsert("b", "# B\nbbb", "feature-x")
	upsert("c", "# C\nccc", "main")

	// feature-x を全削除
	_, out, err := h.handlers.handleDeleteSeries(ctx, nil, DeleteSeriesInput{
		Key: "K", Series: "feature-x",
	})
	if err != nil {
		t.Fatal(err)
	}
	// a: main が残るので保持 → updated=1
	// b: series が空になり物理削除 → removed=1
	// c: feature-x を持たない → 触られない
	if out.RemovedRecords != 1 || out.UpdatedRecords != 1 {
		t.Errorf("got removed=%d updated=%d, want 1/1", out.RemovedRecords, out.UpdatedRecords)
	}

	// main で query しても a と c は残っている (b は消える)
	chunks, err := h.store.GetChunksForSearch(ctx, "K", "main")
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, c := range chunks {
		paths[c.Path] = true
	}
	if !paths["a"] || !paths["c"] {
		t.Errorf("main should still see a and c, got %v", paths)
	}
	if paths["b"] {
		t.Errorf("b should be pruned, got %v", paths)
	}
}

func TestDeleteSeries_NonExistentSeries_NoError(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, _, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
		Key: "K", Series: "main",
		Documents: []UpsertDocument{{Path: "a", Content: "# A\nx"}},
	}); err != nil {
		t.Fatal(err)
	}
	_, out, err := h.handlers.handleDeleteSeries(ctx, nil, DeleteSeriesInput{
		Key: "K", Series: "does-not-exist",
	})
	if err != nil {
		t.Fatalf("nonexistent series should not error: %v", err)
	}
	if out.RemovedRecords != 0 || out.UpdatedRecords != 0 {
		t.Errorf("got removed=%d updated=%d, want 0/0", out.RemovedRecords, out.UpdatedRecords)
	}
}

func TestDeleteSeries_ValidationErrors(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	cases := []DeleteSeriesInput{
		{Series: "s"},          // no key
		{Key: "K"},             // no series
	}
	for _, in := range cases {
		if _, _, err := h.handlers.handleDeleteSeries(ctx, nil, in); err == nil {
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

func TestQuery_DefaultMode_All(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, _, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
		Key: "K", Series: "s",
		Documents: []UpsertDocument{{Path: "a", Content: "# H\ntext"}},
	}); err != nil {
		t.Fatal(err)
	}
	// Mode 未指定 → all（PHIL-01: emb+lex+grep 3 signal 並列 over-recall）
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

// TestQuery_RejectedWhenKeyTrashed は、trash_index 実行後の KEY に対する query が
// 空結果ではなく明示エラーを返すことを検証する（TASK-009, DES-001 §5.7 UC-7）。
func TestQuery_RejectedWhenKeyTrashed(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if _, _, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
		Key: "K", Series: "s",
		Documents: []UpsertDocument{{Path: "p", Content: "# H\nhello world"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.handlers.handleTrashIndex(ctx, nil, TrashIndexInput{Key: "K"}); err != nil {
		t.Fatal(err)
	}

	_, out, err := h.handlers.handleQuery(ctx, nil, QueryInput{
		Query: "hello", Key: "K",
	})
	if err == nil {
		t.Fatal("want error for query against a trashed key")
	}
	if !strings.Contains(err.Error(), "ゴミ箱") || !strings.Contains(err.Error(), "restore_index") {
		t.Errorf("error message = %q, want it to mention ゴミ箱 and restore_index", err.Error())
	}
	if len(out.Results) != 0 {
		t.Errorf("out.Results = %+v, want empty (error path should not return results)", out.Results)
	}
}

// -----------------------------------------------------------------------
// list_indexes
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

// TestListIndexes_ChunkCount は list_indexes の応答に chunk_count が正しく
// 含まれることを検証する（TASK-006）。
func TestListIndexes_ChunkCount(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, _, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
		Key: "K1", Series: "s",
		Documents: []UpsertDocument{{Path: "p", Content: "# H\nx"}},
	}); err != nil {
		t.Fatal(err)
	}
	_, out, err := h.handlers.handleListIndexes(ctx, nil, ListIndexesInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Indexes) != 1 {
		t.Fatalf("len = %d, want 1", len(out.Indexes))
	}
	if out.Indexes[0].ChunkCount != 1 {
		t.Errorf("chunk_count = %d, want 1", out.Indexes[0].ChunkCount)
	}
}

// TestListIndexes_ExcludesTrashedKeys は list_indexes の応答からゴミ箱状態
// (trashed_at が非 NULL) の KEY が除外されることを検証する（TASK-006, DES-001 §8.1）。
func TestListIndexes_ExcludesTrashedKeys(t *testing.T) {
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
	if _, err := h.store.TrashKey(ctx, "K2"); err != nil {
		t.Fatal(err)
	}
	_, out, err := h.handlers.handleListIndexes(ctx, nil, ListIndexesInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Indexes) != 1 {
		t.Fatalf("len = %d, want 1 (K2 はゴミ箱状態のため除外されるべき)", len(out.Indexes))
	}
	if out.Indexes[0].Key != "K1" {
		t.Errorf("Indexes[0].Key = %q, want K1", out.Indexes[0].Key)
	}
}

// -----------------------------------------------------------------------
// trash_index / list_trashed_indexes / restore_index (TASK-007, DES-001 §5.5/§8.1)
// -----------------------------------------------------------------------

func TestTrashIndex(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, _, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
		Key: "K", Series: "s",
		Documents: []UpsertDocument{{Path: "p", Content: "# H\nx"}},
	}); err != nil {
		t.Fatal(err)
	}

	_, out, err := h.handlers.handleTrashIndex(ctx, nil, TrashIndexInput{Key: "K"})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Trashed {
		t.Error("Trashed = false")
	}
	if out.Key != "K" {
		t.Errorf("Key = %q, want K", out.Key)
	}
	if out.TrashedAt == "" {
		t.Error("TrashedAt is empty")
	}

	// 実データは物理削除されず残っている (list_indexes からは除外される)。
	trashed, terr := h.store.IsTrashed(ctx, "K")
	if terr != nil {
		t.Fatal(terr)
	}
	if !trashed {
		t.Error("IsTrashed = false, want true after trash_index")
	}
}

func TestTrashIndex_MissingKey(t *testing.T) {
	h := newHarness(t)
	_, _, err := h.handlers.handleTrashIndex(context.Background(), nil, TrashIndexInput{Key: ""})
	if err == nil {
		t.Fatal("want error for empty key")
	}
}

func TestTrashIndex_NonExistentKey(t *testing.T) {
	h := newHarness(t)
	_, _, err := h.handlers.handleTrashIndex(context.Background(), nil, TrashIndexInput{Key: "no-such-key"})
	if err == nil {
		t.Fatal("want error for non-existent key")
	}
}

func TestTrashIndex_DoubleTrashIsError(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, _, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
		Key: "K", Series: "s",
		Documents: []UpsertDocument{{Path: "p", Content: "# H\nx"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.handlers.handleTrashIndex(ctx, nil, TrashIndexInput{Key: "K"}); err != nil {
		t.Fatal(err)
	}
	// 多重投入はエラー。
	_, _, err := h.handlers.handleTrashIndex(ctx, nil, TrashIndexInput{Key: "K"})
	if err == nil {
		t.Fatal("want error for double trash_index (already trashed)")
	}
}

func TestListTrashedIndexes(t *testing.T) {
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
	// K2 のみゴミ箱投入。K1 は Active のままなので一覧に出ない。
	if _, _, err := h.handlers.handleTrashIndex(ctx, nil, TrashIndexInput{Key: "K2"}); err != nil {
		t.Fatal(err)
	}

	_, out, err := h.handlers.handleListTrashedIndexes(ctx, nil, ListTrashedIndexesInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Indexes) != 1 {
		t.Fatalf("len = %d, want 1", len(out.Indexes))
	}
	entry := out.Indexes[0]
	if entry.Key != "K2" {
		t.Errorf("Key = %q, want K2", entry.Key)
	}
	if entry.TrashedAt == "" {
		t.Error("TrashedAt is empty")
	}
	// testTrashRetentionDays = 3日 = 259200秒。直後の呼び出しなので、残り秒数は
	// ほぼ 259200 秒に近い値になるはず (許容誤差 10秒、テスト実行時間分のブレを吸収)。
	wantApprox := int64(testTrashRetentionDays * 24 * 60 * 60)
	diff := wantApprox - entry.RemainingSeconds
	if diff < 0 || diff > 10 {
		t.Errorf("RemainingSeconds = %d, want close to %d (diff=%d)", entry.RemainingSeconds, wantApprox, diff)
	}
}

func TestListTrashedIndexes_RemainingSecondsClampedToZero(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, _, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
		Key: "K", Series: "s",
		Documents: []UpsertDocument{{Path: "p", Content: "# H\nx"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.TrashKey(ctx, "K"); err != nil {
		t.Fatal(err)
	}
	// trashRetentionDays=0 の Handlers を別途構築し、保持期間超過状態を模擬する。
	h.handlers.trashRetentionDays = 0

	_, out, err := h.handlers.handleListTrashedIndexes(ctx, nil, ListTrashedIndexesInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Indexes) != 1 {
		t.Fatalf("len = %d, want 1", len(out.Indexes))
	}
	if out.Indexes[0].RemainingSeconds != 0 {
		t.Errorf("RemainingSeconds = %d, want 0 (超過分は負値ではなく 0 にクランプされる)", out.Indexes[0].RemainingSeconds)
	}
}

func TestListTrashedIndexes_Empty(t *testing.T) {
	h := newHarness(t)
	_, out, err := h.handlers.handleListTrashedIndexes(context.Background(), nil, ListTrashedIndexesInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Indexes) != 0 {
		t.Errorf("len = %d, want 0", len(out.Indexes))
	}
}

func TestRestoreIndex(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, _, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
		Key: "K", Series: "s",
		Documents: []UpsertDocument{{Path: "p", Content: "# H\nx"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.handlers.handleTrashIndex(ctx, nil, TrashIndexInput{Key: "K"}); err != nil {
		t.Fatal(err)
	}

	_, out, err := h.handlers.handleRestoreIndex(ctx, nil, RestoreIndexInput{Key: "K"})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Restored {
		t.Error("Restored = false")
	}
	if out.Key != "K" {
		t.Errorf("Key = %q, want K", out.Key)
	}

	trashed, terr := h.store.IsTrashed(ctx, "K")
	if terr != nil {
		t.Fatal(terr)
	}
	if trashed {
		t.Error("IsTrashed = true, want false after restore_index")
	}

	// list_indexes に戻っていること (実データがそのまま残っている)。
	_, li, lerr := h.handlers.handleListIndexes(ctx, nil, ListIndexesInput{})
	if lerr != nil {
		t.Fatal(lerr)
	}
	if len(li.Indexes) != 1 || li.Indexes[0].Key != "K" {
		t.Errorf("list_indexes after restore = %+v, want [K]", li.Indexes)
	}
}

func TestRestoreIndex_MissingKey(t *testing.T) {
	h := newHarness(t)
	_, _, err := h.handlers.handleRestoreIndex(context.Background(), nil, RestoreIndexInput{Key: ""})
	if err == nil {
		t.Fatal("want error for empty key")
	}
}

func TestRestoreIndex_NonExistentKey(t *testing.T) {
	h := newHarness(t)
	_, _, err := h.handlers.handleRestoreIndex(context.Background(), nil, RestoreIndexInput{Key: "no-such-key"})
	if err == nil {
		t.Fatal("want error for non-existent key")
	}
}

func TestRestoreIndex_NotTrashedIsError(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, _, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
		Key: "K", Series: "s",
		Documents: []UpsertDocument{{Path: "p", Content: "# H\nx"}},
	}); err != nil {
		t.Fatal(err)
	}
	// K はゴミ箱に入っていない (Active) のに復活を試みる → エラー。
	_, _, err := h.handlers.handleRestoreIndex(ctx, nil, RestoreIndexInput{Key: "K"})
	if err == nil {
		t.Fatal("want error for restoring a non-trashed key")
	}
}

// -----------------------------------------------------------------------
// ゴミ箱 KEY への書き込み系操作拒否 (TASK-012, DES-001 §5.7 UC-7, FNC-009)
// -----------------------------------------------------------------------

// TestWriteOps_RejectedWhenKeyTrashed は、ゴミ箱状態の KEY に対する
// upsert_documents / sync_documents / delete_documents / delete_series /
// schedule_delete_series の呼び出しがいずれも拒否され、trashed_at が
// 変化しないことを検証する。
func TestWriteOps_RejectedWhenKeyTrashed(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if _, _, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
		Key: "K", Series: "s",
		Documents: []UpsertDocument{{Path: "p", Content: "# H\nx"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.handlers.handleTrashIndex(ctx, nil, TrashIndexInput{Key: "K"}); err != nil {
		t.Fatal(err)
	}

	trashedBefore, terr := h.store.ListTrashedKeys(ctx)
	if terr != nil {
		t.Fatal(terr)
	}
	if len(trashedBefore) != 1 {
		t.Fatalf("setup: len(trashedBefore) = %d, want 1", len(trashedBefore))
	}
	trashedAtBefore := trashedBefore[0].TrashedAt

	t.Run("upsert_documents", func(t *testing.T) {
		_, _, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
			Key: "K", Series: "s",
			Documents: []UpsertDocument{{Path: "p2", Content: "# H\ny"}},
		})
		if err == nil {
			t.Fatal("want error for upsert_documents against a trashed key")
		}
		if !strings.Contains(err.Error(), "ゴミ箱") || !strings.Contains(err.Error(), "restore_index") {
			t.Errorf("error message = %q, want it to mention ゴミ箱 and restore_index", err.Error())
		}
	})

	t.Run("sync_documents", func(t *testing.T) {
		_, _, err := h.handlers.handleSyncDocuments(ctx, nil, SyncInput{
			Key: "K", Series: "s",
			Documents: []UpsertDocument{{Path: "p", Content: "# H\nx"}},
		})
		if err == nil {
			t.Fatal("want error for sync_documents against a trashed key")
		}
		if !strings.Contains(err.Error(), "ゴミ箱") || !strings.Contains(err.Error(), "restore_index") {
			t.Errorf("error message = %q, want it to mention ゴミ箱 and restore_index", err.Error())
		}
	})

	t.Run("delete_documents", func(t *testing.T) {
		_, _, err := h.handlers.handleDelete(ctx, nil, DeleteInput{
			Key: "K", Series: "s", Paths: []string{"p"},
		})
		if err == nil {
			t.Fatal("want error for delete_documents against a trashed key")
		}
		if !strings.Contains(err.Error(), "ゴミ箱") || !strings.Contains(err.Error(), "restore_index") {
			t.Errorf("error message = %q, want it to mention ゴミ箱 and restore_index", err.Error())
		}
	})

	t.Run("delete_series", func(t *testing.T) {
		_, _, err := h.handlers.handleDeleteSeries(ctx, nil, DeleteSeriesInput{
			Key: "K", Series: "s",
		})
		if err == nil {
			t.Fatal("want error for delete_series against a trashed key")
		}
		if !strings.Contains(err.Error(), "ゴミ箱") || !strings.Contains(err.Error(), "restore_index") {
			t.Errorf("error message = %q, want it to mention ゴミ箱 and restore_index", err.Error())
		}
	})

	t.Run("schedule_delete_series", func(t *testing.T) {
		_, _, err := h.handlers.handleScheduleDeleteSeries(ctx, nil, ScheduleDeleteSeriesInput{
			Key: "K", Series: "s",
		})
		if err == nil {
			t.Fatal("want error for schedule_delete_series against a trashed key")
		}
		if !strings.Contains(err.Error(), "ゴミ箱") || !strings.Contains(err.Error(), "restore_index") {
			t.Errorf("error message = %q, want it to mention ゴミ箱 and restore_index", err.Error())
		}
	})

	// trashed_at がこの拒否処理によって変更されていないことを確認する。
	trashedAfter, terr := h.store.ListTrashedKeys(ctx)
	if terr != nil {
		t.Fatal(terr)
	}
	if len(trashedAfter) != 1 {
		t.Fatalf("len(trashedAfter) = %d, want 1", len(trashedAfter))
	}
	if trashedAfter[0].TrashedAt != trashedAtBefore {
		t.Errorf("TrashedAt changed by rejected ops: before=%q, after=%q",
			trashedAtBefore, trashedAfter[0].TrashedAt)
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
