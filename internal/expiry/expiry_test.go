package expiry

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/BlueEventHorizon/doc-db-mcp-server/internal/store"
)

// -----------------------------------------------------------------------
// モック store
// -----------------------------------------------------------------------

type mockStore struct {
	expiredKeys     []string
	totalChunks     int
	lruKeys         []store.KeyLRUInfo
	deletedKeys     []string
	listExpiredErr  error
	totalChunksErr  error
	listLRUErr      error
	deleteErrForKey map[string]error
}

func (m *mockStore) ListExpiredKeysByTTL(_ context.Context, _ int) ([]string, error) {
	return m.expiredKeys, m.listExpiredErr
}
func (m *mockStore) TotalChunkCount(_ context.Context) (int, error) {
	return m.totalChunks, m.totalChunksErr
}
func (m *mockStore) ListKeysByLRU(_ context.Context) ([]store.KeyLRUInfo, error) {
	return m.lruKeys, m.listLRUErr
}
func (m *mockStore) DeleteKey(_ context.Context, key string) error {
	if err, ok := m.deleteErrForKey[key]; ok {
		return err
	}
	m.deletedKeys = append(m.deletedKeys, key)
	return nil
}

// -----------------------------------------------------------------------
// Config 正規化
// -----------------------------------------------------------------------

func TestNew_AppliesDefaults(t *testing.T) {
	w := New(&mockStore{}, Config{})
	if w.cfg.IntervalSecs != 3600 {
		t.Errorf("IntervalSecs default = %d, want 3600", w.cfg.IntervalSecs)
	}
	if w.cfg.TTLDays != 30 {
		t.Errorf("TTLDays default = %d, want 30", w.cfg.TTLDays)
	}
	if w.cfg.MaxChunks != 10000 {
		t.Errorf("MaxChunks default = %d, want 10000", w.cfg.MaxChunks)
	}
}

func TestNew_PreservesPositiveValues(t *testing.T) {
	w := New(&mockStore{}, Config{IntervalSecs: 60, TTLDays: 7, MaxChunks: 500})
	if w.cfg.IntervalSecs != 60 || w.cfg.TTLDays != 7 || w.cfg.MaxChunks != 500 {
		t.Errorf("config = %+v, want preserved", w.cfg)
	}
}

func TestNew_NegativeValuesFallbackToDefault(t *testing.T) {
	w := New(&mockStore{}, Config{IntervalSecs: -1, TTLDays: -1, MaxChunks: -1})
	if w.cfg.IntervalSecs != 3600 || w.cfg.TTLDays != 30 || w.cfg.MaxChunks != 10000 {
		t.Errorf("config = %+v, want defaults", w.cfg)
	}
}

// -----------------------------------------------------------------------
// ライフサイクル
// -----------------------------------------------------------------------

func TestStart_StopsOnContextCancel(t *testing.T) {
	w := New(&mockStore{}, Config{IntervalSecs: 3600})
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Start(ctx)
	}()
	cancel()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Start did not return after ctx cancel")
	}
}

// -----------------------------------------------------------------------
// runTTL
// -----------------------------------------------------------------------

