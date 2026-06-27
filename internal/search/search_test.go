package search

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/BlueEventHorizon/doc-db-mcp-server/internal/store"
)

// -----------------------------------------------------------------------
// モック
// -----------------------------------------------------------------------

type mockStore struct {
	chunks []store.Chunk
	err    error
}

func (m *mockStore) GetChunksForSearch(_ context.Context, _, _ string) ([]store.Chunk, error) {
	return m.chunks, m.err
}

type mockEmbedder struct {
	queryVec []float32
	err      error
}

func (m *mockEmbedder) Embed(_ context.Context, texts []string) ([][]float32, []int, error) {
	if m.err != nil {
		return nil, nil, m.err
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = m.queryVec
	}
	return out, nil, nil
}

// mockReranker は scores を直接返す（candidates と同じ長さ）。
// reference doc-db SKILL llm_rerank.py 同方式に合わせた interface。
type mockReranker struct {
	scores []float64
	err    error
}

func (m *mockReranker) Rerank(_ context.Context, _ string, _ []RerankCandidate) ([]float64, error) {
	return m.scores, m.err
}

// makeChunk はテスト用にシンプルな store.Chunk を作る。
func makeChunk(id int64, path, text string, vec []float32) store.Chunk {
	return store.Chunk{
		ID:          id,
		RecordID:    id,
		Key:         "K",
		Path:        path,
		HeadingPath: "# H",
		Text:        text,
		Vector:      vec,
		SeriesKeys:  []string{"main"},
	}
}

// -----------------------------------------------------------------------
// CosineSimilarity
// -----------------------------------------------------------------------

func TestCosineSimilarity(t *testing.T) {
	cases := []struct {
		name string
		a, b []float32
		want float64
	}{
		{"identical", []float32{1, 0, 0}, []float32{1, 0, 0}, 1.0},
		{"orthogonal", []float32{1, 0, 0}, []float32{0, 1, 0}, 0.0},
		{"opposite", []float32{1, 0, 0}, []float32{-1, 0, 0}, -1.0},
		{"empty", []float32{}, []float32{1}, 0.0},
		{"mismatched dim", []float32{1, 0}, []float32{1, 0, 0}, 0.0},
		{"zero vec", []float32{0, 0, 0}, []float32{1, 0, 0}, 0.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CosineSimilarity(tc.a, tc.b)
			if math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("CosineSimilarity = %v, want %v", got, tc.want)
			}
		})
	}
}

// -----------------------------------------------------------------------
// BM25 + ボーナス
// -----------------------------------------------------------------------

func TestComputeLexScores_BasicBM25(t *testing.T) {
	chunks := []store.Chunk{
		makeChunk(1, "a", "apple banana cherry", nil),
		makeChunk(2, "b", "apple apple apple", nil), // apple が頻出
		makeChunk(3, "c", "fig grape", nil),         // クエリ語と無関係
	}
	scores := computeLexScores("apple", chunks, 1.5, 0.75)
	if scores[2] != 0 {
		t.Errorf("無関係チャンクのスコアは 0、got %v", scores[2])
	}
	if scores[0] <= 0 || scores[1] <= 0 {
		t.Errorf("apple を含むチャンクのスコアは正、got %v", scores)
	}
	// 短い文書で頻出 → 通常高スコア
	if scores[1] <= scores[0] {
		t.Errorf("scores[1] (apple x3) = %v, want > scores[0] = %v", scores[1], scores[0])
	}
}

func TestComputeLexScores_IDBonus(t *testing.T) {
	chunks := []store.Chunk{
		makeChunk(1, "a", "ID FNC-001 spec here", nil),
		makeChunk(2, "b", "something else", nil),
	}
	scores := computeLexScores("FNC-001", chunks, 1.5, 0.75)
	if scores[0] < 10.0 {
		t.Errorf("ID bonus should add >=10, got %v", scores[0])
	}
	if scores[1] != 0 {
		t.Errorf("非該当チャンクは 0、got %v", scores[1])
	}
}

