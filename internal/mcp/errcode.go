// エラー種別の機械可読な識別子（APP-001 ERR-01 / DES-001 §10）。
//
// 背景: handler が `fmt.Errorf` を返すと SDK は `CallToolResult{IsError: true}` に
// 包み、`content[].text` の日本語文言だけが残る（go-sdk `mcp/server.go` の
// ToolHandlerFor ラッパー）。クライアントは文言一致でしか分岐できず、文言変更で
// 静かに誤判定する。
//
// 対策: `*jsonrpc.Error` を返すと SDK はそれを JSON-RPC error としてそのまま
// 返すため、`code` / `data` に構造化した識別子を載せられる。識別子は
// **Message 先頭と Data の両方**に載せる:
//
//   - Data（正本）: JSON をパースするクライアント（SKILL の docdb_client.py 等）が
//     `error.data.code` で厳密に分岐する
//   - Message 先頭: MCP クライアント経由で `data` が AI agent へ提示されない場合でも、
//     文言の先頭トークンで判別できる（フォールバック）
package mcp

import (
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

// エラー識別子（文字列）。**公開契約であり、値を変更しない**。
// クライアントはこの値で分岐する（docs/AI_INTEGRATION_GUIDE.md に公開契約として記載）。
const (
	// ErrCodeKeyNotFound は指定 KEY が存在しないことを表す（未作成・タイポ等）。
	// クライアントは索引の作成（sync_documents / upsert_documents）へ進んでよい。
	ErrCodeKeyNotFound = "KEY_NOT_FOUND"
	// ErrCodeKeyTrashed は KEY がゴミ箱状態（trash_index 済み）であることを表す。
	// 索引を作成してはならず、restore_index による復活を案内する（FNC-007 TRS-02/03）。
	ErrCodeKeyTrashed = "KEY_TRASHED"
)

// JSON-RPC の数値コード。JSON-RPC 2.0 の予約域（-32768〜-32000）を避けた
// アプリケーション固有値。判別の正本は Data.code の文字列であり、数値は補助。
const (
	jsonrpcCodeKeyNotFound int64 = -31001
	jsonrpcCodeKeyTrashed  int64 = -31002
)

// errorData は JSON-RPC error の `data` に載せる構造化ペイロード。
type errorData struct {
	// Code は機械判別用の識別子（上記 ErrCode* のいずれか）。
	Code string `json:"code"`
	// Key は対象の KEY（判別後の案内文組み立てに使う）。
	Key string `json:"key"`
}

// newKeyError は識別子付きの JSON-RPC error を組み立てる。
// message には識別子を含まない本文を渡す（先頭への `code: ` 付与は本関数が行う）。
func newKeyError(numeric int64, code, key, message string) error {
	data, err := json.Marshal(errorData{Code: code, Key: key})
	if err != nil {
		// errorData は固定構造で marshal は失敗しない。仮に失敗しても
		// data を捨てて Message 側の識別子だけで判別可能な状態を保つ
		// （silent failure 禁止方針のため理由は Message に残す）。
		return &jsonrpc.Error{
			Code:    numeric,
			Message: fmt.Sprintf("%s: %s (error data の marshal に失敗: %v)", code, message, err),
		}
	}
	return &jsonrpc.Error{
		Code:    numeric,
		Message: fmt.Sprintf("%s: %s", code, message),
		Data:    data,
	}
}

// errKeyNotFound は KEY 不在エラーを返す。
func errKeyNotFound(key string) error {
	return newKeyError(jsonrpcCodeKeyNotFound, ErrCodeKeyNotFound, key,
		fmt.Sprintf("key %q が存在しません", key))
}

// errKeyTrashed は KEY がゴミ箱状態であることを表すエラーを返す。
func errKeyTrashed(key string) error {
	return newKeyError(jsonrpcCodeKeyTrashed, ErrCodeKeyTrashed, key,
		fmt.Sprintf("key %q はゴミ箱に入っています。restore_index で復活してから操作してください", key))
}
