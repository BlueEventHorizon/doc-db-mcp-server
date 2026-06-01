# DES-020 clean-rules スキル設計書

## メタデータ

| 項目   | 値         |
| ------ | ---------- |
| 設計ID | DES-020    |
| 作成日 | 2026-03-19 |

---

> 対象プラグイン: forge | スキル: `/forge:clean-rules`

---

## 1. 概要

forge をインストールしたターゲットプロジェクトで、プロジェクトの `rules/` を開発文書の分類学に基づいて分析し、forge 内蔵 docs との重複を検出・削除し、残る文書を体系的に再構築するスキル。

### 動機

- forge 導入前の rules/ が未整理で、関心事が混在したファイルや分類不明なルールが散在している
- forge 内蔵 docs はプラグインキャッシュ内にあり、forge 自身がレビュー等で参照する。プロジェクト側に同内容のコピーがあると二重管理になり、バージョン不整合が発生する
- 旧設計は「forge docs をプロジェクトにコピー」だったが、forge バージョン更新でコピーが陳腐化する問題があった。新設計は逆に「forge がカバーする内容はプロジェクトから削除」し、forge に委ねる

### 重要な原則

- **forge 優先**: forge 内蔵 docs でカバーされる内容はプロジェクト rules/ から削除する。forge が責任を持って管理する領域を二重管理しない
- **Project-defined を保護**: プロジェクト固有の取り決め（命名規則、Git ワークフロー等）は絶対に削除しない
- **安全性**: 破壊的操作の前に `git stash` で退避し、ロールバック可能にする
- **段階的実行**: デフォルトは分析のみ（ドライラン）。`--delete` / `--rebuild` で明示的に操作を指定する

---

## 2. モード構成

```
/forge:clean-rules                     # 分析のみ（何を削除/再構築すべきか報告）
/forge:clean-rules --delete            # forge 重複部分をセクション単位で削除
/forge:clean-rules --rebuild           # taxonomy に基づく再構築
/forge:clean-rules --delete --rebuild  # 削除してから再構築
```

| モード                 | 操作                                   | 安全性                       |
| ---------------------- | -------------------------------------- | ---------------------------- |
| デフォルト（引数なし） | 分析レポート出力のみ。ファイル変更なし | リスクなし                   |
| `--delete`             | forge 重複セクションの削除             | git stash + カテゴリ単位承認 |
| `--rebuild`            | taxonomy に基づくファイル分割・統合    | git stash + カテゴリ単位承認 |
| `--delete --rebuild`   | 削除 → 再構築を順次実行                | git stash + カテゴリ単位承認 |

---

## 3. 開発文書の分類学（Taxonomy）

スキルの判定基準となる分類体系。詳細は `plugins/forge/skills/clean-rules/docs/taxonomy.md` を参照。

### 次元 1: 内容の種類（Content Type）

| 種類           | 機能                                   |
| -------------- | -------------------------------------- |
| **Constraint** | MUST/MUST NOT の硬いルール。違反はバグ |
| **Convention** | SHOULD の合意事項。チームが変更可能    |
| **Format**     | 成果物の構造テンプレート               |
| **Process**    | ステップバイステップの手順             |
| **Decision**   | 選択の根拠。ADR 相当                   |
| **Reference**  | 参照情報。変更は事実の反映             |

### 次元 2: 権威源（Authority Source）

| 源                    | 管理者             | --delete での扱い          |
| --------------------- | ------------------ | -------------------------- |
| **Tool-provided**     | forge プラグイン   | 削除対象（forge に委ねる） |
| **Project-defined**   | チーム合意         | 保護（絶対に削除しない）   |
| **External standard** | 標準団体・言語仕様 | 保護                       |

### forge とプロジェクトの責務分離

| Content Type   | forge が担う（Tool-provided）        | プロジェクトが担う                 |
| -------------- | ------------------------------------ | ---------------------------------- |
| **Constraint** | レビュー観点（review_criteria_*.md） | プロジェクト固有の制約             |
| **Convention** | —                                    | 命名規則、コードスタイル、用語統一 |
| **Format**     | 文書テンプレート（*_format.md）      | —                                  |
| **Process**    | ワークフロー（SKILL.md 内蔵）        | デプロイ手順、リリースフロー       |
| **Decision**   | —                                    | アーキテクチャ判断（ADR）          |
| **Reference**  | ID 分類カタログ（spec_format）       | プロジェクト固有の参照情報         |

---

## 4. detect_forge_overlap.py

forge docs とプロジェクト rules のセクション単位の重複を Embedding コサイン類似度で検出するスクリプト。
AI の分類判断を補強するデータを提供する。

### 既存資産の活用

- `plugins/doc-advisor/scripts/embedding_api.py` — OpenAI Embedding API 呼び出し（urllib、標準ライブラリのみ）
- `plugins/doc-advisor/scripts/search_docs.py` の `cosine_similarity()` — 純粋 Python のコサイン類似度計算

### インターフェース

```
python3 detect_forge_overlap.py \
  --project-rules file1.md file2.md ... \
  --forge-docs forge1.md forge2.md ... \
  [--threshold 0.5]
```

### 処理フロー

