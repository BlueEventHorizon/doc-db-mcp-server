# 競合調査 (Competitive Landscape)

調査日: 2026-07-03

doc-db-mcp-server と同種の機能（Markdown ドキュメントのハイブリッド検索 MCP サーバー）を
提供する OSS / 商用サービスをウェブ調査した結果のまとめ。

## 結論

**完全代替品は存在しない。** 個々の技術要素（ハイブリッド検索、MCP 対応、SQLite、単一バイナリ）は
それぞれ他プロダクトに実例があるが、全てを組み合わせた上で「Git branch を series として扱う」
「grep を独立 recall signal として温存する二層検索思想 (PHIL-01)」を持つものは見つからなかった。

## 比較表

| プロダクト                                                                                                                                                                 | 言語/配布                   | 検索方式                                         | MCP 対応            | doc-db との違い                                                                                                                          |
| -------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------- | ------------------------------------------------ | ------------------- | ---------------------------------------------------------------------------------------------------------------------------------------- |
| [Qmd](https://github.com/ehc-io/qmd)                                                                                                                                       | TypeScript/Python、npm 配布 | BM25(FTS5)+ベクトル+LLM rerank、SQLite 使用      | あり（stdio/HTTP）  | 最も近い設計思想。単一バイナリではなく npm パッケージ。series/branch 概念なし。GREP 単独 signal はなく「クエリ拡張→融合→rerank」の一本道 |
| [ck (BeaconBay)](https://github.com/BeaconBay/ck)                                                                                                                          | Rust、単一バイナリ (cargo)  | セマンティック(FastEmbed)+BM25(Tantivy)+RRF 融合 | あり (`ck --serve`) | LLM rerank なし。git branch 対応なし。dedup は blake3 のチャンクレベル変更検出（API 課金削減という設計目的は共通）                       |
| [MCP-Markdown-RAG](https://github.com/Zackriya-Solutions/MCP-Markdown-RAG)                                                                                                 | Python                      | FAISS+Whoosh(BM25)+グラフ探索のハイブリッド      | あり                | GREP 無し、series/branch 無し、単一バイナリでもない                                                                                      |
| [ragdocs (andnp)](https://glama.ai/mcp/servers/andnp/ragdocs-mcp)                                                                                                          | Python                      | セマンティック+BM25、デーモン方式                | あり                | git 履歴のセマンティック検索はあるが branch 単位の series 管理ではない                                                                   |
| [MCP Toolbox for Databases (Google Cloud)](https://medium.com/google-cloud/find-the-right-docs-every-time-announcing-versioned-documentation-for-mcp-toolbox-f94e8180d304) | —                           | バージョン管理されたドキュメント検索             | あり                | 「バージョン」はリリースバージョンの概念で、Git branch 名をそのまま series として扱う設計ではない                                        |
| [docs-mcp-server (arabold)](https://github.com/arabold/docs-mcp-server)                                                                                                    | Node.js                     | ライブラリの特定バージョンに絞った検索           | あり                | 対象はライブラリドキュメント（npm/pip 等）で自プロジェクトの Markdown 文書向けではない                                                   |

## 観点別の評価

### 1. ベクトル + BM25 + grep 3-signal 並列の MCP サーバー

Qmd や ck など「ベクトル + BM25」のハイブリッドは一般的。しかし **grep を独立 signal として
常時並列実行し `origin_signals` で出所を可視化する設計**は見当たらなかった。多くは RRF 融合や
単一パイプラインに収斂させる設計。

### 2. Git branch 単位のバージョニング (series)

MCP Toolbox や Document360 のような「バージョン管理」はリリースバージョン軸であり、
**Git branch 名をそのまま series 識別子として複数バージョンの文書を 1 つの KEY 内で共存させる**
設計は発見できなかった。doc-db の独自性が高い部分。

### 3. 単一バイナリ・pure-Go SQLite 配布

Go 実装かつ `modernc.org/sqlite` のような CGO フリードライバでの MCP ドキュメント検索サーバーは
Web 上で見当たらず、事例として稀（ck は Rust で単一バイナリを実現しているが言語が異なる）。

### 4. ハッシュベース dedup による Embedding API 課金削減

ck の blake3 チャンク変更検出は概念的に近いが、**「同一 key+path+SHA-256 一致なら Embedding
計算を完全スキップし既存 record に series を追記する」**という粒度・目的（branch 切替時の無駄な
API 課金ゼロ）を明示した設計は見つからなかった。

## doc-db-mcp-server の強み / 弱み

**強み**

- Go + pure-Go SQLite による依存フリーな単一バイナリ配布
- branch 単位 series 管理によるマルチバージョン共存
- ハッシュ dedup による明確なコスト削減保証 (DIF-02)
- grep 併用による embedding の意味的取りこぼしの補完 (PHIL-01)

**弱み**

- OpenAI Embedding / Chat Completions 依存（Qmd のようなローカルオンリー選択肢と比べて
  API コスト・外部依存がある）
- ユーザーベース・エコシステムの小ささ（Qmd/ck は既に一定の認知度がある）
- rerank が OpenAI Chat Completions 前提でローカル LLM 選択肢がない

## 参考ソース

- [Qmd (mcpmarket.com)](https://mcpmarket.com/server/qmd) / [GitHub](https://github.com/ehc-io/qmd)
- [ck (GitHub)](https://github.com/BeaconBay/ck)
- [ragdocs-mcp (Glama)](https://glama.ai/mcp/servers/andnp/ragdocs-mcp)
- [MCP-Markdown-RAG (GitHub)](https://github.com/Zackriya-Solutions/MCP-Markdown-RAG)
- [MCP Toolbox バージョン管理ドキュメント (Google Cloud Medium)](https://medium.com/google-cloud/find-the-right-docs-every-time-announcing-versioned-documentation-for-mcp-toolbox-f94e8180d304)
- [docs-mcp-server (GitHub)](https://github.com/arabold/docs-mcp-server)
- [glebarez/go-sqlite pure-Go driver](https://github.com/glebarez/go-sqlite)
