package trash

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
	trashedKeys []store.TrashedKeyInfo
	pending     []store.PendingDeletionEntry

	listTrashedErr error
	listPendingErr error

	deleteErrForKey map[string]error // DeleteKey を失敗させる
	sweepErrForKey  map[string]error // SweepOnePendingDeletion を失敗させる
	lockErrForKey   map[string]error // WithKeyLock 自体を失敗させる（fn は実行しない）

	// restoredKeys は IsTrashed が false を返す KEY（WithKeyLock 取得前に RestoreKey で
	// 復活済みという TOCTOU シナリオのシミュレーション用）。
	restoredKeys map[string]bool
	isTrashedErr error

	// gotCutoff は ListPendingDeletionsOlderThan に渡された cutoff を記録する
	// （保持期間から正しく算出されていることの検証用）。
	gotCutoff time.Time

	deletedKeys  []string
	sweptEntries []store.PendingDeletionEntry

	// calls は WithKeyLock / DeleteKey / SweepOnePendingDeletion の呼び出し順序を記録する
	// （DES-001 §4.3 の「呼び出し元がロックで囲む」不変条件の検証用）。
	calls []string
}

func (m *mockStore) ListTrashedKeys(_ context.Context) ([]store.TrashedKeyInfo, error) {
	return m.trashedKeys, m.listTrashedErr
}

// IsTrashed はデフォルトで true（ゴミ箱状態を維持）を返す。restoredKeys[key] が true の
// 場合のみ false を返し、WithKeyLock 取得前に RestoreKey で復活済みだったケースを模擬する。
func (m *mockStore) IsTrashed(_ context.Context, key string) (bool, error) {
	if m.isTrashedErr != nil {
		return false, m.isTrashedErr
	}
	if m.restoredKeys != nil && m.restoredKeys[key] {
		return false, nil
	}
	return true, nil
}

func (m *mockStore) ListPendingDeletionsOlderThan(_ context.Context, cutoff time.Time) ([]store.PendingDeletionEntry, error) {
	m.gotCutoff = cutoff
	return m.pending, m.listPendingErr
}

func (m *mockStore) SweepOnePendingDeletion(_ context.Context, entry store.PendingDeletionEntry) error {
	m.calls = append(m.calls, "SweepOnePendingDeletion:"+entry.Key)
	if err, ok := m.sweepErrForKey[entry.Key]; ok {
		return err
	}
	m.sweptEntries = append(m.sweptEntries, entry)
	return nil
}

func (m *mockStore) DeleteKey(_ context.Context, key string) error {
	m.calls = append(m.calls, "DeleteKey:"+key)
	if err, ok := m.deleteErrForKey[key]; ok {
		return err
	}
	m.deletedKeys = append(m.deletedKeys, key)
	return nil
}

func (m *mockStore) WithKeyLock(_ context.Context, key string, fn func() error) error {
	m.calls = append(m.calls, "WithKeyLock:"+key)
	if err, ok := m.lockErrForKey[key]; ok {
		return err
	}
	err := fn()
	m.calls = append(m.calls, "Unlock:"+key)
	return err
}

// rfc3339 は指定した「現在からの経過日数」だけ過去の RFC3339 文字列を返す。
func rfc3339DaysAgo(days float64) string {
	return time.Now().Add(-time.Duration(days * float64(24*time.Hour))).UTC().Format(time.RFC3339)
}

// -----------------------------------------------------------------------
// Config 正規化
// -----------------------------------------------------------------------

func TestNew_AppliesDefaults(t *testing.T) {
	w := New(&mockStore{}, Config{})
	if w.cfg.IntervalSeconds != 3600 {
		t.Errorf("IntervalSeconds default = %d, want 3600", w.cfg.IntervalSeconds)
	}
	if w.cfg.RetentionDays != 3 {
		t.Errorf("RetentionDays default = %d, want 3", w.cfg.RetentionDays)
	}
}

func TestNew_PreservesPositiveValues(t *testing.T) {
	w := New(&mockStore{}, Config{IntervalSeconds: 60, RetentionDays: 7})
	if w.cfg.IntervalSeconds != 60 || w.cfg.RetentionDays != 7 {
		t.Errorf("config = %+v, want preserved", w.cfg)
	}
}

