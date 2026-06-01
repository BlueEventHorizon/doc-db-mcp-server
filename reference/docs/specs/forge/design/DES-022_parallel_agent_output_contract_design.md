# DES-022 並列 agent 出力契約パターン 設計書

## メタデータ

| 項目   | 値         |
| ------ | ---------- |
| 設計ID | DES-022    |
| 作成日 | 2026-03-22 |

---

> 対象プラグイン: forge | 適用範囲: 全オーケストレーター

---

## 1. 概要

Claude Code の Agent ツールで複数の汎用 Agent を並列起動する場合、共有リソース（YAML/JSON ファイル等）への同時書き込みが競合する問題がある。Agent ツールは OS プロセスレベルの排他制御を提供しないため、並列 Agent が同一ファイルに Write すると後勝ちで上書きされ、先の Agent の結果が消失する。

本設計書は、この問題を根本的に排除する**出力契約パターン**を定義する。

---

## 2. 問題

### 2.1 並列書き込み競合

```
orchestrator
  ├─ Agent A ──Write──→ plan.yaml  ← 書き込み①
  └─ Agent B ──Write──→ plan.yaml  ← 書き込み②（①を上書き）
```

Agent A と Agent B が同時に `plan.yaml` を更新すると、最後に Write した agent の内容のみが残る。Read-Modify-Write の間に他の agent が割り込む典型的な race condition である。

### 2.2 Claude Code 環境での制約

- Agent ツールは独立したサブプロセスとして実行される
- ファイルロック機構は提供されない
- agent 間のメッセージパッシングは Agent ツールの戻り値のみ（完了通知）

---

## 3. 出力契約パターン [MANDATORY]

### 3.1 基本原則

**並列実行される agent は共有リソースに直接書き込まない。**

代わりに以下の3ステップに従う:

1. **各 agent は個別の結果ファイルを Write する**（書き込み先が重ならない）
2. **各 agent は完了通知のみを orchestrator に返す**（Agent ツールの戻り値）
3. **orchestrator は全 agent の完了後に結果ファイルを収集し、共有リソースを1回だけ更新する**

```
orchestrator
  ├─ Agent A ──Write──→ result_a.json  ← 個別ファイル（競合なし）
  └─ Agent B ──Write──→ result_b.json  ← 個別ファイル（競合なし）
  │
  ▼ 全 agent 完了後
  orchestrator ──収集──→ result_a.json + result_b.json
               ──更新──→ plan.yaml（1回だけ）
```

### 3.2 結果ファイルの命名規則

結果ファイル名には agent の識別子を含め、書き込み先が一意になることを保証する:

| パターン                      | 例                                        |
| ----------------------------- | ----------------------------------------- |
| `{prefix}_{identifier}.{ext}` | `review_logic.md`, `eval_resilience.json` |
| `{prefix}_{index}.{ext}`      | `result_0.json`, `result_1.json`          |

**禁止**: 複数の agent が同一ファイル名に書き込むこと。

### 3.3 orchestrator の責務

| 責務                     | 説明                                                         |
| ------------------------ | ------------------------------------------------------------ |
| 個別ファイルの命名を指示 | 各 agent に一意な出力先を渡す                                |
| 全 agent の完了を待機    | `run_in_background` で起動した場合、全通知を受け取るまで待つ |
| 結果ファイルの収集       | glob または明示的パスで収集する                              |
| 共有リソースの一括更新   | スクリプト（`--batch` モード等）で1回だけ更新する            |
| エラーハンドリング       | 一部の agent が失敗した場合の部分マージ戦略を決定する        |

### 3.4 agent の責務

| 責務                         | 説明                                              |
| ---------------------------- | ------------------------------------------------- |
| 指示された出力先にのみ Write | 共有リソースには書き込まない                      |
| 自己完結した結果を出力       | orchestrator が後処理できる構造化データを出力する |
| 読み取りは自由               | 共有リソースの Read は競合しないため制限なし      |

