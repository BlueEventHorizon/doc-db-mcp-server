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
	"github.com/BlueEventHorizon/doc-db-mcp-server/internal/expiry"
	"github.com/BlueEventHorizon/doc-db-mcp-server/internal/fetcher"
	docdbmcp "github.com/BlueEventHorizon/doc-db-mcp-server/internal/mcp"
	"github.com/BlueEventHorizon/doc-db-mcp-server/internal/reranker"
	"github.com/BlueEventHorizon/doc-db-mcp-server/internal/search"
	"github.com/BlueEventHorizon/doc-db-mcp-server/internal/store"
)

// version はビルド時に -ldflags "-X main.version=..." で上書きされる（DES-002 §4.2）。
// VERSION ファイルが canonical（APP-002 VER-01）。手元 `go build` でこの値を埋めるには
// Makefile の build target を使うか、以下のワンライナーを実行する:
//   go build -ldflags "-X main.version=$(cat VERSION)" -o doc-db ./cmd/docdb
var version = "dev"

func main() {
	// --version は設定ファイル読み込み・API キー検証・Store/Expiry 初期化より前に処理する（APP-002 VER-03）。
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

	// Expiry ワーカー起動（DES-001 §8）
	expWorker := expiry.New(st, expiry.Config{
		IntervalSecs: cfg.Expiry.IntervalSeconds,
		TTLDays:      cfg.Expiry.TTLDays,
		MaxChunks:    cfg.Expiry.MaxChunks,
	})
	go expWorker.Start(ctx)

	// MCP サーバー初期化 + ツール登録
	mcpServer := mcpsdk.NewServer(&mcpsdk.Implementation{
		Name:    "doc-db",
		Version: version,
	}, nil)
	handlers := docdbmcp.New(st, ch, emb, fe, pipeline)
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
