# APP-002 doc-db MCP Server インストール要件定義書

## 概要

doc-db MCP Server を macOS ユーザーが Homebrew で簡単にインストールできるようにするための要件を定める。配布方法・整合性検証・登録ガイダンスを含む。

本要件は base 仕様（APP-001）の補完として独立した feature「install」を構成する。base の機能要件（FNC-001〜005）には影響しない。

## 前提条件

| ID     | 要件 |
|--------|------|
| PRE-01 | base 仕様 APP-001 の前提条件（OPENAI_API_*_KEY 環境変数）が満たされていること |
| PRE-02 | インストール先 macOS が Homebrew 公式インストール済みであること（Apple Silicon = `/opt/homebrew`、Intel = `/usr/local`） |
| PRE-03 | macOS 13 (Ventura) 以上であること |
| PRE-04 | Go toolchain（Go 1.22 以上）が Homebrew 経由で導入可能であること（Formula の `depends_on` で確保） |

## 要件一覧

### FNC-101 自家 tap 形式での配布

GitHub リポジトリそのものを Homebrew tap として使用できる形態で配布する（専用 `homebrew-*` リポジトリを別途用意しない）。

| ID      | 要件 |
|---------|------|
| TAP-01  | リポジトリ直下の `Formula/` ディレクトリに Homebrew Formula ファイルを配置する |
| TAP-02  | ユーザーは `brew tap <owner>/<name> <https-url>` の明示 URL 形式でリポジトリを tap できる |
| TAP-03  | tap 後は `brew install <owner>/<name>/doc-db` または短縮形 `brew install doc-db` でインストールできる |
| TAP-04  | アンインストールは `brew uninstall doc-db` および `brew untap <owner>/<name>` で完了する |

### FNC-102 Formula の動作要件

| ID      | 要件 |
|---------|------|
| FML-01  | Formula はソースビルド方式で動作する（`go build` を `install` メソッドで実行） |
| FML-02  | Formula は対象 git tag（例: `v0.1.0`、形式は VER-06 で確定）の commit を `url` + `tag` + `revision` で固定参照する。version bump 時は tag・revision を Formula 側でも更新する |
| FML-03  | Formula は Go toolchain への依存（`depends_on "go" => :build`）と macOS バージョン依存（`depends_on macos: :ventura`）を宣言する |
| FML-04  | ビルド成果物は `doc-db` という名前のシングルバイナリとして `bin/` 配下にインストールされる |
| FML-05  | Formula の `caveats` セクションで、設定ファイル（`~/.doc-db/doc-db.yaml`）配置手順・OpenAI API キー環境変数設定手順・Claude Code / Claude Desktop への MCP サーバー登録手順を明示する |
| FML-06  | Formula の `test` セクションで、インストールされた `doc-db --version` がバージョン文字列を返すことを検証する |

### FNC-103 バージョン整合性

リリース時に Formula と他のバージョン参照箇所が乖離していないことを機械的に検証できる必要がある。

| ID      | 要件 |
|---------|------|
| VER-01  | プロジェクト内のバージョン正本（canonical）は `VERSION` ファイル（plain text）とする |
| VER-02  | バイナリは `doc-db --version` でバージョン文字列を出力する。バイナリに埋め込むバージョン値はビルド時に VERSION ファイルから取得する（実装方式は設計書 §4.2 で確定。Go の `//go:embed` はパッケージ外を参照できないため、`go build -ldflags "-X ..."` 方式を採用する） |
| VER-03  | `--version` は OPENAI API キー検証・設定ファイル読み込み・サーバー起動より前に処理し、即時終了すること（Homebrew test の `brew test doc-db` が長時間化しないようにするため） |
| VER-04  | CHANGELOG.md の最新リリースエントリのバージョンは canonical と一致しなければならない |
| VER-05  | `.version-config.yaml` の version_file は VERSION ファイルを指していなければならない |
| VER-06  | リリースタグおよび Formula の `tag:` は `v{canonical}`（例: `v0.1.0`）形式で統一する。`.version-config.yaml` の `tag_format` は `v{version}` を維持する |
| VER-07  | Formula の `revision:` フィールドの値は、`git tag v{version}` が指す commit の SHA と一致していなければならない |
| VER-08  | VER-04 / VER-05 / VER-06（Formula `tag:`）は version bump 直後に検証可能であること（`scripts/verify_version_consistency.sh` 相当） |
| VER-09  | VER-07 は git tag 確定後に検証可能であること（`scripts/verify_release_tag.sh` 相当） |

### FNC-104 インストール後ガイダンス

