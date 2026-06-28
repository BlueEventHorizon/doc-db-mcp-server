// Package search — 全文 GREP signal (DES-001 §6.4 / APP-001 GRP-01/GRP-02 / PHIL-01)
//
// 設計思想: Embedding は意味類似空間で固有 ID や低頻度トークンを散らかしやすく、
// BM25 もトークナイザ境界で割れる場合がある。GREP は literal 一致のみを見るため、
// 上記 2 signal で取りこぼされる候補を確実に拾える。3 signal は互いに代替不能で
// 「異なる種類の取りこぼし」を埋め合う関係にある。
//
// アルゴリズム (DES-001 §6.4):
//  1. クエリを NFKC + lowercase で正規化
//  2. 全 chunk body を同じ正規化で前処理
//  3. クエリ文字列が body 内に出現する回数を score として返す
//  4. score = 0 のチャンクは「ヒットしなかった」扱い
package search

import (
	"strings"

	"github.com/BlueEventHorizon/doc-db-mcp-server/internal/store"
)

// computeGrepScores は GREP signal のスコアを各チャンクごとに返す。
// 戻り値スライスは chunks と同長で、scores[i] は chunks[i] の grep スコア。
// score = 0 はヒットなしを意味する。
//
// 意味類似は一切評価しない (GRP-02)。クエリは分割せず一塊の文字列として扱う。
func computeGrepScores(query string, chunks []store.Chunk) []float64 {
	scores := make([]float64, len(chunks))
	normQuery := strings.TrimSpace(store.Normalize(query))
	if normQuery == "" {
		return scores
	}
	for i, c := range chunks {
		body := store.Normalize(c.Text)
		cnt := strings.Count(body, normQuery)
		if cnt > 0 {
			scores[i] = float64(cnt)
		}
	}
	return scores
}
