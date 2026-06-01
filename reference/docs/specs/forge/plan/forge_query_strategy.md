# forge 検索バックエンド抽象化 実装戦略

> 本書は DES-027（`/forge:start-plan` への実装戦略フェーズ導入）で規定された Step 1〜4 構造に基づき、forge:DES-001_forge_query_abstraction_design / ADR-001 を入力として策定した実装戦略である。

## 全体構造分析（Step 1）

DES-001 / ADR-001 から把握した実装対象:

- **新規モジュール 5 点**
  - `plugins/forge/skills/query-db-rules/SKILL.md`（継承型・read-only、§3.1 / §8.1）
  - `plugins/forge/skills/query-db-specs/SKILL.md`（同上）
  - `plugins/forge/skills/update-db-rules/SKILL.md`（継承型）
  - `plugins/forge/skills/update-db-specs/SKILL.md`（同上）
  - `plugins/forge/scripts/backend_selection/select_backend.py`（分岐ロジック単一実装。Python 標準ライブラリのみ・read-only）
- **既存 SKILL の出力契約変更 1 点**: `plugins/doc-db/skills/query/SKILL.md` の Output Format を `Required documents:` 先頭ハイブリッド形式に書換（スクリプト本体は不変、§7.1）
- **既存 forge 17+ ファイルの参照置換**（§4.2）: `plugins/forge/skills/{review,start-design,start-plan,clean-rules,merge-specs,create-feature-from-plan,start-requirements,start-uxui-design}/...` 配下の SKILL.md および `docs/*.md` / `review_criteria_*.md` 内の `/doc-advisor:query-*` / `/doc-advisor:create-*-toc` 直呼びを抽象 4 SKILL 呼びに一斉置換（注: `create-feature-from-plan` は Issue #111 で `create-feature-from-markdown-plan` に rename。本記述は当時の skill 名を保持した履歴記述）
- **ガード方針の差分対応**（§8.2）: query 系の「利用可能ならスキップ」ガードは削除、update 系は維持
- **テスト 3 点**
  - 新規 `tests/forge/scripts/test_backend_selection.py`（分岐テーブル A 5 行 + B 8 行 + §1.5.1 API キー判定 + §5.1 エラー文字列完全一致 + read-only 性）
  - 新規 `tests/common/test_query_output_contract.py`（doc-db / doc-advisor の Output Format 先頭が `Required documents:`）
  - 既存 `tests/common/test_query_skill_isolation.py` 拡張（`CONSTRAINT_TARGET_SKILLS` に forge query 系 2 SKILL を追加。fork 検証ではなく継承型整合検証）
- **マニフェスト・文書更新**: `plugins/forge/.claude-plugin/plugin.json` への skill 4 件追加（version は不変、§8.3）、`CLAUDE.md` / `README*.md` / `docs/readme/forge/guide_*.md` の呼び方更新（§4.3）

依存グラフは浅い: `select_backend.py` ← 4 SKILL.md ← 既存 forge SKILL（参照置換）。doc-db SKILL.md 出力契約変更と forge 抽象 SKILL は出力契約テスト（test_query_output_contract.py）でのみ結合する独立変更。

## リスク分析（Step 2）

