package trash

// TASK-011 — ゴミ箱投入 → 一覧確認 → 復活 → 再投入 → 保持期間経過後の自動処分、という
// 一連のフローの統合テスト（DES-003 §7 統合テスト対象 / FNC-009〜FNC-013）。
//
// trash_test.go の mockStore を使った単体テストとは異なり、本ファイルは実 *store.Store
// （SQLite ベース、internal/store の t.TempDir() 上のファイル）を使い、Worker.runOnce が
// Store 層の実装（TrashKey/RestoreKey/ListTrashedKeys/DeleteKey/
// ListPendingDeletionsOlderThan/SweepOnePendingDeletion）と噛み合って実際に動作することを
// 検証する。保持期間は「意図的に trashed_at / marked_at を過去日時へ backdate する」ことで
// 短縮する（store.New の Config はテスト用に保持日数 0 以下を受け付けないため、Worker 側の
// RetentionDays は本番相当の値のまま、DB 側の基準時刻を過去にずらして超過状態を作る）。
//
// internal/expiry/integration_test.go 相当のものは本 feature で expiry が廃止されたため
// 存在しない。keylock_integration_test.go / sync_integration_test.go の「実 store を使う」
// 構成を参考にした。すべて同期的なローカル SQLite 操作のみで hang するおそれが無いため、
// 他統合テストのようなゴルーチン + タイムアウトガードは用いない。

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/BlueEventHorizon/doc-db-mcp-server/internal/store"
)

// newRealStore はテスト用に t.TempDir() 上へ SQLite ファイルを作り実 *store.Store を返す。
// internal/store.newTestStore は unexported のため、package trash からは直接呼べない。
func newRealStore(t *testing.T) *store.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "trash-it.db")
	s, err := store.New(dbPath, 3)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// seedRecord は key/series/path に 1 record（chunk 1件、埋め込みなし）を投入する。
func seedRecord(t *testing.T, s *store.Store, key, series, path string) {
	t.Helper()
	if _, err := s.UpsertRecord(context.Background(), store.Record{
		Key: key, Path: path, ContentHash: "h-" + path, Series: series,
		Chunks: []store.ChunkInput{{ChunkIndex: 0, HeadingPath: "# H", Text: "body"}},
	}); err != nil {
		t.Fatalf("seedRecord(%s/%s/%s): %v", key, series, path, err)
	}
}

// backdateTrashedAt は key の keys.trashed_at を ago 分だけ過去の時刻に書き換える
// （保持期間超過状態を意図的に作るためのテスト専用操作。ExecForTest 経由）。
func backdateTrashedAt(t *testing.T, s *store.Store, key string, ago time.Duration) {
	t.Helper()
	past := time.Now().Add(-ago).UTC().Format(time.RFC3339)
	if _, err := store.ExecForTest(context.Background(), s,
		`UPDATE keys SET trashed_at=? WHERE key=?`, past, key,
	); err != nil {
		t.Fatalf("backdateTrashedAt(%s): %v", key, err)
	}
}

// backdateMarkedAt は pending_deletions.marked_at を ago 分だけ過去の時刻に書き換える。
func backdateMarkedAt(t *testing.T, s *store.Store, key, series, path string, ago time.Duration) {
	t.Helper()
	past := time.Now().Add(-ago).UTC().Format(time.RFC3339)
	if _, err := store.ExecForTest(context.Background(), s,
		`UPDATE pending_deletions SET marked_at=? WHERE key=? AND series=? AND path=?`,
		past, key, series, path,
	); err != nil {
		t.Fatalf("backdateMarkedAt(%s/%s/%s): %v", key, series, path, err)
	}
}

func containsKey(keys []store.KeyInfo, key string) bool {
	for _, k := range keys {
		if k.Key == key {
			return true
		}
	}
	return false
}

func containsTrashedKey(keys []store.TrashedKeyInfo, key string) bool {
	for _, k := range keys {
		if k.Key == key {
			return true
		}
	}
	return false
}

