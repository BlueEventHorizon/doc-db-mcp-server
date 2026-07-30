# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.3.2] - 2026-07-30

### Changed

- **query 系 SKILL の検索既定を現 branch の series 指定へ変更**: `/query-db-specs` /
  `/query-db-rules`（HTTP 版・`mcp版` とも）が series 無指定で全 branch 横断検索していた
  既定を、`resolve_docs.py` の `git_branch` を `--series` に渡す形へ変更。update 側は
  `sync_documents`（desired-state 同期）で登録するため当該 series は「その branch の
  完全な現在状態」であり、series 無指定の全 series 横断検索には、他 branch にしか無い
  文書に加えて **sync で切り離された削除済み文書が物理削除まで混入し得る**
  （APP-001 SYN-03 / DES-001 §4.5 の既知の制約）。ヒット 0 件でも series 無指定での
  再検索は行わない（0 件は「この branch に該当文書が無い」という正しい結果であり、
  他 branch の文書で代替すると、その branch で削除した文書を提示してしまう）
- **`docdb_client.py query` に series 登録検証を追加**（6 コピー共通）: `--series` 指定時は
  検索前に `list_indexes` で登録を確認し、当該 series（または KEY）が無ければ検索せず
  exit 3 で終了する。全 series 横断へのフォールバックはしない。`--no-verify-series` で
  検証を無効化できる。`list_indexes` の series 一覧は record の紐付きから作られるため
  **「未同期」と「同期済みだが対象文書 0 件」を区別できない**ことをエラーメッセージ・
  docstring に明記し、SKILL 手順側では Step 1 で `resolve_docs.py` の `count == 0` を
  先に判定して後者を切り分ける（同期済みで 0 件の branch に無意味な `/update-db-*` の
  再実行を案内し続けないため）
- **`docs/AI_INTEGRATION_GUIDE.md` / SKILL README の追記**: AI agent 一般向けにも
  `series` 指定を推奨とし、上記の区別不能な制約と切り分け手順を明記

### Documentation

- **APP-001 MNG-01 / DES-001 §3.2・§4.5 に `list_indexes` の series 一覧の制約を明記**:
  series 一覧は `series_keys JOIN records` の `DISTINCT`（`fetchSeriesForKey`）であり、
  record の紐付きが残っていない series は現れないため「未同期」と「同期済みだが
  desired-state が空だった」を区別できない。クライアントは送信対象ドキュメント数
  0 件との併用で切り分ける必要があること、および `pending_deletions` の予約行が
  スイープまで内部の痕跡として残る一方でそれを API へ公開せず、恒久的に区別可能な
  状態も新設しない設計判断を追記した（**実装変更は無く、既存実装の観測可能な性質の
  文書化**。query 系 SKILL がこの一覧に依存する検証を追加したため、契約として明文化した）

## [0.3.1] - 2026-07-10

### Fixed

- **シャットダウン時のハング修正**: `internal/trash.Worker` の実行完了を待たず
  `Store.Close()` する競合を修正した際に追加した `Worker.Done()` 待ちが、
  HTTP サーバー起動の即時失敗（ポート使用中等）経路ではシグナル駆動の親
  context が一切キャンセルされず、`run()` が永久にハングする欠陥を招いていた。
  worker 専用の子 context を導入し、`run()` のどの終了経路でも必ずキャンセル
  してから `Done()` を待つよう修正
- **ゴミ箱状態 KEY への削除予約先送り**: `sweepPendingDeletions`/`startupSweep`
  が、対象 KEY のゴミ箱猶予期間と無関係に古い series 全体予約を sweep して
  しまい、`restore_index` 後も series データが戻らない事故を防止。KEY が
  ゴミ箱状態の間は当該 KEY の削除予約処理を先送りするよう修正
- **`trash_index` ツール説明の修正**: 「query も同様に拒否され、誤って検索・
  参照することはできない」という断定を、実際の設計判断（query は読み取り
  専用パスのため TOCTOU 競合ウィンドウを許容する）と整合するよう表現を修正
