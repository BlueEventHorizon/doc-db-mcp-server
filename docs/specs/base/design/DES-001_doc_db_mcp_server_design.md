# DES-001 doc-db MCP Server 設計書

## メタデータ

| 項目     | 値         |
| -------- | ---------- |
| 設計ID   | DES-001    |
| 関連要件 | APP-001    |
| 作成日   | 2026-06-20 |

## 1. 概要

Markdown テキストのハイブリッド検索（ベクトル + BM25 + LLM Rerank）を提供する汎用 MCP サーバー。Go で実装し、純粋 Go 製 SQLite（`modernc.org/sqlite`）を採用することで CGO 不要・シングルバイナリ配布を実現する。MCP go-sdk の Streamable HTTP transport を使用し、OpenAI API は標準ライブラリの `net/http` で直接呼び出す。

**Streamable HTTP 採用理由**: MCP go-sdk の標準 transport であり、SSE と比較して双方向ストリーミングに適している。SSE transport は go-sdk のサポートが限定的（サーバー送信専用）であり、ツール呼び出しの応答返却が煩雑になる。Streamable HTTP は MCP 2025-03 仕様で推奨される transport であり、将来互換性の観点からも採用する（PRE-02/NFR-03）。

## 2. アーキテクチャ概要

### 2.1 二層検索アーキテクチャ (PHIL-01 / PHIL-02)

本サーバーは APP-001 PHIL-01 で定義される**二層検索アーキテクチャ**を実装する:

- **Layer 1 (本サーバー)**: Embedding + BM25 + 全文 GREP の 3 signal を並列実行し、
  各 signal の上位候補を合算した「取りこぼし無き候補プール」を返す。
  signal は互いに異なる種類の miss を補完する関係であり、いずれも代替できない。
- **Layer 2 (呼び出し側 AI agent)**: 返ってきた候補プールの本文を読んで関連性を
  判断する。本サーバーは関与しない。

LLM Rerank は本来 Layer 1 内部の ranking 最適化オプションであり、recall を広げる
手段ではない。Rerank 未使用時も 3 signal の併用結果が返る (PHIL-02)。

```mermaid
flowchart TB
    Client["MCP クライアント\n(Claude Code 等)\n= Layer 2 AI agent"]
    Server["MCP Server\n(go-sdk / Streamable HTTP)"]
    Tools["Tool Handlers\nupsert / delete / query / trash\nsync / schedule"]
    Chunker["Chunker\nMarkdown → Chunks"]
    Embedder["Embedder\nOpenAI API"]
    Fetcher["Fetcher\nURL → Content"]
    SearchEmb["emb signal\n(vector)"]
    SearchLex["lex signal\n(BM25)"]
    SearchGrep["grep signal\n(literal)"]
    Rerank["LLM Rerank\n(optional)"]
    Merge["Candidate Merge\norigin_signals 記録"]
    Store["Store\nSQLite (modernc)\npending_deletions / keys.trashed_at 含む"]
    Trash["Trash Worker\nゴミ箱投入 KEY・orphan record の\n自動最終処分（定期実行）"]
    Sweep["起動時スイープ\n削除予約の物理削除"]

    Client <-->|"Streamable HTTP"| Server
    Server --> Tools
    Tools --> Chunker
    Tools --> Embedder
    Tools --> Fetcher
    Tools --> SearchEmb & SearchLex & SearchGrep
    SearchEmb & SearchLex & SearchGrep --> Merge
    Merge -.->|"mode=rerank のみ"| Rerank
    Embedder -->|"embedding vectors"| Store
    Fetcher -->|"raw content"| Chunker
    SearchEmb & SearchLex & SearchGrep --> Store
    Trash --> Store
    Sweep --> Store
```

### レイヤー構成と依存方向

```
cmd/          → internal/mcp
internal/mcp  → internal/store, internal/search, internal/chunker, internal/embedder, internal/fetcher
internal/search → internal/store
internal/trash  → internal/store
internal/store  → (外部依存なし)
```

上位レイヤーのみが下位を参照する。循環依存は禁止。

## 3. モジュール設計

### 3.1 パッケージ一覧

| パッケージ          | 責務                                                                                                                                                                   | 主な依存                                                                                         |
| ------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------ |
| `cmd/docdb`         | エントリポイント・設定読み込み・起動時スイープ実行（§8.5）・ジョブ用 root context の生成保持（§5.4）・サーバー起動                                                     | `internal/mcp`, `internal/store`, `internal/trash`                                               |
| `internal/mcp`      | MCP ツールハンドラ（upsert/delete/query/trash_index/list_trashed_indexes/restore_index/sync/schedule）・同期ジョブ状態管理                                             | `internal/store`, `internal/search`, `internal/chunker`, `internal/embedder`, `internal/fetcher` |
| `internal/store`    | SQLite の読み書き・トランザクション管理・KEY 単位排他（`WithKeyLock`、§4.3）・削除予約の記録と回収（§4.5）                                                             | `modernc.org/sqlite`                                                                             |
| `internal/chunker`  | Markdown を見出し境界でチャンク分割                                                                                                                                    | （外部依存なし）                                                                                 |
| `internal/embedder` | OpenAI Embedding API 呼び出し                                                                                                                                          | `net/http`                                                                                       |
| `internal/fetcher`  | HTTP/HTTPS URL からコンテンツ取得                                                                                                                                      | `net/http`                                                                                       |
| `internal/search`   | 3 signal 並列検索（emb / BM25 lex / 全文 GREP）・候補 merge・LLM Rerank（オプション）                                                                                  | `internal/store`                                                                                 |
| `internal/reranker` | OpenAI Chat Completions による LLM Rerank（PHIL-02: オプション）                                                                                                       | `internal/search`（interface 実装）                                                              |
| `internal/trash`    | ゴミ箱投入済み KEY・orphan record（`pending_deletions`）の自動最終処分ワーカー（定期実行。KEY 削除は `WithKeyLock` 経由、§4.3。旧 `internal/expiry`（TTL/LRU）を置換） | `internal/store`                                                                                 |

### 3.2 主要な型関係

```mermaid
classDiagram
    class Server {
        +Run(ctx) error
    }
    class UpsertHandler {
        +Handle(ctx, req) UpsertResult
    }
    class DeleteHandler {
        +Handle(ctx, req) DeleteResult
    }
    class QueryHandler {
        +Handle(ctx, req) QueryResult
    }
    class TrashHandler {
        +HandleTrashIndex(ctx, req) TrashIndexResult
        +HandleListTrashedIndexes(ctx, req) ListTrashedIndexesResult
        +HandleRestoreIndex(ctx, req) RestoreIndexResult
    }
    class Store {
        +UpsertRecord(ctx, rec) (int64, error)
        +DeleteSeries(ctx, key, series, paths) error
        +GetChunksForSearch(ctx, key, series) []Chunk
        +ListKeys(ctx) []KeyInfo
        +DeleteKey(ctx, key) error
        +TouchKey(ctx, key) error
        +WithKeyLock(ctx, key, fn) error
        +DetachSeriesFromPath(ctx, key, series, path) (bool, error)
        +TrashKey(ctx, key) (trashedAt string, error)
        +RestoreKey(ctx, key) error
        +ListTrashedKeys(ctx) []TrashedKeyInfo
        +IsTrashed(ctx, key) (bool, error)
        +ListPendingDeletionsOlderThan(ctx, cutoff) []PendingDeletionEntry
        +SweepOnePendingDeletion(ctx, entry) error
    }
    class Chunker {
        +Split(path, content) ([]Chunk, error)
    }
    class Embedder {
        +Embed(ctx, texts) []Vector
    }
    class Fetcher {
        +Fetch(ctx, url) string
    }
    class SearchPipeline {
        +Run(ctx, key, series, query, mode, topN) []SearchResult
    }
    class TrashWorker {
        +Start(ctx)
    }

    Server --> UpsertHandler
    Server --> DeleteHandler
    Server --> QueryHandler
    Server --> TrashHandler
    TrashHandler --> Store
    UpsertHandler --> Chunker
    UpsertHandler --> Embedder
    UpsertHandler --> Fetcher
    UpsertHandler --> Store
    DeleteHandler --> Store
    QueryHandler --> SearchPipeline
    SearchPipeline --> Store
    TrashWorker --> Store
```

**型定義**:

- `KeyInfo`: `ListKeys` の戻り値要素。`key string`・`series []string`・`doc_count int`・`chunk_count int`・`last_updated_at string`・`last_accessed_at string` を含む。MNG-01「KEY・series 一覧・ドキュメント数・chunk 数・最終更新日時・最終アクセス日時を取得できること」に対応する（ゴミ箱投入済み KEY は結果から除外する。FNC-007 TRS-04）。`series` は `series_keys JOIN records` の `DISTINCT`（`fetchSeriesForKey`）であり、record の紐付きが残っていない series は現れない（この帰結として生じる「未同期」と「同期済みだが空」の区別不能性は §4.5 参照）。紐づく series が 0 件の KEY では `fetchSeriesForKey` が nil slice を返すため、`list_indexes` の JSON 応答では `series` が `null` になる（Go の nil slice の直列化。空一覧 `[]` と同義であり、意味の異なる状態を表すものではない。Issue #8）。旧 `expiry_policy` フィールドは TTL/LRU 廃止（FNC-007）に伴い削除した。
- `TrashedKeyInfo`: `ListTrashedKeys` の戻り値要素。`key string`・`trashed_at string` を含む。自動最終処分までの残り時間は `trashed_at` と設定値 `trash.retention_days`（§9.2）から呼び出し元が算出する（Store 層は判定を持たず事実のみを返す方針。ADR-003）。
- `PendingDeletionEntry`: `ListPendingDeletionsOlderThan` の戻り値要素。`key string`・`series string`・`path string`・`marked_at string` を含む。

## 4. データモデル

### 4.1 SQLite スキーマ

```sql
-- インデックスキー管理
CREATE TABLE keys (
    key             TEXT PRIMARY KEY,
    doc_count       INTEGER NOT NULL DEFAULT 0,
    last_accessed_at TEXT NOT NULL,  -- RFC3339
    last_updated_at  TEXT NOT NULL,
    trashed_at      TEXT             -- RFC3339, NULL = Active（ゴミ箱未投入）。FNC-007
);
-- 旧 expiry_policy TEXT カラム（TTL/LRU 廃止に伴い FNC-007 で撤去。既存 DB へは
-- `ALTER TABLE keys ADD COLUMN trashed_at TEXT` を起動時マイグレーションで追加する。§13 参照）

-- embedding record（key + path ごとにコンテンツ1バージョン）
CREATE TABLE records (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    key             TEXT NOT NULL,
    path            TEXT NOT NULL,
    content_hash    TEXT NOT NULL,   -- SHA-256 hex
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL,
    UNIQUE(key, path, content_hash)
);

-- series_keys（record と series の多対多）
CREATE TABLE series_keys (
    record_id INTEGER NOT NULL REFERENCES records(id) ON DELETE CASCADE,
    series    TEXT NOT NULL,
    PRIMARY KEY (record_id, series)
);

-- チャンク（見出し境界で分割されたテキスト）
CREATE TABLE chunks (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    record_id    INTEGER NOT NULL REFERENCES records(id) ON DELETE CASCADE,
    chunk_index  INTEGER NOT NULL,
    heading_path TEXT NOT NULL,  -- "# A > ## B > ### C"
    text         TEXT NOT NULL
);

-- 埋め込みベクトル（BLOB: float32 配列をリトルエンディアンでシリアライズ）
-- dim カラム: 行ごとにベクトル次元数を記録する。起動時に SELECT DISTINCT dim FROM embeddings を実行し、
-- 結果が embedding.dim と異なる場合は「モデル変更後の DB 再構築が必要」として fail-fast する。
CREATE TABLE embeddings (
    chunk_id  INTEGER PRIMARY KEY REFERENCES chunks(id) ON DELETE CASCADE,
    vector    BLOB NOT NULL,
    dim       INTEGER NOT NULL
);

-- 削除予約（sync_documents / schedule_delete_series が記録し、起動時スイープと
-- internal/trash.Worker の定期実行が回収する。§4.5 / §8.5）
-- path = '' は「series 全体の削除予約」を表すセンチネル。SQLite の PRIMARY KEY は NULL の
-- 重複を許容してしまうため、NULL ではなく空文字列で series 全体 / 特定 path を区別する。
CREATE TABLE pending_deletions (
    key       TEXT NOT NULL,
    series    TEXT NOT NULL,
    path      TEXT NOT NULL DEFAULT '',
    marked_at TEXT NOT NULL,
    PRIMARY KEY (key, series, path)
);

-- bm25_stats / bm25_df は廃止された（v0.1.2 で削除）。
-- BM25 の TF/DF は事前集計テーブルを持たず、query 時に substring match で都度計算する
-- 方式に変更されている（reference doc-db SKILL と同方式）。詳細は §6.2 を参照。
```

