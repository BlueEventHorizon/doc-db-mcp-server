package mcp

// TASK-008 — 既存ハンドラの WithKeyLock 排他性 統合テスト (DES-003 §3.5 / APP-003 SYN-08)
//
// internal/store/store_test.go の WithKeyLock 直接テスト（チャネル同期パターン）を
// 「fn の中身がハンドラ実体」の形に移植し、MCP ハンドラ層で KEY 単位排他が
// 実際に効いていることを検証する:
//   - 同一 KEY に対する upsert_documents / delete_documents / delete_index は直列化される
//   - 異なる KEY に対する操作は互いにブロックされない
//
// ハンドラをロック保持状態で停止させる手段として、fakeEmbedder をラップした
// gatedEmbedder を使う（upsert_documents の DIF-03 経路は WithKeyLock の fn 内で
// Embed を呼ぶため、Embed を停止させれば upsert がロックを保持したまま止まる）。
// delete_index / delete_documents の fn（DeleteKey / DeleteSeries）には注入点がなく、
// これらを「長時間ホルダー」にすることは実装無改造では決定的に実現できない。
// このため delete_index × upsert の排他は「upsert 保持中に delete_index がブロック
// される」方向で検証する（WithKeyLock は KEY ごとの単一ロックであり排他は対称。
// ホルダー側/待機側の入れ替わりに依存しない性質は
// store_test.go::TestWithKeyLock_SameKeyBlocks が直接保証している）。
// 加えて完了後の DB 状態（KEY 消滅 = DeleteKey が upsert の書き込み後に実行）で
// 直列化の順序そのものも観測する。

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BlueEventHorizon/doc-db-mcp-server/internal/embedder"
)

// keyLockITTimeout はテストが hang した場合の全体保護タイムアウト
// （store_test.go の keyLockTestTimeout と同じ値）。
const keyLockITTimeout = 5 * time.Second

// keyLockGateMarker を本文に含むドキュメントの Embed 呼び出しのみをゲートで停止させる。
// マーカー無しのドキュメント（seed 投入・別 KEY への upsert）はゲートを素通りするため、
// 1 つの gatedEmbedder で「特定の upsert だけをロック保持中に停止させる」ことができる。
const keyLockGateMarker = "KEYLOCK-GATE"

// gatedEmbedder は fakeEmbedder をラップし、マーカー入りテキストの Embed 呼び出しを
// entered 通知 + release 待機で停止させる（ハンドラを WithKeyLock 保持中に止める仕掛け）。
type gatedEmbedder struct {
	inner   embedder.Embedder
	entered chan struct{} // マーカー入り Embed がゲートに到達した通知（buffered 1）
	release chan struct{} // ゲート解放トリガー（close で解放）
}

func (g *gatedEmbedder) Embed(ctx context.Context, texts []string) ([]embedder.Vector, []int, error) {
	for _, t := range texts {
		if strings.Contains(t, keyLockGateMarker) {
			g.entered <- struct{}{}
			<-g.release
			break
		}
	}
	return g.inner.Embed(ctx, texts)
}