// -----------------------------------------------------------------------
// sweepTrashedKeys: 保持期間超過分のみ処理する
// -----------------------------------------------------------------------

func TestSweepTrashedKeys_OnlyProcessesRetentionExceeded(t *testing.T) {
	ms := &mockStore{
		trashedKeys: []store.TrashedKeyInfo{
			{Key: "old-key", TrashedAt: rfc3339DaysAgo(5)},        // 保持期間 (3日) 超過 → 処理対象
			{Key: "fresh-key", TrashedAt: rfc3339DaysAgo(1)},      // 保持期間内 → 未処理
			{Key: "boundary-key", TrashedAt: rfc3339DaysAgo(3.5)}, // 超過 → 処理対象
		},
	}
	w := New(ms, Config{RetentionDays: 3, IntervalSeconds: 3600})

	if err := w.sweepTrashedKeys(context.Background()); err != nil {
		t.Fatalf("sweepTrashedKeys returned error: %v", err)
	}

	wantDeleted := map[string]bool{"old-key": true, "boundary-key": true}
	if len(ms.deletedKeys) != len(wantDeleted) {
		t.Fatalf("deletedKeys = %v, want keys %v", ms.deletedKeys, wantDeleted)
	}
	for _, k := range ms.deletedKeys {
		if !wantDeleted[k] {
			t.Errorf("unexpected key deleted: %s", k)
		}
	}
	for _, k := range ms.deletedKeys {
		if k == "fresh-key" {
			t.Errorf("fresh-key (未超過) が削除された")
		}
	}

	// fresh-key に対して WithKeyLock/DeleteKey が一切呼ばれていないこと
	for _, c := range ms.calls {
		if c == "WithKeyLock:fresh-key" || c == "DeleteKey:fresh-key" {
			t.Errorf("未超過 KEY に対する呼び出しが発生した: %s", c)
		}
	}
}

func TestSweepTrashedKeys_NoTrashedKeys_NoOp(t *testing.T) {
	ms := &mockStore{}
	w := New(ms, Config{})
	if err := w.sweepTrashedKeys(context.Background()); err != nil {
		t.Fatalf("sweepTrashedKeys returned error: %v", err)
	}
	if len(ms.calls) != 0 {
		t.Errorf("calls = %v, want empty", ms.calls)
	}
}

// -----------------------------------------------------------------------
// sweepTrashedKeys: 個別エラー時の継続動作
// -----------------------------------------------------------------------

func TestSweepTrashedKeys_PartialDeleteFailure_Continues(t *testing.T) {
	ms := &mockStore{
		trashedKeys: []store.TrashedKeyInfo{
			{Key: "k1", TrashedAt: rfc3339DaysAgo(10)},
			{Key: "k2", TrashedAt: rfc3339DaysAgo(10)},
			{Key: "k3", TrashedAt: rfc3339DaysAgo(10)},
		},
		deleteErrForKey: map[string]error{
			"k2": errors.New("delete failed"),
		},
	}
	w := New(ms, Config{RetentionDays: 3})

	if err := w.sweepTrashedKeys(context.Background()); err != nil {
		t.Fatalf("sweepTrashedKeys returned error (should continue on partial failure): %v", err)
	}

	if len(ms.deletedKeys) != 2 {
		t.Fatalf("deletedKeys = %v, want 2 entries (k1, k3)", ms.deletedKeys)
	}

	stats := w.Stats()
	if len(stats.LastKeyErrors) != 1 {
		t.Fatalf("LastKeyErrors = %+v, want 1 entry", stats.LastKeyErrors)
	}
	if stats.LastKeyErrors[0].Key != "k2" || stats.LastKeyErrors[0].Phase != "trashed_key" {
		t.Errorf("LastKeyErrors[0] = %+v, want Key=k2 Phase=trashed_key", stats.LastKeyErrors[0])
	}
}

// -----------------------------------------------------------------------
// sweepTrashedKeys: TOCTOU 対策（WithKeyLock 取得前に RestoreKey で復活済みの場合）
// -----------------------------------------------------------------------

