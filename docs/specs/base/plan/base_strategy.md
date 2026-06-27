# base 実装戦略

## アプローチ

**選択**: ボトムアップ + リスク駆動（複合）

**根拠**:

DES-001 の依存グラフは明確な階層構造を持つ。`store` は外部依存なし、`chunker` も外部依存なし、この2つが他の全モジュールの基盤となる。依存が深いため、ボトムアップが基本戦略として適切。

一方、`embedder`（OpenAI Embedding API）と `internal/mcp`（MCP go-sdk Streamable HTTP transport）はいずれも外部 API・外部ライブラリに依存する技術的不確実性を持つ。特に Streamable HTTP は MCP 2025-03 仕様の新しい transport であり、go-sdk との統合動作が未検証。これらを早期にスパイクして手戻りリスクを潰す必要があるため、リスク駆動の要素を第2フェーズに組み込む。

結果として「基盤を固めながら、リスク要素を早期に解決する」複合アプローチが最適。

## フェーズ

### フェーズ 1: データ基盤とテキスト処理

- **目標**: SQLite スキーマの初期化・CRUD 操作・Markdown チャンク分割が正常動作する。外部 API なしで完結する基盤モジュールが単体テストを通過する。
- **スコープ**:
  - `internal/store` — SQLite スキーマ（§4.1: keys/records/series_keys/chunks/embeddings/bm25_stats/bm25_df）、WAL モード・接続プール設定（§4.2）、UpsertRecord/DeleteSeries/GetChunksForSearch/ListKeys/DeleteKey/TouchKey
  - `internal/chunker` — Markdown 見出し境界チャンク分割（§3.1、`DOCDB_MAX_CHUNK_SIZE` 対応）
  - プロジェクト骨格: `go.mod`、ディレクトリ構成、lint/vet 設定
- **検証ポイント**:
  - `go build ./...` が通ること
  - `store` 単体テスト: インメモリ SQLite（`file::memory:`）で SQL クエリ正確性を検証。DIF-02（同一ハッシュ時の series 追記）・DIF-03（新規/変更時の record 作成）・CleanOtherSeries のロジックを確認
  - `chunker` 単体テスト: 見出し境界での分割境界・`DOCDB_MAX_CHUNK_SIZE` 超過時の追加分割を確認
  - `go test ./internal/store/... ./internal/chunker/...` が全通過

### フェーズ 2: 外部 API 連携とリスク解決

- **目標**: OpenAI Embedding API 呼び出し（リトライ含む）・URL フェッチ（SSRF 対策含む）・BM25/コサイン類似度/RRF の検索パイプラインが動作する。技術的不確実性（外部 API 統合）を本フェーズで解決する。
- **スコープ**:
  - `internal/embedder` — OpenAI Embedding API 呼び出し（§7.1: バッチ上限 100、指数バックオフ 3 回、タイムアウト 60s）、インターフェース定義（テスト用モック可能化）
  - `internal/fetcher` — HTTP/HTTPS コンテンツ取得（§7.2: タイムアウト 30s、リダイレクト最大 5 回、SSRF 対策 RFC1918/ループバック/リンクローカルブロック）、インターフェース定義
  - `internal/search` — コサイン類似度・BM25（LEX-01 トークナイザ: NFKC + 正規表現、ID ボーナス +10.0、クエリ全文ボーナス +2.0）・RRF（k=60）・EMB フォールバック・EMB top-K 保証（§6.1〜6.3）、LLM Rerank（§6.4: gpt-4o-mini、フォールバック RR-02）
- **検証ポイント**:
  - `embedder` 単体テスト: モック HTTP サーバーでリトライロジック（3 回リトライ・バックオフ間隔）を検証
  - `fetcher` 単体テスト: SSRF 対策（RFC1918 アドレスでエラー返却）を検証
  - `search` 単体テスト: コサイン類似度の計算結果・BM25 スコア（k1=1.5, b=0.75）・RRF 融合結果・EMB top-K 保証の動作を固定値で検証
  - `go test ./internal/embedder/... ./internal/fetcher/... ./internal/search/...` が全通過

### フェーズ 3: MCP 統合とエントリポイント

