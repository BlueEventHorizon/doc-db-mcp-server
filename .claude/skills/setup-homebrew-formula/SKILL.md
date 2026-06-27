---
name: setup-homebrew-formula
description: macOS で `brew tap <owner>/<name> <git-url>` + `brew install <name>` で導入可能な「自家 tap」型 Homebrew Formula と、リリース時の整合性検証スクリプト（VERSION / CHANGELOG / Formula tag / commit SHA の自動チェック）を生成し、初回リリース手順をガイドする。Go と Swift を実例として言語アダプタ構造で抽象化し、`--version` 早期終了・tag は `v{version}` 形式・Formula コメントは英語などの実装トラップを既知の経験から組み込む。トリガー例: "homebrew で配布したい", "brew install できるように", "Formula を作成", "self tap セットアップ", "brew でインストールしたい"
when_to_use: プロジェクトを macOS ユーザーに Homebrew 経由で配布できる状態にしたいとき。新規 Formula 作成・整合性検証スクリプト 2 本（version_consistency / release_tag）の導入・README/CHANGELOG の更新・リリース手順の案内まで一貫してカバーする。既存 Formula がある場合は update モードで差分のみを提案する。
disable-model-invocation: true
allowed-tools: Read, Write, Edit, Glob, Grep, Bash
---

# setup-homebrew-formula

macOS 向け Homebrew **自家 tap** 配布（`Formula/` ディレクトリ + `brew tap <owner>/<name> <git-url>` 形式）の初期セットアップと、初回リリースまでの手順を案内する。リポジトリ自体を tap として使うため、専用 `homebrew-*` リポジトリは不要。

## When To Use

- プロジェクトを macOS ユーザーに **`brew install`** で配布したい
- バージョン情報（VERSION / CHANGELOG / Formula tag / git tag commit SHA）の整合性を **機械的に検証** したい
- Go / Swift / その他言語の CLI またはデーモン型ツールが対象（言語別の特性は `references/adapters/` で吸収）

**使わない場面**

- Homebrew 公式 `homebrew-core` への submission（別フローが必要・スコープ外）
- Linuxbrew 専用配布
- ボトル（事前ビルド済みバイナリ）の cross-compile 配布

## Workflow

### Phase 1: 環境確認 [MANDATORY]

以下を Bash で確認：

```bash
brew --version || echo "WARN: Homebrew 未インストール（ローカル検証時のみ必要）"
git rev-parse --is-inside-work-tree
git remote get-url origin   # GitHub owner/repo を抽出する元
```

`git remote` から **canonical な GitHub owner/repo** を取得する（README/CHANGELOG/Formula で同一値を使う）。

### Phase 2: プロジェクトタイプ判別 [MANDATORY]

ファイル存在チェックで言語を判別する：

| ファイル | 言語 | アダプタ |
|---------|------|---------|
| `go.mod` | Go | `references/adapters/go.md` |
| `Package.swift` | Swift | `references/adapters/swift.md` |
| 上記いずれもなし | その他 | `references/adapters/generic.md`（手動入力） |

判別したらユーザーに確認（複数言語混在プロジェクトの場合は主言語を選択）。

### Phase 3: 設計の読み込み [MANDATORY]

判別した言語のアダプタを読み、以下を確定：

| 項目 | 内容 |
|------|------|
| canonical version 位置 | 例: `VERSION` plain text / `Sources/Constants.swift` の static let / `Cargo.toml` |
| バージョン埋め込み方式 | 例: `go build -ldflags "-X main.version=..."` / Swift の static let を直接読む |
| ビルドコマンド | Formula `install` メソッドに記述する内容 |
| 依存 toolchain | `depends_on "go" => :build` 等 |
| `--version` 実装パターン | 早期終了させるコード片（VER-03 相当） |

判明していなければ `references/version_canonical_strategies.md` を読んで方式を選ぶ。

### Phase 4: `--version` 早期終了の確認 [MANDATORY]

**最重要トラップ**: `brew test` は `--version` を呼ぶ。設定ファイル読み込み・API キー検証・サーバー起動が走るとハングする。

対象プロジェクトの main エントリポイントで、`--version` フラグが **何よりも前** に処理されているか確認する。不十分なら言語アダプタの実装パターンを案内して修正を促す（または自動修正）。

詳細は `references/language_traps.md` の「--version 早期終了原則」を参照。

### Phase 5: tap owner / tap 名 確定 [MANDATORY]

`AskUserQuestion` で確認：

- tap owner: 通常 GitHub owner と同じ。lowercase に変換（Homebrew 慣習）
- tap 名: `<owner>/<short-name>` の `<short-name>`（リポジトリ名と一致させるのが普通）
- Formula 内 class 名: PascalCase（例: `DocDb`）と Formula ファイル名: kebab-case（例: `doc-db.rb`）

### Phase 6: ファイル生成

以下を `assets/` のテンプレートから生成する。`{{...}}` プレースホルダを置換する：

