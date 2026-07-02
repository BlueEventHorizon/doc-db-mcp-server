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

`.doc_structure.yaml` の `specs` セクションで定義される Markdown 群を doc-db サーバに
upsert して検索可能にするラッパー。**doc-db サーバの HTTP エンドポイント
(`http://localhost:<port>/mcp`) を直接叩く**ため、Claude Code の MCP 登録は不要。

他のプロジェクトに `.claude/skills/update-db-specs/` をコピーすればそのまま動く。

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

起動後、/update-db-specs をもう一度実行してください。
```

**Claude Code への MCP 登録は不要** (この SKILL は HTTP を直接叩くため)。

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

- **KEY**: `<project_name>-specs`\
  例: doc-db-mcp-server プロジェクトなら `doc-db-mcp-server-specs`\
  複数プロジェクトで doc-db サーバを共有しても KEY が衝突しない
- **series**: `<git_branch>` (Step 2 の JSON の `git_branch` 値)\
  例: main branch なら `series="main"`、feature/auth branch なら `series="feature/auth"`\
  Git repo 外 / detached HEAD / git 不在時は fallback `"main"` を使う\
  **同一 path でも branch が違えば別 series として管理される**\
  同一内容 (SHA-256 一致) なら embedding は共有される (DIF-02)

### Step 4: local_path 経由で upsert (呼び出し側でバッチループ) [MANDATORY]

**doc-db にファイル内容を送らず、絶対パスだけを渡してサーバ側で読ませる**
(payload 削減)。`docdb_client.py` は `~/.doc-db/doc-db.yaml` の port を自動取得し、
MCP handshake (initialize → notifications/initialized → tools/call) を内部で行う。

> ⚠️ **`upsert` サブコマンドを直接呼ばないこと**。`upsert` は全バッチを 1 プロセス内で
> 連続実行するため、大量ファイル (目安 200+) では Claude Code の Bash tool のデフォルト
> timeout (2分) を超えて `Command timed out` になる。timeout はこのプロセスの外側
> (Claude Code harness) の制約であり、スクリプト内から変更できない。
>
> **必ず `upsert-batch` を使い、SKILL 実行者 (この AI) がバッチ単位でループする。**
> 1 回の呼び出しは 30 件・数十秒で完了するため、timeout に依存せず確実に動作する。

**Step 4-1: entries を取得し、バッチ数を計算する**

```bash
python3 .claude/skills/update-db-specs/scripts/resolve_docs.py --type specs \
    | python3 -c "import json,sys; d=json.load(sys.stdin); print(json.dumps(d['entries']))" \
    > /tmp/docdb_specs_entries.json

python3 -c "
import json
entries = json.load(open('/tmp/docdb_specs_entries.json'))
total = len(entries)
batch = 30
print(f'total={total} batches={(total + batch - 1)//batch}')
"
```

**Step 4-2: バッチごとに `upsert-batch` を呼び、結果を集約する [MANDATORY]**

`total` 件を 30 件区切りで `offset` を進めながら **1 バッチ = 1 Bash 呼び出し**で処理する。
AI は以下をループし、**各バッチ完了時にユーザーへ進捗を報告する**
(例: `[60/480] processed=2 skipped=28 failed=0`):

```bash
python3 .claude/skills/update-db-specs/scripts/docdb_client.py upsert-batch \
    --key "<project_name>-specs" \
    --series "<git_branch>" \
    --entries-json "$(cat /tmp/docdb_specs_entries.json)" \
    --offset <0, 30, 60, ...> \
    --limit 30
```

各呼び出しの stdout は 1 バッチ分の JSON:

```json
{
  "offset": 0, "limit": 30, "total": 480, "batch_count": 30,
  "processed": 2, "skipped": 28, "failed": 0, "errors": [], "warnings": []
}
```

AI はこれを **バッチごとに `processed` / `skipped` / `failed` / `errors` / `warnings` へ加算**し、
全バッチ完了後に Step 5 で合計を報告する。

**接続失敗時** (exit 1 + stderr): Step 1 の案内を提示してループを中断する。
1 バッチが失敗 (exit 2, `failed > 0`) しても **ループは継続**し、失敗バッチの
`errors[]` を集約結果に含める。

**注**: doc-db は SHA-256 ハッシュで変更を検出し、同一内容の再 embedding をスキップする
(DIF-02)。毎回全ファイルを送っても課金は「変更されたファイル分の embedding」のみ。

### Step 5: 完了レポート

Step 4-2 で集約した合計値を報告する。

```
✓ doc-db インデックス更新完了
  KEY: doc-db-mcp-server-specs
  total:     480 (16 batches)
  processed: 300 (新規・内容変更)
  skipped:   180 (同一ハッシュで embedding 再利用)
  failed:    0
```

warnings や errors がある場合は必ず含めて報告する (silent failure 禁止方針)。

## Notes

- **HTTP 直叩き**: `docdb_client.py` は Python stdlib のみ (urllib) で MCP Streamable
  HTTP を扱う。Claude Code の MCP client 層に依存しない
- **desired-state 動作**: doc-db 側には「消えたファイルの自動 orphan cleanup」は無い。
  ファイルを削除した場合は別途 `delete_documents` を呼び出すか、KEY 全体を作り直す
- **branch 削除時の series 撤去**: feature branch を削除した後、その series の
  record は残り続ける。`/delete-db-series <series 名>` (v0.1.9+) で specs / rules 両
  KEY から一括除去できる。`manage_index` の TTL 短縮で自然に消えるのを待つ選択肢もあり
- **KEY の TTL/max_chunks**: doc-db のデフォルト (30 days / 10000 chunks) が適用される。
  長期保持したい場合は `manage_index` 相当の呼び出しが必要 (現時点で本 SKILL は対応せず)
