# DES-030 変更行 gate 起動経路用語 TOML 化 設計書

## メタデータ

| 項目       | 値                                                                                                                               |
| ---------- | -------------------------------------------------------------------------------------------------------------------------------- |
| 設計 ID    | DES-030                                                                                                                          |
| 関連要件   | REQ-005_skill_agent_launch_contract                                                                                              |
| 関連設計   | DES-029_skill_agent_launch_contract_design                                                                                       |
| 関連ルール | `docs/rules/skill_launch_paths_definitions.md`, `docs/rules/skill_authoring_notes.md`, `docs/rules/implementation_guidelines.md` |
| 作成日     | 2026-05-26                                                                                                                       |
| 適用範囲   | `tests/forge/subagent/test_changed_lines_policy.py` の変更行 gate                                                                |

---

## 1. 概要

forge-subagent の変更行 gate では、slash command の近傍に起動経路の説明があるかを判定するため、起動経路の公式用語を Python テスト内に直書きしている。

この公式用語リストを、`docs/rules/skill_launch_paths_definitions.md` の機械可読 subset として TOML に切り出す。TOML は Python 3.11 以降の標準ライブラリ `tomllib` で読めるため、外部依存を追加しない。

本設計の目的は「公式起動経路用語の二重管理を減らすこと」に限定する。ユーザー向け使用例マーカー、正規表現、判定距離、旧 CLI 構文検出などのテスト実装詳細は TOML 化しない。

---

## 2. 設計方針

### 2.1 採用する方針

| 方針                      | 内容                                                                                 |
| ------------------------- | ------------------------------------------------------------------------------------ |
| 公式用語だけを TOML 化    | `docs/rules/skill_launch_terms.toml` には起動経路の公式用語だけを置く                |
| テスト文脈語は Python 側  | 使用例マーカーはテスト誤検知抑制のための heuristic なので、テストコードに残す        |
| regex は Python 側に残す  | `subagent` 検出、slash command 検出、旧 review CLI 検出などはテストロジックに残す    |
| version は持たない        | 初期実装では schema version の運用が不要。必要になった時点で運用ルールと共に追加する |
| schema 検査は最小限にする | TOML が読めること、必要な配列が存在し空でないこと、重複定義がないことだけを検査する  |
| Markdown 正本は維持する   | 用語の意味・背景・使い分けは `skill_launch_paths_definitions.md` を正本として扱う    |

### 2.2 採用しない方針

| 代替案                         | 不採用理由                                                                                   |
| ------------------------------ | -------------------------------------------------------------------------------------------- |
| YAML 用語集                    | 標準ライブラリに YAML parser がない。PyYAML 追加は外部依存禁止方針と衝突する                 |
| 使用例マーカーも TOML 化       | 公式用語ではなくテスト都合の heuristic であり、`docs/rules/` 配下の用語集に置くと誤読を招く  |
| 判定パラメータをすべて TOML 化 | Python と TOML の両方を読まないと挙動が分からなくなり、今回の目的に対して重い                |
| Markdown 表の parser を作る    | 表の整形変更に弱い。人間向け文書の自由度を落とす                                             |
| Markdown との完全同期テスト    | 逆方向検査は自動化しない。TOML の各公式用語が Markdown 本文に存在する片方向検査に留める      |
| 全 SKILL baseline を同時に移行 | 変更行 gate の改善が主目的。既存 baseline テストの移行は必要が明確になった時点で別途判断する |

---

## 3. TOML 用語集設計

### 3.1 配置

```text
docs/rules/skill_launch_terms.toml
```

`docs/rules/` 配下に置く理由:

- `skill_launch_paths_definitions.md` と同じ責務領域で管理できる
- 用語定義の近くにあり、変更時に見落としにくい
- plugin 配布物ではなく、リポジトリの品質 gate 用データとして扱える

配置時の副作用確認:

- `.doc_structure.yaml` の `rules.patterns.target_glob` が `**/*.md` であり、通常の rules index 対象に TOML が混入しないことを確認する
- doc-advisor / doc-db / forge rules 検索の実装が拡張子無制限で `docs/rules/` を読む場合は、必要に応じて exclude または対象 glob を追加する
- 将来 `.doc_structure.yaml` の rules 対象 glob を `.toml` まで広げる場合は、`skill_launch_terms.toml` が検索 index に混入しないかを再確認する
- `dprint.jsonc` で TOML plugin が有効であり、`dprint check` の対象になることを確認する

