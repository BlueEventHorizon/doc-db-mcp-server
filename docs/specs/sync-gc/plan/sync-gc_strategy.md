# sync-gc 実装戦略

## アプローチ

**選択**: リスク駆動（部分的にボトムアップを併用）

**根拠**:

DES-003 の技術的リスクは明確に偏在している。

- 最大のリスクは **§3.5 KEY 単位排他ロック（`Store.WithKeyLock`、呼び出し側方式）** である。`internal/store.Store` に新しい `sync.Mutex` レイヤー（`keyLocks`、参照カウント方式）を追加し、既存の `s.mu`（グローバル書き込み Mutex、全 write 系メソッドが自前 lock/defer unlock している）とは別レイヤーとして共存させる必要がある。レビューで指摘された通り、当初案（`DeleteKey` が自身の内部でロックを取得する方式）は再入デッドロックの危険があったため、ロック取得主体を呼び出し側に統一する `WithKeyLock(ctx, key, fn)` 方式へ設計変更した（DES-003 §3.5.1 参照）。`WithKeyLock` 自体の排他性を MCP ツール経由の間接確認ではなく直接の単体テストで先に固めることが最優先。
- 次点のリスクは **GC-05（ジョブ用 context の寿命分離）**。MCP リクエスト context をそのまま goroutine に渡すと「動くように見えて実は動かない」バグになりやすく、見た目のテストでは検出しづらい。
- 一方で、`pending_deletions` テーブルと `MarkSeriesForDeletion` / `MarkDocumentForDeletion` / `ClearPendingDeletion` / `SweepPendingDeletions` は、既存の `DeleteSeries` / `DeleteSeriesAll` を無改造で呼ぶだけの薄いラッパーであり、技術的不確実性は低い。`WithKeyLock` 基盤が固まった後に安全に乗せられる。
- `internal/mcp` の新規 3 ツール（`sync_documents` / `get_sync_status` / `schedule_delete_series`）は、既存の `Handlers.Register` パターン・`upsertOne`（mcp.go:282）・`expiry.Stats` の mutex パターン（expiry.go:50-90）という確立された型に沿うため、設計の新規性は低い。
- フィーチャースライスは採らない。理由: `sync_documents` の縦断フロー自体の基盤（`WithKeyLock`）が先に安定していないと、フェーチャー全体を通しても「たまたま動いた」以上の検証にならない（レースは低頻度にしか顕在化しない）。

## フェーズ

### フェーズ 1: `WithKeyLock` 基盤 + 直接排他性テスト（最大リスクの先行検証）

- **目標**: `internal/store.Store` に `WithKeyLock(ctx, key, fn func() error) error` を新設する（参照カウント方式・バッファ1の channel を mutex 代わりに使い `select` で `ctx.Done()` と競合させる、DES-003 §3.5.1）。`sync.Mutex` ではなく channel を使う理由は、ロック待機中でも `ctx` のキャンセルに応答できるようにするため（GC-05 と矛盾しないための必須要件）。**`DeleteKey` 自身は変更しない**（ロックを内部で取らない構造を維持するため既存実装のまま）。この段階で `internal/store/store_test.go` に、`WithKeyLock` 単体の直接排他性テスト（goroutine A が保持中、goroutine B が同一 KEY でブロックされることをタイムアウト付きで検証）、**ロック待機中に ctx を cancel すると `fn` を実行せず即座に `ctx.Err()` が返ることを検証するテスト**、参照カウントが 0 になったエントリが `keyLocks` map から削除されることのテストを書き、MCP ツール経由の間接確認に頼らず基盤を先に固める。
- **スコープ**: DES-003 §3.5.1〜§3.5.3。対象ファイルは `internal/store/store.go`（`WithKeyLock` 新設のみ、既存メソッドは無改造）。
- **検証ポイント**:
  - `go build ./...` 成功
  - 既存全テスト（DIF-02 3 点セット: `store_test.go::TestAppendAndCleanSeries_DIF02`、`mcp_test.go::TestUpsert_DIF02_SameHashSkips`、`upsert_integration_test.go::TestUpsertIntegration_DIF02_DoesNotCallEmbedder`）が緑のまま
  - `WithKeyLock` の直接排他性テスト（同一 KEY でブロック、異なる KEY では非ブロック）が通過
  - `WithKeyLock` 待機中キャンセルテスト（`fn` 未実行のまま `ctx.Err()` が返る）が通過
  - 参照カウント 0 で `keyLocks` map からエントリが削除されるテストが通過
  - `go test -race ./...` でロック導入によるレースが出ないこと

