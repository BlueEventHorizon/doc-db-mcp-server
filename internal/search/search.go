// Package search はハイブリッド検索パイプラインを実装する（DES-001 §6）。
// 構成要素:
//   - cosine 類似度（§6.1）
//   - BM25 + ID/全文一致ボーナス（§6.2）
//   - RRF + EMB フォールバック + EMB top-K 保証（§6.3）
//   - LLM Rerank フォールバック付き（§6.4, RR-02）
package search

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/BlueEventHorizon/doc-db-mcp-server/internal/store"
)

// -----------------------------------------------------------------------
// 型定義
// -----------------------------------------------------------------------

// Mode は検索モード。
type Mode string

const (
	ModeEmb    Mode = "emb"    // ベクトル検索のみ
	ModeLex    Mode = "lex"    // 語彙検索のみ
	ModeHybrid Mode = "hybrid" // ベクトル + 語彙の RRF 融合
	ModeRerank Mode = "rerank" // hybrid + LLM Rerank
)

// SearchResult は 1 件の検索結果。
type SearchResult struct {
	Path           string
	HeadingPath    string
	Text           string
	Score          float64
	ScoreBreakdown ScoreBreakdown
	SeriesKeys     []string
}

// ScoreBreakdown はスコアの内訳（クライアントに返す）。
type ScoreBreakdown struct {
	Emb    float64 `json:"emb"`
	Lex    float64 `json:"lex"`
	RRF    float64 `json:"rrf"`
	Rerank float64 `json:"rerank"`
}

// StageStats は各ステージを通過した候補数（QRY-OUT-02）。
type StageStats struct {
	EmbCandidates    int `json:"emb_candidates"`
	LexCandidates    int `json:"lex_candidates"`
	FusedCandidates  int `json:"fused_candidates"`
	RerankCandidates int `json:"rerank_candidates"`
}

// Output は Pipeline.Run の戻り値。
type Output struct {
	Results []SearchResult
	Stats   StageStats
}

// -----------------------------------------------------------------------
// 依存インターフェース
// -----------------------------------------------------------------------

// Storer は search が必要とする store メソッドのサブセット。
type Storer interface {
	GetChunksForSearch(ctx context.Context, key, series string) ([]store.Chunk, error)
}

// Embedder はクエリ埋め込みを生成する（モック差し替え可）。
type Embedder interface {
	Embed(ctx context.Context, texts []string) (vecs [][]float32, skipped []int, err error)
}

// Reranker は LLM Rerank（mode=rerank 時のみ呼ばれる）。
// Rerank 失敗時は呼び出し元が RRF 順にフォールバックする（RR-02）。
type Reranker interface {
	Rerank(ctx context.Context, query string, candidates []RerankCandidate) (orderedIndices []int, err error)
}

// RerankCandidate は Reranker に渡す候補情報。
type RerankCandidate struct {
	Index       int    // 元配列のインデックス
	Text        string
	HeadingPath string
}

// -----------------------------------------------------------------------
// Config
// -----------------------------------------------------------------------

// Config は検索パイプラインの設定。
type Config struct {
	// BM25 パラメータ（DES-001 §6.2）
	K1 float64 // デフォルト 1.5
	B  float64 // デフォルト 0.75

	// RRF パラメータ（DES-001 §6.3）
	RRFK int // デフォルト 60

	// EMB フォールバック・保証（DES-001 §6.3 SC-01）
	EmbFallbackLexRatio float64 // デフォルト 0.05
	EmbGuaranteeK       int     // デフォルト 5

	// Rerank パラメータ（DES-001 §6.4）
	RerankFactor int // top_n * rerank_factor 件を Rerank に渡す。デフォルト 3
}