### 3.2 スキーマ

```toml
[metadata]
source_doc = "docs/rules/skill_launch_paths_definitions.md"

[launch_context]
terms = [
  "汎用 Agent",
  "カスタム Agent",
  "継承型 SKILL",
  "fork 型 SKILL",
  "Bash subprocess",
  "Skill ツール",
  "Agent ツール",
]
```

### 3.3 セクション責務

| セクション       | 責務               | テストでの用途                                   |
| ---------------- | ------------------ | ------------------------------------------------ |
| `metadata`       | 用語集の管理情報   | `source_doc` の存在確認                          |
| `launch_context` | 起動経路の公式用語 | slash command 近傍に起動経路説明があるか判定する |

TOML にはテスト用 heuristic を入れない。使用例マーカーは `test_changed_lines_policy.py` の private 定数として残し、公式用語として扱わない。

---

## 4. 実装設計

### 4.1 変更対象

| ファイル                                            | 変更内容                                                          |
| --------------------------------------------------- | ----------------------------------------------------------------- |
| `docs/rules/skill_launch_terms.toml`                | 起動経路公式用語を追加する                                        |
| `docs/rules/skill_launch_paths_definitions.md`      | 機械可読 subset の配置を逆参照として追記する                      |
| `tests/forge/subagent/skill_launch_terms.py`        | `tomllib` で公式用語 TOML を読むテスト用 helper を追加する        |
| `tests/forge/subagent/test_changed_lines_policy.py` | `_LAUNCH_CONTEXT_PATTERNS` を TOML 参照にする                     |
| `tests/forge/subagent/test_skill_launch_terms.py`   | TOML の最小 schema、source_doc 参照、重複リスト回帰防止を検査する |

### 4.2 読み込み helper

2 つのテストから参照するため、読み込み処理は `tests/forge/subagent/skill_launch_terms.py` に集約する。`test_` prefix は付けず、unittest discover の対象外にする。

`tests/` 配下には `tests/forge/helpers.py` / `tests/forge/wrapper_helpers.py` のような非 `test_` helper が既にあるため、同じ方針に従う。

想定実装:

```python
import tomllib
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[3]
TERMS_PATH = REPO_ROOT / "docs/rules/skill_launch_terms.toml"


def load_terms() -> dict:
    with TERMS_PATH.open("rb") as f:
        return tomllib.load(f)
```

`test_changed_lines_policy.py` は TOML から文字列配列を読み、`re.escape()` で literal match 用 pattern に変換して、既存の `_has_launch_context_or_user_example()` の起動経路判定に渡す。pattern 生成は単一消費者である `test_changed_lines_policy.py` 側に置く。正規表現そのものは TOML に置かないため、TOML の編集者が regex escape を意識する必要はない。

`_USER_EXAMPLE_PATTERNS` はテスト用 heuristic として `test_changed_lines_policy.py` に残す。

### 4.3 最小 schema test

`test_skill_launch_terms.py` は以下を検査する。

- `metadata.source_doc` が指すファイルが実在する
- `launch_context.terms` が空でない文字列配列
- `launch_context.terms` の各値が `source_doc` 本文に substring match で出現する
- `test_changed_lines_policy.py` に `launch_context.terms` の文字列 literal が重複定義されていない

重複定義検査は `ast` ベースで行う。`test_changed_lines_policy.py` を parse し、docstring を除外した `ast.Constant` の文字列 literal が公式用語と完全一致しないことを確認する。substring match は採用しない。エラーメッセージや説明文の文字列 literal に公式用語が文の一部として現れるだけなら違反にしない。

実装スケッチ:

```python
tree = ast.parse(target.read_text(encoding="utf-8"))
docstring_nodes = _collect_docstring_constant_ids(tree)
for node in ast.walk(tree):
    if isinstance(node, ast.Constant) and isinstance(node.value, str):
        if id(node) not in docstring_nodes:
            self.assertNotIn(node.value, terms)
```

