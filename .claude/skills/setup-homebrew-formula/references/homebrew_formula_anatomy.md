# Homebrew Formula の解剖図

自家 tap で使う Formula の各セクションの意味と典型値。Swift-Selena / doc-db-mcp-server の実装から得た知見を反映している。

## 必須セクション

| セクション                 | 役割                                                      | 例                                                        |
| -------------------------- | --------------------------------------------------------- | --------------------------------------------------------- |
| `class <Name> < Formula`   | クラス定義。`Name` は PascalCase（`doc-db.rb` → `DocDb`） | `class DocDb < Formula`                                   |
| `desc`                     | 1 行説明（80 文字以内推奨）                               | `"Hybrid search MCP server for Markdown documents"`       |
| `homepage`                 | プロジェクト URL（**実リポジトリ URL を使う**）           | `"https://github.com/BlueEventHorizon/doc-db-mcp-server"` |
| `url` + `tag` + `revision` | ソース pin（次節で詳述）                                  | git URL + 値                                              |
| `license`                  | SPDX 識別子                                               | `"MIT"`、`"Apache-2.0"`                                   |

## url / tag / revision の三位一体

```ruby
url "https://github.com/<owner>/<repo>.git",
    tag:      "v0.1.0",
    revision: "<commit SHA of tag v0.1.0>"
```

- **`url`**: `.git` 拡張子付きの git URL を使う（tarball ではなく git clone される）
- **`tag`**: ユーザーに見えるバージョン識別子。Go プロジェクトなら `v{version}` 形式（`go install` 整合）
- **`revision`**: tag が指す commit の完全 40 桁 SHA。**ここが tag commit とズレると Homebrew が install を拒否する**

初回 Formula 作成時は `revision:` を `"0000...0000"` の placeholder にし、git tag 作成後に SHA で書き換える。`verify_release_tag.sh` がこの整合性を検証する。

## 依存宣言

```ruby
depends_on macos: :ventura       # macOS 13 以上（PRE-03 相当）
depends_on "go" => :build         # ビルド時のみ Go toolchain
depends_on "openssl@3"            # ランタイム依存
```

- `:build` をつけると Homebrew がビルド完了後に依存を unlink できる（ランタイムに不要な toolchain の指定に使う）
- macOS バージョン制限は `:big_sur`（11）/ `:monterey`（12）/ `:ventura`（13）/ `:sonoma`（14）/ `:sequoia`（15）

## install メソッド

```ruby
def install
  # ビルド: 言語別。下記は Go の典型例
  system "go", "build",
         "-trimpath",
         "-ldflags", "-s -w -X main.version=#{version}",
         "-o", bin/"doc-db",
         "./cmd/docdb"

  # サンプル設定ファイルを同梱する場合
  (share/"doc-db").install "doc-db.yaml.example"
end
```

- `bin/"<binary-name>"` で `$(brew --prefix)/bin/<binary-name>` に配置
- `(share/"<name>").install` で `$(brew --prefix)/share/<name>/<file>` に配置（設定サンプル等）
- `etc/`, `var/`, `man/`, `lib/` も使える（用途に応じて）

## caveats メソッド

`brew install` 完了直後に表示されるメッセージ。**インストール直後にユーザーが必ず行うべき作業のみ**を 5 項目程度に絞る。長すぎると読み飛ばされる。

```ruby
def caveats
  <<~EOS
    <name> is installed as `<binary>` and is on your PATH.

    1) Prepare config:
         mkdir -p ~/.<name>
         cp #{share}/<name>/<name>.yaml.example ~/.<name>/<name>.yaml

    2) Export API key:
         export <APP>_API_KEY=...

    3) Register with Claude Code (Streamable HTTP transport):
         claude mcp add --transport http -s user <name> http://localhost:<port>/mcp

    Full documentation: #{homepage}
  EOS
end
```

- Ruby 文字列補間で `#{HOMEBREW_PREFIX}` / `#{share}` / `#{prefix}` / `#{bin}` / `#{homepage}` が使える
- 詳細な使い方は README に誘導する（`Full documentation: #{homepage}`）

### transport 別の登録形式

| transport          | Claude Code 登録例                                                   | Claude Desktop                           |
| ------------------ | -------------------------------------------------------------------- | ---------------------------------------- |
| Streamable HTTP    | `claude mcp add --transport http <name> http://localhost:<port>/mcp` | `{"url": "http://localhost:<port>/mcp"}` |
| stdio (subprocess) | `claude mcp add <name> -- <binary>`                                  | `{"command": "<absolute-path>"}`         |

サーバー本体が HTTP transport なら `command` 形式は **使わない**（矛盾）。

## test メソッド

```ruby
test do
  output = shell_output("#{bin}/<binary> --version")
  assert_match version.to_s, output
end
```

- `version.to_s` は Formula の `version` 属性。`tag: "v0.1.0"` のとき `"0.1.0"`（v 抜き）になる
- `--version` は **設定読み込み・API キー検証より前** に即時終了する必要がある（重要トラップ。`language_traps.md` 参照）
- HTTP サーバー起動を伴うテストは隔離環境（設定ファイル不在・API キーなし）で失敗するので避ける

## コメント言語規約

**Formula 内のコメントは英語にする** ことを推奨。理由：

- Homebrew コミュニティのデファクト（公式 `homebrew-core`・個人 tap のほとんどが英語のみ）
- `brew edit <name>` で外部コントリビューターが読む
- 将来 `homebrew-core` 提出を視野に入れるなら最初から英語が無難

これは「ソースコードのコメントは日本語」というプロジェクト規約への **明示的な例外**。Formula 冒頭に理由をコメントで明記しておくと、規約違反と誤認されない（Swift-Selena Formula の冒頭参照）。

## 命名規則

| 対象               | 規則                                               | 例                             |
| ------------------ | -------------------------------------------------- | ------------------------------ |
| Formula ファイル名 | lowercase kebab-case                               | `doc-db.rb`, `swift-selena.rb` |
| Formula クラス名   | PascalCase（ハイフンは取り除く）                   | `DocDb`, `SwiftSelena`         |
| tap 名             | lowercase                                          | `blueeventhorizon/doc-db`      |
| バイナリ名         | lowercase kebab-case（プロジェクト慣習に合わせる） | `doc-db`, `swift-selena`       |

## 参考にすべき実装

- `Swift-Selena/Formula/swift-selena.rb` — Swift CLI + 設定ファイルなし + stdio MCP
- `doc-db-mcp-server/Formula/doc-db.rb` — Go CLI + YAML 設定ファイル + Streamable HTTP MCP
