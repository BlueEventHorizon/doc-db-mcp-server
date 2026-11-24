#!/usr/bin/env bash
# リリースタグが main の第1親履歴にあり、タグ時点の VERSION と Formula が一致することを検証する。
# タグ直後は --tag-only を使用する。配布前の通常実行では作業中の Formula の固定 SHA も検証する。
# 使用方法: ./scripts/verify_release_tag.sh [--tag-only] [version] [main_ref]

set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

fail() {
  echo "ERROR: $*" >&2
  exit 1
}

formula_field() {
  sed -nE "s/^[[:space:]]*$1:[[:space:]]*\"([^\"]+)\"[,]?[[:space:]]*$/\1/p"
}

tag_only=false
if [[ ${1:-} == --tag-only ]]; then
  tag_only=true
  shift
fi
[[ $# -le 2 ]] || fail "使用方法: $0 [--tag-only] [version] [main_ref]"

canonical=$(tr -d '\n' < VERSION)
version=${1:-$canonical}
[[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail "不正なバージョン: '$version'"
if [[ $# -ge 2 ]]; then
  main_ref=$2
elif git show-ref --verify --quiet refs/heads/main; then
  main_ref=refs/heads/main
else
  main_ref=refs/remotes/origin/main
fi
main_commit=$(git rev-parse --verify --end-of-options "${main_ref}^{commit}" 2>/dev/null) \
  || fail "main の参照 '$main_ref' が見つかりません"

expected_tag="v${version}"
tag_commit=$(git rev-parse --verify "refs/tags/${expected_tag}^{commit}" 2>/dev/null) \
  || fail "タグ '$expected_tag' が見つかりません"
if ! git rev-list --first-parent "$main_commit" | grep -x "$tag_commit" >/dev/null; then
  fail "タグ '$expected_tag' ($tag_commit) は '$main_ref' の第1親履歴上にありません"
fi
echo "ok    $expected_tag ($tag_commit) は $main_ref の第1親履歴上にあります"

tag_version=$(git show "${tag_commit}:VERSION") || fail "タグ時点の VERSION がありません"
[[ $tag_version == "$version" ]] || fail "タグ時点の VERSION '$tag_version' が '$version' と不一致です"

formula_path=Formula/doc-db.rb
tag_formula=$(git show "${tag_commit}:${formula_path}") || fail "タグ時点の Formula がありません"
tag_formula_tag=$(formula_field tag <<< "$tag_formula")
[[ $tag_formula_tag == "$expected_tag" ]] || fail "タグ時点の Formula tag '$tag_formula_tag' が '$expected_tag' と不一致です"
echo "ok    タグ時点の VERSION と Formula tag が一致します"

if [[ $tag_only == false ]]; then
  [[ $canonical == "$version" ]] || fail "現在の VERSION '$canonical' が '$version' と不一致です"
  formula_tag=$(formula_field tag < "$formula_path")
  formula_revision=$(formula_field revision < "$formula_path")
  [[ $formula_tag == "$expected_tag" ]] || fail "現在の Formula tag '$formula_tag' が '$expected_tag' と不一致です"
  [[ $formula_revision == "$tag_commit" ]] \
    || fail "現在の Formula revision '$formula_revision' がタグの SHA '$tag_commit' と不一致です"
  echo "ok    現在の Formula が $expected_tag を SHA で固定しています"
fi