### 4.2 並行アクセス方針（NFR-02）

SQLite WAL モードと Go 側ミューテックスの組み合わせで並行アクセスを制御する。

- **WAL モード採用**: 起動時に `PRAGMA journal_mode=WAL` を実行する。WAL モードでは読み取りと書き込みが並行して実行可能になる（書き込み中も読み取りをブロックしない）。
- **接続プール（複数接続）**: `database/sql` のデフォルト接続プールを使用し `SetMaxOpenConns(N)` で複数接続を許可する（N は `runtime.GOMAXPROCS(0)` を基準に設定）。複数の読み取りゴルーチンが独立した接続を取得し WAL の並行読み取り性能を活用する。`SetMaxOpenConns(1)` は採用しない — 単一接続では WAL モードの効果がなく、読み書きが全て直列化される。
- **書き込み直列化（Go 側 Mutex）**: 書き込み操作（upsert/delete/expiry）は Store レイヤーで `sync.Mutex` によって直列化する。SQLite 自体もシングルライタだが、Go 側でトランザクション単位の整合性を保証するために明示的に保護する。読み取り操作には Go 側ロックを掛けない（WAL が担う）。
- **ビジータイムアウト**: `PRAGMA busy_timeout=5000`（5秒）を設定する。書き込みロック競合が発生した場合（内部ミューテックスが解放後に DB レベル競合が起きる稀なケース）の保険として残す。
- **注意**: `sync.RWMutex` は使用しない。WAL + 接続プールが読み取り並行を担うため、Go 側に ReaderLock を設けても実効性がなく複雑になるだけ。
- **KEY 単位排他（`WithKeyLock`）との役割分担**: 上記 `sync.Mutex`（以下 `s.mu`）は個々の SQLite 書き込みトランザクション単位の直列化を担う。これとは**別レイヤー**の排他として、同一 KEY に対する複数回の Store 呼び出しを跨いだ一連の処理を直列化する KEY 単位ロック `WithKeyLock` を Store に設ける（§4.3）。両者は独立したミューテックスであり、`WithKeyLock` の内側で個々の Store メソッドが `s.mu` を取得しても問題は生じない。

### 4.3 KEY 単位排他制御（WithKeyLock）

`sync_documents`（§5.4、FNC-006 SYN-08）は「documents 処理 → 既存 path 一覧取得 → 欠落 path の切り離し・削除予約」を desired-state 判定の単一の論理処理として扱う。この間に同じ KEY へ他の書き込みが割り込むと、判定開始時点の desired-state と処理完了時点の実データがずれる。割り込みうる操作は series 単位のもの（`upsert_documents` / `delete_documents` / `delete_series` / `schedule_delete_series` / 別の `sync_documents`）に留まらず、**KEY 全体を削除する操作**（`trash_index` によるゴミ箱投入・`internal/trash.Worker` による自動最終処分の `DeleteKey`）も含む。KEY 全体削除が同時に走ると、sync 処理中の KEY 自体が消え、存在しない KEY への書き込みや削除と再挿入の競合による不整合が生じる。旧 EXP-01（TTL）・EXP-02（LRU）は FNC-007 により廃止されたが、KEY 全体削除が SYN-08 の排他対象である点は `trash_index`／自動最終処分に引き継がれている。

これを防ぐため、Store は KEY 単位の論理ロック `WithKeyLock(ctx, key, fn func() error) error` を公開する。生の `LockKey(key) (unlock func())` ペアではなくクロージャ形式を採用するのは、呼び出し側の unlock 忘れを構造的に防ぐため。

**実装方式**: バッファ 1 の channel をミューテックス代わりに使う（send がロック取得、receive が解放に相当）。`sync.Mutex.Lock()` はブロッキング呼び出しで待機中にキャンセルできないが、channel であれば `select` で `ctx.Done()` と競合させられ、**ロック待機中でもキャンセルに応答できる**（FNC-006 GC-05: `sync_documents` がロック待ちのままシャットダウンに応答できなくなることを防ぐ。§5.4）。ロック取得後（`fn` 実行中）のキャンセル対応は、`fn` 自身が受け取った `ctx` を見て中断するかどうかに委ねる。KEY ごとのロックエントリは参照カウント方式で管理し、参照中の goroutine が 0 になったエントリは map から削除する（KEY の生成・削除が繰り返されても無制限に蓄積しない）。

**呼び出しルール [MANDATORY]**:

- `WithKeyLock` は各呼び出し元が対象 KEY につき 1 回だけ呼ぶ。**`fn` の内部でネストして `WithKeyLock` を呼んではならない**（非再入のため、同一 goroutine の二重取得はデッドロックする）
- 個々の Store メソッド（`DeleteKey` / `UpsertRecord` / `DeleteSeries` / `DeleteSeriesAll` 等）は KEY 単位ロックを**内部で取得しない**。KEY 単位排他が必要な呼び出し元が、メソッド呼び出し全体を `WithKeyLock` で囲む。「一部のメソッドは自分でロックを取り、一部の呼び出し元は外側から取る」という二重構造は再入デッドロックの温床になるため、取得主体を呼び出し側に統一する
- `fn` は「対象 KEY に対する Store 呼び出し一式」を指し、単一メソッド呼び出しとは限らない。呼び出し元一覧と囲み方:
  - `upsert_documents` ハンドラ: 複数ドキュメント分の `UpsertRecord` 呼び出しを含む、ハンドラの Store 書き込み処理全体を 1 回で囲む
  - `delete_documents` ハンドラ: `HasRecord` による存在チェック（warning 構築）から `DeleteSeries` までを囲む。**存在チェックをロック外に置いてはならない**: `sync_documents` がロック保持中に作成する path を「存在しない」と誤判定してブロックせず即完了し、削除要求を取りこぼす（TOCTOU）
  - `delete_series` ハンドラ: `DeleteSeriesAll` 呼び出しを囲む
  - `trash_index` ハンドラ: `TrashKey` 呼び出しを囲む。呼び出し直前に `IsTrashed` を再確認する（TOCTOU 対策。§4.6）
  - `restore_index` ハンドラ: `RestoreKey` 呼び出しを囲む
  - `schedule_delete_series` ハンドラ: `MarkSeriesForDeletion` 呼び出しを囲む
  - `internal/trash.Worker` の自動最終処分: ゴミ箱投入済み KEY ごとに `IsTrashed` の再確認（復活済みならスキップ）+ `DeleteKey` 呼び出しを囲む（`storeForTrash` インターフェースに `WithKeyLock` / `IsTrashed` を含める。判定ロジックは §8 参照。旧 `internal/expiry.Worker` の TTL/LRU 判定を置換）
  - `sync_documents` のバックグラウンドゴルーチン: desired-state 判定全体（documents 処理 → path 一覧取得 → series 切り離し → 削除予約の記録・解除）を 1 回で囲む（§5.4）。`fn` 内で呼ぶ各 Store メソッドは `WithKeyLock` を持たないため、構造的に再入は起こり得ない

**ロック粒度は KEY 単位**（key+series 単位ではない）とし、同一 KEY 内の異なる series 間であっても並行実行しない。`trash_index` による KEY のゴミ箱投入・自動最終処分の `DeleteKey` は KEY 全体に効くため、series 単位のロックでは「他の series はロックしていないので削除してよい」という誤った並行実行を防げない。単一 KEY 内で書き込み系操作が同時に複数走る想定は薄く（branch 運用は同一 KEY 内の逐次的な series 追加・削除が主）、並行度低下の実害は小さい。

### 4.4 Embedding Record の series_keys 管理

```mermaid
flowchart TB
    U["upsert_documents\n(key, series, [{path, content/url, hash?}])"]
    HashCheck{"同一 key+path に\n同一ハッシュの\nrecord が存在?"}
    Append["series_keys に\nseries を追記\n(再 Embedding スキップ)"]
    CleanOther1["同一 key+path の\n他 record から\nseries を除去\n(空なら record 削除)"]
    NewRec["新規 record 作成\nチャンク分割・Embedding"]
    CleanOther2["同一 key+path の\n他 record から\nseries を除去\n(空なら record 削除)"]

    U --> HashCheck
    HashCheck -->|"Yes (DIF-02)"| Append
    Append --> CleanOther1
    HashCheck -->|"No (DIF-03)"| NewRec
    NewRec --> CleanOther2
```

**重要**: series の剥がし処理（CleanOtherSeries）は DIF-02（同一ハッシュの Append）でも必須。たとえば series=main が hash=H1 に紐づいていた後、hash=H2 で上書きされ H2.series=[main] になった状態で、再び hash=H1 を main で upsert すると H1 が AppendSeries で復活しても H2 に main が残ったままになる。Append/NewRec のどちらの経路でも CleanOtherSeries を実行することで「同一 key+path+series の組み合わせは常に高々1 record」を保証する。

**例外 — sync_documents の series 切り離し（FNC-006 SYN-03）**: 「series_keys が空になった record は即時物理削除」という上記の掃除は、upsert 経路（DIF-02/03）の正であり続ける。一方、`sync_documents` が desired-state から欠落した path を series から切り離す `DetachSeriesFromPath` は、この不変条件の**意図的な例外**であり、orphan（どの series からも参照されない record）を物理削除せず、起動時スイープまたは `internal/trash.Worker` の定期実行のいずれかまで保持する（自己修復を Embedding 再計算なしで成立させるため。詳細は §4.5）。

### 4.5 削除予約と orphan 回収（pending_deletions）

`sync_documents` / `schedule_delete_series`（FNC-006 SYN-03 / GC-01）は不要データを即時に物理削除せず、`pending_deletions` テーブル（§4.1）へ**削除予約**として記録する。物理削除は起動時スイープ、または保持期間経過後の `internal/trash.Worker` 定期実行のいずれかが行う（§8.5）。予約は 2 種類ある:

- **path 単位**（`path` に実 path）: `sync_documents` が desired-state から欠落した path を series から**即時に切り離した**結果、orphan になった record の物理削除予約。切り離し済みのため当該 series 指定の検索からは既に消えており、予約は残骸（record・chunks・embeddings）の回収のみを意味する
- **series 全体**（`path=''` センチネル）: `schedule_delete_series` による series 丸ごとの削除予約。path 単位と異なり**即時切り離しは行わない**（遅延方式）。誤操作時の影響範囲が branch 全体に及ぶため、「予約は起動まで完全に無害（SYN-04 の再 sync で取り消せば何も起きない）」という安全性を優先する。削除済み branch の series を検索する動線は通常存在せず、検索最新性の実害は小さい

orphan record を物理削除せず残す目的は、SYN-04 の自己修復を Embedding 再計算なし（API 課金ゼロ）で成立させること。誤って欠落させた path を同一内容で再 sync すれば、DIF-02 が既存 record を再発見して series を再紐付けする。

#### Store メソッド仕様

