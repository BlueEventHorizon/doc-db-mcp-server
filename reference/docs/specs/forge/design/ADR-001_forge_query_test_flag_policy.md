# ADR-001: forge:query-db-* は `--toc` / `--index` を受理しない（品質検査用フラグの直呼び方針）

## ステータス

採択（2026-05-18）

## コンテキスト

doc-advisor の `/doc-advisor:query-rules` / `/doc-advisor:query-specs` には `--toc` / `--index` / `auto` の 3 モードが正式引数として定義されている。`--toc` はキーワード ToC のみで検索し、`--index` は Embedding Index のみで検索する。`auto` は両者を並列実行して統合する本番運用モードで、デフォルトに採択されている。

`--toc` / `--index` は **品質検査・評価用途** で個別レイヤを切り分けるためのフラグであり、ユーザーの本番タスク（要件・設計検索）で使うことは想定していない。検索品質測定（`meta/test_docs/` 配下の評価スクリプト）では、auto モード以外のレイヤを切り分けて精度・再現率を測る必要があるため、これらフラグは仕様として残す必要がある。

DES-001 で新規導入する forge 抽象 SKILL（`/forge:query-db-rules` / `/forge:query-db-specs`）は、available-skills と API キー有無からバックエンドを自動選択する**薄いラッパー**である。バックエンドが doc-advisor の場合でも、forge はバックエンドの内部モード（`--toc` / `--index`）を意識しない設計を取る（DES-001 §2.3 分岐テーブル B 末尾の注記、および §3.1 実行フロー 3 で「doc-advisor を呼ぶ際は `--toc` / `--index` を付けずに呼ぶ」と明記済み）。

> 注: 4 抽象 SKILL は DES-001 §2.2 改訂により `user-invocable: false`（内部 SKILL）に確定。ユーザーが `/forge:query-db-rules --toc xxx` を `/` 直接呼出する経路は成立しない。本論点はもっぱら **forge プラグイン内の他 SKILL が `$ARGUMENTS` に `--toc` / `--index` を含む文字列を渡した場合**、または **AI が description マッチで自動起動する際に同様の文字列を渡した場合**の挙動として読み替える。

ここで残る論点が **「`/forge:query-db-rules` に `--toc xxx` を含む引数が渡ったらどうなるか」** である。レビュー指摘（skill_authoring perspective id=26）では以下の懸念が示された:

- (a) `--toc` がタスク文字列として扱われ検索精度が劣化する
- (b) サイレントに無視されて期待挙動と異なる
- (c) バックエンド側でエラー化される

いずれが起きるか SKILL.md から読み取れず、引数仕様の網羅性が欠ける、というのが指摘の本質である。

## 決定: forge 抽象 SKILL は `--toc` / `--index` を正式引数として持たない

forge 抽象 SKILL は **本番運用専用** であり、`--toc` / `--index` を正式引数として受理しない。品質検査でレイヤを切り分けたい場合は、`/doc-advisor:query-rules --toc xxx` のように **doc-advisor / doc-db を直接呼び出す**。

forge 抽象 SKILL の引数表（DES-001 §3.1）は `{task}` のみとし、`--toc` / `--index` を含むユーザー入力がきた場合の挙動は **AI 判断に委ねる**（SKILL.md にゴミ引数の扱いを明記しない）。

### 検討した選択肢

| # | 選択肢                                                                               | 採否     | 根拠                                                                                                                                                                     |
| - | ------------------------------------------------------------------------------------ | -------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| A | forge 抽象 SKILL で `--toc` / `--index` を明示的に非対応化しエラー終了               | 不採用   | バリデーション層を抽象 SKILL に書く必要が生じる。バックエンド間置換可能性を維持する最小契約と矛盾する                                                                    |
| B | forge 抽象 SKILL に `--mode auto\|toc\|index` を正式引数として追加し転送             | 不採用   | 抽象 SKILL の責務は「バックエンド自動選択」であり、ユーザーがバックエンドモードを意識する設計は責務矛盾。`--toc` / `--index` は品質検査用で本番用ではない                |
| C | forge 抽象 SKILL は本番運用用、`--toc` / `--index` は直呼びに限定（AI 判断に委ねる） | **採択** | 責務分離が明確。本番運用 → forge 経由、品質検査 → doc-advisor / doc-db 直呼び。ゴミ引数は AI が文脈で判断する一貫した方針（id=2 の `--top-n` / `--doc-type` 削除と同根） |

