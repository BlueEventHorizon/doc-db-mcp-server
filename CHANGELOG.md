# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.10] - 2026-07-01

### Added

- **config: `db_path` のチルダ展開**サポート。`~/.doc-db/docdb.sqlite` のような
  `~/`-prefixed パスを `$HOME` に展開する。従来は literal `~` ディレクトリが cwd に
  作られる不都合があった
  - `expandTilde` ヘルパを `internal/config` に追加
  - `~/...` → `$HOME/...` に置換
  - 単独 `~` → `$HOME` に置換
  - `~otheruser/...` 形式は誤爆防止でそのまま返す (POSIX 慣習)
  - 空文字列や `~` を含まないパスはそのまま返す
- `doc-db.yaml.example` のデフォルト `db_path` を `./docdb.sqlite` から
  `~/.doc-db/docdb.sqlite` に変更 (cwd 非依存で使いやすく)
- テスト 5 件追加 (HomeSlash / HomeOnly / NoTilde / TildeUser / LoadFrom 統合)

## [0.1.9] - 2026-07-01

### Added

- **新 MCP tool `delete_series`** (APP-001 DEL-03): KEY 内の全 record から指定 series を
  一括除去する。branch cleanup 用途:
  - `Store.DeleteSeriesAll(ctx, key, series) (removed, updated int, err error)` 実装
  - series 除去後 series_keys が空になった record は物理削除 (removed カウント)
  - 他 series が残る record は保持 (updated カウント)
  - 存在しない series を指定してもエラーにならない (no-op)
- **`/delete-db-series <name>` SKILL** 新設 (`.claude/skills/delete-db-series/`):
  - 引数の series を specs / rules 両 KEY から一括除去
  - 現在 checkout 中の branch を指定した場合は警告
  - `git rev-parse --abbrev-ref HEAD` の結果と比較して安全確認
- Store テスト 2 件 + MCP handler テスト 3 件追加

### Changed

- `.claude/skills/README.md` に `/delete-db-series` を追加
- APP-001 FNC-002 タイトルを `delete_documents / delete_series` に更新、DEL-03 追加
- `Register` コメントを「MCP ツール 6 種」→「7 種」に更新

## [0.1.8] - 2026-07-01

### Added

- **`upsert_documents` に `local_path` フィールド追加**。ローカル運用時にファイル本文を
  MCP payload で送らずに、doc-db サーバー側で絶対パスから直接読み込むための経路。
  `content` / `url` / `local_path` は排他 (exactly-one)。
  - 安全制約: 絶対パスのみ、`..` 要素 reject、シンボリックリンク解決後の実パスも再検証、
    10MB サイズ上限、regular file 限定
  - MCP payload 削減の効果大 (大容量 Markdown を content で送ると 100KB+ になるが
    local_path なら数十バイトで済む)
- `internal/mcp/mcp.go` に `readLocalDocument` ヘルパを追加、対応するテスト 6 件追加
  (ReadsFile / RelativePathRejected / TraversalRejected / NotFound /
   ThreeSourcesMutuallyExclusive / ContentURL 両指定は既存維持)

### Changed

- `.claude/skills/{update,query}-db-{specs,rules}/scripts/resolve_docs.py` を拡張:
  出力 JSON に `entries: [{path, local_path}, ...]` を追加 (相対 path + 絶対 local_path)
- `.claude/skills/update-db-{specs,rules}/SKILL.md` を local_path 経路使用に更新
  (従来の `content` 送信から切替、payload 大幅削減)
- `docs/AI_INTEGRATION_GUIDE.md` に 3 経路 (content/url/local_path) の使い分け例を追加
- APP-001 FNC-001 documents フィールド説明を 3 経路対応に更新 (改定履歴: 2026-07-01 エントリ)
- DES-001 §5.2 upsert シーケンス冒頭に 3 経路の表と local_path の安全性制約を追記 (v0.6)

## [0.1.7] - 2026-06-29