- **目標**: MCP サーバーが起動し、`upsert_documents`・`delete_documents`・`query`・`manage_index` の全ツールが MCP クライアントから呼び出せる。廃棄ワーカーがバックグラウンドで動作する。
- **スコープ**:
  - `internal/mcp` — UpsertHandler（§5.2: DIF-02/03 フロー、M1 ハッシュ正規化、M2 部分保存）・DeleteHandler・QueryHandler（§5.3: TouchKey + SearchPipeline）・ManageHandler（§8.4: EXP-04 廃棄ポリシー設定）
  - `internal/expiry` — TTL ワーカー（§8.1: EXP-01）・LRU ワーカー（§8.2: EXP-02）、バックグラウンドゴルーチン、`DOCDB_EXPIRY_INTERVAL` 対応
  - `cmd/docdb` — 環境変数設定読み込み（§9 全変数）・サーバー起動・`OPENAI_API_DOCDB_KEY` fail-fast（§10）
  - MCP go-sdk Streamable HTTP transport 統合（§1: 採用理由確認済み）
- **検証ポイント**:
  - `go build ./cmd/docdb` が通ること
  - サーバーを起動し、MCP クライアント（または curl）で `upsert_documents` を呼び出してチャンク・embedding が DB に保存されること（Embedder モックまたは実 API）
  - `delete_documents` でレコードが削除されること
  - `query` で mode=emb/lex/hybrid/rerank の各モードが動作すること
  - `manage_index` で廃棄ポリシーが設定・取得できること
  - `OPENAI_API_DOCDB_KEY` 未設定でサーバーが即時終了すること

### フェーズ 4: テスト完全網羅と品質担保

- **目標**: §11 に規定された全テスト（単体・統合）が通過し、本番運用可能な品質に達する。
- **スコープ**:
  - 統合テスト: `upsert_documents` の series_keys 共有フロー（同一ハッシュで Embedding スキップ）、`query` の mode 別結果差異
  - 廃棄ポリシー統合テスト: TTL/LRU による KEY 削除動作
  - WAL 並行テスト: `os.MkdirTemp` の実ファイル SQLite で複数ゴルーチンの同時読み書き（§11: インメモリ DB では WAL 挙動検証不可）
  - エラーハンドリング検証: Fetcher タイムアウト・Embedder 部分失敗（M2 部分保存）・Reranker フォールバック（RR-02）
  - 設定値境界テスト: `DOCDB_MAX_CHUNKS` 上限到達時の LRU 動作
- **検証ポイント**:
  - `go test ./...` が全通過
  - 統合テスト: 同一ハッシュ upsert で Embedding API が呼ばれないこと（モックで確認）
  - 統合テスト: mode=hybrid の結果が mode=emb / mode=lex 単体と異なること
  - WAL 並行テスト: 複数ゴルーチン同時アクセスでデータ競合・デッドロックが発生しないこと
  - `go vet ./...`・`go build ./...` がクリーン

## リスクと対策

| リスク | 影響度 | 対策（どのフェーズで潰すか） |
| ------ | ------ | ---------------------------- |
| MCP go-sdk Streamable HTTP transport の go-sdk 統合動作が未検証 | 高 | フェーズ 3 で早期スパイク。ツールハンドラの実装前に transport 単体で接続確認を行う |
| OpenAI Embedding API のバッチ上限・レート制限による遅延 | 中 | フェーズ 2 でリトライロジック・バックオフを実装。モック HTTP サーバーで検証 |
| BM25 + RRF の計算実装誤り（特に EMB top-K 保証・フォールバック条件） | 中 | フェーズ 2 の `search` 単体テストで固定値・境界値を網羅的に検証 |
| series_keys の CleanOtherSeries 漏れ（DIF-02 経路での剥がし忘れ） | 中 | フェーズ 1 の `store` 単体テストで DIF-02/DIF-03 両経路を明示テスト |
| SQLite WAL モードの並行アクセス競合（接続プール + Mutex 設計の検証不足） | 中 | フェーズ 4 の WAL 並行テスト（実ファイル SQLite）で再現確認 |
| SSRF 対策の IPv6・リンクローカルアドレス検証漏れ | 低 | フェーズ 2 の `fetcher` 単体テストで RFC1918 全レンジ + IPv6 ループバック + リンクローカルを網羅 |
| `embeddings.dim` 不一致による起動時 fail-fast の実装誤り（§4.1） | 低 | フェーズ 3 の `cmd/docdb` 起動テストで異なる dim を持つ DB でのエラー動作を確認 |
