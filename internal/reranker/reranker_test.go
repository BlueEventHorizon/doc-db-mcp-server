package reranker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/BlueEventHorizon/doc-db-mcp-server/internal/search"
)

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

// rankingRow は {"ranking":[{"id","score"}]} 形式の 1 行分。
type rankingRow struct {
	ID    string  `json:"id"`
	Score float64 `json:"score"`
}

// respondWithRanking は LLM の返答として {"ranking":[{"id","score"}]} を返す。
func respondWithRanking(rows []rankingRow) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		body, _ := json.Marshal(struct {
			Ranking []rankingRow `json:"ranking"`
		}{Ranking: rows})
		resp := chatResponse{
			Choices: []struct {
				Message chatMessage `json:"message"`
			}{{Message: chatMessage{Role: "assistant", Content: string(body)}}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

func TestRerank_ScoresReturnedInCandidateOrder(t *testing.T) {
	withServer(t, respondWithRanking([]rankingRow{
		{ID: "0", Score: 0.3}, {ID: "1", Score: 0.9}, {ID: "2", Score: 0.1},
	}))
	r := New(Config{APIKey: "k", Model: "x", Timeout: time.Second})

	scores, err := r.Rerank(context.Background(), "q", cands(3))
	if err != nil {
		t.Fatal(err)
	}
	want := []float64{0.3, 0.9, 0.1}
	if len(scores) != 3 {
		t.Fatalf("len = %d, want 3", len(scores))
	}
	for i, s := range scores {
		if s != want[i] {
			t.Errorf("scores[%d] = %v, want %v", i, s, want[i])
		}
	}
}

func TestRerank_EmptyCandidates_NoCall(t *testing.T) {
	calls := 0
	withServer(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	})
	r := New(Config{APIKey: "k", Model: "x", Timeout: time.Second})
	scores, err := r.Rerank(context.Background(), "q", nil)
	if err != nil || scores != nil {
		t.Fatalf("got scores=%v err=%v", scores, err)
	}
	if calls != 0 {
		t.Errorf("API should not be called for empty input; calls=%d", calls)
	}
}

func TestRerank_OutOfRangeID_Error(t *testing.T) {
	withServer(t, respondWithRanking([]rankingRow{{ID: "99", Score: 0.5}}))
	r := New(Config{APIKey: "k", Model: "x", Timeout: time.Second})
	if _, err := r.Rerank(context.Background(), "q", cands(3)); err == nil {
		t.Fatal("want error for out-of-range id")
	}
}

func TestRerank_MissingIDs_GetSentinelScore(t *testing.T) {
	// id=1 だけ返した → 0 と 2 は -1.0 が入る (reference doc-db SKILL と同方式)
	withServer(t, respondWithRanking([]rankingRow{{ID: "1", Score: 0.7}}))
	r := New(Config{APIKey: "k", Model: "x", Timeout: time.Second})

	scores, err := r.Rerank(context.Background(), "q", cands(3))
	if err != nil {
		t.Fatal(err)
	}
	if scores[0] != -1.0 || scores[2] != -1.0 || scores[1] != 0.7 {
		t.Errorf("scores = %v, want [-1.0, 0.7, -1.0]", scores)
	}
}

func TestRerank_DuplicateIDs_LastWriteWins(t *testing.T) {
	withServer(t, respondWithRanking([]rankingRow{
		{ID: "1", Score: 0.3}, {ID: "1", Score: 0.8},
	}))
	r := New(Config{APIKey: "k", Model: "x", Timeout: time.Second})
	scores, err := r.Rerank(context.Background(), "q", cands(3))
	if err != nil {
		t.Fatal(err)
	}
	if scores[1] != 0.8 {
		t.Errorf("scores[1] = %v, want 0.8 (last write wins)", scores[1])
	}
}

func TestRerank_InvalidJSON_Error(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, _ *http.Request) {
		resp := chatResponse{Choices: []struct {
			Message chatMessage `json:"message"`
		}{{Message: chatMessage{Content: "not json"}}}}
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

func TestRerank_RequestShape_PreviewAndPayload(t *testing.T) {
	var got chatRequest
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode req: %v", err)
		}
		respondWithRanking([]rankingRow{{ID: "0", Score: 1.0}, {ID: "1", Score: 0.5}})(w, r)
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
	// user payload に id, preview が入っているか
	if !strings.Contains(got.Messages[1].Content, `"id":"0"`) {
		t.Errorf("user payload should contain id 0: %s", got.Messages[1].Content)
	}
	if !strings.Contains(got.Messages[1].Content, `"preview":`) {
		t.Errorf("user payload should contain preview field: %s", got.Messages[1].Content)
	}
}

func TestTruncateTokens(t *testing.T) {
	long := strings.Repeat("word ", 250)
	out := truncateTokens(long, 200)
	if len(strings.Fields(out)) != 200 {
		t.Errorf("token count = %d, want 200", len(strings.Fields(out)))
	}
	short := "hello world"
	if truncateTokens(short, 100) != short {
		t.Errorf("short input should pass through unchanged")
	}
}
