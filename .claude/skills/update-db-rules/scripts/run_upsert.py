#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""resolve_docs + docdb_client upsert-batch ループを 1 コマンドに統合するラッパー。

これまで AI (SKILL 呼び出し側) が担っていた「entries を中間ファイルに保存し、
件数からバッチ数を計算し、offset をインクリメントしながら docdb_client.py を
繰り返し呼び、結果を手動で合算する」処理を 1 プロセス内で完結させる。

resolve → 全バッチ upsert が同一プロセス内で完結するため、entries JSON を
中間ファイルに書き出す必要がない (これまでの /tmp/docdb_*_entries.json は不要)。

Usage:
    python3 run_upsert.py --type {rules|specs} [--key KEY] [--series SERIES]
                           [--batch-size 30] [--timeout 600]

--key / --series を省略した場合、resolve_docs.py と同じ規則で自動決定する:
    key    = "<project_name>-<type>"
    series = "<git_branch>" (git 不在 / detached HEAD 等は "main")

stdout: 集約結果 JSON (Step 5 の完了レポートにそのまま使える)
stderr: バッチ毎の進捗 (例: [60/480] processed=2 skipped=28 failed=0)

exit code:
    0  全バッチ成功 (対象 0 件を含む)
    1  接続失敗 (doc-db サーバ未起動) または resolve 段階のエラー
    2  一部バッチが失敗 (failed > 0)
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
    ap.add_argument("--batch-size", type=int, default=docdb_client.DEFAULT_BATCH_SIZE, dest="batch_size")
    ap.add_argument("--timeout", type=int, default=docdb_client.DEFAULT_TIMEOUT)
    args = ap.parse_args()

    project_root = Path(os.environ.get("CLAUDE_PROJECT_DIR", os.getcwd())).resolve()

    try:
        files = resolve_docs.resolve(args.type, project_root)
    except SystemExit as e:
        # resolve_docs.resolve() 内の _emit_and_exit が既に JSON を stdout に出力済み
        return int(e.code or 1)

    if not files:
        json.dump({
            "status": "ok",
            "type": args.type,
            "total": 0,
            "message": f"{args.type} 対象文書がありません。.doc_structure.yaml を確認してください。",
        }, sys.stdout, ensure_ascii=False, indent=2)
        sys.stdout.write("\n")
        return 0

    entries = [{"path": rel, "local_path": str(project_root / rel)} for rel in files]
    branch = resolve_docs.detect_git_branch(project_root)
    project_name = resolve_docs.detect_project_name(project_root)
    key = args.key or f"{project_name}-{args.type}"
    series = args.series or branch

    batch_size = max(1, args.batch_size)
    total = len(entries)
    n_batches = (total + batch_size - 1) // batch_size

    aggregated = {
        "key": key, "series": series, "total": total, "batches": n_batches,
        "processed": 0, "skipped": 0, "failed": 0, "errors": [], "warnings": [],
    }

    print(f"upsert start: total={total} batches={n_batches} batch_size={batch_size} "
          f"key={key} series={series}", file=sys.stderr)

    client = docdb_client.Client(timeout=args.timeout)

    for i in range(0, total, batch_size):
        chunk = entries[i:i + batch_size]
        done = min(i + batch_size, total)
        try:
            r = client.call("upsert_documents", {"key": key, "series": series, "documents": chunk})
        except Exception as e:  # noqa: BLE001
            msg = str(e)
            if "接続できません" in msg:
                # サーバ未起動: 残りバッチを回しても無意味なので即中断する
                print(f"ERROR: {msg}", file=sys.stderr)
                return 1
            aggregated["failed"] += len(chunk)
            aggregated["errors"].append({"batch_start": i, "batch_size": len(chunk), "error": msg})
            print(f"[{done:>4}/{total}] BATCH FAILED: {msg}", file=sys.stderr)
            continue

        for k in ("processed", "skipped", "failed"):
            aggregated[k] += int(r.get(k, 0) or 0)
        for k in ("errors", "warnings"):
            v = r.get(k)
            if isinstance(v, list):
                aggregated[k].extend(v)

        print(f"[{done:>4}/{total}] processed={aggregated['processed']:>4} "
              f"skipped={aggregated['skipped']:>4} failed={aggregated['failed']:>3}", file=sys.stderr)

    json.dump(aggregated, sys.stdout, ensure_ascii=False, indent=2)
    sys.stdout.write("\n")
    return 0 if aggregated["failed"] == 0 else 2


if __name__ == "__main__":
    sys.exit(main())
