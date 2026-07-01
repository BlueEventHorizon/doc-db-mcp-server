#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
.doc_structure.yaml から <type> (rules|specs) の対象 Markdown ファイルを
project-root からの相対パスで列挙して JSON で返す。

Usage:
    python3 resolve_docs.py --type {rules|specs}

出力 (stdout):
    {"status":"ok", "type":"specs", "files":["docs/specs/.../a.md", ...]}
    {"status":"error", "message":"..."}

exclude 判定は forge SKILL と同じ「パスの任意階層の bare-name マッチ」を採用
（例: exclude=[plan] は docs/specs/xxx/plan/... を除外）。
"""
from __future__ import annotations

import argparse
import glob
import json
import os
import subprocess
import sys
from pathlib import Path

try:
    import yaml  # type: ignore
except ImportError:
    print(json.dumps({
        "status": "error",
        "message": "pyyaml が必要です。`pip install pyyaml` でインストールしてください。",
    }))
    sys.exit(1)


def detect_git_branch(project_root: Path) -> str:
    """現在の Git branch 名を返す。git repo 外 / detached HEAD / 実行失敗時は "main" を返す。

    series として使うため、path で使えない文字 (typically slashes) はそのまま残す
    (doc-db 側で opaque 文字列として扱われ、slash 含みでも許容される)。
    """
    try:
        result = subprocess.run(
            ["git", "rev-parse", "--abbrev-ref", "HEAD"],
            cwd=str(project_root),
            capture_output=True,
            text=True,
            timeout=5,
        )
        if result.returncode != 0:
            return "main"
        branch = result.stdout.strip()
        if not branch or branch == "HEAD":
            # detached HEAD 状態 → fallback
            return "main"
        return branch
    except (OSError, subprocess.TimeoutExpired):
        return "main"


def _emit_and_exit(payload: dict, code: int = 0) -> None:
    print(json.dumps(payload, ensure_ascii=False))
    sys.exit(code)


def resolve(type_key: str, project_root: Path) -> list[str]:
    cfg_path = project_root / ".doc_structure.yaml"
    if not cfg_path.exists():
        _emit_and_exit({
            "status": "error",
            "message": f".doc_structure.yaml が {project_root} に存在しません。/forge:setup-doc-structure で生成してください。",
        }, code=2)

    with open(cfg_path, encoding="utf-8") as f:
        cfg = yaml.safe_load(f) or {}

    section = cfg.get(type_key)
    if not section:
        _emit_and_exit({
            "status": "error",
            "message": f".doc_structure.yaml に '{type_key}' セクションがありません。",
        }, code=2)

    root_dirs = section.get("root_dirs", []) or []
    patterns = section.get("patterns", {}) or {}
    target_glob = patterns.get("target_glob", "**/*.md")
    excludes = patterns.get("exclude", []) or []

    files: set[str] = set()
    for root_pattern in root_dirs:
        # root_pattern はグロブを含む可能性 (例: "docs/specs/**/design/")
        # 末尾スラッシュを除去して glob するとディレクトリマッチ
        rp = root_pattern.rstrip("/")
        for match in glob.glob(str(project_root / rp), recursive=True):
            p = Path(match)
            if not p.exists():
                continue
            if p.is_dir():
                # target_glob (通常 **/*.md) で再帰列挙
                for md in p.rglob("*.md"):
                    if md.is_file():
                        rel = md.relative_to(project_root)
                        files.add(str(rel))
            elif p.is_file() and p.suffix == ".md":
                rel = p.relative_to(project_root)
                files.add(str(rel))

    # exclude 適用: bare-name パスセグメント一致
    def is_excluded(rel_path: str) -> bool:
        parts = Path(rel_path).parts
        for ex in excludes:
            ex_norm = ex.strip("/")
            if "/" in ex_norm:
                if ex_norm in rel_path:
                    return True
            else:
                if ex_norm in parts:
                    return True
        return False

    result = sorted(f for f in files if not is_excluded(f))
    return result


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--type", required=True, choices=["rules", "specs"])
    args = ap.parse_args()

    project_root = Path(os.environ.get("CLAUDE_PROJECT_DIR", os.getcwd())).resolve()

    try:
        files = resolve(args.type, project_root)
    except SystemExit:
        raise
    except Exception as e:  # noqa: BLE001
        _emit_and_exit({
            "status": "error",
            "message": f"resolve error: {e}",
        }, code=1)

    # 相対パスと絶対パスの両方を返す。upsert_documents では
    #   path       = 相対 (search 結果の表示用識別子)
    #   local_path = 絶対 (doc-db がディスクから読む)
    # として使い分ける。
    entries = [
        {"path": rel, "local_path": str(project_root / rel)}
        for rel in files
    ]
    branch = detect_git_branch(project_root)

    _emit_and_exit({
        "status": "ok",
        "type": args.type,
        "project_root": str(project_root),
        "project_name": project_root.name,
        "git_branch": branch,    # 現在の Git branch (upsert 時の series として使う推奨値)
        "files": files,          # 後方互換: 相対パスのみのリスト
        "entries": entries,      # 新: {path, local_path} オブジェクトのリスト
        "count": len(files),
    })


if __name__ == "__main__":
    main()
