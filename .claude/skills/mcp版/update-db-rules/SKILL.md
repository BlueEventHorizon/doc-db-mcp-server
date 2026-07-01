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

`.doc_structure.yaml` の `rules` セクションで定義される Markdown 群を doc-db MCP サーバーに
upsert して検索可能にするラッパー。

このプロジェクトの forge / doc-advisor 版と同じ目的だが、バックエンドが **doc-db MCP サーバー**
(https://github.com/BlueEventHorizon/doc-db-mcp-server) になっている。他のプロジェクトに
`.claude/skills/update-db-rules/` をコピーすればそのまま動く。

## Procedure

### Step 1: doc-db MCP tool 可用性チェック [MANDATORY]

`mcp__doc-db__upsert_documents` が available tools に存在するか確認する。
存在しない場合は以下のインストール手順を提示して終了する:

```
⚠️ doc-db MCP サーバーがこのプロジェクトに登録されていません。

セットアップ手順:
  1. サーバーインストール (未実施の場合):
       brew tap blueeventhorizon/doc-db https://github.com/BlueEventHorizon/doc-db-mcp-server
       brew install blueeventhorizon/doc-db/doc-db

  2. 設定ファイル配置:
       mkdir -p ~/.doc-db
       cp /opt/homebrew/opt/doc-db/share/doc-db/doc-db.yaml.example ~/.doc-db/doc-db.yaml

  3. API キー export:
       export OPENAI_API_DOCDB_KEY=sk-...

  4. サーバー起動 (別ターミナル or launchd):
       doc-db > /tmp/doc-db.log 2>&1 &

  5. Claude Code に MCP 登録:
       claude mcp add --transport http -s user doc-db http://localhost:58080/mcp

登録後、Claude Code を再起動してから /update-db-rules をもう一度実行してください。
```

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
  複数プロジェクトで doc-db サーバーを共有しても KEY が衝突しない
- **series**: `<git_branch>` (Step 2 の JSON の `git_branch` 値)\
  例: main branch なら `series="main"`、feature/xxx branch なら `series="feature/xxx"`\
  Git repo 外 / detached HEAD / git 不在時は fallback `"main"` を使う\
  **同一 path でも branch が違えば別 series として管理される**\
  同一内容 (SHA-256 一致) なら embedding は共有される (DIF-02)

### Step 4: local_path 経由で upsert

**doc-db にファイル内容を送らず、絶対パスだけを渡してサーバー側で読ませる**
(ローカル運用時の payload 削減)。Step 2 の JSON から `entries[]` を使う:

```
mcp__doc-db__upsert_documents({
  "key": "<project_name>-rules",
  "series": "<git_branch>",   // Step 2 で取得。main / feature/xxx 等
  "documents": [
    {"path": "docs/rules/xxx.md",
     "local_path": "/abs/path/.../xxx.md"},
    ...
  ]
})
```

- `path`: 相対パス (search 結果の表示用識別子として保存される)
- `local_path`: 絶対パス (doc-db がディスクから直接読む)

**注**: doc-db は SHA-256 ハッシュで変更を検出し、同一内容の再 embedding をスキップする
(DIF-02)。毎回全ファイルを送っても課金は「変更されたファイル分の embedding」のみ。

### Step 5: 完了レポート

upsert_documents の戻り値 (processed / skipped / failed / errors) をそのまま報告する。

warnings や errors がある場合は必ず含めて報告する (silent failure 禁止方針)。

## Notes

- **desired-state 動作**: doc-db 側には「消えたファイルの自動 orphan cleanup」は無い。
  ファイルを削除した場合は別途 `mcp__doc-db__delete_documents` で明示削除するか、
  KEY 全体を `mcp__doc-db__delete_index` で作り直す。
- **branch 削除時の series 撤去**: feature branch を削除した後、その series の
  record は残り続ける。`/delete-db-series <series 名>` (v0.1.9+) で specs / rules 両
  KEY から一括除去できる。
- **KEY の TTL/max_chunks**: doc-db のデフォルト (30 days / 10000 chunks) が適用される。