| メソッド                                                                     | 動作                                                                                                                                                                                                                                                                                                                                                                                                                             |
| ---------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `MarkSeriesForDeletion(ctx, key, series) (alreadyScheduled, error)`          | `path=''` で 1 行 upsert（冪等）。既に予約済みなら `alreadyScheduled=true`（`schedule_delete_series` の `already_scheduled` 出力に使用）                                                                                                                                                                                                                                                                                         |
| `DetachSeriesFromPath(ctx, key, series, path) (orphaned, error)`             | 指定 key+path の record 群から当該 series の `series_keys` 行**のみ**を削除（SYN-03 の即時切り離し。当該 series 指定の検索から直ちに消える）。record・chunks・embeddings は削除しない（§4.4 の例外）。orphan が生じた場合 `orphaned=true` を返し、呼び出し元はその場合のみ `MarkDocumentForDeletion` を呼ぶ（他 series が残る record はその series の下で生き続けるため予約不要）。`records` 行は不変のため doc_count 更新は不要 |
| `MarkDocumentForDeletion(ctx, key, series, path)`                            | 指定 path で 1 行 upsert（orphan になった record の物理削除予約）                                                                                                                                                                                                                                                                                                                                                                |
| `ListPaths(ctx, key, series)`                                                | 当該 key+series に登録済みの path 一覧を返す（desired-state との差分検出に使用。読み取りのみ）                                                                                                                                                                                                                                                                                                                                   |
| `ListPendingDeletions(ctx, key, series) (paths, seriesWide, error)`          | 当該 key+series の削除予約を 1 回で取得する（`paths` = path 単位予約の一覧、`seriesWide` = series 全体予約の有無）。`sync_documents` の冒頭で呼び、補償 + 予約解除の対象 path と SYN-04 の series 全体予約解除の要否を判定する（読み取りのみ）                                                                                                                                                                                   |
| `ClearPendingDeletion(ctx, key, series, path)`                               | 該当行を削除（SYN-04 の自己修復に使用）。`path=""` で series 全体の削除予約を解除する                                                                                                                                                                                                                                                                                                                                            |
| `DeleteOrphanRecords(ctx, key, path) (removed, error)`                       | 指定 key+path の record のうち **`series_keys` が 0 件のもののみ**を物理削除する（chunks / embeddings / BM25 整合含む。既存 `deleteRecordWithBM25Tx` を再利用）。series 紐付きが残る record には一切触れないため冪等かつ常に安全。record 削除を伴うため doc_count を更新する。`WithKeyLock` は内部で取得しない（呼び出し元が `fn` 内で保持する。§8.5）                                                                           |
| `ListPendingDeletionsOlderThan(ctx, cutoff) ([]PendingDeletionEntry, error)` | `marked_at < cutoff` の削除予約一覧を返す読み取り専用メソッド（ロック不要）。起動時スイープと `internal/trash.Worker` の定期実行が共通で使う（§8.5）                                                                                                                                                                                                                                                                             |
| `SweepOnePendingDeletion(ctx, entry) error`                                  | 1 件（1 KEY 分）の削除予約を物理削除し予約を解除する。`path=''` なら `DeleteSeriesAll`、path 単位なら `DeleteOrphanRecords` を呼ぶ。呼び出し元がエントリごとに `WithKeyLock(entry.Key, ...)` で囲んで呼ぶ（旧 `SweepPendingDeletions` を分割した API。§8.5）                                                                                                                                                                     |

path 単位のスイープに `DeleteSeries` は**使用しない**。path 単位の予約行は「この path の orphan を回収せよ」という指示であり、`DeleteOrphanRecords` は series 紐付きが残る record に触れないため、**stale な予約行が残っていても live record を壊さない**（予約後に record が復活・再紐付けされていた場合は 0 件処理の冪等動作で行だけが消える）。`DeleteSeries` を使うと復活済み record から series を剥がして削除し得る。

**Mutex 直列化**: `MarkSeriesForDeletion` / `MarkDocumentForDeletion` / `ClearPendingDeletion` / `DetachSeriesFromPath` / `DeleteOrphanRecords` は単一トランザクションの書き込み操作であり、既存の削除系メソッドと同様に各メソッド自身が `s.mu` を取得して直列化する（§4.2。`WithKeyLock` とは別レイヤーであり、呼び出し側取得ルールの対象外）。

**stale 予約の発生源遮断 [MANDATORY]**: `DeleteKey` は当該 KEY の全 `pending_deletions` 行を、`DeleteSeriesAll` は当該 key+series の **series 全体予約（`path=''` センチネル）のみ**を、**それぞれ本体の削除と同一トランザクションで除去する**。これを行わないと、`schedule_delete_series(K, s)` → KEY 全体の自動最終処分による `DeleteKey(K)`（または `delete_series(K, s)`）→ 同名 KEY/series の再作成 → 次回スイープ、の順序で stale な series 全体予約が残存し、起動時スイープ・定期実行の `DeleteSeriesAll` が**再作成された新データを削除する**。除去範囲が両メソッドで**非対称**なのは意図的な設計である:

- `DeleteKey` は orphan record（`series_keys` 0 件）を含む当該 KEY の全 record を物理削除するため、当該 KEY の予約はすべて（path 単位含め）無意味になる → 全行除去してよい
- `DeleteSeriesAll` は `series_keys` を JOIN して対象を列挙するため **orphan record には一切触れない**。切り離し済み orphan の唯一の回収手段は path 単位予約（起動時スイープの `DeleteOrphanRecords`）であり、ここで path 単位予約まで消すと**回収手段のない orphan が永久残留する**。path 単位予約を残しても `DeleteOrphanRecords` は orphan-only のため、同名 series 再作成後の live record を壊さない（無害） → **path 単位予約は残す**

**予約解除（`ClearPendingDeletion`）の実行条件 [MANDATORY]**: `sync_documents` は、`documents` に含まれる path のうち **1 件処理が成功（processed または skipped）し、かつ削除予約が存在する path** について、以下の 2 段階で予約を解除する:

1. **`DeleteOrphanRecords` を先に実行**する。upsert 経路の `CleanOtherSeries` は個別失敗しても警告扱いで処理継続する（record 単位では processed 扱い）ため、「processed = 旧 orphan は掃除済み」とは限らない。この補償呼び出しにより、`CleanOtherSeries` の成否に関係なく旧 orphan が決定的に回収される（掃除済みなら 0 件処理の冪等動作）
2. その後 `ClearPendingDeletion` で予約行を削除する。**手順 1 がエラーを返した場合は手順 2 を実行せず**、ログに記録して予約を保持する（次回 sync、または起動時スイープ・`internal/trash.Worker` の定期実行が再試行する。スイープは orphan-only のため予約残置は無害）

処理が失敗（failed）した path の予約は保持し、上記 2 段階も実行しない。失敗 path は新 record が作られておらず、予約まで解除すると旧 orphan の回収手段が失われる。なお仮に実装が予約有無の判定を省き、成功 path 全件に `DeleteOrphanRecords` を呼んでも事故にはならない（DIF-02/03 の既存掃除の冪等な再実行にすぎない）。`ListPendingDeletions` で対象を絞るのは正しさのためではなく、無駄な呼び出しの削減と実装意図の明確化のためである。

**orphan record の既知の制約**: 起動時スイープまたは `internal/trash.Worker` の定期実行まで一時的に残る orphan record について:

- **series 指定の検索には現れない**（`GetChunksForSearch` の series 指定経路は `series_keys` を JOIN するため）。sync 完了直後から削除済み path は当該 series の検索結果から消える（SYN-03）
- **series 未指定の KEY 全体検索には物理削除まで現れ得る**（`series == ""` 経路は `series_keys` を JOIN しない）。KEY 全体検索は全 series 横断の広域検索であり、over-recall 思想（PHIL-01）の範囲内として許容する
- doc_count（`COUNT(DISTINCT path) FROM records`）にも物理削除まで数えられる（起動時スイープは統計算出より前に走るため、起動時統計には影響しない。FNC-006 GC-03）
- 削除予約中の path を `sync_documents` を経由せず `upsert_documents` で復活させた場合、SYN-04 の予約解除は行われず予約行が残る。ただしスイープは orphan-only のため**復活済みの record が壊れることはなく**、stale な予約行は起動時に 0 件処理で除去されるだけで無害。予約解除を即時に行いたい場合は復活も `sync_documents` で行うこと
- **`list_indexes` の series 一覧からは消える**（`fetchSeriesForKey` は `series_keys JOIN records` の `DISTINCT` であり、orphan record は `series_keys` を持たない）。desired-state が空だった series は当該 KEY の series 一覧に現れなくなるため、**`list_indexes` では「その series で一度も同期していない」と「同期済みだが desired-state が空だった」を区別できない**（FNC-004 MNG-01）。クライアントがこの一覧で未同期を判定する場合は、送信対象のドキュメント数が 0 かどうかを併せて確認して切り分ける必要がある（後者に対して同期の再実行を促しても状態は変わらない）。`list_indexes` はこの区別を公開しない。内部的には `pending_deletions` の予約行（`key` / `series` / `path`。`MarkDocumentForDeletion`）が起動時スイープまたは `internal/trash.Worker` の定期実行まで痕跡として残るため、その間はサーバー内部では「同期済みだった」ことを判別できるが、スイープ後は内部でも区別できなくなる。この一時的な内部状態を API へ公開したり、恒久的に区別可能な状態（例: series 単位の同期履歴）を新設することはしない — 後者でも当該 series の検索結果は必然的に 0 件であり、区別が必要なのは「クライアントが利用者へ出す案内文」に限られるため、その判断はクライアント側の情報（対象ファイル数）で足りる。**`query` も同様に series の登録状態を検証しない**: `handleQuery` が存在検証するのは KEY のみ（不在なら ERR-01 識別子付きエラー、ゴミ箱状態なら明示エラー）であり、series は検索パイプラインへの絞り込み条件としてそのまま渡される。したがって未登録 series を指定した `query` はエラーではなく該当 0 件の成功応答を返す（FNC-003 の安定契約。Issue #8）
- **cross-series の自己修復猶予は保証されない**: SYN-04 の「API 課金ゼロ自己修復」は、当該 orphan が回収される前に同一 series の再 sync が行われた場合の保証である。`DeleteOrphanRecords` は series を見ずに同一 key+path の orphan を全回収し、既存の `CleanOtherSeries` / `AppendAndCleanSeries` も同一 key+path の空 record を掃除するため、別 series が同じ key+path を upsert / sync すると他 series の自己修復用に残していた orphan も回収される。その後の再 sync は通常の新規登録として Embedding を再計算する（動作は常に正しく、失われるのは課金ゼロの猶予のみ）。既存掃除機構と整合した挙動である

**予約を path 粒度とし record_id / content_hash を持たせない理由（設計判断）**:

1. **別内容での復活時**: DIF-03 経路は `UpsertRecord` 直後に必ず `CleanOtherSeries` を呼び、series 条件なしで同一 key+path の空 record を物理削除する。旧 orphan は原則、新 record 作成と同一の処理内で掃除される（同一内容での復活 = DIF-02 経路は record 再利用なので orphan 自体が生じない）。`CleanOtherSeries` の個別失敗は上記 [MANDATORY] の補償（予約解除直前の `DeleteOrphanRecords`）が拾う
2. **同一 key+path に複数 orphan（複数 content_hash）が並ぶ場合**: `DeleteOrphanRecords(key, path)` は path 配下の全 record を走査して orphan を全回収するため、予約行が個々の record を識別する必要がない
3. **record_id を持たせた場合の逆リスク**: DIF-02 の自己修復は既存 record への series 再紐付け（record 再利用）であるため、record 単位の予約では「再紐付けされて生き返った record を旧予約が指したままスイープで誤削除する」事故を防ぐ整合管理が別途必要になる。path 粒度 + 「`series_keys` 0 件のみ物理削除」の組み合わせは、この整合管理なしで安全側に倒れる

### 4.6 KEY 単位ゴミ箱状態（keys.trashed_at）と TOCTOU 対策

FNC-007（ADR-003）により、TTL/LRU による自動判定廃棄（旧 EXP-01/02）を廃止し、削除は必ずユーザー主導のゴミ箱投入（`trash_index`）を経由するモデルへ置き換えた。KEY のゴミ箱状態は `keys.trashed_at`（§4.1、NULL 許容）で表現し、`internal/store.IsTrashed(ctx, key) (bool, error)` が判定を提供する。record 単位（orphan）のゴミ箱管理は既存の `pending_deletions`（§4.5）をそのまま流用し、KEY 単位とは異なるライフサイクル（前者はシステム起点、後者はユーザー操作起点）を分けて表現する。

**書き込み系 5 ツール + query の拒否判定**: `upsert_documents` / `delete_documents` / `delete_series` / `schedule_delete_series` / `sync_documents` の 5 ツールは、対象 KEY がゴミ箱状態の場合、処理を一切実行せず復活操作（`restore_index`）を促すエラーを返す（`trashed_at` も変更しない。黙って復活させない）。`query` も同様にゴミ箱状態の KEY を指定された場合、空結果ではなく明示エラーを返す（空結果では「データが無い」のか「ゴミ箱に入っている」のかをユーザーが区別できないため）。

**TOCTOU 対策 [MANDATORY]**: `IsTrashed` の判定は `WithKeyLock`（§4.3）取得**前**の事前チェックだけでは不十分である。事前チェックと実際の書き込み処理の間に `trash_index` が割り込むと、ゴミ箱投入後に書き込みが実行されてしまう（time-of-check-to-time-of-use）。これを防ぐため、5 ツール全ての書き込みハンドラおよび `sync_documents` のバックグラウンドジョブは、`IsTrashed` を **`WithKeyLock` の `fn` 内部で再確認**する（事前チェックは早期リターンによる無駄なロック取得回避の最適化として残すが、権威ある判定は `fn` 内部の再確認とする）。`WithKeyLock` の相互排他性により、`fn` 内部での再確認が完了するまで `trash_index` は同じ KEY のロックを取得できないため、再確認後に割り込みが発生する余地はない。

