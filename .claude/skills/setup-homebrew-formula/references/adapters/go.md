# Go 言語アダプタ

Go プロジェクトの Homebrew Formula 化に必要な決定値とテンプレート断片。doc-db-mcp-server 実装ベース。

## 検出条件

- `go.mod` が repo root に存在する

## 決定値

| 項目 | 値 |
|------|-----|
| canonical 戦略 | A: `VERSION` plain text ファイル（`version_canonical_strategies.md` 参照） |
| バージョン埋め込み | ldflags `-X main.version=<value>` |
| ビルドコマンド | `go build -trimpath -ldflags "-s -w -X main.version=#{version}" -o bin/"<binary>" ./cmd/<binary>` |
| Formula `depends_on` | `depends_on "go" => :build`（`:build` でランタイム依存から外す） |
| tag 形式 | `v{version}`（`go install` 整合のため） |

## Formula install メソッド例

```ruby
def install
  system "go", "build",
         "-trimpath",
         "-ldflags", "-s -w -X main.version=#{version}",
         "-o", bin/"<binary>",
         "./cmd/<binary>"

  # （任意）設定ファイルサンプルを同梱する場合
  (share/"<name>").install "<name>.yaml.example"
end
```

オプション説明：

- `-trimpath`: ビルド時のローカルパスをバイナリから除去（再現可能ビルド）
- `-s -w`: シンボル情報・DWARF を除去してサイズ削減
- `-X main.version=#{version}`: Formula の `version` 属性を main パッケージの `version` 変数に注入

## main エントリポイントのテンプレート

```go
// cmd/<binary>/main.go
package main

import (
    "fmt"
    "os"
)

// version はビルド時に -ldflags "-X main.version=..." で上書きされる。
// VERSION ファイルが canonical。
//
// 手元 `go build` でこの値を埋めるには Makefile target を使うか、以下のワンライナーを実行する:
//   go build -ldflags "-X main.version=$(cat VERSION)" -o <binary> ./cmd/<binary>
var version = "dev"

func main() {
    // --version は何よりも前に処理する（設定読み込み・API キー検証より前）。
    // brew test (`brew test <name>`) はこの分岐を踏んで即時終了するため、
    // 設定ファイルや API キーがなくてもパスする。
    if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
        fmt.Println(version)
        return
    }

    // ... 通常起動 ...
}
```

## Makefile snippet

```makefile
SHELL := /bin/bash

VERSION := $(shell tr -d '\n' < VERSION)
BIN     := <binary-name>
PKG     := ./cmd/<binary>

LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build version test clean verify verify-version verify-tag help

help:
	@echo "build targets:"
	@echo "  make build           - $(BIN) を $(VERSION) でビルド"
	@echo "  make version         - VERSION ファイルの値を表示"
	@echo "  make test            - go test ./..."
	@echo "  make verify          - verify-version + verify-tag"
	@echo "  make verify-version  - 静的整合性検証"
	@echo "  make verify-tag      - tag 整合性検証"
	@echo "  make clean           - ビルド成果物を削除"

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) $(PKG)
	@echo "built: ./$(BIN) ($(VERSION))"

version:
	@echo "$(VERSION)"

test:
	go test ./...

verify: verify-version verify-tag

verify-version:
	@bash scripts/verify_version_consistency.sh

verify-tag:
	@bash scripts/verify_release_tag.sh

clean:
	rm -f $(BIN)
```

## .gitignore で注意すべき点

ビルドバイナリ名と同名のディレクトリ（例: `cmd/<binary>/`）がある場合、`.gitignore` でルートのみマッチさせる：

```gitignore
# ✅ root のビルドバイナリのみ ignore
/<binary>

# ❌ これは cmd/<binary>/ もマッチして package が ignore される
<binary>
```

詳細は `language_traps.md` G-3 参照。

## go.mod の module path

Go module path はリポジトリの実 canonical URL に合わせる：

```text
module github.com/<actual-owner>/<repo>
```

これが一致しないと将来 `go install github.com/<owner>/<repo>/cmd/<binary>@vX.Y.Z` で取得できない。Homebrew のみで配布する場合は厳密には必須ではないが、将来のために合わせるべき。

## `.version-config.yaml` 例

`/forge:update-version` 連携を想定した最小設定：

```yaml
# version_config_version: 1.0

targets:
  - name: <project-name>
    version_file: VERSION
    version_path: ""          # plain text
    sync_files:
      - path: Formula/<binary>.rb
        pattern: 'tag:      "v{version}"'
        filter: 'tag:'

changelog:
  file: CHANGELOG.md
  format: keep-a-changelog
  git_log_auto: true
  section_per_target: false

git:
  tag_format: "v{version}"
  commit_message: "chore: bump version to {version}"
  auto_tag: false
  auto_commit: false
```

## Formula test の最小形

```ruby
test do
  output = shell_output("#{bin}/<binary> --version")
  assert_match version.to_s, output
end
```

`version.to_s` は Formula の `version` 属性で、`tag: "v0.1.0"` のとき `"0.1.0"`（v 抜き）になる。ldflags で注入した値と一致する。
