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
- **数の上限**: なし。doc-db は「削除すべきかどうか」の判定を一切しない (FNC-007)。
  不要な KEY はユーザーが `trash_index` で明示的にゴミ箱投入した場合のみ、
  保持期間経過後に自動最終処分される

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
  embedding を共有 (再 embedding スキップ)。この dedup は **series 非依存**:
  同一 KEY の別 series に同一内容が登録済みなら、新 series の初回
  upsert / sync でも skipped になり embedding API は呼ばれない (DIF-02)
- ただし成立条件は `key + path + content` の**完全一致**。`path` の表記が
  変わった場合 (プレフィックス変更等) や content に差分がある場合は
  re-embedding (processed) になる。期待に反して processed が多い場合は、
  サーバーログ (level=debug) の re-embedding 理由
  (`embedding new path` / `re-embedding (content changed ...)`) で切り分けられる
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

doc-db は 11 個の MCP ツールを提供します。詳細スキーマは `tools/list` で取得できます。

| Tool                         | 目的                                                                                                     |
| ---------------------------- | -------------------------------------------------------------------------------------------------------- |
| **`upsert_documents`**       | ドキュメントを KEY に追加・更新 (チャンク分割 + embedding)                                               |
| **`sync_documents`**         | desired-state 同期 (v0.2.0+)。完全な現在ファイル一覧を渡し、削除にも追従する非同期ジョブ                 |
| **`get_sync_status`**        | sync ジョブの進捗・完了・エラーを job_id でポーリング (v0.2.0+)                                          |
| **`delete_documents`**       | 特定 path 群のドキュメントから series を除去 (`paths[]` 必須)                                            |
| **`delete_series`**          | KEY 内の全 record から指定 series を除去 (branch cleanup 用途、`paths` 不要)                             |
| **`schedule_delete_series`** | series 全体の削除予約 (v0.2.0+)。即時削除せず起動時スイープ/trash.Worker定期実行で物理削除・取り消し可能 |
| **`query`**                  | 候補プールを検索 (3 signal 並列)                                                                         |
| **`list_indexes`**           | 登録済み KEY の一覧 + メタ情報 (chunk 数を含む。ゴミ箱状態の KEY は除外)                                 |
| **`trash_index`**            | KEY をゴミ箱投入 (即時物理削除はしない。保持期間経過後に自動最終処分。`delete_index` を置換)             |
| **`list_trashed_indexes`**   | ゴミ箱内の KEY 一覧 + 自動最終処分までの残り時間を取得                                                   |
| **`restore_index`**          | ゴミ箱内の KEY を自動最終処分前に利用可能な状態へ戻す                                                    |

**`upsert_documents` と `sync_documents` の使い分け**:

- `upsert_documents`: 追加・更新のみ (削除されたファイルには追従しない)。少数ファイルの
  差分投入や動的生成ドキュメント向け
- `sync_documents`: **ファイル一覧全体を管理する定期同期向け (推奨)**。documents を
  「当該 key・series の完全な現在状態」とみなし、一覧に無い既存 path を series から
  即時に切り離す (当該 series 指定の検索から直ちに消える)。差分管理は upsert と同一
  (hash 一致で embedding 再計算なし) なので、全ファイルを毎回渡しても課金は差分のみ。
  job_id が即時返却されるため、`get_sync_status` で完了をポーリングする

**削除系 3 ツールの使い分け**:

- `delete_documents`: 特定 path 群から series を除去したい時 (個別ドキュメント削除・即時)
- `delete_series`: KEY 全体から series を一括除去したい時 (即時物理削除。path 列挙不要)
- `schedule_delete_series`: branch 削除を検知したが**即時削除はしたくない**時。予約は
  次回サーバー起動まで完全に無害で、同一 key・series への `sync_documents` で取り消せる
  (誤操作に安全)

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
  series: "main",   // 登録に使っている series を指定する (下記注記)
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

**`series` を指定する理由 [推奨]**: `sync_documents` で登録している場合、当該 series は
「その時点の完全な現在状態」であり、削除したファイルは sync 完了直後に当該 series から
外れる (SYN-03)。一方 `series` 省略時の KEY 全体検索は全 series 横断であるため、他 series
にしか無い文書に加えて、**sync で切り離された削除済み文書が物理削除まで結果に混入し得る**
(§4.5 既知の制約)。

`series` 指定でヒット 0 件だった場合に **series 省略で再検索しないこと**。当該 series が
desired-state である以上、0 件は「その series に該当文書が無い」という正しい答えであり、
他 series の文書で代替すると削除済み・別バージョンの文書を掴む。series 自体が未登録
(`list_indexes` の `series[]` に無い) 場合も同様に、横断検索へ切り替えるのではなく
`sync_documents` の実行を促すのが安全側の設計である。

**ただし `list_indexes` の `series[]` は「一度も sync していない」と「sync 済みだが
desired-state が空だった」を区別できない**。`series[]` は record に現在紐づく series から
作られるため、空の `documents` で sync した series は一覧から消える（空リストは正当な
desired-state として受理される。§5.4）。後者に対して `sync_documents` の再実行を促しても
状況は変わらない（再実行しても 0 件のままで series は現れない）。クライアント側で
**送信対象ファイル数が 0 かどうかを先に判定**し、0 件なら「対象文書が無い」として扱い、
1 件以上あるのに `series[]` に無い場合のみ未同期として `sync_documents` を促すこと。