## 5. ユースケース設計

### 5.1 ユースケース一覧

| ユースケース                                              | 対応 MCP ツール           | 関連要件              |
| --------------------------------------------------------- | ------------------------- | --------------------- |
| ドキュメント追加・更新                                    | `upsert_documents`        | FNC-001               |
| ドキュメント削除                                          | `delete_documents`        | FNC-002               |
| series 一括削除（branch cleanup）                         | `delete_series`           | FNC-002 DEL-03        |
| ドキュメント検索                                          | `query`                   | FNC-003               |
| インデックス一覧取得（chunk_count 含む、ゴミ箱 KEY 除外） | `list_indexes`（TBD-008） | FNC-004 MNG-01        |
| KEY のゴミ箱投入                                          | `trash_index`             | FNC-007 TRS-01        |
| ゴミ箱一覧取得                                            | `list_trashed_indexes`    | FNC-007 TRS-04        |
| KEY の復活                                                | `restore_index`           | FNC-007 TRS-05        |
| desired-state 同期（削除ファイル追従）                    | `sync_documents`          | FNC-006 SYN-01〜05/08 |
| 同期ジョブの進捗取得                                      | `get_sync_status`         | FNC-006 SYN-06        |
| series 削除予約（branch 削除検知時）                      | `schedule_delete_series`  | FNC-006 GC-01         |

### 5.2 upsert_documents シーケンス

**content 取得の 3 経路 (exactly-one 排他)**:

| フィールド   | 取得元                  | 用途                                                          |
| ------------ | ----------------------- | ------------------------------------------------------------- |
| `content`    | クライアント payload    | 任意のテキストを直接投入 (旧来の使い方)                       |
| `url`        | Fetcher が HTTP GET     | リモート文書の取り込み                                        |
| `local_path` | doc-db が `os.ReadFile` | **ローカル運用推奨**。大容量文書を MCP payload に載せずに済む |

`local_path` の安全性: 絶対パスのみ、`..` 要素禁止、シンボリックリンク解決後のパスも再検証、
サイズ上限 10 MB、regular file 限定。`path` フィールド (search 表示用の識別子) と分離
されており、任意の相対パスを付けられる (例: `path="docs/api.md"` / `local_path="/abs/.../api.md"`)。

```mermaid
sequenceDiagram
    participant C as MCP クライアント
    participant H as UpsertHandler
    participant F as Fetcher
    participant Ch as Chunker
    participant E as Embedder
    participant S as Store

    C->>H: upsert_documents(key, series, documents)
    loop 各 document
        alt url 指定
            H->>F: Fetch(ctx, url)
            F-->>H: content (失敗時: スキップ・エラー記録)
        end
        H->>H: normalize(content) → SHA-256 → hash
        H->>S: FindRecord(key, path, hash)
        alt 同一ハッシュあり (DIF-02)
            H->>S: AppendSeries(record_id, series)
            H->>S: CleanOtherSeries(key, path, series, except=record_id)
        else 新規 or 内容変更 (DIF-03)
            H->>Ch: Split(path, content)
            Ch-->>H: ([]Chunk, error)
            H->>E: Embed(ctx, chunk_texts)
            E-->>H: []Vector (失敗時: スキップ・エラー記録)
            H->>S: UpsertRecord(key, path, hash, series, chunks, vectors)
            H->>S: CleanOtherSeries(key, path, series, except=new_record_id)
        end
    end
    H-->>C: UpsertResult{processed, skipped, failed, errors}
```

**ハッシュ算出の正規化規則（M1）**:

コンテンツの SHA-256 は以下の正規化を行った後の `[]byte` に対して算出する:

1. **BOM 除去**: UTF-8 BOM（`0xEF 0xBB 0xBF`）が先頭にある場合は除去する
2. **改行コード統一**: `\r\n` および単独 `\r` を `\n` に変換する
3. **エンコーディング**: UTF-8 として扱う（他エンコーディングは変換せず `Content-Type` charset ヘッダを参照。不明な場合は UTF-8 と仮定する）

クライアントが `hash` フィールドを省略せず送付する場合（`content` 指定時）、サーバーは同じ正規化を行った上で hash を算出し、クライアント提供値と照合する。不一致の場合はサーバー算出値を正として扱う（クライアントの正規化漏れを吸収する）。

**部分 Embed 失敗時の一貫性方針（M2）**:

チャンクの一部が Embedding API 呼び出し失敗でスキップされた場合、**成功チャンクのみを保存する（部分 record 保存）**。理由: all-or-nothing では一時的な API 障害で全ドキュメントが登録失敗になり、リトライまで検索不能になる。歯抜け record が検索品質に与える影響は許容範囲内（失敗チャンクは次回 upsert で再登録できる）。失敗したチャンクのインデックス番号はエラー情報（UPS-OUT-01）に含めて返す。

### 5.3 query シーケンス

```mermaid
sequenceDiagram
    participant C as MCP クライアント
    participant H as QueryHandler
    participant SP as SearchPipeline
    participant S as Store
    participant E as Embedder
    participant R as Reranker

    C->>H: query(key, series?, query, mode, top_n)
    H->>S: TouchKey(key)
    H->>SP: Run(ctx, key, series, query, mode, top_n)
    SP->>E: Embed(ctx, [query])
    E-->>SP: queryVector
    SP->>S: GetChunks(key, series)
    S-->>SP: []Chunk + []Vector
    SP->>SP: CosineSimilarity(queryVector, chunkVectors) → embScores
    alt mode == lex or hybrid or rerank
        SP->>SP: BM25Score(query, chunks) → lexScores
    end
    SP->>SP: FuseScores(embScores, lexScores, mode) → topK
    alt mode == rerank
        SP->>R: Rerank(ctx, query, topK)
        R-->>SP: rerankedResults (失敗時: topK をそのまま返す)
    end
    SP-->>H: []SearchResult
    H-->>C: results{path, heading_path, text, score, score_breakdown, series_keys, stage_stats}
```

### 5.4 sync_documents シーケンス（FNC-006 SYN-01〜08）

`upsert_documents` は追加専用の操作であり、クライアント側で削除されたファイルをインデックスから除去できない。`sync_documents` はクライアントが送る当該 key・series の**完全なファイル一覧**（desired-state）を正とし、各要素は既存の DIF-01〜03（`upsertOne`、無改造）で処理、一覧に含まれない既存 path は series から即時に切り離す。大量ファイルでも応答を遅延させないため、ジョブ投入（job_id 即時返却）+ `get_sync_status` ポーリングの 2 ツール構成とする（特別な非同期基盤は使わず、通常の同期 MCP ツールの組み合わせで実現する）。

```mermaid
sequenceDiagram
    participant C as MCP クライアント
    participant H as SyncHandler
    participant J as SyncJob 状態
    participant S as Store

    C->>H: sync_documents(key, series, documents)
    H->>J: job_id 発行・running 登録
    H-->>C: job_id (即時応答)
    Note over H,S: 以降は root context 由来の goroutine で継続 (GC-05)
    H->>S: WithKeyLock(key, fn) 開始 (SYN-08。fn 内でネスト取得しない)
    H->>S: ListPendingDeletions(key, series)
    H->>S: 各 document を既存 DIF-01〜03 (upsertOne) で処理
    H->>S: ListPaths で既存 path 一覧取得、documents に無い path を検出
    H->>S: DetachSeriesFromPath (series から即時切り離し)
    H->>S: orphaned=true の path のみ MarkDocumentForDeletion
    H->>S: 成功した予約中 path に DeleteOrphanRecords → ClearPendingDeletion
    H->>S: series 全体の削除予約があれば解除 (SYN-04)
    Note over H,S: fn 完了、WithKeyLock 解放
    H->>J: done・deleted_paths_marked 更新

    loop ポーリング
        C->>H: get_sync_status(job_id)
        H-->>C: status, processed, skipped, failed, deleted_paths_marked, errors
    end
```

**前提条件**: `key`・`series` は既存 KEY に対する呼び出しで、`documents` は当該 key・series の完全な現在状態であること（クライアントの責務）。要素形式は `upsert_documents` と同一（content / url / local_path 排他、§5.2）。**空リストも正当な desired-state として受理する**（「この series に現存ファイルがない」の宣言。既存 path は全て切り離し + orphan 予約となり、誤送信でも同一内容の再 sync で Embedding 再計算なしに復元できる。即時物理削除する `delete_series` への誘導は自己修復性を失うため行わない）。削除予約中の path の復活は `sync_documents` で行うこと（§4.5 既知の制約）。

**正常フロー**: 上図の通り。変更の無いファイルは DIF-02 によりそのまま skip される。desired-state から欠落した path は series から即時に切り離され、**当該 series を指定した検索には sync 完了直後から現れない**（SYN-03）。orphan になった record（chunks / embeddings 含む）は次回起動のスイープ、または保持期間経過後の `internal/trash.Worker` 定期実行のいずれか（§8.5）まで物理的には残り、series 未指定の KEY 全体検索には現れ得る（§4.5 既知の制約）。予約解除は成功 path のみを対象に `DeleteOrphanRecords` → `ClearPendingDeletion` の 2 段階で行う（§4.5 [MANDATORY]）。同一 KEY を対象とする他の書き込み・削除操作（series を問わず。`trash_index` によるゴミ箱投入・自動最終処分による KEY 削除を含む）は、処理完了までブロックされる（§4.3 SYN-08）。

**エラーフロー**: 個別ドキュメントの Embedding 失敗は既存 `upsertOne` の挙動を継承して処理継続（失敗 path の予約は解除しない）。存在しない・保持期限切れの `job_id` で `get_sync_status` を呼んだ場合はエラーを返す（SYN-06）。サーバーシャットダウンでジョブが中断された場合は `status="failed"` になる（GC-05）。

**ジョブ状態管理（SYN-06/07）**:

```go
type SyncJobStatus struct {
    Status              string // "running" | "done" | "failed"
    Processed, Skipped, Failed int
    DeletedPathsMarked  int
    Errors              []string
}
```

`internal/trash.Worker` の `Stats` + `sync.Mutex` パターン（旧 `internal/expiry.Worker` から継承）を踏襲し、`Handlers` 構造体に `sync.Mutex` で保護された `map[string]*SyncJobStatus` を持たせる。**メモリ保持のみ、永続化しない**（SYN-07。サーバー再起動でジョブ状態が失われても、クライアントが再度 `sync_documents` を呼べば冪等に補われる）。完了済みジョブの保持上限は 100 件（`maxCompletedSyncJobs`）とし、超過分は古い順に破棄する。

**ジョブ用 context の寿命（GC-05）**: バックグラウンド処理に MCP リクエストの context をそのまま渡すと、job_id 返却でリクエストが完了した時点でキャンセルされ、ジョブが途中停止する。これを避けるため:

- サーバー起動時（`cmd/docdb`）に生成する、シャットダウンシグナルで cancel される長寿命の root context（`trash.Worker.Start` に渡すものと同じ）を `Handlers` に保持させる
- バックグラウンドゴルーチンは root context から派生させた context で実行する。クライアントが切断してもジョブは継続する
- root context の cancel（シャットダウン）時は処理を中断し、`Status` を `"failed"` に更新してから終了する
- `WithKeyLock` のロック待機中に cancel された場合も、channel ベース実装（§4.3）により `fn` を実行せず即座に戻れるため、ロック取得前・取得後のいずれの段階でもシャットダウンに応答できる

### 5.5 KEY のゴミ箱投入・復活シーケンス（FNC-007 TRS-01/05）

```mermaid
sequenceDiagram
    actor User
    participant SKILL as manage-db-indexes
    participant MCP as internal/mcp
    participant DB as internal/store

    User->>SKILL: 管理 SKILL を実行
    SKILL->>MCP: list_indexes
    MCP->>DB: ListKeys（chunk_count 含む、ゴミ箱 KEY 除外）
    DB-->>MCP: KeyInfo[]
    MCP-->>SKILL: KEY メタデータ一覧
    SKILL-->>User: 一覧を提示

    User->>SKILL: 削除したい KEY を選択
    alt series が1件以上ある
        SKILL-->>User: 強制確認（本当に削除するか）
        User->>SKILL: 確認 OK
    else series が空
        Note over SKILL: 簡易ゴミ捨て（確認省略）
    end

    SKILL->>MCP: trash_index(key)
    MCP->>DB: WithKeyLock(key, fn)
    MCP->>DB: fn 内: IsTrashed(key) 再確認 → false
    MCP->>DB: fn 内: TrashKey(key) — trashed_at = now()
    DB-->>MCP: trashedAt
    MCP-->>SKILL: 成功（trashed_at）
    SKILL-->>User: ゴミ箱へ投入完了を報告
```