1. 各ファイルを `##` 見出しでセクション分割
2. 各セクションのテキストを `embedding_api.call_embedding_api()` でバッチベクトル化
3. プロジェクト rules の各セクション vs forge docs の各セクションのコサイン類似度を算出
4. 閾値超えのペアを `forge_overlap` 候補として出力

### 出力フォーマット

```json
{
  "status": "ok",
  "overlaps": [{
    "project_file": "docs/rules/version_migration_design.md",
    "project_section": "## 2. 失敗するアンチパターン",
    "forge_file": "plugins/forge/docs/version_migration_spec.md",
    "forge_section": "## Migration function contracts",
    "similarity": 0.82
  }]
}
```

### 前提条件

- API キー環境変数が必須。`OPENAI_API_DOCDB_KEY` を優先参照し、未設定時は `OPENAI_API_KEY` にフォールバック（doc-advisor:DES-007_unified_api_key_reference_design / FNC-004 KEY-01 統一仕様）。両方未設定時はエラー終了

---

## 5. ワークフロー

### Phase 1: 情報収集

1. `.doc_structure.yaml` の存在確認（なければ `/forge:setup-doc-structure` を案内しエラー終了）
2. ルール文書一覧を取得（`resolve_doc_structure.py --type rules`）
3. forge 内蔵 docs のパスを `rules_toc.yaml` から全件取得
4. `detect_forge_overlap.py` で重複検出（Embedding コサイン類似度）
5. 分類学定義を Read（`taxonomy.md`）
6. ルール文書と forge docs を全て Read

### Phase 2: 分類・分析（AI）

`taxonomy.md` の分類学に基づき、各ルール文書のセクション（`##` 見出し）単位で以下を判定:

- **A. Content Type**: Constraint / Convention / Format / Process / Decision / Reference
- **B. Authority Source**: Tool-provided / Project-defined / External standard
- **C. forge 対応**: `detect_forge_overlap.py` の重複スコアを参考に、Tool-provided セクションの forge docs 対応先を特定
- **D. モード別推奨**: 各セクションに対する `--delete` / `--rebuild` の推奨アクション

分析結果を JSON 形式で出力。デフォルトモード（引数なし）はここで終了。

### Phase 3: 安全確保

`--delete` または `--rebuild` が指定された場合のみ実行:

- `git stash` で作業状態を退避
- 変更計画をカテゴリ単位で AskUserQuestion で承認

### Phase 4-D: 削除実行（--delete）

- 分析結果から `delete_recommendation: "delete"` のセクションを処理
- ファイル全体が削除対象 → ファイル削除
- 一部セクションのみ削除対象 → 該当セクションを除去し、残りを保存
- 相互参照の検出と更新

### Phase 4-R: 再構築実行（--rebuild）

- 分割: Content Type が 3 種以上混在 AND 100 行超のファイルのみ
- 統合: 同一 Content Type + 同一関心事の小ファイル群をまとめる
- 各操作後に markdown 構文チェック（見出し階層の整合性）

### Phase 5: 完了処理

- 結果サマリー出力
- `.doc_structure.yaml` の自動更新
- 相互参照の更新レポート
- ルール ToC 更新（`/forge:update-db-rules` が利用可能な場合）
- `/anvil:commit` で commit 確認
- ロールバック手段の提示（`git stash pop`）

---

## 6. ファイル構成

```
plugins/forge/skills/clean-rules/
  SKILL.md                        # スキル定義（3モード構成）
  docs/
    taxonomy.md                   # 分類学の定義（AI 判定基準）
  scripts/
    detect_forge_overlap.py       # Embedding ベースの重複検出

plugins/forge/toc/
  rules/rules_toc.yaml            # forge 内蔵 docs の検索インデックス

tests/forge/clean-rules/
  test_detect_forge_overlap.py    # 重複検出スクリプトのテスト
```

---

## 7. 関連ファイル

| ファイル                                                              | 役割                                                        |
| --------------------------------------------------------------------- | ----------------------------------------------------------- |
| `plugins/forge/skills/doc-structure/scripts/resolve_doc_structure.py` | rules 一覧取得に使用                                        |
| `plugins/doc-advisor/scripts/embedding_api.py`                        | Embedding API 呼び出し（detect_forge_overlap.py が import） |
| `plugins/doc-advisor/scripts/search_docs.py`                          | cosine_similarity() の実装元                                |
| `plugins/forge/skills/review/docs/review_criteria_{type}.md`          | forge 内蔵レビュー観点（種別ごと）                          |

---

## 8. 調査 Sources

- [How to write a good spec for AI agents - Addy Osmani](https://addyosmani.com/blog/good-spec/)
- [Writing a good CLAUDE.md - HumanLayer](https://www.humanlayer.dev/blog/writing-a-good-claude-md)
- [Spec-driven development - Thoughtworks](https://thoughtworks.medium.com/spec-driven-development-d85995a81387)
- [CLAUDE.md Best Practices - UX Planet](https://uxplanet.org/claude-md-best-practices-1ef4f861ce7c)
- [Best Practices for Claude Code](https://code.claude.com/docs/en/best-practices)
- [Taxonomies in Software Engineering - ScienceDirect](https://www.sciencedirect.com/science/article/pii/S0950584917300472)