- **`manage-db-indexes` SKILL のゴミ箱操作コマンド復元**: `docdb_client.py`
  の更新時に誤って削除されていた `list-indexes`/`trash-index`/
  `list-trashed-indexes`/`restore-index` サブコマンドを復元

### Added

- **`docdb_client.py` に `sync-start`/`sync-status` を追加**: `sync_documents`
  投入と完了ポーリングを分離した低レベル API。Bash tool の stderr 出力は
  ユーザーのチャットに中継されないため、AI がポーリングループを回して
  進捗をテキストで報告できるようにした（`update-db-specs`/`update-db-rules`
  SKILL に反映）

## [0.3.0] - 2026-07-10

### Removed

- **TTL/LRU 自動削除ワーカー（`internal/expiry`）を廃止**: `last_accessed_at`（TTL）・総 chunk
  数（LRU）による自動判定廃棄は、実運用で投入直後の唯一の KEY を無警告のまま削除する事故を
  起こしたため撤去した（ADR-003）。doc-db は「削除すべきかどうか」の判定を一切行わない設計に
  転換し、削除は必ずユーザー主導のゴミ箱投入を経由する
- **`manage_index` / `delete_index` MCP ツールを廃止**: KEY ごとの廃棄ポリシー設定・即時物理削除の
  経路を撤去。削除は `trash_index` に一本化した

### Added

- **`trash_index` / `list_trashed_indexes` / `restore_index` MCP ツールを新設**: KEY 単位のゴミ箱投入・
  一覧確認（自動最終処分までの残り時間含む）・復活を提供する。`keys.trashed_at` カラムと
  `TrashKey`/`RestoreKey`/`ListTrashedKeys`/`IsTrashed`（`internal/store`）で実装
- **`internal/trash.Worker`**: ゴミ箱投入済み KEY・orphan record（`pending_deletions`）を、設定した
  保持期間（デフォルト 3 日）経過後に定期実行で自動最終処分するワーカー。旧 `internal/expiry` を置換
- **`list_indexes` に `chunk_count` を追加**: KEY ごとの chunk 数を返し、ゴミ箱状態の KEY は結果から
  除外する
- **`manage-db-indexes` SKILL を新設**: KEY メタデータの確認・ゴミ箱投入・一覧確認・復活を対話的に
  行う管理 SKILL

### Changed

- **ゴミ箱状態の KEY への書き込み系操作を拒否**: `upsert_documents` / `delete_documents` /
  `delete_series` / `schedule_delete_series` / `sync_documents` の 5 ツールは、対象 KEY がゴミ箱
  状態の場合エラーを返し処理を実行しない（復活は `restore_index` の明示操作のみ）。`WithKeyLock`
  取得前後の二重チェックで TOCTOU を防止する
- **`query` はゴミ箱状態の KEY 指定時に明示エラーを返す**（空結果ではない）
- **設定ファイルの `expiry:` セクションを `trash:` に変更**（`retention_days` / `interval_seconds`）。
  既存設定ファイルとの後方互換性は考慮しない。`expiry:` セクションが残っている場合、
  `KnownFields(true)`（CFG-03）により起動時にエラーになる。**アップグレード時は
  `doc-db.yaml` の `expiry:` セクションを手動で `trash:` に置き換えること**
- **削除予約の物理削除処理を分割**: `SweepPendingDeletions`（KEY をまたぐ無条件一括処理）を
  `ListPendingDeletionsOlderThan` + `SweepOnePendingDeletion` に分割し、起動時スイープにも
  猶予期間（`marked_at` の cutoff 絞り込み）を適用するよう修正
- **仕様書統合**: 追加開発ディレクトリ `docs/specs/expiry-visibility/`（FNC-007/DES-003/ADR-003）を
  base 仕様（APP-001/DES-001、新設 ADR-003）へ統合し削除（`/forge:merge-specs`）

## [0.2.1] - 2026-07-07

### Fixed