| リスクカテゴリ     | リスク                                                                                                                                                                                                                                                            | 影響度                   |
| ------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------ |
| 技術的不確実性     | available-skills を Python から取得する API が存在せず、SKILL.md 側で LLM が組立てる必要がある（§10.1）。SKILL.md と `select_backend.py` の責務分割を誤ると分岐表が SKILL.md に流出して SoT 多重化する                                                            | 中                       |
| データ整合性       | §5.1 のエラーメッセージ全文（複数行）と `select_backend.py` の `error` フィールド出力が完全一致する必要あり。改行・空白の不一致でテストが落ち、ユーザー導線が壊れる                                                                                               | 中                       |
| インテグレーション | doc-db 側 SKILL.md の Output Format 変更と forge 抽象 SKILL の出力契約が `Required documents:` 形式で噛み合う必要あり。先に doc-db を直さないと test_query_output_contract.py が落ちる                                                                            | 高                       |
| 依存の複雑性       | §4.2 影響範囲が 17+ ファイル + `.claude/skills/` / `.agents/skills/` まで及ぶ。grep 漏れがあると `/doc-advisor:*` 直呼びが残存し受け入れ条件 #5 を満たせない                                                                                                      | 高                       |
| 副作用暴走         | 新規 query 系 2 SKILL は継承型のため fork 境界による親 context 漏洩遮断がない。Role 制約（B 層）と引数解釈ガード（C 層）の明記漏れで write 系操作・自己再帰・ゴミ引数解釈が起こり得る（COMMON-DES-001 §3.1 / doc-advisor:ADR-002_query_skill_subagent_isolation） | 高                       |
| 非目的の混入       | ADR-001 で「`--toc` / `--index` は forge 抽象 SKILL の正式引数として持たない」と確定。`argument-hint` への混入や SKILL.md 内バリデーション記述で抽象が崩れる                                                                                                      | 低                       |
| パフォーマンス     | 該当なし（Python 標準ライブラリのみ・分岐評価は O(1)）                                                                                                                                                                                                            | -                        |
| バージョン関連     | `docs/rules/implementation_guidelines.md` の「バージョン関連ファイル編集禁止 [MANDATORY]」により plugin.json / marketplace.json / CHANGELOG.md / README バージョン表は本 PR 編集禁止。skill 一覧追加のみ可（§8.3 / §9）                                           | 低（制約として常時遵守） |

## アプローチ（Step 3）

**選択**: ボトムアップ + リスク駆動の組み合わせ

**根拠**:

- ボトムアップ的側面: 依存グラフが浅く一方向（`select_backend.py` → 4 抽象 SKILL → 既存 forge SKILL 参照置換 → 文書・マニフェスト）。最下層の `select_backend.py` を分岐テーブル A/B 網羅テストで固めれば、上層 SKILL.md は薄いラッパー実装に専念でき、逐次積み上げで検証可能性が高い。DES-001 §11 推奨実装手順も同方向（前提確認 → doc-db 出力契約 → select_backend.py → 4 SKILL.md → 参照置換 → 文書）
- リスク駆動的側面: 最大リスクは「インテグレーション（doc-db 出力契約と forge 抽象 SKILL の噛み合い）」と「副作用暴走（継承型 B/C 層制約漏れ）」。スケルトン先行で先に全層を通すと、`Required documents:` 形式が崩れたまま参照置換まで進んでロールバックコストが膨らむ。よって「出力契約 + 分岐ロジック」を最初に固めるリスク駆動の手順を採る
- フィーチャースライス・スケルトン先行は採らない: ユーザー向けの早期 E2E デモは不要（受入条件はバックエンド分岐の網羅性であり、UI フローではない）。SW-01〜SW-11（§10.2 観察的検証）はフェーズ 2 完了時点で初めて全体通しが可能になるため、フェーズ 1 で土台 → フェーズ 2 で全層接続 → フェーズ 3 で参照置換と文書整備、の順が手戻り最小

## フェーズ（Step 4）

### フェーズ 1: 出力契約整合と分岐ロジックの単一実装（土台確立）

- **目標**:
  - `plugins/doc-db/skills/query/SKILL.md` の Output Format 記述が `Required documents:` 先頭ハイブリッド形式に変わり、`tests/common/test_query_output_contract.py` が新規追加されて 3 SKILL（doc-db:query / doc-advisor:query-rules / doc-advisor:query-specs）の出力先頭契約を grep ベースで機械検証して合格する
  - `plugins/forge/scripts/backend_selection/select_backend.py` が `--available` / `--category` / `--operation` を受けて stdout JSON（`{backend, skill, error}`）を返す薄い CLI として完成し、`tests/forge/scripts/test_backend_selection.py` が DES-001 §2.3 分岐テーブル A の 5 行および分岐テーブル B の 8 行を網羅したゴールデンテストとして合格する
  - §5.1 のエラーメッセージ全文（`ERROR:` 行 + ヒント本文の複数行文字列）が `error` フィールドと完全一致することがテストで保証される
