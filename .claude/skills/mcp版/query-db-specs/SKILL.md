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

出力 JSON から以下を取得する:

- `project_name` → **KEY = `<project_name>-specs`**
- `git_branch` → Step A-3 の `series` に渡す
- `count` — **0 の場合は doc-db を叩かず「specs 対象文書がありません」と報告して終了する**
  (Step A-2 は「未同期」と「同期済みだが 0 件」を区別できないため、後者をここで先に切り分ける)

**Step A-2: series の登録確認 [MANDATORY]**

`mcp__doc-db__list_indexes({})` を呼び、`indexes[]` から当該 KEY のエントリを探して
`series[]` に **Step A-1 の `git_branch` が含まれるか**を確認する。

含まれない場合 (KEY 自体が無い場合も含む) は **検索せず終了**し、
「この branch のインデックスが未作成です。`/update-db-specs` を実行してください」と
報告する。series 無指定の全 branch 横断検索へフォールバックしてはならない
(その branch で削除した文書を提示する経路になる。Notes 参照)。

`series[]` は record に現在紐づく series から作られるため、**「未同期」と「同期済みだが
対象文書 0 件」を区別できない**（`sync_documents` は空リストも正当な desired-state として
受理し、その series は一覧から消える）。後者は Step A-1 の `count == 0` で既に終了して
いるため、ここに到達した時点で未同期と確定する。

**Step A-3: doc-db に検索リクエスト**

**現在の branch の series を必ず指定する** (Step A-1 の `git_branch`):

```
mcp__doc-db__query({
  "key": "<project_name>-specs",
  "series": "<git_branch>",  // 当該 branch の desired-state のみを対象にする
  "query": "$ARGUMENTS",     // ユーザーが渡した検索クエリ
  "mode": "all",             // 3 signal 並列 (emb + lex + grep) で over-recall
  "top_n": 20                // Layer 2 (この SKILL 呼び出し元 AI agent) が本文で判定するため多めに
})
```

**ヒット 0 件のとき**: series 無指定での再検索は **行わない**。当該 series はその branch の
完全な現在状態なので、0 件は「この branch に該当文書が無い」という正しい結果である。

全 branch を横断したいとユーザーが明示した場合のみ `series` を外す。その際は削除済み
文書が混入し得ることを応答に明記する。

**Step A-4: 結果の整形**

戻り値 `results[*]` から以下を抽出:

- `path`
- `origin_signals` (どの signal でヒットしたか - 複数 signal 一致は信頼度高)
- `heading_path` (どの章か)
- `series_keys` — **`series` を外して全 branch 横断した場合のみ必ず表示する**
  (どの branch 由来の文書か判別できないと、現 branch に無い文書を掴む事故になる)

`warnings` が空でなければ必ず含めて報告する (silent failure 禁止方針)。

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
- **series 指定と PHIL-01 の関係**: over-recall は **当該 series の中で** 追求する
  (3 signal 並列 + top_n=20)。series を跨いで recall を広げることはしない。update 側は
  `sync_documents` (desired-state 同期) で登録するため当該 series は「その branch の完全な
  現在状態」であり、series 無指定の全 branch 横断には **sync で切り離された削除済み文書が
  物理削除まで混入し得る** (DES-001 §4.5 / APP-001 SYN-03 の既知の制約)。同一内容の文書は
  DIF-02 により複数 series へ紐づくため、branch 上で `/update-db-specs` を実行済みなら
  series 指定でも取りこぼしは生じない。
- **mode の判断**: 通常は `"all"` で十分。特定の ID (例: FNC-001) を厳密に検索したい場合は
  `"grep"` に切り替える判断もあり (ただし argument-hint に含めない YAGNI)。
- **key の意味**: `<project_name>-specs` は SKILL 側の命名規則。doc-db は opaque な文字列
  として扱う。