func TestSweepTrashedKeys_RestoredBeforeLock_SkipsDelete(t *testing.T) {
	ms := &mockStore{
		trashedKeys: []store.TrashedKeyInfo{
			{Key: "restored-key", TrashedAt: rfc3339DaysAgo(10)},
			{Key: "still-trashed-key", TrashedAt: rfc3339DaysAgo(10)},
		},
		// ListTrashedKeys のスナップショット取得後、WithKeyLock 取得前に
		// restored-key がユーザーの RestoreKey 操作で復活済みという TOCTOU シナリオを模擬する。
		restoredKeys: map[string]bool{"restored-key": true},
	}
	w := New(ms, Config{RetentionDays: 3})

	if err := w.sweepTrashedKeys(context.Background()); err != nil {
		t.Fatalf("sweepTrashedKeys returned error: %v", err)
	}

	if len(ms.deletedKeys) != 1 || ms.deletedKeys[0] != "still-trashed-key" {
		t.Fatalf("deletedKeys = %v, want [still-trashed-key]（restored-key は削除されない）", ms.deletedKeys)
	}
	for _, c := range ms.calls {
		if c == "DeleteKey:restored-key" {
			t.Error("復活済み KEY に対して DeleteKey が呼ばれた（TOCTOU 対策が機能していない）")
		}
	}
	// WithKeyLock 自体は取得される（IsTrashed 再確認のため）が DeleteKey は呼ばれないこと
	lockCalled := false
	for _, c := range ms.calls {
		if c == "WithKeyLock:restored-key" {
			lockCalled = true
		}
	}
	if !lockCalled {
		t.Error("restored-key に対して WithKeyLock が呼ばれていない（再確認ロジックが機能していない）")
	}
}

func TestSweepTrashedKeys_IsTrashedCheckFails_RecordsErrorAndContinues(t *testing.T) {
	ms := &mockStore{
		trashedKeys: []store.TrashedKeyInfo{
			{Key: "k1", TrashedAt: rfc3339DaysAgo(10)},
			{Key: "k2", TrashedAt: rfc3339DaysAgo(10)},
		},
		isTrashedErr: errors.New("db down"),
	}
	w := New(ms, Config{RetentionDays: 3})

	if err := w.sweepTrashedKeys(context.Background()); err != nil {
		t.Fatalf("sweepTrashedKeys returned error (should continue on partial failure): %v", err)
	}
	if len(ms.deletedKeys) != 0 {
		t.Fatalf("deletedKeys = %v, want empty (IsTrashed 確認失敗時は削除しない)", ms.deletedKeys)
	}
	stats := w.Stats()
	if len(stats.LastKeyErrors) != 2 {
		t.Fatalf("LastKeyErrors = %+v, want 2 entries", stats.LastKeyErrors)
	}
}

func TestSweepTrashedKeys_LockFailure_Continues(t *testing.T) {
	ms := &mockStore{
		trashedKeys: []store.TrashedKeyInfo{
			{Key: "k1", TrashedAt: rfc3339DaysAgo(10)},
			{Key: "k2", TrashedAt: rfc3339DaysAgo(10)},
		},
		lockErrForKey: map[string]error{
			"k1": errors.New("lock timeout"),
		},
	}
	w := New(ms, Config{RetentionDays: 3})

	if err := w.sweepTrashedKeys(context.Background()); err != nil {
		t.Fatalf("sweepTrashedKeys returned error: %v", err)
	}
	if len(ms.deletedKeys) != 1 || ms.deletedKeys[0] != "k2" {
		t.Fatalf("deletedKeys = %v, want [k2]", ms.deletedKeys)
	}
	stats := w.Stats()
	if len(stats.LastKeyErrors) != 1 || stats.LastKeyErrors[0].Key != "k1" {
		t.Fatalf("LastKeyErrors = %+v, want 1 entry for k1", stats.LastKeyErrors)
	}
}

