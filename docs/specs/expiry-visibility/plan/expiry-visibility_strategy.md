# expiry-visibility 実装戦略

## アプローチ

**選択**: ボトムアップ + リスク駆動（併用）

**根拠**:

設計書（DES-003 §3.1）のモジュール依存グラフは `internal/store`（最下層）→ `internal/trash` / `internal/mcp` → `.claude/skills/manage-db-indexes` という一方向の階層構造であり、循環はない。上位モジュール（MCP ハンドラ、SKILL）はすべて `internal/store` が提供する `TrashKey` / `RestoreKey` / `ListTrashedKeys` / `ListPendingDeletionsOlderThan` / `SweepOnePendingDeletion` / `IsTrashed` に依存するため、基盤層を先に固めるボトムアップが自然。

同時に、DES-003 §4.2「`SweepPendingDeletions` の分割」は、既存の起動時スイープ（GC-02）の挙動を変える破壊的変更であり、CLAUDE.md が明示する DIF-02 不変条件（3 テスト）とも同じ `internal/store` レイヤーで隣接するため、変更を誤ると気づかれにくい回帰を生む最大のリスク要因である。このリスクをフェーズ 1（最下層）に前倒しして最優先で潰すことで、上位フェーズでの手戻りを防ぐ。したがって「基盤から積み上げる」ボトムアップの原則の中に「最もリスクが高い変更を最初に着手する」リスク駆動の要素を組み込む。

## フェーズ

### フェーズ 1: Store 層基盤 + 最高リスク要素の先行決着

- **目標**: `internal/store` 単体で、KEY 単位ゴミ箱（`trashed_at`）と `pending_deletions` の cutoff 絞り込みがすべて実装され、テストが green。既存 DB への `ALTER TABLE` マイグレーションが動作する。特に既存の DIF-02 不変条件 3 テスト（`TestAppendAndCleanSeries_DIF02` 等）が本変更後も無傷で green であることを確認する。
- **スコープ**: DES-003 §3.1「`internal/store`（既存拡張）」、§5 テーブル該当行、§6 マイグレーション。具体的には:
  - `keys` テーブルへ `trashed_at` カラム追加。起動時に `PRAGMA table_info(keys)` で列の有無を確認し、無ければ `ALTER TABLE keys ADD COLUMN trashed_at TEXT` を実行する
  - `TrashKey` / `RestoreKey` / `ListTrashedKeys` / `IsTrashed`（多重投入エラー・存在しない KEY エラーを含む）
  - `KeyInfo.ChunkCount` 追加（`ListKeysByLRU` の chunk 集計 SQL をマージ）
  - 既存の `SweepPendingDeletions`（KEY をまたいだ全件無条件処理・ロック無し）を廃止し、`ListPendingDeletionsOlderThan(ctx, cutoff)`（読み取り専用、cutoff 絞り込み）と `SweepOnePendingDeletion(ctx, entry)`（1 KEY 分の物理削除）に分割する
  - `cmd/docdb/main.go` の `startupSweep` 呼び出しを、新 API（一覧取得 → エントリ 1 件ごとに `WithKeyLock` で囲んで `SweepOnePendingDeletion`）に置き換える
- **検証ポイント**:
  - `go test ./internal/store/... -race`
  - 既存 DIF-02 テスト 3 件が green のまま
  - `TrashKey`/`RestoreKey`/`ListTrashedKeys`/`IsTrashed` の正常系・異常系テスト
  - `ListPendingDeletionsOlderThan` の cutoff 境界値テスト（猶予期間内は一覧に含まれない・超過分のみ含まれる）
  - `trashed_at` カラムが存在しない DB に対するマイグレーションのテスト

### フェーズ 2: internal/trash Worker 新設 + internal/expiry 廃止

