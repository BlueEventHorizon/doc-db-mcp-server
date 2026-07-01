# Swift 言語アダプタ

Swift プロジェクト（SwiftPM ベース）の Homebrew Formula 化に必要な決定値とテンプレート断片。Swift-Selena 実装ベース。

## 検出条件

- `Package.swift` が repo root に存在する

## 決定値

| 項目                 | 値                                                                                      |
| -------------------- | --------------------------------------------------------------------------------------- |
| canonical 戦略       | B: ソースコード内の定数（例: `Sources/<name>/Constants.swift` の `static let version`） |
| バージョン埋め込み   | 不要（const をそのまま参照）                                                            |
| ビルドコマンド       | `swift build --disable-sandbox -c release -Xswiftc -Osize --product <ProductName>`      |
| Formula `depends_on` | `depends_on macos: :ventura` のみ。Swift toolchain は CLT で足りる前提                  |
| tag 形式             | プロジェクトの慣習に合わせる（Swift-Selena は `{version}` を採用）                      |

## Formula install メソッド例

```ruby
def install
  # --disable-sandbox: SwiftPM が外部依存を fetch するため必要（Homebrew sandbox 内）
  # -Xswiftc -Osize: バイナリサイズを優先する最適化
  system "swift", "build",
         "--disable-sandbox",
         "-c", "release",
         "-Xswiftc", "-Osize",
         "--product", "<ProductName>"

  # Package.swift の product 名が PascalCase で、配布時は lowercase に変えたい場合
  bin.install ".build/release/<ProductName>" => "<binary-name>"
end
```

## canonical version の置き場所

```swift
// Sources/<Name>/Constants.swift
import Foundation

enum Constants {
    static let version = "0.1.0"
}
```

## main エントリポイントの早期終了

```swift
// Sources/<Name>/main.swift など
import Foundation

let args = CommandLine.arguments
if args.count > 1, args[1] == "--version" || args[1] == "-v" {
    print(Constants.version)
    exit(0)
}

// ... 通常起動 ...
```

Swift-Selena は stdio MCP server なので `--version` フラグを別に持たないが、Homebrew test 用には必要。MCP `initialize` 経由のテストはコストが高いため、`--version` スモークテストに切り替える方が `brew test` の隔離環境では安定する。

## 整合性検証スクリプトでの canonical 抽出

VERSION ファイル方式と違い、Swift のソースから抽出する：

```bash
canonical=$(grep -E 'static let version = ' Sources/<Name>/Constants.swift \
  | sed -E 's/.*"([0-9]+\.[0-9]+\.[0-9]+)".*/\1/' \
  | head -1)
```

`assets/verify_version_consistency.sh.tmpl` の VERSION 読み取り行を上記の grep に差し替える。

## `.version-config.yaml` 例

```yaml
# version_config_version: 1.0

targets:
  - name: <project-name>
    version_file: Sources/<Name>/Constants.swift
    version_path: "" # plain text 扱い（後述）
    sync_files:
      - path: Formula/<binary>.rb
        pattern: 'tag:      "{version}"'
        filter: "tag:"

git:
  tag_format: "{version}" # Swift-Selena は v なし
  commit_message: "chore: bump to {version}"
  auto_tag: false
  auto_commit: false
```

`version_file` が Swift ソースの場合、`/forge:update-version` の挙動は `version_path` の解釈に依存する。Swift-Selena では `verify_version_consistency.sh` 側で grep + sed して同期チェックを行っている（plain text 扱い）。

## Formula `depends_on` の選択

```ruby
depends_on macos: :ventura
# depends_on xcode: ["15.0", :build]  # CLT のみ環境で fail することが確認された場合に追加
```

Swift-Selena では `depends_on xcode` を意図的に省略している。理由はコメントで明記されている：

> `depends_on xcode` is intentionally omitted: the build is a plain SwiftPM CLI build and Command Line Tools providing Swift 5.9+ should suffice.

CLT のみ環境でビルド失敗が報告されたら追加する方針。

## Formula test の例

stdio MCP server の場合、`--version` が実装されていれば Go と同じスモークテストで足りる：

```ruby
test do
  output = shell_output("#{bin}/<binary> --version")
  assert_match version.to_s, output
end
```

`--version` を実装しない場合は MCP `initialize` JSON-RPC を stdin に流す方式になるが、これは長く・タイムアウト管理が必要。Swift-Selena の test ブロックを参考に：

```ruby
test do
  require "open3"
  require "json"
  request = {
    jsonrpc: "2.0", id: 1, method: "initialize",
    params: { protocolVersion: "2024-11-05", capabilities: {}, clientInfo: { name: "brew-test", version: "0" } },
  }.to_json
  # ... open3.popen3 で stdin に書き込み、stdout から id==1 のレスポンス受信 ...
end
```

詳細は Swift-Selena Formula を参照。シンプルさを優先するなら `--version` を実装するのが推奨。
