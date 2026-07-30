#!/usr/bin/env python3
"""doc-db MCP Streamable HTTP を直接叩く軽量クライアント。

MCP tool ラッパー ("mcp__doc-db__*") を経由せず、http://localhost:<port>/mcp に
JSON-RPC で initialize → notifications/initialized → tools/call を送る。

依存: Python 3.9+ stdlib のみ。

サブコマンド:
    query          KEY に検索クエリを投げ、hits を JSON で stdout に返す。
                   --series 指定時は list_indexes で当該 series の登録を検証し、
                   未登録なら検索せず exit 3 で終了する (他 series の文書を代わりに
                   返さない = 返す結果は常に当該 series の正とする)。
                   --series 省略時は KEY 内の全 series を横断検索するが、
                   sync_documents で切り離された削除済み文書が物理削除まで混入し得る
                   (DES-001 §4.5 / APP-001 SYN-03 の既知の制約)
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
    sync           entries[] を desired-state として sync_documents に投入し、
                   get_sync_status を完了までこのプロセス内でポーリングする。
                   進捗は stderr に表示するが、Claude Code の Bash tool 経由では
                   stderr はユーザーに中継されないため、エージェントが進捗を
                   チャット上で報告する必要がある場合は sync-start/sync-status を
                   使うこと。
    sync-start     sync_documents を投入し、ポーリングせず即座に job_id を返す
                   低レベル API。呼び出し側 (SKILL/AI) が sync-status をループし、
                   そのつどテキストで進捗を報告する前提。
    sync-status    get_sync_status を 1 回だけ呼び、結果を JSON で返す
                   (ポーリングしない)。AI が間隔を空けて繰り返し呼ぶこと。
    delete-series  KEY 内の全 record から series を除去し、結果を stdout に返す
    list-indexes          list_indexes を呼び、KEY メタデータ一覧 (chunk_count 含む、
                          ゴミ箱状態の KEY は除外) を JSON で stdout に返す
    trash-index           trash_index を呼び、指定 KEY をゴミ箱状態にする
    list-trashed-indexes  list_trashed_indexes を呼び、ゴミ箱状態の KEY 一覧
                          (trashed_at / remaining_seconds 含む) を JSON で stdout に返す
    restore-index         restore_index を呼び、ゴミ箱状態の KEY を利用可能な状態へ戻す

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
            f"サーバが起動しているか確認してください: `doc-db &` "
            f"(ログはサーバー自身が ~/.doc-db/doc-db.log に書き込みます。"
            f"`doc-db --show-config` で実際のログ/DB パスを確認できます)"
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


SERIES_NOT_INDEXED_EXIT = 3


def _lookup_series(client: Client, key: str) -> "list[str] | None":
    """list_indexes から当該 KEY の series 一覧を返す。KEY が無ければ None。

    list_indexes はゴミ箱状態 (trash_index 済み) の KEY を一覧から除外するため、
    ゴミ箱投入済みの KEY も None になる。

    返る series 一覧は record に現在紐づく series から作られるため、
    **「未同期」と「同期済みだが対象文書 0 件」を区別できない**
    (sync_documents は空リストも正当な desired-state として受理し、その結果
    当該 series は全 record から切り離されて一覧から消える)。
    呼び出し側はこの両義性を前提にメッセージを組むこと。
    """
    result = client.call("list_indexes", {})
    for info in result.get("indexes") or []:
        if info.get("key") == key:
            return list(info.get("series") or [])
    return None


def cmd_query(args: argparse.Namespace) -> int:
    client = Client(timeout=args.timeout)

    # --series 指定時は検索前に登録を検証する。未登録のときに series 無指定
    # (全 series 横断) へフォールバックしないのは意図的な設計:
    #   - 当該 series は sync_documents の desired-state により「その branch の
    #     完全な現在状態」であるため、ヒット 0 件は「この branch に無い」という
    #     正しい答えである
    #   - 全 series 横断は sync で切り離された削除済み文書を物理削除まで拾い得る
    #     (APP-001 SYN-03)。フォールバックはその混入経路を開き直す
    # 検証が落ちる状態には「未同期」と「同期済みだが対象文書 0 件」の両方が含まれ、
    # list_indexes では区別できない (_lookup_series の docstring 参照)。後者でも
    # 当該 series の検索結果は 0 件のため機能的な損失はなく、メッセージで両方の
    # 可能性を提示して誤った再 update の案内を避ける。
    if args.series and args.verify_series:
        available = _lookup_series(client, args.key)
        if available is None:
            print(
                f"ERROR: KEY が見つかりません (key={args.key})。未作成、または"
                f"ゴミ箱状態 (trash_index 済み) です。update 系 SKILL で"
                f"インデックスを作成してください。",
                file=sys.stderr,
            )
            return SERIES_NOT_INDEXED_EXIT
        if args.series not in available:
            print(
                f"ERROR: この series に検索対象がありません "
                f"(key={args.key} series={args.series})。"
                f"登録済み series: {available}。"
                f"原因は (1) この branch で update 系 SKILL を未実行 (未同期)、"
                f"または (2) 同期済みだが対象文書が 0 件 のいずれかです "
                f"(list_indexes の series 一覧は record の紐付きから作られるため"
                f"両者を区別できません)。いずれの場合も当該 series の検索結果は "
                f"0 件であり、他 series の文書で代替はしません。",
                file=sys.stderr,
            )
            return SERIES_NOT_INDEXED_EXIT

    arguments = {
        "key": args.key,
        "query": args.query,
        "mode": args.mode,
        "top_n": args.top_n,
    }
    if args.series:
        arguments["series"] = args.series
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
                bar = _progress_bar(done_before, total)
                print(f"  {bar} BATCH FAILED ({elapsed:5.1f}s): {e}",
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
            bar = _progress_bar(done_before, total)
            print(f"  {bar} "
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


def _progress_bar(done: int, total: int, width: int = 30) -> str:
    """done/total の割合を █/░ のブロックで表した横長バーを返す。

    Claude Code 経由の実行では \\r 上書きが効かず、更新のたびに新しい行として
    積み上がる (その場で伸びる 1 本のバーにはならない) 制約がある。
    """
    if total <= 0:
        return "[" + "░" * width + "]   0%"
    ratio = min(done / total, 1.0)
    filled = int(width * ratio)
    bar = "█" * filled + "░" * (width - filled)
    return f"[{bar}] {int(ratio * 100):3d}%"


def sync_and_wait(client: Client, key: str, series: str, documents: list[dict],
                  wait_seconds: int, poll_interval: float = 2.0) -> dict:
    """sync_documents を 1 回投入し、get_sync_status を done/failed までポーリングする。

    sync_documents (v0.2.0+) は documents を「当該 key・series の完全な現在状態」と
    みなす desired-state 同期。一覧に無い既存 path は series から即時に切り離される
    (削除ファイルへの追従)。差分管理は upsert と同一 (hash 一致で embedding 再計算
    なし) のため、全件を毎回渡しても課金は差分のみ。job_id が即時返却され、実処理は
    サーバー側バックグラウンドで進む — バッチ分割は不要。

    返り値: 最終の get_sync_status 結果に job_id / total を加えた dict。
    ポーリングが wait_seconds を超えた場合は status="timeout" (ジョブ自体は
    サーバー側で継続しており、再実行すれば冪等に収束する)。
    """
    try:
        r = client.call("sync_documents", {"key": key, "series": series, "documents": documents})
    except RuntimeError as e:
        if "sync_documents" in str(e) or "not found" in str(e).lower():
            raise RuntimeError(
                f"{e}\nsync_documents は doc-db v0.2.0+ の機能です。"
                f"`brew upgrade doc-db` でサーバを更新してください。") from e
        raise
    job_id = r.get("job_id")
    if not job_id:
        raise RuntimeError(f"sync_documents が job_id を返しませんでした: {r}")

    print(f"sync accepted: job_id={job_id} total={len(documents)} "
          f"key={key} series={series}", file=sys.stderr)

    started = time.monotonic()
    last_line = ""
    while True:
        st = client.call("get_sync_status", {"job_id": job_id})
        status = st.get("status", "")
        done = st.get("processed", 0) + st.get("skipped", 0) + st.get("failed", 0)
        bar = _progress_bar(done, len(documents))
        line = (f"  {bar} [{status:>7}] processed={st.get('processed', 0):>4} "
                f"skipped={st.get('skipped', 0):>4} failed={st.get('failed', 0):>3} "
                f"detached={st.get('deleted_paths_marked', 0):>3}")
        if line != last_line:
            print(line, file=sys.stderr)
            last_line = line
        if status in ("done", "failed"):
            st["job_id"] = job_id
            st["total"] = len(documents)
            return st
        if time.monotonic() - started > wait_seconds:
            st["job_id"] = job_id
            st["total"] = len(documents)
            st["status"] = "timeout"
            print(f"WARNING: {wait_seconds}s 以内に完了しませんでした。ジョブは"
                  f"サーバー側で継続中です (get_sync_status job_id={job_id} で確認可、"
                  f"再実行しても DIF-02 により課金は差分のみ)", file=sys.stderr)
            return st
        time.sleep(poll_interval)


def cmd_sync(args: argparse.Namespace) -> int:
    documents = _normalize_entries(json.loads(args.entries_json))
    client = Client(timeout=args.timeout)
    result = sync_and_wait(client, args.key, args.series, documents, args.wait)
    json.dump(result, sys.stdout, ensure_ascii=False, indent=2)
    sys.stdout.write("\n")
    if result.get("status") != "done":
        return 2
    return 0 if int(result.get("failed", 0) or 0) == 0 else 2


def cmd_sync_start(args: argparse.Namespace) -> int:
    """sync_documents を 1 回投入し、ポーリングせず即座に job_id を返す。

    呼び出し側 (SKILL/AI) が sync-status を繰り返し呼んでポーリングし、
    そのつどテキストでユーザーに進捗を中継する前提の低レベル API
    (upsert-batch と同じ設計方針: このプロセスは長時間ブロックしない)。
    Bash tool 経由では stderr の進捗表示がユーザーに届かないため、
    AI が能動的にポーリング結果を報告する必要がある場合はこちらを使うこと。
    """
    documents = _normalize_entries(json.loads(args.entries_json))
    client = Client(timeout=args.timeout)
    try:
        r = client.call("sync_documents", {"key": args.key, "series": args.series, "documents": documents})
    except RuntimeError as e:
        if "sync_documents" in str(e) or "not found" in str(e).lower():
            raise RuntimeError(
                f"{e}\nsync_documents は doc-db v0.2.0+ の機能です。"
                f"`brew upgrade doc-db` でサーバを更新してください。") from e
        raise
    job_id = r.get("job_id")
    if not job_id:
        raise RuntimeError(f"sync_documents が job_id を返しませんでした: {r}")

    result = {"job_id": job_id, "key": args.key, "series": args.series, "total": len(documents)}
    json.dump(result, sys.stdout, ensure_ascii=False, indent=2)
    sys.stdout.write("\n")
    return 0


def cmd_sync_status(args: argparse.Namespace) -> int:
    """get_sync_status を 1 回だけ呼び、結果を JSON で返す (ポーリングしない)。

    呼び出し側 (SKILL/AI) がこのコマンドを間隔を空けて繰り返し呼び、
    そのつど status を読んでテキストでユーザーに進捗を中継する想定。
    """
    client = Client(timeout=args.timeout)
    result = client.call("get_sync_status", {"job_id": args.job_id})
    result["job_id"] = args.job_id
    json.dump(result, sys.stdout, ensure_ascii=False, indent=2)
    sys.stdout.write("\n")
    status = result.get("status", "")
    if status == "failed":
        return 2
    if status == "done":
        return 0 if int(result.get("failed", 0) or 0) == 0 else 2
    return 0  # running: まだ完了していない (呼び出し側が再度ポーリングする)


def cmd_delete_series(args: argparse.Namespace) -> int:
    client = Client(timeout=args.timeout)
    result = client.call("delete_series", {"key": args.key, "series": args.series})
    json.dump(result, sys.stdout, ensure_ascii=False, indent=2)
    sys.stdout.write("\n")
    return 0


def cmd_list_indexes(args: argparse.Namespace) -> int:
    client = Client(timeout=args.timeout)
    result = client.call("list_indexes", {})
    json.dump(result, sys.stdout, ensure_ascii=False, indent=2)
    sys.stdout.write("\n")
    return 0


def cmd_trash_index(args: argparse.Namespace) -> int:
    client = Client(timeout=args.timeout)
    result = client.call("trash_index", {"key": args.key})
    json.dump(result, sys.stdout, ensure_ascii=False, indent=2)
    sys.stdout.write("\n")
    return 0


def cmd_list_trashed_indexes(args: argparse.Namespace) -> int:
    client = Client(timeout=args.timeout)
    result = client.call("list_trashed_indexes", {})
    json.dump(result, sys.stdout, ensure_ascii=False, indent=2)
    sys.stdout.write("\n")
    return 0


def cmd_restore_index(args: argparse.Namespace) -> int:
    client = Client(timeout=args.timeout)
    result = client.call("restore_index", {"key": args.key})
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
    p_query.add_argument("--series", default=None,
                         help="絞り込む series (Git branch 名)。指定時は登録を検証し、"
                              "未登録なら検索せず exit 3 で終了する。"
                              "省略時は全 series 横断検索 (削除済み文書が混入し得る)")
    p_query.add_argument("--no-verify-series", action="store_false",
                         dest="verify_series",
                         help="--series の登録検証 (list_indexes) を省略する。"
                              "未登録 series を指定した場合はヒット 0 件が返る")
    p_query.set_defaults(func=cmd_query, verify_series=True)

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

    p_sync = sub.add_parser("sync",
                            help="doc-db sync_documents (v0.2.0+、desired-state 同期)。"
                                 "entries を完全な現在状態として 1 回で投入し、"
                                 "get_sync_status を完了までポーリングする。削除にも追従。")
    p_sync.add_argument("--key", required=True)
    p_sync.add_argument("--series", required=True)
    p_sync.add_argument("--entries-json", required=True, dest="entries_json",
                        help="[{path, local_path}, ...] の JSON 文字列 (当該 key・series の完全な現在状態)")
    p_sync.add_argument("--wait", type=int, default=DEFAULT_TIMEOUT,
                        help=f"ジョブ完了待ちの上限秒 (デフォルト {DEFAULT_TIMEOUT})")
    p_sync.set_defaults(func=cmd_sync)

    p_sync_start = sub.add_parser("sync-start",
                                  help="sync_documents を投入し、ポーリングせず即座に job_id を返す。"
                                       "Claude Code から呼ぶ場合は SKILL 側で sync-status をループすること"
                                       "（stderr の進捗はユーザーに届かないため）。")
    p_sync_start.add_argument("--key", required=True)
    p_sync_start.add_argument("--series", required=True)
    p_sync_start.add_argument("--entries-json", required=True, dest="entries_json",
                              help="[{path, local_path}, ...] の JSON 文字列 (当該 key・series の完全な現在状態)")
    p_sync_start.set_defaults(func=cmd_sync_start)

    p_sync_status = sub.add_parser("sync-status",
                                   help="get_sync_status を 1 回だけ呼び、結果を JSON で返す (ポーリングしない)。"
                                        "AI が間隔を空けて繰り返し呼び、そのつど進捗をテキストで報告すること。")
    p_sync_status.add_argument("--job-id", required=True, dest="job_id")
    p_sync_status.set_defaults(func=cmd_sync_status)

    p_ds = sub.add_parser("delete-series", help="doc-db delete_series を実行")
    p_ds.add_argument("--key", required=True)
    p_ds.add_argument("--series", required=True)
    p_ds.set_defaults(func=cmd_delete_series)

    p_li = sub.add_parser("list-indexes",
                          help="doc-db list_indexes を実行 (KEY メタデータ一覧、"
                               "chunk_count 含む、ゴミ箱状態の KEY は除外)")
    p_li.set_defaults(func=cmd_list_indexes)

    p_ti = sub.add_parser("trash-index",
                          help="doc-db trash_index を実行 (指定 KEY をゴミ箱状態にする)")
    p_ti.add_argument("--key", required=True)
    p_ti.set_defaults(func=cmd_trash_index)

    p_lt = sub.add_parser("list-trashed-indexes",
                          help="doc-db list_trashed_indexes を実行 (ゴミ箱状態の KEY 一覧、"
                               "trashed_at / remaining_seconds 含む)")
    p_lt.set_defaults(func=cmd_list_trashed_indexes)

    p_ri = sub.add_parser("restore-index",
                          help="doc-db restore_index を実行 (ゴミ箱状態の KEY を復活させる)")
    p_ri.add_argument("--key", required=True)
    p_ri.set_defaults(func=cmd_restore_index)

    args = parser.parse_args()
    try:
        return args.func(args)
    except Exception as e:
        print(f"ERROR: {e}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
