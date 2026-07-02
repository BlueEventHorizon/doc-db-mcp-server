#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
.doc_structure.yaml から <type> (rules|specs) の対象 Markdown ファイルを
project-root からの相対パスで列挙して JSON で返す。

Usage:
    python3 resolve_docs.py --type {rules|specs}

出力 (stdout):
    {"status":"ok", "type":"specs",
     "project_root":"...", "project_name":"...", "git_branch":"main",
     "files":[...], "entries":[{"path":..., "local_path":...}, ...], "count": N}
    {"status":"error", "message":"..."}

依存: Python 3.9+ stdlib のみ (PyYAML は使わない)。

YAML parser は forge の `resolve_doc_structure.py::parse_config` (行ベース) と
同アプローチ。`.doc_structure.yaml` v3.0 のサブセット (コメント / indent map /
`- item` list / `[a, b]` inline list / bare & quoted 文字列) のみ扱う。
anchor / merge / multiline scalar 等は非対応 (使われたら明示的にエラーとせず
そのまま無視されるので、`.doc_structure.yaml` の schema は forge が保証する)。

exclude 判定は「パスの任意階層の bare-name マッチ」を採用
(例: exclude=[plan] は docs/specs/xxx/plan/... を除外)。
"""
from __future__ import annotations

import argparse
import glob
import json
import os
import subprocess
import sys
from pathlib import Path


# ---------------------------------------------------------------------------
# stdlib-only YAML parser (`.doc_structure.yaml` v3.0 サブセット)
# forge の resolve_doc_structure.py::parse_config と互換。
# ---------------------------------------------------------------------------

def _parse_value(value: str):
    """YAML 値をパース (文字列 / 数値 / bool / インライン配列)。"""
    value = value.strip()
    if not value.startswith('"') and '  #' in value:
        value = value[: value.index('  #')].strip()
    if value.startswith('[') and value.endswith(']'):
        inner = value[1:-1].strip()
        if not inner:
            return []
        return [item.strip().strip('"\'') for item in inner.split(',')]
    value = value.strip('"\'')
    if value.lower() == 'true':
        return True
    if value.lower() == 'false':
        return False
    try:
        return int(value)
    except ValueError:
        pass
    return value


def _lookahead_is_list(lines, start_idx: int, parent_indent: int = 4) -> bool:
    for i in range(start_idx, min(start_idx + 10, len(lines))):
        line = lines[i]
        stripped = line.strip()
        if not stripped or stripped.startswith('#'):
            continue
        indent = len(line) - len(line.lstrip())
        if indent <= parent_indent:
            break
        if stripped.startswith('- '):
            return True
        if ':' in stripped:
            return False
    return True


def parse_config(content: str) -> dict:
    """.doc_structure.yaml v3.0 を行ベースで dict にパースする。"""
    result: dict = {}
    current_section = None
    current_subsection = None
    current_list = None
    current_dict = None
    lines = content.split('\n')

    for i, line in enumerate(lines):
        stripped = line.strip()
        if not stripped or stripped.startswith('#'):
            continue
        indent = len(line) - len(line.lstrip())

        if ':' in stripped and not stripped.startswith('- '):
            key, _, value = stripped.partition(':')
            key = key.strip().strip('"\'')
            value = value.strip()

            if indent == 0:
                current_section = key
                result[key] = {}
                current_subsection = None
                current_list = None
                current_dict = None
            elif indent == 2 and current_section:
                current_subsection = key
                if value:
                    result[current_section][key] = _parse_value(value)
                    current_list = None
                else:
                    if _lookahead_is_list(lines, i + 1, parent_indent=2):
                        result[current_section][key] = []
                        current_list = result[current_section][key]
                    else:
                        result[current_section][key] = {}
                        current_list = None
                current_dict = None
            elif indent == 4 and current_section and current_subsection:
                if value:
                    result[current_section][current_subsection][key] = _parse_value(value)
                    current_list = None
                    current_dict = None
                else:
                    if _lookahead_is_list(lines, i + 1):
                        result[current_section][current_subsection][key] = []
                        current_list = result[current_section][current_subsection][key]
                        current_dict = None
                    else:
                        result[current_section][current_subsection][key] = {}
                        current_dict = result[current_section][current_subsection][key]
                        current_list = None
            elif indent == 6 and current_dict is not None:
                current_dict[key] = _parse_value(value) if value else ''
        elif stripped.startswith('- ') and current_list is not None:
            item = stripped[2:].strip().strip('"\'')
            if '  #' in item and not item.startswith('"'):
                item = item[: item.index('  #')].strip()
            current_list.append(item)

    return result


# ---------------------------------------------------------------------------
# doc-db 固有: git branch 検出 / 出力
# ---------------------------------------------------------------------------

def detect_project_name(project_root: Path) -> str:
    """git worktree 環境でも安定した project 識別名 (KEY prefix) を返す。

    git worktree はブランチごとに別ディレクトリ (basename も別) になりうる。
    `project_root.name` を単純に使うと worktree のディレクトリ名が KEY prefix に
    化けてしまい、同一プロジェクトなのに branch (worktree) ごとに別 KEY として
    doc-db に登録されてしまう (DIF-02 の series 設計が意味を成さなくなるバグ)。

    `git rev-parse --git-common-dir` は同一 repo の全 worktree で共通の
    `.git` ディレクトリを指す。その親ディレクトリ (= 本体 repo のルート) の
    名前を使えば、どの worktree から呼んでも同じ project_name が得られる。

    git repo でない場合や git 実行に失敗した場合は、従来通り
    `project_root.name` (ディレクトリ名) にフォールバックする。
    """
    try:
        result = subprocess.run(
            ["git", "-C", str(project_root), "rev-parse", "--git-common-dir"],
            capture_output=True,
            text=True,
            timeout=5,
        )
        if result.returncode == 0:
            common_dir_str = result.stdout.strip()
            if common_dir_str:
                common_dir = Path(common_dir_str)
                if not common_dir.is_absolute():
                    common_dir = project_root / common_dir
                main_repo_root = common_dir.resolve().parent
                if main_repo_root.name:
                    return main_repo_root.name
    except (OSError, subprocess.TimeoutExpired):
        pass
    return project_root.name


def detect_git_branch(project_root: Path) -> str:
    """現在の Git branch 名を返す。git repo 外 / detached HEAD / 実行失敗時は "main"。"""
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
            return "main"
        return branch
    except (OSError, subprocess.TimeoutExpired):
        return "main"


def _emit_and_exit(payload: dict, code: int = 0) -> None:
    print(json.dumps(payload, ensure_ascii=False))
    sys.exit(code)


def _walk_md_files(root: Path) -> list[Path]:
    """root 配下の *.md をシンボリックリンクを辿って再帰的に列挙する。

    pathlib.Path.rglob は再帰の途中で遭遇した symlink ディレクトリを辿らない仕様
    (無限ループ防止のための意図的な設計) のため、monorepo で仕様/ルール文書を
    symlink 経由で共有している構成だと該当ファイルがエラーも警告もなく欠落する。
    os.walk(followlinks=True) で明示的に辿り、symlink loop (自己参照的な循環)
    対策として実体パスの訪問済みセットで検知・打ち切りする。
    """
    seen_real_dirs: set[str] = set()
    result: list[Path] = []
    for dirpath, dirnames, filenames in os.walk(root, followlinks=True):
        real = os.path.realpath(dirpath)
        if real in seen_real_dirs:
            dirnames[:] = []
            continue
        seen_real_dirs.add(real)
        for fname in filenames:
            if fname.endswith(".md"):
                result.append(Path(dirpath) / fname)
    return result


def resolve(type_key: str, project_root: Path) -> list[str]:
    cfg_path = project_root / ".doc_structure.yaml"
    if not cfg_path.exists():
        _emit_and_exit({
            "status": "error",
            "message": f".doc_structure.yaml が {project_root} に存在しません。/forge:setup-doc-structure で生成してください。",
        }, code=2)

    cfg = parse_config(cfg_path.read_text(encoding="utf-8")) or {}

    section = cfg.get(type_key)
    if not section:
        _emit_and_exit({
            "status": "error",
            "message": f".doc_structure.yaml に '{type_key}' セクションがありません。",
        }, code=2)

    root_dirs = section.get("root_dirs", []) or []
    patterns = section.get("patterns", {}) or {}
    excludes = patterns.get("exclude", []) or []

    files: set[str] = set()
    for root_pattern in root_dirs:
        rp = root_pattern.rstrip("/")
        for match in glob.glob(str(project_root / rp), recursive=True):
            p = Path(match)
            if not p.exists():
                continue
            if p.is_dir():
                for md in _walk_md_files(p):
                    if md.is_file():
                        rel = md.relative_to(project_root)
                        files.add(str(rel))
            elif p.is_file() and p.suffix == ".md":
                rel = p.relative_to(project_root)
                files.add(str(rel))

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

    return sorted(f for f in files if not is_excluded(f))


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

    entries = [
        {"path": rel, "local_path": str(project_root / rel)}
        for rel in files
    ]
    branch = detect_git_branch(project_root)
    project_name = detect_project_name(project_root)

    _emit_and_exit({
        "status": "ok",
        "type": args.type,
        "project_root": str(project_root),
        "project_name": project_name,
        "git_branch": branch,
        "files": files,
        "entries": entries,
        "count": len(files),
    })
    return 0


if __name__ == "__main__":
    main()
