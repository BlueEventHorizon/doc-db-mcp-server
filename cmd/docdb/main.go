// doc-db MCP サーバーのエントリポイント。
// 設定読み込み・Store 初期化・MCP サーバー起動を行う（DES-001 §3.1）。
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/k2moons/doc-db-mcp-server/internal/embedder"
	"github.com/k2moons/doc-db-mcp-server/internal/expiry"
	"github.com/k2moons/doc-db-mcp-server/internal/store"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		slog.Error("サーバー終了", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	// PRE-01 fail-fast: API キー未設定時は即時終了（DES-001 §10）
	embedCfg, err := embedder.ConfigFromEnv()
	if err != nil {
		return fmt.Errorf("起動失敗: %w", err)
	}

	// Store 初期化（DES-001 §3.1）
	dbPath := envOrDefault("DOCDB_DB_PATH", "./docdb.sqlite")
	storeCfg, err := storeConfig(embedCfg.Dim)
	if err != nil {
		return fmt.Errorf("store 設定エラー: %w", err)
	}
	st, err := store.New(dbPath, storeCfg.expectedDim)
	if err != nil {
		return fmt.Errorf("store 初期化失敗: %w", err)
	}
	defer st.Close()

	// Expiry ワーカー起動（DES-001 §8）
	expiryInterval := envOrDefaultInt("DOCDB_EXPIRY_INTERVAL", 3600)
	ttlDays := envOrDefaultInt("DOCDB_TTL_DAYS", 30)
	maxChunks := envOrDefaultInt("DOCDB_MAX_CHUNKS", 10000)
	expWorker := expiry.New(st, expiry.Config{
		IntervalSecs: expiryInterval,
		TTLDays:      ttlDays,
		MaxChunks:    maxChunks,
	})
	go expWorker.Start(ctx)

	// TODO: MCP サーバーを起動する（internal/mcp に Server 型が実装されたら以下を有効化）
	// port := envOrDefault("DOCDB_PORT", "8080")
	// srv := mcp.NewServer(mcp.Config{
	// 	Port:    port,
	// 	Store:   st,
	// 	Embedder: embedder.New(embedCfg),
	// })
	// return srv.Run(ctx)

	slog.Info("doc-db MCP サーバー起動準備完了。MCP ハンドラは未実装のため待機します")
	<-ctx.Done()
	slog.Info("シャットダウン")
	return nil
}

// storeConfigParams は store.New に渡す設定パラメータ。
type storeConfigParams struct {
	expectedDim int
}

func storeConfig(embedDim int) (storeConfigParams, error) {
	return storeConfigParams{expectedDim: embedDim}, nil
}

// envOrDefault は環境変数を読み、未設定の場合は defaultVal を返す。
func envOrDefault(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

// envOrDefaultInt は環境変数を int として読み、未設定または不正な場合は defaultVal を返す。
func envOrDefaultInt(key string, defaultVal int) int {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		slog.Warn("環境変数が不正なため既定値を使用", "key", key, "value", v, "default", defaultVal)
		return defaultVal
	}
	return n
}
