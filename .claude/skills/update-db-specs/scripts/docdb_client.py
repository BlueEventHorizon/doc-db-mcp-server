#!/usr/bin/env python3
"""doc-db MCP Streamable HTTP を直接叩く軽量クライアント。

MCP tool ラッパー ("mcp__doc-db__*") を経由せず、http://localhost:<port>/mcp に
JSON-RPC で initialize → notifications/initialized → tools/call を送る。

依存: Python 3.9+ stdlib のみ。

サブコマンド:
    query          KEY に検索クエリを投げ、hits を JSON で stdout に返す
    upsert         entries[] を local_path 経由で upsert する。デフォルトで 30 件
                   ごとにバッチ分割し、進捗を stderr に表示。集約結果を JSON で
                   stdout に返す。
                   注意: 全バッチをこのプロセス内で連続実行するため、大量ファイル
                   (200+) では Claude Code の Bash tool デフォルト timeout (2分) を
                   超えうる。Claude Code から呼ぶ場合は upsert-batch を使うこと。
    upsert-batch   entries[] のうち 1 バッチ分 (--offset/--limit で指定) だけを
                   処理して即 return する。呼び出し側 (SKILL/AI) が全体をループする
                   前提の低レベル API。1 呼び出しは通常 30 秒未満で完了するため、
                   Bash tool のデフォルト timeout に依存せず動作する。
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
import time
import urllib.error
import urllib.request
from pathlib import Path

DEFAULT_PORT = 58080
DEFAULT_BATCH_SIZE = 30
DEFAULT_TIMEOUT = 600
CONFIG_PATH = Path.home() / ".doc-db" / "doc-db.yaml"
PROTOCOL_VERSION = "2025-03-26"


def read_port() -> int:
    """~/.doc-db/doc-db.yaml から port を抽出する (yaml 依存なし、正規表現)。"""
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


def _post(url: str, payload: dict, session_id: str | None, timeout: int) -> tuple[dict | None, dict]:
    """JSON-RPC を POST し、(response body, headers) を返す。notification は body None。"""
    data = json.dumps(payload).encode("utf-8")
    req = urllib.request.Request(url, data=data, method="POST")
    req.add_header("Content-Type", "application/json")
    req.add_header("Accept", "application/json, text/event-stream")
    if session_id:
        req.add_header("Mcp-Session-Id", session_id)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
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


class Client:
    """MCP session を保持する軽量クライアント。同一セッションで複数の tools/call を発行する。"""

    def __init__(self, timeout: int = DEFAULT_TIMEOUT):
        self.url = f"http://localhost:{read_port()}/mcp"
        self.timeout = timeout
        self.session_id: str | None = None
        self._call_id = 1

    def initialize(self) -> None:
        payload = {
            "jsonrpc": "2.0",
            "id": self._call_id,
            "method": "initialize",
            "params": {
                "protocolVersion": PROTOCOL_VERSION,
                "capabilities": {},
                "clientInfo": {"name": "docdb-skill-client", "version": "1.0"},
            },
        }
        self._call_id += 1
        resp, headers = _post(self.url, payload, None, self.timeout)
        if resp is None or "error" in resp:
            raise RuntimeError(f"initialize 失敗: {resp}")
        self.session_id = headers.get("mcp-session-id")
        _post(self.url, {"jsonrpc": "2.0", "method": "notifications/initialized"},
              self.session_id, self.timeout)

    def call(self, name: str, arguments: dict) -> dict:
        if self.session_id is None:
            self.initialize()
        payload = {
            "jsonrpc": "2.0",
            "id": self._call_id,
            "method": "tools/call",
            "params": {"name": name, "arguments": arguments},
        }
        self._call_id += 1
        resp, _ = _post(self.url, payload, self.session_id, self.timeout)
        if resp is None:
            raise RuntimeError("tools/call が空レスポンスを返しました")
        if "error" in resp:
            raise RuntimeError(f"tools/call エラー: {resp['error']}")
        result = resp.get("result", {})

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
    client = Client(timeout=args.timeout)
    result = client.call("query", arguments)
    json.dump(result, sys.stdout, ensure_ascii=False, indent=2)
    sys.stdout.write("\n")
    return 0


def _normalize_entries(raw) -> list[dict]:
    if not isinstance(raw, list):
        raise ValueError("--entries-json はオブジェクトの配列である必要があります")
    documents = []
    for e in raw:
        if not isinstance(e, dict) or "path" not in e or "local_path" not in e:
            raise ValueError(f"entries の各要素は {{path, local_path}} を持つ必要があります: {e}")
        documents.append({"path": e["path"], "local_path": e["local_path"]})
    return documents


def cmd_upsert(args: argparse.Namespace) -> int:
    documents = _normalize_entries(json.loads(args.entries_json))
    total = len(documents)
    batch_size = max(1, args.batch_size)
    client = Client(timeout=args.timeout)

    aggregated = {"processed": 0, "skipped": 0, "failed": 0, "errors": [], "warnings": []}
    started = time.monotonic()

    if total == 0:
        print("(no entries to upsert)", file=sys.stderr)
    else:
        n_batches = (total + batch_size - 1) // batch_size
        print(f"upsert start: total={total} batches={n_batches} batch_size={batch_size} "
              f"key={args.key} series={args.series}", file=sys.stderr)

        for i in range(0, total, batch_size):
            chunk = documents[i:i + batch_size]
            done_before = min(i + batch_size, total)
            arguments = {
                "key": args.key,
                "series": args.series,
                "documents": chunk,
            }
            t0 = time.monotonic()
            try:
                r = client.call("upsert_documents", arguments)
            except Exception as e:
                aggregated["failed"] += len(chunk)
                aggregated["errors"].append({"batch_start": i, "batch_size": len(chunk),
                                             "error": str(e)})
                elapsed = time.monotonic() - t0
                print(f"[{done_before:>4}/{total}] BATCH FAILED ({elapsed:5.1f}s): {e}",
                      file=sys.stderr)
                continue

            for k in ("processed", "skipped", "failed"):
                aggregated[k] += int(r.get(k, 0) or 0)
            for k in ("errors", "warnings"):
                v = r.get(k)
                if isinstance(v, list):
                    aggregated[k].extend(v)

            elapsed = time.monotonic() - t0
            cum_elapsed = time.monotonic() - started
            rate = done_before / cum_elapsed if cum_elapsed > 0 else 0
            eta = (total - done_before) / rate if rate > 0 else 0
            print(f"[{done_before:>4}/{total}] "
                  f"processed={aggregated['processed']:>4} "
                  f"skipped={aggregated['skipped']:>4} "
                  f"failed={aggregated['failed']:>3} "
                  f"({elapsed:5.1f}s / batch, "
                  f"ETA {eta:6.0f}s)", file=sys.stderr)

        total_elapsed = time.monotonic() - started
        print(f"upsert done: total_elapsed={total_elapsed:.1f}s", file=sys.stderr)

    json.dump(aggregated, sys.stdout, ensure_ascii=False, indent=2)
    sys.stdout.write("\n")
    return 0 if aggregated["failed"] == 0 else 2


def cmd_upsert_batch(args: argparse.Namespace) -> int:
    """entries[] のうち [offset:offset+limit] だけを 1 回の tools/call で処理する。

    Claude Code の Bash tool は既定で 2 分 (最大 10 分) の timeout を持つ。
    大量ファイルを 1 プロセスの内部ループで処理する `upsert` はこの timeout に
    かかりうるため、呼び出し側 (SKILL.md の AI) がバッチ単位でこのサブコマンドを
    繰り返し呼ぶことで、1 回あたりの実行時間を timeout の範囲内に収める。
    """
    documents = _normalize_entries(json.loads(args.entries_json))
    total = len(documents)
    offset = max(0, args.offset)
    limit = max(1, args.limit)
    chunk = documents[offset:offset + limit]

    result = {"offset": offset, "limit": limit, "total": total, "batch_count": len(chunk),
              "processed": 0, "skipped": 0, "failed": 0, "errors": [], "warnings": []}

    if not chunk:
        json.dump(result, sys.stdout, ensure_ascii=False, indent=2)
        sys.stdout.write("\n")
        return 0

    client = Client(timeout=args.timeout)
    arguments = {"key": args.key, "series": args.series, "documents": chunk}
    r = client.call("upsert_documents", arguments)

    for k in ("processed", "skipped", "failed"):
        result[k] = int(r.get(k, 0) or 0)
    for k in ("errors", "warnings"):
        v = r.get(k)
        if isinstance(v, list):
            result[k] = v

    json.dump(result, sys.stdout, ensure_ascii=False, indent=2)
    sys.stdout.write("\n")
    return 0 if result["failed"] == 0 else 2


def cmd_delete_series(args: argparse.Namespace) -> int:
    client = Client(timeout=args.timeout)
    result = client.call("delete_series", {"key": args.key, "series": args.series})
    json.dump(result, sys.stdout, ensure_ascii=False, indent=2)
    sys.stdout.write("\n")
    return 0


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--timeout", type=int, default=DEFAULT_TIMEOUT,
                        help=f"HTTP タイムアウト秒 (デフォルト {DEFAULT_TIMEOUT})")
    sub = parser.add_subparsers(dest="command", required=True)

    p_query = sub.add_parser("query", help="doc-db query を実行")
    p_query.add_argument("--key", required=True)
    p_query.add_argument("--query", required=True)
    p_query.add_argument("--mode", default="all",
                         choices=["all", "rerank", "emb", "lex", "grep", "hybrid"])
    p_query.add_argument("--top-n", type=int, default=20, dest="top_n")
    p_query.add_argument("--series", default=None)
    p_query.set_defaults(func=cmd_query)

    p_up = sub.add_parser("upsert",
                          help="doc-db upsert_documents (local_path 経路、バッチ分割 + 進捗表示)")
    p_up.add_argument("--key", required=True)
    p_up.add_argument("--series", required=True)
    p_up.add_argument("--entries-json", required=True, dest="entries_json",
                      help="[{path, local_path}, ...] の JSON 文字列")
    p_up.add_argument("--batch-size", type=int, default=DEFAULT_BATCH_SIZE,
                      dest="batch_size",
                      help=f"1 リクエストあたりのドキュメント数 (デフォルト {DEFAULT_BATCH_SIZE})")
    p_up.set_defaults(func=cmd_upsert)

    p_ub = sub.add_parser("upsert-batch",
                          help="entries[] の 1 バッチ分 (offset/limit) だけを処理して即 return する。"
                               "Claude Code から呼ぶ場合はこちらを SKILL 側でループすること。")
    p_ub.add_argument("--key", required=True)
    p_ub.add_argument("--series", required=True)
    p_ub.add_argument("--entries-json", required=True, dest="entries_json",
                      help="[{path, local_path}, ...] の JSON 文字列 (全件)")
    p_ub.add_argument("--offset", type=int, required=True, help="開始インデックス (0始まり)")
    p_ub.add_argument("--limit", type=int, default=DEFAULT_BATCH_SIZE,
                      help=f"このバッチで処理する件数 (デフォルト {DEFAULT_BATCH_SIZE})")
    p_ub.set_defaults(func=cmd_upsert_batch)

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
