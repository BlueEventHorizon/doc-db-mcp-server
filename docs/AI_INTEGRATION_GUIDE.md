# doc-db AI 統合ガイド

このドキュメントは **AI skill / agent から doc-db MCP サーバーを使う開発者** 向けの
利用ガイドです。MCP `tools/list` で見える情報を補完し、設計思想・典型フロー・
ベストプラクティスを示します。

doc-db の設計詳細・実装は [DES-001](specs/base/design/DES-001_doc_db_mcp_server_design.md)
を参照してください (内部設計書、必読ではありません)。

---

## 1. 設計思想 — 二層検索アーキテクチャ (PHIL-01 / PHIL-02)

### 1.1 なぜこの設計か

開発文書検索においては **「取りこぼし (recall miss)」が precision 低下より致命的** です。
必要な情報が結果に含まれないと AI agent の判断が劣化します。

このため doc-db は **2 つのレイヤー** に責務を分けます:

| Layer       | 担当                | 責務                                              |
| ----------- | ------------------- | ------------------------------------------------- |
| **Layer 1** | doc-db (本サーバー) | **取りこぼし無き候補プール** を返す (over-recall) |
| **Layer 2** | 呼び出し側 AI agent | 候補本文を読んで関連性判断、親 Claude に返す      |

Layer 1 では Embedding + BM25 + 全文 GREP の 3 signal を **並列実行** します。
3 signal は互いに代替不能で、異なる種類の取りこぼしを埋め合う関係にあります:

| signal    | 強み                         | 弱み                                   |
| --------- | ---------------------------- | -------------------------------------- |
| Embedding | 言い換え・抽象概念・多言語   | 固有 ID/低頻度トークンを散らかす       |
| BM25 lex  | トークン頻度に基づく確実性   | トークナイザ境界で割れる、意味理解なし |
| 全文 GREP | literal 一致で取りこぼしゼロ | 意味類似は解さない                     |

### 1.2 LLM Rerank の位置付け (PHIL-02)

LLM Rerank は **ranking 最適化** であり、recall を広げる手段ではありません。
Rerank 入力に正解が含まれていなければ救えません。`mode=rerank` を使うと
3 signal の候補プールを LLM が並び替えますが、PHIL-02 に従い「Rerank 未使用時も
同等の signal recall を持つ」ことが保証されます。

---

## 2. 概念モデル

### 2.1 KEY

インデックスの論理単位。複数のドキュメントセットを分離するための opaque 文字列。

```
例:
  "myrepo-docs"       — リポジトリ A の全文書
  "myrepo-design"     — リポジトリ A の設計書のみ
  "project-x-specs"   — プロジェクト X の仕様書
```

**KEY 設計のベストプラクティス**:

- **粒度**: 一緒に検索したい範囲を 1 KEY にまとめる。横断検索したくないものは別 KEY に
- **命名**: human readable。クライアント側で意味が分かる名前
- **数の上限**: なし (ただし `expiry.max_chunks` 超過時に LRU で古い KEY が削除される)

### 2.2 series

同一 KEY 内の時系列タグ。同じ path のドキュメントが時間と共に変化する場合の管理に使う。

```
例:
  "main"           — メインブランチ
  "feature-auth"   — feature ブランチ
  "v1.2.3"         — 特定リリース
  "2026-01"        — 月次スナップショット
```

**series の動作**:

- 同一 content (= 同一 SHA-256 ハッシュ) のドキュメントは複数 series で
  embedding を共有 (再 embedding スキップ)
- 同 path 同 series で content が変わると新 record 作成 + 旧 record から
  当該 series を除去
- `delete_documents` で series 単位の削除可能

**series 戦略の例**:

- branch ベース: `main` / `feature-x` / `pr-123`
- バージョンベース: `v1.2.3` / `v2.0.0`
- 時系列ベース: `2026-01` / `2026-02`

### 2.3 path

各ドキュメントの識別子。`KEY + series + path` の組で一意。クライアントが自由に定義可。

```
例: "README.md", "src/api.md", "docs/spec/auth.md"
```

---

## 3. 提供ツール (MCP)

doc-db は 6 つの MCP ツールを提供します。詳細スキーマは `tools/list` で取得できます。

