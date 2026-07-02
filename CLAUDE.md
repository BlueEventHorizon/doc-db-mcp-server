# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## プロジェクト概要

Markdown ドキュメントの **ハイブリッド検索 MCP サーバー**（Go / pure-Go SQLite / OpenAI Embedding + BM25 + 全文 GREP + LLM Rerank）。
Streamable HTTP transport で MCP 2025-03 プロトコルを話す。単一バイナリで配布し、
Homebrew 自家 tap (`blueeventhorizon/doc-db`) からインストールする。

**canonical version は `VERSION` ファイル**。`CHANGELOG.md` の最新見出しがこれと一致する必要がある。
現バージョンは常に `cat VERSION` で確認する（会話中に古い値を口述しない）。

## よく使うコマンド

```bash
make build              # VERSION から ldflags 経由でバージョン注入してビルド
make test               # go test ./...
make verify             # verify-version + verify-tag を順に実行
make verify-version     # VERSION / CHANGELOG / .version-config.yaml / Formula tag の静的整合検証
make verify-tag         # Formula revision == git tag commit SHA 検証（tag 作成後のみ pass）

go test -race ./...             # レース検出テスト
go test ./internal/mcp/ -run TestUpsert_DIF02_SameHashSkips -v   # 単一テスト実行
```

`dprint.jsonc` がプロジェクトルートにある。コミット前に `dprint fmt` を必ず走らせる
（未整形のまま commit すると他人の fmt commit で無関係 diff が混入する）。

## アーキテクチャの要点

### 二層検索 (PHIL-01)

本サーバーは「関連候補を **取りこぼしなく** 返す」に責任を持ち、
本文の最終判定は **上位 AI agent 側**に委ねる二層構成。

- **Layer 1（本サーバー）**: Embedding + BM25 + GREP を並列実行し **over-recall** な候補プールを返す
- **Layer 2（AI agent）**: `origin_signals` / `heading_path` / 本文で最終判定

LLM Rerank は **ranking 最適化オプション**であり、recall を広げる手段ではない (PHIL-02)。
検索結果の各 chunk には必ず `origin_signals: ["emb","lex","grep"]` が付き、どの signal で拾われたかが分かる (QRY-OUT-03)。

### レイヤー方向

上位のみが下位を参照する。循環依存なし。

```
cmd/docdb           エントリポイント・設定読み込み・配線
internal/mcp        MCP ツールハンドラ（7 種）
internal/search     3 signal 検索パイプライン（emb / lex / grep / rerank）
internal/reranker   OpenAI Chat Completions ベース LLM Rerank
internal/chunker    Markdown → 見出し境界チャンク分割
internal/embedder   OpenAI Embedding API（部分失敗対応）
internal/fetcher    URL → コンテンツ取得（SSRF 防御付き）
internal/expiry     TTL / LRU 自動廃棄ワーカー
internal/store      SQLite 読み書き・WAL・アトミック AppendAndCleanSeries
internal/config     YAML 設定ローダー（~/.doc-db/doc-db.yaml）
```

### データモデルの中核概念

- **KEY**: インデックスの名前空間（例: `myrepo-specs` / `myrepo-rules`）。opaque な文字列
- **series**: 同一 KEY 内の複数バージョン識別子。**Git branch 名がそのまま入る想定**
- **path**: `upsert_documents` 時に指定する文書の識別子（相対パス）
- **record**: `(key, path, content_hash)` で 1 意。`series_keys` に紐づく series リストを持つ

### ハッシュベース dedup (DIF-02) — 不変条件

**同一 `key + path` で内容が SHA-256 一致するファイルは Embedding を再計算しない。**
既存 record の `series_keys` に新 series を追記して skip 扱いする。

このため branch を切り替えて再 upsert しても、内容不変ならば API 課金ゼロ。
テストで常時検証されている:

- `internal/store/store_test.go::TestAppendAndCleanSeries_DIF02` — Store 層の series 追記 + 空 record 物理削除
- `internal/mcp/mcp_test.go::TestUpsert_DIF02_SameHashSkips` — MCP handler 層の skip + series_keys 両紐付き
- `internal/mcp/upsert_integration_test.go::TestUpsertIntegration_DIF02_DoesNotCallEmbedder` — Embedder spy で API 呼び出しゼロを保証

**この不変条件を壊す変更は必ずテストが落ちる**。逆に言えばこれらのテストは常に緑を保つ必要がある。

### 3 signal 検索モード

`query` ツールの `mode` パラメータ:

| mode                   | 用途                                                  |
| ---------------------- | ----------------------------------------------------- |
| `all`                  | **デフォルト (v0.1.5+)**。emb + lex + grep を並列実行 |
| `rerank`               | 3 signal 候補収集後 LLM で再ランク                    |
| `emb` / `lex` / `grep` | 単独 signal                                           |
| `hybrid`               | legacy: emb + BM25 RRF 融合 (grep なし)               |

デフォルトが `all` である背景は PHIL-01（recall 優先）。変更する際は要件書 APP-001 に立ち返る。

## 設定と起動

- **設定ファイル**: `~/.doc-db/doc-db.yaml`（固定パス、fail-fast、未知キー禁止 = CFG-01 / CFG-03）
- **API キー**: `OPENAI_API_DOCDB_KEY` 環境変数（YAML には書かない）
- **サンプル**: `doc-db.yaml.example`（同梱、Formula が `share/doc-db/` にインストール）
- **db_path**: `~/` プレフィックスは v0.1.10+ で `$HOME` に展開される

## リリースワークフロー（毎バージョン必須）

Formula の `revision:` は git tag 確定後にしか正しい SHA を書けないので **2 段階 commit** で運用する
（v0.1.7 以降の全 bump で踏襲）。**現在どの段階にいるかを常に把握し、次の段階を能動的に提案する**こと。

