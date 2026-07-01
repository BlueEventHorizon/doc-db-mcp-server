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
doc-db MCP サーバー (`mcp__doc-db__query`) へ転送する。doc-db が未接続の場合は grep 簡易検索へ
フォールバックする。

他のプロジェクトに `.claude/skills/query-db-specs/` をコピーすればそのまま動く。

## Procedure

### A. doc-db MCP が利用可能な場合 (推奨パス)

`mcp__doc-db__query` が available tools に存在する場合、以下を実行する。

**Step A-1: KEY の決定**

```bash
python3 .claude/skills/query-db-specs/scripts/resolve_docs.py --type specs
```

出力 JSON の `project_name` を取得し、**KEY = `<project_name>-specs`** とする。

**Step A-2: doc-db に検索リクエスト**

```
mcp__doc-db__query({
  "key": "<project_name>-specs",
  "query": "$ARGUMENTS",   // ユーザーが渡した検索クエリ
  "mode": "all",           // 3 signal 並列 (emb + lex + grep) で over-recall
  "top_n": 20              // Layer 2 (この SKILL 呼び出し元 AI agent) が本文で判定するため多めに
})
```

**Step A-3: 結果の整形**

戻り値 `results[*]` から以下を抽出:
- `path`
- `origin_signals` (どの signal でヒットしたか - 複数 signal 一致は信頼度高)
- `heading_path` (どの章か)

`warnings` が空でなければ必ず含めて報告する (silent failure 禁止方針)。

KEY が存在しないエラーの場合は「/update-db-specs を先に実行してください」と案内。

### B. doc-db MCP が未接続の場合 (grep フォールバック)

**Step B-1: 警告 [MANDATORY]**

応答の冒頭に必ず以下を出す:

```
⚠️ doc-db MCP サーバーがこのプロジェクトに登録されていません。grep 簡易検索にフォールバック
   しました。優先度付き高精度検索を有効にするには `/update-db-specs` の Step 1 に記載の
   インストール手順を実行してください。
```

**Step B-2: 対象パスの解決**

```bash
python3 .claude/skills/query-db-specs/scripts/resolve_docs.py --type specs
```

`status: "error"` なら `message` を報告して終了。`count == 0` なら「specs 対象文書がありません」
と報告して終了。

**Step B-3: 検索語の類義語展開 [MANDATORY]**

grep は表記一致しないとヒットしないため、`$ARGUMENTS` から抽出した検索語ごとに **類義語・関連語** を
展開してから検索する:

- 日英対訳 (例: 「バージョン」↔ `version`、「レビュー」↔ `review`、「権限」↔ `permission`)
- 略語・正式名称 (例: `req` ↔ `requirements`、`CI` ↔ continuous integration)
- 表記ゆれ・活用 (例: `index` / `indexing` / 索引、`config` / configuration / 設定)
- 同義・上位下位概念

**Step B-4: grep 検索**

展開した語を `Grep` ツール (`-i` 相当の大小無視、`|` で連結した正規表現も可) で
Step B-2 の対象ファイル群に横断適用。マッチ語の種類数・出現数が多い順に並べる。
判断に迷う候補は `Read` で実体確認。

## Output Format

冒頭は `Required documents:` 形式 (fallback 時は Step B-1 の警告を先に出してから):

```
Required documents:

- docs/specs/xxx/design/foo.md   [origin_signals: emb, grep]
- docs/specs/xxx/requirements/bar.md   [origin_signals: emb]
```

`origin_signals` は doc-db パスのときのみ表示。grep fallback 時は省略。

## Notes

- **PHIL-01 二層アーキ**: doc-db は「取りこぼし無き候補プール」を返す設計。この SKILL の
  呼び出し元 (親 Claude / AI agent) が本文を読んで最終判断する想定。よって top_n=20 と
  多めに取る。
- **mode の判断**: 通常は `"all"` で十分。特定の ID (例: FNC-001) を厳密に検索したい場合は
  `"grep"` に切り替える判断もあり (ただし argument-hint に含めない YAGNI)。
- **key の意味**: `<project_name>-specs` は SKILL 側の命名規則。doc-db は opaque な文字列
  として扱う。
