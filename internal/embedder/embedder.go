// package embedder は OpenAI Embedding API を呼び出しベクトルを生成する（net/http）。
package embedder

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"time"
)

// Vector は float32 のスライスで表現した埋め込みベクトル。
type Vector []float32

// Embedder はテキストを埋め込みベクトルに変換するインターフェース。
// テスト時にモック実装で差し替え可能にするためにインターフェースとして定義する。
type Embedder interface {
	// Embed は texts をバッチで Embedding API に送り、各テキストのベクトルを返す。
	// 戻り値:
	//   - vecs: texts と同じ長さのスライス。失敗したインデックスの要素は nil になる。
	//   - skipped: 失敗したバッチに含まれていたテキストのインデックス一覧（texts 基準）。
	//   - err: 最初に発生したエラー。部分成功時も non-nil になる。
	// vecs[i] が nil かどうか、または skipped に i が含まれるかを確認して
	// texts[i] とベクトルの対応を判断すること（DES-001 §5.2 M2）。
	Embed(ctx context.Context, texts []string) (vecs []Vector, skipped []int, err error)
}

// Config は OpenAI Embedder の設定。
// 設定値は config.EmbeddingConfig から組み立て、APIKey のみ環境変数で渡す（DES-001 §9.1）。
type Config struct {
	// APIKey は OpenAI API キー（OPENAI_API_DOCDB_KEY → OPENAI_API_KEY のフォールバック）。
	// シークレットのため設定ファイルではなく環境変数経由で渡す（PRE-01）。
	APIKey string

	// Model は Embedding モデル名（doc-db.yaml: embedding.model）。
	Model string

	// Dim は Embedding ベクトルの次元数（doc-db.yaml: embedding.dim）。
	Dim int

	// Timeout は Embedding API 1 回のリクエストタイムアウト（doc-db.yaml: embedding.timeout_seconds）。
	Timeout time.Duration

	// BatchSize は 1 リクエストあたりの最大テキスト数（デフォルト: 100、上限: 100）。
	BatchSize int
}

// openAIEmbedder は Config を使って OpenAI Embedding API を呼び出す実装。
type openAIEmbedder struct {
	cfg    Config
	client *http.Client
}

const (
	defaultModel     = "text-embedding-3-large"
	defaultDim       = 3072
	defaultTimeout   = 60 * time.Second
	defaultBatchSize = 100
	maxBatchSize     = 100

	// リトライ設定（DES-001 §7.1）
	maxRetries = 3
)

// テスト時に差し替え可能にするため var とする（プロダクション値は変更しない）。
var (
	embeddingEndpoint = "https://api.openai.com/v1/embeddings"
	retryBaseWait     = 1 * time.Second
)

// APIKeyFromEnv は OpenAI API キーを環境変数から取得する（PRE-01）。
// OPENAI_API_DOCDB_KEY → OPENAI_API_KEY の順でフォールバックする。
// どちらも未設定の場合はエラーを返す（fail-fast）。
func APIKeyFromEnv() (string, error) {
	if v := os.Getenv("OPENAI_API_DOCDB_KEY"); v != "" {
		return v, nil
	}
	if v := os.Getenv("OPENAI_API_KEY"); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("embedder: neither OPENAI_API_DOCDB_KEY nor OPENAI_API_KEY is set")
}

// New は Config を使って Embedder を生成する。
// HTTP クライアントのタイムアウトは cfg.Timeout で設定される。
func New(cfg Config) Embedder {
	if cfg.BatchSize <= 0 || cfg.BatchSize > maxBatchSize {
		cfg.BatchSize = defaultBatchSize
	}
	return &openAIEmbedder{
		cfg: cfg,
		client: &http.Client{
			Timeout: cfg.Timeout,
		},
	}
}