func TestSweepTrashedKeys_InvalidTimestamp_RecordsErrorAndContinues(t *testing.T) {
	ms := &mockStore{
		trashedKeys: []store.TrashedKeyInfo{
			{Key: "bad-ts", TrashedAt: "not-a-timestamp"},
			{Key: "k2", TrashedAt: rfc3339DaysAgo(10)},
		},
	}
	w := New(ms, Config{RetentionDays: 3})

	if err := w.sweepTrashedKeys(context.Background()); err != nil {
		t.Fatalf("sweepTrashedKeys returned error: %v", err)
	}
	if len(ms.deletedKeys) != 1 || ms.deletedKeys[0] != "k2" {
		t.Fatalf("deletedKeys = %v, want [k2]", ms.deletedKeys)
	}
	stats := w.Stats()
	if len(stats.LastKeyErrors) != 1 || stats.LastKeyErrors[0].Key != "bad-ts" {
		t.Fatalf("LastKeyErrors = %+v, want 1 entry for bad-ts", stats.LastKeyErrors)
	}
}

// -----------------------------------------------------------------------
// sweepPendingDeletions: WithKeyLock + SweepOnePendingDeletion のパターン検証
// -----------------------------------------------------------------------

func TestSweepPendingDeletions_LocksAndSweepsEachEntry(t *testing.T) {
	ms := &mockStore{
		pending: []store.PendingDeletionEntry{
			{Key: "k1", Series: "s1", Path: ""},
			{Key: "k2", Series: "s2", Path: "doc.md"},
		},
	}
	w := New(ms, Config{RetentionDays: 3})

	if err := w.sweepPendingDeletions(context.Background()); err != nil {
		t.Fatalf("sweepPendingDeletions returned error: %v", err)
	}

	if len(ms.sweptEntries) != 2 {
		t.Fatalf("sweptEntries = %+v, want 2 entries", ms.sweptEntries)
	}

	// 呼び出し元が cutoff を渡していること（保持期間から算出）
	wantMax := time.Now().Add(-time.Duration(3) * 24 * time.Hour).Add(time.Second)
	if ms.gotCutoff.After(wantMax) {
		t.Errorf("gotCutoff = %v, too recent (want <= ~%v)", ms.gotCutoff, wantMax)
	}

	// WithKeyLock で各エントリが囲まれていること
	wantCalls := []string{
		"WithKeyLock:k1", "SweepOnePendingDeletion:k1", "Unlock:k1",
		"WithKeyLock:k2", "SweepOnePendingDeletion:k2", "Unlock:k2",
	}
	if len(ms.calls) != len(wantCalls) {
		t.Fatalf("calls = %v, want %v", ms.calls, wantCalls)
	}
	for i, c := range wantCalls {
		if ms.calls[i] != c {
			t.Errorf("calls[%d] = %s, want %s", i, ms.calls[i], c)
		}
	}
}

func TestSweepPendingDeletions_NoEntries_NoOp(t *testing.T) {
	ms := &mockStore{}
	w := New(ms, Config{})
	if err := w.sweepPendingDeletions(context.Background()); err != nil {
		t.Fatalf("sweepPendingDeletions returned error: %v", err)
	}
	if len(ms.calls) != 0 {
		t.Errorf("calls = %v, want empty", ms.calls)
	}
}

// -----------------------------------------------------------------------
// sweepPendingDeletions: 個別エラー時の継続動作
// -----------------------------------------------------------------------

func TestSweepPendingDeletions_PartialFailure_Continues(t *testing.T) {
	ms := &mockStore{
		pending: []store.PendingDeletionEntry{
			{Key: "k1", Series: "s1", Path: ""},
			{Key: "k2", Series: "s2", Path: "doc.md"},
			{Key: "k3", Series: "s3", Path: "doc2.md"},
		},
		sweepErrForKey: map[string]error{
			"k2": errors.New("sweep failed"),
		},
	}
	w := New(ms, Config{RetentionDays: 3})

	if err := w.sweepPendingDeletions(context.Background()); err != nil {
		t.Fatalf("sweepPendingDeletions returned error (should continue): %v", err)
	}

	if len(ms.sweptEntries) != 2 {
		t.Fatalf("sweptEntries = %+v, want 2 entries (k1, k3)", ms.sweptEntries)
	}

	stats := w.Stats()
	if len(stats.LastKeyErrors) != 1 {
		t.Fatalf("LastKeyErrors = %+v, want 1 entry", stats.LastKeyErrors)
	}
	if stats.LastKeyErrors[0].Key != "k2" || stats.LastKeyErrors[0].Phase != "pending_deletion" {
		t.Errorf("LastKeyErrors[0] = %+v, want Key=k2 Phase=pending_deletion", stats.LastKeyErrors[0])
	}
}

