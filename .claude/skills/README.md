# doc-db 用 doc-search SKILLs

このディレクトリの 5 SKILL は、forge/doc-advisor の `query-db-{rules,specs}` /
`update-db-{rules,specs}` と **同じ働き** をするが、バックエンドが
[doc-db MCP サーバー](https://github.com/BlueEventHorizon/doc-db-mcp-server) になっている
ものです。**doc-db サーバの HTTP エンドポイント (`http://localhost:<port>/mcp`) を
直接叩く**ため、Claude Code 側の MCP 登録は**不要**です。

| SKILL                      | 目的                                                                            |
| -------------------------- | ------------------------------------------------------------------------------- |
| `/update-db-specs`         | `.doc_structure.yaml` の specs 対象文書を doc-db に登録 (embedding 更新)        |
| `/update-db-rules`         | 同 rules 対象文書を doc-db に登録                                               |
| `/query-db-specs`          | specs 対象文書を doc-db で検索 (未起動時は grep フォールバック)                 |
| `/query-db-rules`          | rules 対象文書を doc-db で検索 (同上)                                           |
| `/delete-db-series <name>` | 指定 series (Git branch 等) を specs/rules 両 KEY から一括除去 (branch cleanup) |

## 他プロジェクトへの配布

`.claude/skills/` 配下の 5 ディレクトリを **そのまま丸ごとコピー** すれば別プロジェクトでも動作する:

```bash
# コピー先プロジェクトのルートで
rsync -av <src>/.claude/skills/{update,query}-db-{rules,specs}/ \
          <src>/.claude/skills/delete-db-series/ .claude/skills/
```

前提:

1. コピー先プロジェクトに `.doc_structure.yaml` が存在すること (`/forge:setup-doc-structure` で生成)
2. `python3` (3.9 以上) が利用可能なこと。**追加依存なし** (stdlib のみで動作)
3. doc-db サーバがローカルに稼働していること (下記セットアップ)

## doc-db サーバのセットアップ

各 SKILL の Step 1 に記載してあるが、以下 4 ステップで完了する
(Claude Code への MCP 登録は不要):

```bash
# 1. サーバインストール (未実施の場合のみ)
brew tap blueeventhorizon/doc-db https://github.com/BlueEventHorizon/doc-db-mcp-server
brew install blueeventhorizon/doc-db/doc-db

# 2. 設定ファイル配置
mkdir -p ~/.doc-db
cp /opt/homebrew/opt/doc-db/share/doc-db/doc-db.yaml.example ~/.doc-db/doc-db.yaml

# 3. API キー export
export OPENAI_API_DOCDB_KEY=sk-...

# 4. サーバ起動 (別ターミナル or launchd)
doc-db > /tmp/doc-db.log 2>&1 &
```

これだけで各 SKILL の `docdb_client.py` が `http://localhost:58080/mcp` に対して
MCP handshake (initialize → notifications/initialized → tools/call) を発行し
サーバと通信する。

## KEY / series 命名規則

各 SKILL は以下の自動命名を採用する:

- **KEY**: `<project-dir-basename>-<specs|rules>`\
  例: `/Users/moons/data/dev/myrepo` から呼び出せば KEY は `myrepo-specs`。
  doc-db を複数プロジェクト間で共有しても KEY 衝突しない
- **series**: `<current-git-branch>` (`git rev-parse --abbrev-ref HEAD`)\
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

branch を削除した後は `/delete-db-series <branch 名>` で cleanup する。

## トラブルシューティング

| 症状                                 | 原因                     | 対処                                     |
| ------------------------------------ | ------------------------ | ---------------------------------------- |
| `doc-db サーバに接続できません`      | サーバ未起動             | `doc-db > /tmp/doc-db.log 2>&1 &` で起動 |
| `key "xxx" が存在しません`           | まだ upsert していない   | 先に `/update-db-{specs,rules}` を実行   |
| `.doc_structure.yaml が存在しません` | プロジェクト初期化未完了 | `/forge:setup-doc-structure` を実行      |

## 内部設計

- `resolve_docs.py` (各 SKILL 配下 `scripts/` に同一コピー) — `.doc_structure.yaml` から
  対象 Markdown を列挙する共通スクリプト。**stdlib のみ** (forge の
  `resolve_doc_structure.py::parse_config` 互換の行ベース YAML parser を内蔵) で
  project-root 相対パス + 絶対パス + `project_name` + `git_branch` を JSON 出力
- `docdb_client.py` (同じく各 SKILL 配下 `scripts/` に同一コピー) — MCP Streamable HTTP
  を **stdlib のみ (urllib)** で扱う軽量クライアント。`~/.doc-db/doc-db.yaml` から port を
  抽出し、`initialize → notifications/initialized → tools/call` を発行する
- forge に持っていく際は forge の resolver / client に統合する想定 (現状は本プロジェクト
  内で自己完結)

## 関連

- [doc-db AI 統合ガイド](../../docs/AI_INTEGRATION_GUIDE.md) — mode 選び方・origin_signals 解釈・ベストプラクティス
- [doc-db 設計書 (DES-001)](../../docs/specs/base/design/DES-001_doc_db_mcp_server_design.md) — 内部設計