### 5.3 branch 更新

**推奨 (v0.2.0+)**: `sync_documents` で desired-state 同期する。ファイルの追加・更新に
加えて**削除にも追従**する (一覧に無い path は series から即時に消える)。

```javascript
// feature ブランチに切替えた → 現在の全ファイル一覧を渡して同期
const { job_id } = await mcp.call("sync_documents", {
  key: "myrepo-docs",
  series: "feature-auth",
  documents: [...allCurrentFiles], // 完全な現在状態。差分は hash で自動判定 (課金は差分のみ)
});

// 完了をポーリング
let status;
do {
  await sleep(1000);
  status = await mcp.call("get_sync_status", { job_id });
} while (status.status === "running");
// status: { status: "done", processed, skipped, failed, deleted_paths_marked, errors? }

// branch 削除を検知したら series 全体を削除予約 (即時削除しない・sync で取り消し可能)
await mcp.call("schedule_delete_series", {
  key: "myrepo-docs",
  series: "old-feature",
});
```

従来方式 (upsert + 明示削除) も引き続き使える:

```javascript
// 追加・更新のみ (削除ファイルには追従しない)
await mcp.call("upsert_documents", {
  key: "myrepo-docs",
  series: "feature-auth",
  documents: [...]
});

// 不要になった path を明示削除
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
- 不要になった KEY は `trash_index` でゴミ箱投入すると、保持期間 (`trash.retention_days`、
  デフォルト 3 日) 経過後に自動的に物理削除される。保持期間内は
  `restore_index` で復活できる。TTL / LRU による自動削除判定は行わない
  (FNC-007。doc-db は「削除すべきかどうか」の判定をしない)

---

## 8. 独自クライアント / SKILL を作る

doc-db は「クライアントが自分の文書一覧を管理し、サーバーへ同期・検索する」設計のため、
運用にはクライアント実装が必要になる。本リポジトリの
[`.claude/skills/`](../.claude/skills/README.md) に **Claude Code 用 SKILL 6 種の
参考実装**（Python 3.9+ stdlib のみ・MCP 登録不要）を同梱している。
独自クライアントを作る場合の基本形は次の 3 段:

```
1. 一覧解決   : 自分の管理対象ファイルの完全な一覧を作る
                (参考実装: resolve_docs.py が .doc_structure.yaml から解決)
2. 同期       : sync_documents に一覧全体を渡す (local_path 経路なら payload は小さい)。
                job_id が即時返る
3. 完了待ち   : get_sync_status(job_id) を done/failed までポーリング
                (参考実装: docdb_client.py の sync-start + sync-status / run_sync.py --start-only)
```

実装上の要点:

- **MCP handshake**: Streamable HTTP は `initialize` → `notifications/initialized` →
  `tools/call` の順で、`Mcp-Session-Id` ヘッダをセッション維持に使う。stdlib (urllib)
  だけで書ける最小実装が `docdb_client.py` にある（SSE / JSON 両レスポンス対応込み）
- **一覧は必ず「完全な現在状態」を渡す**: sync_documents は一覧に無い既存 path を
  series から切り離す。部分的な一覧を渡すと残りが削除扱いになる（部分投入をしたい
  場合は upsert_documents を使う）。全件渡しても DIF-02 により課金は差分のみ
- **空一覧のガード**: 一覧解決の設定ミスで空リストを渡すと全 path 切り離しになる。
  参考実装 (`run_sync.py`) は 0 件時に同期しない安全弁を入れている
- **タイムアウト設計**: sync のジョブはサーバー側で継続するため、ポーリングを打ち切っても
  再実行すれば冪等に収束する。クライアント側は気軽に中断してよい
- **branch 連動**: KEY を「プロジェクト名 + 種別」、series を「git branch 名」で自動決定
  すると、branch 切替・削除と自然に連動する（参考実装の命名規則）
- **エージェント経由での進捗表示**: `job_id` 取得（ステップ 2）と完了待ちポーリング
  （ステップ 3）を **1 プロセス内で完結させると**（`docdb_client.py sync` の内部実装）、
  進捗はプロセスの stderr に出るだけになる。CLI ツールを人間が直接ターミナルで実行する
  場合はそれで見えるが、Claude Code のような「エージェントが Bash 経由でツールを叩き、
  結果をチャットで報告する」統合では、**Bash 実行結果はユーザーに直接見えない**ため
  stderr の進捗は届かない。エージェント統合で進捗をユーザーに見せたい場合は、
  ステップ 2 とステップ 3 を分離した低レベル API（`sync-start` で job_id だけ即座に
  受け取り、`sync-status` を呼び出し側が間隔を空けて繰り返し呼ぶ）を使い、
  エージェント自身がポーリングのたびにテキストで進捗を報告すること

## 関連ドキュメント

- [同梱 SKILL 参考実装](../.claude/skills/README.md) — 配布方法・`.doc_structure.yaml` の書式
- [APP-001 要件定義書](specs/base/requirements/APP-001_doc_db_mcp_server_requirements.md) — 要件詳細
- [DES-001 設計書](specs/base/design/DES-001_doc_db_mcp_server_design.md) — 内部設計詳細
- [README.md](../README.md) — install / build / 開発者向け情報
- [CHANGELOG.md](../CHANGELOG.md) — 変更履歴
