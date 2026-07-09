// doc-db MCP サーバーのエントリポイント。
// 設定読み込み・依存初期化・MCP HTTP サーバー起動を行う（DES-001 §3.1）。
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/BlueEventHorizon/doc-db-mcp-server/internal/chunker"
	"github.com/BlueEventHorizon/doc-db-mcp-server/internal/config"
	"github.com/BlueEventHorizon/doc-db-mcp-server/internal/embedder"
	"github.com/BlueEventHorizon/doc-db-mcp-server/internal/fetcher"
	docdbmcp "github.com/BlueEventHorizon/doc-db-mcp-server/internal/mcp"
	"github.com/BlueEventHorizon/doc-db-mcp-server/internal/reranker"
	"github.com/BlueEventHorizon/doc-db-mcp-server/internal/search"
	"github.com/BlueEventHorizon/doc-db-mcp-server/internal/store"
	"github.com/BlueEventHorizon/doc-db-mcp-server/internal/trash"
)

// version はビルド時に -ldflags "-X main.version=..." で上書きされる（DES-002 §4.2）。
// VERSION ファイルが canonical（APP-002 VER-01）。手元 `go build` でこの値を埋めるには
// Makefile の build target を使うか、以下のワンライナーを実行する:
//   go build -ldflags "-X main.version=$(cat VERSION)" -o doc-db ./cmd/docdb
var version = "dev"

func main() {
	// --version は設定ファイル読み込み・API キー検証・Store/Trash 初期化より前に処理する（APP-002 VER-03）。
	// Homebrew test（brew test doc-db）はこの分岐を踏んで即時終了するため、
	// 設定ファイルや API キーがなくてもパスする。
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Println(version)
		return
	}

	// --show-config: 起動せずに解決済み設定 (log/db パス等) だけ表示する。
	// `make show-log` や運用者がログ・DB の実配置場所を確認する用途。
	if len(os.Args) > 1 && os.Args[1] == "--show-config" {
		if err := showConfig(); err != nil {
			fmt.Fprintln(os.Stderr, "設定読み込み失敗:", err)
			os.Exit(1)
		}
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		slog.Error("サーバー終了", "error", err)
		os.Exit(1)
	}
}

// showConfig は --show-config フラグ用。設定ファイルを読み込み、解決済みの
// log / db / port 等のパスを標準出力にそのまま表示する（サーバーは起動しない）。
func showConfig() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	fmt.Printf("version:         %s\n", version)
	fmt.Printf("config_path:     %s\n", config.DefaultPath())
	fmt.Printf("log.path:        %s\n", cfg.Log.Path)
	fmt.Printf("log.level:       %s\n", cfg.Log.Level)
	fmt.Printf("server.db_path:  %s\n", cfg.Server.DBPath)
	fmt.Printf("server.port:     %d\n", cfg.Server.Port)
	fmt.Printf("embedding.model: %s\n", cfg.Embedding.Model)
	return nil
}

// parseLogLevel は log.level 文字列 ("debug"/"info"/"warn"/"error") を
// slog.Level に変換する。config.Validate() が値域を保証済みのため、
// 未知の値は info 扱いにフォールバックする（ここでは fail-fast しない）。
func parseLogLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// setupLogging は cfg.Log.Path に応じて slog の出力先を設定する。
// "stdout"/"stderr" は標準出力・標準エラーにそのまま、それ以外は絶対パスの
// ファイルとして開く（無ければ親ディレクトリごと作成）。
// 戻り値の io.Closer はファイル出力時のみ非 nil（呼び出し側で defer Close）。
func setupLogging(cfg *config.Config) (io.Closer, error) {
	level := parseLogLevel(cfg.Log.Level)
	var w io.Writer
	var closer io.Closer

	switch cfg.Log.Path {
	case "stdout":
		w = os.Stdout
	case "stderr":
		w = os.Stderr
	default:
		if err := os.MkdirAll(filepath.Dir(cfg.Log.Path), 0o755); err != nil {
			return nil, fmt.Errorf("log: ディレクトリを作成できません %q: %w", filepath.Dir(cfg.Log.Path), err)
		}
		f, err := os.OpenFile(cfg.Log.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, fmt.Errorf("log: ログファイルを開けません %q: %w", cfg.Log.Path, err)
		}
		w = f
		closer = f
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})))
	return closer, nil
}

