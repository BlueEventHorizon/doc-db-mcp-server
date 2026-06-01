# FNC-005: doc-db Index 生成 要件定義書

## 概要

ドキュメントを **見出し単位の chunk** に分割し、各 chunk を Embedding ベクトル化してローカル Index に永続化する。実装は doc-advisor の embedding 機構（`plugins/doc-advisor/scripts/embed_docs.py`）を出発点として拡張する。

## 前提条件

- REQ-006 PRE-01 / PRE-02 に準ずる（`.doc_structure.yaml` の存在、OpenAI API key の参照規約は FNC-004 / DES-007 に準拠）
- 対象ドキュメントは Markdown 形式（`.md`）である

## 要件一覧

### doc-advisor からの継承

以下は doc-advisor の embedding 機構に準拠する。本要件では再定義しない。

- ファイル走査（`.doc_structure.yaml` の `root_dirs` / `target_glob` / `exclude` 尊重）
- Embedding API 呼び出し（標準ライブラリのみ、バッチ化）
- 差分更新（checksum による変更検出、新規/変更/削除/リネームの反映）
- Index の永続化形式・スキーマ系メタデータ（`schema_version` / モデル名 / 次元数）
- model mismatch / dimensions mismatch 検出時の挙動（既存 Index 保護 + `--full` 案内）
- 標準的なエラーハンドリング（API key 未設定 / レート制限 / 通信エラー / 対象ディレクトリ不在 / 削除検出中の消失）

### doc-db で追加・変更する要件

| ID     | 要件                                                                                                                                                                        |
| ------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| CHK-01 | 1 chunk は Markdown の見出し境界で切り出す。chunk には文書パスと見出し階層パス（例: `# A > ## B > ### C`）を保持する                                                        |
| CHK-02 | 見出しを持たない文書は、文書全体を 1 chunk として扱う                                                                                                                       |
| EMB-01 | 1 文書は複数のベクトル（chunk ごと、必要に応じて field ごと）を持つ（Multi-vector）。Embedding 単位がファイル単位から chunk 単位に変わる                                    |
| ST-01  | specs の Index は `.doc_structure.yaml` の `doc_types_map` で定義される doc_type 単位（`requirement` / `design` / `plan` 等）に分離管理する。rules は単一カテゴリとして扱う |
| ST-02  | Index metadata に build 完了状態（`complete` / `incomplete`）を持ち、失敗 chunk が存在する場合は `incomplete` として保存する                                                |
| OP-01  | ユーザーは検索方式比較のため、Index 全体を `--full` で再生成できる                                                                                                          |

> chunk 分割の上限 token 数・field 別ベクトルの対象 field・ハッシュアルゴリズム・永続化ファイル形式・バッチサイズ・保存先パスのバリデーション規則・migration 戦略は設計書で確定する。

## 関連要件

- 親要件: REQ-006
- 依存元: FNC-006（Index を使用）
- 関連: FNC-007（評価）、NFR-005（配布制約）
- 参照実装: `plugins/doc-advisor/scripts/embed_docs.py` / `embedding_api.py` / `create_checksums.py`

## 変更履歴

| 日付       | 変更者  | 内容                                                                                                                                     |
| ---------- | ------- | ---------------------------------------------------------------------------------------------------------------------------------------- |
| 2026-05-05 | k2moons | 初版作成                                                                                                                                 |
| 2026-05-07 | k2moons | doc-advisor 準拠に再構成。共通基盤の詳細記述（OP / EMB / ST の大半・エラーケース・TBD）を削除し、設計書に委譲。doc-db 固有要件のみに縮退 |
