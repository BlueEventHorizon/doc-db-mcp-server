# REQ-005 review 系 SKILL の起動契約 整理要件

## メタデータ

| 項目         | 値                                                                                                                                                                                                                                                                                                                         |
| ------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 要件 ID      | REQ-005                                                                                                                                                                                                                                                                                                                    |
| サブシステム | forge-subagent                                                                                                                                                                                                                                                                                                             |
| 種別         | 要件定義 (整理 / 文書修正)                                                                                                                                                                                                                                                                                                 |
| 対象         | `/forge:review` 配下の reviewer / evaluator / fixer / present-findings / review                                                                                                                                                                                                                                            |
| 起点 Issue   | [#89](https://github.com/BlueEventHorizon/bw-cc-plugins/issues/89) — review 系 SKILL の Skill ツール / Agent ツール / fork 型 SKILL の起動契約を整理する / [#32](https://github.com/BlueEventHorizon/bw-cc-plugins/issues/32) — Fixer SKILL の誤読があった (AI 自身が `subagent_type: "forge:fixer"` と誤指定した実害事例) |
| 作成日       | 2026-05-24                                                                                                                                                                                                                                                                                                                 |

---

## 1. 背景

### 1.1 現状の問題

`/forge:review` 系 SKILL では、Claude Code の **Skill 呼び出し**、SKILL frontmatter の `context: fork`、Agent ツールによる **汎用 Agent 起動**、および Codex subprocess 起動の責務境界が混在している。

特に、起動される側の SKILL.md に「subagent として動作」「general-purpose subagent を起動」と書かれているが、frontmatter には `context: fork` / `agent:` がなく、呼び出し元が Agent ツールで直接起動する場合には、その SKILL.md が汎用 Agent に読まれる保証がない。

結果として、以下のリスクがある:

- SKILL.md に書いた workflow が実行時プロンプトとして使われず、実装仕様として機能しない
- 起動する側 / 起動される側のどちらが入力契約を持つのか曖昧になる
- `Skill` ツールで呼ぶのか、`Agent` ツールで汎用 Agent として起動するのかが混ざる
- `reviewer/SKILL.md` が「誰の手順書として読まれるか」が曖昧 (orchestrator が prompt として展開する手順書か、汎用 Agent 内で読まれる手順書か)
- `allowed-tools` に `Agent` がない SKILL が「Agent ツールで起動する」と記述している
- 軽量経路 (FNC-413) が追加され、修正の経路が「orchestrator 直接 / Agent ツール起動 / Skill ツール呼び」の 3 種に増えたが、責務境界の文書が未整理
- **AI 自身が誤読する実害事例が発生済み** (Issue #32): prompt 内に `/forge:fixer --batch` という slash command 表記があると、AI が「Skill 名で汎用/カスタム Agent 起動できる」と誤読し、`subagent_type: "forge:fixer"` のような無効指定を行う

### 1.2 関連既存ルール

- `docs/rules/skill_authoring_notes.md`
  - `subagent` という語は曖昧であり、Skill ツールの fork 型 SKILL と Agent ツールの汎用/カスタム Agent は別物
  - `agent:` は `context: fork` と組み合わせてのみ意味がある
  - `allowed-tools` は「承認なしで使える」allowlist であり、指定外でも (permission 設定が許せば) 呼び出し可能 (= 動作上は破綻しないが、契約として読みづらい)
  - SKILL 内から別 SKILL を呼ぶ場合は Skill ツール経由であり、スクリプトから直接呼ぶ構文はない
- `docs/specs/common/design/COMMON-DES-001_skill_base_design.md`
  - `context: fork` がない SKILL は継承型
  - fork 型 SKILL は §4 の規定リストに限る
  - args に親タスクのプロンプトを渡してはならない
  - **継承型のまま Skill ツールで呼ぶことは §4 リストの更新不要**。継承型 Skill 呼びと fork 型 Skill 呼びを混同しないこと

### 1.3 本要件のスコープ

- 対象は **設計・SKILL 文書の責務境界整理**。実装変更 (Python スクリプト・hooks 等) は含まない
- 修正方針 (方針 A / B-1 / B-2 / C) は §5 で評価し、推奨を示す
- 静的検証テストの追加を **再発防止策** として要求する (`CLAUDE.md`「注意ではなく再発防止策を優先する」)
- **COMMON-DES-001 §6 (fork 型 SKILL 一覧) は本要件の検討において固定制約ではない**。必要であれば §4.3 のリスト変更手順 (PR での判断基準提示 / リスト更新 / SKILL.md 修正 / テスト追加) に沿って改訂してよい。すなわち reviewer / evaluator / fixer / present-findings を **fork 型化する選択肢** も評価対象に含める

---

## 2. 抽出された問題箇所

### 2.1 `plugins/forge/skills/evaluator/SKILL.md`

| 項目             | 状態                                                                                                                    |
| ---------------- | ----------------------------------------------------------------------------------------------------------------------- |
| frontmatter      | `context: fork` / `agent:` なし → **継承型**                                                                            |
| `allowed-tools`  | `Read, Write, Bash` (`Agent` なし)                                                                                      |
| 本文の記述       | 「`/forge:review` から汎用 Agent として起動される」(L23)                                                                |
| 呼び出し元の指示 | `/forge:review` 側 (L458) は evaluator を Agent ツールで起動と書くが、`evaluator/SKILL.md` を Read させる明示指示がない |

**問題**: `evaluator/SKILL.md` の workflow が実際に読まれない可能性がある。

該当箇所:

- `plugins/forge/skills/evaluator/SKILL.md:23`
- `plugins/forge/skills/review/SKILL.md:458`

### 2.2 `plugins/forge/skills/reviewer/SKILL.md`

| 項目             | 状態                                                                                            |
| ---------------- | ----------------------------------------------------------------------------------------------- |
| frontmatter      | `context: fork` / `agent:` なし → **継承型**                                                    |
| `allowed-tools`  | `Read, Write, Bash` (`Agent` なし)                                                              |
| 本文の記述       | 「汎用 Agent として動作」(L36) / 「汎用 Agent を起動」(L155)                                    |
| 呼び出し元の指示 | `/forge:review` 側 (L397) は reviewer 起動時に `reviewer/SKILL.md` を Read させる明示指示を持つ |

**問題**: 本文の Claude 実行パスにある「汎用 Agent を起動」は **「reviewer 汎用 Agent 自身がレビュー本文を返す経路のメタ説明」** として書かれているように読めるが、文面上「reviewer 汎用 Agent がさらに別の汎用 Agent を起動する」とも読めてしまうため、誰の手順書として読まれるかが曖昧。

> **注**: 実行上「二重起動」が実際に走るわけではなさそうだが、SKILL.md の記述として **「reviewer 汎用 Agent の中で読まれる手順書である」** と明示すべき。

該当箇所:

- `plugins/forge/skills/reviewer/SKILL.md:36`
- `plugins/forge/skills/reviewer/SKILL.md:37`
- `plugins/forge/skills/reviewer/SKILL.md:155`
- `plugins/forge/skills/review/SKILL.md:397`

### 2.3 `plugins/forge/skills/fixer/SKILL.md`

| 項目             | 状態                                                                                                                                    |
| ---------------- | --------------------------------------------------------------------------------------------------------------------------------------- |
| frontmatter      | `context: fork` / `agent:` なし → **継承型**                                                                                            |
| `allowed-tools`  | `Read, Write, Edit, Bash` (`Agent` なし)                                                                                                |
| 本文の記述       | 「汎用 Agent に実際の Edit/Write を委譲する」(L14) / 「汎用 Agent を起動する」(L173)                                                    |
| 呼び出し元の指示 | `/forge:review` 側 (L538) は fixer を Agent ツールで起動 / `/forge:present-findings` 側 (L349, L393) は `/forge:fixer` を呼び出すと書く |

**問題**:

- fixer 自身の workflow を読ませるのか、fixer の prompt を review 側が直接構成するのかが曖昧
- Skill 呼び出しと Agent 起動の経路が混ざっている
- **設計原則「メインコンテキストの消費を抑える」(L24) は「方針 B (Skill ツール呼び)」と直接衝突する**。Skill ツール経由 (継承型) で呼ぶと親 context を消費するため、本原則を維持するなら方針 A (Agent ツール起動) を採るしかない

該当箇所:

- `plugins/forge/skills/fixer/SKILL.md:14`
- `plugins/forge/skills/fixer/SKILL.md:24` (設計原則「メインコンテキスト消費を抑える」)
- `plugins/forge/skills/fixer/SKILL.md:35`
- `plugins/forge/skills/fixer/SKILL.md:36`
- `plugins/forge/skills/fixer/SKILL.md:173`
- `plugins/forge/skills/review/SKILL.md:538` (prompt 内に `/forge:fixer --batch` 表記。Issue #32 の誤読源)
- `plugins/forge/skills/present-findings/SKILL.md:349` (prompt 内に `/forge:fixer --single` 表記。Issue #32 の誤読源)
- `plugins/forge/skills/present-findings/SKILL.md:393` (prompt 内に `/forge:fixer --batch` 表記。Issue #32 の誤読源)

### 2.4 `plugins/forge/skills/present-findings/SKILL.md`

| 項目                | 状態                                                                                                                  |
| ------------------- | --------------------------------------------------------------------------------------------------------------------- |
| frontmatter         | `allowed-tools: Read, Write, Bash, AskUserQuestion, Skill` (`Agent` なし)                                             |
| 設計原則            | 「fixer は汎用 Agent 経由」「`/forge:fixer` を汎用 Agent として起動する」(L21)                                        |
| 実際の手順          | `/forge:fixer --single` / `/forge:fixer --batch` を呼び出す → Skill ツール呼び出しなのか Agent ツール起動なのかが曖昧 |
| 旧 perspective 残存 | `review_{perspective}.md` 前提が大量に残存                                                                            |

該当箇所:

- `plugins/forge/skills/present-findings/SKILL.md:21`
- `plugins/forge/skills/present-findings/SKILL.md:120`
- `plugins/forge/skills/present-findings/SKILL.md:168`
- `plugins/forge/skills/present-findings/SKILL.md:349`
- `plugins/forge/skills/present-findings/SKILL.md:361`
- `plugins/forge/skills/present-findings/SKILL.md:393`
- `plugins/forge/skills/present-findings/SKILL.md:395`
- `plugins/forge/skills/present-findings/SKILL.md:582`
- `plugins/forge/skills/present-findings/SKILL.md:603`
- `plugins/forge/skills/present-findings/SKILL.md:626`
- `plugins/forge/skills/present-findings/SKILL.md:632`
- `plugins/forge/skills/present-findings/SKILL.md:651`
- `plugins/forge/skills/present-findings/SKILL.md:676-687`
- `plugins/forge/skills/present-findings/SKILL.md:752`

### 2.5 `plugins/forge/skills/review/SKILL.md`

| 項目                                 | 状態                                                                                       |
| ------------------------------------ | ------------------------------------------------------------------------------------------ |
| reviewer Claude エンジン             | 「まず `reviewer/SKILL.md` を Read せよ」と明記 (L397)                                     |
| evaluator (L458) / fixer (L538) 起動 | 「対象 SKILL.md を Read せよ」の明示なし → 不統一                                          |
| 軽量経路 (FNC-413)                   | `--auto` / `--auto-critical` に追加されたが、fixer 経路との責務境界が未明文化 (L515, L538) |

該当箇所:

- `plugins/forge/skills/review/SKILL.md:397`
- `plugins/forge/skills/review/SKILL.md:458`
- `plugins/forge/skills/review/SKILL.md:515`
- `plugins/forge/skills/review/SKILL.md:538`
- `plugins/forge/skills/review/SKILL.md:554`

### 2.6 軽量経路 (FNC-413) との接続

最新コミット `0753f02` で導入された「軽量経路: orchestrator が直接 Edit する」(`review/SKILL.md` §Phase 5 Step 2-A、`present-findings/SKILL.md` §修正実行時の経路分岐) により、**修正実行の経路が 3 種に増えた**。

| # | 経路                            | 起動方法              | context 消費            | 用途                                            |
| - | ------------------------------- | --------------------- | ----------------------- | ----------------------------------------------- |
| 1 | orchestrator 直接 Edit (軽量)   | (起動なし)            | 親 context を消費       | 件数小・auto_fixable な finding の自動修正      |
| 2 | fixer を Agent ツールで起動     | Agent ツール (sub)    | 親 context を消費しない | 件数多 or 非 auto_fixable な finding の修正委譲 |
| 3 | fixer を Skill ツールで呼び出し | Skill ツール (継承型) | 親 context を消費       | present-findings からの呼び出し記述として混在   |

**問題**:

- 3 経路の分岐条件・責務境界が SKILL.md / 設計書 (DES-015 / DES-028 / REQ-004 FNC-413) のどこか 1 箇所に **整理された表として存在しない**
- 経路 3 (Skill ツール呼び) は present-findings の文面に登場するが、実装としては経路 2 (Agent ツール起動) と混在しており、設計原則 (fixer のメインコンテキスト消費を抑える) と矛盾する

該当箇所:

- `plugins/forge/skills/review/SKILL.md:491` (軽量経路判定 [FNC-413])
- `plugins/forge/skills/review/SKILL.md:515` (Step 2-A 軽量経路)
- `plugins/forge/skills/review/SKILL.md:538` (Step 2-B fixer 経路)
- `plugins/forge/skills/present-findings/SKILL.md:356` (修正実行時の経路分岐 [FNC-413])
- `plugins/forge/skills/present-findings/SKILL.md:407` (一括修正の軽量経路判定)

### 2.7 旧 perspective 並列起動仕様の残存 [先行 P1 実施済み]

現在の `/forge:review` は reviewer 1 起動原則・`review_packet`・`review_<種別>.md` に寄せているが、旧 perspective 並列起動仕様が複数文書に残っていた。**先行 P1 (本要件本体と独立) として 2026-05-24 に整理完了**。

> **優先度 [重要]**: `docs/readme/forge/guide_review_ja.md` はユーザーが直接読む文書であり、内部仕様より優先度が高い。本節 (旧 perspective 文書整理) は **独立 PR として先行可能** な機械的整理として扱った (破壊的変更を含まない)。

該当箇所 (実施済み):

- `plugins/forge/docs/session_format.md`: `perspectives[]` スキーマ廃止 → `review_packet` スキーマに書き換え (L57, L504-525, L544-556, L703, L705, L765-775, L807-818, L836)
- `docs/readme/forge/guide_review.md`: mermaid 図 (Phase 2-5) と説明文を reviewer 1 起動原則に整合 (L48, L50, L54, L85, L89, L106, L108, L122, L123)
- `docs/readme/forge/guide_review_ja.md`: 同上 (日本語版)
- `docs/specs/forge/design/DES-011_session_management_design.md:384`: `review_{perspective}.md` → `review_<種別>.md`
- `plugins/forge/skills/present-findings/SKILL.md`: `review_{perspective}.md` 一括置換 (19 箇所)、refs.yaml 例を `review_packet` に更新、`write_interpretation.py --kind` に更新

スコープ外 (本要件本体で扱う):

- `plugins/forge/skills/present-findings/SKILL.md` の Step 1.5 (意味的重複の自動統合, L203-232): reviewer 1 起動原則下では前提が崩れているが、構造的削除を要するため §2.4 で扱う
- `plugins/forge/skills/extract_review_findings.py` の関数名 `extract_perspective_from_filename` および `perspective` フィールド付与: コード側の rename が必要。本要件のスコープ外として別 Issue で対応

スコープ外として残した記述 (OBSOLETE 注釈で残存):

- `plugins/forge/skills/reviewer/SKILL.md:24, 231`: 「perspective 並列起動は旧体系」「旧体系の `review_<perspective>.md` は完全削除」と既に明記済み (歴史的記述として保持)
- `docs/readme/forge/migration_notes/forge_review_v0.2.md`: v0.2 移行ノートとして歴史的記述
- `docs/specs/forge/design/DES-015_review_workflow_design.md:498`: 「固有 perspective 廃止の起点」 (旧仕様撤廃の経緯記述として保持)

参照不能:

- `docs/specs/forge/design/DES-021_review_perspective_split_design.md`: 既に削除済み (git 履歴に存在、現在はファイル不在)。本要件・関連文書から参照を削除

### 2.8 同型不具合として除外したもの

以下は `Agent ツールで並列起動` の記載があるが、呼び出し元 SKILL の `allowed-tools` に `Agent` があり、専用の収集 agent / executor を prompt で直接起動する設計に見えるため、本要件の主問題からは除外する:

- `plugins/forge/skills/start-design/SKILL.md`
- `plugins/forge/skills/start-plan/SKILL.md`
- `plugins/forge/skills/start-implement/SKILL.md`
- `plugins/forge/skills/start-requirements/docs/requirements_reverse_engineering_workflow.md`

ただし、これらも「Agent prompt が自己完結しているか」「SKILL.md を読ませる前提になっていないか」は別途軽く確認してよい。

---

## 3. 機能要件

### FNC-S001: 起動契約の概念分離 [MANDATORY]

各 SKILL.md および設計書において、以下 3 概念が混同されていないこと:

- `Skill ツールで呼ぶ` (継承型 SKILL / fork 型 SKILL を区別)
- `Agent ツールで起動する` (汎用 Agent: general-purpose / Explore / Plan / カスタム Agent)
- `context: fork` (SKILL frontmatter による隔離 context)

### FNC-S002: 起動責務の単一箇所集約 [MANDATORY]

`reviewer`, `evaluator`, `fixer`, `present-findings`, `review` の起動責務が **1 箇所** に整理されていること。具体的には方針 A (§5) を採るなら orchestrator (`review/SKILL.md`) に集約する。

### FNC-S003: Agent prompt の自己完結性 [MANDATORY]

Agent prompt として起動する場合、対象 SKILL.md を読む必要があるなら呼び出し元 prompt に **明示** されていること。現状 reviewer のみ「SKILL.md を Read せよ」が明示されているが、**evaluator / fixer にも同様の明示を追加** する。

### FNC-S004: SKILL.md 側の文言整理 [MANDATORY]

起動される側 SKILL.md には:

- 「自分が同名の汎用/カスタム Agent を新規起動する」ように読める記述が **ない** こと
- 冒頭に「汎用 Agent 内で読まれる手順書」または「継承型 SKILL として読まれる手順書」のいずれかが **明示** されていること

### FNC-S005: allowed-tools の整合 [MANDATORY]

`allowed-tools` に `Agent` がない SKILL が「Agent ツールで起動する」と書いていないこと。allowlist は「承認なしで使える」許可リストであり物理禁止ではないが、契約と allowed-tools の食い違いを解消する。

### FNC-S006: 旧 perspective 仕様の整合 [MANDATORY]

旧 `review_{perspective}.md` / perspective 並列起動前提が、現行仕様で obsolete なら obsolete と明記されるか、現行仕様 (`review_<種別>.md` / reviewer 1 起動原則) へ更新されていること。

### FNC-S007: 軽量経路 (FNC-413) 含む経路分岐の単一表 [MANDATORY]

軽量経路 (FNC-413) を含む修正経路 3 種 (orchestrator 直接 / Agent ツール起動 / Skill ツール呼び) の分岐条件・責務境界が **1 箇所の表** で整理されていること。最有力候補は `docs/specs/forge/design/DES-015_review_workflow_design.md` への追記。

### FNC-S009: prompt 内 slash command 表記の起動経路明示 [MANDATORY]

Issue #32 の再発防止として、SKILL.md または設計書内で **Agent prompt として展開されるテキスト** に `/forge:<skill>` / `/anvil:<skill>` 等の slash command 表記を含める場合、**同じ段落または直前のテキストに** 以下のいずれかを明示する:

- 「汎用 Agent (subagent_type: general-purpose) で起動する」(slash command は **ロール演技用の表記**)
- 「継承型 SKILL として Skill ツールで呼び出す」(= 親 context で Skill ツール経由で実行)
- 「Bash subprocess として起動する」(= subprocess として実行)

明示なく slash command 表記単独で書かない。Issue #32 のように `subagent_type: "forge:fixer"` のような誤指定を AI が試みる経路を文書側で塞ぐため。

> **背景**: Claude Code の Agent ツールでは `subagent_type` は `general-purpose` / `Explore` / `Plan` / カスタム Agent 名のいずれかであり、Skill 名 (slash command 名) は **指定できない**。`/forge:fixer` のような表記はあくまで「prompt 内で fixer のロールを演じさせる際の表現」であり、`subagent_type` の値ではない。

### FNC-S010: COMMON-DES-001 §6 改訂を許容する [MANDATORY]

本要件の検討において、`COMMON-DES-001 §6` (fork 型 SKILL 一覧) は **固定制約として扱わない**。reviewer / evaluator / fixer / present-findings のいずれかを fork 型 SKILL 化することが設計上有利と判断された場合、`COMMON-DES-001 §6.3` のリスト変更手順に沿って §6 を改訂してよい。

§4.3 のリスト変更手順 (本要件適用時の遵守事項):

1. COMMON-DES-001 §3.2 の判断基準 (具体的な実害 / 複数の独立タスクからの呼び出し / 親 context 肥大化) に該当することを **本要件または別 ADR で明示** する
2. §4 のリストを更新し、**fork 採用根拠** を明記する
3. SKILL.md を修正 (frontmatter / Role / 引数解釈ガード)
4. §7.1 静的検証の対象に追加する (本要件 §4 静的テストと整合)

> **意図**: 既存制約に縛られず、最適な設計判断を選べる余地を残す。一方で「fork 型を雑に増やさない」COMMON-DES-001 §3 のデフォルト継承型方針は維持する。fork 型化は **継承型では成立しない実害・必要性が示せた場合に限る** (COMMON-DES-001 §3.2)。

---

## 4. 再発防止策 (静的テスト) [MANDATORY]

`CLAUDE.md` の「同じ種類のミスを繰り返さない / 注意ではなく再発防止策を優先する」に従い、**静的検証テストを追加** する。`tests/common/` または `tests/forge/` に追加し、`python3 -m unittest discover -s tests` で実行可能とする。

### TEST-S001: Agent 言及と allowed-tools の整合性テスト

SKILL.md 本文に `Agent ツール` / `汎用 Agent を起動` / `カスタム Agent を起動` 等の語があれば、frontmatter の `allowed-tools` に `Agent` が含まれることを要求する。

### TEST-S002: Skill 呼び出しと allowed-tools の整合性テスト

SKILL.md 本文に `/forge:<skill>` または `/anvil:<skill>` の Skill 呼び出し記述があれば、frontmatter の `allowed-tools` に `Skill` が含まれることを要求する。

### TEST-S003: 旧 perspective 文字列の完全削除テスト

`review_{perspective}.md` 文字列が `plugins/forge/` 配下と `docs/readme/forge/` 配下の Markdown に存在しないこと。

> **例外**: 「旧体系」「廃止済み」等の文脈で歴史的記述として残す場合は、当該段落に `[OBSOLETE]` マーカーを付ける運用とし、テスト側で除外可能にする。

### TEST-S004: 用語混用防止テスト [任意]

SKILL.md 内で `subagent` が単独で使われている箇所を列挙する (warning 相当)。`継承型 SKILL` / `fork 型 SKILL` / `汎用 Agent` / `カスタム Agent` への置換を促す。

### TEST-S005: prompt 内 slash command 表記の起動経路明示テスト (Issue #32 再発防止)

Agent prompt として展開されるテキストブロック (`` ``` `` で囲まれた prompt 例や Agent 起動セクション内) に `/forge:<skill>` / `/anvil:<skill>` 表記がある場合、**同一テキストブロック内または直前段落** に以下のいずれかの明示があることを要求する:

- `汎用 Agent` (general-purpose / Explore / Plan) または `カスタム Agent`
- `継承型 SKILL` / `fork 型 SKILL` (Skill ツール経由)
- `Bash subprocess` (subprocess として実行)

検出のヒューリスティクス例:

- prompt ブロック内に `/forge:fixer --batch` 等の表記がある
- かつ直近 N 行 (例: 5 行) に `汎用 Agent` / `カスタム Agent` / `継承型 SKILL` / `fork 型 SKILL` / `Bash subprocess` のいずれかの文字列がない
- → 違反としてエラー報告

> **本テストは Issue #32 の AI 誤読 (`subagent_type` に Skill 名を指定) を文書側で塞ぐための具体的検証**。FNC-S009 と対応する。

---

## 5. 修正方針の評価

> **前提**: §1.3 / FNC-S010 に従い、`COMMON-DES-001 §6` (fork 型 SKILL 一覧) は固定制約ではなく、必要なら §6.3 手順で改訂してよい。以下の方針評価はこの前提に立つ。

### 方針一覧

| 方針 | 起動経路                                                  | context 消費 | §4 (fork 型リスト) 改訂 | 推奨度                                   |
| ---- | --------------------------------------------------------- | ------------ | ----------------------- | ---------------------------------------- |
| A    | 汎用 Agent (general-purpose) で SKILL.md を Read して起動 | 消費しない   | 不要                    | 第 2 候補                                |
| B-1  | 継承型 SKILL として Skill ツールで呼ぶ                    | **消費する** | 不要                    | 非推奨 (※)                               |
| B-2  | fork 型 SKILL として Skill ツールで呼ぶ                   | 消費しない   | **§4 追加が必要**       | **第 1 推奨**                            |
| C    | Bash subprocess として起動                                | 消費しない   | 不要                    | reviewer の Codex エンジン以外では非該当 |

※ B-1 は fixer の「メインコンテキスト消費を抑える」設計原則と衝突するため非推奨。

### A と B-2 の実害比較

A (汎用 Agent) と B-2 (fork 型 SKILL) は、いずれも別 context で動作し、親 context を消費しない。当初 B-2 のデメリットとして挙げられていた事項を A と並べて再評価すると、**実害ベースの差はほぼない**。

| 観点                                        | A (汎用 Agent)                                          | B-2 (fork 型 SKILL)                                      | 差           |
| ------------------------------------------- | ------------------------------------------------------- | -------------------------------------------------------- | ------------ |
| 親 context にある情報の再供給               | prompt で再供給が必要                                   | `$ARGUMENTS` で再供給が必要                              | **等価**     |
| 親 context への return                      | Agent 結果テキストのみ                                  | SKILL の return テキストのみ                             | **等価**     |
| ファイル修正の反映経路                      | Edit/Write ツール経由でディスクに反映                   | Edit/Write ツール経由でディスクに反映                    | **等価**     |
| 二重起動 / 二重 fork のリスク               | Agent 内で別 Agent を起動すれば二重起動                 | fork 内で別 fork を呼べば二重 fork                       | **等価**     |
| 親の対話履歴・AskUserQuestion 利用          | 別 context のため利用不可                               | fork 境界のため利用不可                                  | **等価**     |
| 起動契約の自己完結性                        | 呼び出し元 prompt + 対象 SKILL.md の **整合保証が必要** | SKILL.md 単独で完結                                      | **B-2 優位** |
| 起動経路数 (修正経路)                       | 3 種 (orchestrator 直接 / Agent / Skill)                | 2 種 (orchestrator 直接 / fork 型 Skill)                 | **B-2 優位** |
| Issue #32 (`subagent_type` 誤指定) の解消   | 文書側で塞ぐ (FNC-S009 / TEST-S005)                     | 構造的に解消 (Skill ツールは `subagent_type` を取らない) | **B-2 優位** |
| orchestrator (`review/SKILL.md`) の責務集中 | Agent prompt 構成責務が集中し肥大化しやすい             | SKILL.md に分散                                          | **B-2 優位** |
| 作業量                                      | SKILL.md 文言整理 + 静的テスト                          | §4 改訂 + SKILL.md 改訂 + 引数解釈ガード + 静的テスト    | **A 優位**   |
| 段階移行性                                  | 現行実装に近く小ステップで導入可                        | §4 改訂と SKILL.md 改訂を一括で必要とする                | **A 優位**   |

→ A の優位点は **作業量と段階移行性のみ** に縮約される。設計の明快さ・誤読防止・契約の自己完結性は B-2 が優位。

### 方針 B-2: fork 型 SKILL として Skill ツールで呼ぶ [第 1 推奨。§4 改訂を伴う]

`/forge:fixer` / `/forge:reviewer` / `/forge:evaluator` / `/forge:present-findings` のうち適切なものを fork 型 SKILL 化し、Skill ツール経由で呼ぶ。fork 境界で親 context を遮断するため、**「メインコンテキスト消費を抑える」原則と整合する**。

採用前提:

- COMMON-DES-001 §3.2 の判断基準 (具体的実害 / 複数の独立タスクからの呼び出し / 親 context 肥大化) のいずれかに該当することを示す
- COMMON-DES-001 §6 のリストに該当 SKILL を追加し、fork 採用根拠を明記する (FNC-S010 / §4.3 手順)
- fork 境界で `$ARGUMENTS` 経由の親タスク漏洩を防ぐ「引数解釈ガード」を SKILL.md 本文に明記する (COMMON-DES-001 §4.1 / ADR-002 §C)

**メリット**:

- 起動経路が「fork 型 SKILL」1 種に統一でき、概念的に明快
- 軽量経路 (FNC-413) との分岐表が単純になる (orchestrator 直接 / fork 型 SKILL の 2 種に縮約可能)
- 入力契約が SKILL.md 単独で完結 (Agent prompt の自己完結性確保が不要 = FNC-S003 が縮退)
- Issue #32 のような「`subagent_type` に Skill 名を指定」誤読は **構造的に解消** (Skill ツールは `subagent_type` を取らない)
- orchestrator (`review/SKILL.md`) の肥大化を回避できる

**デメリット**:

- §4 リスト更新・SKILL.md 改訂・引数解釈ガード追加・静的検証追加の作業量が方針 A より大きい
- fork 型は SKILL.md + `$ARGUMENTS` を毎回入力として読み込むため、親 context にある情報を args で再供給すると二重コスト (ただし方針 A でも prompt で同等の再供給が必要なので **A との相対差はない**)
- 「同一プラグイン内で fork → fork」となる二重 fork 経路の有無を検証する必要がある (ただし方針 A でも Agent 内 Agent 起動の同等リスクがあり **A との相対差はない**)
- fixer の修正サマリー等、親 context へ戻る情報は return 値のみに制限される (ただし方針 A でも同等の制約があり **A との相対差はない**)
- present-findings をユーザー対話を伴う処理として残す場合、fork 型ではなく **継承型のまま** とする選択肢があり、fork 型化対象は要件・設計フェーズで個別判断する

### 方針 A: `review` orchestrator が汎用 Agent 起動を担当する [第 2 候補]

現行実装に最も近く、変更コストが小さい。

- `review` 側に reviewer / evaluator / fixer の Agent prompt を構成する責務を寄せる
- Agent prompt の冒頭に、対象 SKILL.md を Read して従うか、または必要な workflow を prompt 内に完全展開するかを統一する
- **現状 reviewer のみ「SKILL.md を Read せよ」が明示されているが、evaluator / fixer にも同様の明示を追加** する (FNC-S003)
- 起動される側 SKILL.md に「自分が同名の汎用/カスタム Agent を新規起動する」ように読める文言を削除し、**「汎用 Agent 内で読まれる手順書」** であることを冒頭で明示する
- 起動される側 SKILL.md は「Agent prompt として読まれる実行手順」または「継承型 SKILL として Skill ツールで呼ばれる実行手順」のどちらかを明記する

**メリット**:

- 既存実装の変更が小さい
- §4 改訂不要
- 段階移行に向く (本要件のスコープを「文書整理 + 静的テスト追加」に閉じられる)

**デメリット**:

- 起動経路の責務が `review/SKILL.md` に集中するため、orchestrator が肥大化しやすい
- 起動される側 SKILL.md と Agent prompt の整合 (SKILL.md Read 指示 or workflow 完全展開) を文書側で保証する必要がある (FNC-S003)
- Issue #32 の誤読源 (prompt 内 slash command 表記) は **静的テスト (FNC-S009 / TEST-S005) で塞ぐ** 必要があり、構造的解消ではない
- 起動経路が 3 種 (orchestrator 直接 / Agent / Skill) のまま残り、軽量経路 (FNC-413) との分岐表が複雑になる

### 方針 B-1: 継承型 SKILL として Skill ツールで呼ぶ [非推奨]

設計としては明快だが、**現状の設計原則 (fixer の「メインコンテキスト消費を抑える」) と直接衝突する**。継承型 SKILL は親 context を消費するため、本原則を維持するなら採用できない。

- 採用するなら fixer の設計原則を改訂する必要があり、別 ADR / 別 PR で context 消費許容範囲を再判断するべき
- COMMON-DES-001 §6 は **fork 型 SKILL** のリストであり、**継承型 SKILL の Skill ツール呼びには §4 更新は不要**
- 入力契約は「呼び出し元が何を渡すか」と「呼び出される側がどう解釈するか」に分けて明文化する
- present-findings 内のユーザー対話・確認を伴う処理など、親 context を活用したい場面では合理性が出る場面もあるが、現状の fixer はこれに該当しない

### 方針 C: Bash subprocess として起動

reviewer の Codex エンジン (`run_review_engine.sh` 経由) が該当する既存経路。fixer / evaluator / present-findings に同経路を採用する合理性は本要件のスコープでは見当たらないが、定義文書 (`docs/rules/skill_launch_paths_definitions.md`) で経路の 1 種として整理対象に含める。

### 方針選定の指針

- 設計の明快さ・Issue #32 の構造的解消・契約の自己完結性を優先するなら **方針 B-2** を採用し、§4 改訂を含めた設計フェーズで詳細検討する (第 1 推奨)
- 作業量の最小化・段階移行性を優先するなら **方針 A** を採用し、本要件のスコープを「文書整理 + 静的テスト追加」に閉じる (第 2 候補)
- 実害ベースでは A と B-2 の差はほぼ「作業量 vs 構造的明快さ」のトレードオフに収束する。本要件は **両方の選択肢を許容する** ことを §1.3 / FNC-S010 で保証する
- 方針確定は設計フェーズで判断する (TBD-004)

### 軽量経路 (FNC-413) の取り扱い

修正経路 (orchestrator 直接 / 汎用 Agent 起動 / 継承型 SKILL / fork 型 SKILL / Bash subprocess) の分岐条件・責務境界を **1 箇所に整理された表** で記述する。最有力候補は `docs/specs/forge/design/DES-015_review_workflow_design.md` への追記 (FNC-S007)。方針選定 (A / B-2) によって表に載る経路の組み合わせが変わる (B-2 採用なら 2 種、A 採用なら 3 種)。

---

## 6. 受け入れ基準

### 6.1 機能要件 (FNC-S001 〜 FNC-S007, FNC-S009, FNC-S010)

- §3 の機能要件すべてを満たしていること

### 6.2 静的検証 (TEST-S001 〜 TEST-S005)

- §4 のテストが `tests/` 配下に追加され、`python3 -m unittest discover -s tests` で pass すること

### 6.3 機械的検証

- `dprint check` が pass

### 6.4 方針 B-2 採用時の追加条件 [条件付き]

設計フェーズで方針 B-2 (fork 型 SKILL) を採用した場合、以下を満たすこと:

- COMMON-DES-001 §6 のリストに対象 SKILL (fixer / reviewer / evaluator / present-findings のうち fork 型化したもの) が **fork 採用根拠つきで追加** されている
- 対象 SKILL.md に `context: fork` / `agent:` / 引数解釈ガード / 否定的制約 (Edit/Write を使わない等の該当文言) が明記されている
- COMMON-DES-001 §7.1 の静的検証 (fork 型 frontmatter 検証 / Role 制約文言検証) の対象に追加されている
- 本要件 TEST-S001 / S002 / S005 と矛盾しない (例: fork 型 SKILL 内で Agent ツール起動を行うなら allowed-tools との整合を取る)

---

## 7. 分割案 (実装着手時の参考)

修正範囲が大きい場合は、以下に分割してよい。

### 先行 PR (方針選定と独立に着手可能) [推奨]

以下は破壊的変更を含まず、方針選定 (A / B-2) を待たずに先行 PR 化できる:

| #  | 範囲                                                                                        | 依存 |
| -- | ------------------------------------------------------------------------------------------- | ---- |
| P1 | **旧 perspective 並列起動仕様の文書整理** (§2.7 / FNC-S006)。破壊的変更を含まない機械的置換 | なし |

### 方針 B-2 を採用する場合 [第 1 推奨。COMMON-DES-001 §6 改訂を伴う]

| # | 範囲                                                                                                     | 依存             |
| - | -------------------------------------------------------------------------------------------------------- | ---------------- |
| 1 | fork 採用根拠の文書化 (本要件への追記 or 別 ADR 起票)                                                    | なし             |
| 2 | COMMON-DES-001 §6 リスト改訂 (対象 SKILL を fork 採用根拠つきで追加)                                     | 1 完了後         |
| 3 | 対象 SKILL.md の改訂 (`context: fork` / `agent:` / 引数解釈ガード / 否定的制約 / 自己再帰禁止文言の追加) | 2 完了後         |
| 4 | review / present-findings の fork 型 SKILL 呼び出しへの切り替え + 軽量経路 (FNC-413) の分岐表整理        | 3 完了後         |
| 5 | 静的検証テストの追加 (本要件 §4 + COMMON-DES-001 §7.1 拡張)                                              | 1〜4 + P1 完了後 |

### 方針 A を採用する場合 [第 2 候補]

| # | 範囲                                                                                              | 依存             |
| - | ------------------------------------------------------------------------------------------------- | ---------------- |
| 1 | review 系 SKILL の起動契約整理 (方針 A の適用 + evaluator / fixer への「SKILL.md Read 指示」追加) | なし             |
| 2 | present-findings / fixer の Skill vs Agent 経路整理 + 軽量経路 (FNC-413) の分岐表整理             | 1 完了後         |
| 3 | 静的検証テストの追加 (TEST-S001 〜 S005)                                                          | 1〜2 + P1 完了後 |

---

## 8. 未確定事項

| ID      | 内容                                                                                                                                                                                                                                        | 状態     | 解消内容                                                                                                                                                                                                                                                                                      |
| ------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| TBD-002 | TEST-S001 / S002 の判定ロジック (正規表現ベースか AST ベースか)                                                                                                                                                                             | 解消済み | **正規表現ベースを採用**。SKILL.md は Markdown 文書であり Python AST は適用不可。呼び出し意図の検出は「`→` 直前 + 動詞パターン、受動形除外」の正規表現で実装 (`tests/forge/subagent/test_*_allowedtools_consistency.py` 参照)                                                                 |
| TBD-003 | TEST-S003 の `[OBSOLETE]` マーカー運用 (どこに付与するか・テスト除外条件)                                                                                                                                                                   | 解消済み | **除外条件を 2 種に確定**。① 行内に `OBSOLETE` 文字列を含む場合、② 行内に「旧体系」または「完全削除」を含む場合 (削除済みことを説明する歴史的文書として除外)。また `migration_notes/` ディレクトリは除外対象外 (移行ドキュメントは旧名を記録する)。実装: `test_legacy_perspective_removed.py` |
| TBD-004 | 方針選定 (B-2 第 1 推奨 / A 第 2 候補)。B-2 採用時は対象 SKILL (fixer / reviewer / evaluator / present-findings のうちどれを fork 型化するか含む) の fork 採用根拠 (COMMON-DES-001 §3.2) を本要件または別 ADR に追記し、§4 リストを改訂する | 解消済み | **B-2 (fork 型 SKILL) を採用**。fork 型化対象は reviewer / evaluator / fixer の 3 SKILL (present-findings はメインコンテキスト実行を維持)。採用根拠は forge:DES-029 §5 に記載。§4 リストは refactor/forge-subagent ブランチで改訂済み                                                         |
| TBD-005 | TEST-S005 の検出ヒューリスティクス (近傍行数 N の値、テキストブロックの境界判定、誤検知許容度)                                                                                                                                              | 解消済み | **近傍行数 N=5 (前後各 5 行)**。境界は fenced code block (`` ``` `` で開閉)。`[KNOWN-FP]` マーカーを行末に付与することで個別除外可能。warning 段階 (CI fail なし) のため誤検知は許容。実装: `test_slash_command_launch_context.py`                                                            |

---

## 9. 関連事項 (別 Issue 候補)

本要件の主題からは外れるが、調査中に判明した事項:

- AI 専用スキル (evaluator / reviewer / fixer / present-findings) は `user-invocable: false` だが `disable-model-invocation` が未設定。description に「`/forge:review` から呼び出される」と明記されているため、Claude が自動呼び出しする経路は理論上残っている。`docs/rules/skill_authoring_notes.md` L55「副作用ある操作 → `disable-model-invocation: true`」推奨と照らし、別 Issue で検討する余地あり

---

## 10. 関連文書

| 種別   | パス                                                                      | 関係                                                                                                |
| ------ | ------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------- |
| 定義   | `docs/rules/skill_launch_paths_definitions.md`                            | 起動経路 5 種 (継承型 SKILL / fork 型 SKILL / 汎用 Agent / カスタム Agent / Bash subprocess) の定義 |
| Issue  | [#89](https://github.com/BlueEventHorizon/bw-cc-plugins/issues/89)        | 本要件の起点 (起動契約整理の提起)                                                                   |
| Issue  | [#32](https://github.com/BlueEventHorizon/bw-cc-plugins/issues/32)        | AI 自身が `subagent_type: "forge:fixer"` と誤指定した実害事例。FNC-S009 / TEST-S005 の動機          |
| ルール | `docs/rules/skill_authoring_notes.md`                                     | SKILL.md frontmatter / 構造の規約                                                                   |
| 設計書 | `docs/specs/common/design/COMMON-DES-001_skill_base_design.md`            | SKILL 実行モデル・fork 型リスト                                                                     |
| 設計書 | `docs/specs/forge/design/DES-015_review_workflow_design.md`               | review ワークフロー全体                                                                             |
| 設計書 | `docs/specs/forge/design/DES-028_review_policy_design.md`                 | review_packet / 1 起動原則                                                                          |
| 要件   | `docs/specs/forge/requirements/REQ-004_review_policy.md`                  | FNC-411 / FNC-412 / FNC-413 の出典                                                                  |
| ADR    | `docs/specs/doc-advisor/design/ADR-002_query_skill_subagent_isolation.md` | fork 型採用根拠 (COMMON-DES-001 §6 経由)                                                            |

---

## 変更履歴

| 日付       | 変更者  | 内容                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| ---------- | ------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 2026-05-24 | k2moons | 初版作成。Issue #89 の議論結果を要件として整理。方針 A 推奨・静的テスト 4 種・用語マップ追加を明文化                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| 2026-05-24 | k2moons | Issue #32 (AI が `subagent_type: "forge:fixer"` と誤指定した実害事例) を取り込み。FNC-S009 / TEST-S005 追加。起点 Issue に #32 を併記                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               |
| 2026-05-24 | k2moons | COMMON-DES-001 §6 (fork 型 SKILL 一覧) を固定制約から外す方針を反映。FNC-S010 追加。§6 方針評価を A / B-1 / B-2 / C の 4 区分に再構成し、方針 B-2 (fork 型 Skill 呼び) を第 2 候補として明文化。§7.4 / §8 方針 B-2 採用時の分割案 / TBD-004・TBD-005 を追加                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| 2026-05-24 | k2moons | §3 用語マップを「先行 PR 化可能」と明示。§3.2 配置先選定の判断材料を追加。§8 分割案に「先行 PR (P1 / P2)」セクションを新設し、用語マップ (P1) と旧 perspective 整理 (P2) を方針選定と独立に先行可能と明文化。TBD-001 を「先行 PR レビューで決定」に変更                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| 2026-05-24 | k2moons | §3 を「用語マップ」から「定義文書」概念に再構築。§3.1 で「定義 (definitions)」を新規カテゴリとして位置付け。§3.2 で配置先を `docs/rules/skill_launch_paths_definitions.md` に確定 (候補 C)。`.doc_structure.yaml` 変更を伴う候補 D は将来課題 (TBD-006) として残す。FNC-S008 を更新し FNC-S008a (skill_authoring_notes.md 該当節改修) を新設。TBD-001 解消・TBD-006 追加                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| 2026-05-24 | k2moons | 定義文書 (`docs/rules/skill_launch_paths_definitions.md`) と skill_authoring_notes.md 該当節改修が実装完了したため、本要件から該当部分を削除し残作業に集中。削除: 旧 §3 (定義文書の不在 / 配置先 / 内容仕様 / skill_authoring_notes 改修) / FNC-S008 / FNC-S008a / §7 分割案 P1 / TBD-001 / TBD-006。リナンバリング: §4→§3, §5→§4, §6→§5, §7→§6, §8→§7, §9→§8, §10→§9, §11→§10。残った機能要件は review/reviewer/evaluator/fixer/present-findings の起動契約整理 (FNC-S001〜S007, S009, S010) に絞る。§10 関連文書に定義文書へのリンクを保持                                                                                                                                                                                                                                                                                                        |
| 2026-05-24 | k2moons | §5 方針評価を冷静に再評価。A (汎用 Agent) と B-2 (fork 型 SKILL) は実害ベース (context 消費 / return 制限 / 二重起動リスク / 親 context 活用不可) でほぼ等価であり、差分は「作業量 vs 構造的明快さ」のトレードオフに収束することを「A と B-2 の実害比較」表として明記。これに基づき推奨度を反転 (**B-2 を第 1 推奨**, A を第 2 候補)。§7 分割案の順序も B-2 → A に並び替え。TBD-004 を B-2 第 1 推奨に揃え、fork 型化対象の選定 (fixer / reviewer / evaluator / present-findings のうちどれを fork 型化するか) を設計フェーズの判断事項として明示                                                                                                                                                                                                                                                                                                   |
| 2026-05-24 | k2moons | **先行 P1 (旧 perspective 文書整理) 実施完了**。`guide_review.md` / `guide_review_ja.md` の mermaid 図と説明文を reviewer 1 起動原則に整合、`DES-011:384` を `review_<種別>.md` に置換、`present-findings/SKILL.md` の `review_{perspective}.md` 19 箇所を `review_<種別>.md` に一括置換 + refs.yaml 例を `review_packet` に更新 + `write_interpretation.py --kind` に更新、`session_format.md` を全面書き換え (`perspectives[]` スキーマ廃止 → `review_packet` スキーマ、CLI 仕様更新)。§2.7 の該当箇所リストを「実施済み」に更新し、削除済み `DES-021_review_perspective_split_design.md` への参照を §2.7 / §10 から削除。スコープ外 (本要件本体・コード rename・OBSOLETE 保持) を §2.7 末尾で明示。`present-findings/SKILL.md` Step 1.5 (意味的重複の自動統合) と `extract_review_findings.py` のコード rename は §2.4 / 別 Issue 扱いとして残置 |