// startupSweepCutoff は起動時スイープで使う cutoff。DES-001 §8.5「起動時であっても猶予期間中の
// 予約は処理しない」という設計要求に従い、marked_at が猶予期間（retentionDays、cfg.Trash.RetentionDays
// と同一のソース。internal/trash.Worker の定期実行と別の値を使うと、設定した猶予期間より短い期間で
// 起動時に物理削除されてしまい ADR-003 §2 の保証が崩れるため、必ず同じ値を呼び出し元から受け取る）を
// 超過した予約のみを対象にする。
func startupSweepCutoff(retentionDays int) time.Time {
	return time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour)
}

// startupSweep は削除予約（pending_deletions）の起動時スイープを同期実行し、
// 結果（processed 件数・エラー件数）をログ出力する（GC-02）。
// 個別エラーは警告ログとして出力し、起動は継続する（GC-04、silent failure 禁止）。
// 起動時 DB 統計の算出より前に呼ぶこと（GC-03: 統計値がスイープ後の状態を反映するため）。
// 戻り値は統合テスト（main_test.go）がスイープ結果を検証するために返す。
//
// ListPendingDeletionsOlderThan + SweepOnePendingDeletion（DES-001 §8.5）を使う形に分割済み。
// 予約 1 件ごとに WithKeyLock(entry.Key, ...) で囲んで処理する（KEY 単位排他は呼び出し元の責務、
// DES-001 §4.3）。retentionDays は呼び出し元（run()）が cfg.Trash.RetentionDays から渡す
// （internal/trash.Worker の定期実行と同一のソースを使うことで、猶予期間の不整合を防ぐ）。
func startupSweep(ctx context.Context, st *store.Store, retentionDays int) (processed, errCount int) {
	entries, err := st.ListPendingDeletionsOlderThan(ctx, startupSweepCutoff(retentionDays))
	if err != nil {
		slog.Warn("起動時スイープ: 削除予約一覧の取得に失敗（次回起動時に再試行）", "error", err)
		return 0, 1
	}

	for _, entry := range entries {
		lockErr := st.WithKeyLock(ctx, entry.Key, func() error {
			return st.SweepOnePendingDeletion(ctx, entry)
		})
		if lockErr != nil {
			slog.Warn("起動時スイープ個別エラー（次回起動時に再試行）",
				"key", entry.Key, "series", entry.Series, "path", entry.Path, "error", lockErr)
			errCount++
			continue
		}
		// 対象・実行日時を個別エントリ単位で記録する（FNC-012/013、internal/trash.Worker と同じ粒度）
		slog.Info("起動時スイープ: 削除予約を処理", "key", entry.Key, "series", entry.Series,
			"path", entry.Path, "marked_at", entry.MarkedAt, "deleted_at", time.Now().UTC().Format(time.RFC3339))
		processed++
	}
	slog.Info("起動時スイープ完了", "processed", processed, "error_count", errCount)
	return processed, errCount
}

