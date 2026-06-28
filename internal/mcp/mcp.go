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
//
// 各ツールの description は AI consumer (skill / agent) が tools/list だけで
// 使い方を理解できる粒度で記述する。概念モデル (KEY/series)、いつ使うか、
// 出力の解釈ポイント (origin_signals / warnings) を含める。
func (h *Handlers) Register(s *mcpsdk.Server) {
	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name: "upsert_documents",
		Description: `Markdown ドキュメント群をインデックスに追加・更新する。

【概念モデル】
  - KEY: インデックスの論理単位 (例: "myrepo-docs", "project-x-specs")。
    KEY ごとに独立したベクトル DB を持つ。複数のドキュメントセットを混ぜたくないなら別 KEY にする。
  - series: 同一 KEY 内での時系列タグ (例: "main", "feature-auth", "v1.2.3")。
    branch 切替や複数バージョン保持に使う。同一内容のドキュメントは複数 series で
    embedding を共有 (hash 一致時に再 embedding しない)。
  - path: 各ドキュメントの識別子 (例: "README.md", "src/api.md")。
    KEY+series+path の組で一意。

【動作】
  1. content をクライアントから受領 (または url から取得)
  2. SHA-256 ハッシュを計算 → 既存と一致なら embedding を再利用 (series_keys に追記)
  3. 不一致なら Markdown を見出し境界でチャンク分割し、OpenAI Embedding API で
     ベクトル化して保存

【注意】
  - content と url は排他。両方指定するとエラー。
  - 部分失敗 (Embedding API エラー等) は処理継続し、失敗した path は出力 errors に含まれる`,
	}, h.handleUpsert)

	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name: "delete_documents",
		Description: `指定 KEY+series から特定 path のドキュメントを削除する。

【動作】
  paths に列挙された各ドキュメントから当該 series を除去する。
  series_keys が空になった record のみチャンク・ベクトル共に物理削除される
  (他 series が残る record は保持)。

【典型ユースケース】
  - branch 削除に伴うクリーンアップ (series="feature-x" を全 path から除去)
  - 個別ドキュメントの削除

存在しない path はスキップされ警告として返る (致命的でない)。`,
	}, h.handleDelete)

	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name: "query",
		Description: `自然言語クエリでドキュメントチャンクを検索する。

【設計思想: 二層検索アーキテクチャ (PHIL-01)】
本サーバーは「正解 top-N を返す」のではなく「**取りこぼし無き候補プールを返す**」設計。
呼び出し側 (AI agent) が本文を読んで関連性を判断することを前提とする。
このため Embedding + BM25 + 全文 GREP の 3 signal を並列実行し、合算した
候補プールを返す (mode=all)。

【mode の選び方】
  - all      (デフォルト): 3 signal 並列。取りこぼしを最小化。AI agent が
                          結果を読んで関連性判定する用途に最適
  - rerank   : all の結果を LLM (gpt-4o-mini) でランキング最適化。
              ranking 精度が重要で、ある程度レイテンシが許容される場合
  - emb      : 意味類似のみ。言い換え・抽象クエリに強い
  - lex      : BM25 のみ。トークン頻度ベース
  - grep     : literal 一致のみ。固有 ID・特殊用語・低頻度トークンを確実に拾う
  - hybrid   : emb+lex の RRF 融合 (legacy、grep を含まない)

【origin_signals の解釈】
各 chunk が「どの signal でヒットしたか」を配列で返す (例: ["emb","grep"])。
複数 signal でヒットした chunk は信頼度が高い (上位表示される)。

【出力解釈】
  - results[*]: 候補チャンク。Layer 2 (上位 AI agent) が本文を読んで判定する想定
  - stage_stats: 各 signal でヒットした候補数。recall の健全性チェックに使う
  - warnings: 致命的でない異常 (LLM Rerank フォールバック発動・EMB fallback 等)。
              silent failure 禁止方針により全観測可能化されている`,
	}, h.handleQuery)

	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name: "list_indexes",
		Description: `登録済み KEY (インデックス) の一覧を返す。

返り値の各エントリ:
  - key, series リスト, doc_count
  - last_updated_at / last_accessed_at (RFC3339)
  - expiry_policy (KEY ごとの TTL/max_chunks オーバーライド、未設定なら null)

使い道:
  - どの KEY が存在するかを確認 (query の前段)
  - 廃棄候補の特定 (last_accessed_at が古い KEY)`,
	}, h.handleListIndexes)

	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name: "delete_index",
		Description: `指定 KEY のインデックスを完全削除する (全 series / 全チャンク / 全ベクトル)。

破壊的操作のため使用注意。delete_documents が series 単位の削除なのに対し、
delete_index は KEY 全体を消す。プロジェクト終了時のクリーンアップ等で使う。`,
	}, h.handleDeleteIndex)

	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name: "manage_index",
		Description: `指定 KEY の廃棄ポリシー (TTL/max_chunks) を設定・更新する。

【廃棄ポリシー】
  - ttl_days: 最終アクセスからの経過日数 (この日数を超えたら自動削除)
  - max_chunks: KEY あたりのチャンク上限 (上限超過時に LRU で削除)

【動作】
  - expiry_policy を null にするとサーバーデフォルト (30 days / 10000 chunks) に戻る
  - 一部フィールドだけ指定可 (ttl_days のみ等)

長期保持したい重要 KEY や、逆に短期間で自動廃棄したいテンポラリ KEY の制御に使う。`,
	}, h.handleManageIndex)
}