**前提条件**: 対象 KEY が存在し、まだゴミ箱に入っていない（`trashed_at IS NULL`）こと。
**正常フロー**: 上図の通り。`restore_index` は同様に `WithKeyLock` + `RestoreKey` で `trashed_at` を `NULL` に戻す。
**エラーフロー**: 対象 KEY が既にゴミ箱に入っている場合は `trash_index` がエラーを返す（多重投入防止）。存在しない KEY を指定した場合、またはゴミ箱に入っていない KEY に `restore_index` を呼んだ場合もエラーを返す。

### 5.6 自動最終処分シーケンス（FNC-007 TRS-06/07）

```mermaid
sequenceDiagram
    participant Worker as internal/trash.Worker
    participant DB as internal/store

    loop 定期実行（trash.interval_seconds ごと）
        Worker->>DB: ListTrashedKeys()
        DB-->>Worker: TrashedKeyInfo[]
        loop trashed_at が trash.retention_days 超過の KEY ごと
            Worker->>DB: WithKeyLock(key, fn)
            Worker->>DB: fn 内: IsTrashed(key) 再確認
            alt 再確認で false（復活済み）
                Worker->>Worker: skip（削除しない）
            else 再確認で true
                Worker->>DB: fn 内: DeleteKey(key)
                Worker->>Worker: slog.Info("trash: KEY を最終処分", key, trashed_at, deleted_at)
            end
        end

        Worker->>DB: ListPendingDeletionsOlderThan(cutoff)
        DB-->>Worker: []PendingDeletionEntry
        loop marked_at が保持期間超過の予約 1 件ごと
            Worker->>DB: WithKeyLock(entry.Key, fn)
            Worker->>DB: fn 内: SweepOnePendingDeletion(entry)
            DB-->>Worker: OK
            Worker->>Worker: slog.Info("trash: orphan record を最終処分", key, path, marked_at, deleted_at)
        end
    end
```

**前提条件**: なし（バックグラウンド定期実行）。
**正常フロー**: 上図の通り。`ListTrashedKeys` / `ListPendingDeletionsOlderThan` は読み取り専用で `WithKeyLock` を取得しない。保持期間超過の判定は `trashed_at` / `marked_at` と設定値 `trash.retention_days`（§9.2）から Worker が算出する（Store 層は事実のみを返す。§4.6）。
**エラーフロー**: 個別 KEY・record の削除失敗はログに記録し処理を継続する（silent failure 禁止。旧 `internal/expiry` の個別エラー継続パターンを踏襲）。`WithKeyLock` 内での `IsTrashed` 再確認は、ロック待機中に `restore_index` が先行して復活させた KEY を誤って削除しないための TOCTOU 対策（§4.6）。

**監査記録の永続化方針**: 自動最終処分の記録は `slog.Info` によるログ出力のみとし、DB への監査テーブルは設けない。単一ユーザー運用のログファイルで事後追跡が成立する規模であり、監査テーブル追加の運用・実装コストに見合わないと判断した。ログローテーション・削除によって記録が失われるリスクは残るが許容する。

### 5.7 ゴミ箱 KEY への操作拒否シーケンス（FNC-007 TRS-02/03）

```mermaid
sequenceDiagram
    actor Caller as 書き込み系ツールの呼び出し元
    participant MCP as internal/mcp
    participant DB as internal/store

    Caller->>MCP: upsert_documents / delete_documents / delete_series /\nschedule_delete_series / sync_documents
    MCP->>DB: IsTrashed(key)（事前チェック）
    alt 事前チェックで true
        MCP-->>Caller: エラー（ゴミ箱に入っています。restore_index で復活してください）
    else 事前チェックで false
        MCP->>DB: WithKeyLock(key, fn)
        MCP->>DB: fn 内: IsTrashed(key) 再確認（TOCTOU 対策）
        alt 再確認で true
            MCP-->>Caller: エラー（同上）
        else 再確認で false
            MCP->>DB: fn 内: 各ツールの既存処理を実行
            MCP-->>Caller: 成功
        end
    end
```

**前提条件**: なし。
**正常フロー**: `IsTrashed` が事前チェック・`WithKeyLock` 内再確認の両方で false の場合、各ツールの既存処理をそのまま実行する。
**エラーフロー**: 上図の通り。書き込み系 5 ツールはいずれも処理を一切実行せず `trashed_at` も変更しない（黙って復活させない。復活は `restore_index` のユーザー明示操作のみで行う）。

**設計判断**: 書き込み系の対象は種類を問わずすべて（`upsert_documents` / `delete_documents` / `delete_series` / `schedule_delete_series` / `sync_documents`）。`delete_series`・`schedule_delete_series` も series_keys が空になった record を即時物理削除し得るため、他 3 ツールと同じ理由で拒否対象に含める。既存の `UpsertRecord` の `ON CONFLICT(key) DO UPDATE SET` は `trashed_at` を更新対象に含めないため、対策なしにゴミ箱 KEY へ upsert すると、データは書き込まれるのに `trashed_at` が残ったまま＝`query` から検索できない状態になり得る。書き込み自体を拒否することで、この不整合を構造的に防ぐ。

**`query` は上記 `WithKeyLock` 再確認フローの対象外（設計判断・レビュー反映）**: `query` は `IsTrashed` の事前チェックのみを行い、`空結果ではなく明示エラーを返す`（空結果では「データが無い」のか「ゴミ箱に入っている」のかをユーザーが区別できないため）。ただし `query` は §4.2/§4.3 の方針どおり読み取り専用パスであり `WithKeyLock` を取得しない（KEY 単位ロックは書き込み系操作の排他のみに用いる設計）。そのため事前チェック通過後、`TouchKey`・実際の検索実行までの間に `trash_index` が完了すると、ゴミ箱投入された直後の KEY に対する検索結果が返り得る（TOCTOU の競合ウィンドウ）。これは書き込み系のように永続的なデータ不整合（`trashed_at` は立っているのに新規データが書き込まれる等）を生まない一時的な表示ラグに過ぎず、次回以降の `query` 呼び出しでは事前チェックが正しく拒否するため自己修復する。読み取り性能を優先し `query` を `WithKeyLock` で直列化しない設計上のトレードオフとして許容する。より厳密な線形化可視性が必要になった場合は、検索対象取得の SQL に `keys.trashed_at IS NULL` 条件を含める等、読み取りロックを追加せずに競合窓を縮小する対応を別途検討する。

## 6. 検索パイプライン詳細

### 6.1 ベクトル検索（emb）

- クエリテキストを Embedding API でベクトル化
- 対象 KEY（series 指定時はフィルタ）の全チャンクベクトルをメモリに展開
- コサイン類似度を `math` パッケージで計算（`f32` スライス）
- 上位 `top_n * rerank_factor` 件を候補として返す

**Embedding モデルと次元数の確定値（EMB-02）**:

| モデル（`embedding.model`）            | 次元数 | `embeddings.dim` |
| -------------------------------------- | ------ | ---------------- |
| `text-embedding-3-large`（デフォルト） | 3072   | 3072             |
| `text-embedding-3-small`               | 1536   | 1536             |

デフォルトモデル `text-embedding-3-large` を使用する場合、`embeddings.dim = 3072` で固定される。モデル変更時はデータベースを再構築する（異なる次元数のベクトルは混在不可）。

**モデル選択根拠**: `text-embedding-3-large` をデフォルトとして採用する。reference doc-db SKILL (Python 版) と同モデルにすることで日本語技術文書の検索精度を最大化する。コストは `-3-small` の約 6.5 倍だが、言い換え・抽象クエリでの recall 向上効果が大きい。コスト最適化が必要な場合は `text-embedding-3-small` (dim=1536) に切り替え可能。

**スケール上限**: システム全体のチャンク総数を強制する自動削除（旧 LRU、`expiry.max_chunks`）は FNC-007 により廃止済みで、上限を超えた場合に自動でデータを消す仕組みは持たない。key 単位では通常 1,000〜5,000 チャンク程度を想定する（1,000 チャンク × 1536 dim × 4 byte ≈ 6 MB）。10,000 チャンクでも 60 MB / クエリであり、内部ツール用途では許容範囲。100,000 チャンクを超える場合はベクトルキャッシュ（起動時 mmap またはプロセス内メモリキャッシュ）の導入を検討する。データ量の実態はユーザーが `list_indexes` の chunk_count で確認し、必要に応じて `trash_index` で削除する（§8.1）。

**設計判断**: ベクトルをすべてメモリに展開する方式を採用。`modernc.org/sqlite` は pure-Go のため `sqlite-vec` 等の C 拡張をロードできない。内部開発ツールであり大規模データを前提としないため（NFR-07）、in-process 計算で十分。

### 6.2 BM25 語彙検索（lex）

- 事前集計テーブル（`bm25_stats` / `bm25_df`）は持たない（v0.1.2 で廃止）。クエリ時に対象 key のチャンク本文へ substring match を都度実行して TF/DF を計算する（reference doc-db SKILL と同方式）
- パラメータ（k1, b）はサーバー設定ファイルで指定（デフォルト: k1=1.5, b=0.75。Okapi BM25 の経験則デフォルト値（Robertson et al.）。k1 はワード頻度のサチュレーション、b は文書長正規化を制御する）

**トークナイザ仕様（LEX-01）**:

Unicode 正規化 + 正規表現ベースのトークン分割を採用する（形態素解析器は使用しない）。

1. NFKC 正規化 + 小文字化（`norm.NFKC.String(text)` 後 `strings.ToLower()` を適用。`golang.org/x/text/unicode/norm` + 標準 `strings` パッケージ）
2. 以下のパターンで順に優先マッチ:
   - `[A-Za-z]+-\d+` → ID パターン全体をひとつのトークンとして扱う（例: `FNC-001`）
   - `[A-Za-z0-9_]+` → ASCII 英数字・アンダースコア
   - `[^\W\d_A-Za-z]+` → 連続する CJK 等非 ASCII Unicode 文字をひとつのトークンとして扱う（日本語は単語境界で区切れないため近傍文字をグルーピング）
   - `\d+` → 数字列
3. 空文字列トークンは除外する

**ID 完全一致ボーナス（LEX-01）**:

BM25 スコアに加え、以下のボーナスを加算する:

- **ID パターン一致**: クエリ中の `[A-Z]+-\d+` 形式の ID がチャンク本文に含まれる場合 +10.0（例: `FNC-001`）
- **クエリ全文一致**: 正規化済みクエリ全体がチャンク本文に含まれる場合 +2.0

**チャンク削除時の扱い**: `bm25_stats` / `bm25_df` は廃止済み（v0.1.2）であり、BM25 は query 時の都度計算のため、チャンク削除時に BM25 側で追加の整合性維持処理は不要（`chunks` の `ON DELETE CASCADE` のみで完結する）。

### 6.3 スコア融合（hybrid）

Reciprocal Rank Fusion（RRF）を採用:

```
score(d) = Σ 1 / (k + rank_i(d))   (k = 60。Cormack et al. 2009 原論文の推奨値)
```

embedding ランクと lexical ランクを統合。加重和より外れ値に頑健で、スケール正規化不要のため採用。

**EMB フォールバックと保証（SC-01）**:

- **EMB フォールバック** (`EMB_FALLBACK_LEX_RATIO = 0.05`): lexical ヒット数 / emb ヒット数 < 0.05 の場合（日本語クエリで BM25 がほぼヒットしない場合など）、RRF ではなく embedding スコア降順でフォールバックする。（経験則による暫定値。実運用データで検証し調整する）
- **EMB top-K 保証** (`EMB_GUARANTEE_K = 5`): クロスランゲージ同義語など lexical スコアが 0 の文書が RRF で押し出されることを防ぐため、embedding 上位 5 件は fused 上位 K 件に必ず含まれるよう昇格させる。（経験則による暫定値。実運用データで検証し調整する）

**ステージ候補数トラッキング（QRY-OUT-02）**:

各検索ステージを通過した候補数を `stage_stats` としてクエリ単位で記録し、`SearchResult` に付与する:

```
stage_stats: {
  emb_candidates: N,    // embedding 検索でヒットした候補数
  lex_candidates: N,    // lexical 検索でヒットした候補数
  grep_candidates: N,   // GREP signal でヒットした候補数（§6.4）
  fused_candidates: N,  // RRF 融合後の候補数（merge 前）
  merged_candidates: N, // 3 signal 合算後のユニーク候補数（mode=all）
  rerank_candidates: N  // LLM Rerank に渡した候補数（Rerank スキップ時は 0）
}
```