| 生成先 | テンプレート | 内容 |
|--------|--------------|------|
| `Formula/<short-name>.rb` | `assets/formula.rb.tmpl` | Homebrew Formula 本体 |
| `scripts/verify_version_consistency.sh` | `assets/verify_version_consistency.sh.tmpl` | 静的検証 |
| `scripts/verify_release_tag.sh` | `assets/verify_release_tag.sh.tmpl` | tag commit 検証 |
| `Makefile` への追記 | `assets/makefile_snippet.tmpl` | `make build` / `make verify` |

Formula の `revision:` フィールドは初回は **40 桁 0 の placeholder** にする（git tag 作成後に SHA で更新する）。

ユーザーが希望する場合は、設定ファイルサンプル（例: `<name>.yaml.example`）も同梱して `(share/"<name>").install` で配置する設計を提案する。

### Phase 7: README / CHANGELOG の整備

- README に「Homebrew インストール」セクションを追加（`references/release_workflow.md` のテンプレート参照）
- CHANGELOG は keep-a-changelog 形式を推奨。`[Unreleased]` セクションに今回の追加を記録
- リポジトリ URL は **Phase 1 で取得した実リポジトリ URL** を使用（k2moons → BlueEventHorizon のような差し替えバグを防ぐ）

### Phase 8: 自己検証 [MANDATORY]

```bash
bash scripts/verify_version_consistency.sh
# 期待: 全項目 ok・exit 0

make build  # 言語アダプタの build target を実行
./<binary> --version  # canonical と一致することを確認

brew audit --strict Formula/<short-name>.rb || echo "WARN: brew audit が警告を出した（手動確認）"
```

`brew audit` は `brew --version` が動く環境でのみ実行可能。CI 環境では skip も可。

### Phase 9: リリース手順の案内

初回リリースは以下の順で行う必要がある（Formula `revision:` は tag 作成後にしか確定しない）。**ユーザーに `references/release_workflow.md` を案内** し、必要なら対話的に進める：

1. `make verify-version`（CHANGELOG/Formula tag/.version-config.yaml の静的整合）
2. `git tag v{version}` を作成
3. `Formula/<short-name>.rb` の `revision:` を tag の commit SHA に書き換え
4. `make verify-tag`（Formula revision == tag commit SHA）
5. `git push origin main --tags`
6. ローカルで `brew install --build-from-source ./Formula/<short-name>.rb` を実行して動作確認
7. tap 利用者向けに `brew tap <owner>/<short-name> <git-url>` の手順を README に明記

## Supporting Files

| パス | 読むタイミング |
|------|---------------|
| `references/homebrew_formula_anatomy.md` | Formula の各セクションの意味・コメント言語規約・命名規則を確認するとき |
| `references/version_canonical_strategies.md` | canonical version の置き場所と埋め込み方式の選定で迷ったとき |
| `references/release_workflow.md` | Phase 9 のリリース手順を案内するとき |
| `references/language_traps.md` | 言語別トラップ（Go embed パッケージ外不可・tag v prefix・module path 整合・MCP transport caveats など）を参照するとき |
| `references/adapters/go.md` | Go プロジェクトでビルド・version 埋め込みを設計するとき |
| `references/adapters/swift.md` | Swift プロジェクトで Package.swift ベースのビルドを設計するとき |
| `references/adapters/generic.md` | Go / Swift 以外のプロジェクトで手動入力するとき |
| `assets/formula.rb.tmpl` | Phase 6 で Formula を生成するとき |
| `assets/verify_version_consistency.sh.tmpl` | Phase 6 で静的検証スクリプトを生成するとき |
| `assets/verify_release_tag.sh.tmpl` | Phase 6 で tag 検証スクリプトを生成するとき |
| `assets/makefile_snippet.tmpl` | Phase 6 で Makefile を生成するとき |

## Validation

### サンプル自動トリガー（参考）

```
ユーザー: "このプロジェクトを brew install で配布できるようにしたい"
→ Skill が自動マッチして起動
```

### サンプル手動呼び出し

```
/setup-homebrew-formula
→ Phase 1〜8 を順に実行し、必要なファイルを生成
```

### 期待される最終状態

- `Formula/<short-name>.rb` が `brew audit --strict` をパス
- `bash scripts/verify_version_consistency.sh` が exit 0
- `./<binary> --version` が canonical version を即時出力（設定読み込み前）
- README に Homebrew インストール手順あり
- CHANGELOG `[Unreleased]` に追加項目あり

## 既知の制約

- Formula の `revision:` は git tag 確定後にしか正しい値にできないため、初回は placeholder（40桁0）で生成する
- `brew audit` は Homebrew 自体のインストールが前提（CI で skip 可）
- 本 Skill 自体は Homebrew 環境に依存しないが、生成された成果物の **実 install 検証** には Homebrew が必要
- 言語アダプタは現在 Go / Swift / generic の 3 系統。他言語は generic で手動入力を経由する
