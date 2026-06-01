# DES-023 バージョン管理ワークフロー設計書

## メタデータ

| 項目   | 値         |
| ------ | ---------- |
| 設計ID | DES-023    |
| 作成日 | 2026-03-22 |

---

> 対象プラグイン: forge | スキル: `/forge:setup-version-config`, `/forge:update-version`

---

## 1. 概要

マルチプラグインリポジトリにおいて、バージョン番号が複数ファイル（plugin.json, marketplace.json, README, CHANGELOG）に散在する。手動更新は不整合の原因となるため、設定ファイル `.version-config.yaml` を中心に一括更新する仕組みを提供する。

### スキル構成

| スキル                 | 役割                                                                     | user-invocable |
| ---------------------- | ------------------------------------------------------------------------ | -------------- |
| `setup-version-config` | プロジェクトをスキャンし `.version-config.yaml` を対話的に生成・更新する | Yes            |
| `update-version`       | `.version-config.yaml` に基づきバージョンを一括更新する                  | Yes            |

### 設計原則

| 原則                            | 説明                                                                                                      |
| ------------------------------- | --------------------------------------------------------------------------------------------------------- |
| **ファイル非破壊**              | スクリプトは元ファイルを直接書き換えない。更新内容を stdout に出力し、SKILL.md（AI）が Write する         |
| **テキストベース置換**          | JSON/TOML の AST パースではなくテキスト操作で置換する。フォーマット（インデント・コメント）を保持するため |
| **filter による安全な絞り込み** | 同一ファイル内に複数 target のバージョンが存在する場合、filter で置換対象を限定する                       |
| **標準ライブラリのみ**          | 外部依存なし（PyYAML 等不要）                                                                             |
| **冪等性**                      | scan_version_targets は同一入力に対して同一出力を返す                                                     |

---

## 2. .version-config.yaml スキーマ

### 2.1 全体構造

```yaml
# version_config_version: 1.0

targets: # バージョン管理対象の定義（1つ以上）
changelog: # CHANGELOG 更新設定（任意）
git: # git commit/tag 設定（任意）
```

### 2.2 targets

```yaml
targets:
  - name: forge # target の論理名
    version_file: plugins/forge/.claude-plugin/plugin.json # バージョン値の正規ソース
    version_path: version # JSON/TOML のフィールドパス（ネストは "a.b.c"）
    sync_files: # バージョンを同期するファイルリスト
      - path: .claude-plugin/marketplace.json
        pattern: '"version": "{version}"' # 人間向けのパターン説明（スクリプトは使用しない）
        filter: '"name": "forge"' # 置換対象を限定するための絞り込み文字列
      - path: README.md
        pattern: "| **forge** | {version} |"
        filter: "| **forge**"
      - path: README_ja.md
        pattern: "| **forge** | {version} |"
        filter: "| **forge**"
        optional: true # true: ファイル不在時はスキップ
```

#### フィールド定義

| フィールド     | 型     | 必須 | 説明                                                                                    |
| -------------- | ------ | ---- | --------------------------------------------------------------------------------------- |
| `name`         | string | Yes  | target の論理名。`/forge:update-version` の引数に使用する                               |
| `version_file` | string | Yes  | バージョン値の正規ソースファイルパス（プロジェクトルートからの相対パス）                |
| `version_path` | string | Yes  | version_file 内のバージョンフィールドパス。ネストは `.` 区切り（例: `package.version`） |
| `sync_files`   | array  | No   | バージョンを同期する追加ファイルのリスト                                                |

#### sync_files エントリ

| フィールド | 型      | 必須 | 説明                                                                                              |
| ---------- | ------- | ---- | ------------------------------------------------------------------------------------------------- |
| `path`     | string  | Yes  | 同期対象ファイルパス（プロジェクトルートからの相対パス）                                          |
| `pattern`  | string  | Yes  | 人間向けのパターン説明。`{version}` プレースホルダを含む。**スクリプトは参照しない**（§3.3 参照） |
| `filter`   | string  | No   | 置換対象を限定する文字列。指定時は `_update_with_filter` が使用される（§3.3 参照）                |
| `optional` | boolean | No   | `true` の場合、ファイルが存在しなくてもエラーにならない。デフォルト: `false`                      |

