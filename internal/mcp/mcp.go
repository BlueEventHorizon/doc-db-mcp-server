// Package mcp は MCP ツールハンドラ（upsert / delete / query / manage 系）を実装する。
// DES-001 §3.2 / §5: store / chunker / embedder / fetcher / search を結線する。
//
// DIF-02 経路の実装方針（DES-001 §4.2, §4.3）:
// UpsertHandler で AppendSeries + CleanOtherSeries を個別に呼ぶと、
// 2 呼び出し間で Mutex が外れて競合が発生する。
// 必ず store.AppendAndCleanSeries を使用すること。
package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/BlueEventHorizon/doc-db-mcp-server/internal/chunker"
	"github.com/BlueEventHorizon/doc-db-mcp-server/internal/embedder"
	"github.com/BlueEventHorizon/doc-db-mcp-server/internal/fetcher"
	"github.com/BlueEventHorizon/doc-db-mcp-server/internal/search"
	"github.com/BlueEventHorizon/doc-db-mcp-server/internal/store"
)

// -----------------------------------------------------------------------
// Handlers — 依存集約
// -----------------------------------------------------------------------

// Handlers は MCP ツール群が共有する依存性。
type Handlers struct {
	store    *store.Store
	chunker  *chunker.Chunker
	embedder embedder.Embedder
	fetcher  fetcher.Fetcher
	search   *search.Pipeline
}

// New は Handlers を初期化する。
func New(
	st *store.Store,
	ch *chunker.Chunker,
	emb embedder.Embedder,
	fe fetcher.Fetcher,
	sp *search.Pipeline,
) *Handlers {
	return &Handlers{
		store:    st,
		chunker:  ch,
		embedder: emb,
		fetcher:  fe,
		search:   sp,
	}
}

// Register は MCP ツール 6 種を MCP サーバーに登録する（FNC-001/002/003/004）。
func (h *Handlers) Register(s *mcpsdk.Server) {
	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:        "upsert_documents",
		Description: "ドキュメント群を指定 KEY のインデックスに追加・更新する（FNC-001）",
	}, h.handleUpsert)

	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:        "delete_documents",
		Description: "指定 KEY+series から特定 path のドキュメントを削除する（FNC-002）",
	}, h.handleDelete)

	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:        "query",
		Description: "自然言語クエリでドキュメントを検索する（FNC-003）",
	}, h.handleQuery)

	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:        "list_indexes",
		Description: "登録済み KEY の一覧を返す（MNG-01）",
	}, h.handleListIndexes)

	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:        "delete_index",
		Description: "指定 KEY のインデックスを完全削除する（MNG-02）",
	}, h.handleDeleteIndex)

	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:        "manage_index",
		Description: "指定 KEY の廃棄ポリシー（TTL / max_chunks）を設定・更新する（EXP-04 / MNG-03）",
	}, h.handleManageIndex)
}

// -----------------------------------------------------------------------
// upsert_documents (FNC-001)
// -----------------------------------------------------------------------

// UpsertDocument は upsert_documents の document 1 件分入力（content/url 排他）。
type UpsertDocument struct {
	Path    string `json:"path"`
	Content string `json:"content,omitempty"`
	URL     string `json:"url,omitempty"`
	Hash    string `json:"hash,omitempty"`
}

// UpsertInput は upsert_documents の入力。
type UpsertInput struct {
	Key       string           `json:"key"`
	Series    string           `json:"series"`
	Documents []UpsertDocument `json:"documents"`
}

// UpsertError は失敗・スキップドキュメントの詳細（UPS-OUT-01）。
type UpsertError struct {
	Path  string `json:"path"`
	Error string `json:"error"`
	// SkippedChunks は Embedding 失敗で保存できなかったチャンクのインデックス（M2 / UPS-OUT-02）。
	SkippedChunks []int `json:"skipped_chunks,omitempty"`
}

// UpsertResult は upsert_documents の出力（UPS-OUT-01）。
type UpsertResult struct {
	Processed int           `json:"processed"`
	Skipped   int           `json:"skipped"`
	Failed    int           `json:"failed"`
	Errors    []UpsertError `json:"errors,omitempty"`
}

func (h *Handlers) handleUpsert(
	ctx context.Context, _ *mcpsdk.CallToolRequest, in UpsertInput,
) (*mcpsdk.CallToolResult, UpsertResult, error) {
	if in.Key == "" || in.Series == "" {
		return nil, UpsertResult{}, errors.New("key と series は必須")
	}
	if len(in.Documents) == 0 {
		return nil, UpsertResult{}, errors.New("documents が空")
	}

	result := UpsertResult{}
	for _, doc := range in.Documents {
		if err := h.upsertOne(ctx, in.Key, in.Series, doc, &result); err != nil {
			slog.Warn("upsert: document failed", "path", doc.Path, "error", err)
			continue
		}
	}
	return nil, result, nil
}

