---
name: update-db-specs
description: |
  仕様文書 (要件定義書・設計書・計画書) の追加・改訂後に doc-db 検索インデックスを最新化する。
  新しい仕様文書を /query-db-specs で検索可能にしたい時に実行する。
  トリガー: "仕様検索インデックス更新", "specs インデックス再構築"
user-invocable: true
argument-hint: ""
allowed-tools: Read, Bash
---

`.doc_structure.yaml` の `specs` セクションで定義される Markdown 群を doc-db MCP サーバーに
upsert して検索可能にするラッパー。

このプロジェクトの forge / doc-advisor 版と同じ目的だが、バックエンドが **doc-db MCP サーバー**
(https://github.com/BlueEventHorizon/doc-db-mcp-server) になっている。他のプロジェクトに
`.claude/skills/update-db-specs/` をコピーすればそのまま動く。

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

登録後、Claude Code を再起動してから /update-db-specs をもう一度実行してください。
```

### Step 2: 対象文書の列挙

```bash
python3 .claude/skills/update-db-specs/scripts/resolve_docs.py --type specs
```

stdout の JSON を parse:
- `status: "error"` の場合は `message` を報告して終了
- `entries` (相対 path + 絶対 local_path のオブジェクト配列) を取り出す
- `project_name` (プロジェクト ディレクトリ名) を KEY prefix として使う
- `git_branch` (現在の Git branch 名) を series として使う
- `count == 0` の場合は「specs 対象文書がありません。`.doc_structure.yaml` を確認してください」と報告して終了

### Step 3: KEY と series の決定

- **KEY**: `<project_name>-specs`  
  例: doc-db-mcp-server プロジェクトなら `doc-db-mcp-server-specs`  
  複数プロジェクトで doc-db サーバーを共有しても KEY が衝突しない
- **series**: `<git_branch>` (Step 2 の JSON の `git_branch` 値)  
  例: main branch なら `series="main"`、feature/auth branch なら `series="feature/auth"`  
  Git repo 外 / detached HEAD / git 不在時は fallback `"main"` を使う  
  **同一 path でも branch が違えば別 series として管理される**  
  同一内容 (SHA-256 一致) なら embedding は共有される (DIF-02)

### Step 4: local_path 経由で upsert

**doc-db にファイル内容を送らず、絶対パスだけを渡してサーバー側で読ませる**
(ローカル運用時の payload 削減)。Step 2 の JSON から `entries[]` を使う:

```
mcp__doc-db__upsert_documents({
  "key": "<project_name>-specs",
  "series": "<git_branch>",   // Step 2 で取得。main / feature/xxx 等
  "documents": [
    {"path": "docs/specs/xxx/design/foo.md",
     "local_path": "/abs/path/.../foo.md"},
    {"path": "docs/specs/xxx/requirements/bar.md",
     "local_path": "/abs/path/.../bar.md"},
    ...
  ]
})
```

- `path`: 相対パス (search 結果の表示用識別子として保存される)
- `local_path`: 絶対パス (doc-db がディスクから直接読む)

**注**: doc-db は SHA-256 ハッシュで変更を検出し、同一内容の再 embedding をスキップする
(DIF-02)。毎回全ファイルを送っても課金は「変更されたファイル分の embedding」のみ。

大量ファイル (100+) の場合はバッチ分割 (30 件ずつ等) を推奨。ただし local_path 方式は
payload が小さい (パス文字列のみ) ので通常はバッチ不要。

### Step 5: 完了レポート

upsert_documents の戻り値 (processed / skipped / failed / errors) をそのまま報告する。

```
✓ doc-db インデックス更新完了
  KEY: doc-db-mcp-server-specs
  processed: 2 (新規・内容変更)
  skipped:   2 (同一ハッシュで embedding 再利用)
  failed:    0
```

warnings や errors がある場合は必ず含めて報告する (silent failure 禁止方針)。

## Notes

- **desired-state 動作**: doc-db 側には「消えたファイルの自動 orphan cleanup」は無い。
  ファイルを削除した場合は別途 `mcp__doc-db__delete_documents` で明示削除するか、
  KEY 全体を `mcp__doc-db__delete_index` で作り直す。
- **branch 削除時の series 撤去**: feature branch を削除した後、その series の
  record は残り続ける。別途 `mcp__doc-db__delete_documents` の series 単位削除、
  または `manage_index` の TTL 短縮で自然に消えるのを待つ。将来 `/delete-db-series`
  のような専用 SKILL を追加する余地あり (YAGNI で保留)。
- **KEY の TTL/max_chunks**: doc-db のデフォルト (30 days / 10000 chunks) が適用される。
  長期保持したい場合は `mcp__doc-db__manage_index` で override 可能。
