# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.2] - 2026-06-27

### Changed (アルゴリズムを reference doc-db SKILL と完全同期)

ユーザー指摘により、reference doc-db SKILL (Python 実装、`reference/doc-db/scripts/`) との
アルゴリズム差異を全項目修正した。これらは設計書上は同等仕様だが、実装上の細部で
精度に影響する差があった。

- **chunker**: chunk に `EmbedText` フィールドを追加。Embedding API に渡すテキストは
  `<heading breadcrumb>\n\n<prose 本文 (見出し行除去)>` 形式とし、prose < 50 chars の
  短文 chunk は同一 path の前 chunk から prose を継承する（reference の
  `_enrich_embed_texts` と同等）。これにより heading-only chunk でも階層コンテキストが
  ベクトル化され、言い換えクエリでの精度が向上
- **chunker**: `MAX_CHUNK_CHARS` のデフォルトを `1500` → `8192` に変更（reference と同値）。
  小さすぎる chunk が乱立してノイズ化していたのを解消
- **lex (search)**: BM25 を tokenize-list 比較から **substring match**（`strings.Count(body, token)`）
  に変更。文字数ベースの dl/avgdl で正規化。reference の `lexical_search.py` と同等。
  CJK で形態素解析器なしで部分マッチが効くようになる
- **EMB top-K 保証**: 「emb top-K を fused 先頭に連結」から「侵入者の最高スコアを
  超えるよう RRF スコアを書き換えて昇格」に変更。emb 内の相対順位を保ったまま fused
  の他順位も崩さない（reference `hybrid_score.py:49-66` と同等）
- **Reranker interface**: 戻り値を `[]int (順位)` から `[]float64 (scores)` に変更。
  search.Pipeline 側で `(-rerank_score, -original_score, chunk_id)` のブレンドソートを実施。
  欠落 ID は `-1.0` で末尾扱い（reference `llm_rerank.py` と同等）
- **reranker**: 出力スキーマを `{"ranked":[id]}` から `{"ranking":[{"id","score":0..1}]}` に
  変更。preview は `heading_path + body` を空白区切り 200 tokens に切り詰め（reference の
  `build_preview` と同等）。候補数は `min(len(fused), 30, top_n × factor)` で動的決定
- **store**: `bm25_stats` / `bm25_df` テーブルを廃止。substring match に移行したため
  事前 token 集計は不要。schema に `DROP TABLE IF EXISTS` を追加（既存 DB は次回起動時に
  自動マイグレーション）。`insertBM25StatsForChunk` / 削除時の DF 減算ロジックも除去

## [0.1.1] - 2026-06-27

### Added

- `internal/reranker`: OpenAI Chat Completions（gpt-4o-mini 等）を用いた LLM Rerank の具象実装（DES-001 §6.4）。v0.1.0 では interface のみで `cmd/docdb` が nil 注入していたため `mode=rerank` が実質 hybrid と同等にフォールバックしていた問題を解消
- `cmd/docdb`: Reranker を `search.Pipeline` に配線（API キーは embedder と共通の `OPENAI_API_DOCDB_KEY`）

### Changed

- `doc-db.yaml.example` / Formula caveats / README / 設計書のデフォルト port を `8080` → `58080` に変更（dynamic range から選定。dev server との衝突を回避）
- `search.Pipeline`: `mode=rerank` で reranker 未注入または API 失敗時、`stats.RerankCandidates` を 0 のままにする（旧: fused 数と同値で誤解を生んでいた）。Rerank 不発を caller が確実に判別可能になる

### Fixed

- `mode=rerank` で `score_breakdown.rerank` が常に 0 になっていた問題（Reranker 未配線が原因）

## [0.1.0] - 2026-06-27

### Added

- プロジェクト骨格・go.mod・依存パッケージ初期化
- `internal/store`: SQLite スキーマ + CRUD + WAL 設定 + AppendAndCleanSeries アトミック複合メソッド（DIF-02 対応）
- `internal/chunker`: Markdown 見出し境界チャンク分割（H1〜H6、見出し階層パス保持、最大サイズ 1500 文字）
- `internal/embedder`: OpenAI Embedding API（text-embedding-3-small / 1536 次元）。Embed がスキップインデックスを返す（部分失敗対応、DES-001 §5.2）
- `internal/fetcher`: HTTP フェッチャー（SSRF 防御・Content-Type チェック付き）
- `internal/search`: ハイブリッド検索パイプライン（BM25 + コサイン類似度 + RRF + LLM Rerank、DES-001 §6）
- `internal/expiry`: TTL/LRU 廃棄ワーカー（context 対応シャットダウン）
- `internal/mcp`: MCP ハンドラ 6 種（upsert_documents / delete_documents / query / list_indexes / delete_index / manage_index）
- `internal/config`: YAML 固定ファイル `~/.doc-db/doc-db.yaml` から起動時設定を読み込む（DES-001 §9）
- `cmd/docdb`: エントリポイント・MCP サーバー起動・Expiry ワーカー起動
- `--version` / `-v` フラグの早期終了分岐（APP-002 VER-03）
- `doc-db.yaml.example` 同梱（DES-002 §5.2）
- `Makefile`（`make build` で ldflags 経由のバージョン値注入。`make verify` で整合性検証）
- インストール設計書 APP-002 / DES-002（Homebrew 自家 tap 配布）
- `Formula/doc-db.rb`（Homebrew Formula、tap 名 `blueeventhorizon/doc-db`）
- `scripts/verify_version_consistency.sh`（VERSION / CHANGELOG / .version-config.yaml / Formula tag 整合性検証）
- `scripts/verify_release_tag.sh`（Formula revision == git tag commit SHA 検証）
- `.version-config.yaml` sync_files に `Formula/doc-db.rb` を追加（`/forge:update-version` で自動更新）
- 全パッケージの単体テスト + DES-001 §11 統合テスト（upsert / query / WAL 並行 / 廃棄ポリシー / エラーハンドリング）。go test ./... / go vet ./... / go test -race ./... 全 116 件 pass

### Fixed

- bm25_stats INSERT の `key` カラム欠損を修正
- CJK regex を `[^\x00-\x7F]+` に修正（Go RE2 の `\W` は ASCII 専用のため）
- bm25_df の DF 計算: `termSet` + `df -= 1` に統一（DF はレコード単位、DES-001 §6.2）

[Unreleased]: https://github.com/BlueEventHorizon/doc-db-mcp-server/compare/v0.1.2...HEAD
[0.1.2]: https://github.com/BlueEventHorizon/doc-db-mcp-server/releases/tag/v0.1.2
[0.1.1]: https://github.com/BlueEventHorizon/doc-db-mcp-server/releases/tag/v0.1.1
[0.1.0]: https://github.com/BlueEventHorizon/doc-db-mcp-server/releases/tag/v0.1.0