### フェーズ 2: 既存ハンドラ・`internal/expiry`・`delete_index` への `WithKeyLock` 適用 + pending_deletions 永続層（GC-01〜04）

- **目標**: `upsert_documents` / `delete_documents` / `delete_series` / `delete_index` の各ハンドラで、既存メソッド呼び出し全体を `store.WithKeyLock` で 1 回だけ囲む（処理本体は無改造、DES-003 §3.5.2 の呼び出し元一覧に従う）。`internal/expiry` の `storeForExpiry` インターフェースに `WithKeyLock` を追加し、`runTTL` / `runLRU` の `DeleteKey` 呼び出しを `WithKeyLock` で囲む（TTL/LRU 判定ロジックは無改造）。並行して `pending_deletions` テーブルと `MarkSeriesForDeletion`（`(alreadyScheduled bool, err error)`）/ `MarkDocumentForDeletion` / `ClearPendingDeletion` / `SweepPendingDeletions`（起動時専用、`WithKeyLock` 不要）を実装し、`cmd/docdb/main.go` の起動シーケンス（DB統計表示より前）にスイープを差し込む。
- **スコープ**: DES-003 §3.2（スキーマ）、§3.3（store メソッド）、§3.5.2（呼び出し元一覧）、§4.1「起動時スイープ」ユースケース。
- **検証ポイント**:
  - 自動テスト（フェーズ2完了条件、手動確認に頼らない）: `delete_index` 実行中に別ゴルーチンから同一 KEY への `upsert_documents` を呼ぶと `WithKeyLock` によりブロックされる（＝直列化される）ことを統合テストとして書く（計画書 TASK-008）
  - `MarkSeriesForDeletion` の冪等性・`alreadyScheduled` の正しい返却
  - `SweepPendingDeletions` が series 単位・path 単位それぞれで正しく物理削除すること
  - 回帰テスト: 同一 `content_hash` を複数 series が参照している状態で片方だけ削除予約・スイープした場合に record が残ること（DIF-02 系の安全条件を GC 側でも壊していないことの確認）
  - 個別失敗時にログ記録の上で処理継続すること（silent failure 禁止方針、GC-04）
  - 起動テストで、統計表示（`keyCount`/`totalChunkCount`）の値がスイープ後の状態を反映していること（GC-03）
  - `internal/expiry` のモックテストで `DeleteKey` 呼び出しが `WithKeyLock` 経由であることを呼び出し順序で確認する

### フェーズ 3: `sync_documents` / `get_sync_status` / `schedule_delete_series`（縦断結合 + GC-05）