func run(ctx context.Context) error {
	// 設定ファイル読み込み（DES-001 §9 CFG-01）
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("起動失敗: %w", err)
	}

	// ログ出力先を config.log.path に設定する（デフォルト ~/.doc-db/doc-db.log）。
	// 従来は呼び出し側シェルのリダイレクト (`doc-db > /tmp/doc-db.log 2>&1 &`) に
	// 依存していたが、サーバー自身が出力先を決定・管理するようにした。
	logCloser, err := setupLogging(cfg)
	if err != nil {
		return fmt.Errorf("起動失敗: %w", err)
	}
	if logCloser != nil {
		defer logCloser.Close()
	}

	// 起動確認用バナー: ログをファイルにリダイレクトしていても、operator が
	// ターミナル起動直後に config / log / db の実配置を必ず確認できるよう、
	// slog とは別に標準出力へ直接表示する。
	fmt.Printf("doc-db v%s 起動\n", version)
	fmt.Printf("  config: %s\n", config.DefaultPath())
	fmt.Printf("  log:    %s\n", cfg.Log.Path)
	fmt.Printf("  db:     %s\n", cfg.Server.DBPath)
	fmt.Printf("  addr:   :%d\n", cfg.Server.Port)

	// API キー（PRE-01 fail-fast）
	apiKey, err := embedder.APIKeyFromEnv()
	if err != nil {
		return fmt.Errorf("起動失敗: %w", err)
	}

	// Store
	st, err := store.New(cfg.Server.DBPath, cfg.Embedding.Dim)
	if err != nil {
		return fmt.Errorf("store 初期化失敗: %w", err)
	}
	defer st.Close()

	// 削除予約の起動時スイープ（GC-02）。統計表示より前に同期実行する（GC-03）。
	// retentionDays は internal/trash.Worker の定期実行と同一の cfg.Trash.RetentionDays を使う
	// （猶予期間のソースを分離すると設定した期間より短く物理削除される安全性の問題が生じるため）。
	startupSweep(ctx, st, cfg.Trash.RetentionDays)

	// 起動時 DB 統計（KEY数・総チャンク数）。取得に失敗しても起動は継続する
	// (統計表示はオペレータ向けの付加情報であり、サーバー機能に必須ではないため)。
	var keyCount, totalChunkCount int
	if keyInfos, statErr := st.ListKeys(ctx); statErr != nil {
		slog.Warn("起動時 DB 統計の取得に失敗しました (KEY一覧)", "error", statErr)
	} else if n, statErr := st.TotalChunkCount(ctx); statErr != nil {
		slog.Warn("起動時 DB 統計の取得に失敗しました (総チャンク数)", "error", statErr)
	} else {
		keyCount, totalChunkCount = len(keyInfos), n
		fmt.Printf("  keys:   %d 件 (総チャンク数: %d)\n", keyCount, totalChunkCount)
	}

	// 各コンポーネント
	emb := embedder.New(embedder.Config{
		APIKey:  apiKey,
		Model:   cfg.Embedding.Model,
		Dim:     cfg.Embedding.Dim,
		Timeout: time.Duration(cfg.Embedding.TimeoutSeconds) * time.Second,
	})
	ch := chunker.New(cfg.Chunker.MaxChunkSize)
	fe := fetcher.New(fetcher.Config{
		TimeoutSecs:  cfg.Fetcher.TimeoutSeconds,
		AllowPrivate: cfg.Fetcher.AllowPrivate,
	})

	// LLM Reranker（DES-001 §6.4）。API エラー時は search.Pipeline 側で RRF にフォールバック（RR-02）
	rr := reranker.New(reranker.Config{
		APIKey:  apiKey,
		Model:   cfg.Rerank.Model,
		Timeout: time.Duration(cfg.Rerank.TimeoutSeconds) * time.Second,
	})

	// Search Pipeline
	pipeline := search.New(
		st,
		&docdbmcp.SearchEmbedderAdapter{Inner: emb},
		rr,
		search.Config{
			K1:           cfg.BM25.K1,
			B:            cfg.BM25.B,
			RerankFactor: cfg.Rerank.Factor,
		},
	)

	// ゴミ箱最終処分ワーカー起動（DES-001 §3.1/§8。旧 internal/expiry の TTL/LRU ワーカーを置換）
	trashWorker := trash.New(st, trash.Config{
		IntervalSeconds: cfg.Trash.IntervalSeconds,
		RetentionDays:   cfg.Trash.RetentionDays,
	})
	go trashWorker.Start(ctx)

	// MCP サーバー初期化 + ツール登録
	mcpServer := mcpsdk.NewServer(&mcpsdk.Implementation{
		Name:    "doc-db",
		Version: version,
	}, nil)
	handlers := docdbmcp.New(ctx, st, ch, emb, fe, pipeline, cfg.Trash.RetentionDays)
	handlers.Register(mcpServer)

	// Streamable HTTP transport（NFR-03 / PRE-02）
	handler := mcpsdk.NewStreamableHTTPHandler(
		func(_ *http.Request) *mcpsdk.Server { return mcpServer },
		nil,
	)
	addr := fmt.Sprintf(":%d", cfg.Server.Port)
	httpServer := &http.Server{
		Addr:    addr,
		Handler: handler,
	}

	// graceful shutdown
	errCh := make(chan error, 1)
	go func() {
		slog.Info("doc-db MCP サーバー起動",
			"addr", addr,
			"config_path", config.DefaultPath(),
			"log_path", cfg.Log.Path,
			"db_path", cfg.Server.DBPath,
			"embedding_model", cfg.Embedding.Model,
			"key_count", keyCount,
			"total_chunks", totalChunkCount,
			"version", version)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		slog.Info("シャットダウン開始")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("HTTP shutdown: %w", err)
		}
		// goroutine の終了を待つ
		if err := <-errCh; err != nil {
			return err
		}
		slog.Info("シャットダウン完了")
		return nil
	case err := <-errCh:
		return err
	}
}