| ID      | 要件 |
|---------|------|
| GUI-01  | インストール完了後、ユーザーが追加で行うべき作業（設定ファイル配置・環境変数設定・サーバー起動・MCP クライアント登録）が `brew install` 直後の caveats で表示されること |
| GUI-02  | base 仕様（DES-001）が Streamable HTTP transport を採用しているため、caveats では「サーバーを別途起動した上で MCP クライアントに HTTP URL（`http://localhost:<port>/mcp`）として登録する」手順を示すこと。Claude Code 用には `claude mcp add --transport http doc-db <url>` を例示する |
| GUI-03  | Claude Desktop 用には HTTP transport の `url` フィールド形式の JSON 例を示すこと（subprocess の `command` 起動形式は使用しない） |
| GUI-04  | 設定ファイル `~/.doc-db/doc-db.yaml` のサンプルは Formula が `$(brew --prefix)/share/doc-db/doc-db.yaml.example` に配置し、caveats でホームへのコピー手順を示すこと |
| GUI-05  | サーバーを長時間起動するための方法（手動起動 / launchd 等）は本要件のスコープ外とし、README で補足する |

## エラーケース

| 条件 | 動作 |
|------|------|
| `brew install` 時に macOS バージョンが PRE-03 未満 | Formula の `depends_on macos: :ventura` により Homebrew が install を拒否する |
| Formula の `revision` と git tag の commit SHA が不一致 | `brew install` がエラーメッセージ「`<version>` tag should be `<X>` but is actually `<Y>`」で失敗する。リリース前に VER-06 検証で防ぐ |
| Go toolchain が未導入 | Formula の `depends_on "go" => :build` により Homebrew が自動的に Go を導入する |
| `~/.doc-db/doc-db.yaml` 未配置の状態で `doc-db` を起動 | base 仕様（DES-001 §9 CFG-01）に従い fail-fast でサーバーを終了する。caveats で配置を案内済み |

## 外部依存

| ID      | 依存先 |
|---------|--------|
| INST-EXT-01 | Homebrew（macOS パッケージマネージャー） |
| INST-EXT-02 | Go toolchain 1.22+（Formula ビルド時に Homebrew が導入） |
| INST-EXT-03 | git（Formula の `url` 参照に必要。Homebrew 同梱） |

## 非機能要件

| ID         | 要件 |
|------------|------|
| INST-NFR-01 | 初回 `brew install` 完了までの所要時間は通常のネットワーク環境で 3 分以内を目標とする（Go toolchain 取得済みの場合、`go build` 本体は数十秒で完了する見込み） |
| INST-NFR-02 | アップデートは `brew upgrade doc-db` で完結すること（追加手順を必要としない） |
| INST-NFR-03 | リポジトリの Formula は `brew audit --strict --new Formula/doc-db.rb` がパスする標準的な形式に従うこと |

## スコープ外（後回し）

- 公式 homebrew-core への submission
- ボトル（事前ビルド済みバイナリ）配布
- Linuxbrew 対応
- 公式の専用 tap リポジトリ（`homebrew-doc-db`）の分離
- Docker イメージ配布
- systemd / launchd 自動起動ユニットの同梱

## 未確定事項

| ID       | 内容 | 期限 |
|----------|------|------|
| INST-TBD-01 | tap オーナー名（`<owner>/<name>` の `<owner>`）。GitHub アカウント `k2moons` 直下とするか組織アカウントを用意するか | リリース前 |
| INST-TBD-02 | `--version` 以外の test 項目を追加するか（例: `doc-db --check-config` のような設定ファイル検証サブコマンド） | 設計書で検討 |

## 変更履歴

| 日付 | 変更者 | 内容 |
| ---- | ------ | ---- |
| 2026-06-25 | k2moons | 初版作成（Swift-Selena プロジェクトの Homebrew インストール仕組みを参考） |
| 2026-06-25 | k2moons | レビュー対応: VER-02 を ldflags 方式に確定（Go embed 制約）・VER-03 で `--version` 早期終了を要件化・VER-06 で tag 形式を `v{version}` に統一・GUI-02/03 を HTTP transport 登録形式に修正・GUI-04 で yaml.example の Formula 同梱を明文化 |
| 2026-06-26 | k2moons | レビュー対応: VER-03 `--version` 早期終了を `cmd/docdb/main.go` で実装（`make build` 経由で ldflags 値が注入されることを検証済み）。FNC-101〜104 のうち Formula・yaml.example・verify スクリプト本体は未実装のため、READMEを「現状/設計済み・未実装」表記に変更し設計と実装の乖離を明示（DES-002 §9 実装状況参照） |