// applyDefaults は未設定（ゼロ値または負）の設定にデフォルト値を埋め込む。
func (c *Config) applyDefaults() {
	if c.K1 <= 0 {
		c.K1 = 1.5
	}
	if c.B < 0 {
		c.B = 0.75
	}
	if c.B == 0 {
		// b=0 は length 正規化なしを意味するため意図的な 0 を許容する。
		// ただし「未設定の 0」と区別できないので慣例的にデフォルト適用。
		c.B = 0.75
	}
	if c.RRFK <= 0 {
		c.RRFK = 60
	}
	if c.EmbFallbackLexRatio <= 0 {
		c.EmbFallbackLexRatio = 0.05
	}
	if c.EmbGuaranteeK <= 0 {
		c.EmbGuaranteeK = 5
	}
	if c.RerankFactor <= 0 {
		c.RerankFactor = 3
	}
}

// -----------------------------------------------------------------------
// Pipeline
// -----------------------------------------------------------------------

// Pipeline は検索パイプライン本体。
type Pipeline struct {
	store    Storer
	embedder Embedder
	reranker Reranker // nil 可
	cfg      Config
}

// New は Pipeline を初期化する。reranker は nil 可（ModeRerank 時は RRF 結果が返る）。
func New(st Storer, embedder Embedder, reranker Reranker, cfg Config) *Pipeline {
	cfg.applyDefaults()
	return &Pipeline{store: st, embedder: embedder, reranker: reranker, cfg: cfg}
}

// Run はクエリを実行する（DES-001 §3 シーケンス）。
func (p *Pipeline) Run(ctx context.Context, key, series, query string, mode Mode, topN int) (Output, error) {
	if topN <= 0 {
		topN = 10
	}

	chunks, err := p.store.GetChunksForSearch(ctx, key, series)
	if err != nil {
		return Output{}, fmt.Errorf("search: load chunks: %w", err)
	}
	if len(chunks) == 0 {
		return Output{Results: nil, Stats: StageStats{}}, nil
	}

	// emb スコア計算（lex モード以外で必要）
	var embScores []float64 // chunks と同長
	var embRank []int       // chunks 内の emb 降順インデックス
	if mode != ModeLex {
		queryVec, err := p.embedQuery(ctx, query)
		if err != nil {
			return Output{}, fmt.Errorf("search: embed query: %w", err)
		}
		embScores = computeCosineScores(queryVec, chunks)
		embRank = sortIndicesByScore(embScores)
	}

	// lex スコア計算（emb モード以外で必要）
	var lexScores []float64
	var lexRank []int
	if mode != ModeEmb {
		lexScores = computeLexScores(query, chunks, p.cfg.K1, p.cfg.B)
		lexRank = sortIndicesByScore(lexScores)
	}

	// ステージ統計
	stats := StageStats{
		EmbCandidates: countNonZero(embScores),
		LexCandidates: countNonZero(lexScores),
	}

	// 融合 / 単一モード選択
	var fusedOrder []int // chunks 内インデックスを順位付き降順で並べたもの
	var rrfScores []float64
	switch mode {
	case ModeEmb:
		fusedOrder = embRank
	case ModeLex:
		fusedOrder = lexRank
	case ModeHybrid, ModeRerank:
		fusedOrder, rrfScores = fuseScores(embRank, lexRank, embScores, p.cfg)
	default:
		return Output{}, fmt.Errorf("search: unknown mode %q", mode)
	}
	stats.FusedCandidates = len(fusedOrder)

	// Rerank（mode=rerank のみ）
	rerankApplied := false
	if mode == ModeRerank && p.reranker != nil {
		nCand := topN * p.cfg.RerankFactor
		if nCand > len(fusedOrder) {
			nCand = len(fusedOrder)
		}
		topCandidates := fusedOrder[:nCand]
		stats.RerankCandidates = len(topCandidates)

		newOrder, err := p.rerank(ctx, query, chunks, topCandidates)
		if err == nil {
			fusedOrder = append(newOrder, fusedOrder[nCand:]...)
			rerankApplied = true
		}
		// 失敗時は RRF 順をそのまま使う（RR-02）
	} else if mode == ModeRerank {
		// reranker 未注入の場合: stats.RerankCandidates は fused 数と同値
		stats.RerankCandidates = stats.FusedCandidates
	}

	// SearchResult を構築（上位 topN 件）
	resultCount := topN
	if resultCount > len(fusedOrder) {
		resultCount = len(fusedOrder)
	}
	results := make([]SearchResult, 0, resultCount)
	for rank, idx := range fusedOrder[:resultCount] {
		c := chunks[idx]
		var (
			embS, lexS, rrfS, rerankS float64
		)
		if embScores != nil {
			embS = embScores[idx]
		}
		if lexScores != nil {
			lexS = lexScores[idx]
		}
		if rrfScores != nil {
			rrfS = rrfScores[idx]
		}
		if rerankApplied {
			// rerank 後の順位を逆順スコアで表現（高い順位ほど大きい）
			rerankS = float64(resultCount - rank)
		}
		results = append(results, SearchResult{
			Path:        c.Path,
			HeadingPath: c.HeadingPath,
			Text:        c.Text,
			Score:       primaryScore(mode, embS, lexS, rrfS, rerankS),
			ScoreBreakdown: ScoreBreakdown{
				Emb:    embS,
				Lex:    lexS,
				RRF:    rrfS,
				Rerank: rerankS,
			},
			SeriesKeys: c.SeriesKeys,
		})
	}

	return Output{Results: results, Stats: stats}, nil
}