// Embed は texts をバッチ分割して Embedding API に送り、ベクトルのスライスを返す。
// 戻り値の vecs は texts と同じ長さであり、vecs[i] は texts[i] に対応する。
// バッチ失敗時は該当インデックスの vecs[i] を nil にし、skipped に追加して処理を継続する（部分成功 M2）。
func (e *openAIEmbedder) Embed(ctx context.Context, texts []string) ([]Vector, []int, error) {
	if len(texts) == 0 {
		return nil, nil, nil
	}

	results := make([]Vector, len(texts)) // texts と同長: 失敗インデックスは nil のまま
	var skipped []int
	var firstErr error

	for start := 0; start < len(texts); start += e.cfg.BatchSize {
		end := start + e.cfg.BatchSize
		if end > len(texts) {
			end = len(texts)
		}
		batch := texts[start:end]

		vecs, err := e.embedBatchWithRetry(ctx, batch)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			// 部分成功方針（M2）: 失敗バッチのインデックスを skipped に記録して処理継続
			for i := start; i < end; i++ {
				skipped = append(skipped, i)
			}
			continue
		}
		// vecs[j] は batch[j] = texts[start+j] に対応する
		for j, v := range vecs {
			results[start+j] = v
		}
	}

	return results, skipped, firstErr
}

// embedBatchWithRetry は 1 バッチに対して指数バックオフリトライを行う。
// 最大 maxRetries 回試行し、全て失敗したらエラーを返す。
func (e *openAIEmbedder) embedBatchWithRetry(ctx context.Context, texts []string) ([]Vector, error) {
	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			wait := retryBaseWait * time.Duration(math.Pow(2, float64(attempt-1)))
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
		}

		vecs, err := e.embedBatch(ctx, texts)
		if err == nil {
			return vecs, nil
		}
		lastErr = err

		// コンテキストキャンセル時はリトライしない
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, fmt.Errorf("embedder: all %d retries failed: %w", maxRetries, lastErr)
}

// openAIRequest は OpenAI Embedding API のリクエストボディ。
type openAIRequest struct {
	Model          string   `json:"model"`
	Input          []string `json:"input"`
	EncodingFormat string   `json:"encoding_format"`
}

// openAIResponse は OpenAI Embedding API のレスポンスボディ。
type openAIResponse struct {
	Object string `json:"object"`
	Data   []struct {
		Object    string    `json:"object"`
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
	Model string `json:"model"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
	// エラーレスポンス用
	Error *openAIError `json:"error,omitempty"`
}

// openAIError は OpenAI API のエラーレスポンス。
type openAIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
}

// embedBatch は 1 回の API コールでバッチを Embed する。
func (e *openAIEmbedder) embedBatch(ctx context.Context, texts []string) ([]Vector, error) {
	reqBody := openAIRequest{
		Model:          e.cfg.Model,
		Input:          texts,
		EncodingFormat: "float",
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("embedder: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, embeddingEndpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("embedder: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.cfg.APIKey)

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedder: http request: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("embedder: read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var apiErr openAIResponse
		if jsonErr := json.Unmarshal(respBytes, &apiErr); jsonErr == nil && apiErr.Error != nil {
			return nil, fmt.Errorf("embedder: API error (status %d): %s", resp.StatusCode, apiErr.Error.Message)
		}
		return nil, fmt.Errorf("embedder: unexpected status %d: %s", resp.StatusCode, string(respBytes))
	}

	var apiResp openAIResponse
	if err := json.Unmarshal(respBytes, &apiResp); err != nil {
		return nil, fmt.Errorf("embedder: unmarshal response: %w", err)
	}

	// レスポンスの index 順に並べ直す（API は順序を保証しないため）
	vecs := make([]Vector, len(texts))
	for _, d := range apiResp.Data {
		if d.Index < 0 || d.Index >= len(texts) {
			return nil, fmt.Errorf("embedder: response index %d out of range [0, %d)", d.Index, len(texts))
		}
		vecs[d.Index] = Vector(d.Embedding)
	}

	// 欠損インデックスの確認
	for i, v := range vecs {
		if v == nil {
			return nil, fmt.Errorf("embedder: missing embedding for index %d in response", i)
		}
	}

	return vecs, nil
}