- **目標**: サーバー起動時に `internal/trash.Worker` が定期実行され、保持期間超過の KEY（`DeleteKey`）と `pending_deletions`（`ListPendingDeletionsOlderThan` + `SweepOnePendingDeletion`）を、それぞれ KEY 単位で `WithKeyLock` を取って自動処分する。TTL/LRU 関連コード（`internal/expiry` 一式、`manage_index` 由来の TTL/LRU オーバーライドロジック）が削除され、ビルドが通る。設定は `doc-db.yaml` の `trash:`（`retention_days` / `interval_seconds`）に置き換わる。
- **スコープ**: DES-003 §3.1「`internal/trash`（新設）」、§4.3 シーケンス図（UC-5/UC-6）、§6 マイグレーション（`trash:` 設定スキーマ）。具体的には:
  - `internal/trash/trash.go` 新設（`Worker`, `Start`, `runOnce`, `sweepTrashedKeys`, `sweepPendingDeletions` — `internal/expiry/expiry.go` の Worker 構造を土台に踏襲）
  - `runOnce` は `ListTrashedKeys` → 保持期間超過 KEY ごとに `WithKeyLock` + `DeleteKey`、`ListPendingDeletionsOlderThan` → エントリ 1 件ごとに `WithKeyLock` + `SweepOnePendingDeletion`、の 2 系統を実行する
  - 個別 KEY・record 削除失敗時はログ記録し処理継続（silent failure 禁止、CLAUDE.md 記載の feedback rule 準拠）。監査記録は `slog.Info` のみとし、DB 監査テーブルは設けない（レビューで確定した方針）
  - `cmd/docdb/main.go` の `internal/expiry.Worker` 起動箇所を `internal/trash.Worker` に置換
  - `internal/config` の `ExpiryConfig` を `TrashConfig{RetentionDays, IntervalSeconds}` に置き換え、`doc-db.yaml.example` の `expiry:` を `trash:` に置き換える
  - `internal/expiry` パッケージおよび呼び出し元の削除
- **検証ポイント**:
  - `go test ./internal/trash/... -race`
  - `runOnce` が保持期間超過分のみ処理し未超過分に触れないことのテスト
  - 個別エラー時の継続動作テスト（既存 `expiry` 回帰パターン踏襲）
  - `go build ./...` 成功、`grep -r "internal/expiry"` がゼロ件（デッドコード検出）
  - DIF-02 3 テスト再確認（green 維持）

### フェーズ 3: internal/mcp ハンドラ拡充

- **目標**: MCP ツール経由で UC-1〜UC-4（KEY メタデータ確認・ゴミ箱投入・一覧確認・復活）が一通り動作する。`manage_index` と `delete_index` が完全に削除され、`trash_index`/`list_trashed_indexes`/`restore_index` に置き換わる。`upsert_documents`/`sync_documents`/`delete_documents`/`schedule_delete_series` の 4 ツールすべてがゴミ箱状態の KEY への操作を拒否し、`query` はゴミ箱状態の KEY 指定時に明示エラーを返す。
- **スコープ**: DES-003 §3.1「`internal/mcp`（既存拡張）」、§4.2〜§4.5 シーケンス図（UC-2, UC-7, UC-8）。具体的には:
  - `handleListIndexes` に `chunk_count` 追加、ゴミ箱状態の KEY を結果から除外
  - `trash_index`（`TrashKey` を呼ぶ。`delete_index` を置換）/ `list_trashed_indexes`（`ListTrashedKeys` を呼び、`trashed_at` と設定値 `trash.retention_days` から `remaining_seconds` を算出して応答に含める。`Store` 層は設定値を持たないため、この計算は handler 層の責務とする）/ `restore_index`（`RestoreKey` を呼ぶ）新規ハンドラ
  - `handleDeleteIndex`・`DeleteIndexInput`/`Result` を削除する
  - `manage_index` ツール定義・ハンドラ・`ManageIndexInput` 系、`SetExpiryPolicy`/`ExpiryPolicy` の削除
  - `handleQuery` で処理開始前に `IsTrashed(key)` を確認し、true なら検索を実行せず明示エラーを返す（空結果にしない）
  - `handleUpsert` / `handleSyncDocuments` / `handleDeleteDocuments` / `handleScheduleDeleteSeries` の 4 ハンドラすべてに、処理開始前の `IsTrashed(key)` チェックと拒否エラーを追加する（**初版はここが `upsert_documents`/`sync_documents` の 2 ツールのみだったが、レビューで `delete_documents`/`schedule_delete_series` も対象漏れと指摘され 4 ツールに拡大した**）
