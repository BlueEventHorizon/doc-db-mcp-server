# sync-gc 実装戦略

## アプローチ

**選択**: リスク駆動（部分的にボトムアップを併用）

**根拠**:

DES-003 の技術的リスクは明確に偏在している。

- 最大のリスクは **§3.5 KEY 単位排他ロック（`Store.WithKeyLock`、呼び出し側方式）** である。`internal/store.Store` に新しい **channel ベースの KEY 単位ロック**レイヤー（`keyLocks` map、参照カウント方式。ロック本体はバッファ1の channel で `ctx.Done()` と select 可能にし、map 自体の保護のみ小さな `sync.Mutex`（`keyLocksMu`）を使う）を追加し、既存の `s.mu`（グローバル書き込み Mutex、全 write 系メソッドが自前 lock/defer unlock している）とは別レイヤーとして共存させる必要がある。レビューで指摘された通り、当初案（`DeleteKey` が自身の内部でロックを取得する方式）は再入デッドロックの危険があったため、ロック取得主体を呼び出し側に統一する `WithKeyLock(ctx, key, fn)` 方式へ設計変更した（DES-003 §3.5.1 参照）。`WithKeyLock` 自体の排他性を MCP ツール経由の間接確認ではなく直接の単体テストで先に固めることが最優先。
- 次点のリスクは **GC-05（ジョブ用 context の寿命分離）**。MCP リクエスト context をそのまま goroutine に渡すと「動くように見えて実は動かない」バグになりやすく、見た目のテストでは検出しづらい。
- **第三のリスクは `DetachSeriesFromPath` が導入する orphan record**（DES-003 §3.3）。既存 store の「series_keys が空になった record は即時物理削除」という全削除経路（`CleanOtherSeries` / `DeleteSeries` / `DeleteSeriesAll`）に埋め込まれた不変条件への**意図的な例外**であり、検索可視性・回収経路（`CleanOtherSeries` による別内容復活時の回収、`DeleteOrphanRecords` による予約解除直前の決定的補償、スイープによる起動時回収、`ClearPendingDeletion` の成功 path 限定）の組み合わせで初めてリークなしが成立する。単体では薄い変更に見えるが、既存不変条件との相互作用が本 feature で最も見落としやすい箇所であり、フェーズ 2 のテスト重点とする。一方、`MarkSeriesForDeletion` / `MarkDocumentForDeletion` / `ClearPendingDeletion` / `SweepPendingDeletions` 自体は既存の `DeleteSeries` / `DeleteSeriesAll` を無改造で呼ぶだけの薄いラッパーで、この部分の技術的不確実性は低い。
- `internal/mcp` の新規 3 ツール（`sync_documents` / `get_sync_status` / `schedule_delete_series`）は、既存の `Handlers.Register` パターン・`upsertOne`（mcp.go:282）・`expiry.Stats` の mutex パターン（expiry.go:50-90）という確立された型に沿うため、設計の新規性は低い。
- フィーチャースライスは採らない。理由: `sync_documents` の縦断フロー自体の基盤（`WithKeyLock`）が先に安定していないと、フェーチャー全体を通しても「たまたま動いた」以上の検証にならない（レースは低頻度にしか顕在化しない）。

## フェーズ

### フェーズ 1: `WithKeyLock` 基盤 + 直接排他性テスト（最大リスクの先行検証）