### フェーズ 0: develop ブランチで bump 準備

`/forge:update-version <patch|minor|major>` で以下を自動更新:

- `VERSION`
- `CHANGELOG.md`（`[Unreleased]` セクションを新版へ昇格 + 新規 `[Unreleased]` を追加）
- `Formula/doc-db.rb` の `tag:`

**手動**: Formula の `revision:` を `"0000000000000000000000000000000000000000"` にリセット
（`sync_files` は SHA を扱えないため）。

検証: `make verify-version` が全 ok になるまで進まない。

### フェーズ 1: bump commit（develop）

```bash
/anvil:commit    # または手動:
git add VERSION CHANGELOG.md Formula/doc-db.rb .version-config.yaml README.md
git commit -m "chore: bump version to X.Y.Z"
git push
```

### フェーズ 2: main へ merge

```bash
git checkout main
git merge develop --no-ff -m "Merge develop: vX.Y.Z <一言概要>"
git push origin main
```

または GitHub PR 経由（プロジェクトに CI があれば PR 推奨）。

### フェーズ 3: git tag を main に打つ

```bash
git checkout main
git pull                                   # merge を確実に取り込む
git tag vX.Y.Z                             # main HEAD (= merge commit) に tag
tag_commit=$(git rev-parse "vX.Y.Z^{commit}")
echo "tag_commit: $tag_commit"
```

### フェーズ 4: Formula revision を実 SHA に確定（main）

```bash
# Formula/doc-db.rb の revision: を $tag_commit に書き換える
# (Edit ツールで "0000...0000" → $tag_commit)

git add Formula/doc-db.rb
git commit -m "chore(release): Formula revision を vX.Y.Z tag commit に確定

tag vX.Y.Z → $tag_commit
verify_release_tag.sh: VER-07 OK"

make verify-tag   # revision == tag commit SHA を検証。落ちたらやり直し
```

### フェーズ 5: 全部 push

```bash
git push origin main
git push origin vX.Y.Z
```

### フェーズ 6: main から develop へ戻す

```bash
git checkout develop
git merge main --ff-only    # main の chore(release) commit を develop に取り込む
git push
```

これをしないと develop の Formula に古い revision (`0000...0`) が残り続け、次リリースの
`verify-version` は通るが view 上のノイズになる。

### 現状を判定する目印

- Formula `revision:` が `"0000...0000"` → **フェーズ 1〜3 の途中**（bump 済みだが tag 未確定）
- Formula `revision:` が実 SHA + `verify-tag` pass → **リリース完了**
- develop に main の chore(release) commit が反映されていない → **フェーズ 6 が未実施**

詳細な背景と回避すべきパターン（`--amend` + `git tag -f` など）: `.claude/skills/setup-homebrew-formula/references/release_workflow.md`

## `.claude/skills/` の構成

このリポジトリには 6 つの SKILL がある。**doc-db を使う側** (`update-db-*` / `query-db-*` / `delete-db-series`) と
**doc-db を配布する側** (`setup-homebrew-formula`) が同居している。

`update-db-*` / `query-db-*` / `delete-db-series` は **doc-db サーバの HTTP エンドポイント
(`http://localhost:<port>/mcp`) を Python stdlib のみで直接叩く**（v0.1.11+）。
Claude Code の MCP 登録は不要。他プロジェクトへ 5 ディレクトリを rsync すれば単独で動く設計。

- **`docdb_client.py`** (5 SKILL に同一コピー): MCP Streamable HTTP handshake を stdlib のみで実装。
  `upsert` はデフォルト 30 件バッチ + 進捗を stderr に表示（600+ ファイルでもハングに見えない）
- **`resolve_docs.py`** (5 SKILL に同一コピー): `.doc_structure.yaml` v3.0 を stdlib のみでパース。
  forge の `resolve_doc_structure.py::parse_config` 互換の行ベース YAML parser を内蔵

**依存**: Python 3.9+ stdlib のみ。**PyYAML など外部パッケージを追加しない**（プロジェクト方針）。

将来は forge に統合予定（現状は本プロジェクト内で自己完結）。

## 参照すべき仕様書

コード改変前に該当する設計書を必ず読む（recall 優先の設計思想があるため、要件と設計を跨いでいる）:

| 変更対象                                                | 参照                                                                            |
| ------------------------------------------------------- | ------------------------------------------------------------------------------- |
| MCP tool の入出力 / 3 signal の挙動                     | `docs/specs/base/design/DES-001`                                                |
| 要件レベルの ID (FNC / DIF / QRY / GRP / ALL / PHIL 等) | `docs/specs/base/requirements/APP-001`                                          |
| Homebrew Formula / インストール手順                     | `docs/specs/install/design/DES-002` / `docs/specs/install/requirements/APP-002` |
| AI agent から doc-db を使う実装ガイド                   | `docs/AI_INTEGRATION_GUIDE.md`                                                  |

## 避けるべきこと

- **`revision:` を手動更新し忘れる**: bump commit で 40 桁 0 に戻さないと、次リリース後に古い commit を指し続ける
- **PyYAML など外部 Python 依存の追加**: SKILL 群は stdlib のみで動く前提。Ruby / Go 側の依存追加は別議論
- **silent failure**: エラー catch 後の continue / 空値 fallback は必ず log + caller 伝播（feedback rule）
- **DIF-02 の不変条件を壊す変更**: 上記 3 テストが必ず落ちる。逆にこれらのテストを緑に保つよう実装する
- **`.doc_structure.yaml` を PyYAML で読む前提のコード**: forge の行ベース parser 互換の実装を維持する
