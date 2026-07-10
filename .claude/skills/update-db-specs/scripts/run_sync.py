#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""resolve_docs + docdb_client sync (desired-state 同期) を 1 コマンドに統合するラッパー。

doc-db v0.2.0+ の `sync_documents` を使い、対象文書一覧を「当該 key・series の
完全な現在状態」として 1 回で投入する。upsert 方式 (run_upsert.py) と異なり
**削除されたファイルにも追従する** (一覧に無い既存 path は series から即時に
切り離され、当該 series 指定の検索から直ちに消える)。

差分管理は upsert と同一 (SHA-256 一致で embedding 再計算なし) のため、
全件を毎回渡しても課金は「変更されたファイル分」のみ。実処理はサーバー側の
バックグラウンドジョブで進み、本スクリプトは get_sync_status を完了まで
ポーリングする — upsert のようなバッチ分割は不要。

Usage:
    python3 run_sync.py --type {rules|specs} [--key KEY] [--series SERIES]
                        [--wait 600] [--timeout 600]

--key / --series を省略した場合、resolve_docs.py と同じ規則で自動決定する:
    key    = "<project_name>-<type>"
    series = "<git_branch>" (git 不在 / detached HEAD 等は "main")

stdout: 最終ジョブ状態 JSON (完了レポートにそのまま使える)
stderr: 投入受付 + ポーリング毎の進捗 (例: [running] processed=12 skipped=460 ...)

Claude Code の Bash tool 経由では stderr の進捗表示はユーザーに中継されない
(エージェントには見えるがチャット上には出ない)。エージェントが途中経過を
ユーザーに報告したい場合は --start-only を使うこと: 対象文書列挙 → sync_documents
投入 → job_id を含む JSON を即座に返して終了する (完了までブロックしない)。
呼び出し側 (SKILL.md の AI) が docdb_client.py sync-status --job-id <id> を
間隔を空けて繰り返し呼び、そのつどテキストで進捗を報告する。

exit code (デフォルト、--start-only 未指定時):
    0  ジョブ done かつ failed=0 (対象 0 件を含む)
    1  接続失敗 (doc-db サーバ未起動 / v0.2.0 未満) または resolve 段階のエラー
    2  ジョブ failed / 一部ドキュメント失敗 (failed > 0) / 完了待ちタイムアウト

exit code (--start-only 指定時):
    0  投入成功 (job_id を返す。対象 0 件の場合は同期せず total=0 で終了)
    1  接続失敗 / resolve 段階のエラー
"""
from __future__ import annotations

import argparse
import json
import os
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import docdb_client  # noqa: E402
import resolve_docs  # noqa: E402


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--type", required=True, choices=["rules", "specs"])
    ap.add_argument("--key", default=None, help="省略時: <project_name>-<type>")
    ap.add_argument("--series", default=None, help="省略時: 現在の git branch (fallback: main)")
    ap.add_argument("--wait", type=int, default=docdb_client.DEFAULT_TIMEOUT,
                    help=f"ジョブ完了待ちの上限秒 (デフォルト {docdb_client.DEFAULT_TIMEOUT})")
    ap.add_argument("--timeout", type=int, default=docdb_client.DEFAULT_TIMEOUT)
    ap.add_argument("--start-only", action="store_true", dest="start_only",
                    help="sync_documents を投入し job_id を即座に返して終了する "
                         "(完了までブロックしない。呼び出し側が sync-status をループする前提)")
    args = ap.parse_args()

    project_root = Path(os.environ.get("CLAUDE_PROJECT_DIR", os.getcwd())).resolve()

    try:
        files = resolve_docs.resolve(args.type, project_root)
    except SystemExit as e:
        # resolve_docs.resolve() 内の _emit_and_exit が既に JSON を stdout に出力済み
        return int(e.code or 1)

    branch = resolve_docs.detect_git_branch(project_root)
    project_name = resolve_docs.detect_project_name(project_root)
    key = args.key or f"{project_name}-{args.type}"
    series = args.series or branch

    # 対象 0 件でも sync は正当 (「現存ファイルなし」の desired-state 宣言 = 全 path
    # 切り離し) だが、.doc_structure.yaml の設定ミスで全消しになる事故を防ぐため、
    # 0 件時は同期せずに終了する。全 path を意図的に外したい場合は
    # /delete-db-series か docdb_client.py sync --entries-json '[]' を直接使うこと。
    if not files:
        json.dump({
            "status": "ok",
            "type": args.type,
            "total": 0,
            "message": f"{args.type} 対象文書がありません。.doc_structure.yaml を確認してください"
                       f"（誤設定時の全切り離しを防ぐため、0 件では同期しません）。",
        }, sys.stdout, ensure_ascii=False, indent=2)
        sys.stdout.write("\n")
        return 0

    entries = [{"path": rel, "local_path": str(project_root / rel)} for rel in files]

    client = docdb_client.Client(timeout=args.timeout)

    if args.start_only:
        # 完了を待たず job_id を即座に返す。呼び出し側 (SKILL.md の AI) が
        # docdb_client.py sync-status --job-id <id> をループしてテキストで
        # 進捗を報告する (stderr の進捗表示はユーザーに中継されないため)。
        try:
            r = client.call("sync_documents", {"key": key, "series": series, "documents": entries})
        except RuntimeError as e:
            msg = str(e)
            if "sync_documents" in msg or "not found" in msg.lower():
                msg = (f"{msg}\nsync_documents は doc-db v0.2.0+ の機能です。"
                       f"`brew upgrade doc-db` でサーバを更新してください。")
            print(f"ERROR: {msg}", file=sys.stderr)
            json.dump({"status": "error", "key": key, "series": series, "error": msg},
                      sys.stdout, ensure_ascii=False, indent=2)
            sys.stdout.write("\n")
            return 1
        job_id = r.get("job_id")
        if not job_id:
            msg = f"sync_documents が job_id を返しませんでした: {r}"
            print(f"ERROR: {msg}", file=sys.stderr)
            json.dump({"status": "error", "key": key, "series": series, "error": msg},
                      sys.stdout, ensure_ascii=False, indent=2)
            sys.stdout.write("\n")
            return 1
        json.dump({"status": "accepted", "job_id": job_id, "key": key, "series": series,
                   "total": len(entries)}, sys.stdout, ensure_ascii=False, indent=2)
        sys.stdout.write("\n")
        return 0

    try:
        result = docdb_client.sync_and_wait(client, key, series, entries, args.wait)
    except RuntimeError as e:
        msg = str(e)
        print(f"ERROR: {msg}", file=sys.stderr)
        json.dump({"status": "error", "key": key, "series": series, "error": msg},
                  sys.stdout, ensure_ascii=False, indent=2)
        sys.stdout.write("\n")
        return 1

    result["key"] = key
    result["series"] = series
    json.dump(result, sys.stdout, ensure_ascii=False, indent=2)
    sys.stdout.write("\n")
    if result.get("status") != "done":
        return 2
    return 0 if int(result.get("failed", 0) or 0) == 0 else 2


if __name__ == "__main__":
    sys.exit(main())
