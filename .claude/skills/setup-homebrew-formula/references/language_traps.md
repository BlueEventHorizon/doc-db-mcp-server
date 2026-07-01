# 言語別トラップ集

doc-db-mcp-server / Swift-Selena の実装で実際に踏んだトラップを再発防止用に整理したもの。Formula 作成時に必ず確認する。

## 共通トラップ

### T-1: `--version` 早期終了原則 [致命的]

**症状**: `brew test <name>` がタイムアウトまでハング、または fail。

**原因**: `--version` フラグが設定ファイル読み込み・API キー検証・サーバー起動 **より後** に処理されている。`brew test` の隔離環境では設定ファイル・API キーが存在しないため、main がそこで詰まる。

**対処**: main エントリポイントの **冒頭** で `os.Args`（または相当）を直接チェックし、`--version` / `-v` を検知したら **`fmt.Println(version); return`** で即終了する。

```go
// ❌ 悪い例: 設定読み込みの後で --version を処理
func main() {
    cfg := loadConfig()        // 設定ファイル不在で fail-fast
    apiKey := requireAPIKey()  // API キー不在で fail-fast
    if hasFlag("--version") {
        fmt.Println(version)
        return
    }
}

// ✅ 良い例: 何よりも前で処理
func main() {
    if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
        fmt.Println(version)
        return
    }
    cfg := loadConfig()
    // ...
}
```

`brew test` 環境を想定して書く：「設定ファイル不在・API キー不在で動くか」をチェックする。

### T-2: tag 形式の不統一 [中程度]

**症状**: `brew install` が `<version> tag should be <A> but is actually <B>` で fail。または、`go install` で取得できない。

**原因**: CHANGELOG / .version-config.yaml / Formula tag / 実 git tag のいずれかで `v` prefix の有無が混在している。

**対処**: 全箇所で `v{version}` 形式に統一する（推奨）。`verify_version_consistency.sh` が静的に検証する。

### T-3: Formula コメント言語の規約 [低]

**症状**: 規約違反として指摘される（ローカルルール）。

**対処**: Formula 内コメントは **英語** にする。プロジェクトの日本語コメント規約に対する明示的な例外として、Formula 冒頭にその旨をコメントで記載する（Swift-Selena Formula 参照）。

## Go 固有のトラップ

### G-1: `//go:embed` パッケージ外参照不可 [致命的]

**症状**: `pattern VERSION: no matching files found` または同等のビルドエラー。

**原因**: Go の `//go:embed` ディレクティブはパッケージサブツリー外のファイル（`../VERSION` など `..` で抜けるパス）を参照できない。リポジトリ root の `VERSION` ファイルを `cmd/<binary>/main.go` や `internal/version/version.go` から embed しようとすると必ず失敗する。

**対処**: 以下のいずれか。

- **ldflags 方式（推奨）**: `go build -ldflags "-X main.version=$(cat VERSION)" ...`
- **root に embed パッケージを新設**: `version/version.go` を root に作り同パッケージ内で `//go:embed VERSION` する
- VERSION ファイルをやめて `internal/version/version.go` 内の `const Version = "..."` を canonical にする

doc-db では ldflags 方式を採用。

### G-2: `go.mod` module path とリポジトリ URL の不整合 [中程度]

**症状**: `go install github.com/<owner>/<repo>/cmd/<binary>@vX.Y.Z` が fail（`module declares its path as <other-path>`）。

**原因**: `go.mod` の `module github.com/k2moons/...` のように、実リポジトリ owner と一致しない module path で書かれている。

**対処**: `go.mod` の module path を実リポジトリの canonical URL に合わせる：

```text
module github.com/<actual-owner>/<repo>
```

`go install` をサポートしない（Homebrew のみ）プロジェクトなら必須ではないが、将来のため整合させる方が良い。

### G-3: `.gitignore` のディレクトリマッチ罠 [低]

**症状**: `git add cmd/docdb/main.go` が「ignored by .gitignore」で fail。

**原因**: `.gitignore` に `docdb` と書くと「どこにある docdb という名前のファイル/ディレクトリにもマッチ」する。`cmd/docdb/` ディレクトリ全体が ignore される。

**対処**: ビルドバイナリを ignore するときは `/docdb`（root のみ）と書く：

```gitignore
# ❌ 悪い: cmd/docdb/ も ignored される
docdb

# ✅ 良い: root の docdb バイナリのみ ignored
/docdb
/doc-db
```

