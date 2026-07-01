#!/usr/bin/env python3
"""doc-db MCP Streamable HTTP を直接叩く軽量クライアント。

MCP tool ラッパー ("mcp__doc-db__*") を経由せず、http://localhost:<port>/mcp に
JSON-RPC で initialize → notifications/initialized → tools/call を送る。

依存: Python 3.9+ stdlib のみ (yaml が無くても port 抽出は正規表現で fallback)。

サブコマンド:
    query          KEY に検索クエリを投げ、hits を JSON で stdout に返す
    upsert         entries[] を local_path 経由で upsert し、結果を stdout に返す
    delete-series  KEY 内の全 record から series を除去し、結果を stdout に返す

いずれのサブコマンドも stdout に JSON、失敗時は stderr にエラー詳細を書き
non-zero exit する (silent failure 禁止方針)。
"""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
import urllib.error
import urllib.request
import uuid
from pathlib import Path

DEFAULT_PORT = 58080
CONFIG_PATH = Path.home() / ".doc-db" / "doc-db.yaml"
PROTOCOL_VERSION = "2025-03-26"


def read_port() -> int:
    """~/.doc-db/doc-db.yaml から port を抽出する。yaml 依存なし。"""
    if not CONFIG_PATH.exists():
        return DEFAULT_PORT
    try:
        text = CONFIG_PATH.read_text(encoding="utf-8")
    except OSError:
        return DEFAULT_PORT
    m = re.search(r"^\s*port\s*:\s*(\d+)", text, re.MULTILINE)
    if m:
        return int(m.group(1))
    return DEFAULT_PORT


def _parse_response(raw: bytes, content_type: str) -> dict:
    """JSON or SSE のどちらでも parse して JSON-RPC response dict を返す。"""
    body = raw.decode("utf-8", errors="replace")
    if "text/event-stream" in content_type:
        for line in body.splitlines():
            if line.startswith("data:"):
                data = line[len("data:"):].strip()
                if data:
                    return json.loads(data)
        raise RuntimeError(f"SSE レスポンスに data 行が見つかりません: {body!r}")
    return json.loads(body)


def _post(url: str, payload: dict, session_id: str | None) -> tuple[dict | None, dict]:
    """JSON-RPC を POST し、(response body, headers) を返す。notification は body None。"""
    data = json.dumps(payload).encode("utf-8")
    req = urllib.request.Request(url, data=data, method="POST")
    req.add_header("Content-Type", "application/json")
    req.add_header("Accept", "application/json, text/event-stream")
    if session_id:
        req.add_header("Mcp-Session-Id", session_id)
    try:
        with urllib.request.urlopen(req, timeout=120) as resp:
            headers = {k.lower(): v for k, v in resp.headers.items()}
            raw = resp.read()
    except urllib.error.HTTPError as e:
        raise RuntimeError(f"HTTP {e.code}: {e.read().decode('utf-8', errors='replace')}") from e
    except urllib.error.URLError as e:
        raise RuntimeError(
            f"doc-db サーバに接続できません ({url}): {e.reason}. "
            f"サーバが起動しているか確認してください: `doc-db > /tmp/doc-db.log 2>&1 &`"
        ) from e

    if not raw:
        return None, headers
    ct = headers.get("content-type", "")
    return _parse_response(raw, ct), headers


def call_tool(name: str, arguments: dict) -> dict:
    """MCP handshake を行い、tools/call を実行して result を返す。"""
    port = read_port()
    url = f"http://localhost:{port}/mcp"

    init_payload = {
        "jsonrpc": "2.0",
        "id": 1,
        "method": "initialize",
        "params": {
            "protocolVersion": PROTOCOL_VERSION,
            "capabilities": {},
            "clientInfo": {"name": "docdb-skill-client", "version": "1.0"},
        },
    }
    init_resp, headers = _post(url, init_payload, None)
    if init_resp is None or "error" in init_resp:
        raise RuntimeError(f"initialize 失敗: {init_resp}")
    session_id = headers.get("mcp-session-id")

    _post(url, {"jsonrpc": "2.0", "method": "notifications/initialized"}, session_id)

    call_payload = {
        "jsonrpc": "2.0",
        "id": 2,
        "method": "tools/call",
        "params": {"name": name, "arguments": arguments},
    }
    call_resp, _ = _post(url, call_payload, session_id)
    if call_resp is None:
        raise RuntimeError("tools/call が空レスポンスを返しました")
    if "error" in call_resp:
        raise RuntimeError(f"tools/call エラー: {call_resp['error']}")

    result = call_resp.get("result", {})

    if "structuredContent" in result:
        return result["structuredContent"]

    content = result.get("content") or []
    for item in content:
        if item.get("type") == "text":
            text = item.get("text", "")
            try:
                return json.loads(text)
            except json.JSONDecodeError:
                return {"text": text}
    return result


def cmd_query(args: argparse.Namespace) -> int:
    arguments = {
        "key": args.key,
        "query": args.query,
        "mode": args.mode,
        "top_n": args.top_n,
    }
    if args.series:
        arguments["series"] = args.series
    result = call_tool("query", arguments)
    json.dump(result, sys.stdout, ensure_ascii=False, indent=2)
    sys.stdout.write("\n")
    return 0


def cmd_upsert(args: argparse.Namespace) -> int:
    entries = json.loads(args.entries_json)
    if not isinstance(entries, list):
        raise ValueError("--entries-json はオブジェクトの配列である必要があります")
    documents = []
    for e in entries:
        if not isinstance(e, dict) or "path" not in e or "local_path" not in e:
            raise ValueError(f"entries の各要素は {{path, local_path}} を持つ必要があります: {e}")
        documents.append({"path": e["path"], "local_path": e["local_path"]})
    arguments = {
        "key": args.key,
        "series": args.series,
        "documents": documents,
    }
    result = call_tool("upsert_documents", arguments)
    json.dump(result, sys.stdout, ensure_ascii=False, indent=2)
    sys.stdout.write("\n")
    return 0


def cmd_delete_series(args: argparse.Namespace) -> int:
    arguments = {"key": args.key, "series": args.series}
    result = call_tool("delete_series", arguments)
    json.dump(result, sys.stdout, ensure_ascii=False, indent=2)
    sys.stdout.write("\n")
    return 0


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    sub = parser.add_subparsers(dest="command", required=True)

    p_query = sub.add_parser("query", help="doc-db query を実行")
    p_query.add_argument("--key", required=True)
    p_query.add_argument("--query", required=True)
    p_query.add_argument("--mode", default="all",
                         choices=["all", "rerank", "emb", "lex", "grep", "hybrid"])
    p_query.add_argument("--top-n", type=int, default=20, dest="top_n")
    p_query.add_argument("--series", default=None)
    p_query.set_defaults(func=cmd_query)

    p_up = sub.add_parser("upsert", help="doc-db upsert_documents (local_path 経路)")
    p_up.add_argument("--key", required=True)
    p_up.add_argument("--series", required=True)
    p_up.add_argument("--entries-json", required=True, dest="entries_json",
                      help="[{path, local_path}, ...] の JSON 文字列")
    p_up.set_defaults(func=cmd_upsert)

    p_ds = sub.add_parser("delete-series", help="doc-db delete_series を実行")
    p_ds.add_argument("--key", required=True)
    p_ds.add_argument("--series", required=True)
    p_ds.set_defaults(func=cmd_delete_series)

    args = parser.parse_args()
    try:
        return args.func(args)
    except Exception as e:
        print(f"ERROR: {e}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
