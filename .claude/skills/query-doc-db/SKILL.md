---
name: query-doc-db
description: |
  /build-doc-db で格納した汎用文書 (KEY: generic-docs) を、キーワード・機能名・自然文で
  高速・高品位に、優先度をつけて検索する。specs / rules の枠に入らない汎用文書・メモ・
  外部資料を参照したいときに使う。
  トリガー: "汎用文書を検索", "generic-docs を検索", "格納した文書を探す", "query doc db"
user-invocable: true
argument-hint: "task description"
allowed-tools: Read, Grep, Glob, Bash
---

`/build-doc-db` で格納した汎用文書 (KEY: `generic-docs`) を検索する read-only ラッパー。
**doc-db サーバの HTTP エンドポイント (`http://localhost:<port>/mcp`) を直接叩く**ため、
Claude Code の MCP 登録は不要。サーバ未起動時は manifest の文書一覧に対する
grep 簡易検索へフォールバックする。

他のプロジェクトに `.claude/skills/query-doc-db/` をコピーすればそのまま動く
(KEY・manifest がグローバルなので、どのディレクトリから実行しても同じ格納庫を検索する)。

## Procedure

### Step 1: manifest の確認

```bash
python3 .claude/skills/query-doc-db/scripts/show_manifest.py
```

- `count == 0` → **doc-db を叩かず**「generic-docs にはまだ文書が格納されていません。
  `/build-doc-db <path>...` で格納してください」と報告して終了する
- `documents[]` (絶対パス) は Step 3 の grep フォールバック対象として保持する
- `missing` があれば「manifest 上 N 件がディスクに存在しません。`/build-doc-db` の
  resync で切り離せます」と最終報告に含める

### Step 2: doc-db に検索リクエスト (推奨パス)

`docdb_client.py` は `~/.doc-db/doc-db.yaml` の port を自動取得し、MCP handshake
(initialize → notifications/initialized → tools/call) を内部で行う。
**series は build 側と同じ固定値 `main` を指定する**:

```bash
python3 .claude/skills/query-doc-db/scripts/docdb_client.py query \
    --key "generic-docs" \
    --series "main" \
    --query "$ARGUMENTS" \
    --mode all \
    --top-n 20
```

stdout に `{"results": [...], "stage_stats": {...}, "warnings"?: [...]}` の JSON が返る。

**series を固定する理由**: 登録 API (`sync_documents`) は series 必須のため、build 側は
固定値 `main` で desired-state 同期している。series 無指定の全 series 横断検索には、
sync で切り離された削除済み文書が物理削除まで混入し得る (DES-001 §4.5 / APP-001 SYN-03)
ので、query 側も同じ固定値を指定して「manifest の完全な現在状態」だけを検索する。

**終了コードの扱い [MANDATORY]**

| exit | 意味                                          | 対応                                                                                                                          |
| ---- | --------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------- |
| 0    | 検索成功                                      | Step 4 へ                                                                                                                     |
| 3    | series 未登録 (manifest はあるがサーバ未同期) | **grep にフォールバックしない**。「インデックスが未同期です。`/build-doc-db` の resync を実行してください」と報告して終了する |
| 1    | サーバ未起動・その他エラー                    | Step 3 (grep フォールバック) へ                                                                                               |

**ヒット 0 件のとき**: `--series` を外した再検索は行わない (削除済み文書の混入を防ぐ)。
0 件は「格納済み文書に該当が無い」という正しい結果として報告する。

### Step 3: doc-db サーバ未起動時のフォールバック (grep)

**Step 3-1: 警告 [MANDATORY]**

応答の冒頭に必ず以下を出す:

```
⚠️ doc-db サーバが起動していません (http://localhost:<port>/mcp に接続失敗)。
   grep 簡易検索にフォールバックしました。優先度付き高精度検索を有効にするには
   `/build-doc-db` の Step 1 に記載の起動手順を実行してください。
```

**Step 3-2: 検索語の類義語展開 [MANDATORY]**

grep は表記一致しないとヒットしないため、`$ARGUMENTS` から抽出した検索語ごとに
**類義語・関連語** を展開してから検索する:

- 日英対訳 (例: 「バージョン」↔ `version`、「レビュー」↔ `review`、「権限」↔ `permission`)
- 略語・正式名称 (例: `req` ↔ `requirements`、`CI` ↔ continuous integration)
- 表記ゆれ・活用 (例: `index` / `indexing` / 索引、`config` / configuration / 設定)
- 同義・上位下位概念

**Step 3-3: grep 検索**

Step 1 の `documents[]` (絶対パス) を対象に、展開した語を `Grep` ツール
(`-i` 相当の大小無視、`|` で連結した正規表現も可) で横断適用。
マッチ語の種類数・出現数が多い順に並べる。判断に迷う候補は `Read` で実体確認。

### Step 4: 結果の整形

doc-db パス (Step 2) で `results[*]` から以下を抽出:

- `path` (絶対パス)
- `origin_signals` (どの signal でヒットしたか - 複数 signal 一致は信頼度高)
- `heading_path` (どの章か)

戻り値の `warnings` が空でなければ必ず含めて報告する (silent failure 禁止方針)。

## Output Format

冒頭は `Required documents:` 形式 (fallback 時は Step 3-1 の警告を先に出してから):

```
Required documents:

- /Users/xxx/notes/foo.md   [origin_signals: emb, grep]
- /Users/xxx/research/bar.md   [origin_signals: emb]
```

`origin_signals` は doc-db パスのときのみ表示。grep fallback 時は省略。

## Notes

- **HTTP 直叩き**: `docdb_client.py` は Python stdlib のみ (urllib) で MCP Streamable
  HTTP を扱う。Claude Code の MCP client 層に依存しない
- **PHIL-01 二層アーキ**: doc-db は「取りこぼし無き候補プール」を返す設計。この SKILL の
  呼び出し元 (親 Claude / AI agent) が本文を読んで最終判断する想定。よって top_n=20 と
  多めに取る
- **mode の判断**: 通常は `all` で十分。特定の ID や固有名を厳密に検索したい場合は
  `--mode grep` に切り替える判断もあり
- **KEY / series は build-doc-db と対**: `--key generic-docs` / `--series main` の固定値は
  `.claude/skills/build-doc-db/` 側の設計と対になっている。片方だけ変えると検索できなくなる