- **検証ポイント**:
  - `go test ./internal/mcp/... -race`
  - `trash_index` 実行後に `query` が当該 KEY に対して明示エラーを返すこと（空結果にならないこと）
  - ゴミ箱状態の KEY への 4 ツール（upsert/sync/delete_documents/schedule_delete_series）呼び出しがすべて拒否され `trashed_at` が変化しないこと
  - `manage_index`/`delete_index` 削除後、`ManageIndexInput`/`DeleteIndexInput` 系テストが残っていないことの確認（grep によるデッドコード検出）
  - DIF-02 3 テスト再確認（green 維持）

### フェーズ 4: manage-db-indexes SKILL 新設 + 統合検証

- **目標**: UC-2〜UC-8 のフロー全体（ゴミ箱投入 → 一覧確認 → 復活 → 復活後の再ゴミ箱投入 → 保持期間経過後の自動処分 → ゴミ箱状態への操作拒否 → query 明示エラー）が SKILL 経由・MCP 経由でエンドツーエンドに動作する。
- **スコープ**: DES-003 §3.1「`.claude/skills/manage-db-indexes`（新設）」。具体的には:
  - `.claude/skills/delete-db-series/` の雛形（frontmatter、`docdb_client.py`、`resolve_docs.py`、Step 構成）を再利用して新規 SKILL を構築（PyYAML 等の外部依存を追加しない、CLAUDE.md 記載の制約に準拠）
  - `list_indexes` 一覧提示 → series 有無に応じた確認分岐（FNC-009）→ `trash_index` 呼び出しの対話フロー実装
  - ゴミ箱一覧提示（残り時間含む）、`restore_index` 対話フロー実装
- **検証ポイント**:
  - UC-2〜UC-8 の統合テストシナリオ（DES-003 §7 統合テスト対象に準拠、保持期間は短縮設定でテスト）
  - `make verify` 全体通過
  - DIF-02 3 テスト最終確認（green 維持）
  - README / `docs/AI_INTEGRATION_GUIDE.md` 等、SKILL 一覧・ツール一覧に関する記述の更新漏れ確認（`delete_index` 廃止・`trash_index` 等の新設を反映）

## リスクと対策

| リスク                                                                                                  | 影響度 | 対策（どのフェーズで潰すか）                                                                                                       |
| ------------------------------------------------------------------------------------------------------- | ------ | ---------------------------------------------------------------------------------------------------------------------------------- |
| `pending_deletions` の cutoff 絞り込み漏れ・起動時スイープ呼び出しとの不整合で GC-01/GC-02 が回帰する   | 高     | フェーズ 1 で最優先実装し、cutoff 境界値テスト（猶予期間内は未処理／超過分のみ処理）を先行して green にする                        |
| DIF-02 不変条件（同一 key+path のハッシュ dedup）が周辺変更（schema・pending 処理契機変更）で破壊される | 高     | 全フェーズ完了ごとに既存 3 テスト（`TestAppendAndCleanSeries_DIF02` 等）を都度実行し green を維持するチェックポイント化            |
| ゴミ箱操作拒否の対象漏れ（4 ツールのうち一部だけ実装してしまう）                                        | 高     | フェーズ 3 で 4 ツール（upsert/sync/delete_documents/schedule_delete_series）を横並びでチェックリスト化し、grep で網羅性を確認する |
| `manage_index`/`delete_index` 廃止に伴うデッドコード（`ManageIndexInput`/`DeleteIndexInput` 等）の残存  | 中     | フェーズ 3 で grep ベースのデッドコード確認をチェックリスト化して実施                                                              |
| SKILL 新設時に誤って PyYAML 等の外部 Python 依存を追加してしまう                                        | 低     | フェーズ 4 で `delete-db-series` の既存ファイル（stdlib のみ）をそのままコピーし、新規実装差分を対話フローのみに限定               |
| `internal/trash.Worker` の定期実行が `WithKeyLock` 未適用のまま他の書き込み系操作と競合する             | 中     | フェーズ 2 で SYN-08 準拠のレビューを行い、`go test -race` でレース検出テストを実行                                                |
