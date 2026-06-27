// Package reranker は OpenAI Chat Completions API を呼んで候補チャンクを並べ替える
// 実装を提供する（DES-001 §6.4 LLM Rerank）。
//
// 設計:
//   - search.Reranker インターフェースの具象実装
//   - gpt-4o-mini 等にクエリと候補リストを渡し、関連度順の ID 配列を返させる
//   - JSON 出力を強制（response_format=json_object）
//   - タイムアウト・API エラー時は呼び出し元（search.Pipeline）が RRF 順にフォールバック（RR-02）
package reranker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/BlueEventHorizon/doc-db-mcp-server/internal/search"
)

// chatCompletionsEndpoint はテスト時に差し替えるため var とする。
var chatCompletionsEndpoint = "https://api.openai.com/v1/chat/completions"

// Config は OpenAIReranker の設定。
type Config struct {
	// APIKey は OpenAI API キー（embedder と共通の環境変数経由）。
	APIKey string
	// Model は使用する Chat Completions モデル名（例: gpt-4o-mini）。
	Model string
	// Timeout は HTTP リクエストのタイムアウト。
	Timeout time.Duration
}

// OpenAIReranker は OpenAI Chat Completions を用いた Reranker。
type OpenAIReranker struct {
	cfg    Config
	client *http.Client
}

// New は Config を使って OpenAIReranker を生成する。
func New(cfg Config) *OpenAIReranker {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	return &OpenAIReranker{
		cfg:    cfg,
		client: &http.Client{Timeout: cfg.Timeout},
	}
}

// Rerank は候補チャンクを LLM に並べ替えさせ、cands 配列内の relevance 降順インデックスを返す。
// LLM 出力が解釈できない場合や API エラー時は error を返し、search.Pipeline 側で RRF 順
// フォールバックされる（RR-02）。
func (r *OpenAIReranker) Rerank(ctx context.Context, query string, cands []search.RerankCandidate) ([]int, error) {
	if len(cands) == 0 {
		return nil, nil
	}

	userMsg, err := buildUserMessage(query, cands)
	if err != nil {
		return nil, fmt.Errorf("reranker: build prompt: %w", err)
	}

	reqBody := chatRequest{
		Model: r.cfg.Model,
		Messages: []chatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userMsg},
		},
		ResponseFormat: &responseFormat{Type: "json_object"},
		Temperature:    0,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("reranker: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatCompletionsEndpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("reranker: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.cfg.APIKey)

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reranker: http request: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reranker: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var apiErr chatResponse
		if jsonErr := json.Unmarshal(respBytes, &apiErr); jsonErr == nil && apiErr.Error != nil {
			return nil, fmt.Errorf("reranker: API error (status %d): %s", resp.StatusCode, apiErr.Error.Message)
		}
		return nil, fmt.Errorf("reranker: unexpected status %d: %s", resp.StatusCode, string(respBytes))
	}

	var apiResp chatResponse
	if err := json.Unmarshal(respBytes, &apiResp); err != nil {
		return nil, fmt.Errorf("reranker: unmarshal response: %w", err)
	}
	if len(apiResp.Choices) == 0 {
		return nil, fmt.Errorf("reranker: empty choices in response")
	}

	order, err := parseRankedIDs(apiResp.Choices[0].Message.Content, len(cands))
	if err != nil {
		return nil, fmt.Errorf("reranker: parse model output: %w", err)
	}
	return order, nil
}

// -----------------------------------------------------------------------
// プロンプト構築
// -----------------------------------------------------------------------

const systemPrompt = `You are a document search reranker. The user gives you a query and a list of candidate snippets, each with an integer id. Reorder the candidates by their relevance to the query, most relevant first. Reply with JSON only, in this exact shape:

{"ranked": [<id>, <id>, ...]}

Include every id from the input exactly once. Do not invent ids. Do not add commentary.`

// userPayload はモデルに渡す候補リストの JSON 形。
type userPayload struct {
	Query      string              `json:"query"`
	Candidates []userPayloadCand   `json:"candidates"`
}

type userPayloadCand struct {
	ID          int    `json:"id"`
	HeadingPath string `json:"heading_path,omitempty"`
	Text        string `json:"text"`
}

func buildUserMessage(query string, cands []search.RerankCandidate) (string, error) {
	pl := userPayload{
		Query:      query,
		Candidates: make([]userPayloadCand, len(cands)),
	}
	for i, c := range cands {
		pl.Candidates[i] = userPayloadCand{
			ID:          i, // cands 配列内の局所 ID（chunks index ではない）
			HeadingPath: c.HeadingPath,
			Text:        c.Text,
		}
	}
	b, err := json.Marshal(pl)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// parseRankedIDs はモデル応答の JSON から ranked 配列を取り出し、検証して返す。
// 検証ルール:
//   - すべての ID が [0, n) の範囲内
//   - 重複なし
//   - 不足は許容（モデルが id を一部落とした場合は、末尾に欠落 id を昇順で補う）
//   - 余分は許容しない（範囲外 id があればエラー）
func parseRankedIDs(content string, n int) ([]int, error) {
	var out struct {
		Ranked []int `json:"ranked"`
	}
	if err := json.Unmarshal([]byte(content), &out); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}

	seen := make([]bool, n)
	order := make([]int, 0, n)
	for _, id := range out.Ranked {
		if id < 0 || id >= n {
			return nil, fmt.Errorf("ranked id %d out of range [0,%d)", id, n)
		}
		if seen[id] {
			continue // 重複は無視
		}
		seen[id] = true
		order = append(order, id)
	}
	// 欠落補完: モデルが落とした id を末尾に昇順で追加
	for id := 0; id < n; id++ {
		if !seen[id] {
			order = append(order, id)
		}
	}
	return order, nil
}

// -----------------------------------------------------------------------
// OpenAI Chat Completions API ペイロード
// -----------------------------------------------------------------------

type chatRequest struct {
	Model          string          `json:"model"`
	Messages       []chatMessage   `json:"messages"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
	Temperature    float64         `json:"temperature"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type responseFormat struct {
	Type string `json:"type"` // "json_object"
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Error *chatError `json:"error,omitempty"`
}

type chatError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
}