// upsertOne は 1 ドキュメント分の upsert を実行する。
// 失敗時は result.Errors に追加し、戻り値の error にも同じ内容を返す（呼出元はログ用途）。
func (h *Handlers) upsertOne(
	ctx context.Context, key, series string, doc UpsertDocument, result *UpsertResult,
) error {
	if doc.Path == "" {
		err := errors.New("path is required")
		result.Failed++
		result.Errors = append(result.Errors, UpsertError{Path: doc.Path, Error: err.Error()})
		return err
	}
	if (doc.Content == "" && doc.URL == "") || (doc.Content != "" && doc.URL != "") {
		err := errors.New("content と url のいずれか一方を指定")
		result.Failed++
		result.Errors = append(result.Errors, UpsertError{Path: doc.Path, Error: err.Error()})
		return err
	}

	// 1. content 取得
	content := doc.Content
	if doc.URL != "" {
		var err error
		content, err = h.fetcher.Fetch(ctx, doc.URL)
		if err != nil {
			result.Failed++
			result.Errors = append(result.Errors, UpsertError{Path: doc.Path, Error: "fetch: " + err.Error()})
			return err
		}
	}

	// 2. 正規化 + SHA-256 算出（M1）
	normalized := normalizeContent(content)
	hash := sha256hex([]byte(normalized))

	// 3. 同一ハッシュ既存 record の確認（DIF-02 経路）
	existing, err := h.store.FindRecord(ctx, key, doc.Path, hash)
	if err != nil {
		result.Failed++
		result.Errors = append(result.Errors, UpsertError{Path: doc.Path, Error: "find: " + err.Error()})
		return err
	}
	if existing != 0 {
		// 同一内容 → series 追記 + 他 record の同 series を除去（DIF-02）
		if err := h.store.AppendAndCleanSeries(ctx, existing, key, doc.Path, series); err != nil {
			result.Failed++
			result.Errors = append(result.Errors, UpsertError{Path: doc.Path, Error: "append series: " + err.Error()})
			return err
		}
		result.Skipped++
		return nil
	}

	// 4. 新規 or 内容変更 → Chunker + Embedder（DIF-03 経路）
	chunks, err := h.chunker.Split(doc.Path, normalized)
	if err != nil {
		result.Failed++
		result.Errors = append(result.Errors, UpsertError{Path: doc.Path, Error: "chunk: " + err.Error()})
		return err
	}
	// Embedding API には EmbedText（heading breadcrumb + prose）を渡す。
	// EmbedText が空の場合のみ Text にフォールバック（reference doc-db SKILL と同方式）。
	texts := make([]string, len(chunks))
	for i, c := range chunks {
		if c.EmbedText != "" {
			texts[i] = c.EmbedText
		} else {
			texts[i] = c.Text
		}
	}

	vecs, skipped, embErr := h.embedder.Embed(ctx, texts)

	// 部分失敗（M2）: 成功チャンクのみ vector 付きで保存。失敗チャンクは vector=nil で保存
	// （テキスト検索だけは行えるようにする）。
	chunkInputs := make([]store.ChunkInput, 0, len(chunks))
	for i, c := range chunks {
		var vec []float32
		if i < len(vecs) && vecs[i] != nil {
			vec = []float32(vecs[i])
		}
		chunkInputs = append(chunkInputs, store.ChunkInput{
			ChunkIndex:  c.ChunkIndex,
			HeadingPath: c.HeadingPath,
			Text:        c.Text,
			Vector:      vec,
		})
	}

	// 5. UpsertRecord + CleanOtherSeries（DIF-03）
	recordID, err := h.store.UpsertRecord(ctx, store.Record{
		Key:         key,
		Path:        doc.Path,
		ContentHash: hash,
		Series:      series,
		Chunks:      chunkInputs,
	})
	if err != nil {
		result.Failed++
		result.Errors = append(result.Errors, UpsertError{Path: doc.Path, Error: "upsert: " + err.Error()})
		return err
	}
	if err := h.store.CleanOtherSeries(ctx, key, doc.Path, series, recordID); err != nil {
		// CleanOtherSeries 失敗は致命ではない（UpsertRecord は既に成功）が、警告情報として返す
		result.Errors = append(result.Errors, UpsertError{Path: doc.Path, Error: "clean other series: " + err.Error()})
	}

	result.Processed++
	if embErr != nil || len(skipped) > 0 {
		result.Errors = append(result.Errors, UpsertError{
			Path:          doc.Path,
			Error:         embErrMessage(embErr),
			SkippedChunks: skipped,
		})
	}
	return nil
}