- **目標**: `internal/store.Store` に `WithKeyLock(ctx, key, fn func() error) error` を新設する（参照カウント方式・バッファ1の channel を mutex 代わりに使い `select` で `ctx.Done()` と競合させる、DES-003 §3.5.1）。`sync.Mutex` ではなく channel を使う理由は、ロック待機中でも `ctx` のキャンセルに応答できるようにするため（GC-05 と矛盾しないための必須要件）。**`DeleteKey` 自身は変更しない**（ロックを内部で取らない構造を維持するため既存実装のまま）。この段階で `internal/store/store_test.go` に、`WithKeyLock` 単体の直接排他性テスト（goroutine A が保持中、goroutine B が同一 KEY でブロックされることをタイムアウト付きで検証）、**ロック待機中に ctx を cancel すると `fn` を実行せず即座に `ctx.Err()` が返ることを検証するテスト**、参照カウントが 0 になったエントリが `keyLocks` map から削除されることのテストを書き、MCP ツール経由の間接確認に頼らず基盤を先に固める。
- **スコープ**: DES-003 §3.5.1〜§3.5.3。対象ファイルは `internal/store/store.go`（`WithKeyLock` 新設 + `Store` 構造体へのフィールド追加（`keyLocksMu` / `keyLocks`）+ `store.New` での `keyLocks` map 初期化。nil map への書き込みは panic するため初期化は必須。既存メソッドは無改造）。
- **検証ポイント**:
  - `go build ./...` 成功
  - 既存全テスト（DIF-02 3 点セット: `store_test.go::TestAppendAndCleanSeries_DIF02`、`mcp_test.go::TestUpsert_DIF02_SameHashSkips`、`upsert_integration_test.go::TestUpsertIntegration_DIF02_DoesNotCallEmbedder`）が緑のまま
  - `WithKeyLock` の直接排他性テスト（同一 KEY でブロック、異なる KEY では非ブロック）が通過
  - `WithKeyLock` 待機中キャンセルテスト（`fn` 未実行のまま `ctx.Err()` が返る）が通過
  - 参照カウント 0 で `keyLocks` map からエントリが削除されるテストが通過
  - `go test -race ./...` でロック導入によるレースが出ないこと

### フェーズ 2: 既存ハンドラ・`internal/expiry`・`delete_index` への `WithKeyLock` 適用 + pending_deletions 永続層（GC-01〜04）

- **目標**: `upsert_documents` / `delete_documents` / `delete_series` / `delete_index` の各ハンドラで、既存メソッド呼び出し全体を `store.WithKeyLock` で 1 回だけ囲む（処理本体は無改造、DES-003 §3.5.2 の呼び出し元一覧に従う）。`internal/expiry` の `storeForExpiry` インターフェースに `WithKeyLock` を追加し、`runTTL` / `runLRU` の `DeleteKey` 呼び出しを `WithKeyLock` で囲む（TTL/LRU 判定ロジックは無改造）。並行して `pending_deletions` テーブルと `DetachSeriesFromPath`（series 紐付けのみ即時削除・record は物理削除しない。SYN-03 の検索最新性の要、DES-003 §3.3）/ `DeleteOrphanRecords`（orphan-only の決定的回収。`CleanOtherSeries` 個別失敗の補償 + 起動時スイープの path 単位処理。stale 予約行が live record を壊さない要）/ `ListPendingDeletions`（fn 冒頭での予約一括取得）/ `MarkSeriesForDeletion`（`(alreadyScheduled bool, err error)`）/ `MarkDocumentForDeletion` / `ClearPendingDeletion` / `SweepPendingDeletions`（起動時専用、`WithKeyLock` 不要。path 単位は `DeleteOrphanRecords`・series 単位は `DeleteSeriesAll`）を実装し、`cmd/docdb/main.go` の起動シーケンス（DB統計表示より前）にスイープを差し込む。
- **スコープ**: DES-003 §3.2（スキーマ）、§3.3（store メソッド）、§3.5.2（呼び出し元一覧）、§4.1「起動時スイープ」ユースケース。
- **検証ポイント**:
  - 自動テスト（フェーズ2完了条件、手動確認に頼らない）: `delete_index` 実行中に別ゴルーチンから同一 KEY への `upsert_documents` を呼ぶと `WithKeyLock` によりブロックされる（＝直列化される）ことを統合テストとして書く（計画書 TASK-008）
  - `MarkSeriesForDeletion` の冪等性・`alreadyScheduled` の正しい返却
  - `DetachSeriesFromPath` の単体テスト: 切り離し後に series 指定検索へ現れないこと・record/chunks/embeddings が残存すること（orphan 保持）・`orphaned` の真偽が参照 series 数に応じて正しいこと
  - `DeleteOrphanRecords` の単体テスト: series_keys 0 件の record のみ物理削除・紐付き record には不触・orphan 不在時は冪等・doc_count 更新
  - `SweepPendingDeletions` が series 単位（`DeleteSeriesAll`）・path 単位（`DeleteOrphanRecords`）それぞれで正しく物理削除すること
  - stale 予約行の無害性検証: 予約が残ったまま record が復活した状態でスイープしても live record が保持され予約行のみ消えること（`DeleteSeries` ベースへの退行検出）
  - 回帰テスト: 同一 `content_hash` を複数 series が参照している状態で片方だけ切り離し・スイープした場合に record が残ること（DIF-02 系の安全条件を GC 側でも壊していないことの確認）
  - 個別失敗時にログ記録の上で処理継続すること（silent failure 禁止方針、GC-04）
  - 起動テストで、統計表示（`keyCount`/`totalChunkCount`）の値がスイープ後の状態を反映していること（GC-03）
  - `internal/expiry` のモックテストで `DeleteKey` 呼び出しが `WithKeyLock` 経由であることを呼び出し順序で確認する

