# Generic 言語アダプタ（その他の言語向け）

Go / Swift 以外のプロジェクト（Rust / Node.js / Python / シェルスクリプト等）で、手動入力により Formula 化するときの質問項目とテンプレート。

## 検出条件

- `go.mod` / `Package.swift` のいずれも存在しない

## 確認すべき項目

`AskUserQuestion` で順に確認：

| # | 項目 | 例 |
|---|------|-----|
| 1 | canonical version の置き場所 | `Cargo.toml` / `package.json` / `pyproject.toml` / `VERSION` |
| 2 | バージョン抽出コマンド | `grep '^version' Cargo.toml | sed ...` 等 |
| 3 | ビルドコマンド | `cargo build --release --target-dir bin` / `npm ci && npm run build` |
| 4 | ビルド成果物のパス | `target/release/<binary>` / `dist/cli.js` 等 |
| 5 | バイナリ配置先 | `bin/"<binary>"` |
| 6 | 必要な Homebrew 依存 | `depends_on "rust" => :build` / `depends_on "node"` 等 |
| 7 | `--version` 早期終了の実装状況 | 既存 OK / 要修正 |
| 8 | tag 形式 | `v{version}` 推奨 |

## 言語別の参考値

### Rust

```ruby
depends_on "rust" => :build

def install
  system "cargo", "install", *std_cargo_args
end
```

`std_cargo_args` は Homebrew が提供するヘルパで、`--locked --root #{prefix} --path .` を展開する。`Cargo.toml` の `[package] version` が canonical、`env!("CARGO_PKG_VERSION")` で `--version` 出力に使う。

### Node.js（純粋スクリプト）

```ruby
depends_on "node"

def install
  system "npm", "install", *Language::Node.std_npm_install_args(libexec)
  bin.install_symlink Dir["#{libexec}/bin/*"]
end
```

`package.json` の `"version"` が canonical。`process.env.npm_package_version` または `require('./package.json').version` で参照。

### Python

```ruby
depends_on "python@3.12"

def install
  virtualenv_install_with_resources
end
```

`pyproject.toml` の `[project] version` が canonical。Homebrew は `Language::Python::Virtualenv` を提供している。

### シェルスクリプトのみ

```ruby
def install
  bin.install "<script>.sh" => "<binary>"
end
```

依存 toolchain 不要。canonical は `<script>.sh` 内の `VERSION="X.Y.Z"` 等を grep する。

## 整合性検証スクリプトの canonical 抽出ロジック

`verify_version_consistency.sh.tmpl` の冒頭の VERSION 読み取り部分を、対象言語の canonical 抽出コマンドに差し替える：

```bash
# 例: Cargo.toml
canonical=$(grep -E '^version = ' Cargo.toml | sed -E 's/.*"([0-9]+\.[0-9]+\.[0-9]+)".*/\1/' | head -1)

# 例: package.json
canonical=$(grep -E '"version":' package.json | head -1 | sed -E 's/.*"([0-9]+\.[0-9]+\.[0-9]+)".*/\1/')

# 例: pyproject.toml
canonical=$(grep -E '^version = ' pyproject.toml | sed -E 's/.*"([0-9]+\.[0-9]+\.[0-9]+)".*/\1/' | head -1)
```

## カスタマイズ手順

1. SKILL.md Phase 6 でテンプレートを生成
2. 生成後、`scripts/verify_version_consistency.sh` の `canonical=` 行を上記から選んで差し替え
3. `Formula/<binary>.rb` の `install` メソッドを言語に合わせて編集
4. `Makefile` の `LDFLAGS` / `build` target を言語に合わせて差し替え（言語によっては Makefile 自体不要）
5. `references/release_workflow.md` の手順は言語非依存なのでそのまま使える
