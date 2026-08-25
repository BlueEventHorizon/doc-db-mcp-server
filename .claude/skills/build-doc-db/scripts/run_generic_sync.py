#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""汎用文書を doc-db に「蓄積」するためのラッパー (manifest 方式)。

doc-db の `sync_documents` (v0.2.0+) は desired-state 同期であり、渡した一覧が
「当該 key・series の完全な現在状態」になる。そのまま使うと呼び出しごとに
前回格納分が切り離されてしまうため、本スクリプトは登録済み文書の一覧を
manifest (`~/.doc-db/skill-manifests/<key>.json`) に永続化し、**毎回 manifest
全体を sync する**ことで「追加で貯める」セマンティクスを実現する。

- 文書識別子 (doc-db の path) は**絶対パス**を使う。KEY はプロジェクト横断の
  グローバルな格納庫として扱うため、プロジェクト相対パスでは衝突し得る
- manifest に載っているがディスク上に存在しなくなったファイルは自動 prune され、
  sync により series から切り離される (削除追従)
- series はサーバ API 上必須のため固定値 "main" を使う (汎用文書は branch 版管理をしない)

Usage:
    python3 run_generic_sync.py add <file|dir>... [--key KEY] [--series S]
                                [--exts .md,.markdown,.txt] [--start-only]
                                [--wait 600] [--timeout 600]
    python3 run_generic_sync.py remove <file|dir>... [--key KEY] [...]
    python3 run_generic_sync.py resync [--key KEY] [...]   # manifest 全体を再同期
    python3 run_generic_sync.py list [--key KEY]           # manifest の内容を表示

- add:    指定 path 群を manifest にマージして全体を sync (dir は --exts で再帰列挙。
          ファイルを直接指定した場合は拡張子に関わらず対象に含める)
- remove: 指定 path 群 (dir なら配下全件) を manifest から除去して全体を sync。
          結果 0 件の sync (全切り離し) も明示操作なので許可する
- resync: manifest をそのまま sync (削除ファイルの prune + 内容変更の再 embedding)
- list:   manifest の JSON を stdout に出す (通信しない)

stdout: 結果 JSON / stderr: 進捗・エラー

exit code (--start-only 未指定時):
    0  ジョブ done かつ failed=0
    1  接続失敗 (サーバ未起動 / v0.2.0 未満) / 引数エラー
    2  ジョブ failed / 一部ドキュメント失敗 / 完了待ちタイムアウト
