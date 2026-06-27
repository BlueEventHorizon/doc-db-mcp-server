package reranker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/BlueEventHorizon/doc-db-mcp-server/internal/search"
)

// withServer はモック OpenAI Chat Completions サーバーを立て、エンドポイントを差し替える。
func withServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(h)
	orig := chatCompletionsEndpoint
	chatCompletionsEndpoint = ts.URL
	t.Cleanup(func() {
		chatCompletionsEndpoint = orig
		ts.Close()
	})
	return ts
}

func cands(n int) []search.RerankCandidate {
	out := make([]search.RerankCandidate, n)
	for i := 0; i < n; i++ {
		out[i] = search.RerankCandidate{Index: i * 10, Text: "text", HeadingPath: "# H"}
	}
	return out
}

// レスポンスを {"ranked":[...]} 形で返すヘルパ。
func respondWith(ranked []int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		bodyJSON, _ := json.Marshal(struct {
			Ranked []int `json:"ranked"`
		}{Ranked: ranked})
		resp := chatResponse{
			Choices: []struct {
				Message chatMessage `json:"message"`
			}{
				{Message: chatMessage{Role: "assistant", Content: string(bodyJSON)}},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

func TestRerank_HappyPath(t *testing.T) {
	withServer(t, respondWith([]int{2, 0, 1}))
	r := New(Config{APIKey: "k", Model: "gpt-4o-mini", Timeout: time.Second})

	order, err := r.Rerank(context.Background(), "q", cands(3))
	if err != nil {
		t.Fatal(err)
	}
	if want := []int{2, 0, 1}; !equal(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}
}

func TestRerank_EmptyCandidates_NoCall(t *testing.T) {
	calls := 0
	withServer(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	})
	r := New(Config{APIKey: "k", Model: "x", Timeout: time.Second})
	order, err := r.Rerank(context.Background(), "q", nil)
	if err != nil || order != nil {
		t.Fatalf("got %v / %v", order, err)
	}
	if calls != 0 {
		t.Errorf("API should not be called for empty input; calls=%d", calls)
	}
}

func TestRerank_OutOfRangeID_Error(t *testing.T) {
	withServer(t, respondWith([]int{99}))
	r := New(Config{APIKey: "k", Model: "x", Timeout: time.Second})
	if _, err := r.Rerank(context.Background(), "q", cands(3)); err == nil {
		t.Fatal("want error for out-of-range id")
	}
}

func TestRerank_MissingIDs_AppendedAtEnd(t *testing.T) {
	// モデルが id=0 と id=2 だけ返した → id=1 は末尾に補完される
	withServer(t, respondWith([]int{2, 0}))
	r := New(Config{APIKey: "k", Model: "x", Timeout: time.Second})

	order, err := r.Rerank(context.Background(), "q", cands(3))
	if err != nil {
		t.Fatal(err)
	}
	if want := []int{2, 0, 1}; !equal(order, want) {
		t.Errorf("order = %v, want %v (missing id at tail)", order, want)
	}
}

func TestRerank_DuplicateIDs_Deduplicated(t *testing.T) {
	withServer(t, respondWith([]int{1, 1, 0, 2}))
	r := New(Config{APIKey: "k", Model: "x", Timeout: time.Second})
	order, err := r.Rerank(context.Background(), "q", cands(3))
	if err != nil {
		t.Fatal(err)
	}
	if want := []int{1, 0, 2}; !equal(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}
}

func TestRerank_InvalidJSON_Error(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, _ *http.Request) {
		resp := chatResponse{
			Choices: []struct {
				Message chatMessage `json:"message"`
			}{{Message: chatMessage{Content: "not json"}}},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	r := New(Config{APIKey: "k", Model: "x", Timeout: time.Second})
	if _, err := r.Rerank(context.Background(), "q", cands(3)); err == nil {
		t.Fatal("want error for non-JSON content")
	}
}

func TestRerank_APIError(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(chatResponse{
			Error: &chatError{Message: "bad request", Type: "invalid_request_error"},
		})
	})
	r := New(Config{APIKey: "k", Model: "x", Timeout: time.Second})
	if _, err := r.Rerank(context.Background(), "q", cands(3)); err == nil {
		t.Fatal("want error for HTTP 400")
	}
}

func TestRerank_RequestShape(t *testing.T) {
	var got chatRequest
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode req: %v", err)
		}
		respondWith([]int{0, 1})(w, r)
	})

	r := New(Config{APIKey: "k", Model: "gpt-4o-mini", Timeout: time.Second})
	if _, err := r.Rerank(context.Background(), "the query", cands(2)); err != nil {
		t.Fatal(err)
	}
	if got.Model != "gpt-4o-mini" {
		t.Errorf("model = %q", got.Model)
	}
	if got.ResponseFormat == nil || got.ResponseFormat.Type != "json_object" {
		t.Errorf("response_format not set to json_object: %+v", got.ResponseFormat)
	}
	if len(got.Messages) != 2 || got.Messages[0].Role != "system" || got.Messages[1].Role != "user" {
		t.Errorf("messages shape unexpected: %+v", got.Messages)
	}
	if got.Temperature != 0 {
		t.Errorf("temperature = %v, want 0", got.Temperature)
	}
}

func equal(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