### 6.4 全文 GREP signal（GRP-01 / GRP-02 / PHIL-01）

#### 目的

Embedding は固有 ID・特殊用語・低頻度トークンを意味空間で散らかす。BM25 もトークナイザ
境界で割れる場合がある。GREP は **literal 一致 (substring) のみ** を見るため、これら
2 signal で取りこぼされる候補を確実に拾える。3 signal は互いに代替できない関係にある
（PHIL-01）。

#### アルゴリズム

```
1. 正規化済みクエリ q_norm = NFKC.lower(query) を計算（BM25 と同じ正規化）
2. 全 chunk の正規化済み body について q_norm の出現回数 cnt を数える
3. cnt > 0 のチャンクのみを結果に含める
4. grep_score = cnt（単純な出現回数。スコア融合では使わず、ヒット有無の signal として扱う）
```

クエリの分割は行わない（query 全体を 1 文字列として substring 検索）。
複数キーワード対応は将来要件（YAGNI: 現状 1 keyword で十分実用的）。

#### 出力

GREP signal も他 signal と同様に candidate pool に合流し、`origin_signals` に `"grep"` を
付与する。`score_breakdown` には `grep` フィールドを含める。

### 6.5 Candidate Merge（ALL-01 / QRY-OUT-03）

`mode=all`（デフォルト）では 3 signal の結果を以下の手順で合算する:

```
1. emb 上位 N_emb 件、lex 上位 N_lex 件、grep ヒット全件を集める（GREP は順位なし）
   N_emb / N_lex は top_n に基づく（実装で確定）
2. chunk_id 単位で重複を排除し、各 chunk に origin_signals を記録
   例: chunk X が emb と grep で見つかれば origin_signals = ["emb", "grep"]
3. 合算結果を以下の優先順でソート:
   a. signal hit 数の降順 (3 → 2 → 1)
   b. emb_score 降順（tie-break）
   c. chunk_id 昇順（決定性）
4. 上位 top_n 件を返す（上位 AI agent は更にこれを本文判定する想定）
```

Layer 1 の責務は「**取りこぼしの無い候補プール**」を提供することなので、ranking の
fine-tuning は最小限とする（PHIL-01）。

### 6.6 LLM Rerank（オプション）

- OpenAI Chat Completions API（`gpt-4o-mini` 等、設定で変更可）を使用
- `mode=rerank` のときのみ実行
- §6.5 の merge 結果上位を LLM に渡し、関連度順に並び替える
- 候補チャンクとクエリを JSON で渡し、`{"ranking":[{"id","score":0..1}]}` を返させる
- プロンプトテンプレートは設定ファイルで上書き可
- タイムアウト（デフォルト 30s）超過または API エラー時は merge 順でフォールバック（RR-02）
- silent failure 禁止: 範囲外 id を返した場合は skip + `dropped_ids` を warnings に記録（detection memory）
- PHIL-02: Rerank は ranking 最適化であり、recall を広げる手段ではない。`mode=all` でも
  merge 結果は同等の signal recall を持ち、Rerank の有無で候補プールは変わらない

## 7. 外部 API 連携

### 7.1 OpenAI Embedding API

- エンドポイント: `https://api.openai.com/v1/embeddings`
- モデル: 設定ファイルで指定（例: `text-embedding-3-large`）
- バッチ上限: 1 リクエストあたり最大 100 テキスト（OpenAI 制限は 2048 だが、ペイロードサイズと遅延を抑えるため 100 を上限とする）
- リトライ: 指数バックオフ（初回 1s、最大 3 回）
- タイムアウト: 60s（設定可）
- API キー: 環境変数 `OPENAI_API_DOCDB_KEY` → フォールバック `OPENAI_API_KEY`（PRE-01）

### 7.2 URL コンテンツ取得（Fetcher）

- `net/http.Client` にタイムアウト 30s を設定
- リダイレクト最大 5 回
- `Content-Type` が `text/` 系以外はエラーとしてスキップ
- 取得後に SHA-256 を算出し、hash フィールドとして扱う（クライアント提供 hash は `content` 指定時のみ）

**SSRF 対策**: 本サーバーは内部ネットワークへの配備を前提とするため、プライベート IP へのリクエストをブロックする。

- DNS 解決後の IP アドレスが以下に該当する場合はエラーとしてスキップ:
  - `127.0.0.0/8`（ループバック）
  - `10.0.0.0/8`、`172.16.0.0/12`、`192.168.0.0/16`（RFC1918 プライベート）
  - `169.254.0.0/16`（リンクローカル / AWS IMDS 等）
  - `::1`、`fc00::/7`（IPv6 ループバック / ユニークローカル）
- ホワイトリストが必要な場合は設定ファイルの `fetcher.allow_private: true` で無効化できる（デプロイ管理者が責任を持つ）。

## 8. ゴミ箱管理と自動最終処分（internal/trash.Worker）

`internal/trash.Worker` はバックグラウンドゴルーチンとして起動し、定期的（`trash.interval_seconds`。デフォルト 3600 秒）に実行する。旧 `internal/expiry.Worker`（TTL/LRU による自動判定廃棄）を FNC-007（ADR-003）で廃止し、削除は必ずユーザー主導のゴミ箱投入（`trash_index`）を経由するモデルへ置き換えた。KEY のゴミ箱状態は `trashed_at`（§4.1）で表現し、削除の実行（`DeleteKey`）は KEY 単位排他の対象であり、対象 KEY ごとに `WithKeyLock` で囲んで呼び出す（FNC-006 SYN-08、§4.3。`sync_documents` 処理中の KEY を自動最終処分が同時に消す競合を防ぐ）。

**シャットダウン時の Store クローズ順序（レビュー反映）**: `Worker.Start(ctx)` は `ctx` キャンセルを検知して終了するが、`runOnce` 実行中は次の `select` 評価まで終了を待たされる。呼び出し元（`cmd/docdb`）が `Worker` の終了を待たずに `Store.Close()` を呼ぶと、実行中の DB 操作とクローズが競合しうる。これを防ぐため `Worker` は `Done() <-chan struct{}`（`Start` の goroutine 終了時に close される）を公開し、`cmd/docdb` は `defer` の登録順序を利用して `Store.Close()` より先に `<-trashWorker.Done()` を待つ。

**設計判断（旧 TTL/LRU からの転換理由）**: `last_accessed_at`（TTL）や `total_chunks`（LRU）という推測的な指標に基づく自動判定廃棄は、実運用で投入直後の唯一の KEY を無警告のまま削除する事故を起こした。doc-db は「削除すべきかどうか」の判定を一切行わない設計へ転換し、KEY ごとの正確なメタデータ（chunk 数・doc 数・最終アクセス日時等）をユーザーの近辺まで届けることに徹し、削除の要否判断と実行はその情報を見た人間・AI エージェントに委ねる（詳細は ADR-003 参照）。

### 8.1 KEY のゴミ箱投入・復活（FNC-007 TRS-01/04/05）

- **`trash_index`**: 対象 KEY を Active からゴミ箱状態へ遷移させる（`trashed_at = now()`）。既にゴミ箱状態の KEY への多重投入はエラーとする。シーケンスは §5.5 参照
- **`list_trashed_indexes`**: ゴミ箱状態の KEY 一覧を `trashed_at` 昇順（古い順）で返す。自動最終処分までの残り時間は `trashed_at` と `trash.retention_days` から算出する（§4.6）
- **`restore_index`**: ゴミ箱状態の KEY を Active に戻す（`trashed_at = NULL`）。ゴミ箱に入っていない KEY への呼び出しはエラーとする
- `list_indexes` はゴミ箱状態の KEY を結果から除外する（Active な KEY のみを提示する。FNC-004 MNG-01）

### 8.2 自動最終処分（FNC-007 TRS-06/07）

```
SELECT key, trashed_at FROM keys WHERE trashed_at IS NOT NULL ORDER BY trashed_at
```

上記で取得した KEY のうち `trashed_at` が `trash.retention_days`（デフォルト 3 日）を超過したものを、KEY ごとに `WithKeyLock` + `IsTrashed` 再確認（§4.6 TOCTOU 対策）+ `DeleteKey` で物理削除する。対象 KEY のすべての records・chunks・embeddings・series_keys を削除後、`keys` レコードも削除する。シーケンスは §5.6 参照。

**監査記録**: 自動最終処分の記録は `slog.Info` によるログ出力のみとし、DB への監査テーブルは設けない（§5.6 設計判断）。

### 8.3 series 廃棄ポリシー（解決済み — 削除予約 + 起動時スイープ）

feature ブランチ運用ではブランチ削除後も series が残存し続ける問題があり、当初は廃棄ポリシー未確定（旧 TBD、APP-001 TBD-009）だった。FNC-006 で以下の方式により解決した:

- **`schedule_delete_series`（GC-01）**: クライアントが branch 削除等の事実を検知した時点で series 全体の削除予約を記録する（§4.5）。**即時削除はしない**
- **起動時スイープ + 定期実行（GC-02、§8.5）**: 予約された series を、サーバー起動時と `internal/trash.Worker` の定期実行の双方で物理削除する
- **自己修復（SYN-04）**: 予約後に同一 key・series へ `sync_documents` が呼ばれた場合は予約を解除する。予約は処理まで完全に無害であり、誤操作しても取り消せる

**TTL/LRU の単位を series に拡張する案は不採用**（FNC-007 により TTL/LRU 自体も廃止済み）。`last_accessed_at` という推測的指標に基づく自動廃棄を series へ広げるより、クライアントが確実に把握している事実（「この series はもう使われない」）だけを根拠にする方が、必要なデータが推測で消える事故を構造的に避けられるためである。ファイル単位の削除追従は `sync_documents` の desired-state 同期（§5.4）が担う。

### 8.4 KEY ごとのポリシーオーバーライド（廃止）

旧 EXP-04（専用 MCP ツール `manage_index` による KEY ごとの廃棄ポリシー設定、TBD-007 旧確定）は、TTL/LRU 廃止（FNC-007）に伴い前提自体が消滅したため撤去した。KEY のライフサイクル管理は `trash_index` / `list_trashed_indexes` / `restore_index`（§8.1）に置き換わっている。

### 8.5 削除予約の最終処分（GC-02〜04）

`pending_deletions`（§4.5）に記録された削除予約は、以下の 2 メソッドに分割された API で処理する（旧 `SweepPendingDeletions` は KEY をまたいで無条件一括処理する 1 メソッドであり、§4.3 の「呼び出し元が対象 KEY への Store 呼び出し一式を 1 回の `WithKeyLock` で囲む」規約と両立しなかったため分割した）:

- `ListPendingDeletionsOlderThan(ctx, cutoff)`: `marked_at < cutoff` の予約一覧を返す読み取り専用メソッド（ロック不要）
- `SweepOnePendingDeletion(ctx, entry)`: 1 件（1 KEY 分）の予約を物理削除し、予約を解除する。呼び出し元がエントリごとに `WithKeyLock(entry.Key, ...)` で囲んで呼ぶ

**起動時スイープ**: `cmd/docdb` の起動シーケンスは `store.New()` 直後・**起動時 DB 統計表示（keyCount / totalChunkCount 算出）より前**に、上記 2 メソッドを使って同期的にスイープする（GC-03。統計が削除済みデータを含んだ値にならないようにする）。
**定期実行**: `internal/trash.Worker` が §8.2 の KEY 自動最終処分と同じ実行サイクルで、上記 2 メソッドを使って定期的にスイープする（§5.6）。

- `path=''` の行（series 全体予約）: `SweepOnePendingDeletion` 内部で `DeleteSeriesAll` を呼ぶ（他 series が参照する record は保持する既存の安全な不変条件込み）
- path 単位の行: `SweepOnePendingDeletion` 内部で `DeleteOrphanRecords` を呼ぶ（orphan-only。stale な予約行が残っていても live record を壊さない。§4.5）
- 成功した行は `pending_deletions` から削除する。個別失敗は警告ログ + エラー集約で記録し**処理を継続**する（GC-04、silent failure 禁止方針）。失敗行・消し忘れ行は次回のスイープで再試行されるだけで安全（両処理とも冪等）

起動時スイープは MCP リクエストを受け付ける前の時間帯にのみ実行されるため、エントリごとの `WithKeyLock` は理論上不要だが、`internal/trash.Worker` の定期実行と処理を共通化するため両者とも一貫して `WithKeyLock` で囲む。

