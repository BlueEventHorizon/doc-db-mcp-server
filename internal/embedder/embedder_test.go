package embedder

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// -----------------------------------------------------------------------
// テストヘルパー
// -----------------------------------------------------------------------

// withServer はモック HTTP サーバーを立ち上げ、embeddingEndpoint をそこへ向ける。
// 終了時に元の値に戻す。
func withServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(handler)
	origEndpoint := embeddingEndpoint
	embeddingEndpoint = ts.URL
	t.Cleanup(func() {
		embeddingEndpoint = origEndpoint
		ts.Close()
	})
	return ts
}

// fastRetry はテスト中に retryBaseWait を 1ms に短縮する。
func fastRetry(t *testing.T) {
	t.Helper()
	orig := retryBaseWait
	retryBaseWait = 1 * time.Millisecond
	t.Cleanup(func() { retryBaseWait = orig })
}

// okHandler は texts と同じ長さの単純なベクトルを返すレスポンスを生成する。
func okHandler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req openAIRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("bad request body: %v", err)
			http.Error(w, "bad req", http.StatusBadRequest)
			return
		}
		resp := openAIResponse{Object: "list", Model: req.Model}
		for i := range req.Input {
			resp.Data = append(resp.Data, struct {
				Object    string    `json:"object"`
				Embedding []float32 `json:"embedding"`
				Index     int       `json:"index"`
			}{
				Object:    "embedding",
				Embedding: []float32{float32(i), 0.5, -0.5},
				Index:     i,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

func makeTexts(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("text-%d", i)
	}
	return out
}

// -----------------------------------------------------------------------
// APIKeyFromEnv
// -----------------------------------------------------------------------

func TestAPIKeyFromEnv_Priority(t *testing.T) {
	t.Setenv("OPENAI_API_DOCDB_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	if _, err := APIKeyFromEnv(); err == nil {
		t.Fatal("want error when both env vars are empty")
	}

	t.Setenv("OPENAI_API_KEY", "fallback")
	got, err := APIKeyFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if got != "fallback" {
		t.Errorf("got %q, want fallback", got)
	}

	t.Setenv("OPENAI_API_DOCDB_KEY", "primary")
	got, err = APIKeyFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if got != "primary" {
		t.Errorf("got %q, want primary (priority over OPENAI_API_KEY)", got)
	}
}

// -----------------------------------------------------------------------
// Embed: 基本系
// -----------------------------------------------------------------------

func TestEmbed_EmptyInput(t *testing.T) {
	e := New(Config{APIKey: "k", Model: "m", Dim: 3, BatchSize: 100, Timeout: time.Second})
	vecs, skipped, err := e.Embed(context.Background(), nil)
	if err != nil || vecs != nil || skipped != nil {
		t.Errorf("Embed(nil) = (%v, %v, %v), want all nil", vecs, skipped, err)
	}
}

func TestEmbed_Success(t *testing.T) {
	withServer(t, okHandler(t))
	e := New(Config{APIKey: "k", Model: "m", Dim: 3, BatchSize: 100, Timeout: 2 * time.Second})

	texts := []string{"a", "b", "c"}
	vecs, skipped, err := e.Embed(context.Background(), texts)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 0 {
		t.Errorf("skipped = %v, want empty", skipped)
	}
	if len(vecs) != len(texts) {
		t.Fatalf("len(vecs) = %d, want %d", len(vecs), len(texts))
	}
	for i, v := range vecs {
		if len(v) != 3 {
			t.Errorf("vecs[%d] dim = %d, want 3", i, len(v))
		}
		if v[0] != float32(i) {
			t.Errorf("vecs[%d][0] = %v, want %d (index marker)", i, v[0], i)
		}
	}
}

// -----------------------------------------------------------------------
// バッチ分割
// -----------------------------------------------------------------------

func TestEmbed_BatchSplit(t *testing.T) {
	var batchSizes []int
	var mu sync.Mutex
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req openAIRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("bad request body: %v", err)
			http.Error(w, "bad req", http.StatusBadRequest)
			return
		}
		mu.Lock()
		batchSizes = append(batchSizes, len(req.Input))
		mu.Unlock()

		resp := openAIResponse{Object: "list"}
		for i := range req.Input {
			resp.Data = append(resp.Data, struct {
				Object    string    `json:"object"`
				Embedding []float32 `json:"embedding"`
				Index     int       `json:"index"`
			}{
				Object:    "embedding",
				Embedding: []float32{1, 2, 3},
				Index:     i,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	// BatchSize=100 で 250 件 → 100/100/50 の 3 リクエスト
	e := New(Config{APIKey: "k", Model: "m", Dim: 3, BatchSize: 100, Timeout: 2 * time.Second})
	texts := makeTexts(250)
	vecs, _, err := e.Embed(context.Background(), texts)
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 250 {
		t.Fatalf("len(vecs) = %d, want 250", len(vecs))
	}

	mu.Lock()
	defer mu.Unlock()
	if len(batchSizes) != 3 {
		t.Fatalf("requests = %d, want 3 (100+100+50)", len(batchSizes))
	}
	wantBatches := []int{100, 100, 50}
	for i, w := range wantBatches {
		if batchSizes[i] != w {
			t.Errorf("batch[%d] size = %d, want %d", i, batchSizes[i], w)
		}
	}
}

// -----------------------------------------------------------------------
// リトライ
// -----------------------------------------------------------------------

func TestEmbed_RetryThenSuccess(t *testing.T) {
	fastRetry(t)
	var attempts int32
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n < 3 {
			http.Error(w, "transient", http.StatusInternalServerError)
			return
		}
		okHandler(t).ServeHTTP(w, r)
	})

	e := New(Config{APIKey: "k", Model: "m", Dim: 3, BatchSize: 100, Timeout: 2 * time.Second})
	vecs, skipped, err := e.Embed(context.Background(), []string{"a"})
	if err != nil {
		t.Fatalf("expected success on 3rd attempt, got err: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
	if len(skipped) != 0 {
		t.Errorf("skipped = %v, want empty", skipped)
	}
	if len(vecs) != 1 || len(vecs[0]) != 3 {
		t.Errorf("vecs = %v", vecs)
	}
}

func TestEmbed_AllRetriesFail_PartialSuccess(t *testing.T) {
	fastRetry(t)
	var attempts int32
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		http.Error(w, "always fail", http.StatusInternalServerError)
	})

	e := New(Config{APIKey: "k", Model: "m", Dim: 3, BatchSize: 100, Timeout: 2 * time.Second})
	vecs, skipped, err := e.Embed(context.Background(), []string{"a", "b"})
	if err == nil {
		t.Fatal("want error after all retries fail")
	}
	if got := atomic.LoadInt32(&attempts); got != int32(maxRetries) {
		t.Errorf("attempts = %d, want %d", got, maxRetries)
	}
	// 失敗インデックスは skipped に積まれる
	if len(skipped) != 2 {
		t.Errorf("skipped = %v, want [0, 1]", skipped)
	}
	// vecs は texts と同長で、失敗位置は nil
	if len(vecs) != 2 || vecs[0] != nil || vecs[1] != nil {
		t.Errorf("vecs = %v, want both nil", vecs)
	}
}

// -----------------------------------------------------------------------
// 部分失敗（複数バッチのうち一部のみ失敗）
// -----------------------------------------------------------------------

func TestEmbed_PartialBatchFailure(t *testing.T) {
	fastRetry(t)
	var attempts int32
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req openAIRequest
		_ = json.Unmarshal(body, &req)

		n := atomic.AddInt32(&attempts, 1)
		// バッチ 2 (texts: "text-100" 以降) は常に失敗、それ以外は成功
		_ = n
		if len(req.Input) > 0 && req.Input[0] == "text-100" {
			http.Error(w, "fail batch2", http.StatusInternalServerError)
			return
		}
		resp := openAIResponse{Object: "list"}
		for i := range req.Input {
			resp.Data = append(resp.Data, struct {
				Object    string    `json:"object"`
				Embedding []float32 `json:"embedding"`
				Index     int       `json:"index"`
			}{
				Object:    "embedding",
				Embedding: []float32{1, 2, 3},
				Index:     i,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	e := New(Config{APIKey: "k", Model: "m", Dim: 3, BatchSize: 100, Timeout: 2 * time.Second})
	texts := makeTexts(150) // batch1: 0..99 成功 / batch2: 100..149 失敗
	vecs, skipped, err := e.Embed(context.Background(), texts)

	if err == nil {
		t.Fatal("expected non-nil err on partial failure")
	}
	if len(vecs) != 150 {
		t.Fatalf("len(vecs) = %d, want 150", len(vecs))
	}
	for i := 0; i < 100; i++ {
		if vecs[i] == nil {
			t.Errorf("vecs[%d] should be set (batch1)", i)
		}
	}
	for i := 100; i < 150; i++ {
		if vecs[i] != nil {
			t.Errorf("vecs[%d] should be nil (batch2 failed)", i)
		}
	}
	if len(skipped) != 50 {
		t.Errorf("len(skipped) = %d, want 50", len(skipped))
	}
}

// -----------------------------------------------------------------------
// レスポンスの index 順序入れ替え対応
// -----------------------------------------------------------------------

func TestEmbed_ReordersByResponseIndex(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req openAIRequest
		_ = json.Unmarshal(body, &req)

		// レスポンスを逆順で返す
		resp := openAIResponse{Object: "list"}
		for i := len(req.Input) - 1; i >= 0; i-- {
			resp.Data = append(resp.Data, struct {
				Object    string    `json:"object"`
				Embedding []float32 `json:"embedding"`
				Index     int       `json:"index"`
			}{
				Object:    "embedding",
				Embedding: []float32{float32(i), 0, 0}, // index を marker に
				Index:     i,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	e := New(Config{APIKey: "k", Model: "m", Dim: 3, BatchSize: 100, Timeout: 2 * time.Second})
	vecs, _, err := e.Embed(context.Background(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range vecs {
		if v[0] != float32(i) {
			t.Errorf("vecs[%d][0] = %v, want %d (response should be reordered)", i, v[0], i)
		}
	}
}

// -----------------------------------------------------------------------
// バッチサイズ正規化
// -----------------------------------------------------------------------

func TestNew_BatchSizeNormalization(t *testing.T) {
	cases := []struct {
		in   int
		want int
	}{
		{0, defaultBatchSize},
		{-5, defaultBatchSize},
		{50, 50},
		{maxBatchSize, maxBatchSize},
		{maxBatchSize + 1, defaultBatchSize}, // 上限超過 → デフォルトにフォールバック
	}
	for _, tc := range cases {
		e := New(Config{APIKey: "k", BatchSize: tc.in}).(*openAIEmbedder)
		if e.cfg.BatchSize != tc.want {
			t.Errorf("BatchSize(in=%d) = %d, want %d", tc.in, e.cfg.BatchSize, tc.want)
		}
	}
}
