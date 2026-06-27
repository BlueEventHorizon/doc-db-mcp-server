package expiry

// TASK-021 — 廃棄ポリシー統合テスト (DES-001 §11)
//
// 実 store を使い、Worker.runOnce を呼んだときの TTL/LRU 削除動作を検証する:
//   - EXP-01: last_accessed_at を過去日時に置いた KEY が TTL ワーカーで削除される
//   - EXP-02: MaxChunks 超過時に最古アクセス KEY から削除される
//   - EXP-04: manage_index 相当の KEY 固有ポリシー (ttl_days override) が default より優先
//   - 廃棄済み KEY への query 相当（KeyExists=false）の確認

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/BlueEventHorizon/doc-db-mcp-server/internal/store"
)

const testDim = 3

func newStore(t *testing.T) *store.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "expiry_integration.db")
	s, err := store.New(dbPath, testDim)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func makeChunks(n int) []store.ChunkInput {
	out := make([]store.ChunkInput, n)
	for i := 0; i < n; i++ {
		out[i] = store.ChunkInput{
			ChunkIndex:  i,
			HeadingPath: "# H",
			Text:        "text",
			Vector:      []float32{float32(i + 1), 0.5, -0.5},
		}
	}
	return out
}

// store の内部 db に直接アクセスできないため、last_accessed_at の操作は
// store が提供しているテストフックがない場合、テスト用ヘルパを追加するか
// store パッケージ内のテストから別経路で動作確認する必要がある。
// ここでは store の SQL を direct exec できる test-only ヘルパを使う。
// → 既存の store_test.go では同パッケージ内なので db フィールドに触れているが
//   別パッケージからは触れない。代替として store.RawExec のような export を作るのは
//   過剰なので、本テストでは store パッケージ側にすでに用意されている
//   ListExpiredKeysByTTL のセマンティクスを利用し、SetExpiryPolicy で
//   override 経路の挙動を確認する形に絞る。

// EXP-04: KEY 固有 ttl_days override が default より優先される。
// 5 日 override + 10 日経過状態が再現できないため、ここでは override の方が
// short い（=1 日）KEY と、default 30 日でかつ last_accessed_at の操作ができない
// 通常 KEY の対比という形で確認する。
//
// 実際の TTL 経過状態の検証は store パッケージ内テスト
// TestListExpiredKeysByTTL_DefaultAndOverride（store_test.go）が SQL 直叩きで
// 同一仕様を網羅しているため、ここでは「Worker.runTTL が ListExpiredKeysByTTL の
// 結果を実際に DeleteKey まで持っていく」結線部分を確認する。

func TestExpiryIntegration_TTL_DeletesExpiredKey(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	// 2 KEY 投入: KEEP と DROP
	for _, k := range []string{"KEEP", "DROP"} {
		if _, err := s.UpsertRecord(ctx, store.Record{
			Key: k, Path: "p", ContentHash: "h_" + k, Series: "s",
			Chunks: makeChunks(2),
		}); err != nil {
			t.Fatal(err)
		}
	}

	// DROP の last_accessed_at を 100 日前に backdate
	if err := backdateKeyAccess(ctx, s, "DROP", 100); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	w := New(s, Config{TTLDays: 30, MaxChunks: 99999})
	if err := w.runTTL(ctx); err != nil {
		t.Fatalf("runTTL: %v", err)
	}

	// KEEP は残る / DROP は消える
	if exists, _ := s.KeyExists(ctx, "KEEP"); !exists {
		t.Error("KEEP should remain")
	}
	if exists, _ := s.KeyExists(ctx, "DROP"); exists {
		t.Error("DROP should be deleted by TTL")
	}
}

// EXP-04: KEY 固有 ttl_days override
func TestExpiryIntegration_TTL_PerKeyOverride(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	for _, k := range []string{"DEFAULT_PROTECTED", "OVERRIDE_SHORT"} {
		if _, err := s.UpsertRecord(ctx, store.Record{
			Key: k, Path: "p", ContentHash: "h_" + k, Series: "s",
			Chunks: makeChunks(1),
		}); err != nil {
			t.Fatal(err)
		}
	}

	// 両方とも 10 日経過
	if err := backdateKeyAccess(ctx, s, "DEFAULT_PROTECTED", 10); err != nil {
		t.Fatal(err)
	}
	if err := backdateKeyAccess(ctx, s, "OVERRIDE_SHORT", 10); err != nil {
		t.Fatal(err)
	}

	// OVERRIDE_SHORT は ttl_days=5 を設定 → 10 日 > 5 日で expire
	ttl := 5
	if err := s.SetExpiryPolicy(ctx, "OVERRIDE_SHORT", &store.ExpiryPolicy{TTLDays: &ttl}); err != nil {
		t.Fatal(err)
	}

	w := New(s, Config{TTLDays: 30, MaxChunks: 99999})
	if err := w.runTTL(ctx); err != nil {
		t.Fatal(err)
	}

	// DEFAULT_PROTECTED: 10 日 < default 30 日 → 残る
	if exists, _ := s.KeyExists(ctx, "DEFAULT_PROTECTED"); !exists {
		t.Error("DEFAULT_PROTECTED should remain (10d < 30d default)")
	}
	// OVERRIDE_SHORT: override ttl_days=5 → 削除
	if exists, _ := s.KeyExists(ctx, "OVERRIDE_SHORT"); exists {
		t.Error("OVERRIDE_SHORT should be deleted (override 5d)")
	}
}

