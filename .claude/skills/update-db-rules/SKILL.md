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

doc-db サーバが起動していなければ以下を提示して終了する。実際の起動確認は Step 4 の
`docdb_client.py` 実行時に接続失敗 (exit 1 + stderr) で判定する。

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
       doc-db > /tmp/doc-db.log 2>&1 &

起動後、/update-db-rules をもう一度実行してください。
```

**Claude Code への MCP 登録は不要** (この SKILL は HTTP を直接叩くため)。

### Step 2: 対象文書の列挙

```bash
python3 .claude/skills/update-db-rules/scripts/resolve_docs.py --type rules
```

stdout の JSON を parse:

- `status: "error"` の場合は `message` を報告して終了
- `entries` (相対 path + 絶対 local_path のオブジェクト配列) を取り出す
- `project_name` (プロジェクト ディレクトリ名) を KEY prefix として使う
- `git_branch` (現在の Git branch 名) を series として使う
- `count == 0` の場合は「rules 対象文書がありません。`.doc_structure.yaml` を確認してください」と報告して終了

### Step 3: KEY と series の決定

- **KEY**: `<project_name>-rules`\
  例: doc-db-mcp-server プロジェクトなら `doc-db-mcp-server-rules`\
  複数プロジェクトで doc-db サーバを共有しても KEY が衝突しない
- **series**: `<git_branch>` (Step 2 の JSON の `git_branch` 値)\
  例: main branch なら `series="main"`、feature/xxx branch なら `series="feature/xxx"`\
  Git repo 外 / detached HEAD / git 不在時は fallback `"main"` を使う\
  **同一 path でも branch が違えば別 series として管理される**\
  同一内容 (SHA-256 一致) なら embedding は共有される (DIF-02)

### Step 4: local_path 経由で upsert

**doc-db にファイル内容を送らず、絶対パスだけを渡してサーバ側で読ませる**
(payload 削減)。`docdb_client.py` は `~/.doc-db/doc-db.yaml` の port を自動取得し、
MCP handshake (initialize → notifications/initialized → tools/call) を内部で行う。

Step 2 の JSON の `entries[]` をそのまま `--entries-json` に渡す:

```bash
ENTRIES_JSON=$(python3 .claude/skills/update-db-rules/scripts/resolve_docs.py --type rules \
    | python3 -c "import json,sys; d=json.load(sys.stdin); print(json.dumps(d['entries']))")

python3 .claude/skills/update-db-rules/scripts/docdb_client.py upsert \
    --key "<project_name>-rules" \
    --series "<git_branch>" \
    --entries-json "$ENTRIES_JSON"
```

`entries[]` の各要素は `{path, local_path}` (path=相対、local_path=絶対)。

**接続失敗時** (exit 1 + stderr): Step 1 の案内を提示。

**注**: doc-db は SHA-256 ハッシュで変更を検出し、同一内容の再 embedding をスキップする
(DIF-02)。

### Step 5: 完了レポート

stdout の JSON (processed / skipped / failed / errors) をそのまま報告する。
warnings や errors がある場合は必ず含めて報告する (silent failure 禁止方針)。

## Notes

- **HTTP 直叩き**: `docdb_client.py` は Python stdlib のみ (urllib) で MCP Streamable
  HTTP を扱う。Claude Code の MCP client 層に依存しない
- **desired-state 動作**: doc-db 側には「消えたファイルの自動 orphan cleanup」は無い。
  ファイルを削除した場合は別途 `delete_documents` を呼び出すか、KEY 全体を作り直す
- **branch 削除時の series 撤去**: feature branch を削除した後、その series の
  record は残り続ける。`/delete-db-series <series 名>` (v0.1.9+) で specs / rules 両
  KEY から一括除去できる
- **KEY の TTL/max_chunks**: doc-db のデフォルト (30 days / 10000 chunks) が適用される