// -----------------------------------------------------------------------
// upsert_documents (FNC-001)
// -----------------------------------------------------------------------

// UpsertDocument は upsert_documents の document 1 件分入力（content/url 排他）。
type UpsertDocument struct {
	Path    string `json:"path" jsonschema:"ドキュメントの識別子。KEY+series+path の組で一意。例: 'README.md', 'src/api.md'。クライアントが自由に定義する。"`
	Content string `json:"content,omitempty" jsonschema:"ドキュメント本文の Markdown テキスト。url と排他。"`
	URL     string `json:"url,omitempty" jsonschema:"ドキュメント取得元 URL (http/https)。サーバーが取得しハッシュを算出。content と排他。"`
	Hash    string `json:"hash,omitempty" jsonschema:"コンテンツの SHA-256 ハッシュ (省略時はサーバーが算出)。content 指定時に渡すと検証に使われる。url 指定時は無視される。"`
}

// UpsertInput は upsert_documents の入力。
type UpsertInput struct {
	Key       string           `json:"key" jsonschema:"インデックスの論理単位。複数のドキュメントセットを分離するための opaque 文字列。例: 'myrepo-docs', 'project-x-specs'。"`
	Series    string           `json:"series" jsonschema:"同一 KEY 内の時系列タグ。branch / バージョン / feature 分岐を表現する。例: 'main', 'feature-auth', 'v1.2.3'。同一内容のドキュメントは複数 series で embedding を共有する。"`
	Documents []UpsertDocument `json:"documents" jsonschema:"登録するドキュメントのリスト。各要素は content または url のいずれか一方を指定する。"`
}

// UpsertError は失敗・スキップドキュメントの詳細（UPS-OUT-01）。
type UpsertError struct {
	Path          string `json:"path" jsonschema:"失敗したドキュメントの path。"`
	Error         string `json:"error" jsonschema:"失敗理由 (例: 'fetch: connection refused', 'embed: API error')。"`
	SkippedChunks []int  `json:"skipped_chunks,omitempty" jsonschema:"Embedding が失敗したチャンクのインデックス。部分失敗時 (M2): 成功チャンクは保存され、失敗チャンクのみ vector=nil でテキスト保存される。"`
}

// UpsertResult は upsert_documents の出力（UPS-OUT-01）。
type UpsertResult struct {
	Processed int           `json:"processed" jsonschema:"新規 / 内容変更で処理されたドキュメント数。"`
	Skipped   int           `json:"skipped" jsonschema:"同一ハッシュ既存で再 embedding をスキップしたドキュメント数 (series_keys に追記のみ)。"`
	Failed    int           `json:"failed" jsonschema:"完全に失敗したドキュメント数 (fetch エラー・バリデーションエラー等)。"`
	Errors    []UpsertError `json:"errors,omitempty" jsonschema:"失敗または部分失敗の詳細リスト。"`
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
	Key    string   `json:"key" jsonschema:"対象インデックスの KEY。"`
	Series string   `json:"series" jsonschema:"削除対象の series。各ドキュメントの series_keys からこれを除去する。空になった record はチャンク・ベクトル共に物理削除される。"`
	Paths  []string `json:"paths" jsonschema:"削除するドキュメントの path リスト (upsert_documents で登録した path の値)。存在しない path はスキップされ warnings に記録される。"`
}

// DeleteResult は delete_documents の出力。
type DeleteResult struct {
	Deleted  int      `json:"deleted" jsonschema:"実際に削除処理された path 数。"`
	Warnings []string `json:"warnings,omitempty" jsonschema:"非致命的な警告 (存在しない path のスキップ等)。"`
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
	Key    string `json:"key" jsonschema:"検索対象の KEY。list_indexes で確認できる。"`
	Series string `json:"series,omitempty" jsonschema:"絞り込む series。省略時は KEY 内の全 series を横断検索する。"`
	Mode   string `json:"mode,omitempty" jsonschema:"検索方式。'all' (デフォルト、推奨) = emb+lex+grep 3 signal 並列 (PHIL-01 over-recall)。'rerank' = all + LLM ranking 最適化。'emb' = 意味類似のみ。'lex' = BM25 のみ。'grep' = literal 一致のみ (固有 ID/特殊用語向け)。'hybrid' = emb+lex RRF (legacy、grep なし)。"`
	TopN   int    `json:"top_n,omitempty" jsonschema:"返却件数の上限 (デフォルト 10)。上位 AI agent が候補を読んで判定する設計のため、recall を確保するなら 10-30 程度を推奨。"`
}