### Fixed

- `query` ツールの `query` field に jsonschema description が抜けていた問題を修正。
  v0.1.6 の tools/list 実検証で発見。`tools/list` レスポンスで `query` field が
  `(no description)` 状態だったのを「検索クエリ。自然言語の質問でも、ID/固有名詞/
  関数名のような literal 文字列でも可」と明示するよう修正

## [0.1.6] - 2026-06-28

### Added (AI consumer 向けドキュメント拡充)

「AI skill から呼び出して使う」観点でユーザーが指摘した不足箇所を解消。
`tools/list` だけで AI agent が doc-db を使いこなせるよう、説明を厚くした。

- **MCP tool descriptions の大幅拡充** (6 tool すべて):
  - 概念モデル (KEY / series / path) を tool description 内に明記
  - `query` には PHIL-01 二層アーキ・mode の使い分け・origin_signals 解釈を含める
  - `upsert_documents` の content/url 排他・部分失敗の扱いを明記
  - `manage_index` の TTL/max_chunks 仕様を明記
- **jsonschema field tags の全付与**:
  - `UpsertInput` / `UpsertDocument` / `UpsertResult` / `UpsertError`
  - `DeleteInput` / `DeleteResult`
  - `QueryInput` / `QueryHit` / `QueryResult`
  - `ListIndexesResult` / `DeleteIndexInput` / `DeleteIndexResult`
  - `ManageIndexInput` / `ManageIndexResult`
  - `tools/list` レスポンスで全 field に `description` が含まれるようになり、
    AI agent が input/output の意味を schema だけで理解できる
- **`docs/AI_INTEGRATION_GUIDE.md`** 新設:
  - 設計思想 (PHIL-01 二層アーキ・PHIL-02 Rerank の位置付け)
  - 概念モデル (KEY 設計・series 戦略・path)
  - 6 tool の使い分け
  - `mode` 別の選択指針 (all/rerank/emb/lex/grep/hybrid)
  - `origin_signals` / `stage_stats` / `warnings` の解釈
  - 典型フロー (セットアップ/検索/branch 更新)
  - エラー処理ベストプラクティス
  - FAQ
- **README.md**: ドキュメント表に AI 統合ガイドを追加 (最上位)

### Notes

実装ロジックの変更は無し。コードコメントの追加 + jsonschema タグ追加 + 新規ドキュメント。
全テスト pass (go test ./... / -race)。

## [0.1.5] - 2026-06-28

### Added (PHIL-01 二層検索アーキ + 全文 GREP signal)

ユーザー指摘「文書検索では取りこぼし回避が最重要、Forge/DocAdvisor は Embedding + BM25 +
GREP の併用で over-recall して AI agent に判定を委ねる」を受け、本サーバーにも 3 signal
並列検索を導入。APP-001 / DES-001 を改訂し、新規実装。

- **設計書改訂** ([APP-001](docs/specs/base/requirements/APP-001_doc_db_mcp_server_requirements.md) / [DES-001](docs/specs/base/design/DES-001_doc_db_mcp_server_design.md)):
  - PHIL-01: 二層検索アーキ (Layer 1=本サーバー候補収集 / Layer 2=上位 AI agent 内容判定)
  - PHIL-02: LLM Rerank は ranking 最適化オプションであり recall を広げる手段ではない
  - GRP-01/GRP-02: 全文 GREP signal の必須化
  - ALL-01: `mode=all` で 3 signal 並列実行
  - QRY-OUT-03: 各 chunk の `origin_signals: [emb,lex,grep]` を出力
- **`internal/search/grep.go`** 新設: `computeGrepScores` (NFKC + lowercase の substring 一致、出現回数 score)
- **`internal/search/search.go`**:
  - `ModeGrep` / `ModeAll` 定数追加
  - `SearchResult.OriginSignals []string` 追加
  - `ScoreBreakdown.Grep float64` 追加
  - `StageStats.GrepCandidates` / `MergedCandidates` 追加
  - `mergeThreeSignals` 関数で 3 signal 合算 (signal hit 数 → emb_score → chunk index でソート)
  - `filterPositiveRank` で lex/grep モードの score>0 絞り込み
