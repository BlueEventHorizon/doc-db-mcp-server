---
name: query-db-specs
description: |
  プロジェクトの仕様書 (要件定義書・設計書・計画書) を、キーワード・機能名・自然文で
  高速・高品位に、優先度をつけて検索する。設計・実装・レビュー等、開発作業のあらゆる場面で
  仕様を参照したいときに使う。
  トリガー: "仕様を検索", "spec を探す", "設計書を検索"
user-invocable: true
argument-hint: "task description"
allowed-tools: Read, Grep, Glob, Bash
---

仕様文書 (`.doc_structure.yaml` の `specs` セクション) を検索する read-only ラッパー。
**doc-db サーバの HTTP エンドポイント (`http://localhost:<port>/mcp`) を直接叩く**ため、
Claude Code の MCP 登録は不要。サーバ未起動時は grep 簡易検索へフォールバックする。

他のプロジェクトに `.claude/skills/query-db-specs/` をコピーすればそのまま動く。

## Procedure

### Step 1: KEY の決定

```bash
python3 .claude/skills/query-db-specs/scripts/resolve_docs.py --type specs
```

出力 JSON の `project_name` を取得し、**KEY = `<project_name>-specs`** とする。

### Step 2: doc-db に検索リクエスト (推奨パス)

`docdb_client.py` は `~/.doc-db/doc-db.yaml` の port を自動取得し、MCP handshake
(initialize → notifications/initialized → tools/call) を内部で行う。デフォルトは
**series 指定なし = KEY 内の全 branch 横断検索** (PHIL-01: recall 優先):

```bash
python3 .claude/skills/query-db-specs/scripts/docdb_client.py query \
    --key "<project_name>-specs" \
    --query "$ARGUMENTS" \
    --mode all \
    --top-n 20
```

stdout に `{"results": [...], "stage_stats": {...}, "warnings"?: [...]}` の JSON が返る。

現在の branch のみに絞りたい場合は `--series <git_branch>` を追加 (Step 1 の JSON
の `git_branch` を使う)。

**サーバ未起動時**: `docdb_client.py` が exit 1 + stderr に接続エラーメッセージ。
その場合は Step 3 (grep フォールバック) へ。

### Step 3: doc-db サーバ未起動時のフォールバック (grep)

**Step 3-1: 警告 [MANDATORY]**

応答の冒頭に必ず以下を出す:

```
⚠️ doc-db サーバが起動していません (http://localhost:<port>/mcp に接続失敗)。
   grep 簡易検索にフォールバックしました。優先度付き高精度検索を有効にするには
   `/update-db-specs` の Step 1 に記載の起動手順を実行してください。
```

**Step 3-2: 対象パスの解決**

Step 1 の JSON の `entries[]` (相対 path) を対象ファイル群とする。`count == 0` の
場合は「specs 対象文書がありません」と報告して終了。

**Step 3-3: 検索語の類義語展開 [MANDATORY]**

grep は表記一致しないとヒットしないため、`$ARGUMENTS` から抽出した検索語ごとに
**類義語・関連語** を展開してから検索する:

- 日英対訳 (例: 「バージョン」↔ `version`、「レビュー」↔ `review`、「権限」↔ `permission`)
- 略語・正式名称 (例: `req` ↔ `requirements`、`CI` ↔ continuous integration)
- 表記ゆれ・活用 (例: `index` / `indexing` / 索引、`config` / configuration / 設定)
- 同義・上位下位概念

**Step 3-4: grep 検索**

展開した語を `Grep` ツール (`-i` 相当の大小無視、`|` で連結した正規表現も可) で
Step 3-2 の対象ファイル群に横断適用。マッチ語の種類数・出現数が多い順に並べる。
判断に迷う候補は `Read` で実体確認。

### Step 4: 結果の整形

doc-db パス (Step 2) で `results[*]` から以下を抽出:

- `path`
- `origin_signals` (どの signal でヒットしたか - 複数 signal 一致は信頼度高)
- `heading_path` (どの章か)

戻り値の `warnings` が空でなければ必ず含めて報告する (silent failure 禁止方針)。

KEY が存在しないエラー (stderr に該当メッセージ) の場合は「/update-db-specs を先に
実行してください」と案内。

## Output Format

冒頭は `Required documents:` 形式 (fallback 時は Step 3-1 の警告を先に出してから):

```
Required documents:

- docs/specs/xxx/design/foo.md   [origin_signals: emb, grep]
- docs/specs/xxx/requirements/bar.md   [origin_signals: emb]
```

`origin_signals` は doc-db パスのときのみ表示。grep fallback 時は省略。

## Notes

- **HTTP 直叩き**: `docdb_client.py` は Python stdlib のみ (urllib) で MCP Streamable
  HTTP を扱う。Claude Code の MCP client 層に依存しない
- **PHIL-01 二層アーキ**: doc-db は「取りこぼし無き候補プール」を返す設計。この SKILL の
  呼び出し元 (親 Claude / AI agent) が本文を読んで最終判断する想定。よって top_n=20 と
  多めに取る
- **mode の判断**: 通常は `all` で十分。特定の ID (例: FNC-001) を厳密に検索したい場合は
  `--mode grep` に切り替える判断もあり
- **key の意味**: `<project_name>-specs` は SKILL 側の命名規則。doc-db は opaque な文字列
  として扱う