func TestComputeLexScores_FullPhraseBonus(t *testing.T) {
	chunks := []store.Chunk{
		makeChunk(1, "a", "the quick brown fox", nil),
		makeChunk(2, "b", "fox quick the brown", nil), // 同一語含むが連続フレーズではない
	}
	scores := computeLexScores("quick brown", chunks, 1.5, 0.75)
	// chunk 0 は連続フレーズ "quick brown" を含む → +2 ボーナス
	if scores[0] <= scores[1] {
		t.Errorf("phrase match bonus expected: scores[0]=%v, scores[1]=%v", scores[0], scores[1])
	}
}

func TestComputeLexScores_EmptyQuery(t *testing.T) {
	chunks := []store.Chunk{makeChunk(1, "a", "anything", nil)}
	scores := computeLexScores("", chunks, 1.5, 0.75)
	if scores[0] != 0 {
		t.Errorf("empty query → all 0, got %v", scores)
	}
}

// -----------------------------------------------------------------------
// RRF + EMB top-K 保証
// -----------------------------------------------------------------------

func TestFuseScores_RRFBasic(t *testing.T) {
	// emb: [0, 1, 2]（chunk 0 が emb 1 位）
	// lex: [2, 1, 0]（chunk 2 が lex 1 位）
	embRank := []int{0, 1, 2}
	lexRank := []int{2, 1, 0}
	embScores := []float64{0.9, 0.5, 0.1}
	lexScores := []float64{0.3, 0.5, 0.9}
	cfg := Config{}
	cfg.applyDefaults()

	order, rrf := fuseScores(embRank, lexRank, embScores, lexScores, cfg)

	if len(order) != 3 {
		t.Fatalf("order len = %d, want 3", len(order))
	}
	// 全候補が RRF スコアを持つこと
	for i, s := range rrf {
		if s <= 0 {
			t.Errorf("rrf[%d] = %v, want > 0", i, s)
		}
	}
	// chunk 1 は両方で 2 位 → 上位寄り
	// chunk 0 と 2 は片方 1 位、片方 3 位 → 同じスコア
	if math.Abs(rrf[0]-rrf[2]) > 1e-9 {
		t.Errorf("対称な順位の chunk 0 と 2 は同 RRF: %v vs %v", rrf[0], rrf[2])
	}
}

func TestFuseScores_EmbFallback(t *testing.T) {
	// lex がほぼゼロヒット (lex_score > 0 が 0 件) → emb 順がそのまま返る
	embRank := []int{2, 0, 1}
	lexRank := []int{0, 1, 2}
	embScores := []float64{0.5, 0.3, 0.9}
	lexScores := []float64{0, 0, 0} // 全て 0 → lex_hits = 0 でフォールバック発動
	cfg := Config{}
	cfg.applyDefaults()

	order, _ := fuseScores(embRank, lexRank, embScores, lexScores, cfg)
	if !reflect.DeepEqual(order, embRank) {
		t.Errorf("EMB フォールバック: order = %v, want %v", order, embRank)
	}
}

func TestPromoteEmbTopK(t *testing.T) {
	// emb 上位 K=3: [10, 20, 30]
	// 現在の fused: [50, 40, 30, 20, 10, 60] — 10, 20 が上位 3 に入っていない
	fused := []int{50, 40, 30, 20, 10, 60}
	emb := []int{10, 20, 30}

	got := promoteEmbTopK(fused, emb, 3)

	// 10 と 20 が先頭に昇格していること
	upper := got[:3]
	embSet := map[int]bool{10: true, 20: true, 30: true}
	for _, e := range upper {
		if !embSet[e] {
			t.Errorf("top-3 should contain only emb-top: got %v in %v", e, upper)
		}
	}
	if len(got) != len(fused) {
		t.Errorf("len mismatch: got %d, want %d", len(got), len(fused))
	}
}

