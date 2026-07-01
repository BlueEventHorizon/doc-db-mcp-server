---
name: query-db-rules
description: |
  プロジェクトのルール文書を、キーワード・機能名・自然文で、高速・高品位に、優先度をつけて検索する。
  設計・実装・レビュー等、開発作業のあらゆる場面でルールを参照したいときに使う。
  トリガー: "ルールを検索", "rules を探す"
user-invocable: true
argument-hint: "task description"
allowed-tools: Read, Grep, Glob, Bash
---

ルール文書 (`.doc_structure.yaml` の `rules` セクション) を検索する read-only ラッパー。
doc-db MCP サーバー (`mcp__doc-db__query`) へ転送する。doc-db が未接続の場合は grep 簡易検索へ
フォールバックする。

他のプロジェクトに `.claude/skills/query-db-rules/` をコピーすればそのまま動く。

## Procedure

### A. doc-db MCP が利用可能な場合 (推奨パス)

`mcp__doc-db__query` が available tools に存在する場合、以下を実行する。

**Step A-1: KEY の決定**

```bash
python3 .claude/skills/query-db-rules/scripts/resolve_docs.py --type rules
```

出力 JSON の `project_name` を取得し、**KEY = `<project_name>-rules`** とする。

**Step A-2: doc-db に検索リクエスト**

デフォルトでは **series 指定なし = KEY 内の全 branch を横断検索** する
(PHIL-01: recall 優先):

```
mcp__doc-db__query({
  "key": "<project_name>-rules",
  "query": "$ARGUMENTS",
  "mode": "all",
  "top_n": 20
})
```

現在の branch のみ検索したい場合は `series=<git_branch>` を追加 (Step A-1 の JSON から
取得可能)。ユーザーが `$ARGUMENTS` に明示的な branch 指定を含めた場合の対応は呼び出し元
AI の判断に委ねる。

**Step A-3: 結果の整形**

戻り値 `results[*]` から以下を抽出:

- `path`
- `origin_signals` (どの signal でヒットしたか)
- `heading_path`

`warnings` が空でなければ必ず含めて報告する。

KEY が存在しないエラーの場合は「/update-db-rules を先に実行してください」と案内。

### B. doc-db MCP が未接続の場合 (grep フォールバック)

**Step B-1: 警告 [MANDATORY]**

```
⚠️ doc-db MCP サーバーがこのプロジェクトに登録されていません。grep 簡易検索にフォールバック
   しました。優先度付き高精度検索を有効にするには `/update-db-rules` の Step 1 に記載の
   インストール手順を実行してください。
```

**Step B-2: 対象パスの解決**

```bash
python3 .claude/skills/query-db-rules/scripts/resolve_docs.py --type rules
```

`status: "error"` なら `message` を報告して終了。`count == 0` なら「rules 対象文書がありません」
と報告して終了。

**Step B-3: 検索語の類義語展開 [MANDATORY]**

grep は表記一致しないとヒットしないため、`$ARGUMENTS` から抽出した検索語ごとに類義語・関連語を
展開してから検索する:

- 日英対訳 (例: 「バージョン」↔ `version`、「レビュー」↔ `review`)
- 略語・正式名称 (例: `req` ↔ `requirements`)
- 表記ゆれ・活用 (例: `index` / `indexing` / 索引)
- 同義・上位下位概念

**Step B-4: grep 検索**

展開した語を `Grep` ツールで Step B-2 の対象ファイル群に横断適用。マッチ語の種類数・出現数が
多い順に並べる。判断に迷う候補は `Read` で実体確認。

## Output Format

冒頭は `Required documents:` 形式:

```
Required documents:

- docs/rules/xxx.md   [origin_signals: emb, grep]
- docs/rules/yyy.md   [origin_signals: emb]
```

`origin_signals` は doc-db パスのときのみ表示。

## Notes

- **PHIL-01 二層アーキ**: doc-db は「取りこぼし無き候補プール」を返す設計。この SKILL の
  呼び出し元 (親 Claude / AI agent) が本文を読んで最終判断する想定。よって top_n=20 と
  多めに取る。
- **key の意味**: `<project_name>-rules` は SKILL 側の命名規則。doc-db は opaque な文字列
  として扱う。