func TestRunTTL_DeletesExpiredKeys(t *testing.T) {
	m := &mockStore{expiredKeys: []string{"K1", "K2"}}
	w := New(m, Config{TTLDays: 30})

	if err := w.runTTL(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, want := m.deletedKeys, []string{"K1", "K2"}; !equalSlice(got, want) {
		t.Errorf("deleted = %v, want %v", got, want)
	}
}

func TestRunTTL_NoExpiredKeys_Noop(t *testing.T) {
	m := &mockStore{}
	w := New(m, Config{TTLDays: 30})
	if err := w.runTTL(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(m.deletedKeys) != 0 {
		t.Errorf("deleted = %v, want empty", m.deletedKeys)
	}
}

func TestRunTTL_PartialDeleteFailure_Continues(t *testing.T) {
	m := &mockStore{
		expiredKeys:     []string{"K1", "K2", "K3"},
		deleteErrForKey: map[string]error{"K2": errors.New("delete fail")},
	}
	w := New(m, Config{TTLDays: 30})

	if err := w.runTTL(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, want := m.deletedKeys, []string{"K1", "K3"}; !equalSlice(got, want) {
		t.Errorf("deleted = %v, want %v", got, want)
	}

	// silent failure 禁止: 個別 KEY 失敗が Stats.LastKeyErrors で観測可能
	stats := w.Stats()
	if len(stats.LastKeyErrors) != 1 {
		t.Fatalf("LastKeyErrors len = %d, want 1", len(stats.LastKeyErrors))
	}
	got := stats.LastKeyErrors[0]
	if got.Key != "K2" || got.Phase != "ttl" || got.Err == "" {
		t.Errorf("LastKeyErrors[0] = %+v, want {Key=K2 Phase=ttl Err=...}", got)
	}
}

func TestRunTTL_ListError_Propagates(t *testing.T) {
	m := &mockStore{listExpiredErr: errors.New("db down")}
	w := New(m, Config{TTLDays: 30})
	if err := w.runTTL(context.Background()); err == nil {
		t.Fatal("want error when ListExpiredKeysByTTL fails")
	}
}

// -----------------------------------------------------------------------
// runLRU
// -----------------------------------------------------------------------

func TestRunLRU_UnderLimit_Noop(t *testing.T) {
	m := &mockStore{totalChunks: 50}
	w := New(m, Config{MaxChunks: 100})
	if err := w.runLRU(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(m.deletedKeys) != 0 {
		t.Errorf("deleted = %v, want none (under limit)", m.deletedKeys)
	}
}

func TestRunLRU_DeletesOldestUntilUnderLimit(t *testing.T) {
	// total=150, max=100 → 50 chunks 削減が必要
	// 古い順: K1(30), K2(40), K3(50), K4(30)
	// K1+K2 = 70 削減で total=80 となり停止する
	m := &mockStore{
		totalChunks: 150,
		lruKeys: []store.KeyLRUInfo{
			{Key: "K1", ChunkCount: 30},
			{Key: "K2", ChunkCount: 40},
			{Key: "K3", ChunkCount: 50},
			{Key: "K4", ChunkCount: 30},
		},
	}
	w := New(m, Config{MaxChunks: 100})

	if err := w.runLRU(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, want := m.deletedKeys, []string{"K1", "K2"}; !equalSlice(got, want) {
		t.Errorf("deleted = %v, want %v (oldest first until under max)", got, want)
	}
}

func TestRunLRU_PartialDeleteFailure_Continues(t *testing.T) {
	m := &mockStore{
		totalChunks: 150,
		lruKeys: []store.KeyLRUInfo{
			{Key: "K1", ChunkCount: 100},
			{Key: "K2", ChunkCount: 30},
			{Key: "K3", ChunkCount: 30},
		},
		deleteErrForKey: map[string]error{"K1": errors.New("fail")},
	}
	w := New(m, Config{MaxChunks: 100})

	if err := w.runLRU(context.Background()); err != nil {
		t.Fatal(err)
	}
	// K1 失敗で total=150 のまま → K2 削除で 120、K3 削除で 90 → 上限以下で停止
	if got, want := m.deletedKeys, []string{"K2", "K3"}; !equalSlice(got, want) {
		t.Errorf("deleted = %v, want %v", got, want)
	}
}

func TestRunLRU_TotalCountError_Propagates(t *testing.T) {
	m := &mockStore{totalChunksErr: errors.New("count fail")}
	w := New(m, Config{MaxChunks: 100})
	if err := w.runLRU(context.Background()); err == nil {
		t.Fatal("want error when TotalChunkCount fails")
	}
}

// -----------------------------------------------------------------------
// runOnce
// -----------------------------------------------------------------------

func TestRunOnce_BothPathsExecuted(t *testing.T) {
	m := &mockStore{
		expiredKeys: []string{"TTL-K"},
		totalChunks: 200,
		lruKeys:     []store.KeyLRUInfo{{Key: "LRU-K", ChunkCount: 150}},
	}
	w := New(m, Config{TTLDays: 30, MaxChunks: 100})

	if err := w.runOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"TTL-K", "LRU-K"}
	if !equalSlice(m.deletedKeys, want) {
		t.Errorf("deleted = %v, want %v", m.deletedKeys, want)
	}
}

// -----------------------------------------------------------------------
// ユーティリティ
// -----------------------------------------------------------------------

func equalSlice(a, b []string) bool {
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