コメントは AST に現れないため検査対象外とする。単純 substring 検査は docstring / コメントの説明文まで fail させるため採用しない。

`source_doc` 整合検査は片方向である。TOML にある公式用語が Markdown 本文に存在することだけを保証し、Markdown 側に新しい公式用語が追加されたのに TOML が未更新である drift は人間レビューで担保する。

---

## 5. 受け入れ条件

- `python3 -m unittest tests.forge.subagent.test_changed_lines_policy -v` が pass する
- `python3 -m unittest tests.forge.subagent.test_skill_launch_terms -v` が pass する
- `python3 -m unittest discover -s tests -p 'test_*.py' -v` が pass する
- `dprint check` が pass する
- `test_skill_launch_terms.py` が、AST ベースで `test_changed_lines_policy.py` に公式用語の文字列 literal 重複が残っていないことを検査する
- 外部依存を追加しない

---

## 6. 移行手順

| Step | 作業                                                                                                                                            |
| ---- | ----------------------------------------------------------------------------------------------------------------------------------------------- |
| 1    | `.doc_structure.yaml` の rules 対象 glob が `.md` 限定であること、`dprint.jsonc` の TOML plugin が有効であることを確認する                      |
| 2    | `docs/rules/skill_launch_terms.toml` を追加し、`docs/rules/skill_launch_paths_definitions.md` に機械可読 subset の逆参照を追記する              |
| 3    | `tests/forge/subagent/skill_launch_terms.py` / `test_skill_launch_terms.py` を追加し、`test_changed_lines_policy.py` を TOML 読み込みに変更する |
| 4    | 対象テストと全体テストを実行する                                                                                                                |

Step 2-3 は同一変更セットで実施する。commit する場合も同一 commit にまとめ、中間状態として未使用 helper や fail する重複検査を main に積まない。

`skill_launch_paths_definitions.md` への追記例:

```markdown
> 機械可読 subset: `docs/rules/skill_launch_terms.toml`
```

---

## 7. リスク・制約

### 7.1 Python 3.11 未満では動作しない

`tomllib` は Python 3.11 以降の標準ライブラリである。Python 3.10 以下をサポートする場合は `tomli` が必要になるが、本リポジトリでは外部依存を増やさないため対象外とする。

### 7.2 テスト用 heuristic を公式用語へ逆流させない

使用例マーカーはテストの誤検知を避けるための heuristic であり、公式用語ではない。`docs/rules/skill_launch_terms.toml` や `skill_launch_paths_definitions.md` に「用語」として追加しない。

### 7.3 用語集とテスト実装の境界が崩れる可能性

TOML に regex、判定距離、ignore marker、旧 CLI 契約を追加し始めると、テストの設定ファイル化が進む。今回の TOML は起動経路公式用語のリストに限定する。

### 7.4 Markdown 正本との完全同期は保証しない

TOML は機械可読 subset であり、意味・背景・使い分けは Markdown 側に残す。自動テストは TOML から Markdown への片方向検査だけを行う。Markdown から TOML への逆方向 drift は、将来も人間レビューで担保する方針とする。

---

## 改定履歴

| 日付       | バージョン | 内容                                                                                                                                                     |
| ---------- | ---------- | -------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 2026-05-26 | 0.1        | 初版作成。変更行 gate のハードコード定数を TOML policy に移し、`tomllib` で読み込む設計を定義。                                                          |
| 2026-05-26 | 0.2        | 初手として過剰だった policy 化を縮小。TOML は起動経路公式用語と使用例マーカーだけを持つ機械可読用語集とし、regex・判定距離・CLI 契約は Python 側に残す。 |
| 2026-05-26 | 0.3        | レビュー指摘対応。TOML 対象を公式起動経路用語だけに再縮小し、version を削除。使用例マーカーはテスト heuristic として Python 側に残す設計へ変更。         |
| 2026-05-26 | 0.4        | レビュー指摘対応。中間状態でテストが落ちない移行手順へ変更し、重複定義検査を AST ベースと明記。source_doc 実在検査と片方向 drift 方針も明確化。          |
| 2026-05-26 | 0.5        | レビュー指摘対応。AST 重複検査を完全一致、source_doc 出現検査を substring match と明記。helper 追加と利用側改修を同一変更セットに統合。                  |
