// doc-db MCP サーバーのエントリポイント。
// 設定読み込み・依存初期化・MCP HTTP サーバー起動を行う（DES-001 §3.1）。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		slog.Error("サーバー終了", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	// 設定ファイル読み込み（DES-001 §9 CFG-01）
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("起動失敗: %w", err)
	}

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
			"db_path", cfg.Server.DBPath,
			"embedding_model", cfg.Embedding.Model,
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
