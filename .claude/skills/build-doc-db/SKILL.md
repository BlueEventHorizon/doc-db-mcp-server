---
name: build-doc-db
description: |
  任意の文書 (Markdown / テキスト) を汎用 doc-db インデックス (KEY: generic-docs) に
  格納・蓄積して /query-doc-db で検索可能にする。specs / rules の枠に入らない
  汎用文書・メモ・外部資料・調査ノート等を貯めたいときに使う。格納の取り消し
  (remove) と登録一覧の確認 (list) もこの SKILL で行う。
  トリガー: "文書を格納", "generic-docs に追加", "汎用インデックスに登録", "build doc db"
user-invocable: true
argument-hint: "<file|dir> ..."
allowed-tools: Read, Bash
---

任意のファイル / ディレクトリを doc-db の汎用 KEY (`generic-docs`) に **蓄積** するラッパー。
`.doc_structure.yaml` は使わず、引数で渡された path をそのまま対象にする。
**doc-db サーバの HTTP エンドポイント (`http://localhost:<port>/mcp`) を直接叩く**ため、
Claude Code の MCP 登録は不要。

他のプロジェクトに `.claude/skills/build-doc-db/` をコピーすればそのまま動く
(むしろ KEY・manifest がグローバルなので、どのディレクトリから実行しても同じ格納庫に入る)。

## 設計の要点 (update-db-specs との違い)

- **KEY は固定 `generic-docs`** (プロジェクト横断のグローバル格納庫)。`--key` で上書き可
- **series は固定 `main`**。doc-db の登録 API (`sync_documents`) は series 必須のため、
  branch 版管理をしない汎用文書では固定値を使う (query 側も同じ固定値で検索する)
- **manifest 方式で蓄積**: `sync_documents` は desired-state 同期 (渡した一覧 = 完全な
  現在状態) なので、素朴に使うと呼び出しごとに前回格納分が切り離される。本 SKILL は
  登録済み一覧を `~/.doc-db/skill-manifests/generic-docs.json` に永続化し、毎回
  manifest 全体を sync することで「追加で貯める」を実現する
- **文書識別子は絶対パス** (グローバル格納庫でのプロジェクト間衝突を避けるため)
- **削除追従**: manifest 上のファイルがディスクから消えていれば自動 prune → sync で
  series から切り離される

## Procedure

### Step 1: doc-db サーバ起動確認 [MANDATORY]

実際の起動確認は Step 2 の実行時に接続失敗 (exit 1 + stderr) で判定する。
失敗したら以下を提示して終了する (サーバが v0.2.0 未満の場合も exit 1 になり、
stderr に `brew upgrade doc-db` の案内が出る):

```
⚠️ doc-db サーバが起動していません (http://localhost:<port>/mcp に接続失敗)。

セットアップ手順:
  1. サーバインストール (未実施の場合):
       brew tap blueeventhorizon/doc-db https://github.com/BlueEventHorizon/doc-db-mcp-server
       brew install blueeventhorizon/doc-db/doc-db

  2. 設定ファイル配置:
       mkdir -p ~/.doc-db
       cp /opt/homebrew/opt/doc-db/share/doc-db/doc-db.yaml.example ~/.doc-db/doc-db.yaml

  3. API キー export:
       export OPENAI_API_DOCDB_KEY=sk-...

  4. サーバ起動 (別ターミナル or launchd):
       doc-db &

起動後、/build-doc-db をもう一度実行してください。
```

### Step 2: `run_generic_sync.py add` で格納を投入 [MANDATORY]

`$ARGUMENTS` の file / dir を渡す (dir は `.md` / `.markdown` / `.txt` を再帰列挙、
ファイル直指定は拡張子を問わず対象)。`--start-only` で job_id を即座に受け取る:

```bash
python3 .claude/skills/build-doc-db/scripts/run_generic_sync.py add <path>... --start-only
```

- 引数に path が無い場合は、ユーザーに格納対象を確認してから実行する
- 対象拡張子を変えたい場合のみ `--exts` を付ける (例: `--exts .md,.rst`)

**接続失敗・サーバが v0.2.0 未満** (exit 1): Step 1 の案内を提示して終了する。

### Step 3: `sync-status` をポーリングして進捗を報告 [MANDATORY]

Bash tool の stderr はユーザーに直接見えないため、**AI 自身が `sync-status` を
間隔を空けて繰り返し呼び、そのつどテキストでユーザーに進捗を報告すること**:

```bash
python3 .claude/skills/build-doc-db/scripts/docdb_client.py sync-status --job-id <Step 2 の job_id>
```

- `status: "running"` → `processed`/`skipped`/`failed` を 1 行で報告し、数秒待って再度呼ぶ
  (初回投入や大量文書時は間隔を広げてよい。無音のまま長時間待たせないことが目的)
- `status: "done"` → Step 4 へ
- `status: "failed"` → stdout の `errors[]` を含めて Step 4 で報告する
- ポーリングを打ち切ってもジョブ自体はサーバー側で継続する。後で同じ `job_id` に
  `sync-status` を呼べば確認でき、再実行しても DIF-02 により冪等に収束する

**注**: doc-db は SHA-256 ハッシュで変更を検出し、同一内容の再 embedding をスキップする
(DIF-02)。manifest 全体を毎回送っても課金は「変更されたファイル分の embedding」のみ。

### Step 4: 完了レポート

```
✓ generic-docs へ格納完了 (job_id: xxxx)
  KEY: generic-docs / series: main
  total:     42  (manifest 全体)
  added:     5   (今回新たに manifest へ追加)
  processed: 5   (新規・内容変更で embedding 実行)
  skipped:   37  (同一ハッシュで embedding 再利用)
  pruned:    1   (ディスクから消えたファイルを manifest から除去 → series から切り離し)
  failed:    0
```

warnings や errors がある場合は必ず含めて報告する (silent failure 禁止方針)。

## その他の操作

```bash
# 登録一覧の確認 (通信しない)
python3 .claude/skills/build-doc-db/scripts/run_generic_sync.py list

# 格納の取り消し (dir を渡すと配下全件を除去。残り 0 件になる sync も許可される)
python3 .claude/skills/build-doc-db/scripts/run_generic_sync.py remove <path>... --start-only

# manifest 全体の再同期 (削除ファイルの prune + 内容変更の再 embedding)
python3 .claude/skills/build-doc-db/scripts/run_generic_sync.py resync --start-only
```

remove / resync も Step 3 と同様に `sync-status` をポーリングして進捗を報告する。

## Notes

- **HTTP 直叩き**: `docdb_client.py` は Python stdlib のみ (urllib) で MCP Streamable
  HTTP を扱う。Claude Code の MCP client 層に依存しない
- **manifest とサーバの整合**: manifest の保存は sync 投入の成功後に行う。投入に失敗した
  場合 manifest は更新されず、次回の add / resync で自然に収束する
- **KEY 全体の廃棄**: `generic-docs` を丸ごと消したい場合は `manage-db-indexes` SKILL の
  ゴミ箱投入を使う (manifest ファイルは手動で削除する:
  `rm ~/.doc-db/skill-manifests/generic-docs.json`)
