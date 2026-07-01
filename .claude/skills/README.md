# doc-db 用 doc-search SKILLs

このディレクトリの 4 SKILL は、forge/doc-advisor の `query-db-{rules,specs}` /
`update-db-{rules,specs}` と **同じ働き** をするが、バックエンドが
[doc-db MCP サーバー](https://github.com/BlueEventHorizon/doc-db-mcp-server) になっている
ものです。

| SKILL | 目的 |
|---|---|
| `/update-db-specs` | `.doc_structure.yaml` の specs 対象文書を doc-db に登録 (embedding 更新) |
| `/update-db-rules` | 同 rules 対象文書を doc-db に登録 |
| `/query-db-specs` | specs 対象文書を doc-db で検索 (未接続時は grep フォールバック) |
| `/query-db-rules` | rules 対象文書を doc-db で検索 (同上) |
| `/delete-db-series <name>` | 指定 series (Git branch 等) を specs/rules 両 KEY から一括除去 (branch cleanup) |

## 他プロジェクトへの配布

`.claude/skills/` 配下の 4 ディレクトリを **そのまま丸ごとコピー** すれば別プロジェクトでも動作する:

```bash
# コピー先プロジェクトのルートで
rsync -av <src>/.claude/skills/{update,query}-db-{rules,specs}/ .claude/skills/
```

前提:
1. コピー先プロジェクトに `.doc_structure.yaml` が存在すること (`/forge:setup-doc-structure` で生成)
2. `python3` + `pyyaml` が利用可能なこと (`pip install pyyaml` or `brew install python-yaml`)
3. doc-db MCP サーバーがローカルに稼働 + Claude Code に登録されていること (下記手順)

## doc-db MCP サーバーのセットアップ

各 SKILL の Step 1 に記載してあるが、以下 5 ステップで完了する:

```bash
# 1. サーバーインストール (未実施の場合のみ)
brew tap blueeventhorizon/doc-db https://github.com/BlueEventHorizon/doc-db-mcp-server
brew install blueeventhorizon/doc-db/doc-db

# 2. 設定ファイル配置
mkdir -p ~/.doc-db
cp /opt/homebrew/opt/doc-db/share/doc-db/doc-db.yaml.example ~/.doc-db/doc-db.yaml

# 3. API キー export
export OPENAI_API_DOCDB_KEY=sk-...

# 4. サーバー起動 (別ターミナル or launchd)
doc-db > /tmp/doc-db.log 2>&1 &

# 5. Claude Code に MCP 登録 (ユーザースコープで 1 回だけ)
claude mcp add --transport http -s user doc-db http://localhost:58080/mcp
```

登録後 Claude Code を再起動すれば `mcp__doc-db__*` tools が使えるようになる。

## KEY / series 命名規則

各 SKILL は以下の自動命名を採用する:

- **KEY**: `<project-dir-basename>-<specs|rules>`  
  例: `/Users/moons/data/dev/myrepo` から呼び出せば KEY は `myrepo-specs`。
  doc-db を複数プロジェクト間で共有しても KEY 衝突しない
- **series**: `<current-git-branch>` (`git rev-parse --abbrev-ref HEAD`)  
  例: main branch なら `series="main"`、`feature/auth` なら `series="feature/auth"`。
  Git repo 外 / detached HEAD の場合は fallback `"main"`

**branch 別インデックスの効果**:
- 同一 path のファイルでも branch が違えば別 series として管理される
- 同一内容 (SHA-256 一致) なら embedding は共有される (doc-db DIF-02)
- query 側はデフォルトで series 指定なし = **KEY 内の全 branch を横断検索** (recall 優先)

## 使用フロー

初回:

```
/update-db-specs
  ↓
doc-db に project-specs KEY で全 specs 文書を登録

/query-db-specs "RRF スコア融合の設計理由"
  ↓
doc-db から関連 chunk を取得 → 親 Claude が本文で最終判定
```

以降 specs 文書を追加・改訂したら再度 `/update-db-specs` を実行する
(同一ハッシュは skip されるので embedding コストは差分のみ)。

## トラブルシューティング

| 症状 | 原因 | 対処 |
|---|---|---|
| `mcp__doc-db__* が見つかりません` | MCP 未登録 or Claude Code 未再起動 | 上記セットアップ手順を実施 → Claude Code 再起動 |
| `pyyaml が必要です` | Python 環境に pyyaml 未インストール | `pip install pyyaml` |
| `key "xxx" が存在しません` | まだ upsert していない | 先に `/update-db-{specs,rules}` を実行 |
| `.doc_structure.yaml が存在しません` | プロジェクト初期化未完了 | `/forge:setup-doc-structure` を実行 |

## 内部設計

- `resolve_docs.py` (各 SKILL 配下 `scripts/` に同一コピー) — `.doc_structure.yaml` から
  対象 Markdown を列挙する共通スクリプト。project-root 相対パス配列を JSON 出力
- forge の resolve_doc_structure.py に相当するが、依存を排除するため各 SKILL に複製配置

## 関連

- [doc-db AI 統合ガイド](../../docs/AI_INTEGRATION_GUIDE.md) — mode 選び方・origin_signals 解釈・ベストプラクティス
- [doc-db 設計書 (DES-001)](../../docs/specs/base/design/DES-001_doc_db_mcp_server_design.md) — 内部設計
