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
	"log/slog"
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
	ModeGrep   Mode = "grep"   // 全文 GREP (literal 一致) のみ — DES-001 §6.4
	ModeHybrid Mode = "hybrid" // emb + lex の RRF 融合 (grep 含まず、legacy)
	ModeAll    Mode = "all"    // emb + lex + grep の 3 signal 並列 merge — PHIL-01 デフォルト
	ModeRerank Mode = "rerank" // all + LLM Rerank
)

// SearchResult は 1 件の検索結果。
type SearchResult struct {
	Path           string
	HeadingPath    string
	Text           string
	Score          float64
	ScoreBreakdown ScoreBreakdown
	SeriesKeys     []string
	// OriginSignals はこの chunk がヒットした signal のリスト (PHIL-01 / QRY-OUT-03)。
	// 例: ["emb", "grep"] — emb と grep の両方で見つかった
	// mode=all/rerank で複数 signal が並行実行される場合に意味を持つ。
	// 単一 signal モード (emb/lex/grep) では当該 signal のみが入る。
	OriginSignals []string
}

// ScoreBreakdown はスコアの内訳（クライアントに返す）。
type ScoreBreakdown struct {
	Emb    float64 `json:"emb"`
	Lex    float64 `json:"lex"`
	Grep   float64 `json:"grep"` // 全文 GREP signal score (出現回数)
	RRF    float64 `json:"rrf"`
	Rerank float64 `json:"rerank"`
}

// StageStats は各ステージを通過した候補数（QRY-OUT-02）。
type StageStats struct {
	EmbCandidates    int `json:"emb_candidates"`
	LexCandidates    int `json:"lex_candidates"`
	GrepCandidates   int `json:"grep_candidates"`   // GREP signal でヒットした候補数
	FusedCandidates  int `json:"fused_candidates"`  // emb+lex RRF 後の候補数 (ModeHybrid のみ)
	MergedCandidates int `json:"merged_candidates"` // 3 signal merge 後のユニーク候補数 (ModeAll/ModeRerank)
	RerankCandidates int `json:"rerank_candidates"`
}

