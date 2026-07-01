---
name: delete-db-series
description: |
  doc-db インデックスから指定 series (Git branch / バージョンタグ等) を全 record から除去する。
  branch を削除した後の cleanup 用途。specs / rules 両方の KEY を対象にする。
  トリガー: "series を削除", "branch cleanup", "feature branch を doc-db から消す"
user-invocable: true
argument-hint: "<series 名>   例: feature/auth, pr-123, v1.0-old"
allowed-tools: Bash
---

指定した series を doc-db の `<project_name>-specs` および `<project_name>-rules` の
両 KEY から一括除去するラッパー。**doc-db サーバの HTTP エンドポイント
(`http://localhost:<port>/mcp`) を直接叩く**ため、Claude Code の MCP 登録は不要。
他プロジェクトに `.claude/skills/delete-db-series/` をコピーすればそのまま動く。

## Procedure

### Step 1: 引数チェック

`$ARGUMENTS` が空の場合はエラー案内:

```
使い方: /delete-db-series <series 名>
例:
  /delete-db-series feature/auth   ← 削除済み branch の cleanup
  /delete-db-series pr-123
  /delete-db-series v1.0-old
```

現在 checkout 中の branch を削除しようとした場合は警告する
(`git rev-parse --abbrev-ref HEAD` の結果と比較)。

### Step 2: KEY プレフィックスの取得

```bash
python3 .claude/skills/delete-db-series/scripts/resolve_docs.py --type specs
```

出力 JSON の `project_name` を取得し、対象 2 KEY を決める:

- `<project_name>-specs`
- `<project_name>-rules`

### Step 3: 両 KEY に対して delete-series を実行

`docdb_client.py` は `~/.doc-db/doc-db.yaml` の port を自動取得し、MCP handshake
(initialize → notifications/initialized → tools/call) を内部で行う。

```bash
python3 .claude/skills/delete-db-series/scripts/docdb_client.py delete-series \
    --key "<project_name>-specs" \
    --series "<$ARGUMENTS>"

python3 .claude/skills/delete-db-series/scripts/docdb_client.py delete-series \
    --key "<project_name>-rules" \
    --series "<$ARGUMENTS>"
```

各呼び出しの戻り値 `{removed_records, updated_records}` (JSON) を集約する。

KEY 自体が存在しない場合はサーバがエラーを返す (stderr にエラー、exit 1)。
cleanup 目的なので rules KEY のエラーは無視して続行する (specs KEY のみエラーの
場合も同様)。両方失敗した場合は接続エラー等の可能性ありとして報告する。

### Step 4: 完了レポート

```
✓ doc-db series 除去完了
  series: <$ARGUMENTS>
  specs KEY:
    removed_records: N (series が最後の 1 つで物理削除された record 数)
    updated_records: M (他 series が残り保持された record 数)
  rules KEY:
    ...
```

removed も updated も 0 なら「該当 series は既に存在しません (no-op)」と報告する。

## Notes

- **HTTP 直叩き**: `docdb_client.py` は Python stdlib のみ (urllib) で MCP Streamable
  HTTP を扱う。Claude Code の MCP client 層に依存しない
- **接続失敗**: サーバ未起動時は `python3 docdb_client.py` が exit 1 + stderr に接続
  エラーメッセージ。案内メッセージで `doc-db > /tmp/doc-db.log 2>&1 &` を提示
- **series が最後の 1 つだった record は物理削除**。チャンク・ベクトル・BM25 統計含めて全部消える
- **他 series が残る record は保持**。例: main と feature-x を持つ record から feature-x のみ削除すると main の record として残る
- **現在 checkout 中の branch を削除しない**。誤操作防止のため Step 1 で警告する