func embErrMessage(err error) string {
	if err == nil {
		return "partial embedding failure"
	}
	return "embed: " + err.Error()
}

// -----------------------------------------------------------------------
// delete_documents (FNC-002)
// -----------------------------------------------------------------------

// DeleteInput は delete_documents の入力。
type DeleteInput struct {
	Key    string   `json:"key"`
	Series string   `json:"series"`
	Paths  []string `json:"paths"`
}

// DeleteResult は delete_documents の出力。
type DeleteResult struct {
	Deleted  int      `json:"deleted"`
	Warnings []string `json:"warnings,omitempty"`
}

func (h *Handlers) handleDelete(
	ctx context.Context, _ *mcpsdk.CallToolRequest, in DeleteInput,
) (*mcpsdk.CallToolResult, DeleteResult, error) {
	if in.Key == "" || in.Series == "" || len(in.Paths) == 0 {
		return nil, DeleteResult{}, errors.New("key / series / paths は必須")
	}

	// 事前に存在チェックして warning を構築（DEL-02）
	var warnings []string
	existing := make([]string, 0, len(in.Paths))
	for _, p := range in.Paths {
		found, err := h.store.HasRecord(ctx, in.Key, p)
		if err != nil {
			return nil, DeleteResult{}, fmt.Errorf("check path %q: %w", p, err)
		}
		if !found {
			warnings = append(warnings, fmt.Sprintf("path %q は存在しないためスキップ", p))
			continue
		}
		existing = append(existing, p)
	}

	if len(existing) > 0 {
		if err := h.store.DeleteSeries(ctx, in.Key, in.Series, existing); err != nil {
			return nil, DeleteResult{}, fmt.Errorf("delete: %w", err)
		}
	}

	return nil, DeleteResult{Deleted: len(existing), Warnings: warnings}, nil
}

// -----------------------------------------------------------------------
// query (FNC-003)
// -----------------------------------------------------------------------

// QueryInput は query の入力。
type QueryInput struct {
	Query  string `json:"query"`
	Key    string `json:"key"`
	Series string `json:"series,omitempty"`
	Mode   string `json:"mode,omitempty"`  // emb / lex / hybrid / rerank (default: rerank)
	TopN   int    `json:"top_n,omitempty"` // default 10
}

// QueryHit は検索結果 1 件（QRY-OUT-01）。
type QueryHit struct {
	Path           string                `json:"path"`
	HeadingPath    string                `json:"heading_path"`
	Text           string                `json:"text"`
	Score          float64               `json:"score"`
	ScoreBreakdown search.ScoreBreakdown `json:"score_breakdown"`
	SeriesKeys     []string              `json:"series_keys"`
}

// QueryResult は query の出力（QRY-OUT-02）。
//
// Warnings: 致命的でない異常（TouchKey 失敗・Rerank フォールバック等）を caller に伝達する。
// silent failure 禁止方針に従い、log にしか出ない情報は警告メッセージとして含める。
type QueryResult struct {
	Results    []QueryHit        `json:"results"`
	StageStats search.StageStats `json:"stage_stats"`
	Warnings   []string          `json:"warnings,omitempty"`
}

func (h *Handlers) handleQuery(
	ctx context.Context, _ *mcpsdk.CallToolRequest, in QueryInput,
) (*mcpsdk.CallToolResult, QueryResult, error) {
	if in.Query == "" || in.Key == "" {
		return nil, QueryResult{}, errors.New("query と key は必須")
	}
	mode := search.Mode(in.Mode)
	if mode == "" {
		mode = search.ModeRerank
	}
	if in.TopN <= 0 {
		in.TopN = 10
	}

	// KEY 存在確認
	exists, err := h.store.KeyExists(ctx, in.Key)
	if err != nil {
		return nil, QueryResult{}, fmt.Errorf("key check: %w", err)
	}
	if !exists {
		return nil, QueryResult{}, fmt.Errorf("key %q が存在しません", in.Key)
	}

	var warnings []string

	// TouchKey（last_accessed_at 更新）。致命的ではないが caller に観測可能化する。
	if err := h.store.TouchKey(ctx, in.Key); err != nil {
		slog.Warn("query: TouchKey failed", "key", in.Key, "error", err)
		warnings = append(warnings, fmt.Sprintf("TouchKey failed for key %q: %v", in.Key, err))
	}

	out, err := h.search.Run(ctx, in.Key, in.Series, in.Query, mode, in.TopN)
	if err != nil {
		return nil, QueryResult{}, fmt.Errorf("search: %w", err)
	}

	// search.Pipeline が記録した Rerank フォールバック等の警告を取り込む
	if out.Warnings != nil {
		warnings = append(warnings, out.Warnings...)
	}

	hits := make([]QueryHit, len(out.Results))
	for i, r := range out.Results {
		hits[i] = QueryHit{
			Path:           r.Path,
			HeadingPath:    r.HeadingPath,
			Text:           r.Text,
			Score:          r.Score,
			ScoreBreakdown: r.ScoreBreakdown,
			SeriesKeys:     r.SeriesKeys,
		}
	}
	return nil, QueryResult{Results: hits, StageStats: out.Stats, Warnings: warnings}, nil
}