// QueryHit は検索結果 1 件（QRY-OUT-01 / QRY-OUT-03）。
type QueryHit struct {
	Path           string                `json:"path" jsonschema:"ドキュメントの path (upsert 時に指定した識別子)。"`
	HeadingPath    string                `json:"heading_path" jsonschema:"チャンクの見出し階層パス。例: 'DES-001 設計書 > 6. 検索パイプライン > 6.2 BM25'。chunk の位置を上位 AI agent が把握する手がかり。"`
	Text           string                `json:"text" jsonschema:"チャンクの本文 (上位 AI agent が読んで関連性を判定するための主要データ)。"`
	Score          float64               `json:"score" jsonschema:"代表スコア (mode により emb/lex/grep/rerank いずれかの値)。順位の参考だが、最終的な関連性判定は AI agent が本文を読んで行う。"`
	OriginSignals  []string              `json:"origin_signals,omitempty" jsonschema:"この chunk がヒットした signal のリスト。例: ['emb','grep']。複数 signal でヒットした chunk は信頼度が高い。PHIL-01 二層アーキで上位 AI agent が候補をフィルタする際の重要な手がかり。"`
	ScoreBreakdown search.ScoreBreakdown `json:"score_breakdown" jsonschema:"各 signal のスコア内訳。emb/lex/grep/rrf/rerank。0 の signal はそのチャンクでヒットしなかったことを意味する (ただし emb は cos<=0 でも返ることがある)。"`
	SeriesKeys     []string              `json:"series_keys" jsonschema:"この chunk が紐づく series のリスト。同一内容で複数 series に共有された場合は複数値が入る。"`
}

// QueryResult は query の出力（QRY-OUT-02）。
//
// Warnings: 致命的でない異常（TouchKey 失敗・Rerank フォールバック等）を caller に伝達する。
// silent failure 禁止方針に従い、log にしか出ない情報は警告メッセージとして含める。
type QueryResult struct {
	Results    []QueryHit        `json:"results" jsonschema:"検索結果チャンクのリスト。Layer 2 (上位 AI agent) が本文を読んで関連性判定する想定の候補プール。"`
	StageStats search.StageStats `json:"stage_stats" jsonschema:"各検索ステージで残った候補数。emb_candidates/lex_candidates/grep_candidates/merged_candidates/rerank_candidates。recall の健全性チェックに使う (例: grep_candidates=0 なら literal hit 無し)。"`
	Warnings   []string          `json:"warnings,omitempty" jsonschema:"致命的でない異常 (LLM Rerank フォールバック発動・EMB fallback 発動・TouchKey 失敗等)。silent failure 禁止方針により全て観測可能化されている。空配列なら全 signal 正常実行。"`
}

func (h *Handlers) handleQuery(
	ctx context.Context, _ *mcpsdk.CallToolRequest, in QueryInput,
) (*mcpsdk.CallToolResult, QueryResult, error) {
	if in.Query == "" || in.Key == "" {
		return nil, QueryResult{}, errors.New("query と key は必須")
	}
	mode := search.Mode(in.Mode)
	if mode == "" {
		// PHIL-01: デフォルトは all (3 signal 並列 over-recall)。
		// v0.1.4 以前は rerank がデフォルトだったが PHIL-01/02 に従い変更。
		mode = search.ModeAll
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
			OriginSignals:  r.OriginSignals,
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
	Indexes []store.KeyInfo `json:"indexes" jsonschema:"登録済みインデックスのリスト。各エントリに key/series 一覧/doc_count/last_updated_at/last_accessed_at/expiry_policy を含む。"`
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
	Key string `json:"key" jsonschema:"削除する KEY。指定 KEY の全 series / 全チャンク / 全ベクトルが物理削除される (破壊的)。"`
}

// DeleteIndexResult は delete_index の出力。
type DeleteIndexResult struct {
	Deleted bool `json:"deleted" jsonschema:"削除に成功したか。"`
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
	Key          string              `json:"key" jsonschema:"対象 KEY。"`
	ExpiryPolicy *store.ExpiryPolicy `json:"expiry_policy,omitempty" jsonschema:"廃棄ポリシー設定。ttl_days (最終アクセスからの自動削除日数) と max_chunks (KEY あたりのチャンク上限) を指定。null/省略でサーバーデフォルト (30days/10000chunks) にリセット。"`
}

// ManageIndexResult は manage_index の出力。
type ManageIndexResult struct {
	Updated bool `json:"updated" jsonschema:"設定が更新されたか。"`
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
