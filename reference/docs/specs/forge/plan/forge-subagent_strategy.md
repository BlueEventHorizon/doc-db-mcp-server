# forge-subagent 実装戦略書

## メタデータ

| 項目         | 値                                            |
| ------------ | --------------------------------------------- |
| 機能名       | forge-subagent                                |
| 設計書       | DES-029_skill_agent_launch_contract_design.md |
| 作成日       | 2026-05-26                                    |
| 対象ブランチ | refactor/forge-subagent                       |

---

## 1. 実装の全体構造と方針

### 何を実装するか

`/forge:review` 配下の 5 SKILL の起動契約を整理し、reviewer / evaluator / fixer を **fork 型 SKILL** として再定義する。

- **現状 (As-Is)**: reviewer / evaluator / fixer は継承型 SKILL として定義されており、`/forge:review` から汎用 Agent ツール（`Agent` ツール）で起動されている
- **目標 (To-Be)**: 3 SKILL を fork 型 SKILL（`context: fork`）として再定義し、`Skill` ツールで呼び出す

### なぜこの順で実装するか

設計書 §11 の移行計画は「**安全制約が不完全な中間状態を作らない**」という原則に基づいている。

- **Step 1 が最初**: fork 安全機能（context: fork / Role 否定的制約 / 引数解釈ガード / 自己再帰禁止）を先に全適用し、中間状態での安全リスクをゼロにする
- **Step 4 が並列可能**: mark_fixed.py の責務移転（fixer → 呼び出し元）は caller/callee の両側を同一段階で変更しないと不整合になる。Step 2〜3 と並列可能だが Step 1 完了後のみ
- **Step 5 が後回し**: review/SKILL.md の起動方法変更（Agent → Skill ツール）は、呼び出し先 3 SKILL の改訂が完了してから行う
- **Step 7 (テスト) が最後**: 全 SKILL 改訂後にまとめてテストを追加する（テスト先行は過検知になる）

### 実装スコープ全体

| 対象ファイル                                                           | 変更種別                                                    |
| ---------------------------------------------------------------------- | ----------------------------------------------------------- |
| `docs/specs/common/design/COMMON-DES-001_skill_base_design.md`         | §6 リストに 3 行追加                                        |
| `plugins/forge/skills/reviewer/SKILL.md`                               | fork 型化 + 文言改訂                                        |
| `plugins/forge/skills/evaluator/SKILL.md`                              | fork 型化 + 文言改訂                                        |
| `plugins/forge/skills/fixer/SKILL.md`                                  | fork 型化 + mark_fixed.py 呼び出し削除                      |
| `plugins/forge/skills/review/SKILL.md`                                 | 起動方法変更 + 軽量経路 mark_fixed.py 削除 + 経路分岐表追記 |
| `plugins/forge/skills/present-findings/SKILL.md`                       | fixer 起動を Skill ツール (fork) に変更                     |
| `tests/forge/subagent/` (新規ディレクトリ)                             | TEST-S001〜S005 + frontmatter 検証                          |
| `docs/specs/forge/design/DES-015_review_workflow_design.md`            | 経路分岐表追記                                              |
| `docs/specs/forge/design/DES-028_review_policy_design.md`              | 軽量経路手順更新                                            |
| `docs/specs/forge/requirements/REQ-005_skill_agent_launch_contract.md` | TBD-002〜005 解消                                           |

---

## 2. フェーズ分割

| フェーズ | ゴール                           | 設計書ステップ  | 並列実行                           |
| -------- | -------------------------------- | --------------- | ---------------------------------- |
| Phase 1  | fork 安全機能の全適用            | Step 1          | 単独（依存なし）                   |
| Phase 2  | 誤読源文言の削除                 | Step 2 + Step 3 | 2 と 3 は並列可                    |
| Phase 3  | mark_fixed.py 責務移転の完結     | Step 4          | 単独（Phase 1 後）                 |
| Phase 4  | オーケストレーターの起動方法変更 | Step 5 + Step 6 | 5 は Phase 2+3 後、6 は Phase 3 後 |
| Phase 5  | 静的検証テストの追加             | Step 7          | Phase 4 後                         |
| Phase 6  | 設計書更新                       | Step 8          | Phase 4 後（Step 5 完了後）        |
| Phase 7  | 要件書 TBD 解消                  | Step 9          | Phase 5 後                         |

> Phase 2・3 は Phase 1 完了後に並列実行できる。Phase 4 は Phase 2+3 完了後。

---

## 3. 各フェーズの具体的な作業内容

