# REQ-003 SKILL.md と script の責務分離要件

## メタデータ

| 項目       | 値                                                      |
| ---------- | ------------------------------------------------------- |
| 要件 ID    | REQ-003                                                 |
| プラグイン | forge                                                   |
| 種別       | 要件定義                                                |
| 対象       | forge の全 SKILL.md と全 script (共有 / SKILL 固有とも) |
| 関連設計   | DES-024 (配置・契約設計)                                |

---

## 1. 背景

SKILL.md は AI が tool 呼び出しの都度ロードする常駐コンテキストである。script 固有の情報 (引数列挙・状態遷移規則・JSON スキーマ・誤用禁止警告) が SKILL.md に混入すると AI の判断領域を圧迫し、タスクと無関係な認知負荷が増える。

## 2. 目的

SKILL.md は「AI が今なすべき操作の 1 行指示と判断戦略」のみを保持する。状態遷移・flag 合成・JSON スキーマは script 内部に閉じ込め、AI に「どの script を呼ぶか」「どんな引数を組み立てるか」を考えさせない。

## 3. 要件

### FNC-001: SKILL.md の clean state

SKILL.md に残してよい情報:

- AI が今なすべき操作の 1 行指示 (例: 「修正完了時は `mark_finding_fixed.py {session_dir} {id}` を呼ぶ」)
- AI の対話・判断戦略 (例: 「critical は対話省略可」)

SKILL.md に書かない情報:

- 状態遷移の許容マトリクス
- エラー時の引数合成手順
- 出力 JSON のフィールドスキーマ
- 「別の script を直接呼ぶな」型の禁止警告
- 誤用シナリオの列挙
- flag の有無で挙動を分岐させる選択肢の提示

### FNC-002: 引数最小化

script 呼び出しの引数は、AI がその時点で自然に持っている値のみで構成する。

- 必須: operation を特定する最小情報 (session_dir / ID 等)
- 任意: AI が文脈から自然に生成できる短い文字列 (skip 理由等)
- 排除: status 値の選択 / flag の組合せ / ファイル一覧の組立

理想形は `script {session_dir} {id}` のみで完結すること。意味論が異なる operation は別 script に分離する (同一 script 内を flag で切り替える設計を避ける)。

### FNC-003: 1 場面 1 script

AI が「何を呼ぶか」「どのオプションを組み合わせるか」を判断する余地を残さない。

- SKILL.md の各場面で呼ぶべき script は 1 つに確定している
- 複数候補から AI が選ぶ構造を作らない
- 同じ script 内で flag によって operation が分岐する構造は避ける (別 script にする)

### FNC-004: 誤用防御の配置

誤用の再発防止は以下の順で責務を配置する。

1. script 内部のガード (最優先)
2. script のエラーメッセージ (ヒント提示)
3. script の docstring / `--help`
4. `plugins/forge/docs/` 配下の Just-In-Time 参照文書

SKILL.md での禁止警告は使わない。警告文は AI に禁止対象の存在を認識させ、逆効果になりうる。

### FNC-005: 実測ベース

- 仮想の問題で refactor しない
- 対象は実際に SKILL.md 上に存在するノイズ (flag 露出・禁止警告・多引数呼び出し等) に限る
- 合否は目視レビュー + grep による Yes/No 判定。定量的な baseline 固定・再計測による達成率判定は行わない

## 4. 適用対象

forge プラグインの全 SKILL.md と全 script (共有 / SKILL 固有とも)。

## 5. 非目標

- ラッパー script 数の最小化
- 共有ラッパー層の新設
- 統一エントリーポイント (単一 wrapper から全 operation を dispatch する構造)
- 定量目標値の固定

## 6. 関連文書

- [DES-024 SKILL.md と script の配置・契約設計](../design/DES-024_skill_script_layout_design.md) — 本要件の How を定める
- [REQ-001 オーケストレータパターン要件](REQ-001_orchestrator_pattern.md) — SKILL の機能分業 (相補関係)
