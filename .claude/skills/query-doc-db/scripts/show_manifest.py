#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""build-doc-db が管理する manifest (`~/.doc-db/skill-manifests/<key>.json`) を
読み出して JSON で表示する read-only スクリプト。

query-doc-db が「格納済み文書があるか」の事前判定と、doc-db サーバ未起動時の
grep フォールバック対象一覧の取得に使う。doc-db サーバとは通信しない。

Usage:
    python3 show_manifest.py [--key generic-docs]

stdout: {"status": "ok", "key": ..., "count": N, "documents": [絶対パス...]}
exit code: 0 成功 (manifest 無しは count=0) / 1 manifest 破損
"""
from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

DEFAULT_KEY = "generic-docs"
MANIFEST_DIR = Path.home() / ".doc-db" / "skill-manifests"


def manifest_path(key: str) -> Path:
    # build-doc-db/scripts/run_generic_sync.py::manifest_path と同一ロジックを維持すること
    safe = "".join(c if (c.isalnum() or c in "-_.") else "_" for c in key)
    return MANIFEST_DIR / f"{safe}.json"


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--key", default=DEFAULT_KEY)
    args = ap.parse_args()

    p = manifest_path(args.key)
    documents: "list[str]" = []
    if p.exists():
        try:
            data = json.loads(p.read_text(encoding="utf-8"))
            documents = [str(d) for d in data.get("documents", [])]
        except (json.JSONDecodeError, OSError) as e:
            print(f"ERROR: manifest の読み込みに失敗しました ({p}): {e}", file=sys.stderr)
            return 1

    # 存在しなくなったファイルは fallback grep の対象から除外して報告する
    # (manifest 自体の更新は build-doc-db 側の責務なのでここでは書き換えない)
    existing = [d for d in sorted(documents) if Path(d).is_file()]
    missing = len(documents) - len(existing)
    out = {"status": "ok", "key": args.key, "count": len(existing), "documents": existing}
    if missing:
        out["missing"] = missing
    json.dump(out, sys.stdout, ensure_ascii=False, indent=2)
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
