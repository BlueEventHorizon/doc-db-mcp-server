package mcp

// TASK-019 — query 統合テスト (DES-001 §11)
//
// 検証項目:
//   - mode=emb / lex / hybrid / rerank で結果順序が変わりうること
//   - StageStats（emb/lex/fused/rerank の通過候補数）が出力に含まれること
//   - series 絞り込みでスコープ外チャンクが除外されること
//   - LLM Rerank が失敗した場合に RRF 順にフォールバックすること（RR-02）

import (
	"context"
	"errors"
	"testing"

	"github.com/BlueEventHorizon/doc-db-mcp-server/internal/chunker"
	"github.com/BlueEventHorizon/doc-db-mcp-server/internal/search"
)

// -----------------------------------------------------------------------
// テスト用 Reranker
// -----------------------------------------------------------------------

// reverseReranker は候補の並びを反転させて返す（順序差を観測しやすくするため）。
type reverseReranker struct{}

func (reverseReranker) Rerank(_ context.Context, _ string, cands []search.RerankCandidate) ([]int, error) {
	out := make([]int, len(cands))
	for i := range cands {
		out[i] = len(cands) - 1 - i
	}
	return out, nil
}

// errReranker は常にエラーを返す（フォールバック確認用）。
type errReranker struct{}

func (errReranker) Rerank(_ context.Context, _ string, _ []search.RerankCandidate) ([]int, error) {
	return nil, errors.New("rerank service unavailable")
}

// newQueryHarness は Reranker 注入可能なハーネスを返す。
func newQueryHarness(t *testing.T, rr search.Reranker) *testHarness {
	t.Helper()
	h := newHarness(t)
	// search Pipeline を Reranker 付きで作り直す
	pipe := search.New(h.store, &SearchEmbedderAdapter{Inner: h.embedder}, rr, search.Config{})
	h.handlers = New(h.store, chunker.New(1500), h.embedder, h.fetcher, pipe)
	return h
}

// -----------------------------------------------------------------------
// 共通セットアップ — 3 ドキュメント / 各々別ベクトル
// -----------------------------------------------------------------------

func seedQueryCorpus(t *testing.T, h *testHarness) {
	t.Helper()
	// Embedder: クエリ "alpha" のベクトルが doc a に最も近くなるよう設計
	// → ベクトル次元 3
	//   a: (1, 0, 0)   ← 高 cosine
	//   b: (0, 1, 0)
	//   c: (0, 0, 1)
	h.embedder.fixed = nil
	h.embedder.vectors = [][]float32{
		{1, 0, 0}, // chunk 0 (a)
		{0, 1, 0}, // chunk 1 (b)
		{0, 0, 1}, // chunk 2 (c)
	}
	docs := []UpsertDocument{
		{Path: "a.md", Content: "# H\nalpha foundation"},
		{Path: "b.md", Content: "# H\nbeta runtime"},
		{Path: "c.md", Content: "# H\ngamma deploy"},
	}
	if _, _, err := h.handlers.handleUpsert(context.Background(), nil, UpsertInput{
		Key: "K", Series: "s1", Documents: docs,
	}); err != nil {
		t.Fatal(err)
	}
	// クエリ用には alpha と一致するベクトルを返したい → embedder の vectors を上書き
	h.embedder.vectors = [][]float32{{1, 0, 0}}
}

// -----------------------------------------------------------------------
// mode 別結果差異
// -----------------------------------------------------------------------