- **目標**: フェーズ 1・2 で固めた基盤の上に、新規 3 MCP ツールを実装する。`cmd/docdb/main.go` が保持する shutdown 用 root context（既存の `expWorker.Start(ctx)` に渡しているものと同じ）を `Handlers` に渡すよう `mcp.New` のシグネチャを変更する（この配線変更をフェーズ 3 のスコープとして明示）。`sync_documents` は **1 回の `WithKeyLock` で desired-state 判定全体（documents 処理〜削除予約記録〜自己修復）を囲み**、`fn` 内で `upsertOne` / `MarkDocumentForDeletion` / `ClearPendingDeletion` を直接呼ぶ（ネストで `WithKeyLock` を再度呼ばない、DES-003 §3.5.2 の禁止規約）。`SyncJobStatus` のメモリ管理（`expiry.Stats` パターン踏襲、保持ポリシーは APP-003 TBD-101 として実装時に確定しコード内コメントに根拠を残す）と `get_sync_status` ポーリングを実装。`schedule_delete_series` は `MarkSeriesForDeletion` を `WithKeyLock` で囲む薄いラッパー。
- **スコープ**: DES-003 §3.1（`internal/mcp` 新規3ツール行）、§3.4、§3.6、§4（ユースケース・シーケンス図全体）。対象ファイルは `internal/mcp/mcp.go`（新規ハンドラ3種 + `Handlers` へのジョブ map・root context フィールド追加）、`internal/mcp/*_test.go`、`cmd/docdb/main.go`（`mcp.New` 呼び出しへの root context 引数追加）。
- **検証ポイント**:
  - `sync_documents` が `job_id` を即座に返しブロックしないこと（SYN-05）
  - `get_sync_status` が進捗・完了を正しく反映し、存在しない/期限切れ `job_id` でエラーを返すこと（SYN-06）
  - desired-state から path が欠落した場合に削除予約が作られ、再度含まれた場合に解除されること（SYN-03/04、series 全体の自己修復含む）
  - `schedule_delete_series` が即時削除せず `already_scheduled` を正しく返すこと（GC-01）
  - **最重要回帰テスト**: `sync_documents` 処理中に同一 KEY への `upsert_documents` / `delete_index` / TTL・LRU 相当の直接呼び出しがブロックされること（SYN-08）
  - root context を cancel した状態で `sync_documents` を実行するとジョブが `"failed"` になること（GC-05）
  - **再発防止テスト（GC-05）**: `sync_documents` が job_id を返した直後に MCP リクエスト context を cancel しても（root context は生存させたまま）ジョブが `"done"` まで完走すること。「request context を誤って goroutine に渡す」バグの再発を直接検出する
  - `go test -race ./...` 全体通過

## リスクと対策

| リスク                                                                                                                | 影響度 | 対策（どのフェーズで潰すか）                                                                                                                                                  |
| --------------------------------------------------------------------------------------------------------------------- | ------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `WithKeyLock` のネスト呼び出しによる再入デッドロック                                                                  | 高     | DES-003 §3.5.2 の禁止規約をコードコメントで明示。`DeleteKey` 等の基盤メソッドは `WithKeyLock` を内部で持たない構造にし、フェーズ 1 で構造的にネストが起きないことを確定させる |
| フェーズ 2 の `WithKeyLock` 導入によるレース見逃し                                                                    | 高     | フェーズ 1 で `WithKeyLock` 単体の排他性を直接テストで先に固めているため、フェーズ 2 は「呼ぶだけ」で済む。手動確認 + `go test -race` で検証                                  |
| `keyLocks` map の参照カウントがバグって 0 にならず、KEY を跨いで無制限に蓄積する（メモリリーク）                      | 中     | フェーズ 1 の完了条件に「参照カウントが 0 でエントリが削除されること」の単体テストを含める                                                                                    |
| `pending_deletions` のスイープ順序ミスで、他 series が参照する record を誤って物理削除する（DIF-02 系不変条件の破壊） | 高     | フェーズ 2 で「複数 series が同一 hash を共有した状態で片方だけスイープしても record が残る」回帰テストを必須化                                                               |
| ジョブ状態のメモリ蓄積（APP-003 TBD-101: 保持ポリシー未確定）                                                         | 低〜中 | フェーズ 3 着手前に「件数上限」方式を実装判断として確定し、コメントで根拠を残す                                                                                               |
| `mcp.New` シグネチャ変更の影響範囲                                                                                    | 低     | フェーズ 3 で明示的にスコープに含め、`cmd/docdb/main.go` の配線変更として扱う                                                                                                 |