### G-4: バイナリ出力名とパッケージ名の不整合 [低]

**症状**: 開発者が `go build ./cmd/docdb` を実行すると `docdb` バイナリができるが、Formula は `doc-db` を期待している。

**対処**: 出力名は常に `-o` で明示する。Makefile build target で `go build -o <binary-name>` を強制する。

## Swift 固有のトラップ

### S-1: `--disable-sandbox` が必要 [中程度]

**症状**: `brew install` の `swift build` フェーズで `error: failed to install ...: sandbox`.

**原因**: Homebrew sandbox 内で SwiftPM が外部依存を fetch しようとして拒否される。

**対処**: Formula の `install` で `--disable-sandbox` を付ける：

```ruby
system "swift", "build",
       "--disable-sandbox",      # SwiftPM の依存 fetch を許可
       "-c", "release",
       "-Xswiftc", "-Osize",
       "--product", "<ProductName>"
```

### S-2: Command Line Tools のみのビルド可否 [低]

**症状**: フル Xcode 不在の環境で `brew install` が fail。

**原因**: SwiftPM CLI ビルドは Command Line Tools の Swift 5.9+ で十分なはずだが、未検証の場合がある。

**対処**: Swift-Selena Formula は `depends_on xcode: ["15.0", :build]` を **意図的に省略** している（CLI ビルドは CLT で足りる前提）。CLT のみ環境で fail することが確認されたら追加する。

### S-3: SwiftPM の product 名と Formula install 配置 [低]

**症状**: `bin.install ".build/release/<wrong-name>"` で no such file。

**対処**: `Package.swift` の `.executableTarget` または `.executable` プロダクト名を確認し、`.build/release/<exact-name>` で参照する。配置先のバイナリ名は `=>` で別名にできる：

```ruby
bin.install ".build/release/Swift-Selena" => "swift-selena"
```

## MCP サーバー固有のトラップ

### M-1: transport と登録形式の不整合 [中程度]

**症状**: caveats の登録例どおりにユーザーが登録しても接続できない。

**原因**: サーバーが Streamable HTTP transport で動作するのに、caveats が stdio（`-- <binary>` 形式）の登録例を出している。

**対処**: transport ごとに正しい登録形式を案内：

| transport       | Claude Code                                                          | Claude Desktop              |
| --------------- | -------------------------------------------------------------------- | --------------------------- |
| Streamable HTTP | `claude mcp add --transport http <name> http://localhost:<port>/mcp` | `{"url": "http://..."}`     |
| stdio           | `claude mcp add <name> -- <binary>`                                  | `{"command": "<abs-path>"}` |

### M-2: HTTP transport は事前起動が必要 [低]

**症状**: ユーザーがインストール直後に接続を試して失敗する。

**対処**: caveats と README で「HTTP transport なのでサーバーを別途起動する必要がある」ことを明記する。launchd / systemd 設定例は README に置く（caveats は短く保つため）。

### M-3: `command` 形式と HTTP サーバーの矛盾 [中程度]

**症状**: Claude Desktop の `mcpServers` に `"command": ".../doc-db"` と書いたら subprocess 起動するが、サーバーは port をひらいてしまい二重起動になる。

**対処**: HTTP transport のサーバーには `"url"` 形式しか使わない。caveats のサンプルも `"url"` 形式に統一。

## 整合性検証スクリプト固有のトラップ

### V-1: tag_format の v prefix を verify 側で処理する [低]

**症状**: `tag_format: "v{version}"` を採用しているのに、`verify_version_consistency.sh` の Formula tag 比較が常に fail。

**対処**: Formula `tag:` から値を抽出するときに先頭 `v` を sed で剥がしてから canonical と比較する：

```bash
formula_tag=$(grep -E '^\s*tag:' Formula/<name>.rb \
  | sed -E 's/.*"v?([^"]+)".*/\1/' | head -1)
# ↑ "v?" で v の有無両対応
```

### V-2: `verify_release_tag.sh` は tag 作成後にしか pass しない [仕様]

**症状**: 初回 Formula 作成時に `make verify-tag` を実行すると fail する。

**対処**: これは仕様。tag 作成前の検証は `verify_version_consistency.sh` のみ。`verify_release_tag.sh` はリリースフローの後半（git tag 作成後）にのみ実行する。Makefile help 等で明示しておく。