**ゴミ箱状態の KEY への削除予約はスキップする [MANDATORY]（レビュー反映）**: `pending_deletions.marked_at` は当該 KEY の `trashed_at`（§8.1）と完全に独立して進行する。対策なしでは、`schedule_delete_series` で series 全体予約を作った**後に**当該 KEY を `trash_index` した場合、KEY 自体はまだ猶予期間内（`restore_index` で復活可能）であっても、予約作成済みの `marked_at` が保持期間を超過していれば `SweepOnePendingDeletion` が series を物理削除してしまい、その後 `restore_index` で KEY を復活させても series データは戻らない（ADR-003 の「猶予期間中いつでも復活できる」という保証に反する）。これを防ぐため、`WithKeyLock(entry.Key, ...)` の `fn` 内で `IsTrashed(entry.Key)` を確認し、true の場合は `SweepOnePendingDeletion` を呼ばずスキップする（今回のスイープでは処理せず、次回以降 KEY が Active に戻った時点、または KEY 自体が最終処分（`DeleteKey`）された時点のいずれかで解消させる。`DeleteKey` は当該 KEY の全 `pending_deletions` 行を同一トランザクションで除去するため、スキップした予約行が孤立して残り続けることはない。§4.5 [MANDATORY] 参照）。起動時スイープ・`internal/trash.Worker` の定期実行の両方に同一の対策を適用する。

## 9. 設定

### 9.1 設定方式

設定は **YAML 設定ファイル** で管理する（環境変数による設定値オーバーライドは行わない）。API キーのみシークレットとして環境変数で扱う。

| 種別         | 渡し方        | 対象                                                                |
| ------------ | ------------- | ------------------------------------------------------------------- |
| シークレット | 環境変数      | `OPENAI_API_DOCDB_KEY`（優先） / `OPENAI_API_KEY`（フォールバック） |
| 動作設定     | YAML ファイル | Embedding モデル・タイムアウト・ポート・パス等のすべて              |

**設定ファイルパス（CFG-01）**: `~/.doc-db/doc-db.yaml`（固定）。`$HOME` が解決できない、またはファイルが存在しない場合は **fail-fast** でサーバーを終了する。CLI フラグ・環境変数によるパス変更は提供しない（設計上の簡潔性を優先）。

**ロード方式（CFG-02）**: 起動時に 1 回だけ読み込み、`internal/config` パッケージが `Config` 構造体を返す。サーバー稼働中の設定再読み込みは行わない（変更時は再起動）。

**検証（CFG-03）**: パース後にバリデーションを行う。未知のキー、型不一致、必須項目欠落、値域外（ポート範囲・正の整数等）は fail-fast で起動を中止する。

> **CFG-03 の例外（v0.1.12+）**: `log` セクションは省略可とする。既存の `doc-db.yaml`
> に破壊的変更を強いないための後方互換措置。省略時は `path: ~/.doc-db/doc-db.log` /
> `level: info` がロード時に補完される。他の全セクションは引き続き必須項目欠落を
> fail-fast で拒否する（この例外は `log` セクションのみに限定する）。

### 9.2 設定ファイルスキーマ

```yaml
# ~/.doc-db/doc-db.yaml
server:
  port: 58080 # HTTP ポート（dynamic range から選定）
  db_path: "./docdb.sqlite" # SQLite ファイルパス

embedding:
  model: "text-embedding-3-large" # Embedding モデル（変更時は DB 再構築必須）
  dim: 3072 # ベクトル次元数（EMB-02 確定値）
  timeout_seconds: 60 # API タイムアウト

rerank:
  model: "gpt-4o-mini" # LLM Rerank モデル
  factor: 3 # top_n × factor 件を Rerank に渡す
  timeout_seconds: 30 # API タイムアウト

chunker:
  max_chunk_size: 1500 # チャンクあたり最大文字数（CHK-03 確定値）

bm25:
  k1: 1.5 # サチュレーション係数（Robertson 経験則）
  b: 0.75 # 文書長正規化係数

fetcher:
  timeout_seconds: 30 # URL フェッチタイムアウト
  allow_private: false # プライベート IP へのフェッチを許可（SSRF 対策無効化）

trash:
  retention_days: 3 # ゴミ箱投入から自動最終処分までの保持日数（FNC-007 TRS-06）
  interval_seconds: 3600 # internal/trash.Worker のチェック間隔

log:
  path: "~/.doc-db/doc-db.log" # ログ出力先（省略可。"stdout"/"stderr" も指定可）
  level: "info" # debug/info/warn/error（省略可）
```

### 9.3 設計判断

| 項目                         | 判断                                 | 理由                                                                                                                           |
| ---------------------------- | ------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------ |
| YAML 採用                    | TOML/JSON ではなく YAML              | コメント可・階層構造の可読性・運用者の編集容易性                                                                               |
| 環境変数オーバーライド不採用 | ファイルが唯一の正本                 | 設定の出所を一元化し、デバッグ時の挙動推定を容易にする                                                                         |
| パス固定                     | `~/.doc-db/doc-db.yaml` のみ         | CLI / env でのパス変更が増えると「実際にどの設定が使われているか」の判定コストが増す。サーバー用途では複数構成を持つ必要がない |
| API キーだけ環境変数         | シークレットは設定ファイルに書かない | 平文記録・git 誤コミットのリスク回避                                                                                           |

## 10. エラーハンドリング方針

外部要因エラーのみキャッチし、内部バグはサイレントフォールバックせず伝播させる（APP-001 エラーケース節）。

| レイヤー                       | 方針                                                                                                                                                                                                                                   |
| ------------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 起動時                         | `OPENAI_API_DOCDB_KEY` と `OPENAI_API_KEY` の両方が未設定の場合、サーバーを即座に終了する（PRE-01 fail-fast）。実際の API 疎通確認はしない（遅延検出コストが不要）。起動後に無効キーだと判明した場合は Embedder レイヤーでエラーを返す |
| Fetcher                        | タイムアウト・HTTP エラーをキャッチし、該当 document をスキップして error 情報を返す                                                                                                                                                   |
| Embedder                       | API エラーをキャッチし、複数バッチ失敗は `errors.Join` で全件保持し caller に返す。指数バックオフで最大 3 回リトライ                                                                                                                   |
| Reranker                       | エラー時は merge 順結果にフォールバック（RR-02）。フォールバック発生は `QueryResult.warnings` に記録し caller が観測可能にする（silent failure 禁止方針）                                                                              |
| Store                          | DB エラーは内部バグとして伝播させる（panic または error return で MCP エラーレスポンス）。tx.Rollback 失敗は `errors.Join` で caller に伝達                                                                                            |
| TrashWorker                    | 個別 KEY・record 削除失敗はログ + `Worker.Stats().LastKeyErrors` に記録し observability を担保。サーバー停止はしない（旧 ExpiryWorker から継承）                                                                                       |
| MCP 書き込み系ハンドラ / query | ゴミ箱状態の KEY への操作はエラーとして拒否する（黙って復活・スキップしない。FNC-007 TRS-02/03、§4.6）                                                                                                                                 |

**silent failure 禁止方針**: 全エラー経路で「ログのみ」で終わらせず caller / observable state
に必ず伝達する。詳細は memory `feedback_no_silent_failure.md` 参照。

### 10.1 エラー種別の機械判別（APP-001 ERR-01）

`internal/mcp/errcode.go` に識別子と組み立て関数を集約する。

**識別子（公開契約。値を変更しない）**:

| 識別子          | 意味                            | クライアントが取るべき行動                        | 生成箇所                                |
| --------------- | ------------------------------- | ------------------------------------------------- | --------------------------------------- |
| `KEY_NOT_FOUND` | 指定 KEY が存在しない           | 索引の作成（`sync_documents` 等）へ進んでよい     | `query` の KeyExists 判定（§5.3）       |
| `KEY_TRASHED`   | KEY がゴミ箱状態（TRS-02 / 03） | 索引を作成してはならず `restore_index` を案内する | `rejectIfTrashed`（§4.6。1 箇所に集約） |

**載せ方 — `*jsonrpc.Error` を返す**: go-sdk の `ToolHandlerFor` ラッパー（`mcp/server.go`）は
handler が返した error を次の 2 通りに分岐させる。

- `*jsonrpc.Error` → **そのまま JSON-RPC error として返す**（`code` / `message` / `data` が保持される）
- それ以外の error → `CallToolResult{IsError: true}` に包み、**`content[].text` の文言だけが残る**（handler が返した `res` は破棄される）

したがって後者では構造化情報を載せる余地がなく、クライアントは文言一致に退行する。ERR-01 の 2 種は前者で返す。

**識別子は Message 先頭と Data の両方に載せる**（経路によって `data` がクライアントへ届くとは限らないため）:

- `Data`（判別の正本）: `{"code": "<識別子>", "key": "<対象 KEY>"}`。JSON を直接パースするクライアント（SKILL の `docdb_client.py` 等）は `error.data.code` で厳密に分岐する
- `Message` 先頭: `<識別子>: <日本語の説明>`。MCP クライアント経由で `data` が AI agent へ提示されない場合のフォールバック
- `Code`（数値）: JSON-RPC 2.0 の予約域（-32768〜-32000）を避けたアプリケーション固有値。判別の正本は `Data.code` の文字列であり、数値は補助

**回帰テスト**: `internal/mcp/errcode_test.go` が「`*jsonrpc.Error` であること・`Message` 先頭の識別子・`Data.code` / `Data.key`」を handler 経路込みで検証する。`fmt.Errorf` へ戻す変更はこのテストが落ちる。

## 11. テスト設計

- **単体テスト対象**: `store`（SQL クエリ正確性）、`chunker`（Markdown 分割境界）、`search`（コサイン類似度・BM25・RRF の計算結果）、`embedder`（リトライロジック）
- **統合テスト対象**: `upsert_documents` の series_keys 共有フロー（同一ハッシュで Embedding スキップされること）、`query` の mode 別結果差異、ゴミ箱投入・自動最終処分の削除動作
- **モック方針**: `Embedder` と `Fetcher` はインターフェース経由でモック可能にする。SQLite は通常の単体テストではインメモリ（`file::memory:`）を使用する
- **WAL 並行テスト**: WAL モードはファイルベースでしか有効化されない。並行アクセスの統合テスト（複数ゴルーチンの同時読み書き）は `os.MkdirTemp` で作成したテンポラリディレクトリに実 SQLite ファイルを使用する。インメモリ DB では WAL の挙動を検証できないため代替不可
- **KEY 単位排他（§4.3）**: `WithKeyLock` の直接排他性（同一 KEY ブロック / 異 KEY 非ブロック / 待機中 ctx キャンセル / 参照カウントによるエントリ解放）と、MCP ハンドラ層での排他（`sync_documents` 処理中の同一 KEY への `upsert_documents` / `trash_index` / 自動最終処分相当呼び出しがブロックされること = SYN-08）を検証する
- **削除予約と orphan 回収（§4.5 / §8.5）**: 予約の冪等性、`DetachSeriesFromPath` の即時切り離し（series 指定検索から直ちに消え、record は物理残存）、stale 予約行の無害性（復活済み record をスイープが壊さないこと）、`DeleteKey` / `DeleteSeriesAll` の予約除去（非対称込み）、共有 content_hash record の保全、`DeleteOrphanRecords` の orphan-only 性を回帰テストとして常時検証する
- **sync_documents（§5.4）**: job_id 即時返却（SYN-05）、検索最新性（欠落 path が sync 完了直後の series 指定検索に現れないこと = SYN-03）、自己修復の API 課金ゼロ（Embedder spy で呼び出し 0 回 = SYN-04）、空 desired-state の受理、リクエスト context 非依存・root context キャンセルでの failed 遷移（GC-05）を検証する
- **DIF-02 不変条件**: 同一 `key + path + content_hash` で Embedding を再計算しないことを Store 層・ハンドラ層・Embedder spy の 3 テストで常時保証する（これらのテストを壊す変更は不変条件の破壊を意味する）
- **KEY ゴミ箱状態と TOCTOU 対策（§4.6 / §5.5〜§5.7、FNC-007）**: `TrashKey` / `RestoreKey` / `ListTrashedKeys` / `IsTrashed` の正常系・多重投入エラー・存在しない KEY のエラー、`trash_index` 実行後に `query` が当該 KEY へ明示エラーを返すこと（空結果ではないこと）、ゴミ箱状態の KEY に対する書き込み系 5 ツールが拒否され `trashed_at` が変化しないこと、`WithKeyLock` 内での `IsTrashed` 再確認により事前チェック後の割り込み投入・復活が正しく反映されること（TOCTOU 回帰テスト）、`TrashWorker.runOnce` が保持期間超過分のみを処理し未超過分に触れないこと、`ListPendingDeletionsOlderThan` の cutoff 絞り込み・`SweepOnePendingDeletion` の単発処理、起動時マイグレーション（`trashed_at` カラムが存在しない DB に対して `ALTER TABLE` が実行されること）を検証する
- **ゴミ箱状態 KEY への削除予約先送り（§8.5 [MANDATORY]、レビュー回帰テスト）**: `sweepPendingDeletions`/`startupSweep` が、`marked_at` 超過済みでも対象 KEY が現在ゴミ箱状態なら `SweepOnePendingDeletion` を呼ばずスキップすること、`IsTrashed` 自体のエラーは silent failure にせず記録して継続すること、KEY を `trash_index` → 保持期間内の `restore_index` で復活させた場合に series データ（chunk）が失われていないことを検証する
- **シャットダウン時の Worker 待機**: `internal/trash.Worker.Done()` が `Start` の goroutine 終了後に close されること（`ctx` キャンセル前は close されないこと・キャンセル後は妥当な時間内に close されること）を検証する