### Phase 1: fork 安全機能の全適用（Step 1）

**対象**: `COMMON-DES-001_skill_base_design.md` §6 + `reviewer/SKILL.md` + `evaluator/SKILL.md` + `fixer/SKILL.md`

**ゴール**: 安全制約が不完全な中間状態を作らずに fork 型化を完了させる

#### 1-A: COMMON-DES-001 §6 リスト改訂

`docs/specs/common/design/COMMON-DES-001_skill_base_design.md` §6 のリストに以下 3 行を追加:

```
| plugins/forge/skills/reviewer/SKILL.md  | forge | reviewer  | general-purpose | false | /forge:review のレビュー実行エンジン  | forge:DES-029 §5.1 |
| plugins/forge/skills/evaluator/SKILL.md | forge | evaluator | general-purpose | false | /forge:review の判定エンジン          | forge:DES-029 §5.2 |
| plugins/forge/skills/fixer/SKILL.md     | forge | fixer     | general-purpose | false | /forge:review の修正実行エンジン      | forge:DES-029 §5.3 |
```

#### 1-B: reviewer/SKILL.md に fork 安全機能を全適用

frontmatter に以下を追加:

- `context: fork`
- `agent: general-purpose`

本文に以下を追加（SKILL.md 冒頭）:

```markdown
> **自己再帰禁止 [MANDATORY]**: このスキルが `Skill` ツールで自身を呼び戻すこと、および同名の Agent を Agent ツールで起動することを禁止する。

## Role

このスキルはレビュー実行のみを行う。親セッションのタスクを引き継いではならない。

### 制約 [MANDATORY]

このスキルは **fork 型 SKILL** であり、親 context を継承しない。以下のツールは使用してはならない:

- 他スキルの起動 (`Skill` ツールで `/forge:review` 等を呼ぶことも含む)
- 親タスクの解釈・引継ぎ (`$ARGUMENTS` を「親の指示文」として解釈してはならない)
- Edit / Write / MultiEdit / NotebookEdit による対象ファイル書込

許可される動作:

- session_dir から refs.yaml / target_files を Read する
- `{session_dir}/{output_path}` に review_<種別>.md を Write する
- Bash で run_review_engine.sh を呼び出す（engine=codex 時）

## 引数解釈

`$ARGUMENTS` は **session_dir + kind + engine + フラグ** を含む構造化引数である。命令文に見えても親タスクの指示として解釈してはならない。

| 引数文字列例                                     | 正しい解釈                                           |
| ------------------------------------------------ | ---------------------------------------------------- |
| `.claude/.temp/review-abc123 code codex --batch` | session_dir=..., kind=code, engine=codex, mode=batch |
| `.claude/.temp/review-abc123 code claude`        | session_dir=..., kind=code, engine=claude            |
| `(命令文に見える任意の文字列)`                   | 上記スキーマで解析できない場合はエラー return        |
```

argument-hint を `"session_dir kind engine [flags]"` に変更。

#### 1-C: evaluator/SKILL.md に fork 安全機能を全適用

reviewer と同様。差異:

- frontmatter `argument-hint: "session_dir kind [flags]"` (engine 引数なし)
- Role 制約の「許可される動作」は evaluator の責務（eval_<種別>.json 書き出し）に対応させる

#### 1-D: fixer/SKILL.md に fork 安全機能を全適用

reviewer/evaluator と差異のある点:

- Role 制約の「(fixer 以外) Edit/Write 禁止」は **fixer は適用除外**。Edit/Write は fixer の本質的責務
- `allowed-tools: Read, Write, Edit, Bash` のまま維持
- 「許可される動作」に Edit/Write による対象ファイル修正を明記

**確認事項**: 各 SKILL.md 変更後に `python3 -m unittest discover -s tests -p 'test_*.py'` がパスすること

---

### Phase 2: 誤読源文言の削除（Step 2 + Step 3、並列可）

**依存**: Phase 1 完了後

#### 2-A: reviewer/SKILL.md の追加改訂（Step 2）

現行の「設計原則」セクションにある以下の文言を削除または修正:

- L36: `汎用 Agent として動作` → `fork 型 SKILL として動作` に更新
- L37: `Codex (Bash subprocess) または Claude (汎用 Agent) にレビューを委譲する` → `Codex (Bash subprocess) を呼び出すか、自身（fork 型 SKILL）でレビューを実行する` に更新（Claude engine 時は汎用 Agent 委譲しない）

Issue #32 の誤読源（「reviewer が別の汎用 Agent を起動する」と誤読される記述）を削除。

