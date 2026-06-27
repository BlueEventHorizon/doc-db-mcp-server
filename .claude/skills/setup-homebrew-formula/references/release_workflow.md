# リリースワークフロー

初回リリースおよび version bump 後のリリース手順。Formula の `revision:` は **git tag 確定後にしか正しい値にできない** ため、整合性検証は 2 段階に分かれる。

## なぜ 2 段階なのか

| 検証 | タイミング | 検証対象 |
|------|-----------|---------|
| `verify_version_consistency.sh` | version bump 直後（tag 作成前） | canonical / CHANGELOG / .version-config.yaml / Formula `tag:` の **静的整合** |
| `verify_release_tag.sh` | git tag 作成後（push 前） | Formula `revision:` == git tag が指す commit SHA |

順序を間違えると、push 後に `brew install` が「`<version>` tag should be `<X>` but is actually `<Y>`」で失敗する。

## ステップ詳細

### Step 1: version bump

`/forge:update-version` または手動で：

- `VERSION` を新版に書き換え
- `CHANGELOG.md` の `[Unreleased]` を `[X.Y.Z] - YYYY-MM-DD` に昇格、新しい `[Unreleased]` セクションを追加
- 末尾のリンク定義 `[X.Y.Z]: .../releases/tag/v{X.Y.Z}` を追加

### Step 2: Formula tag 更新

`Formula/<name>.rb` の `tag:` を新版に書き換える：

```ruby
tag:      "v0.1.1",          # canonical と一致
revision: "0000000000000000000000000000000000000000"  # まだ placeholder
```

`.version-config.yaml` の `sync_files` で自動化する場合：

```yaml
sync_files:
  - path: Formula/doc-db.rb
    pattern: 'tag:      "v{version}"'
    filter: 'tag:'
```

### Step 3: 静的整合検証

```bash
make verify-version
# または
bash scripts/verify_version_consistency.sh
```

全項目 ok にならない限り次に進まない。

### Step 4: git commit + tag 作成

```bash
git add VERSION CHANGELOG.md Formula/<name>.rb .version-config.yaml
git commit -m "chore: bump version to X.Y.Z"
git tag vX.Y.Z
```

### Step 5: Formula revision 更新

tag が指す commit SHA を取得して Formula に埋める：

```bash
tag_commit=$(git rev-parse "vX.Y.Z^{commit}")
# Formula/<name>.rb の revision: を $tag_commit に書き換え
```

書き換え後、もう一度 commit する（または amend するが、tag は元の commit を指したまま）：

```text
注意: revision 更新の commit を amend すると tag が古い commit を指したまま残り、
verify_release_tag.sh が pass しなくなる。

代わりに：
  - commit を新規追加する → tag を新しい commit に移動 (`git tag -f vX.Y.Z`)
  - または、tag を作成する前に revision を仮置きで commit しておき、
    tag 作成後に revision を再 commit & tag 移動する
```

実用的な手順は次のいずれか：

**手順 A: tag 後に revision commit を追加して tag 移動**

```bash
git tag vX.Y.Z                            # 仮置きで tag
tag_commit=$(git rev-parse "vX.Y.Z^{commit}")
# Formula revision を更新
git add Formula/<name>.rb
git commit --amend --no-edit              # 同じ commit に追加
git tag -f vX.Y.Z                         # tag を amend 後の commit に移動
```

**手順 B: ローカルで複数回 commit → push 直前に整える**

リリースの最後に `git rebase -i` で整理する。

### Step 6: tag 整合性検証

```bash
make verify-tag
# または
bash scripts/verify_release_tag.sh
```

`Formula/<name>.rb` の `revision:` が `git rev-parse vX.Y.Z^{commit}` と一致することを確認。

### Step 7: push

```bash
git push origin main --tags
```

### Step 8: ローカル brew install 検証

push が完了したら、リモートから brew install を試す：

```bash
brew tap <owner>/<short-name> https://github.com/<owner>/<repo>
brew install <owner>/<short-name>/<short-name>

# または、ローカルクローンから直接：
brew install --build-from-source ./Formula/<short-name>.rb
```

`brew test <short-name>` で Formula の `test` ブロック（`--version` スモークテスト）も実行できる：

```bash
brew test <short-name>
```

### Step 9: 利用者向けアナウンス

README の Homebrew インストール手順が正しく書かれていることを確認：

```markdown
## インストール

```bash
brew tap <owner>/<short-name> https://github.com/<owner>/<repo>
brew install <short-name>
```
```

## エラー時の対処

| 症状 | 原因 | 対処 |
|------|------|------|
| `brew install` が「`vX.Y.Z` tag should be `<A>` but is actually `<B>`」 | Formula revision と tag commit のズレ | Step 5 〜 6 をやり直す。tag を `git tag -f vX.Y.Z` で再配置 |
| `verify_version_consistency.sh` の Formula tag 項目が fail | Formula の `tag:` が canonical と不一致 | Step 2 をやり直す |
| `brew test` がハング | バイナリの `--version` が即時終了しない | `language_traps.md`「--version 早期終了原則」を参照して main エントリを修正 |
| `brew audit --strict` が警告 | Formula の Ruby スタイル / metadata 不備 | 警告内容に従って修正。`homebrew-core` 提出予定でなければ警告のみで継続可 |

## 通常リリース（2 回目以降）のチェックリスト

- [ ] VERSION 更新
- [ ] CHANGELOG `[Unreleased]` → `[X.Y.Z] - YYYY-MM-DD` 昇格
- [ ] Formula tag 更新
- [ ] `make verify-version` pass
- [ ] commit
- [ ] git tag 作成
- [ ] Formula revision 更新
- [ ] tag 移動（手順 A の場合）
- [ ] `make verify-tag` pass
- [ ] `git push origin main --tags`
- [ ] `brew install --build-from-source ./Formula/<name>.rb` 検証
- [ ] `brew test <name>` 検証
