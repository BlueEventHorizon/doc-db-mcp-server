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
upsert して検索可能にするラッパー。**doc-db サーバの HTTP エンドポイント
(`http://localhost:<port>/mcp`) を直接叩く**ため、Claude Code の MCP 登録は不要。

他のプロジェクトに `.claude/skills/update-db-rules/` をコピーすればそのまま動く。

## Procedure

### Step 1: doc-db サーバ起動確認 [MANDATORY]

doc-db サーバが起動していなければ以下を提示して終了する。実際の起動確認は Step 3 の
`run_upsert.py` 実行時に接続失敗 (exit 1 + stderr) で判定する。

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

大量ファイル (目安 200+) かどうかで Step 3 の Bash timeout 指定を変えるため、
先に件数だけ把握しておく:

```bash
python3 .claude/skills/update-db-rules/scripts/resolve_docs.py --type rules \
    | python3 -c "import json,sys; print(json.load(sys.stdin)['count'])"
```

`status: "error"` が返る場合はエラー内容を報告して終了。`0` の場合は
「rules 対象文書がありません。`.doc_structure.yaml` を確認してください」と報告して終了。

### Step 3: `run_upsert.py` で resolve〜upsert を一括実行 [MANDATORY]

`run_upsert.py` が「対象文書列挙 → KEY/series 自動決定 → local_path 経由の
upsert をバッチ処理 → 結果集約」までを **1 プロセス内で完結**させる。
AI 側でバッチ数計算・offset ループ・中間 JSON ファイルの用意は不要。

KEY・series は省略時に自動決定される (KEY: `<project_name>-rules`、
series: 現在の git branch。git 不在等は `main`)。手動指定したい場合のみ
`--key` / `--series` を渡す。

```bash
python3 .claude/skills/update-db-rules/scripts/run_upsert.py --type rules
```

Step 2 で件数が **200 を大きく超える場合**は Bash tool のデフォルト timeout (2分) に
収まらない可能性があるため、`timeout` パラメータで最大 600000 (10分) を指定するか、
それでも収まらない見込みなら `run_in_background: true` で実行し完了通知を待つ。

stdout は集約結果 JSON、stderr にはバッチ毎の進捗
(例: `[60/90] processed=2 skipped=28 failed=0`) が出る。

**接続失敗時** (exit 1): Step 1 の案内を提示して終了する。
一部バッチが失敗 (exit 2, `failed > 0`) しても他バッチは処理済みなので、
stdout の `errors[]` を含めて Step 4 で報告する。

**注**: doc-db は SHA-256 ハッシュで変更を検出し、同一内容の再 embedding をスキップする
(DIF-02)。

### Step 4: 完了レポート

Step 3 の stdout JSON をそのまま報告する。warnings や errors がある場合は必ず含めて報告する
(silent failure 禁止方針)。

## Notes

- **HTTP 直叩き**: `docdb_client.py` は Python stdlib のみ (urllib) で MCP Streamable
  HTTP を扱う。Claude Code の MCP client 層に依存しない
- **desired-state 動作**: doc-db 側には「消えたファイルの自動 orphan cleanup」は無い。
  ファイルを削除した場合は別途 `delete_documents` を呼び出すか、KEY 全体を作り直す
- **branch 削除時の series 撤去**: feature branch を削除した後、その series の
  record は残り続ける。`/delete-db-series <series 名>` (v0.1.9+) で specs / rules 両
  KEY から一括除去できる
- **KEY の TTL/max_chunks**: doc-db のデフォルト (30 days / 10000 chunks) が適用される
