// doc-db MCP サーバーのエントリポイント。
// 設定読み込み・Store 初期化・MCP サーバー起動を行う（DES-001 §3.1）。
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/k2moons/doc-db-mcp-server/internal/config"
	"github.com/k2moons/doc-db-mcp-server/internal/embedder"
	"github.com/k2moons/doc-db-mcp-server/internal/expiry"
	"github.com/k2moons/doc-db-mcp-server/internal/store"
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

	// API キーのみ環境変数から取得（PRE-01 fail-fast）
	apiKey, err := embedder.APIKeyFromEnv()
	if err != nil {
		return fmt.Errorf("起動失敗: %w", err)
	}

	// Embedder 設定（DES-001 §9.2 embedding セクション）
	_ = embedder.Config{
		APIKey:  apiKey,
		Model:   cfg.Embedding.Model,
		Dim:     cfg.Embedding.Dim,
		Timeout: time.Duration(cfg.Embedding.TimeoutSeconds) * time.Second,
	}

	// Store 初期化（DES-001 §3.1）
	st, err := store.New(cfg.Server.DBPath, cfg.Embedding.Dim)
	if err != nil {
		return fmt.Errorf("store 初期化失敗: %w", err)
	}
	defer st.Close()

	// Expiry ワーカー起動（DES-001 §8）
	expWorker := expiry.New(st, expiry.Config{
		IntervalSecs: cfg.Expiry.IntervalSeconds,
		TTLDays:      cfg.Expiry.TTLDays,
		MaxChunks:    cfg.Expiry.MaxChunks,
	})
	go expWorker.Start(ctx)

	// TODO: MCP サーバーを起動する（internal/mcp に Server 型が実装されたら以下を有効化）
	// srv := mcp.NewServer(mcp.Config{
	// 	Port:     cfg.Server.Port,
	// 	Store:    st,
	// 	Embedder: embedder.New(embedCfg),
	// 	Chunker:  chunker.New(cfg.Chunker.MaxChunkSize),
	// 	Fetcher:  fetcher.New(fetcher.Config{
	// 		TimeoutSecs:  cfg.Fetcher.TimeoutSeconds,
	// 		AllowPrivate: cfg.Fetcher.AllowPrivate,
	// 	}),
	// })
	// return srv.Run(ctx)

	slog.Info("doc-db MCP サーバー起動準備完了。MCP ハンドラは未実装のため待機します",
		"port", cfg.Server.Port,
		"db_path", cfg.Server.DBPath,
		"embedding_model", cfg.Embedding.Model)
	<-ctx.Done()
	slog.Info("シャットダウン")
	return nil
}
