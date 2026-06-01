# ADR-001: build_index / search_index の I/O 分離と内部呼び出し設計

## ステータス

採択・修正済（2026-05-08）

## コンテキスト

doc-db の設計書 DES-026 §5.3 は SearchOrchestrator の責務として以下を定義する:

1. Index ロード
2. 鮮度確認 → stale なら差分 build を **内部で** 実行
3. incomplete chunk があれば再 embedding
4. Embedding + Lexical 検索 → Hybrid 統合 → Rerank
5. 結果を JSON で stdout に返却

つまり search は **build を内部に包含する** 設計であり、build を外部サブプロセスとして呼ぶ doc-advisor とは根本的に異なる。

しかし build_index.py の初期実装は `run_build()` 内で `print(json.dumps(...))` していたため、search 実行中に auto-rebuild が発火すると **build の JSON と search の JSON が stdout に混在** し、呼び出し側のパースが壊れる。

## 決定: build_index.py のロジック/I/O 分離

`_run_build_one()` / `run_build()` を「Dict を return する純粋関数」に変更し、`main()` のみが stdout に出力する。

### 検討した選択肢

| # | 選択肢                                              | 採否     | 根拠                                                                                                                              |
| - | --------------------------------------------------- | -------- | --------------------------------------------------------------------------------------------------------------------------------- |
| A | SKILL 層に orchestration を移す（doc-advisor 方式） | 不採用   | DES-026 §5.3 に反する。auto-rebuild は search の内部責務。evaluate.py 等のプログラマティック呼び出しで orchestration が介在しない |
| B | `contextlib.redirect_stdout` で抑制                 | 不採用   | スレッドセーフでない、全 caller にボイラープレート要、本質的問題を隠蔽                                                            |
| C | ロジックと I/O の分離                               | **採択** | Python 標準ベストプラクティス、設計書と整合、全 caller で一貫動作                                                                 |

### なぜ doc-advisor 方式（A）は不適切か

doc-advisor は「stale = エラー終了」が正常動作であり、SKILL の AI 駆動 workflow.md が build → search の 2 段呼び出しを orchestrate する。この設計は以下の前提に依存する:

- search に auto-rebuild の責務がない
- プログラマティック呼び出し元（evaluate.py 等）が存在しない
- AI エージェントが workflow.md を正しく実行する（非決定論的）

doc-db はこれら 3 前提すべてが成立しないため、A は設計矛盾を生む。

## 批判的分析: この決定の不完全性

### 問題 1（修正済）: search_index.py に同じ問題が存在していた

`build_index.run_build()` は修正済みだったが、`search_index.search()` 自体が内部で `print(json.dumps(output))` していた。

**修正**: `search()` を `Tuple[int, Dict]` を return する純粋関数に変更。`main()` のみが print する。build_index と同一パターン。

### 問題 2（修正済）: _error() の SystemExit

`search_index._error()` が `raise SystemExit(exit_code)` しており、ライブラリとして import した場合にプロセスが終了していた。

**修正**: `SearchError` カスタム例外クラスを導入。`_error()` は `SearchError` を raise し、`main()` でのみ catch して SystemExit に変換する。呼び出し元は通常の try/except でエラーハンドリング可能になった。

### 問題 3（修正済）: embedding_api.py のバッチ未分割

`EMBEDDING_BATCH_SIZE = 100` が定数として定義されていたが、実際のコードでは使われていなかった。

**根本原因**: doc-advisor からのコピー時に、doc-advisor がファイル単位（1ファイル=1 embedding）だった前提が chunk 単位（1ファイル=N chunks）に変わったことへの対応漏れ。

**修正**: `call_embedding_api()` を `EMBEDDING_BATCH_SIZE` ごとに分割して呼び出すように変更。内部実装を `_call_embedding_batch()` に分離。

### 問題 4（修正済）: _estimate_tokens の日本語問題

`llm_rerank._estimate_tokens()` は `re.findall(r"\S+", text)` で空白分割していた。日本語テキストで 10-20 倍の過小推定が発生。

**修正**: ASCII 単語数 + 非 ASCII 文字数 / 2 のハイブリッド推定に変更。完璧ではないが、日本語で 10-20 倍の誤差が 1.5-2 倍程度に改善。NFR-005（外部ライブラリ不可）の制約下での pragmatic な妥協。

### 問題 5（修正済）: error_type の分類未実装

DES-026 §4.1 は `failed_chunks.error_type` を 5 種に分類すると定義していたが、全て `"other"` で記録されていた。

**修正**: `_classify_embedding_error()` 関数を追加。HTTPError のステータスコード、URLError の reason、例外メッセージの文字列パターンから `rate_limit | timeout | 5xx | invalid_request | other` を判別する。

### 問題 6（解決済）: chunk 上限が chars vs tokens

DES-026 §6.2 は当初「1 chunk = 8192 tokens を上限」と記述していたが、実装は `MAX_CHUNK_CHARS = 8192` で characters 単位。

正確な token 計算には tiktoken 等の外部ライブラリが必要であり NFR-005 に抵触する。日本語主体の対象文書では chars 近似で実用上問題がないことから、DES-026 §6.2 を「8192 文字（chars）近似」に改定し、設計と実装を整合させた。

### 問題 7（残存・低）: 二相書き込みの原子性の限界

`save_index_and_checksums_two_phase()` は以下の順序で実行する:

1. `os.replace(index_tmp, index_path)` ← ここで crash すると…
2. `os.replace(checksums_tmp, checksums_path)`

1 と 2 の間でプロセスが kill されると、新 Index + 旧 checksums という不整合状態になる。この場合 `generated_at` 不一致で検出され `--full` 案内になるため**データ破損はしない**が、ユーザーに再生成を強いる。

真の原子性には単一ファイルへの統合か、WAL が必要だが、NFR-005 の制約下では過剰設計。現行の「検出→案内」方式は pragmatic な妥協として妥当。

## 残存する設計上の判断事項

なし。全レビュー指摘は修正済み、または設計書を改定して整合させた。

## この ADR の位置づけ

本文書は DES-026 の補遺であり、以下を記録する:

- DES-026 では「§5.3: search が build を内包する」と書かれた設計意図が、実装上どのような問題を引き起こしたか
- 採択した解法（ロジック/I/O 分離）の合理性と限界
- 発見された問題とその修正内容（問題 1-5: 修正済）
- 実装が設計書に忠実でない箇所（chars/tokens 乖離）の明示的記録

DES-026 は「何を作るか」を定義する。本 ADR は「なぜこう作ったか」「何が壊れていたか」「どう直したか」を記録する。

## 変更履歴

| 日付       | 変更者  | 内容     |
| ---------- | ------- | -------- |
| 2026-05-08 | k2moons | 初版作成 |