func TestPromoteEmbTopK_AlreadyInTop(t *testing.T) {
	fused := []int{10, 20, 30, 40}
	emb := []int{10, 20}
	got := promoteEmbTopK(fused, emb, 2)
	if !reflect.DeepEqual(got, fused) {
		t.Errorf("no change expected, got %v", got)
	}
}

// -----------------------------------------------------------------------
// Pipeline.Run: モード別
// -----------------------------------------------------------------------

func TestRun_EmbMode(t *testing.T) {
	q := []float32{1, 0, 0}
	chunks := []store.Chunk{
		makeChunk(1, "a", "alpha", []float32{1, 0, 0}),  // cos=1
		makeChunk(2, "b", "beta", []float32{0, 1, 0}),   // cos=0
		makeChunk(3, "c", "gamma", []float32{-1, 0, 0}), // cos=-1
	}
	p := New(&mockStore{chunks: chunks}, &mockEmbedder{queryVec: q}, nil, Config{})

	out, err := p.Run(context.Background(), "K", "", "query", ModeEmb, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 3 {
		t.Fatalf("len = %d", len(out.Results))
	}
	if out.Results[0].Path != "a" {
		t.Errorf("top = %q, want a (cos=1)", out.Results[0].Path)
	}
}

func TestRun_LexMode(t *testing.T) {
	chunks := []store.Chunk{
		makeChunk(1, "a", "no relevant", nil),
		makeChunk(2, "b", "FNC-001 spec details", nil), // ID match
		makeChunk(3, "c", "another doc", nil),
	}
	p := New(&mockStore{chunks: chunks}, &mockEmbedder{queryVec: []float32{1, 0, 0}}, nil, Config{})

	out, err := p.Run(context.Background(), "K", "", "FNC-001", ModeLex, 10)
	if err != nil {
		t.Fatal(err)
	}
	if out.Results[0].Path != "b" {
		t.Errorf("top = %q, want b (ID match)", out.Results[0].Path)
	}
}

func TestRun_HybridMode(t *testing.T) {
	q := []float32{1, 0, 0}
	chunks := []store.Chunk{
		makeChunk(1, "a", "alpha foo bar", []float32{1, 0, 0}),  // emb 1 位
		makeChunk(2, "b", "lex match foo", []float32{0, 1, 0}),  // lex 1 位 (foo 含)
		makeChunk(3, "c", "unrelated", []float32{-1, 0, 0}),
	}
	p := New(&mockStore{chunks: chunks}, &mockEmbedder{queryVec: q}, nil, Config{})

	out, err := p.Run(context.Background(), "K", "", "foo", ModeHybrid, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 3 {
		t.Fatalf("len = %d", len(out.Results))
	}
	// stage_stats が記録されていること
	if out.Stats.EmbCandidates == 0 {
		t.Error("EmbCandidates should be > 0")
	}
	if out.Stats.LexCandidates == 0 {
		t.Error("LexCandidates should be > 0")
	}
	if out.Stats.FusedCandidates == 0 {
		t.Error("FusedCandidates should be > 0")
	}
}

func TestRun_RerankSuccess(t *testing.T) {
	q := []float32{1, 0, 0}
	chunks := []store.Chunk{
		makeChunk(1, "a", "doc a foo", []float32{1, 0, 0}),
		makeChunk(2, "b", "doc b foo", []float32{0.8, 0.2, 0}),
		makeChunk(3, "c", "doc c foo", []float32{0.5, 0.5, 0}),
	}
	// Reranker は候補 0 を最高、2 を最低スコアにする
	// → reverse 効果（[2, 1, 0] の rank マッピング → score 0.1, 0.5, 0.9）
	reranker := &mockReranker{scores: []float64{0.1, 0.5, 0.9}}
	p := New(&mockStore{chunks: chunks}, &mockEmbedder{queryVec: q}, reranker, Config{RerankFactor: 3})

	out, err := p.Run(context.Background(), "K", "", "foo", ModeRerank, 3)
	if err != nil {
		t.Fatal(err)
	}
	if out.Results[0].ScoreBreakdown.Rerank == 0 {
		t.Error("rerank score should be set on the top result")
	}
	if out.Stats.RerankCandidates == 0 {
		t.Error("RerankCandidates should be recorded")
	}
}

func TestRun_RerankFallback(t *testing.T) {
	q := []float32{1, 0, 0}
	chunks := []store.Chunk{
		makeChunk(1, "a", "doc a foo", []float32{1, 0, 0}),
		makeChunk(2, "b", "doc b foo", []float32{0.5, 0.5, 0}),
	}
	// Reranker がエラー → RR-02: RRF 順にフォールバック
	reranker := &mockReranker{err: errors.New("LLM timeout")}
	p := New(&mockStore{chunks: chunks}, &mockEmbedder{queryVec: q}, reranker, Config{RerankFactor: 3})

	out, err := p.Run(context.Background(), "K", "", "foo", ModeRerank, 2)
	if err != nil {
		t.Fatalf("rerank failure should fall back, not propagate: %v", err)
	}
	if len(out.Results) != 2 {
		t.Fatalf("len = %d, want 2", len(out.Results))
	}
	// 全 result の rerank スコアは 0（fallback で rerank 適用なし）
	for i, r := range out.Results {
		if r.ScoreBreakdown.Rerank != 0 {
			t.Errorf("result[%d] rerank = %v, want 0 on fallback", i, r.ScoreBreakdown.Rerank)
		}
	}
}

func TestRun_EmptyChunks(t *testing.T) {
	p := New(&mockStore{chunks: nil}, &mockEmbedder{queryVec: []float32{1}}, nil, Config{})
	out, err := p.Run(context.Background(), "K", "", "q", ModeHybrid, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 0 {
		t.Errorf("len = %d, want 0", len(out.Results))
	}
}

func TestRun_StoreError(t *testing.T) {
	p := New(&mockStore{err: errors.New("db down")}, &mockEmbedder{}, nil, Config{})
	_, err := p.Run(context.Background(), "K", "", "q", ModeHybrid, 5)
	if err == nil {
		t.Fatal("want store error to propagate")
	}
}

func TestRun_UnknownMode(t *testing.T) {
	p := New(&mockStore{chunks: []store.Chunk{makeChunk(1, "a", "x", []float32{1})}},
		&mockEmbedder{queryVec: []float32{1}}, nil, Config{})
	_, err := p.Run(context.Background(), "K", "", "q", Mode("bogus"), 5)
	if err == nil {
		t.Fatal("want error for unknown mode")
	}
}

func TestRun_TopNCappedToAvailable(t *testing.T) {
	chunks := []store.Chunk{
		makeChunk(1, "a", "a", []float32{1, 0, 0}),
		makeChunk(2, "b", "b", []float32{0.5, 0.5, 0}),
	}
	p := New(&mockStore{chunks: chunks}, &mockEmbedder{queryVec: []float32{1, 0, 0}}, nil, Config{})
	out, err := p.Run(context.Background(), "K", "", "q", ModeEmb, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 2 {
		t.Errorf("len = %d, want 2 (capped to available)", len(out.Results))
	}
}

// -----------------------------------------------------------------------
// applyDefaults
// -----------------------------------------------------------------------

func TestConfig_ApplyDefaults(t *testing.T) {
	c := Config{}
	c.applyDefaults()
	if c.K1 != 1.5 || c.B != 0.75 || c.RRFK != 60 ||
		c.EmbFallbackLexRatio != 0.05 || c.EmbGuaranteeK != 5 || c.RerankFactor != 3 {
		t.Errorf("defaults not applied: %+v", c)
	}
}