// EXP-02: LRU で MaxChunks 超過時に最古アクセス KEY から削除される
func TestExpiryIntegration_LRU_DeletesOldestUntilUnderMax(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	// 3 KEY × 各 4 chunks = 12 chunks
	for _, k := range []string{"OLD", "MID", "NEW"} {
		if _, err := s.UpsertRecord(ctx, store.Record{
			Key: k, Path: "p", ContentHash: "h_" + k, Series: "s",
			Chunks: makeChunks(4),
		}); err != nil {
			t.Fatal(err)
		}
	}

	// アクセス時刻を傾斜させる
	if err := backdateKeyAccessHours(ctx, s, "OLD", 5); err != nil {
		t.Fatal(err)
	}
	if err := backdateKeyAccessHours(ctx, s, "MID", 3); err != nil {
		t.Fatal(err)
	}
	if err := backdateKeyAccessHours(ctx, s, "NEW", 1); err != nil {
		t.Fatal(err)
	}

	// 上限 6 → 12 から 6 以下に。OLD(4) を削除して合計 8 → さらに MID(4) を削除して 4 で停止
	w := New(s, Config{TTLDays: 99999, MaxChunks: 6})
	if err := w.runLRU(ctx); err != nil {
		t.Fatal(err)
	}

	if exists, _ := s.KeyExists(ctx, "OLD"); exists {
		t.Error("OLD should be deleted by LRU (oldest)")
	}
	if exists, _ := s.KeyExists(ctx, "MID"); exists {
		t.Error("MID should be deleted by LRU (still over limit after OLD)")
	}
	if exists, _ := s.KeyExists(ctx, "NEW"); !exists {
		t.Error("NEW should remain (newest)")
	}

	total, err := s.TotalChunkCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if total > 6 {
		t.Errorf("TotalChunkCount = %d, should be <= 6", total)
	}
}

// 廃棄済み KEY への参照: KeyExists で false / GetChunksForSearch が空
func TestExpiryIntegration_DeletedKey_IsUnreachable(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	if _, err := s.UpsertRecord(ctx, store.Record{
		Key: "GONE", Path: "p", ContentHash: "h", Series: "s",
		Chunks: makeChunks(2),
	}); err != nil {
		t.Fatal(err)
	}
	if err := backdateKeyAccess(ctx, s, "GONE", 100); err != nil {
		t.Fatal(err)
	}

	w := New(s, Config{TTLDays: 30, MaxChunks: 99999})
	if err := w.runOnce(ctx); err != nil {
		t.Fatal(err)
	}

	exists, err := s.KeyExists(ctx, "GONE")
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("GONE should be deleted")
	}

	chunks, err := s.GetChunksForSearch(ctx, "GONE", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 0 {
		t.Errorf("GetChunksForSearch on deleted key returned %d chunks, want 0", len(chunks))
	}
}

// -----------------------------------------------------------------------
// last_accessed_at を操作するためのテストヘルパ
//
// store パッケージは external API として時刻操作を提供しない。
// テスト用に直接 db.Exec を呼ぶ手段が必要だが、Store.db は unexported。
// そのため store パッケージ側に test-only エクスポート (ExecForTest) を追加する。
// このヘルパはそのエクスポートを呼ぶ。
// -----------------------------------------------------------------------

func backdateKeyAccess(ctx context.Context, s *store.Store, key string, days int) error {
	_, err := store.ExecForTest(ctx, s,
		`UPDATE keys SET last_accessed_at = datetime('now', ?) WHERE key = ?`,
		negDays(days), key,
	)
	return err
}

func backdateKeyAccessHours(ctx context.Context, s *store.Store, key string, hours int) error {
	_, err := store.ExecForTest(ctx, s,
		`UPDATE keys SET last_accessed_at = datetime('now', ?) WHERE key = ?`,
		negHours(hours), key,
	)
	return err
}

func negDays(d int) string {
	return formatOffset(d, "days")
}

func negHours(h int) string {
	return formatOffset(h, "hours")
}

func formatOffset(n int, unit string) string {
	// 例: "-5 days" / "-3 hours"
	if n < 0 {
		n = -n
	}
	return "-" + itoa(n) + " " + unit
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	negative := n < 0
	if negative {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if negative {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