// Output は Pipeline.Run の戻り値。
//
// Warnings: 致命的でない異常（Rerank API 失敗→RRF フォールバック、EMB フォールバック発動等）を
// caller (handler) に伝達する。silent failure 禁止方針 (no-silent-failure memory) に従い、
// 観測可能性をログ以外の経路でも担保する。
type Output struct {
	Results  []SearchResult
	Stats    StageStats
	Warnings []string
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
//
// 戻り値の scores は candidates と同じ長さで、scores[i] は candidates[i] に対する
// 関連度スコア (0..1)。呼び出し元は (-rerank_score, -original_score, chunk_id) の順で
// ブレンドソートする（reference doc-db SKILL llm_rerank.py と同方式）。
// 欠落 ID は -1.0 として扱う想定 → reference のように「末尾に追いやる」効果。
type Reranker interface {
	Rerank(ctx context.Context, query string, candidates []RerankCandidate) (scores []float64, err error)
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

	var warnings []string

	chunks, err := p.store.GetChunksForSearch(ctx, key, series)
	if err != nil {
		return Output{}, fmt.Errorf("search: load chunks: %w", err)
	}
	if len(chunks) == 0 {
		return Output{Results: nil, Stats: StageStats{}}, nil
	}

	// 各 signal の必要性を判定して並列計算する。
	// PHIL-01: mode=all/rerank では 3 signal を全て実行 (over-recall)。
	needEmb := mode == ModeEmb || mode == ModeHybrid || mode == ModeAll || mode == ModeRerank
	needLex := mode == ModeLex || mode == ModeHybrid || mode == ModeAll || mode == ModeRerank
	needGrep := mode == ModeGrep || mode == ModeAll || mode == ModeRerank

	var embScores, lexScores, grepScores []float64
	var embRank, lexRank, grepRank []int

	if needEmb {
		queryVec, err := p.embedQuery(ctx, query)
		if err != nil {
			return Output{}, fmt.Errorf("search: embed query: %w", err)
		}
		embScores = computeCosineScores(queryVec, chunks)
		embRank = sortIndicesByScore(embScores)
	}
	if needLex {
		lexScores = computeLexScores(query, chunks, p.cfg.K1, p.cfg.B)
		lexRank = sortIndicesByScore(lexScores)
	}
	if needGrep {
		grepScores = computeGrepScores(query, chunks)
		grepRank = sortIndicesByScore(grepScores)
	}

	// ステージ統計
	stats := StageStats{
		EmbCandidates:  countNonZero(embScores),
		LexCandidates:  countNonZero(lexScores),
		GrepCandidates: countNonZero(grepScores),
	}

	// 各 chunk index → origin signals のマップ (PHIL-01 / QRY-OUT-03)。
	// 単一 signal モードでは当該 signal のみ、merge モードでは複数 signal が記録される。
	originSignals := make(map[int][]string)

	// 融合 / 単一モード選択
	var fusedOrder []int // chunks 内インデックスを順位付き降順で並べたもの
	var rrfScores []float64
	switch mode {
	case ModeEmb:
		// reference doc-db SKILL に合わせ、emb は全 chunk を cos 降順で返す (negative も含む)。
		// cos<=0 もユーザーが「emb モード」を明示している以上は判断材料として残す。
		fusedOrder = embRank
		for _, idx := range fusedOrder {
			originSignals[idx] = []string{"emb"}
		}
	case ModeLex:
		// lex は score>0 のみ (reference lexical_search.py:84)。
		fusedOrder = filterPositiveRank(lexRank, lexScores)
		for _, idx := range fusedOrder {
			originSignals[idx] = []string{"lex"}
		}
	case ModeGrep:
		// grep は score>0 のみ (literal 一致が無い chunk を返す意味はない)。
		fusedOrder = filterPositiveRank(grepRank, grepScores)
		for _, idx := range fusedOrder {
			originSignals[idx] = []string{"grep"}
		}
	case ModeHybrid:
		var embFallback bool
		fusedOrder, rrfScores, embFallback = fuseScores(embRank, lexRank, embScores, lexScores, p.cfg)
		if embFallback {
			warnings = append(warnings, fmt.Sprintf("emb fallback triggered (lex_hits=%d / emb_hits=%d < %.2f)",
				countNonZero(lexScores), countNonZero(embScores), p.cfg.EmbFallbackLexRatio))
		}
		stats.FusedCandidates = len(fusedOrder)
		// hybrid では emb と lex の組み合わせ。各 chunk の score 内訳から signals を導出。
		for _, idx := range fusedOrder {
			var sigs []string
			if idx < len(embScores) && embScores[idx] > 0 {
				sigs = append(sigs, "emb")
			}
			if idx < len(lexScores) && lexScores[idx] > 0 {
				sigs = append(sigs, "lex")
			}
			originSignals[idx] = sigs
		}
	case ModeAll, ModeRerank:
		// PHIL-01: 3 signal merge。各 signal の top max(topN, MAX) を合算 + grep 全件。
		fusedOrder, originSignals = mergeThreeSignals(
			embRank, lexRank, grepRank,
			embScores, lexScores, grepScores,
			topN, maxRerankCandidates,
		)
		stats.MergedCandidates = len(fusedOrder)
	default:
		return Output{}, fmt.Errorf("search: unknown mode %q", mode)
	}

	// Rerank（mode=rerank のみ）
	// reranker が未注入、または API 呼び出しが失敗した場合は RRF 順をそのまま使う（RR-02）。
	// reference doc-db SKILL と同方式:
	//   - 候補は top-N (= min(MAX, len(fused), top_n * factor)) を採用
	//   - LLM が返す score 0..1 と元 RRF/emb スコアを (-rerank, -orig, idx) でブレンドソート
	//   - 欠落 ID の rerank_score = -1.0 → 末尾扱い
	rerankApplied := false
	rerankScoreMap := make(map[int]float64) // chunk index -> rerank score
	if mode == ModeRerank && p.reranker != nil {
		nCand := chooseRerankCandidateCount(len(fusedOrder), topN, p.cfg.RerankFactor)
		topCandidates := fusedOrder[:nCand]

		scores, err := p.rerank(ctx, query, chunks, topCandidates)
		if err != nil {
			slog.Warn("rerank: API 呼び出し失敗、RRF 順にフォールバック (RR-02)",
				"error", err, "candidates", len(topCandidates), "query", query)
			warnings = append(warnings, fmt.Sprintf("rerank fallback to RRF: %v", err))
		} else if len(scores) != len(topCandidates) {
			slog.Warn("rerank: scores 件数不一致、RRF 順にフォールバック",
				"got", len(scores), "want", len(topCandidates), "query", query)
			warnings = append(warnings, fmt.Sprintf("rerank fallback: scores length mismatch (got %d, want %d)", len(scores), len(topCandidates)))
		}
		if err == nil && len(scores) == len(topCandidates) {
			// rerank スコアを chunk index にマッピング
			for i, ci := range topCandidates {
				rerankScoreMap[ci] = scores[i]
			}
			// 元の RRF / emb スコアを取得する補助関数
			origScore := func(idx int) float64 {
				if len(rrfScores) > 0 {
					return rrfScores[idx]
				}
				if len(embScores) > 0 {
					return embScores[idx]
				}
				return 0
			}
			// reranked = top候補を (-rerank, -orig, idx) でソート
			rerankedTop := make([]int, len(topCandidates))
			copy(rerankedTop, topCandidates)
			sort.SliceStable(rerankedTop, func(i, j int) bool {
				ri, rj := rerankScoreMap[rerankedTop[i]], rerankScoreMap[rerankedTop[j]]
				if ri != rj {
					return ri > rj
				}
				oi, oj := origScore(rerankedTop[i]), origScore(rerankedTop[j])
				if oi != oj {
					return oi > oj
				}
				return rerankedTop[i] < rerankedTop[j]
			})
			fusedOrder = append(rerankedTop, fusedOrder[nCand:]...)
			rerankApplied = true
			stats.RerankCandidates = len(topCandidates)
		}
		// 失敗 / 件数不一致時は RerankCandidates=0 のままで RRF 順をそのまま使う
	}

	// SearchResult を構築（上位 topN 件）
	resultCount := topN
	if resultCount > len(fusedOrder) {
		resultCount = len(fusedOrder)
	}
	results := make([]SearchResult, 0, resultCount)
	for _, idx := range fusedOrder[:resultCount] {
		c := chunks[idx]
		var (
			embS, lexS, grepS, rrfS, rerankS float64
		)
		if embScores != nil {
			embS = embScores[idx]
		}
		if lexScores != nil {
			lexS = lexScores[idx]
		}
		if grepScores != nil {
			grepS = grepScores[idx]
		}
		if rrfScores != nil {
			rrfS = rrfScores[idx]
		}
		if rerankApplied {
			// Rerank が適用された chunk は LLM の返した 0..1 スコアを記録。
			// rerank に渡されなかった末尾候補は 0 のまま。
			if v, ok := rerankScoreMap[idx]; ok {
				rerankS = v
			}
		}
		results = append(results, SearchResult{
			Path:        c.Path,
			HeadingPath: c.HeadingPath,
			Text:        c.Text,
			Score:       primaryScore(mode, embS, lexS, grepS, rrfS, rerankS),
			OriginSignals: originSignals[idx],
			ScoreBreakdown: ScoreBreakdown{
				Emb:    embS,
				Lex:    lexS,
				Grep:   grepS,
				RRF:    rrfS,
				Rerank: rerankS,
			},
			SeriesKeys: c.SeriesKeys,
		})
	}

	return Output{Results: results, Stats: stats, Warnings: warnings}, nil
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

// rerank は LLM Reranker を呼び、candidates と同じ長さの score slice を返す。
// scores[i] は cands[i] (= chunks[idxs[i]]) に対する LLM の関連度スコア。
func (p *Pipeline) rerank(ctx context.Context, query string, chunks []store.Chunk, idxs []int) ([]float64, error) {
	cands := make([]RerankCandidate, len(idxs))
	for i, ci := range idxs {
		cands[i] = RerankCandidate{Index: ci, Text: chunks[ci].Text, HeadingPath: chunks[ci].HeadingPath}
	}
	scores, err := p.reranker.Rerank(ctx, query, cands)
	if err != nil {
		return nil, err
	}
	if len(scores) != len(cands) {
		return nil, fmt.Errorf("rerank: scores length %d != candidates %d", len(scores), len(cands))
	}
	return scores, nil
}

// rerank 候補数を reference doc-db SKILL (search_index.py:232) と同方針で決める。
//
// reference は fused から `max(top_n, MAX_CANDIDATES=30)` 件を切り出して全部 LLM に渡す。
// top_n が小さくても常に最大 30 候補まで広く LLM に評価させる方針で、言い換えクエリで
// emb 上位に正解が無い場合でも救えるよう設計されている。
//
// 上限 (MAX=30) は context window budget から逆算した安全値（gpt-4o-mini 128k window）。
// 大規模化時の budget チェックは省略（138 chunks 規模では到達しない）。
const (
	minRerankCandidates = 5
	maxRerankCandidates = 30
)

func chooseRerankCandidateCount(fusedLen, topN, factor int) int {
	_ = factor // reference は factor を使わず max(top_n, MAX) で決定する
	if fusedLen <= 0 {
		return 0
	}
	if fusedLen <= minRerankCandidates {
		return fusedLen
	}
	// preferred = max(top_n, MAX_CANDIDATES)
	preferred := topN
	if maxRerankCandidates > preferred {
		preferred = maxRerankCandidates
	}
	// 上限は fusedLen
	if preferred > fusedLen {
		preferred = fusedLen
	}
	return preferred
}

// primaryScore は表示用の代表スコアを選ぶ。
// rerank が適用された候補は LLM の score を優先。それ以外は mode に応じた signal score。
func primaryScore(mode Mode, emb, lex, grep, rrf, rerank float64) float64 {
	if rerank > 0 {
		return rerank
	}
	switch mode {
	case ModeEmb:
		return emb
	case ModeLex:
		return lex
	case ModeGrep:
		return grep
	case ModeAll:
		// mode=all では複数 signal の最大値を代表として返す
		// (PHIL-01 の二層アーキで「強い signal で見つかった」順位付けの目安)
		s := emb
		if lex > s {
			s = lex
		}
		if grep > s {
			s = grep
		}
		return s
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
// reference doc-db SKILL (lexical_search.py) と同一アルゴリズム:
//
//   - TF は **substring match**（normalized body 内の token 出現回数）
//   - DF も substring match（token を含む body 数）
//   - dl / avgdl は **文字数**（rune 単位ではなく len() 互換のバイト数 — Python str と同等）
//   - IDF は Robertson 形式: log((N - df + 0.5) / (df + 0.5) + 1)
//
// ボーナス:
//   - クエリ内 ID パターン（[A-Z]+-\d+）が body に含まれる → +10
//   - 正規化クエリ全体が body に含まれる → +2
//
// substring 方式は形態素解析器なしで CJK の連続文字列を部分マッチでき、
// 「廃棄ポリシー」というクエリで token が「廃棄」「ポリシー」「廃棄ポリシー」
// と複数粒度で TF を稼げる（reference の評価で実用品質が確認されている）。
func computeLexScores(query string, chunks []store.Chunk, k1, b float64) []float64 {
	scores := make([]float64, len(chunks))

	normalizedQuery := store.Normalize(query)
	queryTerms := store.Tokenize(query)
	if len(queryTerms) == 0 {
		return scores
	}
	uniqQueryTerms := uniqueStrings(queryTerms)

	// 各 chunk の正規化済み body を事前計算（reference: normalized_bodies）
	normBodies := make([]string, len(chunks))
	for i, c := range chunks {
		normBodies[i] = store.Normalize(c.Text)
	}

	// 文書長 = body の文字数（Python の len(str) 相当 = 内部 UTF-16 単位だが
	// reference は CJK で安定動作するので、ここでは UTF-8 バイト数で代用しても
	// 比例関係が保たれる。厳密に揃えるため rune 単位で計測する）
	docLens := make([]int, len(chunks))
	var totalLen int
	for i, b := range normBodies {
		l := 0
		for range b {
			l++
		}
		docLens[i] = l
		totalLen += l
	}
	N := float64(len(chunks))
	avgDocLen := 1.0
	if N > 0 {
		avgDocLen = float64(totalLen) / N
	}

	// IDF: substring DF を unique query token ごとに計算
	idf := make(map[string]float64, len(uniqQueryTerms))
	for _, t := range uniqQueryTerms {
		df := 0
		for _, body := range normBodies {
			if strings.Contains(body, t) {
				df++
			}
		}
		idf[t] = math.Log((N-float64(df)+0.5)/(float64(df)+0.5) + 1.0)
	}

	// ID パターンは大文字版で抽出して lowercase の body と比較する
	idTokensUpper := idBonusPattern.FindAllString(strings.ToUpper(normalizedQuery), -1)

	for i := range chunks {
		body := normBodies[i]
		dl := float64(docLens[i])
		var score float64

		for _, t := range queryTerms {
			tf := float64(strings.Count(body, t))
			if tf == 0 {
				continue
			}
			tfNorm := tf * (k1 + 1) / (tf + k1*(1-b+b*dl/avgDocLen))
			score += idf[t] * tfNorm
		}

		// ID パターンボーナス（lowercase で比較）
		for _, m := range idTokensUpper {
			if strings.Contains(body, strings.ToLower(m)) {
				score += 10.0
			}
		}
		// 全文一致ボーナス
		if normalizedQuery != "" && strings.Contains(body, normalizedQuery) {
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

// fuseScores は emb / lex の順位を RRF で融合する（reference doc-db SKILL hybrid_score.rrf_fuse と同方式）。
//
//   - EMB フォールバック: lex_score > 0 の chunk 数 / emb_score > 0 の chunk 数 が
//     EMB_FALLBACK_LEX_RATIO (0.05) 未満なら emb only モード
//   - RRF 計算では lex_score > 0 の chunk のみ lex_rank に含める
//   - EMB top-K 保証を適用（スコア書き換え昇格方式）
//
// 戻り値:
//   - order: chunks インデックスを RRF スコア降順に並べた配列
//   - rrfScores: chunks インデックスに対応する RRF スコア
//   - embFallback: EMB フォールバックが発動した場合 true（caller が warnings に記録する用）
func fuseScores(embRank, lexRank []int, embScores, lexScores []float64, cfg Config) ([]int, []float64, bool) {
	embHits := countNonZero(embScores)
	lexHits := countNonZero(lexScores)

	// EMB フォールバック判定（reference hybrid_score.py:31 と同方式）
	if embHits > 0 {
		ratio := float64(lexHits) / float64(embHits)
		if ratio < cfg.EmbFallbackLexRatio {
			// EMB スコア降順のみで返す
			scores := make([]float64, len(embScores))
			for _, idx := range embRank {
				scores[idx] = embScores[idx]
			}
			return embRank, scores, true
		}
	}

	// lex_rank を lex_score > 0 のものだけにフィルタする
	// （reference の _to_rank_map は score > 0 のみが渡される lex_items を rank 化）
	filteredLexRank := make([]int, 0, lexHits)
	for _, idx := range lexRank {
		if idx >= 0 && idx < len(lexScores) && lexScores[idx] > 0 {
			filteredLexRank = append(filteredLexRank, idx)
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
	rankInScores(filteredLexRank)

	order := sortIndicesByScore(rrf)

	// EMB top-K 保証: emb 上位 K 件が fused 上位 K に含まれるよう、必要な分だけ
	// スコアを書き換えて昇格させる（reference doc-db SKILL と同方式）。
	rrf = promoteEmbTopKByScore(order, rrf, embRank, cfg.EmbGuaranteeK)
	order = sortIndicesByScore(rrf)

	return order, rrf, false
}

// promoteEmbTopKByScore は EMB 上位 K 件のスコアを「侵入者 (= emb top-K に
// 含まれない fused 上位) の最高スコアを超える値」に書き換えて昇格させる。
// emb 内の相対順位を保つため、rank に応じた微小オフセット (1e-9 単位) を加える。
// reference doc-db SKILL (hybrid_score.py:49-66) と同等。
func promoteEmbTopKByScore(fusedOrder []int, rrfScores []float64, embRank []int, K int) []float64 {
	if K <= 0 || len(embRank) == 0 || len(fusedOrder) == 0 {
		return rrfScores
	}
	if K > len(embRank) {
		K = len(embRank)
	}
	topEmb := embRank[:K]
	topEmbSet := make(map[int]struct{}, len(topEmb))
	for _, idx := range topEmb {
		topEmbSet[idx] = struct{}{}
	}

	upper := K
	if upper > len(fusedOrder) {
		upper = len(fusedOrder)
	}

	// fused 上位 K のうち emb-top に含まれない侵入者を抽出
	var intruders []int
	for _, idx := range fusedOrder[:upper] {
		if _, ok := topEmbSet[idx]; !ok {
			intruders = append(intruders, idx)
		}
	}
	if len(intruders) == 0 {
		return rrfScores
	}

	// 侵入者の最高スコアを取得
	threshold := rrfScores[intruders[0]]
	for _, idx := range intruders[1:] {
		if rrfScores[idx] > threshold {
			threshold = rrfScores[idx]
		}
	}

	// emb-top のうち fused 上位 K に未到達の chunk のスコアを書き換える
	inTopFused := make(map[int]struct{}, upper)
	for _, idx := range fusedOrder[:upper] {
		inTopFused[idx] = struct{}{}
	}
	for rankIdx, idx := range topEmb {
		if _, ok := inTopFused[idx]; ok {
			continue // 既に fused 上位にいる
		}
		// rank_idx に応じた微小オフセットを加えて emb 内の相対順位を保つ
		rrfScores[idx] = threshold + float64(K-rankIdx)*1e-9
	}
	return rrfScores
}

// promoteEmbTopK は legacy 実装（後方互換用に残す。新コードからは呼ばない）。
// 戦略: emb-top を先頭にコピーし、残りの fused 要素を後続に連結する。
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

// filterPositiveRank は rank 配列を score > 0 の chunk のみに絞り込む。
// 単一 signal モード (emb / lex / grep) で「ヒットしなかった chunk」を結果から除外する用途。
func filterPositiveRank(rank []int, scores []float64) []int {
	out := make([]int, 0, len(rank))
	for _, idx := range rank {
		if idx >= 0 && idx < len(scores) && scores[idx] > 0 {
			out = append(out, idx)
		}
	}
	return out
}

// mergeThreeSignals は emb / lex / grep の 3 signal を合算した候補プールを返す
// （DES-001 §6.5 / PHIL-01）。
//
// 戦略:
//   - emb と lex は top max(topN, perSignalCap) 件を採用（recall を広げる）
//   - grep はヒット全件採用（literal 一致は元々件数が少ないため絞らない）
//   - chunk index 単位で重複を排除し、各 chunk の origin_signals を記録
//   - ソートキー: (signal hit 数 降順, emb_score 降順, chunk index 昇順)
//     → 複数 signal で見つかった chunk を上位に集約
//
// 戻り値:
//   - order: 合算後の chunk index リスト (上位ほど multi-signal hit & 高 emb_score)
//   - signals: chunk index → origin_signals マップ
func mergeThreeSignals(
	embRank, lexRank, grepRank []int,
	embScores, lexScores, grepScores []float64,
	topN, perSignalCap int,
) ([]int, map[int][]string) {
	cap := perSignalCap
	if topN > cap {
		cap = topN
	}

	signals := make(map[int][]string)
	addSignal := func(idx int, sig string) {
		signals[idx] = append(signals[idx], sig)
	}

	// emb top K (score > 0 のみ)
	{
		n := 0
		for _, idx := range embRank {
			if idx < 0 || idx >= len(embScores) || embScores[idx] <= 0 {
				continue
			}
			addSignal(idx, "emb")
			n++
			if n >= cap {
				break
			}
		}
	}
	// lex top K (score > 0 のみ)
	{
		n := 0
		for _, idx := range lexRank {
			if idx < 0 || idx >= len(lexScores) || lexScores[idx] <= 0 {
				continue
			}
			addSignal(idx, "lex")
			n++
			if n >= cap {
				break
			}
		}
	}
	// grep 全件 (score > 0)。reference doc-db SKILL の方針: literal 一致は希少なため全件採用。
	for _, idx := range grepRank {
		if idx < 0 || idx >= len(grepScores) || grepScores[idx] <= 0 {
			continue
		}
		addSignal(idx, "grep")
	}

	// 順序付き出力 (signal count, emb_score, index でソート)
	order := make([]int, 0, len(signals))
	for idx := range signals {
		order = append(order, idx)
	}
	sort.SliceStable(order, func(i, j int) bool {
		ci, cj := len(signals[order[i]]), len(signals[order[j]])
		if ci != cj {
			return ci > cj
		}
		ei, ej := 0.0, 0.0
		if order[i] < len(embScores) {
			ei = embScores[order[i]]
		}
		if order[j] < len(embScores) {
			ej = embScores[order[j]]
		}
		if ei != ej {
			return ei > ej
		}
		return order[i] < order[j]
	})
	return order, signals
}