- **cross-series DIF-02 dedup の回帰テスト追加**: 「同一 KEY の別 series に同一ファイルが
  存在しても skipped にならず全件 re-embedding される」という報告 (Issue #4) を調査し、
  series をまたいだ hash 参照（key 単位の embedding 共有）は仕様通り実装済みであることを
  確認。`sync_documents` 経由での cross-series DIF-02 挙動を content / local_path 両経路で
  回帰テスト化し、今後の regression を検出可能にした

### Added

- **re-embedding 理由の診断ログ**: `upsertOne` の DIF-03 経路（新規 Embedding）で、
  「純新規 path」か「同一 path の内容変更（hash 不一致）」かを区別する debug ログを追加。
  期待に反して re-embedding が発生した場合の原因切り分けに使う

### Changed

- **update-db-\* skill を sync_documents ベースの desired-state 同期に対応** (v0.2.0+ サーバー向け):
  `docdb_client.py` に `sync` サブコマンドを追加し、`run_sync.py` を新設。削除ファイルへの
  追従（一覧に無い path の即時切り離し）に対応し、upsert 方式のバッチ分割・offset ループが
  不要になった
- **ドキュメント整備**: dedup 成立条件（`key + path + 内容` の完全一致・series 非依存）を
  `AI_INTEGRATION_GUIDE.md` / SKILL.md 群に明記。README に AI エージェント向け SKILL 参考実装の
  導線を追加

## [0.2.0] - 2026-07-06

### Added (sync-gc: desired-state 同期 + 削除予約の起動時ガベージコレクション)

`upsert_documents` が追加専用でクライアント側のファイル削除に追従できない問題
（削除済みファイルの record が無期限に残留し検索結果を汚染する）を解消する
新機能一式 (APP-001 FNC-006 / DES-001 v0.9)。MCP ツールは 7 種 → 10 種になった。

- **`sync_documents` ツール新設** (SYN-01〜08): documents を当該 key・series の
  完全な現在状態 (desired-state) とみなし、一覧に含まれない既存 path を series から
  **即時に切り離す**（当該 series 指定の検索から直ちに消える）。差分管理は既存
  DIF-01〜03 を無改造再利用（hash 一致で Embedding 再計算なし）。ジョブ投入方式で
  job_id を即時返却し、処理はバックグラウンドで継続（サーバーシャットダウンには
  ロック待機中でも応答）。空リストも「現存ファイルなし」の正当な desired-state
  として受理する
- **`get_sync_status` ツール新設** (SYN-06/07): job_id からジョブ進捗
  (processed / skipped / failed / deleted_paths_marked) と完了・エラーを返す。
  ジョブ状態はメモリ保持のみ（完了ジョブは 100 件まで保持）
- **`schedule_delete_series` ツール新設** (GC-01): series 全体の削除予約を記録する
  （即時削除しない）。予約は次回起動まで完全に無害で、同一 key・series への
  `sync_documents` で自己修復（予約解除）できる
- **削除予約の起動時スイープ** (GC-02〜04): `pending_deletions` テーブルを新設し、
  予約された series / orphan record をサーバー起動時（DB 統計表示より前）に一括
  物理削除する。個別失敗はログ記録して起動継続
- **KEY 単位排他ロック `WithKeyLock`** (SYN-08): 同一 KEY への書き込み系操作
  （upsert / delete 系 / delete_index / TTL / LRU / sync）を直列化する。
  channel ベース実装でロック待機中も ctx キャンセルに応答可能

### Fixed

- `DeleteKey` / `DeleteSeriesAll` が stale な削除予約を残し、同名 KEY/series の
  再作成後に起動時スイープが新データを破壊し得た問題を修正（予約行を本体削除と
  同一トランザクションで除去。`DeleteSeriesAll` は orphan 回収手段を保全するため
  series-wide 予約のみ除去する非対称設計）
- `delete_documents` の存在チェックがロック外にあり、`sync_documents` 処理中に
  作成される path への削除要求を取りこぼす TOCTOU を修正（チェック〜削除を
  1 つの `WithKeyLock` 内へ移動）

### Docs

- sync-gc の要件・設計を本体仕様へ統合: APP-001 に FNC-006 を新設（TBD-009 解決）、
  DES-001 v0.9 に §4.3 排他制御 / §4.5 削除予約 / §5.4 sync シーケンス / §8.5
  起動時スイープを新設し、§8.3 の「series 自動廃棄は TBD」の矛盾を解消

### Chore

- MIT ライセンス表記を追加

## [0.1.13] - 2026-07-03

### Added

- 起動時に DB 統計 (KEY 数・総チャンク数) を標準出力へ表示 (`cmd/docdb/main.go`)。
  取得に失敗しても起動は継続する

### Fixed

- symbolic link 周りの不具合を修正

### Docs

- `docs/AI_INTEGRATION_GUIDE.md` の提供ツール表を実装 (7 ツール) に合わせて修正。
  漏れていた `delete_series` を追加し、`delete_documents` との使い分けを明記
- 競合調査ドキュメント `COMPETITORS.md` を追加

### Chore

- `reference/` 配下の古い forge / doc-advisor 関連ドキュメントを整理・削除

## [0.1.12] - 2026-07-02

### Added (config.log セクション + 起動時可視化 + リクエストログ)

ログ出力先が呼び出し側シェルのリダイレクト (`doc-db > /tmp/doc-db.log 2>&1 &`) 任せに
なっており、`doc-db.yaml` で管理されていなかった問題を解消。加えて、リクエスト単位の
処理状況が全く可視化されていなかった問題も解消した。

- **`config.log` セクション新設** (`internal/config`):
  - `log.path`（デフォルト `~/.doc-db/doc-db.log`。`"stdout"`/`"stderr"` も指定可）
  - `log.level`（`debug`/`info`/`warn`/`error`。デフォルト `info`）
  - **CFG-03 の例外**: `log` セクションは省略可。省略時は上記デフォルト値が補完される
    (既存の `doc-db.yaml` に変更を強いない後方互換措置。DES-001 §9 に明記)
- **サーバー自身がログファイルを開くように変更** (`cmd/docdb/main.go`):
  - `cfg.Log.Path` を `os.OpenFile` (`O_APPEND|O_CREATE`) で開き `slog` の出力先に設定
  - 親ディレクトリが無ければ自動作成
  - `"stdout"`/`"stderr"` 指定時はそのまま標準出力・標準エラーに出力
- **起動時バナー**: `slog` とは別に、config / log / db の実配置パスと待受ポートを
  標準出力へ直接 1 度だけ表示する。ログをファイルにリダイレクトしていても、
  起動直後にターミナルで確認できる
- **新規 `doc-db --show-config` フラグ**: サーバーを起動せずに解決済みの
  `version` / `config_path` / `log.path` / `log.level` / `db_path` / `port` /
  `embedding.model` を表示する
- **`make show-config` / `make show-log`** (Makefile): `--show-config` の結果から
  ログパスを取得して `tail -f` する。ログファイルが未作成の場合や `log.path` が
  `stdout`/`stderr` の特殊値の場合は分かりやすいメッセージを表示する
- **リクエスト単位のログ** (`internal/mcp/mcp.go`): 7 ツール全てに開始・終了ログを追加
  (`upsert_documents` / `delete_documents` / `delete_series` / `query` /
  `list_indexes` / `delete_index` / `manage_index`)。`upsert`/`delete_documents`/
  `delete_series`/`query` は処理時間 (`duration_ms`) も記録。`tail -f` で
  リアルタイムに処理状況を追えるようになった

### Changed (ドキュメント)

- `doc-db.yaml.example` / `README.md` / `docs/specs/base/design/DES-001` に
  `log:` セクションの説明を追加
- `.claude/skills/README.md` / `update-db-{specs,rules}/SKILL.md` /
  `docdb_client.py` (5 コピー) のサーバ起動案内から
  `doc-db > /tmp/doc-db.log 2>&1 &` を撤去し、`doc-db &` + `--show-config` 案内に統一

### テスト

- `internal/config`: `log` セクション省略時のデフォルト適用・明示指定・
  `stdout`/`stderr` 特殊値・不正な `level` の fail-fast を検証する 4 テストを追加
- 既存の全パッケージテスト (`go test -race ./...`) は変更なしで green を維持

## [0.1.11] - 2026-07-02

### Changed (SKILL バックエンドを MCP tool 経由 → HTTP 直叩きへ)

Claude Code の MCP tool ラッパー (`mcp__doc-db__*`) 経由の呼び出しから、
`http://localhost:<port>/mcp` を SKILL 内から Python stdlib のみで直接叩く
方式へ切り替え。Claude Code 側での MCP 登録が不要になり、他プロジェクトへの
配布 (rsync のみ) が完結する。

- **新規 `docdb_client.py`** (各 SKILL の `scripts/` に同一コピー、stdlib のみ):
  MCP Streamable HTTP handshake (`initialize` → `notifications/initialized` →
  `tools/call`) を実装。サブコマンド `query` / `upsert` / `delete-series`
- **`upsert` のバッチ分割 + 進捗表示** (v0.1.11 追加):
  - デフォルト 30 件ごとにバッチ分割 (`--batch-size` で変更可)。600+ ファイル
    一括 upsert でのタイムアウト・ハング見えを回避
  - MCP session を再利用 (initialize は最初の 1 回のみ、Client クラス導入)
  - 進捗を stderr に表示: `[done/total] processed/skipped/failed (Xs / batch, ETA Ys)`
  - バッチ失敗は他バッチを続行し `errors[]` に集約。最終 `failed>0` で exit 2
  - `--timeout` を top-level オプション化 (デフォルト 600 秒)
- **`resolve_docs.py` から PyYAML 依存を排除**:
  forge の `resolve_doc_structure.py::parse_config` 互換の stdlib-only 行ベース
  YAML parser を内蔵。追加パッケージインストール不要 (Python 3.9+ のみで動作)
- **5 SKILL.md を書き換え**: `mcp__doc-db__*` 可用性チェック →
  `docdb_client.py` 実行時の接続失敗判定に置換。Claude Code MCP 登録手順を撤去

### Changed (ドキュメント)

- **`README.md`** を v0.1.10 実装状況に整合。「実装状況」欄の未実装マーク撤去、
  MCP ツール 7 種を実装済みとして記述、Homebrew インストールを実利用可能な手順
  として再構成。開発コマンド節を追加
- **`README.md` §series による多バージョン管理**: SHA-256 ハッシュ dedup
  (DIF-02) の挙動・シナリオ別テーブル・コスト効果・テスト保証 3 レイヤーを追記
  (`TestAppendAndCleanSeries_DIF02` / `TestUpsert_DIF02_SameHashSkips` /
  `TestUpsertIntegration_DIF02_DoesNotCallEmbedder`)
- **`.claude/skills/README.md`** を HTTP 直叩き前提に更新。pyyaml 前提の記述撤去、
  `docdb_client.py` / `resolve_docs.py` の内部設計を明記
- `update-db-{specs,rules}/SKILL.md` の Notes を v0.1.9+ の `/delete-db-series`
  実装済みに整合させた記述に修正

### Notes

Go コード本体 (`cmd/` / `internal/`) の実装変更なし。バイナリは v0.1.10 と同一。
今回の変更は `.claude/skills/` 配下 (SKILL 定義・スクリプト) と `README.md` / 関連
文書のみ。Homebrew Formula の `tag:` は `v0.1.11` に更新済み。`revision:` は
本 bump commit 時点では 40 桁の `0` プレースホルダとし、git tag `v0.1.11` を打った
後に `chore(release): Formula revision を v0.1.11 tag commit に確定` で実際の
commit SHA へ更新する (慣行通り)。

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

[Unreleased]: https://github.com/BlueEventHorizon/doc-db-mcp-server/compare/v0.3.2...HEAD
[0.2.0]: https://github.com/BlueEventHorizon/doc-db-mcp-server/releases/tag/v0.2.0
[0.1.12]: https://github.com/BlueEventHorizon/doc-db-mcp-server/releases/tag/v0.1.12
[0.1.11]: https://github.com/BlueEventHorizon/doc-db-mcp-server/releases/tag/v0.1.11
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
