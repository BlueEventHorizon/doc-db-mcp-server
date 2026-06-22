// package mcp は MCP ツールハンドラ（upsert/delete/query/manage）を担う。
// スキャフォールドプレースホルダー: 実装時に置き換える。
//
// DIF-02 経路の実装方針（DES-001 §4.2, §4.3）:
// UpsertHandler で AppendSeries + CleanOtherSeries を個別に呼ぶと、
// 2 呼び出し間で Mutex が外れて競合が発生する（finding 2 参照）。
// 必ず store.AppendAndCleanSeries を使用すること。
package mcp

import _ "github.com/modelcontextprotocol/go-sdk/mcp" // MCP go-sdk (Streamable HTTP transport)
