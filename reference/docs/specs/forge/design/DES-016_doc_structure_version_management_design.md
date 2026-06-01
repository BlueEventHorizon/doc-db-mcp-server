# DES-016 .doc_structure.yaml バージョン管理設計

## メタデータ

| 項目   | 値         |
| ------ | ---------- |
| 設計ID | DES-016    |
| 作成日 | 2026-03-14 |

---

> COMMON-REQ-001（段階的バージョンマイグレーション要件）の `.doc_structure.yaml` への適用設計

## 概要

`.doc_structure.yaml` のバージョン管理・マイグレーション方式を定義する。
COMMON-REQ-001 に準拠し、Python スクリプトによる機械的なマイグレーションを実現する。

## バージョン識別子

YAML コメント行でバージョンを記録する:

```yaml
# doc_structure_version: 3.0
```

- **フォーマット**: `X.Y`（X = メジャー、Y = マイナー）
- **マーカー**: `doc_structure_version:` — `.doc_structure.yaml` 独自のバージョン識別子
- **配置**: ファイル先頭のコメントブロック内

### バージョン番号の意味

| 変更種別                                   | バージョン          | 例        |
| ------------------------------------------ | ------------------- | --------- |
| 破壊的変更（フィールド削除、構造変更）     | メジャー X を上げる | 2.0 → 3.0 |
| 後方互換の追加（新オプショナルフィールド） | マイナー Y を上げる | 2.0 → 2.1 |

## マイグレーション方式

COMMON-REQ-001（段階的バージョンマイグレーション要件）に準拠する。

### マイグレーションフロー

```
1. .doc_structure.yaml を読み込み
2. バージョン検出（get_major_version）
3. detected_version < CURRENT_VERSION なら段階的マイグレーション適用
4. マイグレーション後のデータを返す（元データは書き換えない: NFR-01）
```

### CURRENT_VERSION 定数

```python
CURRENT_VERSION = 3  # FR-02-5
```

### MIGRATIONS レジストリ

メジャーバージョン変更時に、構造変換関数を登録する:

```python
MIGRATIONS = {
    2: migrate_v1_to_v2,   # v1 → v2: config.yaml 互換フォーマット採用
    3: migrate_v2_to_v3,   # v2 → v3: Doc Advisor 内部フィールド除去
}
```

- キー: ターゲットバージョン番号（FR-02-2）
- 値: 変換関数 `fn(content: str) -> str`（FR-03-1: 1入力1出力。本適用では str テキスト操作）
- 段階的適用: `detected_version < v <= CURRENT_VERSION` の範囲を昇順に実行（FR-02-1, FR-02-3）
- 各関数は前段の出力を入力として受け取る（FR-02-4: チェーン実行）
- 冪等であること（FR-03-2）、副作用を持たないこと（FR-03-3）

### エラーハンドリング

- マイグレーション関数は純粋関数（FR-03-3）であり、例外はコードバグとして伝播させる（FR-04-1）。サイレントフォールバックで不具合を隠蔽しない
- 検出バージョンが CURRENT_VERSION より大きい場合、マイグレーションをスキップしそのまま使用する（FR-04-2）
- バージョン検出に失敗した場合は v1 として扱う（FR-04-3, FR-01-2）

### マイグレーション追加手順

1. `migrate_vN_to_vN1(content: str) -> str` を実装する（FR-05-2: 既存関数は変更しない）
2. `MIGRATIONS[N+1] = migrate_vN_to_vN1` を登録する（FR-02-2）
3. `CURRENT_VERSION` を `N+1` に更新する（FR-02-5）
4. この設計書のマイグレーション履歴セクションに記録する

## 実装上の設計判断

### data の型: str（テキスト操作）

標準ライブラリに YAML パーサーがない（NFR-02）ため、マイグレーション関数はテキスト操作で実装する。

- 入力: `str`（`.doc_structure.yaml` の生テキスト）
- 出力: `str`（マイグレーション後のテキスト）
- バージョンマーカー（`# doc_structure_version: X.0`）はコメント行なので dict 変換すると消える。テキスト操作ならコメント行を自然に保持できる