- **スコープ**:
  - DES-001 §7.1 / §11 手順 2: doc-db SKILL.md Output Format 書換（スクリプト本体は不変、§7.2 によりバージョン編集なし）
  - DES-001 §8.1 / §11 手順 3 / §10.3: `select_backend.py` 実装 + ユニットテスト
  - DES-001 §1.5.1 API キー判定式（`OPENAI_API_DOCDB_KEY` / `OPENAI_API_KEY` のフォールバック、doc-advisor:DES-007_unified_api_key_reference_design 統一仕様）を Python で同等実装
  - `tests/common/test_query_output_contract.py` 新規（§10.3）
- **検証ポイント**:
  - `python3 -m unittest tests.forge.scripts.test_backend_selection -v` が全 13+ ケース（A 5 行 + B 8 行 + API キー判定パス + read-only 検証）で合格
  - `python3 -m unittest tests.common.test_query_output_contract -v` が合格
  - `select_backend.py` を tmp ディレクトリで実行後、checksum 比較でファイル変更が発生しないこと（read-only 性）
  - スクリプトが Python 標準ライブラリのみで動作（`pip install` 不要、`docs/rules/implementation_guidelines.md` 準拠）
  - §5.1 エラーメッセージ完全一致を grep ではなく文字列等価でテスト（改行含む）

### フェーズ 2: 抽象 4 SKILL の新設と全層接続（中核機能）

- **目標**:
  - `plugins/forge/skills/{query-db-rules, query-db-specs, update-db-rules, update-db-specs}/SKILL.md` 4 件が新設され、§8.1 frontmatter テンプレ通りに記述される（version 編集なし）
  - 各 SKILL.md は「available-skills を LLM が読む → Bash で `select_backend.py` 呼出 → JSON 結果を解釈し `Skill` ツールで該当バックエンド起動」のシンプル構造に統一され、SKILL.md 内に分岐テーブルを複製していない
  - query 系 2 SKILL の SKILL.md が B 層・C 層多重防御契約（read-only 制約 / バックエンド検索 SKILL 以外の Skill 起動禁止 / `/doc-db:build-index` 等の書き込み系起動禁止 / 引数解釈 [MANDATORY] / 自己再帰禁止 / 出力契約 `Required documents:`）を doc-advisor:ADR-002_query_skill_subagent_isolation / §3.1 SKILL 契約に従って明記する
  - 雛形は **継承型に変更済みの** `plugins/forge/skills/query-forge-rules/SKILL.md` 構造（fork 関連 frontmatter を引き継がないため）
  - `plugins/forge/.claude-plugin/plugin.json` の `skills` リストに新規 4 件が追加される（**`version` フィールドは編集しない**、§8.3）
  - `tests/common/test_query_skill_isolation.py` の `CONSTRAINT_TARGET_SKILLS` に新規 2 SKILL（query-db-rules / query-db-specs）を追加して継承型整合（`context: fork` 不在 / Role 制約文言 / 引数解釈セクション存在 / Output Format 先頭契約）を機械検証して合格する。`FORK_TARGET_SKILLS` には追加しない
  - update 系 2 SKILL は §3.2 引数 `--full` 任意転送のフローを SKILL.md に記述する
- **スコープ**:
  - DES-001 §2.2 新規 SKILL 一覧 / §3.1 query 系仕様 / §3.2 update 系仕様 / §8.1 SKILL.md 構造とテンプレ
  - DES-001 §10.3 マニフェスト整合性テスト（4 件登録チェック）と継承型整合テストの追加
  - ADR-001: query 系 SKILL.md の `argument-hint` は `"task description"` のみ、`--toc` / `--index` を SKILL.md に書かない
  - doc-advisor:ADR-002_query_skill_subagent_isolation §B / §C: read-only 制約と引数解釈ガードの SKILL.md への明記（COMMON-DES-001 §3.1 デフォルト継承型に整合、`context: fork` を付けない）