### フェーズ 3: `sync_documents` / `get_sync_status` / `schedule_delete_series`（縦断結合 + GC-05）

- **目標**: フェーズ 1・2 で固めた基盤の上に、新規 3 MCP ツールを実装する。`cmd/docdb/main.go` が保持する shutdown 用 root context（既存の `expWorker.Start(ctx)` に渡しているものと同じ）を `Handlers` に渡すよう `mcp.New` のシグネチャを変更する（この配線変更をフェーズ 3 のスコープとして明示）。`sync_documents` は **1 回の `WithKeyLock` で desired-state 判定全体（documents 処理〜削除予約記録〜自己修復）を囲み**、`fn` 内で `upsertOne` / `ListPendingDeletions`（冒頭で予約一括取得）/ `DetachSeriesFromPath` / `MarkDocumentForDeletion` / `DeleteOrphanRecords` / `ClearPendingDeletion` を直接呼ぶ（ネストで `WithKeyLock` を再度呼ばない、DES-003 §3.5.2 の禁止規約。予約解除は `DeleteOrphanRecords` → `ClearPendingDeletion` の 2 段階、補償失敗時は Clear しない）。`SyncJobStatus` のメモリ管理（`expiry.Stats` パターン踏襲、保持ポリシーは APP-003 TBD-101 として実装時に確定しコード内コメントに根拠を残す）と `get_sync_status` ポーリングを実装。`schedule_delete_series` は `MarkSeriesForDeletion` を `WithKeyLock` で囲む薄いラッパー。
- **スコープ**: DES-003 §3.1（`internal/mcp` 新規3ツール行）、§3.4、§3.6、§4（ユースケース・シーケンス図全体）。対象ファイルは `internal/mcp/mcp.go`（新規ハンドラ3種 + `Handlers` へのジョブ map・root context フィールド追加）、`internal/mcp/*_test.go`、`cmd/docdb/main.go`（`mcp.New` 呼び出しへの root context 引数追加）。
- **検証ポイント**:
  - `sync_documents` が `job_id` を即座に返しブロックしないこと（SYN-05）
  - `get_sync_status` が進捗・完了を正しく反映し、存在しない/期限切れ `job_id` でエラーを返すこと（SYN-06）
  - **検索最新性の回帰テスト（SYN-03 の核心）**: path が欠落した sync 完了直後に当該 series 指定の検索を実行し、削除された path の chunk が返らないこと（「削除予約はされたが検索に残り続ける」実装への退行を検出）
  - desired-state から path が欠落した場合に series から即時切り離され、orphan record のみ物理削除予約が作られ、再度含まれた場合に解除されること（SYN-03/04、series 全体の自己修復含む）
  - 自己修復の API 課金ゼロ検証: 切り離し済み path を同一内容で再 sync すると Embedder が呼ばれず（Embedder spy）復活すること（SYN-04）
  - orphan 非リーク検証: 切り離し済み path を**別内容**で再 sync すると旧 orphan record が物理削除されること・`CleanOtherSeries` が掃除し損ねた状況（人工 orphan）でも `DeleteOrphanRecords` 補償で回収されること・upsertOne 失敗 path の削除予約は解除されないこと（DES-003 §3.3 の実行条件）
  - `schedule_delete_series` が即時削除せず `already_scheduled` を正しく返すこと（GC-01）
  - **最重要回帰テスト**: `sync_documents` 処理中に同一 KEY への `upsert_documents` / `delete_index` / TTL・LRU 相当の直接呼び出しがブロックされること（SYN-08）
  - root context を cancel した状態で `sync_documents` を実行するとジョブが `"failed"` になること（GC-05）
  - **再発防止テスト（GC-05）**: `sync_documents` が job_id を返した直後に MCP リクエスト context を cancel しても（root context は生存させたまま）ジョブが `"done"` まで完走すること。「request context を誤って goroutine に渡す」バグの再発を直接検出する
  - `go test -race ./...` 全体通過

