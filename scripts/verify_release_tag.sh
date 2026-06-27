#!/usr/bin/env bash
# git tag v{version} が指す commit と Formula の revision が一致するか検証する
# （DES-002 §4.3 / APP-002 VER-09）。
#
# 検証項目:
#   - 期待する tag (v{canonical}) が git に存在する
#   - tag が指す commit SHA と Formula revision: の値が一致する
#
# verify_version_consistency.sh が「version 文字列の静的一致」を見るのに対し、
# 本スクリプトは「tag commit SHA == Formula revision」という git object 識別子の一致を見る。
# tag 確定後にしか検証できない項目。リリースフロー Phase 3（git tag → push 前）で実行する。
#
# 使用方法:
#   ./scripts/verify_release_tag.sh

set -uo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

# canonical
if [ ! -f VERSION ]; then
  echo "ERROR: VERSION ファイルが見つかりません" >&2
  exit 1
fi
canonical=$(tr -d '\n' < VERSION)
expected_tag="v${canonical}"
echo "expected tag: $expected_tag"

# 期待する tag が指す commit
tag_commit=$(git rev-parse "${expected_tag}^{commit}" 2>/dev/null) || {
  echo "ERROR: tag '$expected_tag' が見つかりません（tag 作成前か push されていない可能性）" >&2
  exit 1
}
echo "tag $expected_tag が指す commit: $tag_commit"

# Formula の revision
formula_path="Formula/doc-db.rb"
if [ ! -f "$formula_path" ]; then
  echo "ERROR: $formula_path が見つかりません" >&2
  exit 1
fi
formula_revision=$(grep -E '^\s*revision:' "$formula_path" \
  | sed -E 's/.*"([0-9a-f]+)".*/\1/' | head -1)
if [ -z "$formula_revision" ]; then
  echo "ERROR: $formula_path から revision: を抽出できません" >&2
  exit 1
fi

errors=0
if [ "$formula_revision" != "$tag_commit" ]; then
  echo "ERROR: $formula_path revision '$formula_revision' が tag '$expected_tag' の commit '$tag_commit' と不一致" >&2
  echo "  → Formula の revision: を $tag_commit に更新してください" >&2
  errors=$((errors + 1))
else
  echo "ok    $formula_path revision == tag $expected_tag が指す commit"
fi

if [ "$errors" -eq 0 ]; then
  echo ""
  echo "✓ Formula が tag $expected_tag ($tag_commit) と整合している"
  exit 0
else
  echo "" >&2
  echo "✗ $errors 件の不整合を検出しました" >&2
  exit 1
fi