COMMON-REQ-001 の設計パターンは `data` を dict として描いているが、本適用では `str` とする。要件 FR-03-1「1つ前のバージョンの出力形式を入力として受け取り、ターゲットバージョンの形式を返す」は str でも満たせる。

```python
MIGRATIONS = {
    2: migrate_v1_to_v2,   # fn(str) -> str
    3: migrate_v2_to_v3,   # fn(str) -> str
}
```

### バージョン検出の再利用方法

`resolve_doc_structure.py` の `get_version()` / `get_major_version()` はテキストベースの正規表現マッチであり、`migrate_doc_structure.py` から再利用可能。

方法: `sys.path` にスクリプトのディレクトリを追加して import する。`resolve_doc_structure.py` は `plugins/forge/skills/doc-structure/scripts/` に配置されているため:

```python
import sys
from pathlib import Path
SCRIPT_DIR = Path(__file__).resolve().parent
PLUGIN_ROOT = SCRIPT_DIR.parent  # plugins/forge/
sys.path.insert(0, str(PLUGIN_ROOT / 'skills' / 'doc-structure' / 'scripts'))
from resolve_doc_structure import get_version, get_major_version
```

### v1 形式の構造

git 履歴から確認した v1 形式:

```yaml
version: "1.0"

specs:
  design:
    paths: [docs/specs/design/]
    description: "Design specifications..."
  plan:
    paths: [docs/specs/plan/]
    description: "Development plans..."
  requirement:
    paths: [docs/specs/requirement/]
    description: "Functional and non-functional requirements"

rules:
  rule:
    paths: [docs/rules/]
    description: "Development rules..."
```

特徴:

- `version: "1.0"` — YAML フィールドとしてバージョンを記録（v2+ はコメント行）
- `{category}.{doc_type}.paths` — doc_type がキー、paths が配列
- `description` フィールドあり（v2+ では廃止）

### migrate_v1_to_v2 の変換仕様

v1 → v2 変換:

1. `version: "1.0"` 行を削除
2. `# doc_structure_version: 2.0` コメント行を先頭に追加
3. `{category}.{doc_type}.paths` → `{category}.root_dirs` + `{category}.doc_types_map` に変換
4. `description` フィールドを削除
5. `patterns`, `toc_file`, `checksums_file`, `work_dir`, `output`, `common` を追加（v2 形式のデフォルト値）

### migrate_v2_to_v3 の変換仕様

v2 → v3 変換:

1. `# doc_structure_version: 2.0` → `# doc_structure_version: 3.0` に置換
2. `toc_file`, `checksums_file`, `work_dir` 行を削除
3. `output:` セクション（`header_comment`, `metadata_name`）を削除
4. `common:` セクション全体を削除
5. `root_dirs`, `doc_types_map`, `patterns` は保持

---

## マイグレーションスクリプト

### 配置

```
plugins/forge/scripts/migrate_doc_structure.py
```

forge プラグインのスクリプトとして配置する。doc-advisor の `merge_config.py` とは独立。

### CLI インターフェース

```bash
# マイグレーション実行（結果を stdout に出力。元ファイルは書き換えない: NFR-01）
python3 migrate_doc_structure.py <file_path>

# ドライラン（マイグレーション内容を表示するが適用しない）
python3 migrate_doc_structure.py <file_path> --dry-run

# バージョン情報のみ表示
python3 migrate_doc_structure.py <file_path> --check
```

### 出力仕様

| モード      | stdout                                 | 終了コード               |
| ----------- | -------------------------------------- | ------------------------ |
| 通常        | マイグレーション後の YAML テキスト     | 0: 成功, 1: エラー       |
| `--dry-run` | 適用されるマイグレーション一覧（JSON） | 0: 変更あり, 2: 変更なし |
| `--check`   | バージョン情報（JSON）                 | 0                        |