func TestSweepPendingDeletions_LockFailure_Continues(t *testing.T) {
	ms := &mockStore{
		pending: []store.PendingDeletionEntry{
			{Key: "k1", Series: "s1", Path: ""},
			{Key: "k2", Series: "s2", Path: "doc.md"},
		},
		lockErrForKey: map[string]error{
			"k1": errors.New("lock timeout"),
		},
	}
	w := New(ms, Config{RetentionDays: 3})

	if err := w.sweepPendingDeletions(context.Background()); err != nil {
		t.Fatalf("sweepPendingDeletions returned error: %v", err)
	}
	if len(ms.sweptEntries) != 1 || ms.sweptEntries[0].Key != "k2" {
		t.Fatalf("sweptEntries = %+v, want 1 entry (k2)", ms.sweptEntries)
	}
	stats := w.Stats()
	if len(stats.LastKeyErrors) != 1 || stats.LastKeyErrors[0].Key != "k1" {
		t.Fatalf("LastKeyErrors = %+v, want 1 entry for k1", stats.LastKeyErrors)
	}
}

// -----------------------------------------------------------------------
// runOnce: 両経路の実行、致命的エラー、Stats リセット
// -----------------------------------------------------------------------

func TestRunOnce_BothPathsExecuted(t *testing.T) {
	ms := &mockStore{
		trashedKeys: []store.TrashedKeyInfo{
			{Key: "trashed-key", TrashedAt: rfc3339DaysAgo(10)},
		},
		pending: []store.PendingDeletionEntry{
			{Key: "pending-key", Series: "s1", Path: "doc.md"},
		},
	}
	w := New(ms, Config{RetentionDays: 3})

	if err := w.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce returned error: %v", err)
	}
	if len(ms.deletedKeys) != 1 || ms.deletedKeys[0] != "trashed-key" {
		t.Errorf("deletedKeys = %v, want [trashed-key]", ms.deletedKeys)
	}
	if len(ms.sweptEntries) != 1 || ms.sweptEntries[0].Key != "pending-key" {
		t.Errorf("sweptEntries = %+v, want [pending-key]", ms.sweptEntries)
	}
}

func TestRunOnce_ListTrashedKeysError_Propagates(t *testing.T) {
	ms := &mockStore{listTrashedErr: errors.New("db down")}
	w := New(ms, Config{})
	if err := w.runOnce(context.Background()); err == nil {
		t.Fatal("runOnce should return error when ListTrashedKeys fails")
	}
}

func TestRunOnce_ListPendingDeletionsError_Propagates(t *testing.T) {
	ms := &mockStore{listPendingErr: errors.New("db down")}
	w := New(ms, Config{})
	if err := w.runOnce(context.Background()); err == nil {
		t.Fatal("runOnce should return error when ListPendingDeletionsOlderThan fails")
	}
}

func TestRunOnce_ResetsKeyErrorsBetweenRuns(t *testing.T) {
	ms := &mockStore{
		trashedKeys: []store.TrashedKeyInfo{
			{Key: "k1", TrashedAt: rfc3339DaysAgo(10)},
		},
		deleteErrForKey: map[string]error{"k1": errors.New("boom")},
	}
	w := New(ms, Config{RetentionDays: 3})

	if err := w.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce returned error: %v", err)
	}
	if len(w.Stats().LastKeyErrors) != 1 {
		t.Fatalf("first run: LastKeyErrors = %+v, want 1 entry", w.Stats().LastKeyErrors)
	}

	// 2 回目は失敗要因を除去し、Stats がクリアされることを確認する
	ms.deleteErrForKey = nil
	ms.trashedKeys = nil
	if err := w.runOnce(context.Background()); err != nil {
		t.Fatalf("second runOnce returned error: %v", err)
	}
	if len(w.Stats().LastKeyErrors) != 0 {
		t.Errorf("second run: LastKeyErrors = %+v, want empty (reset)", w.Stats().LastKeyErrors)
	}
}