- **`internal/mcp/mcp.go`**:
  - `QueryHit.OriginSignals` フィールド追加 (QRY-OUT-03)
  - クエリ `mode` のデフォルトを `rerank` → **`all`** に変更 (PHIL-01)

### Changed (Breaking)

- **`query` ツールのデフォルト `mode` を `rerank` から `all` に変更**。
  既存クライアントが `mode` 省略時、3 signal 並列実行 + GREP 結果込みの候補プールが
  返るようになる。従来の rerank 動作が必要な場合は明示的に `mode: "rerank"` を指定。

### Notes

- `mode=rerank` は内部実装が「emb+lex RRF → rerank」から「3 signal merge → rerank」に
  変更された。LLM Rerank の入力候補に GREP hit も含まれる
- `mode=hybrid` は legacy 互換として emb+lex RRF のまま保持 (grep を含まない)
- `score_breakdown.grep` 追加、`stage_stats.grep_candidates` / `merged_candidates` 追加
- 全テスト pass (chunker / store / search / mcp / reranker / expiry / embedder / fetcher、race 通過)

## [0.1.4] - 2026-06-27

### Fixed (Q3/Q7 silent rerank failure 究明 + 修正)

v0.1.3 評価で Q3 ("automatic cleanup of stale indexes") と Q7 ("DIF-03") のみ
`stage_stats.rerank_candidates=0` となる現象を究明した結果、**gpt-4o-mini が
30 candidates (id 0-29) に対し id=30 を含むランキングを返す** off-by-one が
判明。我々の parseRankingScores が範囲外 id を error 扱いしていたため rerank
全体が破棄されていた。

- `reranker.parseRankingScores`: 範囲外/不正な id を error → graceful skip に変更。
  reference llm_rerank.py:115 と同方針（rank_map に登録するだけで lookup 時に
  見つからず無視）。silent failure 禁止のため、skip した id は `dropped_ids` として
  `slog.Warn` に記録（観測可能）

### Changed (silent failure 全箇所を検出可能化)

ユーザー指摘「エラーはログだけではダメ。caller が明確に捕まえられないと気づかない」を
受け、全 silent failure サイトを propagate 可能な形に修正。

- **`store.go`**: `defer tx.Rollback() //nolint:errcheck` × 4 箇所を `rollbackErrInto`
  ヘルパに置換。named return + `errors.Join` で Rollback 失敗を caller の error 返り値に伝達。
  `sql.ErrTxDone` (benign) は除外
- **`embedder.go`**: 複数バッチ失敗時の `firstErr` のみ保持を `errors.Join(batchErrs...)` に
  変更。全 batch エラーを caller に伝達
- **`mcp.go` `QueryResult`**: `Warnings []string` フィールド追加。TouchKey 失敗を
  log だけでなく MCP レスポンスにも含める
- **`search.go` `Output`**: `Warnings []string` フィールド追加。Rerank API 失敗 /
  EMB フォールバック発動を caller に伝達。`fuseScores` 戻り値に `embFallback bool` 追加
- **`expiry.go` `Worker`**: `Stats()` メソッドで `KeyDeleteError` リスト・`LastRunErr` ・
  `TotalRuns` ・`LastRunAtRF` を公開。個別 KEY 削除失敗をログだけでなく構造化状態として保持
- 全変更で対応するテスト追加（dropped_ids / Stats.LastKeyErrors / EMB fallback bool 等）

### Memory

