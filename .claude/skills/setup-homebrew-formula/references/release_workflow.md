# リリースワークフロー

初回リリースおよび version bump 後のリリース手順。Formula の `revision:` は **git tag 確定後にしか正しい値にできない** ため、整合性検証は 2 段階に分かれる。

> **重要: この 2 段階運用は「初回作成時だけ」ではなく「毎バージョンリリースで繰り返す」**。
> v0.1.7 / v0.1.8 / v0.1.9 / v0.1.10 の全 bump で以下のパターンが実行されている:
>
> 1. **bump commit** (`chore: bump version to X.Y.Z`): `VERSION` / `CHANGELOG` / Formula `tag:` を更新し、`revision:` は **40 桁 0 の placeholder にリセット**する
> 2. **git tag vX.Y.Z** を打つ
> 3. **`chore(release): Formula revision を vX.Y.Z tag commit に確定`** commit で、`revision:` に tag commit の実 SHA を書き込む
>
> `.version-config.yaml` の `sync_files` は文字列単純置換のため `revision:` は対象外。bump 毎に手動リセットが必要。

## なぜ 2 段階なのか

| 検証                            | タイミング                      | 検証対象                                                                      |
| ------------------------------- | ------------------------------- | ----------------------------------------------------------------------------- |
| `verify_version_consistency.sh` | version bump 直後（tag 作成前） | canonical / CHANGELOG / .version-config.yaml / Formula `tag:` の **静的整合** |
| `verify_release_tag.sh`         | git tag 作成後（push 前）       | Formula `revision:` == git tag が指す commit SHA                              |

順序を間違えると、push 後に `brew install` が「`<version>` tag should be `<X>` but is actually `<Y>`」で失敗する。

## ステップ詳細

### Step 1: version bump

`/forge:update-version` または手動で：

- `VERSION` を新版に書き換え
- `CHANGELOG.md` の `[Unreleased]` を `[X.Y.Z] - YYYY-MM-DD` に昇格、新しい `[Unreleased]` セクションを追加
- 末尾のリンク定義 `[X.Y.Z]: .../releases/tag/v{X.Y.Z}` を追加

### Step 2: Formula tag 更新 + revision リセット

`Formula/<name>.rb` の `tag:` を新版に書き換え、`revision:` を **40 桁 0 の placeholder に戻す**（前バージョンの SHA が残っているとリリース後に古い commit を指し続けるため）：

```ruby
tag:      "v0.1.1",          # canonical と一致
revision: "0000000000000000000000000000000000000000"  # bump 毎にリセット必須
```

`.version-config.yaml` の `sync_files` で `tag:` は自動化されるが、`revision:` は
文字列単純置換の対象外 (SHA なので version 文字列ではない)。手動でリセットする:

```yaml
sync_files:
  - path: Formula/doc-db.rb
    pattern: 'tag:      "v{version}"'
    filter: "tag:"
  # revision: は sync_files 対象外。bump 毎に手動で 40 桁 0 に戻す。
  # tag 確定後、chore(release) commit で実 SHA に更新する (Step 5)。
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

### Step 5: Formula revision 更新 (`chore(release)` 別 commit)

**本プロジェクトの実運用**: tag は動かさず、tag 確定後に別 commit で `revision:` を
実 SHA に書き換える。tag そのものが指す commit ではなく、その **後続 commit** で
Formula ファイルが「tag が指している commit の SHA」を保持する形になる。

`brew install` は `url + tag + revision` で clone → checkout revision するので、
ビルドソースは tag commit のまま。Formula ファイル自体は main HEAD (`chore(release)`
commit) から読まれる。これで整合性が保たれる。

```bash
tag_commit=$(git rev-parse "vX.Y.Z^{commit}")
# Formula/<name>.rb の revision: を "$tag_commit" に書き換え
git add Formula/<name>.rb
git commit -m "chore(release): Formula revision を vX.Y.Z tag commit に確定"
# 例: git show fd739f4 で v0.1.10 の同 commit を確認できる
```

**この方式の利点**:

- tag を動かさない (`git tag -f` 不要 → force push リスクなし)
- リリース履歴に「revision 確定」の意図が commit として明示される
- `brew install` は url+tag+revision で pin されたビルドを取得できる

**避けるべき手順** (過去に検討したが不採用):

- `git commit --amend` + `git tag -f`: tag を強制移動する必要があり、force push が
  必要になる。push 済みなら整合が壊れる
- revision を先に仮置きしてから tag: 循環依存 (SHA を先に確定できない)

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

````markdown
## インストール

```bash
brew tap <owner>/<short-name> https://github.com/<owner>/<repo>
brew install <short-name>
```
````

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
- [ ] Formula `tag:` 更新
- [ ] **Formula `revision:` を 40 桁 0 の placeholder にリセット** ← 忘れやすい
- [ ] `make verify-version` pass
- [ ] **bump commit** (`chore: bump version to X.Y.Z`)
- [ ] main へ merge
- [ ] git tag `vX.Y.Z` 作成
- [ ] Formula `revision:` に tag commit SHA を書き込み
- [ ] **`chore(release)` commit** (`Formula revision を vX.Y.Z tag commit に確定`)
- [ ] `make verify-tag` pass
- [ ] `git push origin main --tags`
- [ ] `brew install --build-from-source ./Formula/<name>.rb` 検証
- [ ] `brew test <name>` 検証
```