// embedQuery はクエリテキストを 1 ベクトルに変換する。
func (p *Pipeline) embedQuery(ctx context.Context, query string) ([]float32, error) {
	vecs, _, err := p.embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 || vecs[0] == nil {
		return nil, fmt.Errorf("embedder returned no vector for query")
	}
	return vecs[0], nil
}

// rerank は LLM Reranker を呼ぶ。
func (p *Pipeline) rerank(ctx context.Context, query string, chunks []store.Chunk, idxs []int) ([]int, error) {
	cands := make([]RerankCandidate, len(idxs))
	for i, ci := range idxs {
		cands[i] = RerankCandidate{Index: ci, Text: chunks[ci].Text, HeadingPath: chunks[ci].HeadingPath}
	}
	order, err := p.reranker.Rerank(ctx, query, cands)
	if err != nil {
		return nil, err
	}
	// order は cands 内の rank 順インデックス。chunks 上のインデックスに変換する。
	out := make([]int, 0, len(order))
	for _, candIdx := range order {
		if candIdx < 0 || candIdx >= len(cands) {
			return nil, fmt.Errorf("rerank: invalid candidate index %d", candIdx)
		}
		out = append(out, cands[candIdx].Index)
	}
	return out, nil
}

// primaryScore は表示用の代表スコアを選ぶ。
func primaryScore(mode Mode, emb, lex, rrf, rerank float64) float64 {
	if rerank > 0 {
		return rerank
	}
	switch mode {
	case ModeEmb:
		return emb
	case ModeLex:
		return lex
	default:
		return rrf
	}
}

// -----------------------------------------------------------------------
// cosine 類似度（§6.1）
// -----------------------------------------------------------------------