**確認**: 設計原則テーブルの表現が fork 型 SKILL の動作モデルと整合していること

#### 2-B: evaluator/SKILL.md の追加改訂（Step 3、Step 2 と並列可）

現行の「設計原則」セクションにある以下の文言を修正:

- L23: `汎用 Agent として動作` → `fork 型 SKILL として動作` に更新
- L23 補足: `/forge:review` から汎用 Agent として起動されるという記述を削除し、Skill ツール (fork) で起動されると変更

**確認事項**: 変更後もテストがパスすること

---

### Phase 3: mark_fixed.py 責務移転の完結（Step 4）

**依存**: Phase 1 完了後（Phase 2 と並列可）

**重要**: caller/callee の両側を同一段階で変更することで中間不整合を防ぐ

#### 3-A: fixer/SKILL.md の追加改訂

- 「汎用 Agent への委譲」記述を「自身で Edit/Write」に変更
  - 現行: `汎用 Agent (general-purpose) に実際の Edit/Write を委譲する`
  - 変更後: `自身 (fork 型 SKILL) が直接 Edit/Write を実行する`
- `mark_fixed.py` の呼び出し手順を削除（呼び出し元の責務として整理）
- 出力欄に `patch_result.json` への書き込みを明記
- return スキーマを DES-029 §6.5 に合わせて追記:
  ```
  {status, patched_ids, failed_ids, files_modified, error_message?}
  ```

#### 3-B: review/SKILL.md の軽量経路 mark_fixed.py 直呼び削除

- 軽量経路 (FNC-413) での `mark_fixed.py` 直接呼び出し手順を削除
- 代わりに「単独修正レビュー後に呼び出し元の責務として mark_fixed.py を呼ぶ」契約に変更

#### 3-C: present-findings/SKILL.md の対応変更

- fixer 完了後の `mark_fixed.py` 呼び出し手順を削除
- 代わりに「fixer return 後、単独修正レビューを完了してから mark_fixed.py を呼ぶ」フローに変更

**確認**: 軽量経路・fixer 経路両方で mark_fixed.py が適切なタイミングで呼ばれる契約になっていること

---

### Phase 4: オーケストレーターの起動方法変更（Step 5 + Step 6）

#### 4-A: review/SKILL.md の起動方法変更（Step 5）

**依存**: Phase 2 + Phase 3 完了後

- reviewer / evaluator / fixer の起動を `Agent ツール` → `Skill ツール (fork)` に変更
  - 変更前: `汎用 Agent として /forge:reviewer を起動する`
  - 変更後: `Skill ツール (fork) で reviewer を呼び出す (args: session_dir kind engine [flags])`
- `allowed-tools` から `Agent` を削除（fork 型 SKILL 起動に `Agent` ツールは不要）
- 経路分岐表（DES-029 §7）を追記:

```markdown
## 修正経路分岐表

| # | 経路名             | 起動方法              | context 消費    | 用途                                   | 適用条件                                                                                |
| - | ------------------ | --------------------- | --------------- | -------------------------------------- | --------------------------------------------------------------------------------------- |
| 1 | 軽量経路 (FNC-413) | (起動なし、Edit 直接) | 親 context 消費 | 件数小・auto_fixable な finding の修正 | recommendation=fix AND status∈{pending,in_progress} の件数 ≤ 3 AND 全 auto_fixable=true |
| 2 | fork 型 fixer 経路 | Skill ツール (fork)   | 遮断            | 件数多または非 auto_fixable の修正     | 軽量経路の条件を満たさない場合                                                          |
```

**注意**: `allowed-tools` の `Agent` 削除は破壊的変更。Phase 2+3 完了後にのみ行う。

#### 4-B: present-findings/SKILL.md の残り改訂（Step 6）

**依存**: Phase 3 完了後（Phase 4-A と独立して進められる）

- fixer 起動を `汎用 Agent (general-purpose)` → `Skill ツール (fork)` に変更
- 設計原則テーブルの「fixer は汎用 Agent 経由」→「fixer は Skill ツール (fork) 経由」に更新
- `allowed-tools` に `Skill` が含まれていることを確認（現行で既に含まれている）

**確認**: `allowed-tools: Read, Write, Bash, AskUserQuestion, Skill` で `Agent` が不要なことを確認

---

### Phase 5: 静的検証テストの追加（Step 7）

**依存**: Phase 4 完了後

**新規ディレクトリ**: `tests/forge/subagent/`

#### 5-A: TEST-S001 — Agent ツール記述と allowed-tools Agent の整合検証

