---
name: build-index
description: |
  doc-db の検索インデックスを構築・更新する。
  新しい文書を doc-db の検索対象に含めたいときに実行する。
  トリガー: "/doc-db:build-index", "doc-db の index を作る"
user-invocable: true
argument-hint: "[--category rules|specs] [--full] [--check] [--doc-type requirement,design,plan]"
allowed-tools: Bash
---

# /doc-db:build-index

`plugins/doc-db/scripts/build_index.py` を呼び出して index を構築する。

```bash
python3 "${CLAUDE_PLUGIN_ROOT}/scripts/build_index.py" "$@"
```