### 2.3 changelog

```yaml
changelog:
  file: CHANGELOG.md
  format: keep-a-changelog # keep-a-changelog | simple
  git_log_auto: false # true: git log からコミット内容を自動反映
  section_per_target: true # true: target ごとに ### サブセクション
```

### 2.4 git

```yaml
git:
  tag_format: "{target}-v{version}" # マルチターゲット時。単一ターゲットは "v{version}"
  commit_message: "chore: bump {target} to {version}"
  auto_tag: false
  auto_commit: false
```

---

## 3. スクリプト設計

### 3.1 scan_version_targets.py

**責務**: プロジェクトをスキャンしてバージョン関連ファイルのメタデータを JSON で出力する。

**配置**: `plugins/forge/skills/setup-version-config/scripts/scan_version_targets.py`

| 検出対象               | 出力キー              | 検出方法                                                         |
| ---------------------- | --------------------- | ---------------------------------------------------------------- |
| バージョンファイル     | `version_files[]`     | plugin.json, package.json, Cargo.toml, pyproject.toml を再帰探索 |
| カタログファイル       | `catalog_files[]`     | marketplace.json を探索                                          |
| README                 | `readme_files[]`      | プロジェクトルートの README*.md                                  |
| バージョン参照ファイル | `version_ref_files[]` | プロジェクトルートの全テキストファイルを走査（§3.1.1 参照）      |
| CHANGELOG              | `changelog`           | プロジェクトルートの CHANGELOG*.md。形式を自動判定               |

**出力例**:

```json
{
  "project_root": "/path/to/project",
  "version_files": [
    {
      "path": "plugins/forge/.claude-plugin/plugin.json",
      "type": "plugin.json",
      "detected_name": "forge",
      "current_version": "0.0.23"
    }
  ],
  "catalog_files": [
    { "path": ".claude-plugin/marketplace.json", "type": "marketplace.json" }
  ],
  "readme_files": ["README.md", "README_ja.md"],
  "version_ref_files": [
    {
      "path": "CLAUDE.md",
      "references": [{ "name": "forge", "version": "0.0.23" }]
    }
  ],
  "changelog": { "file": "CHANGELOG.md", "format": "keep-a-changelog" }
}
```

**制約**:

- `SKIP_DIRS` でスキャン対象外ディレクトリを除外（node_modules, .git, **pycache** 等）
- 最大探索深度: 6（デフォルト）
- シンボリックリンクの循環を検出して回避

#### 3.1.1 version_ref_files の検出アルゴリズム

**目的**: バージョン参照を持つファイルを漏れなく発見する。事前登録リストに依存しない。

**アルゴリズム**:

1. `version_files` から検出済みの `(name, current_version)` ペアの辞書を構築する
2. プロジェクトルート直下の全ファイルを列挙する（再帰しない）
3. 各ファイルについて以下のフィルタを適用する:
   - **拡張子フィルタ**: `.md`, `.rst`, `.txt`, `.yaml`, `.yml`, `.json`, `.toml` のみ対象。バイナリファイルの誤検出を防ぐ
   - **除外フィルタ**: 他カテゴリで既に検出・管理されるファイルを除外する
     - `readme_files` で検出済みのファイル（README.md 等）
     - `catalog_files` で検出済みのファイル（marketplace.json 等）
     - `version_files` のファイル名（plugin.json 等）
     - CHANGELOG 系ファイル（CHANGELOG.md, HISTORY.md 等の固定リスト）
4. フィルタを通過したファイルを Read し、各 `current_version` 文字列が含まれるか単純文字列マッチで検査する
5. 1つ以上のバージョン文字列を含むファイルを `version_ref_files` として報告する

**走査範囲**: プロジェクトルート直下のみ。サブディレクトリは走査しない。理由:

- sync_files の対象は設定ファイル・ドキュメント等のルートファイルが主である
- サブディレクトリ（docs/, src/ 等）のバージョン参照は偽陽性が多い（テストデータ、設計書の例示等）
- サブディレクトリのバージョン参照が必要な場合は手動で sync_files に追加する

**偽陽性のリスク**: バージョン文字列が短い場合（例: `0.0.1`）、無関係なファイルが誤検出される可能性がある。これは setup-version-config SKILL.md の Step 4（対話的確認）でユーザーが確認・除外する。スクリプトは検出のみを担い、sync_files への追加判断は AI + ユーザーが行う。

**除外理由の整理**:

| 除外対象      | 理由                                                 |
| ------------- | ---------------------------------------------------- |
| readme_files  | 専用の README 処理ルールで sync_files に追加される   |
| catalog_files | 専用の catalog 処理ルール（filter 付き）で追加される |
| version_files | version_file 自体は `--version-path` で更新される    |
| CHANGELOG     | SKILL.md の Step 6 で専用処理される                  |

### 3.2 calculate_version.py

**責務**: 現在のバージョンとバンプ種別から新バージョンを計算する。

**配置**: `plugins/forge/skills/update-version/scripts/calculate_version.py`

**入力**: `<current_version> <version_spec>`

| version_spec | 動作                                                  | 例              |
| ------------ | ----------------------------------------------------- | --------------- |
| `patch`      | パッチ番号をインクリメント                            | 0.0.23 → 0.0.24 |
| `minor`      | マイナー番号をインクリメント、パッチを 0 に           | 0.0.23 → 0.1.0  |
| `major`      | メジャー番号をインクリメント、マイナー・パッチを 0 に | 0.0.23 → 1.0.0  |
| `X.Y.Z`      | 直接指定                                              | 0.0.23 → 1.0.0  |

**出力**: JSON

```json
{ "status": "ok", "current": "0.0.23", "new": "0.0.24", "spec": "patch" }
```

**警告**: `new_version ≦ current_version` の場合、`warning` フィールドを付与する。

### 3.3 update_version_files.py

**責務**: ファイル内のバージョン文字列を置換し、更新後の内容を stdout に出力する。元ファイルは書き換えない。

**配置**: `plugins/forge/skills/update-version/scripts/update_version_files.py`

**入力**: `<file_path> <old_version> <new_version> [--version-path <path>] [--filter <pattern>]`

**出力**:

- stdout: 更新後のファイル内容
- stderr: JSON ステータス `{"status": "ok", "file": "...", "old": "...", "new": "..."}`

#### 3つの置換モード

引数の組み合わせで3つのモードが決まる:

| モード            | 条件                  | 関数                    | 用途                                              |
| ----------------- | --------------------- | ----------------------- | ------------------------------------------------- |
| **filter モード** | `--filter` あり       | `_update_with_filter()` | marketplace.json, README 等の複数 target ファイル |
| **path モード**   | `--version-path` あり | `_update_with_path()`   | version_file（plugin.json 等）のフィールド更新    |
| **simple モード** | どちらもなし          | `_replace_first()`      | 単一 target ファイル（フォールバック）            |

**優先順位**: filter > path > simple

#### filter モードの動作

```
_update_with_filter(content, old_version, new_version, filter_pattern, max_distance=10)
```

1. ファイルを行単位で走査する
2. `filter_pattern in line` がマッチした行で `in_block = True` にする
3. ブロック内（filter 行から最大 `max_distance` 行以内）で `old_version in line` がマッチした行を置換する
4. 置換は1回のみ（最初のマッチで終了）
5. filter がマッチしたがバージョンが見つからない場合は `ValueError`

**重要**: `pattern` フィールドはスクリプトに渡されない。スクリプトは `old_version` の文字列置換を行い、`filter` で対象行を限定する。`pattern` は人間が設定を読む際の参考情報である。

#### filter が必要なケース [MANDATORY]

同一ファイル内に複数 target のバージョンが記載されるファイルには **必ず `filter` を指定する**:

| ファイル種別          | 理由                               | filter 例           |
| --------------------- | ---------------------------------- | ------------------- |
| marketplace.json      | 複数プラグインの version が存在    | `'"name": "forge"'` |
| README.md テーブル    | 複数プラグインのバージョン行が存在 | `'\| **forge**'`    |
| README_ja.md テーブル | 同上                               | `'\| **forge**'`    |

**filter がない場合**: `_replace_first()` が使用され、ファイル内の最初のマッチのみ置換される。同一バージョン番号の target が複数存在すると、意図しない行が置換される危険がある。

**安全指針**: target が単一であっても、将来の target 追加に備えて filter を付与することを推奨する。

#### path モードの動作

```
_update_with_path(content, old_version, new_version, version_path)
```

1. `version_path` の最終キー名を含む行を正規表現で検索
2. ネストパスの場合は親キーの後の最初のマッチを置換
3. JSON の `"version": "X.Y.Z"` と TOML の `version = "X.Y.Z"` の両方に対応

---

## 4. ワークフロー

### 4.1 setup-version-config ワークフロー

```
Step 1: 既存 .version-config.yaml の確認
    ↓ (存在しない or 再生成)
Step 2: scan_version_targets.py でプロジェクトスキャン
    ↓
Step 3: スキャン結果から設定草案を生成
    ↓
Step 4: AskUserQuestion で草案を確認
    ↓
Step 5: .version-config.yaml を Write
    ↓
Step 6: 結果表示と次のステップ案内
```

#### 設定草案の生成ルール

**targets**: 各 version_file を1つの target として定義する。

**sync_files の構成**:

1. **catalog_files** → 各 target の sync_files に追加。filter で `"name": "{name}"` を指定
2. **readme_files** → README を Read してバージョン記載パターンを確認し追加。filter で target 名の行識別子を指定
3. **optional**: 存在しない可能性があるファイル（README_ja.md 等）には `optional: true`

**changelog**: 検出された場合に追加。`git_log_auto: false` をデフォルト。

**git**: マルチターゲットの場合は `tag_format: "{target}-v{version}"`、単一の場合は `"v{version}"`。

### 4.2 update-version ワークフロー

```
Step 1: .version-config.yaml を Read
    ↓
Step 2: 引数解析（target_name, version_spec）
    ↓
Step 3: version_file から現在のバージョンを Read
    ↓
Step 4: calculate_version.py で新バージョンを算出
    ↓
Step 5: 変更内容の収集（バージョン更新前にコミット履歴から CHANGELOG エントリを生成）
    ↓
Step 6: ファイル更新
    6-1. version_file を update_version_files.py --version-path で更新
    6-2. sync_files を順次 update_version_files.py [--filter] で更新
    ↓
Step 7: CHANGELOG 挿入（Step 5 で生成したエントリを挿入）
    ↓
Step 8: テスト実行（tests/ が存在する場合）
    ↓
Step 9: git 操作（auto_commit/auto_tag が有効な場合）
```

> **Step 5 → 6 → 7 の順序が重要**: 変更内容の収集（Step 5）はバージョンファイル更新（Step 6）より前に実行する。バージョン番号を書き換える前にコミット履歴を収集することで、バージョン番号変更のノイズが混入しない。

#### Step 5: 変更内容の収集

前バージョンからのコミット履歴を取得し、CHANGELOG エントリを生成する:

1. **タグが存在する場合**: `git log {prev_tag}..HEAD --pretty=format:"%s" --no-merges`
2. **タグが存在しない場合**: CHANGELOG.md の前バージョンエントリ日付から `git log --after="{date}" --pretty=format:"%s" --no-merges`
3. **いずれもない場合**: `git log --oneline -30 --no-merges` で直近のコミットを取得し、AskUserQuestion で範囲を確認

AI がコミットメッセージを Conventional Commits 形式で分類し、意味のある単位でグループ化して CHANGELOG エントリを生成する。空テンプレート（`-` のみ）は禁止。

#### Step 6 の詳細

version_file と各 sync_file に対して `update_version_files.py` を呼び出す:

| 対象                    | 引数                            | モード        |
| ----------------------- | ------------------------------- | ------------- |
| version_file            | `--version-path {version_path}` | path モード   |
| sync_file (filter あり) | `--filter "{filter}"`           | filter モード |
| sync_file (filter なし) | （追加引数なし）                | simple モード |

スクリプトの stdout を Write でファイルに書き出す。stderr の JSON で `status: "error"` の場合はエラー報告して終了。

#### Step 7: CHANGELOG 挿入

Step 5 で生成したエントリを CHANGELOG ファイルの最初の `## [` 行の直前に挿入する。

**keep-a-changelog 形式**:

```markdown
## [{new_version}] - {YYYY-MM-DD}

### {target_name}

- **feat**: [機能の説明]
- **fix**: [修正の説明]
```

**`git_log_auto: true`**: 前バージョンのタグから HEAD までの git log を取得し、Conventional Commits 形式に従って分類する。

| コミットタイプ   | CHANGELOG セクション |
| ---------------- | -------------------- |
| `feat:`          | ### Added            |
| `fix:`           | ### Fixed            |
| `chore:`, その他 | ### Changed          |

`section_per_target: true` の場合、各 target の変更を `### {target_name}` サブセクションに分ける。

---

## 5. エラーハンドリング

### 5.1 setup-version-config

| エラー                | 対応                                                 |
| --------------------- | ---------------------------------------------------- |
| スキャン結果が空      | 「バージョンファイルが見つかりません」と報告して終了 |
| README のパターン不明 | AI が判断し、AskUserQuestion で確認                  |

### 5.2 update-version

| エラー                              | 対応                                                    |
| ----------------------------------- | ------------------------------------------------------- |
| `.version-config.yaml` がない       | エラー表示。`/forge:setup-version-config` の実行を案内  |
| 不正な target 名                    | 有効な target 名の一覧を表示して終了                    |
| 不正なバージョン形式                | 「バージョン形式が不正です（例: 1.2.3）」と表示して終了 |
| 新バージョン ≦ 現バージョン         | AskUserQuestion で続行を確認                            |
| sync_file 不在 + optional: true     | スキップ（警告なし）                                    |
| sync_file 不在 + optional: false    | 警告を表示して次のファイルへ（処理は継続）              |
| filter ブロック内にバージョン未発見 | ValueError。エラー報告して終了                          |
| テスト失敗                          | 失敗内容を表示し AskUserQuestion で続行を確認           |

---

## 6. テスト

### テストファイル一覧

| ファイル                                                        | テスト数 | 対象                    |
| --------------------------------------------------------------- | -------- | ----------------------- |
| `tests/forge/setup-version-config/test_scan_version_targets.py` | ~40      | scan_version_targets.py |
| `tests/forge/update-version/test_calculate_version.py`          | ~20      | calculate_version.py    |
| `tests/forge/update-version/test_update_version_files.py`       | ~30      | update_version_files.py |

### 主要テストケース

#### update_version_files.py

| カテゴリ      | ケース                                                   |
| ------------- | -------------------------------------------------------- |
| simple モード | クォート付き/なしの置換、バージョン未発見エラー          |
| path モード   | トップレベル/ネストフィールドの置換                      |
| filter モード | ブロック内置換、max_distance 超過、filter 未マッチエラー |
| CLI           | stdout/stderr 分離、終了コード                           |

---

## 7. 影響範囲

| ファイル                                                                    | 役割                               |
| --------------------------------------------------------------------------- | ---------------------------------- |
| `.version-config.yaml`                                                      | 設定ファイル（プロジェクトルート） |
| `plugins/forge/skills/setup-version-config/SKILL.md`                        | 設定生成ワークフロー               |
| `plugins/forge/skills/setup-version-config/scripts/scan_version_targets.py` | プロジェクトスキャン               |
| `plugins/forge/skills/update-version/SKILL.md`                              | バージョン更新ワークフロー         |
| `plugins/forge/skills/update-version/scripts/calculate_version.py`          | バージョン計算                     |
| `plugins/forge/skills/update-version/scripts/update_version_files.py`       | ファイル更新                       |
