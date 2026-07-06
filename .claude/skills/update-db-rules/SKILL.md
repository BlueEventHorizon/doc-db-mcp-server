---
name: update-db-rules
description: |
  ルール文書の追加・改訂後に doc-db 検索インデックスを最新化する。
  新しいルール文書を /query-db-rules で検索可能にしたい時に実行する。
  トリガー: "ルール検索インデックス更新", "rules インデックス再構築"
user-invocable: true
argument-hint: ""
allowed-tools: Read, Bash
---

`.doc_structure.yaml` の `rules` セクションで定義される Markdown 群を doc-db サーバに
**desired-state 同期 (`sync_documents`、v0.2.0+)** して検索可能にするラッパー。
追加・更新に加えて**削除されたファイルにも追従する**（一覧に無い既存 path は series
から即時に切り離される）。**doc-db サーバの HTTP エンドポイント
(`http://localhost:<port>/mcp`) を直接叩く**ため、Claude Code の MCP 登録は不要。

他のプロジェクトに `.claude/skills/update-db-rules/` をコピーすればそのまま動く。

## Procedure

### Step 1: doc-db サーバ起動確認 [MANDATORY]

doc-db サーバが起動していなければ以下を提示して終了する。実際の起動確認は Step 3 の
`run_sync.py` 実行時に接続失敗 (exit 1 + stderr) で判定する。
サーバが v0.2.0 未満の場合も exit 1 になり、stderr に `brew upgrade doc-db` の案内が出る。

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
       # ログはサーバー自身が ~/.doc-db/doc-db.log に書き込む (v0.1.12+)。
       # 実際のログ/DB パスは `doc-db --show-config` で確認できる。

起動後、/update-db-rules をもう一度実行してください。
```

**Claude Code への MCP 登録は不要** (この SKILL は HTTP を直接叩くため)。

### Step 2: 対象文書数の確認 (任意)

初回投入など Embedding が大量に走る見込みかどうかで Step 3 の Bash timeout 指定を
変えるため、先に件数だけ把握しておく:

```bash
python3 .claude/skills/update-db-rules/scripts/resolve_docs.py --type rules \
    | python3 -c "import json,sys; print(json.load(sys.stdin)['count'])"
```

`status: "error"` が返る場合はエラー内容を報告して終了。`0` の場合は
「rules 対象文書がありません。`.doc_structure.yaml` を確認してください」と報告して終了。

### Step 3: `run_sync.py` で resolve〜sync を一括実行 [MANDATORY]

`run_sync.py` が「対象文書列挙 → KEY/series 自動決定 → 一覧全体を desired-state
として `sync_documents` に 1 回投入 → `get_sync_status` を完了までポーリング」を
**1 プロセス内で完結**させる。バッチ分割・offset ループは不要
（local_path 経路のため投入 payload は小さく、Embedding はサーバー側ジョブで進む）。

KEY・series は省略時に自動決定される (KEY: `<project_name>-rules`、
series: 現在の git branch。git 不在等は `main`)。手動指定したい場合のみ
`--key` / `--series` を渡す。

```bash
python3 .claude/skills/update-db-rules/scripts/run_sync.py --type rules
```

2 回目以降の同期は大半が skip (hash 一致) なので数秒で終わる。**初回投入や大量変更**
(目安 200+ ファイルの新規 Embedding) は完了まで時間がかかるため、Bash tool の
`timeout` パラメータで最大 600000 (10分) を指定するか、`run_in_background: true` で
実行し完了通知を待つ。ポーリングが `--wait` (デフォルト 600s) を超えても、ジョブは
サーバー側で継続しており再実行すれば冪等に収束する (status="timeout" で報告される)。

stdout は最終ジョブ状態 JSON、stderr には進捗
(例: `[running] processed=2 skipped=88 failed=0 detached=1`) が出る。

**接続失敗・サーバが v0.2.0 未満** (exit 1): Step 1 の案内を提示して終了する。
ジョブ失敗・一部ドキュメント失敗 (exit 2) は stdout の `errors[]` を含めて
Step 4 で報告する（失敗 path の削除予約は保持され、次回 sync で自己修復される）。

**注**: doc-db は SHA-256 ハッシュで変更を検出し、同一内容の再 embedding をスキップする
(DIF-02)。毎回全ファイルを送っても課金は「変更されたファイル分の embedding」のみ。

### Step 4: 完了レポート

Step 3 の stdout JSON をそのまま報告する (processed / skipped /
detached=`deleted_paths_marked` / failed)。warnings や errors がある場合は必ず含めて
報告する (silent failure 禁止方針)。

## Notes

- **HTTP 直叩き**: `docdb_client.py` は Python stdlib のみ (urllib) で MCP Streamable
  HTTP を扱う。Claude Code の MCP client 層に依存しない
- **desired-state 同期 (v0.2.0+)**: `sync_documents` は渡した一覧を「完全な現在状態」と
  みなすため、**削除・リネームされたファイルは自動で series から切り離される**
  (`delete_documents` の個別呼び出しは不要)。切り離された record の物理削除は次回
  サーバー起動時に行われ、それまでは同一内容の再 sync で課金なしに復元できる (自己修復)
- **旧方式**: `run_upsert.py` (upsert バッチ方式、削除に追従しない) も残置してあるが、
  doc-db v0.2.0 未満のサーバに対する後方互換のためのみ。通常は `run_sync.py` を使う
- **branch 削除時の series 撤去**: feature branch を削除した後、その series の
  record は残り続ける。`/delete-db-series <series 名>` (v0.1.9+) で specs / rules 両
  KEY から一括除去できる
- **KEY の TTL/max_chunks**: doc-db のデフォルト (30 days / 10000 chunks) が適用される
