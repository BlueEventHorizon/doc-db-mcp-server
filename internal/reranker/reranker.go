// Package reranker は OpenAI Chat Completions API を呼んで候補チャンクを並べ替える
// 実装を提供する（DES-001 §6.4 LLM Rerank）。
//
// reference doc-db SKILL (llm_rerank.py) と同方式:
//   - candidates ごとに preview = `heading_path + body` を ~200 tokens に切り詰める
//   - LLM へは {"query","candidates":[{"id","preview"}]} を渡し、
//     {"ranking":[{"id","score":0..1}]} を要求する（response_format=json_object）
//   - 戻り値は candidates と同長の scores（欠落 ID は -1.0、search.Pipeline 側で
//     ブレンドソートに使われる）
//   - タイムアウト・API エラー時は呼び出し元（search.Pipeline）が RRF 順にフォールバック（RR-02）
package reranker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
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

// previewMaxTokens は 1 candidate に渡す preview の最大 token 数（reference doc-db SKILL と同値）。
const previewMaxTokens = 200

// Rerank は候補チャンクを LLM に並べ替えさせ、candidates と同じ長さの score 配列を返す。
// scores[i] は cands[i] に対する LLM の関連度 (0..1)。LLM が出力しなかった id は -1.0。
// API エラーや JSON パース失敗時は error を返し、呼び出し元 (search.Pipeline) で RRF にフォールバック（RR-02）。
func (r *OpenAIReranker) Rerank(ctx context.Context, query string, cands []search.RerankCandidate) ([]float64, error) {
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

	scores, err := parseRankingScores(apiResp.Choices[0].Message.Content, len(cands))
	if err != nil {
		return nil, fmt.Errorf("reranker: parse model output: %w", err)
	}
	return scores, nil
}

// -----------------------------------------------------------------------
// プロンプト構築（reference llm_rerank.py と同方式）
// -----------------------------------------------------------------------

const systemPrompt = `You rerank candidates by relevance to the query. ` +
	`Return JSON: {"ranking":[{"id":"...","score":0..1}, ...]} ` +
	`with all ids included exactly once.`

// userPayload はモデルに渡す候補リストの JSON 形。
type userPayload struct {
	Query      string            `json:"query"`
	Candidates []userPayloadCand `json:"candidates"`
}

type userPayloadCand struct {
	ID      string `json:"id"`
	Preview string `json:"preview"`
}

func buildUserMessage(query string, cands []search.RerankCandidate) (string, error) {
	pl := userPayload{
		Query:      query,
		Candidates: make([]userPayloadCand, len(cands)),
	}
	for i, c := range cands {
		pl.Candidates[i] = userPayloadCand{
			ID:      fmt.Sprintf("%d", i), // cands 内の局所 ID（chunks index ではない）
			Preview: buildPreview(c),
		}
	}
	b, err := json.Marshal(pl)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// buildPreview は heading_path + body を空白区切りでつないだ後、tokens を上限まで切り詰める。
// reference doc-db SKILL の build_preview と同方式（200 tokens 上限）。
func buildPreview(c search.RerankCandidate) string {
	text := strings.TrimSpace(c.HeadingPath + "\n" + c.Text)
	return truncateTokens(text, previewMaxTokens)
}

// whitespaceTokenRe は空白区切りの token 列。reference の `re.findall(r"\S+", text)` と同等。
var whitespaceTokenRe = regexp.MustCompile(`\S+`)

func truncateTokens(text string, maxTokens int) string {
	tokens := whitespaceTokenRe.FindAllString(text, -1)
	if len(tokens) <= maxTokens {
		return text
	}
	return strings.Join(tokens[:maxTokens], " ")
}

// parseRankingScores はモデル応答の JSON から ranking 配列を取り出し、
// candidates と同じ長さのスコア配列に展開する。
// 検証ルール:
//   - 範囲外 ID はエラー
//   - 重複 ID は最後の値を採用
//   - 欠落 ID のスコアは -1.0（reference llm_rerank.py の `-rank_map.get(id, -1.0)` 相当）
//   - 余分なフィールドは無視
func parseRankingScores(content string, n int) ([]float64, error) {
	var out struct {
		Ranking []struct {
			ID    json.RawMessage `json:"id"`
			Score float64         `json:"score"`
		} `json:"ranking"`
	}
	if err := json.Unmarshal([]byte(content), &out); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}

	scores := make([]float64, n)
	seen := make([]bool, n)
	for i := range scores {
		scores[i] = -1.0
	}

	for _, row := range out.Ranking {
		// id は "0" / "1" のような数値文字列、または数値リテラルの両方を許容
		var idStr string
		if err := json.Unmarshal(row.ID, &idStr); err != nil {
			var idNum int
			if err2 := json.Unmarshal(row.ID, &idNum); err2 != nil {
				return nil, fmt.Errorf("invalid id field %s", string(row.ID))
			}
			idStr = fmt.Sprintf("%d", idNum)
		}
		var id int
		if _, err := fmt.Sscanf(idStr, "%d", &id); err != nil {
			return nil, fmt.Errorf("non-numeric id %q", idStr)
		}
		if id < 0 || id >= n {
			return nil, fmt.Errorf("ranking id %d out of range [0,%d)", id, n)
		}
		scores[id] = row.Score
		seen[id] = true
	}
	return scores, nil
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
