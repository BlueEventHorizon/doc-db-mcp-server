# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

[Unreleased]: https://github.com/k2moons/doc-db-mcp-server/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/k2moons/doc-db-mcp-server/releases/tag/v0.1.0
