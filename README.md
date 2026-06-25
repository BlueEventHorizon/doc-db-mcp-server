# doc-db MCP Server

Markdown ドキュメントをハイブリッド検索（ベクトル + BM25 + LLM Rerank）で横断検索できる汎用 MCP サーバー。

> **開発ステータス: 設計フェーズ完了・実装途中（v0.1.0）**
>
> 現状は基盤コンポーネント（store / chunker / embedder / fetcher / expiry）と CLI 骨格までが実装されています。MCP ハンドラ・検索パイプライン・Homebrew 配布・YAML 設定方式は **設計済みだが未実装** です。実装ロードマップは下記「実装状況」を参照してください。

## 何の問題を解決するのか

AI アシスタント（Claude Code 等）は、プロジェクトの仕様書・設計書・ルールドキュメントを「その都度読んで」作業する。ファイル数が増えると：

- どのドキュメントが関連するか判断できない
- すべて読み込むとコンテキスト上限を超える
- キーワードが一致しないと関連文書が見つからない

**doc-db** は MCP ツールとしてこれを解決する。ドキュメントを事前にインデックスしておき、自然言語クエリで関連チャンクだけを取り出せるようにする。

## 主な利点（設計目標）

| 特徴 | 説明 |
|------|------|
| **ハイブリッド検索** | ベクトル類似度 (Embedding) + BM25 語彙一致 + LLM Rerank の3段階で高精度な検索を実現 |
| **ID パターン対応** | `FNC-001` や `DES-028` のような規格 ID を語彙検索で正確にマッチ |
| **重複 Embedding 排除** | 同一内容のドキュメントは hash で検出し、Embedding を共有。複数 branch/series を低コストで管理 |
| **シングルバイナリ** | CGO 不要（pure-Go SQLite）。Homebrew で一発インストール（実装予定） |
| **TTL / LRU 自動廃棄** | 期限切れ・容量超過のインデックスを自動削除。メンテナンス不要 |
| **SSRF 防御済み** | URL 登録機能はプライベート IP への接続をデフォルトでブロック |

## アーキテクチャ

```
MCP クライアント (Claude Code / Desktop 等)
        │  Streamable HTTP (MCP 2025-03)
        │  http://localhost:8080/mcp
        ▼
┌─────────────────────────────────────────────────┐
│  MCP Server (go-sdk)                            │
│  ┌──────────┬──────────┬─────────┬───────────┐  │
│  │ upsert   │  delete  │  query  │  manage   │  │
│  └────┬─────┴────┬─────┴────┬────┴─────┬─────┘  │
│       │          │          │          │         │
│  ┌────▼───┐ ┌────▼────┐ ┌──▼───────┐  │         │
│  │Chunker │ │Embedder │ │ Search   │  │         │
│  │(見出し │ │(OpenAI) │ │ Pipeline │  │         │
│  │ 境界)  │ └────┬────┘ │vector+  │  │         │
│  └────┬───┘      │      │BM25+    │  │         │
│       │          │      │rerank   │  │         │
│       └──────────┴──────┴─────────┴──┘         │
│                         │                       │
│              ┌──────────▼──────────┐            │
│              │  Store (SQLite)     │ ◄── Expiry  │
│              │  records / chunks   │    Worker   │
│              │  embeddings / bm25  │  (TTL/LRU)  │
│              └─────────────────────┘            │
└─────────────────────────────────────────────────┘
```

### レイヤー構成

```
cmd/docdb          エントリポイント・設定読み込み
internal/mcp       MCP ツールハンドラ（4 ツール）           [未実装]
internal/search    検索パイプライン（emb/lex/hybrid/rerank） [未実装]
internal/chunker   Markdown → 見出し境界チャンク分割
internal/embedder  OpenAI Embedding API
internal/fetcher   URL → コンテンツ取得（SSRF 防御付き）
internal/expiry    TTL / LRU 自動廃棄ワーカー
internal/store     SQLite 読み書き・BM25 統計管理
```

上位レイヤーのみが下位を参照する。循環依存なし。

## 実装状況

| 領域 | 状態 | 仕様文書 |
|------|------|----------|
| 基盤コンポーネント（store/chunker/embedder/fetcher/expiry） | ✅ 実装済み | `docs/specs/base/design/DES-001` |
| `--version` フラグ | ✅ 実装済み | DES-002 §4.2.1 |
| MCP サーバー本体（Streamable HTTP）・ツールハンドラ 4 種 | 🚧 未実装（DES-001 §3.1 で設計済み） | DES-001 |
| 検索パイプライン（emb/lex/hybrid/rerank） | 🚧 未実装（DES-001 §6 で設計済み） | DES-001 |
| **YAML 設定ファイル方式**（`~/.doc-db/doc-db.yaml`） | 🚧 未実装（DES-001 §9 で設計済み） | DES-001 §9 |
| **Homebrew 配布**（`Formula/doc-db.rb` + tap 方式） | 🚧 未実装（設計のみ） | `docs/specs/install/design/DES-002` |
| **整合性検証スクリプト**（`scripts/verify_*.sh`） | 🚧 未実装（設計のみ） | DES-002 §4.3 |
| **`doc-db.yaml.example` 同梱** | 🚧 未実装 | DES-002 §5.2 |

---

## 現状の使い方（暫定）