- **検証ポイント**:
  - `python3 -m unittest discover -s tests -p 'test_*.py' -v` が全テスト通過（既存テスト破壊なし + 新規テスト合格）
  - `tests/common/` のマニフェスト整合性テスト（plugin.json への 4 件登録）が合格
  - query 系 2 SKILL の frontmatter に `context: fork` が含まれないこと、Role 章に「Edit / Write / MultiEdit / NotebookEdit を使わない」「`/doc-db:build-index` 等を起動しない」が明記されていることを grep で確認
  - update 系 2 SKILL の SKILL.md に `--full` 任意転送のフローと「ToC 更新は副作用更新で主処理に必須ではない」旨のガード方針注記がある（§8.2）
  - 観察的検証 SW-01〜SW-11 のうち、SW-01 / SW-04a / SW-04b / SW-05 / SW-09 / SW-10 / SW-11 を実際の Claude Code セッションで実行し、分岐テーブル A/B 通りのバックエンド起動を確認

### フェーズ 3: 既存 forge SKILL の参照置換と文書整備（仕上げ）

- **目標**:
  - DES-001 §4.2 の既知対象 17+ ファイル + `.claude/skills/` / `.agents/skills/` 配下を含むプロジェクトルート全体で `/doc-advisor:query-*` / `/doc-advisor:create-*-toc` 直呼びが新規 4 抽象 SKILL 呼びに置換される
  - `grep -rn -E 'query-rules|query-specs|create-rules-toc|create-specs-toc' ./` をプロジェクトルートで実行した結果、説明文中の言及（再帰防止注記 / README の説明）以外で **0 件**になる（受入条件 #5）
  - query 系のガード（「利用可能ならスキップ」）が **削除**され、update 系のガードは **維持**される（§8.2）
  - `merge-specs` の Phase 0「doc-advisor 必須」検査が「抽象 skill 必須検査」に置き換わる（§4.2 末尾、`/forge:query-db-specs` または `/forge:update-db-specs` の存在判定）
  - `CLAUDE.md` / `README.md` / `README_en.md` / `docs/readme/guide_doc-advisor_ja.md` / `docs/readme/forge/guide_*.md` の呼び方が抽象 SKILL に統一される（§4.3、ただし **バージョン表の数値変更は禁止**、`docs/rules/implementation_guidelines.md`）
  - `plugins/forge/docs/` 配下の forge 内蔵知識ベース ToC（`plugins/forge/toc/rules/rules_toc.yaml`）が必要に応じて `update-forge-toc` で再生成される（§8.4）
  - SW-02 / SW-03a / SW-03b / SW-06 / SW-07 / SW-08 を含む §10.2 SW-01〜SW-11 全シナリオが観察的検証で期待動作を示す（受入条件 #7）
- **スコープ**:
  - DES-001 §4 forge 配下の置換対象 / §4.2 影響範囲 / §4.3 CLAUDE.md / README / guide 文書
  - DES-001 §8.2 既存 skill 参照置換とガード方針 / §8.3 plugin.json（4 件追加部分の最終確認）/ §8.4 forge 内部 docs/rules への追記
  - DES-001 §11 推奨実装手順 5 / 6 / 9（参照置換 → 文書更新 → 観察的検証）
- **検証ポイント**:
  - `grep -rn -E 'query-rules|query-specs|create-rules-toc|create-specs-toc' ./` のヒット件数が説明文中の言及のみに収束（受入条件 #5）
  - `grep -rn -E '\-\-toc|\-\-index' plugins/forge/skills/` で抽象 SKILL 内に `--toc` / `--index` 文字列が **0 件**（受入条件 #6 / SW-11）
  - `python3 -m unittest discover -s tests -p 'test_*.py' -v` 全合格を再確認
  - `dprint check` 通過（JSON / Markdown / YAML フォーマット、`CLAUDE.md` の制約）
  - §10.2 観察的検証シナリオ SW-01〜SW-11 を Claude Code セッションで一通り実行し、§13 受入条件 1〜10 をチェックリストで確認
  - **バージョン関連ファイル（plugin.json の `version` / marketplace.json / CHANGELOG.md / README バージョン表の数値変更）に diff が出ていないことを `git diff` で確認**（`docs/rules/implementation_guidelines.md` 必須）

