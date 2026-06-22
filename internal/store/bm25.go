// Package store — BM25 トークナイザと upsert ヘルパー（DES-001 §6.2 / LEX-01）
package store

import (
	"context"
	"database/sql"
	"fmt"
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

// tokenize は LEX-01 に従いテキストをトークン列に変換する（DES-001 §6.2）。
// 1. NFKC 正規化 + 小文字化
// 2. 正規表現マッチでトークン分割
// 3. 空トークンを除外
func tokenize(text string) []string {
	normalized := strings.ToLower(norm.NFKC.String(text))
	matches := lexTokenRe.FindAllString(normalized, -1)
	result := matches[:0]
	for _, m := range matches {
		if m != "" {
			result = append(result, m)
		}
	}
	return result
}

// calcTF はトークン列からタームごとの TF（term frequency）を計算する。
// TF = タームの出現回数 / トークン総数。総数が 0 の場合は空 map を返す。
func calcTF(tokens []string) map[string]float64 {
	if len(tokens) == 0 {
		return nil
	}
	counts := make(map[string]int, len(tokens))
	for _, t := range tokens {
		counts[t]++
	}
	total := float64(len(tokens))
	tf := make(map[string]float64, len(counts))
	for term, cnt := range counts {
		tf[term] = float64(cnt) / total
	}
	return tf
}

// insertBM25StatsForChunk はチャンク 1 件分の bm25_stats と bm25_df を upsert する。
// bm25_stats はチャンク削除時に CASCADE で自動削除される。
// bm25_df は (key, term) 単位で DF を管理し、新規タームは INSERT、既存は df++ で更新する。
func insertBM25StatsForChunk(ctx context.Context, tx *sql.Tx, key string, chunkID int64, text string) error {
	tokens := tokenize(text)
	tf := calcTF(tokens)
	if len(tf) == 0 {
		return nil
	}

	for term, tfVal := range tf {
		// bm25_stats: チャンク単位の TF を記録
		if _, err := tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO bm25_stats (key, chunk_id, term, tf) VALUES (?, ?, ?, ?)`,
			key, chunkID, term, tfVal,
		); err != nil {
			return fmt.Errorf("store.insertBM25StatsForChunk: insert bm25_stats: %w", err)
		}

		// bm25_df: (key, term) 単位の DF をインクリメント
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO bm25_df (key, term, df) VALUES (?, ?, 1)
             ON CONFLICT(key, term) DO UPDATE SET df = df + 1`,
			key, term,
		); err != nil {
			return fmt.Errorf("store.insertBM25StatsForChunk: upsert bm25_df: %w", err)
		}
	}
	return nil
}