```bash
# --check の出力例
python3 migrate_doc_structure.py .doc_structure.yaml --check
# → {"detected_version": 2, "current_version": 3, "needs_migration": true}

# バージョン検出失敗時（FR-04-3: v1 として扱う）
# → {"detected_version": 1, "current_version": 3, "needs_migration": true}

# --dry-run の出力例
python3 migrate_doc_structure.py .doc_structure.yaml --dry-run
# → {"migrations": [{"from": 2, "to": 3, "description": "Remove doc-advisor internal fields"}]}

# 通常実行の出力例（stdout にマイグレーション後の YAML）
python3 migrate_doc_structure.py .doc_structure.yaml > .doc_structure.yaml.new
mv .doc_structure.yaml.new .doc_structure.yaml
```

### 書き出しの責務分担

NFR-01「元データを書き換えない」に従い、スクリプトは stdout に出力するのみ。ファイルへの書き出しは呼び出し元（`setup-doc-structure` SKILL.md または手動）が行う。

### setup-doc-structure からの呼び出しフロー

`setup-doc-structure/SKILL.md` の Step 1（既存ファイルの確認）にバージョンチェックを追加する:

```
1. .doc_structure.yaml が存在するか確認
2. 存在する場合:
   a. python3 migrate_doc_structure.py .doc_structure.yaml --check でバージョン確認
   b. needs_migration: true の場合:
      - AskUserQuestion:「.doc_structure.yaml を v{detected} → v{current} にマイグレーションしますか？」
      - Yes → python3 migrate_doc_structure.py .doc_structure.yaml で変換結果を取得 → Write で書き出し
      - No → 現状のまま続行
   c. needs_migration: false → レビューモードへ
3. 存在しない場合 → Step 2（ディレクトリスキャン）へ
```

---

## ユーザー設定の保持ルール

マイグレーション時に以下のユーザー設定を保持する:

| 設定                   | 保持条件   |
| ---------------------- | ---------- |
| `root_dirs`            | 非空の場合 |
| `doc_types_map`        | 非空の場合 |
| `patterns.target_glob` | 非空の場合 |
| `patterns.exclude`     | 非空の場合 |

> **Note**: Doc Advisor v5.0 で `output.*`, `common.*`, `toc_file`, `checksums_file`, `work_dir` は `.doc_structure.yaml` から除去され、Doc Advisor のコードデフォルト（`toc_utils.py`）で管理される。マイグレーション時にこれらのフィールドが存在しても無視される（後方互換性あり）。

## バージョン検出 API

`resolve_doc_structure.py` が提供する:

```bash
python3 resolve_doc_structure.py --version
# → {"status": "ok", "version": "3.0", "major_version": 3}
```

```python
from resolve_doc_structure import get_version, get_major_version

content = open('.doc_structure.yaml').read()
version = get_version(content)      # "3.0"
major = get_major_version(content)  # 3
```

## マイグレーション履歴

| バージョン | 変更内容                                                                                                                              |
| ---------- | ------------------------------------------------------------------------------------------------------------------------------------- |
| 1.0        | 旧形式（doc_type-centric: `version: "1.0"` YAML フィールド）                                                                          |
| 2.0        | config.yaml 互換フォーマット採用。バージョンマーカーを `doc_structure_version:` コメント行に変更                                      |
| 3.0        | Doc Advisor v5.0 対応。内部フィールド（toc_file, checksums_file, work_dir, output._, common._）を除去。文書構造フィールドのみに簡素化 |

## 関連ファイル

| ファイル                                                              | 役割                                                 |
| --------------------------------------------------------------------- | ---------------------------------------------------- |
| `plugins/forge/scripts/migrate_doc_structure.py`                      | マイグレーションスクリプト（COMMON-REQ-001 準拠）    |
| `plugins/forge/skills/doc-structure/scripts/resolve_doc_structure.py` | バージョン検出（`get_version`, `get_major_version`） |
| `plugins/forge/skills/setup-doc-structure/SKILL.md`                   | マイグレーション呼び出し元（Step 1）                 |
| `plugins/forge/docs/doc_structure_format.md`                          | フォーマット仕様                                     |
| `docs/specs/common/requirement/COMMON-REQ-001_versioned_migration.md` | マイグレーション要件定義                             |