### なぜ A（エラー化）を採らないか

エラー化は「未定義引数は弾く」明示的設計に見えるが、forge 抽象 SKILL は **薄いラッパー** として最小契約に倒している。ここに引数バリデーションを書くと、

1. SKILL.md にエラーメッセージ仕様が増えて契約が肥大化する
2. 将来 doc-advisor / doc-db 側のフラグが増えるたびに forge 側のバリデーション表を更新する必要が生じる（抽象漏れ）
3. 「ゴミ引数は AI が文脈で判断する」という DES-001 全体の一貫方針（id=2 の `--top-n` / `--doc-type` 削除でも同じ判断）と矛盾する

ため不適切である。

### なぜ B（`--mode` 正式追加）を採らないか

`--toc` / `--index` を抽象 SKILL の正式引数にすると、

1. **責務矛盾**: 抽象 SKILL の責務は「バックエンド自動選択」。ユーザーがバックエンドモードを意識する時点で抽象が破綻している
2. **品質検査用途の混在**: `--toc` / `--index` は品質検査用フラグで、本番タスクで使うことは想定外。本番用 SKILL に検査用フラグを混ぜると意図が曖昧になる
3. **バックエンド非対称**: doc-db には `--toc` / `--index` に相当するフラグがない。doc-db 採用時に警告するか無視するかの分岐を抽象側で書く必要が生じ、最小契約に反する

## 影響範囲

- `docs/specs/forge/design/DES-001_forge_query_abstraction_design.md` §3.1 引数表は `{task}` のみで現状維持（本 ADR を SoT 参照する注記を追記）
- `plugins/forge/skills/query-db-rules/SKILL.md` / `plugins/forge/skills/query-db-specs/SKILL.md`（実装時）の `argument-hint` は `"task description"` のみ。`--toc` / `--index` の非対応を SKILL.md に明記しない（AI 判断）
- `meta/test_docs/` 配下の品質評価スクリプトは本 ADR の影響を受けない（既に doc-advisor / doc-db を直接呼んでいる）

## 残存する判断事項

### 残存 1: ユーザーガイドへの記載要否

`docs/readme/guide_doc-advisor_ja.md` / `docs/readme/forge/guide_*.md` で「品質検査をする場合は doc-advisor / doc-db を直接呼ぶ」旨を記載するか否かは、ユーザー向け文書の責務として別途検討する（DES-001 §12 残課題と並列）。

### 残存 2: 品質検査の自動化導線

将来、品質検査を CI で定期実行する仕組みを導入する場合、forge ではなく doc-advisor / doc-db のフラグを直接呼ぶ運用が必要。CI 用ラッパーを別 SKILL として切り出すかは、品質検査自動化の Issue 化時点で再評価する。

## この ADR の位置づけ

本文書は DES-001（文書検索バックエンドの抽象化（switch-query）設計書）の補遺であり、以下を記録する:

- forge 抽象 SKILL の引数仕様が `{task}` のみで完結する設計判断（responsibility separation）
- `--toc` / `--index` を品質検査用と位置づけ、本番運用ラッパーから分離する根拠
- ゴミ引数を AI 判断に委ねる方針（id=2 の `--top-n` / `--doc-type` 削除と同根の一貫した設計哲学）

DES-001 は「何を作るか」を定義する。本 ADR は「なぜ抽象 SKILL に検査用フラグを取り込まないか」を記録する。

## 関連

- DES-001: 文書検索バックエンドの抽象化（switch-query）設計書（forge 抽象 SKILL の責務定義）
- doc-advisor:ADR-002_query_skill_subagent_isolation: query-rules / query-specs SKILL の subagent 隔離と read-only 制約
- doc-advisor:FNC-001_context_external_search_spec: コンテキスト外検索 要件定義書
- `meta/test_docs/README.md`: 検索品質評価の実行手順（git 管理外）

## 変更履歴

| 日付       | 変更者  | 内容     |
| ---------- | ------- | -------- |
| 2026-05-18 | k2moons | 初版作成 |