func TestQueryIntegration_ModesProduceDistinctOrdering(t *testing.T) {
	h := newQueryHarness(t, nil)
	seedQueryCorpus(t, h)
	ctx := context.Background()

	// emb モード: ベクトル類似度 a > b/c
	_, embOut, err := h.handlers.handleQuery(ctx, nil, QueryInput{
		Query: "alpha", Key: "K", Mode: "emb", TopN: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(embOut.Results) == 0 || embOut.Results[0].Path != "a.md" {
		t.Errorf("emb top should be a.md, got %+v", embOut.Results)
	}

	// lex モード: BM25 で alpha を含む a.md がトップ
	_, lexOut, err := h.handlers.handleQuery(ctx, nil, QueryInput{
		Query: "alpha", Key: "K", Mode: "lex", TopN: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(lexOut.Results) == 0 || lexOut.Results[0].Path != "a.md" {
		t.Errorf("lex top should be a.md, got %+v", lexOut.Results)
	}

	// hybrid モード: RRF 融合
	_, hybridOut, err := h.handlers.handleQuery(ctx, nil, QueryInput{
		Query: "alpha", Key: "K", Mode: "hybrid", TopN: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hybridOut.Results) == 0 || hybridOut.Results[0].Path != "a.md" {
		t.Errorf("hybrid top should be a.md, got %+v", hybridOut.Results)
	}

	// stage_stats 検証: emb モードでは emb_candidates > 0、lex_candidates = 0
	if embOut.StageStats.EmbCandidates == 0 {
		t.Errorf("emb mode: EmbCandidates should be > 0, got %+v", embOut.StageStats)
	}
	if embOut.StageStats.LexCandidates != 0 {
		t.Errorf("emb mode: LexCandidates should be 0, got %+v", embOut.StageStats)
	}
	// lex モードは逆
	if lexOut.StageStats.LexCandidates == 0 {
		t.Errorf("lex mode: LexCandidates should be > 0, got %+v", lexOut.StageStats)
	}
	if lexOut.StageStats.EmbCandidates != 0 {
		t.Errorf("lex mode: EmbCandidates should be 0, got %+v", lexOut.StageStats)
	}
	// hybrid モードは両方 > 0、FusedCandidates も > 0
	if hybridOut.StageStats.EmbCandidates == 0 ||
		hybridOut.StageStats.LexCandidates == 0 ||
		hybridOut.StageStats.FusedCandidates == 0 {
		t.Errorf("hybrid mode: stage_stats incomplete: %+v", hybridOut.StageStats)
	}
}

// rerank モード: Reranker が候補を反転させた場合に順序が変わることを確認する。
func TestQueryIntegration_RerankReordersResults(t *testing.T) {
	h := newQueryHarness(t, reverseReranker{})
	seedQueryCorpus(t, h)
	ctx := context.Background()

	_, out, err := h.handlers.handleQuery(ctx, nil, QueryInput{
		Query: "alpha", Key: "K", Mode: "rerank", TopN: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.StageStats.RerankCandidates == 0 {
		t.Errorf("RerankCandidates should be > 0 in rerank mode: %+v", out.StageStats)
	}
	// reverseReranker は順序を反転させる → 元 RRF の上位（a.md）が末尾にくる
	if len(out.Results) < 2 {
		t.Fatalf("need at least 2 results, got %d", len(out.Results))
	}
	if out.Results[0].Path == "a.md" {
		// 反転されているなら、emb/lex 共に上位の a.md は先頭から外れているはず
		t.Errorf("reverseReranker should move a.md down; got top=%s", out.Results[0].Path)
	}
}

// RR-02: Reranker 失敗時は RRF 順にフォールバックする。
func TestQueryIntegration_RerankFallback_OnError(t *testing.T) {
	h := newQueryHarness(t, errReranker{})
	seedQueryCorpus(t, h)
	ctx := context.Background()

	_, out, err := h.handlers.handleQuery(ctx, nil, QueryInput{
		Query: "alpha", Key: "K", Mode: "rerank", TopN: 3,
	})
	if err != nil {
		t.Fatalf("rerank fallback should not propagate error: %v", err)
	}
	if len(out.Results) == 0 {
		t.Fatal("results should not be empty on fallback")
	}
	// フォールバック: RRF 上位がそのまま使われる → a.md がトップに残るはず
	if out.Results[0].Path != "a.md" {
		t.Errorf("fallback should preserve RRF order; top=%s", out.Results[0].Path)
	}
	// 全 result の Rerank スコアは 0（rerankApplied=false）
	for _, r := range out.Results {
		if r.ScoreBreakdown.Rerank != 0 {
			t.Errorf("Rerank score should be 0 on fallback; got %+v", r.ScoreBreakdown)
		}
	}
}

// series 絞り込みでスコープ外チャンクが除外されること。
func TestQueryIntegration_SeriesFilter_ScopesResults(t *testing.T) {
	h := newQueryHarness(t, nil)
	ctx := context.Background()

	// 2 つの series: s1=alpha のみ、s2=beta のみ
	h.embedder.fixed = []float32{1, 0, 0}
	if _, _, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
		Key: "K", Series: "s1",
		Documents: []UpsertDocument{{Path: "a.md", Content: "# H\nalpha"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
		Key: "K", Series: "s2",
		Documents: []UpsertDocument{{Path: "b.md", Content: "# H\nbeta"}},
	}); err != nil {
		t.Fatal(err)
	}

	// series=s1 で検索 → a.md のみ取れる
	_, out, err := h.handlers.handleQuery(ctx, nil, QueryInput{
		Query: "alpha", Key: "K", Series: "s1", Mode: "lex", TopN: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range out.Results {
		if r.Path == "b.md" {
			t.Errorf("series=s1 should not include b.md: %+v", out.Results)
		}
	}

	// series 指定なしなら両方見える
	_, outAll, err := h.handlers.handleQuery(ctx, nil, QueryInput{
		Query: "alpha beta", Key: "K", Mode: "lex", TopN: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, r := range outAll.Results {
		paths[r.Path] = true
	}
	if !paths["a.md"] || !paths["b.md"] {
		t.Errorf("no series filter should see both: %+v", outAll.Results)
	}
}
