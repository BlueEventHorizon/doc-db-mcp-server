# canonical version 戦略

プロジェクト内で **バージョンの唯一の正本（canonical）** をどこに置き、どう各場所（Formula tag・CHANGELOG・バイナリの `--version` 出力）に伝播させるかの設計指針。

## 原則

1. **canonical は 1 か所だけ**。複数あると同期が壊れる。
2. **canonical から派生する場所** は機械検証する（`verify_version_consistency.sh`）。
3. **手動で書く場所を増やさない**。CHANGELOG 等は人間が書くが、機械チェックで canonical との一致を担保する。

## 戦略パターン

### A. プレーンテキストファイル（推奨：Go / Rust / Node.js）

- canonical: `VERSION` ファイル（root 直下）
- 内容: `0.1.0\n` のような plain text
- バイナリへの埋め込み: ビルドフラグ（Go なら `-ldflags "-X main.version=$(cat VERSION)"`）
- 利点: 言語非依存・読み取り簡単・他ツール（CI スクリプト等）からも参照しやすい
- 欠点: ビルド時に必ず埋め込み処理を挟む必要がある

### B. ソースコード内の定数（Swift / iOS / 言語が許す場合）

- canonical: `Sources/Constants.swift` の `static let version = "0.1.0"` 等
- バイナリへの埋め込み: そのまま参照（追加の埋め込み処理不要）
- 利点: ビルド時の追加ステップ不要・コードから直接読める
- 欠点: 機械検証スクリプトが対象言語の syntax を理解する必要がある（grep + sed で抽出）
- 例: Swift-Selena は `Sources/Constants.swift` を canonical にしている

### C. パッケージマニフェスト内（言語ネイティブ）

- canonical: `Cargo.toml` の `version = "0.1.0"` / `package.json` の `"version"` 等
- バイナリへの埋め込み: 言語の標準機能（`env!("CARGO_PKG_VERSION")` / `process.env.npm_package_version`）
- 利点: 言語慣習に沿う
- 欠点: 機械検証で TOML/JSON パースが必要

## 戦略選定基準

| 条件                      | 推奨戦略                                                                                                                         |
| ------------------------- | -------------------------------------------------------------------------------------------------------------------------------- |
| Go プロジェクト           | **A**（VERSION + ldflags）。`//go:embed` は **パッケージ外参照不可** のため root の VERSION を internal/version から取り込めない |
| Swift プロジェクト        | **B**（Sources/Constants.swift）。Swift-Selena 実績あり                                                                          |
| Rust プロジェクト         | **C**（Cargo.toml）。`env!("CARGO_PKG_VERSION")` で取得                                                                          |
| Node.js プロジェクト      | **C**（package.json）                                                                                                            |
| マルチ言語 / 言語制約なし | **A** がもっとも汎用・移植性が高い                                                                                               |

## バイナリへの埋め込み実装例

### Go（A: VERSION + ldflags）

```go
// cmd/docdb/main.go
package main

import (
    "fmt"
    "os"
)

// version はビルド時に -ldflags "-X main.version=..." で上書きされる。
// VERSION ファイルが canonical。
var version = "dev"

func main() {
    // 必ず設定読み込み・API キー検証・サーバー起動 *より前* に処理する
    if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
        fmt.Println(version)
        return
    }
    // ... 通常起動 ...
}
```

ビルド時：

```bash
go build -ldflags "-X main.version=$(cat VERSION)" -o <binary> ./cmd/<binary>
```

`//go:embed VERSION` は採用できない。Go の `embed` ディレクティブはパッケージサブツリー外（`..` で抜けるパス）を禁止するため、リポジトリ root の `VERSION` を `cmd/<binary>` や `internal/version` から参照できない。`version/` パッケージを root に新設して同パッケージ内で embed する案もあるが、ldflags のほうがシンプル。

### Swift（B: Sources/Constants.swift）

```swift
// Sources/<name>/Constants.swift
import Foundation

enum Constants {
    static let version = "0.1.0"
}
```

main エントリポイントで：

```swift
if CommandLine.arguments.contains("--version") || CommandLine.arguments.contains("-v") {
    print(Constants.version)
    exit(0)
}
// ... 通常起動 ...
```

ビルド時の追加ステップ不要。`swift build -c release` だけで埋め込まれる。

### Rust（C: Cargo.toml）

```rust
// src/main.rs
fn main() {
    let args: Vec<String> = std::env::args().collect();
    if args.len() > 1 && (args[1] == "--version" || args[1] == "-v") {
        println!("{}", env!("CARGO_PKG_VERSION"));
        return;
    }
    // ... 通常起動 ...
}
```

## tag 形式

| 形式         | 例       | 推奨度                                                                                                          |
| ------------ | -------- | --------------------------------------------------------------------------------------------------------------- |
| `v{version}` | `v0.1.0` | **推奨**。Go の `go install <pkg>@v0.1.0` で必須・SemVer 慣習・Homebrew でも `version.to_s` が自動で v を剥がす |
| `{version}`  | `0.1.0`  | Swift-Selena 採用例。Go install 非対応プロジェクトでは選択肢になる                                              |

`.version-config.yaml` の `tag_format` でこの選択を表現する：

```yaml
git:
  tag_format: "v{version}" # 推奨
```

整合性検証スクリプト（`verify_version_consistency.sh`）は Formula `tag:` の値の先頭 `v` を剥がしてから canonical と照合する設計にすると、両方の形式に対応できる。

## CHANGELOG との整合

[Keep a Changelog](https://keepachangelog.com/) 形式を推奨：

```markdown
## [Unreleased]

### Added

- ...

## [0.1.0] - 2026-06-24

### Added

- ...
```

リンク定義は v prefix 付きで git tag URL を指す：

```markdown
[0.1.0]: https://github.com/<owner>/<repo>/releases/tag/v0.1.0
```

`verify_version_consistency.sh` は `## [X.Y.Z]` 行から最新リリースを抽出して canonical と比較する（先頭の `v` は含まれない）。