## リスクと対策

| リスク                                                                               | 影響度     | 対策（どのフェーズで潰すか）                                                                                                                                                                                                                                           |
| ------------------------------------------------------------------------------------ | ---------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| §5.1 エラーメッセージ全文と `error` フィールドの不一致                               | 中         | フェーズ 1: `test_backend_selection.py` で文字列等価テスト（改行含む完全一致）を最初に書き、`select_backend.py` 実装と同時に固定する                                                                                                                                   |
| doc-db / doc-advisor の出力先頭が `Required documents:` で揃わない                   | 高         | フェーズ 1: doc-db SKILL.md Output Format を先に書換し、`test_query_output_contract.py` を新規追加して 3 SKILL の先頭契約を機械検証。フェーズ 2 以降で抽象 SKILL を組む前に契約を固定する                                                                              |
| 継承型 SKILL の B/C 層制約漏れによる副作用暴走（write 操作・自己再帰・ゴミ引数解釈） | 高         | フェーズ 2: 雛形を継承型 `query-forge-rules` から派生させ、`test_query_skill_isolation.py` の `CONSTRAINT_TARGET_SKILLS` 拡張で「`context: fork` 不在 / Role 制約文言 / 引数解釈セクション / Output Format 先頭契約」を機械検証                                        |
| §4.2 影響範囲の grep 漏れによる `/doc-advisor:*` 直呼び残存                          | 高         | フェーズ 3: スキル名ベース grep（プレフィックスあり/なし両形式を一括捕捉）をプロジェクトルート全体（`.claude/skills/` / `.agents/skills/` 含む）で実行し受入条件 #5 をチェックリスト化。`test_query_output_contract.py` の対象範囲に grep 結果検証を組み込むことも検討 |
| SKILL.md 側に分岐テーブルが流出して SoT 多重化                                       | 中         | フェーズ 2: SKILL.md レビュー時に「分岐テーブルが書かれていないこと」を明示チェック。`select_backend.py` のみが分岐 SoT となるよう、SKILL.md は available-skills 構築 + Bash 呼出 + Skill 起動の 3 ステップに限定                                                      |
| ADR-001 違反（`--toc` / `--index` の SKILL.md への流出）                             | 低         | フェーズ 2: `argument-hint: "task description"` のみとし、SKILL.md 本文に `--toc` / `--index` 文字列を含めない。フェーズ 3 で grep 検証（SW-11）                                                                                                                       |
| バージョン関連ファイルへの不用意な編集                                               | 低（制約） | 全フェーズ共通: 各フェーズ完了時に `git diff -- plugins/*/.claude-plugin/plugin.json .claude-plugin/marketplace.json CHANGELOG.md` で version 行に変更がないことを確認。plugin.json への skill 追加は構造変更のみ許可（§8.3）                                          |
| 観察的検証（SW-01〜SW-11）の手動実行漏れ                                             | 中         | フェーズ 2 で SW-01 / 04a / 04b / 05 / 09 / 10 / 11 を実施、フェーズ 3 で残り（SW-02 / 03a / 03b / 06 / 07 / 08）を実施しチェックリストで管理                                                                                                                          |
| doc-advisor 側 SoT との矛盾（auto モード前提が破綻）                                 | 低         | フェーズ 0 相当の §11 手順 1 として、フェーズ 1 着手前に「doc-advisor がフラグなし呼び出しで auto モード `Required documents:` 応答を返す」前提を観察的に確認。前提が崩れている場合は doc-advisor 側 SoT の修正を別 Issue 化し、本 PR は中断                           |
