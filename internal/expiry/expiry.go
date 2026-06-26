// package expiry は TTL/LRU 廃棄ポリシーを管理するワーカーを担う（internal/store に依存）。
// DES-001 §8: バックグラウンドゴルーチンとして定期実行（デフォルト 1 時間ごと）。
package expiry

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/BlueEventHorizon/doc-db-mcp-server/internal/store"
)

// Config は ExpiryWorker の設定。
type Config struct {
	// IntervalSecs は廃棄チェック間隔（秒）。
	// 環境変数 DOCDB_EXPIRY_INTERVAL で上書き可能（デフォルト: 3600）。
	IntervalSecs int

	// TTLDays は最終アクセスからの廃棄日数（デフォルト: 30）。
	// 環境変数 DOCDB_TTL_DAYS で上書き可能。
	TTLDays int

	// MaxChunks はシステム全体のチャンク上限（デフォルト: 10000）。
	// 環境変数 DOCDB_MAX_CHUNKS で上書き可能。
	MaxChunks int
}

// storeForExpiry は expiry が必要とする store メソッドのサブセット。
// テスト時にモック実装で差し替え可能にするためにインターフェースとして定義する。
type storeForExpiry interface {
	DeleteKey(ctx context.Context, key string) error
}

// Worker は TTL/LRU 廃棄ワーカー。
type Worker struct {
	st   storeForExpiry
	rawDB interface{ QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) }
	cfg  Config
}

// storeWithDB は store.Store の QueryContext を expiry から使えるようにするための型アサーション用インターフェース。
type storeWithDB interface {
	storeForExpiry
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// New は Config を使って Worker を生成する。
func New(st *store.Store, cfg Config) *Worker {
	if cfg.IntervalSecs <= 0 {
		cfg.IntervalSecs = 3600
	}
	if cfg.TTLDays <= 0 {
		cfg.TTLDays = 30
	}
	if cfg.MaxChunks <= 0 {
		cfg.MaxChunks = 10000
	}
	return &Worker{st: st, cfg: cfg}
}

// Start はバックグラウンドゴルーチンとして廃棄チェックを定期実行する。
// ctx がキャンセルされると終了する（DES-001 §8）。
// エラーはログ出力して継続する（サーバー停止はしない: DES-001 §10）。
func (w *Worker) Start(ctx context.Context) {
	interval := time.Duration(w.cfg.IntervalSecs) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	slog.Info("expiry: ワーカー起動", "interval", interval, "ttl_days", w.cfg.TTLDays, "max_chunks", w.cfg.MaxChunks)

	for {
		select {
		case <-ctx.Done():
			slog.Info("expiry: ワーカー停止")
			return
		case <-ticker.C:
			if err := w.runOnce(ctx); err != nil {
				// エラーはログ出力して次回チェックまで継続（DES-001 §10 ExpiryWorker 方針）
				slog.Error("expiry: チェック失敗", "error", err)
			}
		}
	}
}

// runOnce は TTL チェックと LRU チェックを 1 回実行する。
func (w *Worker) runOnce(ctx context.Context) error {
	if err := w.runTTL(ctx); err != nil {
		return fmt.Errorf("expiry: TTL チェック失敗: %w", err)
	}
	if err := w.runLRU(ctx); err != nil {
		return fmt.Errorf("expiry: LRU チェック失敗: %w", err)
	}
	return nil
}

// runTTL は最終アクセスが TTLDays 日以上前の KEY を削除する（DES-001 §8.1 EXP-01）。
// store.Store が TTL クエリ用の公開メソッドを持っていないため、
// TODO: store パッケージに ListExpiredKeysByTTL メソッドが追加されたら置き換える。
func (w *Worker) runTTL(ctx context.Context) error {
	// store パッケージに TTL クエリの公開メソッドが未実装のため骨格のみ。
	// 実装予定:
	//   keys, err := w.st.ListExpiredKeysByTTL(ctx, w.cfg.TTLDays)
	//   for _, key := range keys {
	//       if err := w.st.DeleteKey(ctx, key); err != nil {
	//           slog.Error("expiry: TTL 削除失敗", "key", key, "error", err)
	//       }
	//   }
	slog.Debug("expiry: TTL チェック実行（store に ListExpiredKeysByTTL 未実装のためスキップ）")
	return nil
}

// runLRU はシステム全体のチャンク数が MaxChunks を超えた場合、
// 最終アクセスが最も古い KEY から削除する（DES-001 §8.2 EXP-02）。
// TODO: store パッケージに ListKeysByLRU / TotalChunkCount メソッドが追加されたら置き換える。
func (w *Worker) runLRU(ctx context.Context) error {
	// store パッケージに LRU クエリの公開メソッドが未実装のため骨格のみ。
	// 実装予定:
	//   total, err := w.st.TotalChunkCount(ctx)
	//   if total <= w.cfg.MaxChunks { return nil }
	//   keys, err := w.st.ListKeysByLRU(ctx)
	//   for _, key := range keys {
	//       if err := w.st.DeleteKey(ctx, key); err != nil { ... }
	//       total -= key.ChunkCount
	//       if total <= w.cfg.MaxChunks { break }
	//   }
	slog.Debug("expiry: LRU チェック実行（store に TotalChunkCount/ListKeysByLRU 未実装のためスキップ）")
	return nil
}
