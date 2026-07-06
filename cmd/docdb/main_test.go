// main_test.go は起動時スイープの差し込み（TASK-007、GC-02〜04）の統合テスト。
//
// main() 自体は HTTP サーバー起動まで含むため直接テストせず、run() から抽出した
// startupSweep を実際の Store に対して呼び、「統計値（ListKeys / TotalChunkCount）が
// スイープ後の状態を反映する」こと（GC-03）を検証する。
// SweepPendingDeletions 自体の網羅的な単体テストは internal/store/pending_test.go にあり、
// ここでは重複する検証は行わない。
package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/BlueEventHorizon/doc-db-mcp-server/internal/store"
)

// TestStartupSweep_StatsReflectSweptState は、pending_deletions に手動投入した
// 削除予約行が startupSweep で処理され、起動時 DB 統計（run() が表示に使う
// ListKeys / TotalChunkCount）がスイープ後の状態を反映することを検証する。
func TestStartupSweep_StatsReflectSweptState(t *testing.T) {
	ctx := context.Background()

	st, err := store.New(filepath.Join(t.TempDir(), "test.db"), 3)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer st.Close()

	// 生存する record: series "main" に紐付いたまま残る
	if _, err := st.UpsertRecord(ctx, store.Record{
		Key: "K", Path: "keep.md", ContentHash: "h_keep", Series: "main",
		Chunks: []store.ChunkInput{{
			ChunkIndex: 0, HeadingPath: "# H", Text: "keep body",
			Vector: []float32{1, 0.5, -0.5},
		}},
	}); err != nil {
		t.Fatalf("UpsertRecord(keep.md): %v", err)
	}

	// スイープ対象の orphan record: series から切り離し済み（sync の欠落検出相当）
	if _, err := st.UpsertRecord(ctx, store.Record{
		Key: "K", Path: "gone.md", ContentHash: "h_gone", Series: "s1",
		Chunks: []store.ChunkInput{{
			ChunkIndex: 0, HeadingPath: "# H", Text: "gone body",
			Vector: []float32{2, 0.5, -0.5},
		}},
	}); err != nil {
		t.Fatalf("UpsertRecord(gone.md): %v", err)
	}
	if orphaned, err := st.DetachSeriesFromPath(ctx, "K", "s1", "gone.md"); err != nil || !orphaned {
		t.Fatalf("DetachSeriesFromPath: orphaned=%v err=%v, want true/nil", orphaned, err)
	}

	// 前回セッションで永続化された削除予約行を ExecForTest で手動投入する
	// （acceptance criteria: 手動投入した pending_deletions 行が起動後に消えていること）
	if _, err := store.ExecForTest(ctx, st,
		`INSERT INTO pending_deletions (key, series, path, marked_at) VALUES (?, ?, ?, ?)`,
		"K", "s1", "gone.md", "2026-01-01T00:00:00Z",
	); err != nil {
		t.Fatalf("pending_deletions 手動投入: %v", err)
	}

	// 事前確認: orphan の chunk が物理的に残っている（スイープ前は総チャンク数 2）
	if n, err := st.TotalChunkCount(ctx); err != nil || n != 2 {
		t.Fatalf("スイープ前 TotalChunkCount = %d (err=%v), want 2", n, err)
	}

	// 起動時スイープ（run() が統計算出より前に呼ぶものと同一の関数）
	processed, errCount := startupSweep(ctx, st)
	if errCount != 0 {
		t.Fatalf("startupSweep errCount = %d, want 0", errCount)
	}
	if processed != 1 {
		t.Errorf("startupSweep processed = %d, want 1", processed)
	}

	// 統計値がスイープ後の状態を反映していること（GC-03）:
	// orphan の chunk は物理削除され、総チャンク数は生存 record の 1 のみ
	if n, err := st.TotalChunkCount(ctx); err != nil || n != 1 {
		t.Errorf("スイープ後 TotalChunkCount = %d (err=%v), want 1", n, err)
	}
	keys, err := st.ListKeys(ctx)
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	if len(keys) != 1 || keys[0].Key != "K" {
		t.Fatalf("ListKeys = %+v, want key K のみ", keys)
	}
	if keys[0].DocCount != 1 {
		t.Errorf("doc_count = %d, want 1 (gone.md は物理削除済み)", keys[0].DocCount)
	}

	// 手動投入した予約行が消えていること: 2 回目のスイープは 0 件処理になる
	if processed, errCount := startupSweep(ctx, st); processed != 0 || errCount != 0 {
		t.Errorf("2回目 startupSweep = (processed=%d, errCount=%d), want (0, 0)（予約行残存の疑い）",
			processed, errCount)
	}
}
