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
両 KEY から一括除去するラッパー。他プロジェクトに `.claude/skills/delete-db-series/` を
コピーすればそのまま動く。

## Procedure

### Step 1: doc-db MCP tool 可用性チェック [MANDATORY]

`mcp__doc-db__delete_series` が available tools に存在するか確認する。
存在しない場合は以下のセットアップ案内を提示して終了する:

```
⚠️ doc-db MCP サーバーがこのプロジェクトに登録されていません。
`.claude/skills/update-db-specs/SKILL.md` の Step 1 に記載のインストール手順を実行してください。
```

### Step 2: 引数チェック

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

### Step 3: KEY プレフィックスの取得

```bash
python3 .claude/skills/delete-db-series/scripts/resolve_docs.py --type specs
```

出力 JSON の `project_name` を取得し、対象 2 KEY を決める:

- `<project_name>-specs`
- `<project_name>-rules`

### Step 4: 両 KEY に対して delete_series を実行

```
mcp__doc-db__delete_series({"key": "<project_name>-specs", "series": "<$ARGUMENTS>"})
mcp__doc-db__delete_series({"key": "<project_name>-rules", "series": "<$ARGUMENTS>"})
```

各呼び出しの戻り値 `{removed_records, updated_records}` を集約する。
KEY 自体が存在しない場合は doc-db 側でエラーになるので try-catch 相当で無視する
(cleanup 目的なので rules KEY が無くても問題無い)。

### Step 5: 完了レポート

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

- **series が最後の 1 つだった record は物理削除**。チャンク・ベクトル・BM25 統計含めて全部消える
- **他 series が残る record は保持**。例: main と feature-x を持つ record から feature-x のみ削除すると main の record として残る
- **現在 checkout 中の branch を削除しない**。誤操作防止のため Step 2 で警告する
- **KEY 自体が存在しない場合はエラーだが無視する** (cleanup 目的の性質上 no-op として扱う)