- 新規 feedback: `silent failure 禁止` をプロジェクト memory に記録
  ([feedback_no_silent_failure.md](https://github.com/BlueEventHorizon/doc-db-mcp-server/blob/main/.claude/memory/feedback_no_silent_failure.md))

## [0.1.3] - 2026-06-27

### Changed (reference doc-db SKILL との追加同期 — 残存差異 5 件)

v0.1.2 後の詳細監査で発見した reference (`reference/doc-db/scripts/*.py`) との
残存差異を全て修正した。設計書には現れない実装上の微細な差が精度に影響していた。

- **Embedding モデル**: デフォルトを `text-embedding-3-small` (1536 dim) →
  `text-embedding-3-large` (3072 dim) に変更（reference と同モデル）。日本語技術文書の
  recall が向上。コストは ~6.5x だが精度差が顕著
- **heading_path から Markdown 記号除去**: `"# A > ## B > ### C"` →
  `"A > B > C"` 形式に変更（reference [chunk_extractor.py:128](reference/doc-db/scripts/chunk_extractor.py) と同方式）。
  Embedding API に渡す breadcrumb 内の `#`/`##`/`###` がノイズとして
  ベクトル品質を下げていた問題を解消
- **EMB フォールバック判定の修正**: `lex_hits / emb_hits` 比率を「lex_score > 0 の
  chunk 数」で計算するよう修正（v0.1.2 までは全 chunk 数で近似していて事実上
  常時 1.0 となり、フォールバックが発動しなかった）。CJK 言い換えクエリで lex
  がほぼ空振りした際に正しく emb-only モードに切り替わる
- **RRF の lex_rank フィルタ**: lex_score > 0 の chunk のみを lex_rank に含めるよう
  修正（reference [hybrid_score.py:36](reference/doc-db/scripts/hybrid_score.py) と同方式）。v0.1.2 までは全 chunk が末尾 rank で参加し
  て systematic noise を生んでいた
- **Rerank 候補数決定**: `top_n × factor` から `max(top_n, MAX_CANDIDATES=30)` に変更
  （reference [search_index.py:232](reference/doc-db/scripts/search_index.py) と同方式）。小さい top_n でも常に最大 30 候補を LLM に渡し、
  言い換えクエリで emb 上位に正解が無いケースでも救えるようにする

### Breaking

- **Embedding 次元数が 1536 → 3072 に変更**。既存 DB は次元数不一致で起動時 fail-fast。
  ユーザーは `~/.doc-db/doc-db.yaml` の `embedding.dim` を `3072` に更新し、
  `docdb.sqlite` を削除して DB を再構築する必要がある
- `chunks.heading_path` カラムのフォーマット変更（`#` プレフィックス除去）。
  DB 再構築で自動的に新フォーマットになる

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

[Unreleased]: https://github.com/BlueEventHorizon/doc-db-mcp-server/compare/v0.1.10...HEAD
[0.1.10]: https://github.com/BlueEventHorizon/doc-db-mcp-server/releases/tag/v0.1.10
[0.1.9]: https://github.com/BlueEventHorizon/doc-db-mcp-server/releases/tag/v0.1.9
[0.1.8]: https://github.com/BlueEventHorizon/doc-db-mcp-server/releases/tag/v0.1.8
[0.1.7]: https://github.com/BlueEventHorizon/doc-db-mcp-server/releases/tag/v0.1.7
[0.1.6]: https://github.com/BlueEventHorizon/doc-db-mcp-server/releases/tag/v0.1.6
[0.1.5]: https://github.com/BlueEventHorizon/doc-db-mcp-server/releases/tag/v0.1.5
[0.1.4]: https://github.com/BlueEventHorizon/doc-db-mcp-server/releases/tag/v0.1.4
[0.1.3]: https://github.com/BlueEventHorizon/doc-db-mcp-server/releases/tag/v0.1.3
[0.1.2]: https://github.com/BlueEventHorizon/doc-db-mcp-server/releases/tag/v0.1.2
[0.1.1]: https://github.com/BlueEventHorizon/doc-db-mcp-server/releases/tag/v0.1.1
[0.1.0]: https://github.com/BlueEventHorizon/doc-db-mcp-server/releases/tag/v0.1.0