## リスクと対策

| リスク                                                                                                                                                                                  | 影響度 | 対策（どのフェーズで潰すか）                                                                                                                                                                                                                                        |
| --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `WithKeyLock` のネスト呼び出しによる再入デッドロック                                                                                                                                    | 高     | DES-003 §3.5.2 の禁止規約をコードコメントで明示。`DeleteKey` 等の基盤メソッドは `WithKeyLock` を内部で持たない構造にし、フェーズ 1 で構造的にネストが起きないことを確定させる                                                                                       |
| フェーズ 2 の `WithKeyLock` 導入によるレース見逃し                                                                                                                                      | 高     | フェーズ 1 で `WithKeyLock` 単体の排他性を直接テストで先に固めているため、フェーズ 2 は「呼ぶだけ」で済む。自動統合テスト（`delete_index` 中の `upsert_documents` ブロック検証、計画書 TASK-008）+ `go test -race` で検証                                           |
| `keyLocks` map の参照カウントがバグって 0 にならず、KEY を跨いで無制限に蓄積する（メモリリーク）                                                                                        | 中     | フェーズ 1 の完了条件に「参照カウントが 0 でエントリが削除されること」の単体テストを含める                                                                                                                                                                          |
| `pending_deletions` のスイープ順序ミスで、他 series が参照する record を誤って物理削除する（DIF-02 系不変条件の破壊）                                                                   | 高     | フェーズ 2 で「複数 series が同一 hash を共有した状態で片方だけスイープしても record が残る」回帰テストを必須化                                                                                                                                                     |
| `DetachSeriesFromPath` が導入する orphan record（既存の「空 record 即時物理削除」不変条件の意図的な例外）が、既存経路（DIF-02 の record 再発見・doc_count・検索）と想定外に相互作用する | 中     | フェーズ 2 の `DetachSeriesFromPath` 単体テスト（orphan 残存・series 指定検索非表示・`orphaned` 真偽）で例外の境界を固定し、DIF-02 3 点セットの緑維持を完了条件に含める。フェーズ 3 で自己修復 API 課金ゼロ検証（Embedder spy）により orphan 再紐付け経路を直接検証 |
| ジョブ状態のメモリ蓄積（APP-003 TBD-101: 保持ポリシー未確定）                                                                                                                           | 低〜中 | フェーズ 3 着手前に「件数上限」方式を実装判断として確定し、コメントで根拠を残す                                                                                                                                                                                     |
| `mcp.New` シグネチャ変更の影響範囲                                                                                                                                                      | 低     | フェーズ 3 で明示的にスコープに含め、`cmd/docdb/main.go` の配線変更として扱う                                                                                                                                                                                       |
