// エラー識別子（ERR-01）の回帰テスト。
//
// このテストが守る不変条件: KEY 不在・ゴミ箱状態のエラーは、日本語文言ではなく
// **機械可読な識別子**で判別できること。handler が `fmt.Errorf` に戻ると
// SDK が JSON-RPC error ではなく text だけの CallToolResult に包んでしまい、
// クライアントは文言一致に退行する（このテストが落ちる）。
package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

// assertKeyError は err が期待する識別子付きの JSON-RPC error であることを検証する。
func assertKeyError(t *testing.T, err error, wantCode, wantKey string, wantNumeric int64) {
	t.Helper()
	if err == nil {
		t.Fatal("error を期待したが nil")
	}

	// SDK は *jsonrpc.Error のみ JSON-RPC error としてそのまま返す
	// （それ以外は text だけの CallToolResult に包まれ data が失われる）。
	wireErr, ok := err.(*jsonrpc.Error)
	if !ok {
		t.Fatalf("*jsonrpc.Error を期待したが %T: %v", err, err)
	}

	if wireErr.Code != wantNumeric {
		t.Errorf("Code = %d, want %d", wireErr.Code, wantNumeric)
	}

	// Message 先頭の識別子（data が AI agent へ提示されない経路のフォールバック）。
	if !strings.HasPrefix(wireErr.Message, wantCode+": ") {
		t.Errorf("Message は %q で始まるべき: %q", wantCode+": ", wireErr.Message)
	}

	// Data（判別の正本）。
	if len(wireErr.Data) == 0 {
		t.Fatal("Data が空（クライアントが厳密に分岐できない）")
	}
	var got errorData
	if uerr := json.Unmarshal(wireErr.Data, &got); uerr != nil {
		t.Fatalf("Data の unmarshal 失敗: %v (data=%s)", uerr, wireErr.Data)
	}
	if got.Code != wantCode {
		t.Errorf("Data.code = %q, want %q", got.Code, wantCode)
	}
	if got.Key != wantKey {
		t.Errorf("Data.key = %q, want %q", got.Key, wantKey)
	}
}

func TestErrKeyNotFound_HasMachineReadableCode(t *testing.T) {
	assertKeyError(t, errKeyNotFound("K"), ErrCodeKeyNotFound, "K", jsonrpcCodeKeyNotFound)
}

func TestErrKeyTrashed_HasMachineReadableCode(t *testing.T) {
	assertKeyError(t, errKeyTrashed("K"), ErrCodeKeyTrashed, "K", jsonrpcCodeKeyTrashed)
}

// query が存在しない KEY に対して KEY_NOT_FOUND を返すこと（handler 経路）。
func TestQuery_KeyNotFound_ReturnsErrorCode(t *testing.T) {
	h := newQueryHarness(t, nil)
	ctx := context.Background()

	_, _, err := h.handlers.handleQuery(ctx, nil, QueryInput{
		Query: "x", Key: "no-such-key", Mode: "lex", TopN: 5,
	})
	assertKeyError(t, err, ErrCodeKeyNotFound, "no-such-key", jsonrpcCodeKeyNotFound)
}

// ゴミ箱状態の KEY に対する query が KEY_TRASHED を返すこと（KEY_NOT_FOUND と区別される）。
func TestQuery_KeyTrashed_ReturnsErrorCode(t *testing.T) {
	h := newQueryHarness(t, nil)
	ctx := context.Background()

	h.embedder.fixed = []float32{1, 0, 0}
	if _, _, err := h.handlers.handleUpsert(ctx, nil, UpsertInput{
		Key: "K", Series: "s1",
		Documents: []UpsertDocument{{Path: "a.md", Content: "# H\nalpha"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.handlers.handleTrashIndex(ctx, nil, TrashIndexInput{Key: "K"}); err != nil {
		t.Fatal(err)
	}

	_, _, err := h.handlers.handleQuery(ctx, nil, QueryInput{
		Query: "alpha", Key: "K", Mode: "lex", TopN: 5,
	})
	assertKeyError(t, err, ErrCodeKeyTrashed, "K", jsonrpcCodeKeyTrashed)
}
