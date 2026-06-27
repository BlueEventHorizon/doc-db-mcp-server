// Package store — テキスト正規化・LEX-01 トークナイザ（DES-001 §6.2）
//
// 注: bm25_stats / bm25_df の pre-compute は v0.1.2 で廃止された。
// reference doc-db SKILL と同方式（query 時に substring match で TF/DF を都度計算）に揃えたため、
// 本ファイルでは正規化・トークナイザのみを提供する。
package store

import (
	"regexp"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// lexTokenRe は LEX-01 の優先マッチパターン（DES-001 §6.2）。
// 優先順位: ID パターン → ASCII 英数字 → CJK 等非 ASCII → 数字列
var lexTokenRe = regexp.MustCompile(
	`[A-Za-z]+-\d+` + // ID パターン（例: FNC-001）
		`|[A-Za-z0-9_]+` + // ASCII 英数字・アンダースコア
		`|[^\x00-\x7F]+` + // 連続 CJK 等非 ASCII（日本語グルーピング）
		`|\d+`, // 数字列
)

// Normalize はテキストに NFKC 正規化 + 小文字化を適用する（LEX-01）。
// 検索クエリと body 双方に同じ正規化を適用するために公開する。
func Normalize(text string) string {
	return strings.ToLower(norm.NFKC.String(text))
}

// Tokenize は LEX-01 に従いテキストをトークン列に変換する（DES-001 §6.2）。
func Tokenize(text string) []string {
	normalized := Normalize(text)
	matches := lexTokenRe.FindAllString(normalized, -1)
	result := matches[:0]
	for _, m := range matches {
		if m != "" {
			result = append(result, m)
		}
	}
	return result
}
