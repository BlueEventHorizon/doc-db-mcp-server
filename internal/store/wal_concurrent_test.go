package store

// TASK-020 — WAL 並行テスト (DES-001 §11)
//
// 目的:
//   - 実ファイルバックの SQLite で WAL モードを有効にした状態の挙動を確認する
//   - 複数ゴルーチンから同時に UpsertRecord（書き込み）と GetChunksForSearch（読み取り）
//     を行い、データ競合・デッドロックが発生しないこと
//   - 既存の TestConcurrent_UpsertSerializedByMutex はインメモリ DB 不可な振る舞いの
//     検証として TempDir 内に実ファイルを作っており本要件を概ね満たすが、本テストは
//     さらに「書き込みと並行する読み取り」を加える

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestWAL_ConcurrentReadWrite は WAL モード下で書き込みと読み取りを並行実行し、
// race detector でデータ競合が検出されないことを確認する。-race フラグ付きで実行すること。
func TestWAL_ConcurrentReadWrite(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "wal_concurrent.db")
	s, err := New(dbPath, testDim)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// 検索対象がゼロだと GetChunksForSearch がすぐ抜けてしまうため事前に 1 件入れておく
	ctx := context.Background()
	if _, err := s.UpsertRecord(ctx, Record{
		Key: "K", Path: "seed", ContentHash: "h_seed", Series: "s",
		Chunks: makeChunks("seed text"),
	}); err != nil {
		t.Fatal(err)
	}

	// WAL モードが有効か確認（実ファイルでのみ有効になる）
	var mode string
	if err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode=%q, want wal", mode)
	}

	const (
		writers      = 4
		readers      = 4
		writesPerG   = 25
		readsPerG    = 50
		testDuration = 2 * time.Second
	)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	errs := make(chan error, writers+readers)

	// writers: 異なる path/content_hash を投入し続ける
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < writesPerG; i++ {
				select {
				case <-stop:
					return
				default:
				}
				_, err := s.UpsertRecord(ctx, Record{
					Key:         "K",
					Path:        fmt.Sprintf("w%d-p%d", id, i),
					ContentHash: fmt.Sprintf("h-w%d-i%d", id, i),
					Series:      "s",
					Chunks:      makeChunks(fmt.Sprintf("w%d body %d", id, i)),
				})
				if err != nil {
					errs <- fmt.Errorf("writer %d: %w", id, err)
					return
				}
			}
		}(w)
	}

	// readers: GetChunksForSearch を回し続ける（series 指定あり/なし混在）
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			series := ""
			if id%2 == 0 {
				series = "s"
			}
			for i := 0; i < readsPerG; i++ {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := s.GetChunksForSearch(ctx, "K", series); err != nil {
					errs <- fmt.Errorf("reader %d: %w", id, err)
					return
				}
			}
		}(r)
	}

	// タイムアウト保護
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(testDuration):
		close(stop)
		<-done
		t.Fatalf("did not complete within %v — possible deadlock", testDuration)
	}
	close(errs)
	for err := range errs {
		t.Errorf("concurrent op: %v", err)
	}

	// 終了後の整合性: seed + 全 writer 投入分の record が存在する
	var got int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM records WHERE key='K'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	want := 1 + writers*writesPerG
	if got != want {
		t.Errorf("records = %d, want %d (seed + writers*writesPerG)", got, want)
	}
}

// TestWAL_ConcurrentDeleteAndRead は書き込み（UpsertRecord/DeleteKey）と並行読み取りを
// 同時に行ったときにデッドロックや race が起きないことを確認する。
func TestWAL_ConcurrentDeleteAndRead(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "wal_delete.db")
	s, err := New(dbPath, testDim)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	// 複数 KEY を用意
	for _, k := range []string{"A", "B", "C", "D"} {
		if _, err := s.UpsertRecord(ctx, Record{
			Key: k, Path: "p", ContentHash: "h_" + k, Series: "s",
			Chunks: makeChunks("x", "y"),
		}); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	errs := make(chan error, 8)

	// 削除ワーカー: A,B,C,D を順に消す
	wg.Add(1)
	go func() {
		defer wg.Done()
		for _, k := range []string{"A", "B", "C", "D"} {
			if err := s.DeleteKey(ctx, k); err != nil {
				errs <- err
				return
			}
		}
	}()

	// 並行読み取り
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 30; i++ {
				if _, err := s.ListKeys(ctx); err != nil {
					errs <- err
					return
				}
				if _, err := s.TotalChunkCount(ctx); err != nil {
					errs <- err
					return
				}
			}
		}()
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("op: %v", err)
	}

	// 最終的に全 KEY 削除済み
	keys, err := s.ListKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Errorf("ListKeys after deletes: %d, want 0", len(keys))
	}
}
