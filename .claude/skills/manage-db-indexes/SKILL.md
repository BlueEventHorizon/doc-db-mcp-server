---
name: manage-db-indexes
description: |
  doc-db インデックス (KEY) のメタデータ (chunk 数・doc 数・series・最終アクセス日時) を
  提示し、ゴミ箱投入・ゴミ箱一覧確認・復活を対話的に行う。KEY の削除は即時物理削除ではなく
  ゴミ箱経由 (保持期間経過後に自動最終処分)。削除要否の判定はユーザーが行い、doc-db はしない。
  トリガー: "doc-db の KEY を確認", "インデックスを削除", "ゴミ箱を確認", "KEY を復活"
user-invocable: true
argument-hint: "[list|trash|trashed|restore] [KEY名]   例: list / trash myrepo-specs / trashed / restore myrepo-specs"
allowed-tools: Bash
---

doc-db の KEY (インデックス) を管理する対話型ラッパー。**doc-db サーバの HTTP エンドポイント
(`http://localhost:<port>/mcp`) を直接叩く**ため、Claude Code の MCP 登録は不要。
他プロジェクトに `.claude/skills/manage-db-indexes/` をコピーすればそのまま動く。

**doc-db は「削除すべきかどうか」の判定を一切しない。** 本 SKILL は KEY ごとの事実情報
(chunk 数・doc 数・series・最終アクセス日時) を提示するだけであり、削除するかどうかの
判断は必ずユーザーに委ねる。

## Procedure

### Step 0: 操作の判定

`$ARGUMENTS` の先頭語で分岐する:

- 空 or `list` → Step 1 (KEY メタデータ一覧の提示) から開始
- `trash <KEY>` → Step 1 を実行した上で、指定 KEY を対象に Step 2 (ゴミ箱投入) を実行
- `trashed` → Step 3 (ゴミ箱一覧の提示) から開始
- `restore <KEY>` → Step 3 を実行した上で、指定 KEY を対象に Step 4 (復活) を実行

引数が無い場合は、まず Step 1 の一覧を見せた上でユーザーに次の操作 (削除 / 何もしない /
ゴミ箱確認) を選んでもらう。

### Step 1: KEY メタデータ一覧の提示 (FNC-008)

```bash
python3 .claude/skills/manage-db-indexes/scripts/docdb_client.py list-indexes
```

戻り値 `{"indexes": [{"key", "series", "doc_count", "chunk_count", "last_updated_at",
"last_accessed_at"}, ...]}` を表形式で提示する。**ゴミ箱状態の KEY はこの一覧に含まれない**
(Step 3 で別途確認する)。

提示する情報は事実のみ (chunk 数・doc 数・series 一覧・最終更新/アクセス日時)。
「古い」「大きすぎる」等の判定コメントは付けない。判断はユーザーに委ねる。

`indexes` が空の場合は「登録済み KEY がありません」と報告して終了。

### Step 2: ゴミ箱投入 (FNC-009)

ユーザーが Step 1 の一覧から削除したい KEY を選択したら、対象 KEY の `series` を確認し
分岐する:

- **`series` が 1 件以上ある場合 → 確認必須**: 「KEY `<KEY>` には series `<一覧>` が
  紐づいています。削除すると <chunk_count> chunk / <doc_count> doc が (即時ではなく)
  ゴミ箱状態になり、保持期間経過後に自動的に物理削除されます。よろしいですか?」と
  明示的に確認を取る。ユーザーが明確に同意するまで `trash_index` を呼ばない
- **`series` が空の場合 → 簡易操作**: 実データが紐づいていないため、確認を省略して
  そのまま次に進んでよい (ユーザーが明示的に削除を選択した時点で十分な意思確認済みと
  みなす)

確認 (または簡易操作の判断) が済んだら実行する:

```bash
python3 .claude/skills/manage-db-indexes/scripts/docdb_client.py trash-index --key "<KEY>"
```

戻り値 `{"key", "trashed": true, "trashed_at"}` を報告する。

**このステップで実データ (record/chunk/embedding) は物理削除されない。** ゴミ箱状態に
遷移するのみで、保持期間 (`trash.retention_days`、デフォルト 3 日) 経過後に
`internal/trash.Worker` が自動的に最終処分する。

エラー時 (既にゴミ箱状態・存在しない KEY) は stderr のエラーメッセージをそのまま報告する。

### Step 3: ゴミ箱一覧の提示 (FNC-010)

```bash
python3 .claude/skills/manage-db-indexes/scripts/docdb_client.py list-trashed-indexes
```

戻り値 `{"indexes": [{"key", "trashed_at", "remaining_seconds"}, ...]}` を提示する。
`remaining_seconds` は読みやすい形式 (時間・日数) に変換して見せる
(例: `259200` → `約3日`)。

`indexes` が空の場合は「ゴミ箱は空です」と報告する。

orphan record (`sync_documents` で参照されなくなった record 単位のゴミ箱) はこの一覧に
含まれない。orphan record はシステムが自動管理し、同一内容の再 `sync_documents` で自動
復活するため、本 SKILL のユーザー操作対象ではない (FNC-013)。

### Step 4: 復活 (FNC-011)

ユーザーが Step 3 の一覧から復活させたい KEY を選択したら実行する:

```bash
python3 .claude/skills/manage-db-indexes/scripts/docdb_client.py restore-index --key "<KEY>"
```

戻り値 `{"key", "restored": true}` を報告する。復活操作に確認は不要
(ゴミ箱から取り出すだけであり、データを失う操作ではないため)。

復活した KEY はゴミ箱投入前と同じデータ (record/chunk/embedding) がそのまま利用できる。

エラー時 (ゴミ箱状態でない KEY・存在しない KEY) は stderr のエラーメッセージをそのまま
報告する。

## Notes

- **HTTP 直叩き**: `docdb_client.py` は Python stdlib のみ (urllib) で MCP Streamable
  HTTP を扱う。Claude Code の MCP client 層に依存しない
- **接続失敗**: サーバ未起動時は `python3 docdb_client.py` が exit 1 + stderr に接続
  エラーメッセージ。案内メッセージで `doc-db &` を提示 (ログはサーバー自身が
  `~/.doc-db/doc-db.log` に書き込む)
- **削除は必ずゴミ箱を経由する**: `trash_index` は即時物理削除ではない。旧 `delete_index`
  (即時物理削除) は廃止されており、本 SKILL からは呼び出さない
- **ゴミ箱状態の KEY への書き込み系操作は拒否される**: `upsert_documents` /
  `sync_documents` / `delete_documents` / `delete_series` / `schedule_delete_series` は
  ゴミ箱状態の KEY に対してエラーを返す。復活 (Step 4) してから操作する必要がある
- **ゴミ箱状態の KEY への `query` は明示エラー**: 空結果ではなく「ゴミ箱に入っている」
  旨のエラーになる
- **判定はしない**: 本 SKILL および doc-db 自体は「削除すべきか」を一切判定しない。
  Step 1/3 で提示する情報は事実のみであり、削除・復活の意思決定はユーザーが行う
- **自動最終処分**: ゴミ箱投入から `trash.retention_days` (デフォルト 3 日) 経過後、
  `internal/trash.Worker` が自動的に物理削除する。これを取り消したい場合は保持期間内に
  Step 4 (復活) を実行する
