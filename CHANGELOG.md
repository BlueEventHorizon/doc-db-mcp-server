# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- `internal/config` パッケージ新設（YAML 固定ファイル `~/.doc-db/doc-db.yaml` から起動時設定を読み込む。DES-001 §9）
- `doc-db.yaml.example` 同梱（DES-002 §5.2）
- `--version` / `-v` フラグの早期終了分岐（APP-002 VER-03）
- `Makefile`（`make build` で ldflags 経由のバージョン値注入。`make verify` で整合性検証）
- インストール設計書 APP-002 / DES-002（Homebrew 自家 tap 配布）
- `Formula/doc-db.rb`（Homebrew Formula、tap 名 `blueeventhorizon/doc-db`、tag `v0.1.0` / revision は git tag 作成後に確定）
- `scripts/verify_version_consistency.sh`（VERSION / CHANGELOG / .version-config.yaml / Formula tag 整合性検証）
- `scripts/verify_release_tag.sh`（Formula revision == git tag commit SHA 検証）
- `.version-config.yaml` sync_files に `Formula/doc-db.rb` を追加（`/forge:update-version` で自動更新）

### Changed
- 動作設定の出所を環境変数（`DOCDB_*`）から YAML ファイルに変更（DES-001 §9）
- `embedder.ConfigFromEnv` を廃止し `APIKeyFromEnv`（シークレットのみ）に分離
- `fetcher.ConfigFromEnv` を廃止（呼び出し側で `config.FetcherConfig` から組み立て）
- `chunker.New()` を引数受け取り型 `chunker.New(maxChunkSize int)` に変更
- Go module path を `github.com/k2moons/doc-db-mcp-server` から `github.com/BlueEventHorizon/doc-db-mcp-server` に変更（実リポジトリ URL と一致させ、将来の `go install github.com/BlueEventHorizon/doc-db-mcp-server/cmd/docdb@vX.Y.Z` をサポート可能にする）

## [0.1.0] - 2026-06-24

### Added
- プロジェクト骨格・go.mod・依存パッケージ初期化
- BM25 トークナイザー（DES-001 §6.2 / LEX-01）— ID パターン・ASCII 英数字・CJK 非 ASCII・数字列の優先マッチ
- Store: AppendAndCleanSeries アトミック複合メソッド（DIF-02 対応）
- Embedder: Embed がスキップインデックスを返すよう署名変更（DES-001 §5.2）
- HTTP フェッチャー: SSRF 防御・Content-Type チェック付き実装
- Expiry ワーカー: TTL/LRU 期限切れワーカー（context 対応シャットダウン）
- サーバーエントリポイント骨格実装（ConfigFromEnv / store.New / expiry worker 起動）

### Fixed
- bm25_stats INSERT の `key` カラム欠損を修正
- CJK regex を `[^\x00-\x7F]+` に修正（Go RE2 の `\W` は ASCII 専用のため）
- bm25_df の DF 計算: `termSet` + `df -= 1` に統一（DF はレコード単位、DES-001 §6.2）

[Unreleased]: https://github.com/BlueEventHorizon/doc-db-mcp-server/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/BlueEventHorizon/doc-db-mcp-server/releases/tag/v0.1.0