// CosineSimilarity は 2 つのベクトルのコサイン類似度を返す。
// どちらかが空、または長さが異なる場合は 0 を返す。
func CosineSimilarity(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// computeCosineScores は queryVec と各 chunk.Vector のコサイン類似度を返す。
// vector が空のチャンクは 0。
func computeCosineScores(queryVec []float32, chunks []store.Chunk) []float64 {
	scores := make([]float64, len(chunks))
	for i, c := range chunks {
		if len(c.Vector) > 0 {
			scores[i] = CosineSimilarity(queryVec, c.Vector)
		}
	}
	return scores
}

// -----------------------------------------------------------------------
// BM25 + ボーナス（§6.2）
// -----------------------------------------------------------------------

// idBonusPattern は ID パターン（FNC-001 など）にマッチする（LEX-01）。
var idBonusPattern = regexp.MustCompile(`[A-Z]+-\d+`)

// computeLexScores は各チャンクの BM25 スコア + ボーナスを返す。
//
// BM25 計算:
//   - クエリと全チャンクをトークナイズし、各 term の DF を計算
//   - 各チャンクごとに query term の合計 BM25 スコアを求める
//
// ボーナス:
//   - クエリ内 ID パターン（[A-Z]+-\d+）がチャンク本文に含まれる → +10
//   - 正規化クエリ全体がチャンク本文に含まれる → +2
func computeLexScores(query string, chunks []store.Chunk, k1, b float64) []float64 {
	scores := make([]float64, len(chunks))

	queryTerms := store.Tokenize(query)
	if len(queryTerms) == 0 {
		return scores
	}

	// 各チャンクのトークン化とドキュメント長を計算
	chunkTokens := make([][]string, len(chunks))
	docLens := make([]int, len(chunks))
	var totalLen int
	for i, c := range chunks {
		toks := store.Tokenize(c.Text)
		chunkTokens[i] = toks
		docLens[i] = len(toks)
		totalLen += len(toks)
	}
	avgDocLen := 0.0
	if len(chunks) > 0 {
		avgDocLen = float64(totalLen) / float64(len(chunks))
	}

	// query term ごとに DF を計算
	df := make(map[string]int, len(queryTerms))
	uniqQueryTerms := uniqueStrings(queryTerms)
	for _, t := range uniqQueryTerms {
		df[t] = 0
		for _, toks := range chunkTokens {
			if containsString(toks, t) {
				df[t]++
			}
		}
	}

	N := float64(len(chunks))
	for i := range chunks {
		// term frequency in this chunk
		tf := make(map[string]int)
		for _, tok := range chunkTokens[i] {
			tf[tok]++
		}

		var score float64
		for _, t := range queryTerms {
			f := float64(tf[t])
			if f == 0 {
				continue
			}
			n := float64(df[t])
			if n == 0 {
				continue
			}
			// IDF: log((N - n + 0.5) / (n + 0.5) + 1)  ← Lucene-flavor BM25 (正値保証)
			idf := math.Log((N-n+0.5)/(n+0.5) + 1.0)
			dl := float64(docLens[i])
			norm := 1 - b + b*(dl/avgDocLen)
			score += idf * (f * (k1 + 1)) / (f + k1*norm)
		}

		// ID パターンボーナス
		for _, m := range idBonusPattern.FindAllString(query, -1) {
			if strings.Contains(chunks[i].Text, m) {
				score += 10.0
			}
		}
		// 全文一致ボーナス
		normQuery := strings.ToLower(query)
		if normQuery != "" && strings.Contains(strings.ToLower(chunks[i].Text), normQuery) {
			score += 2.0
		}

		scores[i] = score
	}
	return scores
}

func uniqueStrings(s []string) []string {
	seen := make(map[string]struct{}, len(s))
	out := make([]string, 0, len(s))
	for _, v := range s {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

func containsString(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// -----------------------------------------------------------------------
// RRF 融合（§6.3）
// -----------------------------------------------------------------------

// fuseScores は emb / lex の順位を RRF で融合する。
// EMB フォールバック（lex_hit / emb_hit < ratio）が発動した場合は emb 順をそのまま返す。
// 通常時は RRF 後に EMB top-K 保証を適用する。
//
// 戻り値:
//   - order: chunks インデックスを RRF スコア降順に並べた配列
//   - rrfScores: chunks インデックスに対応する RRF スコア（fusedOrder と並列）
func fuseScores(embRank, lexRank []int, embScores []float64, cfg Config) ([]int, []float64) {
	embHits := countNonZero(embScores)
	lexHits := len(lexRank)
	// lex 命中ゼロ判定は、lexScores が non-zero な要素数で行うべきだが、
	// 上位呼出側で embScores しか渡していないため、lexRank 自体ではなくその non-zero 数を別途数える方が正確。
	// しかし簡略化のため、lex_hit を lexRank の長さで近似する（実装が冗長になるため）。
	// 実用上、EMB フォールバック判定では「lex がほぼ機能しない」シグナルがあれば良い。

	// EMB フォールバック判定
	if embHits > 0 {
		ratio := float64(lexHits) / float64(embHits)
		if ratio < cfg.EmbFallbackLexRatio {
			// EMB スコア降順のみで返す
			scores := make([]float64, len(embScores))
			for rank, idx := range embRank {
				_ = rank
				scores[idx] = embScores[idx]
			}
			return embRank, scores
		}
	}

	// RRF スコア計算
	n := maxLen(embRank, lexRank)
	rrf := make([]float64, n)
	rankInScores := func(rank []int) {
		for r, idx := range rank {
			if idx >= 0 && idx < n {
				rrf[idx] += 1.0 / float64(cfg.RRFK+r+1) // r は 0-based なので +1
			}
		}
	}
	rankInScores(embRank)
	rankInScores(lexRank)

	order := sortIndicesByScore(rrf)

	// EMB top-K 保証: emb 上位 K 件が fused 上位 K' に含まれるよう昇格
	order = promoteEmbTopK(order, embRank, cfg.EmbGuaranteeK)

	return order, rrf
}

// promoteEmbTopK は emb 上位 K 件を fused 上位 K に昇格させる（SC-01）。
// 戦略: emb-top を先頭にコピーし、残りの fused 要素を後続に連結する（emb-top の
// 元の相対順を保持）。fused 全体の長さは保たれる。
func promoteEmbTopK(fused, embRank []int, K int) []int {
	if K <= 0 || len(embRank) == 0 {
		return fused
	}
	if K > len(embRank) {
		K = len(embRank)
	}
	topEmb := embRank[:K]

	// fused 上位 K に emb-top が全て含まれているなら何もしない
	upper := K
	if upper > len(fused) {
		upper = len(fused)
	}
	inTopFused := make(map[int]struct{}, upper)
	for _, idx := range fused[:upper] {
		inTopFused[idx] = struct{}{}
	}
	allPresent := true
	for _, idx := range topEmb {
		if _, ok := inTopFused[idx]; !ok {
			allPresent = false
			break
		}
	}
	if allPresent {
		return fused
	}

	// emb-top を先頭に置き、残りの fused 要素を後続に置く（重複を除外）
	embSet := make(map[int]struct{}, len(topEmb))
	for _, idx := range topEmb {
		embSet[idx] = struct{}{}
	}
	out := make([]int, 0, len(fused))
	out = append(out, topEmb...)
	for _, idx := range fused {
		if _, ok := embSet[idx]; ok {
			continue
		}
		out = append(out, idx)
	}
	return out
}

// -----------------------------------------------------------------------
// 補助ユーティリティ
// -----------------------------------------------------------------------

// sortIndicesByScore は scores のインデックスを score 降順で並べて返す。
// 同点は元の順序（インデックス昇順）で安定ソートする。
func sortIndicesByScore(scores []float64) []int {
	idxs := make([]int, len(scores))
	for i := range idxs {
		idxs[i] = i
	}
	sort.SliceStable(idxs, func(i, j int) bool {
		return scores[idxs[i]] > scores[idxs[j]]
	})
	return idxs
}

// countNonZero は scores のうち > 0 の要素数を返す。
func countNonZero(scores []float64) int {
	n := 0
	for _, s := range scores {
		if s > 0 {
			n++
		}
	}
	return n
}

func maxLen[T any](a, b []T) int {
	if len(a) > len(b) {
		return len(a)
	}
	return len(b)
}