// installGate は harness の embedder を gatedEmbedder でラップして差し替える
// （newHarness の構成は変えず、upsert_integration_test.go の spyEmbedder と同じ差し替え方式）。
// release は冪等で、テスト失敗時も t.Cleanup 経由で必ず呼ばれ goroutine leak を防ぐ。
func installGate(t *testing.T, h *testHarness) (entered <-chan struct{}, release func()) {
	t.Helper()
	g := &gatedEmbedder{
		inner:   h.handlers.embedder,
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	h.handlers.embedder = g
	var once sync.Once
	rel := func() { once.Do(func() { close(g.release) }) }
	t.Cleanup(rel)
	return g.entered, rel
}

// seedUpsert はゲート対象外の内容でドキュメントを投入するテスト前提データ用ヘルパー。
func seedUpsert(t *testing.T, h *testHarness, key, series, path, content string) {
	t.Helper()
	_, out, err := h.handlers.handleUpsert(context.Background(), nil, UpsertInput{
		Key: key, Series: series,
		Documents: []UpsertDocument{{Path: path, Content: content}},
	})
	if err != nil || out.Failed != 0 {
		t.Fatalf("seed upsert(%s/%s/%s): out=%+v err=%v", key, series, path, out, err)
	}
}

// upsertOutcome は goroutine 実行した handleUpsert の結果受け渡し用。
type upsertOutcome struct {
	out UpsertResult
	err error
}

// startGatedUpsert はマーカー入り内容の upsert_documents を goroutine で起動し、
// ゲート到達（= WithKeyLock 保持中に Embed で停止した状態）まで待機してから戻る。
func startGatedUpsert(t *testing.T, h *testHarness, entered <-chan struct{}, key, series, path string) <-chan upsertOutcome {
	t.Helper()
	done := make(chan upsertOutcome, 1)
	go func() {
		_, out, err := h.handlers.handleUpsert(context.Background(), nil, UpsertInput{
			Key: key, Series: series,
			Documents: []UpsertDocument{{Path: path, Content: "# H\n" + keyLockGateMarker + " updated body"}},
		})
		done <- upsertOutcome{out: out, err: err}
	}()
	select {
	case <-entered:
		// upsert が KEY ロックを保持したまま Embed ゲートで停止した
	case <-time.After(keyLockITTimeout):
		t.Fatal("upsert がゲート（Embed）に到達しない")
	}
	return done
}

// TestKeyLockIntegration_DeleteIndexAndUpsert_SameKeySerialized は、同一 KEY への
// delete_index と upsert_documents が WithKeyLock で直列化されることを検証する
// （SYN-08。ホルダー = upsert / 待機側 = delete_index。方向の根拠は冒頭コメント参照）。
// ブロックされずに割り込むと「削除中の KEY への不整合書き込み」が再発する（DES-003 §3.5）。
func TestKeyLockIntegration_DeleteIndexAndUpsert_SameKeySerialized(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	seedUpsert(t, h, "K", "s", "p.md", "# H\nv1")
	entered, release := installGate(t, h)

	// A: upsert が KEY ロックを保持したままゲートで停止する
	aDone := startGatedUpsert(t, h, entered, "K", "s", "p.md")

	// B: 同一 KEY への delete_index は A の完了までブロックされる
	bDone := make(chan error, 1)
	go func() {
		_, _, err := h.handlers.handleDeleteIndex(ctx, nil, DeleteIndexInput{Key: "K"})
		bDone <- err
	}()

	select {
	case err := <-bDone:
		t.Fatalf("upsert がロック保持中に delete_index が完了した（排他が効いていない）err=%v", err)
	case <-time.After(100 * time.Millisecond):
		// ブロックされている（期待どおり）
	}

	// A を解放すると A → B の順で完了する
	release()
	select {
	case a := <-aDone:
		if a.err != nil {
			t.Errorf("upsert error: %v", a.err)
		}
		if a.out.Processed != 1 || a.out.Failed != 0 {
			t.Errorf("upsert out = %+v, want Processed=1 Failed=0", a.out)
		}
	case <-time.After(keyLockITTimeout):
		t.Fatal("解放後も upsert が完了しない")
	}
	select {
	case err := <-bDone:
		if err != nil {
			t.Errorf("delete_index error: %v", err)
		}
	case <-time.After(keyLockITTimeout):
		t.Fatal("upsert 完了後も delete_index が完了しない")
	}

	// 直列化の順序検証: delete_index (DeleteKey) が upsert の書き込み後に実行されたなら、
	// upsert が書いた record ごと KEY が消えている。排他が破れて DeleteKey が upsert の
	// 書き込み前に割り込んでいた場合は upsert の UpsertRecord が KEY を再生成してしまう。
	exists, err := h.store.KeyExists(ctx, "K")
	if err != nil {
		t.Fatalf("KeyExists: %v", err)
	}
	if exists {
		t.Error("delete_index 完了後も KEY が存在する（upsert の書き込みが delete に割り込んだ）")
	}
}

// TestKeyLockIntegration_UpsertBlocksDeleteDocuments_SameKey は、upsert_documents 実行中に
// 同一 KEY への delete_documents がブロックされることを検証する（SYN-08）。
func TestKeyLockIntegration_UpsertBlocksDeleteDocuments_SameKey(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	seedUpsert(t, h, "K", "s", "p.md", "# H\nv1")
	entered, release := installGate(t, h)

	// A: p.md を新内容（DIF-03 経路）で upsert し、ロック保持中にゲートで停止する
	aDone := startGatedUpsert(t, h, entered, "K", "s", "p.md")

	// B: 同一 KEY・同一 path への delete_documents は A の完了までブロックされる
	//（handleDelete の HasRecord 事前チェックはロック外だが、DeleteSeries はロック内）
	bDone := make(chan error, 1)
	var bOut DeleteResult
	go func() {
		_, out, err := h.handlers.handleDelete(ctx, nil, DeleteInput{
			Key: "K", Series: "s", Paths: []string{"p.md"},
		})
		bOut = out
		bDone <- err
	}()

	select {
	case err := <-bDone:
		t.Fatalf("upsert がロック保持中に delete_documents が完了した（排他が効いていない）err=%v", err)
	case <-time.After(100 * time.Millisecond):
		// ブロックされている（期待どおり）
	}

	release()
	select {
	case a := <-aDone:
		if a.err != nil {
			t.Errorf("upsert error: %v", a.err)
		}
		if a.out.Processed != 1 || a.out.Failed != 0 {
			t.Errorf("upsert out = %+v, want Processed=1 Failed=0", a.out)
		}
	case <-time.After(keyLockITTimeout):
		t.Fatal("解放後も upsert が完了しない")
	}
	select {
	case err := <-bDone:
		if err != nil {
			t.Errorf("delete_documents error: %v", err)
		}
		if bOut.Deleted != 1 {
			t.Errorf("delete_documents out = %+v, want Deleted=1", bOut)
		}
	case <-time.After(keyLockITTimeout):
		t.Fatal("upsert 完了後も delete_documents が完了しない")
	}

	// 直列化の順序検証: upsert(v2 書き込み) → delete の順で実行されたなら p.md は消えている。
	// 排他が破れて delete が旧 record だけを消し、後から upsert が v2 を書いた場合は
	// p.md が残ってしまう（desired-state ズレの再発形）。
	found, err := h.store.HasRecord(ctx, "K", "p.md")
	if err != nil {
		t.Fatalf("HasRecord: %v", err)
	}
	if found {
		t.Error("delete_documents 完了後も p.md が存在する（upsert の書き込みが delete に割り込んだ）")
	}
}

// TestKeyLockIntegration_DifferentKeysDoNotBlock は、KEY が異なれば操作が互いに
// ブロックされないことを検証する（並行度が KEY 単位を超えて落ちていないこと。DES-003 §3.5.3）。
func TestKeyLockIntegration_DifferentKeysDoNotBlock(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	seedUpsert(t, h, "K1", "s", "p.md", "# H\nv1")
	seedUpsert(t, h, "K2", "s", "q.md", "# H\nqqq")
	entered, release := installGate(t, h)

	// A: K1 のロックを保持したままゲートで停止する
	aDone := startGatedUpsert(t, h, entered, "K1", "s", "p.md")

	// A が K1 を保持している間でも、K2 への各操作は即座に完了する。
	// マーカー無し内容のためゲートは素通りする。
	runK2 := func(name string, fn func() error) {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- fn() }()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("%s error: %v", name, err)
			}
		case <-time.After(keyLockITTimeout):
			t.Fatalf("%s が K1 のロックにブロックされた（KEY 単位を超えた排他）", name)
		}
	}
	runK2("upsert_documents(K2)", func() error {
		_, out, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
			Key: "K2", Series: "s",
			Documents: []UpsertDocument{{Path: "r.md", Content: "# H\nrrr"}},
		})
		if err == nil && out.Failed != 0 {
			t.Errorf("upsert(K2) out = %+v, want Failed=0", out)
		}
		return err
	})
	runK2("delete_documents(K2)", func() error {
		_, _, err := h.handlers.handleDelete(ctx, nil, DeleteInput{
			Key: "K2", Series: "s", Paths: []string{"q.md"},
		})
		return err
	})
	runK2("delete_index(K2)", func() error {
		_, _, err := h.handlers.handleDeleteIndex(ctx, nil, DeleteIndexInput{Key: "K2"})
		return err
	})

	// K1 側の upsert は依然として完了していない（自分のロックを保持したまま）
	select {
	case a := <-aDone:
		t.Fatalf("解放前に K1 の upsert が完了した: %+v", a)
	default:
	}

	release()
	select {
	case a := <-aDone:
		if a.err != nil {
			t.Errorf("upsert(K1) error: %v", a.err)
		}
	case <-time.After(keyLockITTimeout):
		t.Fatal("解放後も K1 の upsert が完了しない")
	}
}