`tests/forge/subagent/test_agent_allowedtools_consistency.py`

検証ロジック:

- `plugins/` 配下の SKILL.md を全スキャン
- 本文（コードブロック・制約セクション除外後）に「Agent ツール」「汎用 Agent を起動」「カスタム Agent を起動」があれば、frontmatter `allowed-tools` に `Agent` が含まれることを確認
- 除外スコープ: fenced コードブロック、`### 制約` / `### 禁止事項` 見出し配下

#### 5-B: TEST-S002 — Skill 呼出記述と allowed-tools Skill の整合検証

`tests/forge/subagent/test_skill_allowedtools_consistency.py`

検証ロジック:

- 本文に `/forge:<skill>` / `/anvil:<skill>` 表記があれば、`allowed-tools` に `Skill` が含まれることを確認
- 除外スコープ: fenced コードブロック、制約セクション

#### 5-C: TEST-S003 — 旧 perspective ファイル参照の除去確認

`tests/forge/subagent/test_legacy_perspective_removed.py`

検証ロジック:

- `plugins/forge/` / `docs/readme/forge/` 配下に `review_{perspective}.md` 文字列が存在しないことを確認
- OBSOLETE マーカー付きは除外

#### 5-D: TEST-S004 — `subagent` 単独使用の警告

`tests/forge/subagent/test_subagent_term_usage.py`

検証ロジック:

- SKILL.md 内で `subagent` が単独使用されている箇所を列挙（warning として出力、CI は fail させない）

#### 5-E: TEST-S005 — Slash command 起動経路の明示確認

`tests/forge/subagent/test_slash_command_launch_context.py`

検証ロジック:

- Agent prompt として展開されるテキストブロック内に `/forge:<skill>` 表記がある場合、近傍 5 行内に起動経路の明示があることを確認
- 誤検知許容: `[KNOWN-FP]` マーカーで除外可能（warning 段階、CI fail なし）

#### 5-F: fork 型 SKILL frontmatter 検証

既存テストの拡張または `tests/forge/subagent/test_fork_skill_frontmatter.py` として新規作成

検証内容:

- reviewer / evaluator / fixer の frontmatter に `context: fork` が含まれる
- reviewer / evaluator / fixer の frontmatter に `agent: general-purpose` が含まれる
- reviewer / evaluator の本文に Edit/Write 禁止の制約文言がある
- reviewer / evaluator / fixer の本文に「親タスクを引き継がない」旨の Role 制約がある
- reviewer / evaluator / fixer の本文に「自己再帰禁止」の明示がある

---

### Phase 6: 設計書更新（Step 8）

**依存**: Phase 4 完了後

#### 6-A: DES-015 への経路分岐表追記

`docs/specs/forge/design/DES-015_review_workflow_design.md`

- DES-029 §7 の修正経路分岐表を追記
- 追記箇所: レビューワークフローの修正フェーズ（Phase 6 前後）

#### 6-B: DES-028 の軽量経路手順更新

`docs/specs/forge/design/DES-028_review_policy_design.md`

- 軽量経路手順の Step 4 を新 fixed 遷移契約（呼び出し元の責務）に更新
  - 変更前: fixer 内 または 軽量経路内で mark_fixed.py を呼ぶ
  - 変更後: 単独修正レビュー後に呼び出し元（review / present-findings）が mark_fixed.py を呼ぶ

---

### Phase 7: 要件書 TBD 解消（Step 9）

**依存**: Phase 5 完了後

`docs/specs/forge/requirements/REQ-005_skill_agent_launch_contract.md` の以下の TBD を解消:

| TBD ID  | 内容                         | 解消方法                                             |
| ------- | ---------------------------- | ---------------------------------------------------- |
| TBD-002 | 静的テスト判定ロジックの詳細 | TEST-S001〜S005 の実装内容（Phase 5）を参照して記述  |
| TBD-003 | OBSOLETE マーカー運用ルール  | TEST-S003 の除外ロジックから運用ルールを逆算して記述 |
| TBD-004 | 検出ヒューリスティクス       | TEST-S002 / TEST-S005 の境界定義から記述             |
| TBD-005 | (内容確認後に対応)           | DES-029 §9 の対応内容に集約                          |

---

## 4. テスト戦略

### テストタイミング