> 以下は **現時点で動作する手順** です。MCP ハンドラ未実装のため、起動しても接続できる MCP クライアントはありません。基盤コンポーネントの動作確認用です。

### 前提条件

- Go 1.22 以上
- OpenAI API キー

### ビルド

```bash
make build
# または
go build -ldflags "-X main.version=$(cat VERSION)" -o doc-db ./cmd/docdb
```

### バージョン確認

```bash
./doc-db --version
# 0.1.0
```

`--version` / `-v` は設定読み込み・API キー検証・サーバー起動より前に即時終了します。

### 現状の起動（環境変数方式 — 暫定）

> ⚠️ **注意**: 現行 `cmd/docdb/main.go` は `DOCDB_*` 環境変数を読みます。DES-001 §9 で確定済みの **YAML 設定ファイル方式（`~/.doc-db/doc-db.yaml`）に置き換える予定**ですが、まだ実装されていません。下記は **置き換え前の暫定動作** です。

```bash
export OPENAI_API_DOCDB_KEY=sk-...
export DOCDB_DB_PATH=./docdb.sqlite       # 任意（既定値）
export DOCDB_TTL_DAYS=30                  # 任意（既定値）
export DOCDB_MAX_CHUNKS=10000             # 任意（既定値）
export DOCDB_EXPIRY_INTERVAL=3600         # 任意（既定値・秒）
./doc-db
# "doc-db MCP サーバー起動準備完了。MCP ハンドラは未実装のため待機します"
# Ctrl+C で終了
```

---

## 設計済み・未実装の使い方（将来）

### Homebrew インストール（実装予定）

設計書 `docs/specs/install/design/DES-002` で確定済み。実装後は以下で導入できる予定：

```bash
brew tap k2moons/doc-db https://github.com/k2moons/doc-db-mcp-server
brew install k2moons/doc-db/doc-db
```

実装に必要なファイル（いずれも未作成）：

- `Formula/doc-db.rb`（Homebrew Formula）
- `doc-db.yaml.example`（リポジトリ直下に設定ファイルサンプル）
- `scripts/verify_version_consistency.sh`
- `scripts/verify_release_tag.sh`

### YAML 設定方式（実装予定）

設計書 `docs/specs/base/design/DES-001` §9 で確定済み。実装後は `~/.doc-db/doc-db.yaml` を以下のように記述：

```yaml
server:
  port: 8080
  db_path: "./docdb.sqlite"
embedding:
  model: "text-embedding-3-small"
  dim: 1536
  timeout_seconds: 60
rerank:
  model: "gpt-4o-mini"
  factor: 3
  timeout_seconds: 30
chunker:
  max_chunk_size: 1500
bm25:
  k1: 1.5
  b: 0.75
fetcher:
  timeout_seconds: 30
  allow_private: false
expiry:
  ttl_days: 30
  max_chunks: 10000
  interval_seconds: 3600
```

すべての項目は設定ファイルが正本。**環境変数によるオーバーライドは行わない**（API キーを除く）。実装後、現在の `DOCDB_*` 環境変数読み取りは廃止される予定です。

### Claude Code / Desktop への登録（実装予定）

doc-db は Streamable HTTP transport で動作するため、URL 形式で登録します（subprocess 形式の `command` は使用しない）：

```bash
# Claude Code (user scope)
claude mcp add --transport http -s user doc-db http://localhost:8080/mcp
```

Claude Desktop（`~/Library/Application Support/Claude/claude_desktop_config.json`）：

```json
{
  "mcpServers": {
    "doc-db": {
      "url": "http://localhost:8080/mcp"
    }
  }
}
```

---

## MCP ツール一覧（設計）

| ツール | 説明 | 実装状態 |
|--------|------|----------|
| `upsert_documents` | ドキュメントを登録・更新。テキスト直送または URL 指定 | 未実装 |
| `delete_documents` | 指定 series のドキュメントを削除 | 未実装 |
| `query` | 自然言語クエリでチャンクを検索 | 未実装 |
| `manage` | KEY 一覧取得・インデックス削除 | 未実装 |

### query の検索モード（設計）

| mode | 説明 |
|------|------|
| `emb` | ベクトル類似度のみ |
| `lex` | BM25 語彙一致のみ |
| `hybrid` | ベクトル + BM25 の融合スコア |
| `rerank` | hybrid 上位候補を LLM で再ランク（デフォルト・最高精度） |

### series による多バージョン管理（設計）

`key` はインデックスの名前空間、`series` は同一ドキュメントセットの複数バージョンを識別する：

```
key: "myrepo"
  series: "main"      → main ブランチのスナップショット
  series: "feature-x" → feature ブランチのスナップショット
```

同一 `key + path` でハッシュが一致する場合、Embedding は共有される（重複 API 呼び出しなし）。

## ドキュメント

| 文書 | 内容 |
|------|------|
| `docs/specs/base/requirements/APP-001` | 基本機能要件定義書 |
| `docs/specs/base/design/DES-001` | 基本設計書（アーキテクチャ・データモデル・検索・YAML 設定方式） |
| `docs/specs/install/requirements/APP-002` | インストール要件定義書（Homebrew 自家 tap） |
| `docs/specs/install/design/DES-002` | インストール設計書（Formula・整合性検証・caveats） |
| `CHANGELOG.md` | バージョン履歴（keep-a-changelog 形式） |
| `VERSION` | canonical バージョン文字列（plain text） |

## ライセンス

MIT