## 12. 使用する既存コンポーネント

初版は新規プロジェクトのため再利用対象なし。FNC-006（desired-state 同期 + 削除予約 GC）は以下の既存コンポーネントを再利用して実装されている:

| コンポーネント                                                                     | 場所              | 利用方法                                                                                                                                                                         |
| ---------------------------------------------------------------------------------- | ----------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `upsertOne`（DIF-01〜03 差分管理）                                                 | `internal/mcp`    | `sync_documents` の documents 処理で無改造再利用（SYN-02）                                                                                                                       |
| `Store.DeleteSeriesAll`                                                            | `internal/store`  | series 全体予約のスイープで再利用（series-wide 予約行の同一 tx 除去を追加、§4.5 [MANDATORY]）                                                                                    |
| `Store.deleteRecordWithBM25Tx`                                                     | `internal/store`  | `DeleteOrphanRecords` の物理削除（chunks / embeddings / BM25 整合）で再利用                                                                                                      |
| `Store.DeleteKey`                                                                  | `internal/store`  | 自動最終処分（`internal/trash.Worker`）の削除実行。呼び出し側が `WithKeyLock` で囲む（§4.3。予約行の同一 tx 全行除去を追加）                                                     |
| `expiry.Worker` の Stats パターン（`Start`, `runOnce`, `Stats`, `KeyDeleteError`） | `internal/expiry` | `internal/trash.Worker`（FNC-007）の実装土台として構造を踏襲。`SyncJobStatus`（`sync.Mutex` 保護 map）の設計も同パターンを踏襲（§5.4）。TTL/LRU の判定ロジック自体は再利用しない |
| `.claude/skills/delete-db-series/`（SKILL.md, `scripts/docdb_client.py`）          | `.claude/skills/` | 新規 SKILL `manage-db-indexes`（FNC-007）の雛形（frontmatter 構成、MCP HTTP 直叩き方式、Step 構成）として再利用                                                                  |

## 13. マイグレーション（FNC-007: TTL/LRU → ゴミ箱への移行）

**DB スキーマ**: 既存の `initSchema` は `CREATE TABLE IF NOT EXISTS` のみで、既存テーブルへのカラム追加を行わない。起動時に `PRAGMA table_info(keys)` で `trashed_at` 列の有無を確認し、無ければ `ALTER TABLE keys ADD COLUMN trashed_at TEXT` を実行してから起動を継続する（旧 `expiry_policy` カラムは残置してよい。読み書きしないため実害はない）。

**設定ファイル（`doc-db.yaml`）**: `expiry:` セクションの削除に伴う既存設定ファイルとの後方互換性は考慮しない（本プロジェクトは単一ユーザー運用のため、利用者自身が `doc-db.yaml` から `expiry:` セクションを手動で削除し `trash:` セクション（§9.2）に置き換える前提とする）。`KnownFields(true)`（CFG-03）により、`expiry:` セクションが残ったままの設定ファイルは起動時にエラーになる。この点は CHANGELOG に明記する。`internal/config.Config` の `ExpiryConfig` は `TrashConfig{RetentionDays int, IntervalSeconds int}` に置き換える。

## 改定履歴

| 日付       | バージョン | 内容                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| ---------- | ---------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 2026-06-20 | 0.1        | 初版作成                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| 2026-06-20 | 0.2        | レビュー対応: C2(DIF-02 series 剥がし漏れ修正)・C3(WAL+接続プール方針に改訂)・H1(トークナイザ仕様追加)・H2(ID boost/EMB guarantee追加)・H3(スケール上限明記)・H5(LRU SQL修正)・H6(SSRF対策追加)・H7(起動時 fail-fast)・Chunker依存修正・WALテスト注記・series廃棄TBD追加                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| 2026-06-20 | 0.3        | レビュー対応(追補): M1(ハッシュ正規化規則追加)・M2(部分 Embed 失敗方針を部分保存に確定)・§4.1(dim 検査の動作主体を明示)・§3.1(internal/mcp の embedder 依存を追記)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| 2026-06-24 | 0.4        | §9 を YAML 設定ファイル方式に変更（`~/.doc-db/doc-db.yaml` 固定パス・環境変数オーバーライド不採用・API キーのみ環境変数）。本文中の `DOCDB_*` 環境変数参照を設定ファイルキー参照に更新                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| 2026-06-28 | 0.5        | APP-001 PHIL-01/02 (二層検索アーキ) に対応: §2.1 アーキテクチャ概要に Layer 1/2 説明と更新 mermaid 図を追加。§6.4 全文 GREP signal の設計を新規追加 (substring 一致・origin_signals 記録)。§6.5 Candidate Merge を新規追加 (3 signal 合算ロジック)。§6.6 LLM Rerank を従来の §6.4 から番号変更 + PHIL-02 (Rerank は optional) を明記。§10 エラーハンドリングを silent failure 禁止方針 (memory: no-silent-failure) に整合させ Embedder の `errors.Join` / Reranker の warnings / Expiry の Stats() 公開を反映                                                                                                                                                                                                                                                                                                                                                                                                                       |
| 2026-07-01 | 0.6        | §5.2 upsert_documents シーケンス冒頭に content 取得 3 経路 (content / url / **local_path**) の表を追加。local_path はローカル運用時の payload 削減用途で、doc-db が絶対パスから直接ディスク読み込みする。安全性制約 (絶対パス必須・`..` 禁止・symlink 解決後再検証・10MB 上限・regular file 限定) を明記                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| 2026-07-03 | 0.7        | §4.1 の `bm25_stats`/`bm25_df` スキーマ定義・§6.2 の関連更新手順を、v0.1.2 で廃止済み (実装は substring match による都度計算方式) の実態に合わせて修正                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| 2026-07-04 | 0.8        | §5.1 ユースケース一覧に未掲載だった `delete_series` / `manage_index` を追加し、既存ツール一覧として完全化                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| 2026-07-06 | 0.9        | FNC-006（desired-state 同期ジョブ + 削除予約の起動時 GC、追加設計書 v1.12 相当）を統合: §4.3 KEY 単位排他制御（WithKeyLock）・§4.5 削除予約と orphan 回収（pending_deletions、[MANDATORY] 2 件含む）を新設、§4.1 に pending_deletions スキーマ、§4.4 に sync 切り離しの意図的例外注記、§5.1 を 10 ツール化 + §5.4 sync_documents シーケンス新設、§8.3 の series 廃棄 TBD を解決（削除予約 + 起動時スイープ方式、TTL/LRU series 拡張は不採用）、§8.5 起動時スイープ新設、§11 に排他・削除予約・sync・DIF-02 不変条件の検証観点を追加、§12 に再利用コンポーネント表を追加                                                                                                                                                                                                                                                                                                                                                             |
| 2026-07-09 | 1.0        | FNC-007（KEY のゴミ箱管理、追加設計書 DES-003 相当）を統合し `docs/specs/expiry-visibility/` を削除（`/forge:merge-specs` 実行）。TTL/LRU 自動廃棄（旧 EXP-01/02、`internal/expiry`）・`manage_index`・`delete_index` を廃止し、`trash_index` / `list_trashed_indexes` / `restore_index` + `internal/trash.Worker`（定期実行）による user-driven ゴミ箱モデルに置き換え。§2.1/§3.1/§3.2 の図・型・パッケージ一覧を更新、§4.1 に `keys.trashed_at` カラム追加、§4.6 KEY 単位ゴミ箱状態と TOCTOU 対策（`WithKeyLock` 内での `IsTrashed` 再確認）を新設、§5.1 ユースケース一覧更新、§5.5〜§5.7 にゴミ箱投入・自動最終処分・操作拒否のシーケンスを新設、§8 を「ゴミ箱管理と自動最終処分」に全面改訂（TTL/LRU 判定ロジック削除）、§9.2 設定スキーマを `expiry:` → `trash:` に変更、§10 エラーハンドリング表更新、§11/§12 に検証観点・再利用コンポーネントを追加、§13 マイグレーション（DB スキーマ ALTER・設定ファイル非後方互換）を新設 |
| 2026-07-10 | 1.1        | レビュー指摘 3 件に対応。(1) §5.6/§8.5: `sweepPendingDeletions`/`startupSweep` が KEY のゴミ箱猶予期間と無関係に古い series 全体予約を sweep してしまい、`restore_index` 後も series データが戻らない事故を防ぐため、KEY がゴミ箱状態の間は当該 KEY の削除予約処理を先送りする仕様を明記（`internal/trash/trash.go`・`cmd/docdb/main.go` を対応する実装に修正）。(2) §5.7: `query` は `WithKeyLock` 再確認の対象外（読み取り専用パスのため）である点と、それに伴う TOCTOU 競合ウィンドウの許容理由を明記（診断のみで実装は変更せず）。(3) §8: シャットダウン時に `internal/trash.Worker` の実行完了を待たず `Store.Close()` する問題を修正するため `Worker.Done()` を新設し、`cmd/docdb/main.go` がこれを待ってから Store を閉じるよう変更した旨を記録                                                                                                                                                                              |
| 2026-07-10 | 1.2        | 追加レビュー指摘 2 件に対応。(1) §8: 1.1 で導入した `Worker.Done()` 待ちが、`run()` の親 ctx（`signal.NotifyContext` で SIGINT/SIGTERM のみに連動）を worker にそのまま渡していたため、HTTP サーバー起動の即時失敗（ポート使用中等）経路では親 ctx が一切キャンセルされず `run()` が永久にハングする欠陥を修正。worker 専用の子 context（`context.WithCancel(ctx)`）を用意し、`run()` のどの終了経路でも必ず子 context 自身をキャンセルしてから `Done()` を待つ設計に変更した旨を追記。(2) `trash_index` MCP ツールの Description（`internal/mcp/mcp.go`）が「query も同様に拒否され、誤って検索・参照することはできない」と断定していた箇所を、§5.7 で既に文書化した TOCTOU 競合ウィンドウの許容方針と整合するよう表現を修正した旨を記録                                                                                                                                                                                           |
| 2026-07-30 | 1.3        | §4.5 orphan record の既知の制約に「`list_indexes` の series 一覧からは消える」を追加。`fetchSeriesForKey` が `series_keys JOIN records` の `DISTINCT` であるため、desired-state が空だった series は一覧に現れず、**`list_indexes` では「未同期」と「同期済みだが空」を区別できない**（FNC-004 MNG-01）。クライアントは送信対象ドキュメント数 0 件との併用で切り分ける必要がある旨と、`pending_deletions` の予約行がスイープまで内部の痕跡として残る一方でそれを API へ公開せず、恒久的に区別可能な状態も新設しない設計判断（後者でも検索結果は必然的に 0 件であり、区別が必要なのは利用者への案内文に限られる）を明記。§3.2 の `KeyInfo` 説明にも series の集計元を追記。実装変更は無く、既存実装の観測可能な性質の文書化のみ                                                                                                                                                                                                      |
| 2026-08-01 | 1.4        | §10.1「エラー種別の機械判別」を新設（APP-001 ERR-01 / Issue #7）。識別子 `KEY_NOT_FOUND` / `KEY_TRASHED` を公開契約として定義し、`*jsonrpc.Error` で返す設計を明記（go-sdk はそれ以外の error を text だけの `CallToolResult` に包むため構造化情報を載せられない）。識別子は `Data`（判別の正本）と `Message` 先頭（`data` が届かない経路のフォールバック）の両方に載せる。生成箇所を `internal/mcp/errcode.go` に集約し、回帰テスト `errcode_test.go` で不変条件を保証する                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