| フェーズ         | テスト内容                                | 実行方法                                                  |
| ---------------- | ----------------------------------------- | --------------------------------------------------------- |
| 各フェーズ完了後 | 既存テスト全件パス確認                    | `python3 -m unittest discover -s tests -p 'test_*.py' -v` |
| Phase 4 完了後   | `/forge:review code --auto` 手動 E2E 確認 | 手動実行                                                  |
| Phase 5 完了後   | 新規静的検証テスト全件パス確認            | 同上                                                      |

### テストの新規作成ルール

- `tests/forge/subagent/` ディレクトリに配置（新規作成）
- Python 標準ライブラリのみ使用
- 命名規則: `test_{検証内容}.py`
- `__init__.py` を `tests/forge/subagent/` に追加

### 既存テストへの影響確認

- `tests/forge/review/` 配下: Agent ツール起動を前提とした統合テストがあれば Skill ツール (fork) 起動に置き換える（Phase 5 で確認）
- `tests/common/` 配下: マニフェスト整合性テストへの影響なし（SKILL.md の変更はマニフェストに影響しない）

---

## 5. リスクと注意点

### R-1: fork 境界での AskUserQuestion 制限

**リスク**: reviewer / evaluator / fixer は `context: fork` のため、AskUserQuestion が使えない可能性がある。

**対策**: 想定外の入力エラー時は `return {status: "error", error_message: "..."}` で orchestrator に判断を委ねる設計とする（DES-029 §12.1 に明記済み）。

### R-2: mark_fixed.py 責務移転による呼び出し元の変更漏れ

**リスク**: Phase 3 で fixer の mark_fixed.py 削除と呼び出し元の追加を同時に行わないと、fixed 遷移が一切行われなくなる。

**対策**: Step 4 の「caller/callee 同一段階で完結」という設計方針を厳守。Phase 3 は 3-A / 3-B / 3-C を1コミットで完結させる。

### R-3: review/SKILL.md の allowed-tools Agent 削除

**リスク**: review/SKILL.md から `Agent` を `allowed-tools` から削除するタイミングを誤ると、まだ Agent ツールで起動しているコードパスが壊れる。

**対策**: Phase 4-A (Step 5) で行う。この時点で reviewer / evaluator / fixer は全て Skill ツール (fork) に変更済みであること。Phase 2+3 完了後にのみ実施。

### R-4: 設計書参照の一時的不整合

**リスク**: Phase 1〜4 の SKILL.md 変更中は、設計書 DES-015 / DES-028 がまだ旧起動方法を記述している状態になる。

**対策**: 設計書更新は Phase 6（Phase 4 後）で行うため、実装中の不整合は「移行計画に基づく意図的な状態」として許容する。

### R-5: TEST-S005 の誤検知

**リスク**: review 配下以外の SKILL.md / 設計書に残る `/forge:*` 表記が TEST-S005 で大量に誤検知される可能性がある。

**対策**: TEST-S005 は warning 段階（CI を fail させない）。誤検知箇所は `[KNOWN-FP]` マーカーで除外可能。Phase 5 実装時に誤検知数を確認し、閾値設定を検討する。

### R-6: COMMON-DES-001 §6 の権限確認

**リスク**: COMMON-DES-001 §6 のリスト変更手順（§6.3）に「PR で判断基準明示 → レビュー後にマージ」という手順がある可能性がある。

**対策**: Phase 1 実施前に COMMON-DES-001 §6.3 の手順を確認し、必要があればレビュープロセスを踏む。

---

## 6. 実装順序サマリー

```
Phase 1 (Step 1)
  └─ COMMON-DES-001 §6 リスト追加
  └─ reviewer/SKILL.md: fork 安全機能全適用
  └─ evaluator/SKILL.md: fork 安全機能全適用
  └─ fixer/SKILL.md: fork 安全機能全適用
       ↓
Phase 2 (Step 2+3, 並列可) ─────────── Phase 3 (Step 4, 並列可)
  ├─ reviewer/SKILL.md: 誤読源文言削除    ├─ fixer/SKILL.md: Agent委譲→自身で Edit
  └─ evaluator/SKILL.md: 誤読源文言削除  ├─ review/SKILL.md: 軽量経路 mark_fixed 削除
                                          └─ present-findings/SKILL.md: 対応変更
       ↓ (Phase 2+3 完了後)
Phase 4
  ├─ review/SKILL.md: 起動方法変更 (Step 5)
  └─ present-findings/SKILL.md: 残り改訂 (Step 6)
       ↓
Phase 5 (Step 7)    ── Phase 6 (Step 8)
  └─ 静的検証テスト追加  └─ DES-015 + DES-028 更新
       ↓
Phase 7 (Step 9)
  └─ REQ-005 TBD 解消
```
