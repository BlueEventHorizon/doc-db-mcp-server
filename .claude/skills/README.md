# doc-db 用 doc-search SKILLs（参考実装）

このディレクトリの 5 SKILL は、[doc-db MCP サーバー](https://github.com/BlueEventHorizon/doc-db-mcp-server)
をプロジェクトの文書検索基盤として使うための **Claude Code 用クライアント参考実装**です。
「プロジェクトの文書一覧を `.doc_structure.yaml` で定義 → doc-db へ同期 → 自然言語で検索」
という運用を、**Python 3.9+ stdlib のみ**で実現します。

**doc-db サーバの HTTP エンドポイント (`http://localhost:<port>/mcp`) を直接叩く**ため、
Claude Code 側の MCP 登録は**不要**です（doc-db を MCP 登録して使うこともできますが、
これらの SKILL は登録なしで完結します）。

| SKILL                      | 目的                                                                                               |
| -------------------------- | -------------------------------------------------------------------------------------------------- |
| `/update-db-specs`         | `.doc_structure.yaml` の specs 対象文書を desired-state 同期 (追加・更新・**削除に追従**。v0.2.0+) |
| `/update-db-rules`         | 同 rules 対象文書を desired-state 同期                                                             |
| `/query-db-specs`          | specs 対象文書を doc-db で検索 (未起動時は grep フォールバック)                                    |
| `/query-db-rules`          | rules 対象文書を doc-db で検索 (同上)                                                              |
| `/delete-db-series <name>` | 指定 series (Git branch 等) を specs/rules 両 KEY から一括除去 (branch cleanup)                    |

## 他プロジェクトへの配布

`.claude/skills/` 配下の 5 ディレクトリを **そのまま丸ごとコピー** すれば別プロジェクトでも動作する:

```bash
# コピー先プロジェクトのルートで
rsync -av <src>/.claude/skills/{update,query}-db-{rules,specs}/ \
          <src>/.claude/skills/delete-db-series/ .claude/skills/
```

前提:

1. コピー先プロジェクトのルートに `.doc_structure.yaml` が存在すること (下記の書式)
2. `python3` (3.9 以上) が利用可能なこと。**追加依存なし** (stdlib のみで動作)
3. doc-db サーバ **v0.2.0+** がローカルに稼働していること (下記セットアップ)

## `.doc_structure.yaml` の書式

「どのディレクトリの Markdown を specs / rules として扱うか」をプロジェクトルートの
`.doc_structure.yaml` で定義する。最小構成:

```yaml
# .doc_structure.yaml (プロジェクトルートに置く)
# rules: このディレクトリ配下の **/*.md をルール文書として扱う
rules:
  root_dirs:
    - docs/rules/
  patterns:
    exclude: []

# specs: root_dirs は glob パターン可 (** で再帰マッチ)。
# exclude はディレクトリ名 / パス断片の部分一致で除外する
specs:
  root_dirs:
    - "docs/specs/**/design/"
    - "docs/specs/**/requirements/"
  patterns:
    exclude: [plan]
```

- 同梱の `resolve_docs.py` が読むのは各セクションの **`root_dirs`**（ディレクトリ or
  glob パターン。配下の `*.md` を再帰列挙）と **`patterns.exclude`**（ディレクトリ名 or
  パス断片の部分一致で除外）の 2 キーのみ。他のキーが書かれていても無視される
  （[forge](https://github.com/BlueEventHorizon/bw-cc-plugins) の `.doc_structure.yaml`
  v3.0 と互換で、forge 利用時は `/forge:setup-doc-structure` で対話生成できる。
  forge がなくても上記を手書きすれば足りる）
- パーサーは stdlib のみの行ベース実装（PyYAML 不要）。**コメントは行頭 `#` のみ対応で、
  値の後ろの行内コメント（`- docs/rules/ # メモ` 等）は書けない**（値の一部として
  解釈され、対象が解決できなくなる）。挙動の正本は `*/scripts/resolve_docs.py`

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
doc-db &
```

**ログはサーバー自身が `~/.doc-db/doc-db.log` に書き込む** (v0.1.12+)。従来のような
シェルリダイレクト (`doc-db > /tmp/doc-db.log 2>&1 &`) は不要。実際のログ/DB パスは
`doc-db --show-config`（または本体リポジトリの `make show-log` / `make show-config`）
で確認できる。

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
doc-db に project-specs KEY で全 specs 文書を desired-state 同期

/query-db-specs "RRF スコア融合の設計理由"
  ↓
doc-db から関連 chunk を取得 → 親 Claude が本文で最終判定
```

以降 specs 文書を追加・改訂・**削除**したら再度 `/update-db-specs` を実行する。
同期は desired-state 方式 (doc-db v0.2.0+ の `sync_documents`) なので:

- 同一ハッシュの文書は skip され、embedding コストは差分のみ (DIF-02)
- **削除・リネームされたファイルは自動で series から切り離される**
  (個別の delete 呼び出しは不要。切り離された実体は次回サーバー起動時に物理削除)

branch を削除した後は `/delete-db-series <branch 名>` で cleanup する。

## トラブルシューティング

| 症状                                 | 原因                 | 対処                                                |
| ------------------------------------ | -------------------- | --------------------------------------------------- |
| `doc-db サーバに接続できません`      | サーバ未起動         | `doc-db &` で起動                                   |
| `sync_documents は doc-db v0.2.0+`   | サーバが旧バージョン | `brew upgrade doc-db` してサーバ再起動              |
| `key "xxx" が存在しません`           | まだ同期していない   | 先に `/update-db-{specs,rules}` を実行              |
| `.doc_structure.yaml が存在しません` | 未作成               | 上記書式で手書きするか `/forge:setup-doc-structure` |

## 内部設計（独自 SKILL / クライアントを作る際の参考）

各スクリプトは「文書一覧の解決」と「MCP 通信」を分離してあり、独自クライアントの
雛形として流用できる:

- `resolve_docs.py` (各 SKILL 配下 `scripts/` に同一コピー) — `.doc_structure.yaml` から
  対象 Markdown を列挙する共通スクリプト。**stdlib のみ**の行ベース YAML parser を内蔵し、
  project-root 相対パス + 絶対パス + `project_name` + `git_branch` を JSON 出力
- `docdb_client.py` (同じく同一コピー) — MCP Streamable HTTP を **stdlib のみ (urllib)**
  で扱う軽量クライアント。`~/.doc-db/doc-db.yaml` から port を抽出し、
  `initialize → notifications/initialized → tools/call` の handshake を発行する。
  サブコマンド: `query` / `sync` (v0.2.0+ 推奨、削除追従 + 完了ポーリング) /
  `upsert` `upsert-batch` (旧方式) / `delete-series`
- `run_sync.py` (update-db-{specs,rules} 配下) — 「resolve → sync_documents 投入 →
  get_sync_status ポーリング」を 1 コマンドに統合したラッパー。**独自 SKILL を作る場合は
  この組み立て（一覧解決 → sync → ポーリング）が基本形**。詳細は
  [AI 統合ガイドの「独自クライアント / SKILL を作る」](../../docs/AI_INTEGRATION_GUIDE.md)を参照
- forge に持っていく際は forge の resolver / client に統合する想定 (現状は本プロジェクト
  内で自己完結)

## 関連

- [doc-db AI 統合ガイド](../../docs/AI_INTEGRATION_GUIDE.md) — mode 選び方・origin_signals 解釈・ベストプラクティス
- [doc-db 設計書 (DES-001)](../../docs/specs/base/design/DES-001_doc_db_mcp_server_design.md) — 内部設計