// TestTrashIntegration_KeyLifecycle_TrashListRestoreRetrashSweep は KEY 単位ゴミ箱の
// 全ライフサイクル（DES-003 UC-2〜UC-5）を実 store で検証する:
//
//	ゴミ箱投入 → 一覧確認 → 保持期間内は runOnce で削除されない → 復活 → ListKeys に戻る →
//	再投入 → 保持期間超過 (backdate) → runOnce で実際に DeleteKey される
func TestTrashIntegration_KeyLifecycle_TrashListRestoreRetrashSweep(t *testing.T) {
	s := newRealStore(t)
	ctx := context.Background()
	const key = "K-lifecycle"

	seedRecord(t, s, key, "s1", "p.md")

	// --- ゴミ箱投入 → 一覧確認 -------------------------------------------------
	if err := s.TrashKey(ctx, key); err != nil {
		t.Fatalf("TrashKey: %v", err)
	}
	trashed, err := s.ListTrashedKeys(ctx)
	if err != nil {
		t.Fatalf("ListTrashedKeys: %v", err)
	}
	if !containsTrashedKey(trashed, key) {
		t.Fatalf("ListTrashedKeys = %+v, want containing %q", trashed, key)
	}
	// ゴミ箱投入直後は list_indexes（ListKeys）から除外される（FNC-008）
	activeKeys, err := s.ListKeys(ctx)
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	if containsKey(activeKeys, key) {
		t.Fatalf("ListKeys に trashed KEY %q が含まれている", key)
	}

	// --- 保持期間内は runOnce を実行しても削除されない --------------------------
	w := New(s, Config{RetentionDays: 3, IntervalSeconds: 3600})
	if err := w.runOnce(ctx); err != nil {
		t.Fatalf("runOnce (in-retention): %v", err)
	}
	exists, err := s.KeyExists(ctx, key)
	if err != nil {
		t.Fatalf("KeyExists: %v", err)
	}
	if !exists {
		t.Fatal("保持期間内にも関わらず KEY が削除された")
	}
	stillTrashed, err := s.IsTrashed(ctx, key)
	if err != nil {
		t.Fatalf("IsTrashed: %v", err)
	}
	if !stillTrashed {
		t.Fatal("保持期間内にも関わらず trashed_at がクリアされた")
	}

	// --- 復活 → ListKeys に戻る / ListTrashedKeys から消える --------------------
	if err := s.RestoreKey(ctx, key); err != nil {
		t.Fatalf("RestoreKey: %v", err)
	}
	activeKeys, err = s.ListKeys(ctx)
	if err != nil {
		t.Fatalf("ListKeys (after restore): %v", err)
	}
	if !containsKey(activeKeys, key) {
		t.Fatalf("復活後も ListKeys に %q が含まれない", key)
	}
	trashed, err = s.ListTrashedKeys(ctx)
	if err != nil {
		t.Fatalf("ListTrashedKeys (after restore): %v", err)
	}
	if containsTrashedKey(trashed, key) {
		t.Fatalf("復活後も ListTrashedKeys に %q が残っている", key)
	}

	// --- 再投入 → 保持期間超過（backdate）→ runOnce で実際に物理削除される -----
	if err := s.TrashKey(ctx, key); err != nil {
		t.Fatalf("TrashKey (re-trash): %v", err)
	}
	backdateTrashedAt(t, s, key, 10*24*time.Hour) // 10日前 > RetentionDays=3

	if err := w.runOnce(ctx); err != nil {
		t.Fatalf("runOnce (retention exceeded): %v", err)
	}
	exists, err = s.KeyExists(ctx, key)
	if err != nil {
		t.Fatalf("KeyExists (after sweep): %v", err)
	}
	if exists {
		t.Fatal("保持期間超過後も KEY が最終処分（DeleteKey）されていない")
	}
	activeKeys, err = s.ListKeys(ctx)
	if err != nil {
		t.Fatalf("ListKeys (after sweep): %v", err)
	}
	if containsKey(activeKeys, key) {
		t.Fatalf("最終処分後も ListKeys に %q が残っている", key)
	}
}

// TestTrashIntegration_OrphanRecordLifecycle は orphan record
// （pending_deletions、DES-003 UC-6 / FNC-013）についても KEY 単位ゴミ箱と同じ保持期間の
// 定義に従うことを実 store で検証する:
//
//	series からの切り離しで orphan 化 → 保持期間内は runOnce で削除されない →
//	保持期間超過 (backdate) → runOnce で実際に物理削除される
func TestTrashIntegration_OrphanRecordLifecycle(t *testing.T) {
	s := newRealStore(t)
	ctx := context.Background()
	const key = "K-orphan"
	const series = "s1"
	const path = "orphan.md"

	seedRecord(t, s, key, series, path)

	// series からの切り離しで record を orphan 化する（sync_documents の実運用パターン）
	orphaned, err := s.DetachSeriesFromPath(ctx, key, series, path)
	if err != nil {
		t.Fatalf("DetachSeriesFromPath: %v", err)
	}
	if !orphaned {
		t.Fatal("DetachSeriesFromPath: want orphaned=true")
	}
	if err := s.MarkDocumentForDeletion(ctx, key, series, path); err != nil {
		t.Fatalf("MarkDocumentForDeletion: %v", err)
	}

	// record 自体はまだ物理的に存在する（ゴミ箱状態、猶予期間中）
	has, err := s.HasRecord(ctx, key, path)
	if err != nil {
		t.Fatalf("HasRecord: %v", err)
	}
	if !has {
		t.Fatal("ゴミ箱投入直後に record が消えている（即時削除されてはならない）")
	}

	w := New(s, Config{RetentionDays: 3, IntervalSeconds: 3600})

	// --- 保持期間内は runOnce を実行しても削除されない --------------------------
	if err := w.runOnce(ctx); err != nil {
		t.Fatalf("runOnce (in-retention): %v", err)
	}
	has, err = s.HasRecord(ctx, key, path)
	if err != nil {
		t.Fatalf("HasRecord (in-retention): %v", err)
	}
	if !has {
		t.Fatal("保持期間内にも関わらず orphan record が削除された")
	}

	// --- 保持期間超過（backdate）→ runOnce で実際に物理削除される --------------
	backdateMarkedAt(t, s, key, series, path, 10*24*time.Hour) // 10日前 > RetentionDays=3
	if err := w.runOnce(ctx); err != nil {
		t.Fatalf("runOnce (retention exceeded): %v", err)
	}
	has, err = s.HasRecord(ctx, key, path)
	if err != nil {
		t.Fatalf("HasRecord (after sweep): %v", err)
	}
	if has {
		t.Fatal("保持期間超過後も orphan record が最終処分（物理削除）されていない")
	}
}