---

## 4. 適用例

### 4.1 review パイプライン — reviewer (歴史的例)

> 注: 現行 reviewer は **1 起動原則** (DES-028 / REQ-004 FNC-412) に統一されており、観点軸での並列分割は撤廃されている。以下は本設計が制定された当時の例示として保存する。並列出力契約の 3 原則 (個別書き込み / 完了通知のみ / オーケストレータ一括更新) 自体は現行でも有効。

```
(旧設計の例)
review orchestrator
  ├─ reviewer(logic)          → review_logic.md
  ├─ reviewer(resilience)     → review_resilience.md
  └─ reviewer(maintainability)→ review_maintainability.md
  │
  ▼ 全 reviewer 完了後
  extract_review_findings.py → review.md + plan.yaml
```

現行の review パイプラインは reviewer 1 起動 → `review_<種別>.md` を 1 ファイルに出力する。

### 4.2 review パイプライン — evaluator (歴史的例)

> 注: 現行 evaluator も reviewer の 1 起動に対応して 1 起動で動作する (DES-028 §4.3)。以下は本設計当時の例示として保存する。

```
(旧設計の例)
review orchestrator
  ├─ evaluator(logic)          → eval_logic.json
  ├─ evaluator(resilience)     → eval_resilience.json
  └─ evaluator(maintainability)→ eval_maintainability.json
  │
  ▼ 全 evaluator 完了後
  update_plan.py --batch → plan.yaml（1回だけ更新）
```

### 4.3 start-implement — コンテキスト収集

```
start-implement orchestrator
  ├─ rules agent  → refs/rules.yaml
  └─ code agent   → refs/code.yaml
  │
  ▼ 全 agent 完了後
  orchestrator が refs/ を読み取り、task-executor に渡す
```

---

## 5. 一括マージの実装パターン

### 5.1 スクリプトによるバッチ更新

```bash
# 結果ファイルを収集して plan.yaml を1回で更新
cat eval_*.json | python3 update_plan.py {session_dir} --batch
```

スクリプトは `{"updates": [...]}` 形式と JSON 配列 `[...]` 形式の両方を受け付ける。

### 5.2 専用スクリプトによる統合

```bash
# 複数の review_*.md を統合して review.md + plan.yaml を生成
python3 extract_review_findings.py {session_dir}
```

session_dir モードでは `review_*.md` を glob で自動収集し、重複除去・重大度統合を行う。

---

## 6. アンチパターン

### 6.1 共有ファイルへの直接書き込み [MANDATORY]

```
# NG: 並列 agent が同一ファイルに書き込む
Agent A → plan.yaml
Agent B → plan.yaml  ← Agent A の結果が消失
```

### 6.2 agent 内での Read-Modify-Write

```
# NG: agent が共有リソースを Read → 加工 → Write
Agent A: Read plan.yaml → 加工 → Write plan.yaml
Agent B: Read plan.yaml → 加工 → Write plan.yaml  ← Agent A の変更が消失
```

Read は安全だが、Read した内容に基づく Write は race condition を引き起こす。

### 6.3 ファイルロックによる排他制御

Claude Code の Agent 環境ではファイルロック（`flock` 等）の信頼性が保証されない。ロックに依存するのではなく、書き込み先を分離する本パターンを使用する。

---

## 7. 新しい並列 agent を設計する際のチェックリスト

| # | 確認項目                                                      |
| - | ------------------------------------------------------------- |
| 1 | 各 agent の出力先ファイル名は一意か                           |
| 2 | 共有リソースへの Write は orchestrator のみが行うか           |
| 3 | orchestrator は全 agent の完了を待機してからマージするか      |
| 4 | 一部の agent が失敗した場合の部分マージ戦略は定義されているか |
| 5 | 結果ファイルの命名規則はプロジェクト内で一貫しているか        |
