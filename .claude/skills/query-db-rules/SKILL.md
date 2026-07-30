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
**doc-db サーバの HTTP エンドポイント (`http://localhost:<port>/mcp`) を直接叩く**ため、
Claude Code の MCP 登録は不要。サーバ未起動時は grep 簡易検索へフォールバックする。

他のプロジェクトに `.claude/skills/query-db-rules/` をコピーすればそのまま動く。

## Procedure

### Step 1: KEY の決定

```bash
python3 .claude/skills/query-db-rules/scripts/resolve_docs.py --type rules
```

出力 JSON から以下を取得する:

- `project_name` → **KEY = `<project_name>-rules`**
- `git_branch` → Step 2 の `--series` に渡す
- `count` — **0 の場合は doc-db を叩かず「rules 対象文書がありません」と報告して終了する**
  (ローカルに文書が無ければインデックス側にも無い。Step 2 の series 検証は
  「未同期」と「同期済みだが 0 件」を区別できないため、後者をここで先に切り分ける)

### Step 2: doc-db に検索リクエスト (推奨パス)

`docdb_client.py` は `~/.doc-db/doc-db.yaml` の port を自動取得し、MCP handshake
(initialize → notifications/initialized → tools/call) を内部で行う。
**現在の branch の series を必ず指定する** (Step 1 の JSON の `git_branch`):

```bash
python3 .claude/skills/query-db-rules/scripts/docdb_client.py query \
    --key "<project_name>-rules" \
    --series "<git_branch>" \
    --query "$ARGUMENTS" \
    --mode all \
    --top-n 20
```

stdout に `{"results": [...], "stage_stats": {...}, "warnings"?: [...]}` の JSON が返る。

**series を指定する理由**: update 側 (`/update-db-rules`) は `sync_documents`
(desired-state 同期) で登録するため、**当該 series は「その branch の完全な現在状態」**
になる。一方 series 無指定の全 series 横断検索には、他 branch にしか無い文書に加え、
**sync で切り離された削除済み文書が物理削除まで混入し得る**
(DES-001 §4.5 / APP-001 SYN-03 の既知の制約)。

**終了コードの扱い [MANDATORY]**

| exit | 意味                                                           | 対応                                                                                                                                                                                                                                                      |
| ---- | -------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 0    | 検索成功                                                       | Step 4 へ                                                                                                                                                                                                                                                 |
| 3    | 当該 series に検索対象が無い (未同期、または同期済みだが 0 件) | **grep にフォールバックしない**。Step 1 で `count > 0` を確認済みなら未同期と確定するので「この branch のインデックスが未作成です。`/update-db-rules` を実行してください」と報告して終了する。`count == 0` は Step 1 で既に終了しているためここには来ない |
| 1    | サーバ未起動・その他エラー                                     | Step 3 (grep フォールバック) へ                                                                                                                                                                                                                           |

**ヒット 0 件のとき**: series 無指定での再検索は **行わない**。当該 series はその branch の
完全な現在状態なので、0 件は「この branch に該当文書が無い」という正しい結果である。
他 branch の文書で代替すると、その branch で削除した文書を提示してしまう。

全 branch を横断したいとユーザーが明示した場合のみ `--series` を外す。その際は
削除済み文書が混入し得ることを応答に明記する。

### Step 3: doc-db サーバ未起動時のフォールバック (grep)

**Step 3-1: 警告 [MANDATORY]**

```
⚠️ doc-db サーバが起動していません (http://localhost:<port>/mcp に接続失敗)。
   grep 簡易検索にフォールバックしました。優先度付き高精度検索を有効にするには
   `/update-db-rules` の Step 1 に記載の起動手順を実行してください。
```

**Step 3-2: 対象パスの解決**

Step 1 の JSON の `entries[]` (相対 path) を対象ファイル群とする。`count == 0` の
場合は「rules 対象文書がありません」と報告して終了。

**Step 3-3: 検索語の類義語展開 [MANDATORY]**

grep は表記一致しないとヒットしないため、`$ARGUMENTS` から抽出した検索語ごとに
類義語・関連語を展開してから検索する:

- 日英対訳 (例: 「バージョン」↔ `version`、「レビュー」↔ `review`)
- 略語・正式名称 (例: `req` ↔ `requirements`)
- 表記ゆれ・活用 (例: `index` / `indexing` / 索引)
- 同義・上位下位概念

**Step 3-4: grep 検索**

展開した語を `Grep` ツールで Step 3-2 の対象ファイル群に横断適用。マッチ語の種類数・
出現数が多い順に並べる。判断に迷う候補は `Read` で実体確認。

### Step 4: 結果の整形

doc-db パスで `results[*]` から以下を抽出:

- `path`
- `origin_signals` (どの signal でヒットしたか)
- `heading_path`
- `series_keys` — **`--series` を外して全 branch 横断した場合のみ必ず表示する**
  (どの branch 由来の文書か判別できないと、現 branch に無い文書を掴む事故になる)。
  series 指定時は全件が当該 series を含むため省略してよい

`warnings` が空でなければ必ず含めて報告する。

## Output Format

冒頭は `Required documents:` 形式:

```
Required documents:

- docs/rules/xxx.md   [origin_signals: emb, grep]
- docs/rules/yyy.md   [origin_signals: emb]
```

`origin_signals` は doc-db パスのときのみ表示。

## Notes

- **HTTP 直叩き**: `docdb_client.py` は Python stdlib のみ (urllib) で MCP Streamable
  HTTP を扱う。Claude Code の MCP client 層に依存しない
- **PHIL-01 二層アーキ**: doc-db は「取りこぼし無き候補プール」を返す設計。この SKILL の
  呼び出し元 (親 Claude / AI agent) が本文を読んで最終判断する想定。よって top_n=20 と
  多めに取る
- **series 指定と PHIL-01 の関係**: over-recall は **当該 series の中で** 追求する
  (3 signal 並列 + top_n=20)。series を跨いで recall を広げることはしない。branch を
  跨いだ文書は「現在の作業対象ではない別バージョン」であり、recall の対象ではなく
  誤導のもとになる。同一内容の文書は DIF-02 により複数 series へ紐づくため、branch 上で
  `/update-db-rules` を実行済みなら series 指定でも取りこぼしは生じない
- **key の意味**: `<project_name>-rules` は SKILL 側の命名規則。doc-db は opaque な文字列
  として扱う