| Tool                   | 目的                                                       |
| ---------------------- | ---------------------------------------------------------- |
| **`upsert_documents`** | ドキュメントを KEY に追加・更新 (チャンク分割 + embedding) |
| **`delete_documents`** | 特定 path のドキュメントから series を除去                 |
| **`query`**            | 候補プールを検索 (3 signal 並列)                           |
| **`list_indexes`**     | 登録済み KEY の一覧 + メタ情報                             |
| **`delete_index`**     | KEY 全体を物理削除 (破壊的)                                |
| **`manage_index`**     | KEY ごとの廃棄ポリシー (TTL / max_chunks) 設定             |

---

## 4. `query` ツールの使い分け

### 4.1 mode の選び方

| mode                   | 推奨ケース                                   | 動作                                   |
| ---------------------- | -------------------------------------------- | -------------------------------------- |
| **`all`** (デフォルト) | 通常の検索全般 (推奨)                        | 3 signal 並列 + 合算                   |
| `rerank`               | ranking 精度が重要、レイテンシ許容           | all + LLM (gpt-4o-mini) で再ランキング |
| `emb`                  | 純粋に意味類似だけ見たい                     | ベクトル類似度のみ                     |
| `lex`                  | BM25 単独で検証                              | 語彙頻度のみ                           |
| **`grep`**             | **固有 ID/関数名/特殊用語** を確実に拾いたい | literal 一致のみ                       |
| `hybrid`               | legacy (推奨せず)                            | emb+lex RRF (grep 含まず)              |

**判断指針**:

- 普段は `mode=all` で十分。AI agent が候補本文を読んで判定する Layer 2 設計のため
- 抽象的・言い換えの多い質問は `rerank` でランキング精度を上げる選択肢あり
  (ただしレイテンシ ~10s、課金あり)
- 「特定の関数名や ID を含む箇所だけ知りたい」なら `grep` が最速・最確実

### 4.2 `origin_signals` の解釈

各 chunk が「どの signal でヒットしたか」を配列で返します:

```json
{
  "path": "docs/api.md",
  "origin_signals": ["emb", "grep"],
  "score_breakdown": { "emb": 0.85, "lex": 0, "grep": 2, "rrf": 0, "rerank": 0 }
}
```

- `["emb"]` のみ: 意味的に近いが literal 一致なし → 言い換えで見つかった
- `["grep"]` のみ: 意味は遠いが literal 一致あり → 固有 ID 等
- `["emb", "lex", "grep"]`: 3 signal 全部でヒット → **強い候補**

**AI agent のフィルタリング指針**:

- 複数 signal でヒットした候補は概ね信頼度が高い
- 1 signal のみの候補は本文を読んで判定する価値あり (Layer 2 の役目)

### 4.3 `stage_stats` で recall を健全性チェック

```json
{
  "stage_stats": {
    "emb_candidates": 138,
    "lex_candidates": 5,
    "grep_candidates": 0,
    "merged_candidates": 30,
    "rerank_candidates": 30
  }
}
```

- `*_candidates` が極端に少ない (0 や 1-2) → そのクエリで当該 signal がほぼ機能していない
- 例: 英語クエリで `lex_candidates=0` → BM25 が日本語コーパスで空振り → emb/grep が
  recall を支える
- `merged_candidates` (mode=all/rerank) が `top_n` 未満なら候補不足

### 4.4 `warnings` の見方

致命的でない異常はここに集約されます (silent failure 禁止方針):

```json
"warnings": [
  "emb fallback triggered (lex_hits=0 / emb_hits=138 < 0.05)",
  "rerank fallback to RRF: rerank API error: ..."
]
```

- `emb fallback triggered`: lex がほぼ空振りで RRF が不安定 → emb-only モードに切替えた
- `rerank fallback`: LLM Rerank が失敗、ranking 最適化スキップ → 結果は merge 順
- `TouchKey failed`: last_accessed_at 更新失敗 (廃棄ポリシーに影響)

`warnings` が空配列なら全 signal 正常動作。

---

## 5. 典型フロー

### 5.1 初回セットアップ

3 種類の投入経路 (`content` / `url` / `local_path` は排他、exactly-one)。ローカル運用なら
**`local_path` (絶対パス) 推奨** — payload が小さくなり、大容量ドキュメントでも軽快:

```javascript
// パターン A: local_path (ローカルファイル、payload 削減)
await mcp.call("upsert_documents", {
  key: "myrepo-docs",
  series: "main",
  documents: [
    { path: "README.md", local_path: "/Users/me/proj/README.md" },
    { path: "src/api.md", local_path: "/Users/me/proj/src/api.md" }
  ]
});

// パターン B: content 直接送信 (リモート client 等、file access 不可な場合)
await mcp.call("upsert_documents", {
  key: "myrepo-docs",
  series: "main",
  documents: [
    { path: "README.md", content: "# Project\n..." },
  ]
});

// パターン C: url 取得 (公開ドキュメントの一括取り込み)
await mcp.call("upsert_documents", {
  key: "external-docs",
  series: "main",
  documents: [
    { path: "spec.md", url: "https://example.com/spec.md" }
  ]
});
// → { processed: N, skipped: 0, failed: 0 }
```

### 5.2 検索 → 本文判定

```javascript
// 2. 検索 (mode=all がデフォルト)
const r = await mcp.call("query", {
  key: "myrepo-docs",
  query: "認証エラーのハンドリング"
});

// 3. Layer 2: AI agent が候補本文を読んで関連性判定
for (const hit of r.results) {
  if (looksRelevant(hit.text)) {
    relevant.push(hit);
  }
}

// 4. warnings をチェック
if (r.warnings?.length > 0) {
  console.warn("検索に異常:", r.warnings);
}
```

### 5.3 branch 更新

```javascript
// feature ブランチに切替えた → 該当 series を更新
await mcp.call("upsert_documents", {
  key: "myrepo-docs",
  series: "feature-auth",
  documents: [...]
});

// 不要になった series を削除
await mcp.call("delete_documents", {
  key: "myrepo-docs",
  series: "old-feature",
  paths: ["src/old.md", "src/legacy.md"]
});
```

---

## 6. エラー処理ベストプラクティス

### 6.1 部分失敗の扱い

`upsert_documents` は致命的でない失敗があっても処理を続行します:

```json
{
  "processed": 8,
  "skipped": 2,
  "failed": 1,
  "errors": [
    { "path": "missing.md", "error": "fetch: 404" },
    {
      "path": "partial.md",
      "error": "partial embedding failure",
      "skipped_chunks": [3]
    }
  ]
}
```

- `failed > 0` → 該当 path は完全に処理できず。retry 候補
- `skipped_chunks` あり → 部分的に成功 (テキストは保存、一部 vector 欠落)

### 6.2 致命的エラー

以下は例外として throw されます (MCP error response):

- KEY が存在しない (query / upsert で必須)
- 入力バリデーション (key/series 必須、content+url 排他等)
- OpenAI API キー未設定 (起動時 fail-fast)

### 6.3 warnings は必ず確認

silent failure 禁止方針のため、致命的でない異常は全て `warnings` に集約されます。
監視・運用観点で重要:

- ログだけ見ていても見落とすので **`warnings` を必ずチェック**
- フォールバック発動が頻発しているなら設計見直しのシグナル

---

## 7. よくある質問

### Q1. mode=all がデフォルトだが、いつ別 mode を使う?

- **`grep`**: 「`OPENAI_API_KEY` を使う箇所」「`FNC-001` の仕様」のように **特定文字列が含まれる箇所**
  を確実に拾いたい時。query 文字列がそのまま検索される
- **`rerank`**: 質問が抽象的・複雑で、ranking 精度が必要な時。レイテンシ ~5-15s
- **`emb`**: 純粋に意味類似だけ見たい時 (デバッグ用途が中心)

### Q2. top_n はどう決める?

Layer 2 の AI agent が **本文を読んで判定** する設計なので、生 ranking より recall 重視。

- 通常: `top_n: 10` (デフォルト)
- 重要な検索や難しいクエリ: `top_n: 20-30`

### Q3. KEY と series の使い分けは?

- **別の文書セット** (異なるプロジェクト・別チーム・別テーマ) → **別 KEY**
- **同じ文書の異なる時点 / branch / バージョン** → **同 KEY 別 series**

### Q4. embedding コスト管理

- 同一 content の再 upsert は再 embedding スキップ (DIF-02)
- 内容変更 (hash 不一致) でのみ embedding API が呼ばれる
- 不要になった KEY は `delete_index` で物理削除可
- `expiry.max_chunks` 上限超過で LRU 自動削除も働く

---

## 関連ドキュメント

- [APP-001 要件定義書](specs/base/requirements/APP-001_doc_db_mcp_server_requirements.md) — 要件詳細
- [DES-001 設計書](specs/base/design/DES-001_doc_db_mcp_server_design.md) — 内部設計詳細
- [README.md](../README.md) — install / build / 開発者向け情報
- [CHANGELOG.md](../CHANGELOG.md) — 変更履歴