exit code (--start-only 指定時): 0 投入成功 / 1 接続失敗・引数エラー
"""
from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import docdb_client  # noqa: E402

DEFAULT_KEY = "generic-docs"
DEFAULT_SERIES = "main"
DEFAULT_EXTS = ".md,.markdown,.txt"
MANIFEST_DIR = Path.home() / ".doc-db" / "skill-manifests"


def manifest_path(key: str) -> Path:
    # KEY は opaque だが '/' 等を含むとパス崩壊するため安全化する
    safe = "".join(c if (c.isalnum() or c in "-_.") else "_" for c in key)
    return MANIFEST_DIR / f"{safe}.json"


def load_manifest(key: str) -> "list[str]":
    p = manifest_path(key)
    if not p.exists():
        return []
    try:
        data = json.loads(p.read_text(encoding="utf-8"))
    except (json.JSONDecodeError, OSError) as e:
        print(f"ERROR: manifest の読み込みに失敗しました ({p}): {e}", file=sys.stderr)
        raise SystemExit(1)
    docs = data.get("documents", [])
    if not isinstance(docs, list):
        print(f"ERROR: manifest の形式が不正です ({p}): documents が配列ではありません",
              file=sys.stderr)
        raise SystemExit(1)
    return [str(d) for d in docs]


def save_manifest(key: str, series: str, documents: "list[str]") -> None:
    MANIFEST_DIR.mkdir(parents=True, exist_ok=True)
    manifest_path(key).write_text(
        json.dumps({"key": key, "series": series, "documents": sorted(documents)},
                   ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8")


def expand_paths(raw_paths: "list[str]", exts: "set[str]") -> "list[str]":
    """file / dir を絶対パスのファイル一覧に展開する。

    dir は exts に一致するファイルを再帰列挙。file 直指定は拡張子を問わず含める。
    存在しない path は即エラー (推測で埋めない)。
    """
    files: "set[str]" = set()
    for raw in raw_paths:
        p = Path(raw).expanduser().resolve()
        if p.is_file():
            files.add(str(p))
        elif p.is_dir():
            for f in sorted(p.rglob("*")):
                if f.is_file() and f.suffix.lower() in exts:
                    files.add(str(f.resolve()))
        else:
            print(f"ERROR: path が存在しません: {raw}", file=sys.stderr)
            raise SystemExit(1)
    return sorted(files)


def sync_all(args: argparse.Namespace, key: str, series: str,
             documents: "list[str]", extra: dict) -> int:
    """manifest 全体を sync し、成功したら manifest を保存する。"""
    entries = [{"path": p, "local_path": p} for p in documents]
    client = docdb_client.Client(timeout=args.timeout)

    def fail(msg: str) -> int:
        if "sync_documents" in msg or "not found" in msg.lower():
            msg += ("\nsync_documents は doc-db v0.2.0+ の機能です。"
                    "`brew upgrade doc-db` でサーバを更新してください。")
        print(f"ERROR: {msg}", file=sys.stderr)
        json.dump({"status": "error", "key": key, "series": series, "error": msg, **extra},
                  sys.stdout, ensure_ascii=False, indent=2)
        sys.stdout.write("\n")
        return 1

    if args.start_only:
        try:
            r = client.call("sync_documents",
                            {"key": key, "series": series, "documents": entries})
        except RuntimeError as e:
            return fail(str(e))
        job_id = r.get("job_id")
        if not job_id:
            return fail(f"sync_documents が job_id を返しませんでした: {r}")
        save_manifest(key, series, documents)
        json.dump({"status": "accepted", "job_id": job_id, "key": key, "series": series,
                   "total": len(entries), **extra}, sys.stdout, ensure_ascii=False, indent=2)
        sys.stdout.write("\n")
        return 0

    try:
        result = docdb_client.sync_and_wait(client, key, series, entries, args.wait)
    except RuntimeError as e:
        return fail(str(e))
    save_manifest(key, series, documents)
    result["key"] = key
    result["series"] = series
    result.update(extra)
    json.dump(result, sys.stdout, ensure_ascii=False, indent=2)
    sys.stdout.write("\n")
    if result.get("status") != "done":
        return 2
    return 0 if int(result.get("failed", 0) or 0) == 0 else 2


def prune_missing(documents: "list[str]") -> "tuple[list[str], list[str]]":
    """ディスク上に存在しなくなった manifest エントリを除去する。"""
    kept, pruned = [], []
    for p in documents:
        (kept if Path(p).is_file() else pruned).append(p)
    return kept, pruned


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("command", choices=["add", "remove", "resync", "list"])
    ap.add_argument("paths", nargs="*", help="add/remove の対象 file / dir")
    ap.add_argument("--key", default=DEFAULT_KEY)
    ap.add_argument("--series", default=DEFAULT_SERIES,
                    help=f"doc-db の series (デフォルト {DEFAULT_SERIES!r} 固定。"
                         f"サーバ API 上、登録時の省略は不可)")
    ap.add_argument("--exts", default=DEFAULT_EXTS,
                    help=f"dir 展開時に対象とする拡張子 (デフォルト {DEFAULT_EXTS})")
    ap.add_argument("--start-only", action="store_true", dest="start_only",
                    help="sync を投入し job_id を即座に返して終了する "
                         "(呼び出し側が docdb_client.py sync-status をループする前提)")
    ap.add_argument("--wait", type=int, default=docdb_client.DEFAULT_TIMEOUT)
    ap.add_argument("--timeout", type=int, default=docdb_client.DEFAULT_TIMEOUT)
    args = ap.parse_args()

    exts = {e if e.startswith(".") else f".{e}" for e in args.exts.lower().split(",") if e}
    manifest = load_manifest(args.key)

    if args.command == "list":
        json.dump({"status": "ok", "key": args.key, "count": len(manifest),
                   "documents": sorted(manifest)}, sys.stdout, ensure_ascii=False, indent=2)
        sys.stdout.write("\n")
        return 0

    if args.command in ("add", "remove") and not args.paths:
        print(f"ERROR: {args.command} には対象 path を 1 つ以上指定してください",
              file=sys.stderr)
        return 1
    if args.command == "resync" and args.paths:
        print("ERROR: resync は path を取りません (manifest 全体を再同期します)",
              file=sys.stderr)
        return 1

    if args.command == "add":
        new_files = expand_paths(args.paths, exts)
        if not new_files:
            print(f"ERROR: 指定 path から対象ファイルが見つかりません "
                  f"(対象拡張子: {sorted(exts)})", file=sys.stderr)
            return 1
        merged = sorted(set(manifest) | set(new_files))
        added = sorted(set(new_files) - set(manifest))
        kept, pruned = prune_missing(merged)
        return sync_all(args, args.key, args.series, kept,
                        {"added": len(added), "pruned": len(pruned)})

    if args.command == "remove":
        # remove は「manifest から外す」操作なので、ディスクに無い path も除去対象にする
        targets: "set[str]" = set()
        for raw in args.paths:
            p = Path(raw).expanduser().resolve()
            sp = str(p)
            if p.is_dir():
                prefix = sp.rstrip("/") + "/"
                targets |= {m for m in manifest if m.startswith(prefix)}
            else:
                targets.add(sp)
        removed = sorted(set(manifest) & targets)
        unknown = sorted(targets - set(manifest))
        if unknown:
            print(f"WARN: manifest に無い path を無視します: {unknown}", file=sys.stderr)
        if not removed:
            print("ERROR: 指定 path はいずれも manifest に登録されていません", file=sys.stderr)
            return 1
        remaining = [m for m in manifest if m not in targets]
        kept, pruned = prune_missing(remaining)
        # remaining が 0 件でも sync する (全切り離し)。remove は明示操作のため許可
        return sync_all(args, args.key, args.series, kept,
                        {"removed": len(removed), "pruned": len(pruned)})

    # resync
    if not manifest:
        json.dump({"status": "ok", "key": args.key, "total": 0,
                   "message": "manifest が空です。先に add で文書を格納してください。"},
                  sys.stdout, ensure_ascii=False, indent=2)
        sys.stdout.write("\n")
        return 0
    kept, pruned = prune_missing(manifest)
    return sync_all(args, args.key, args.series, kept, {"pruned": len(pruned)})


if __name__ == "__main__":
    sys.exit(main())