// -----------------------------------------------------------------------
// list_indexes (MNG-01)
// -----------------------------------------------------------------------

// ListIndexesInput は list_indexes の入力（パラメータなし）。
type ListIndexesInput struct{}

// ListIndexesResult は list_indexes の出力。
type ListIndexesResult struct {
	Indexes []store.KeyInfo `json:"indexes"`
}

func (h *Handlers) handleListIndexes(
	ctx context.Context, _ *mcpsdk.CallToolRequest, _ ListIndexesInput,
) (*mcpsdk.CallToolResult, ListIndexesResult, error) {
	keys, err := h.store.ListKeys(ctx)
	if err != nil {
		return nil, ListIndexesResult{}, fmt.Errorf("list keys: %w", err)
	}
	return nil, ListIndexesResult{Indexes: keys}, nil
}

// -----------------------------------------------------------------------
// delete_index (MNG-02)
// -----------------------------------------------------------------------

// DeleteIndexInput は delete_index の入力。
type DeleteIndexInput struct {
	Key string `json:"key"`
}

// DeleteIndexResult は delete_index の出力。
type DeleteIndexResult struct {
	Deleted bool `json:"deleted"`
}

func (h *Handlers) handleDeleteIndex(
	ctx context.Context, _ *mcpsdk.CallToolRequest, in DeleteIndexInput,
) (*mcpsdk.CallToolResult, DeleteIndexResult, error) {
	if in.Key == "" {
		return nil, DeleteIndexResult{}, errors.New("key は必須")
	}
	if err := h.store.DeleteKey(ctx, in.Key); err != nil {
		return nil, DeleteIndexResult{}, fmt.Errorf("delete key: %w", err)
	}
	return nil, DeleteIndexResult{Deleted: true}, nil
}

// -----------------------------------------------------------------------
// manage_index (EXP-04 / MNG-03)
// -----------------------------------------------------------------------

// ManageIndexInput は manage_index の入力。
// ExpiryPolicy が nil の場合は keys.expiry_policy を NULL にリセットする。
type ManageIndexInput struct {
	Key          string              `json:"key"`
	ExpiryPolicy *store.ExpiryPolicy `json:"expiry_policy,omitempty"`
}

// ManageIndexResult は manage_index の出力。
type ManageIndexResult struct {
	Updated bool `json:"updated"`
}

func (h *Handlers) handleManageIndex(
	ctx context.Context, _ *mcpsdk.CallToolRequest, in ManageIndexInput,
) (*mcpsdk.CallToolResult, ManageIndexResult, error) {
	if in.Key == "" {
		return nil, ManageIndexResult{}, errors.New("key は必須")
	}
	if err := h.store.SetExpiryPolicy(ctx, in.Key, in.ExpiryPolicy); err != nil {
		return nil, ManageIndexResult{}, fmt.Errorf("set expiry policy: %w", err)
	}
	return nil, ManageIndexResult{Updated: true}, nil
}

// -----------------------------------------------------------------------
// ユーティリティ
// -----------------------------------------------------------------------

// normalizeContent は M1 の正規化を適用する:
//   1. UTF-8 BOM 除去
//   2. \r\n / 単独 \r → \n に統一
func normalizeContent(s string) string {
	const bom = "\xef\xbb\xbf"
	s = strings.TrimPrefix(s, bom)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return s
}

func sha256hex(b []byte) string {
	h := sha256.New()
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

// SearchEmbedderAdapter は embedder.Embedder を search.Embedder に適合させる。
// embedder.Vector（type Vector []float32）と search の [][]float32 のシグネチャ差を吸収する。
type SearchEmbedderAdapter struct {
	Inner embedder.Embedder
}

// Embed は内部 Embedder を呼び、結果を [][]float32 に変換して返す。
func (a *SearchEmbedderAdapter) Embed(ctx context.Context, texts []string) ([][]float32, []int, error) {
	vecs, skipped, err := a.Inner.Embed(ctx, texts)
	out := make([][]float32, len(vecs))
	for i, v := range vecs {
		out[i] = []float32(v)
	}
	return out, skipped, err
}
