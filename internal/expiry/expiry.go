// package expiry は TTL/LRU 廃棄ポリシーを管理するワーカーを担う（internal/store に依存）。
// DES-001 §8: バックグラウンドゴルーチンとして定期実行（デフォルト 1 時間ごと）。
package expiry

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
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
	ListExpiredKeysByTTL(ctx context.Context, defaultTTLDays int) ([]string, error)
	TotalChunkCount(ctx context.Context) (int, error)
	ListKeysByLRU(ctx context.Context) ([]store.KeyLRUInfo, error)
	DeleteKey(ctx context.Context, key string) error
}

// KeyDeleteError は個別 KEY の削除失敗を記録する（silent failure 禁止方針）。
type KeyDeleteError struct {
	Key       string
	Phase     string // "ttl" | "lru"
	Err       string
	OccurAtRF string // RFC3339 タイムスタンプ
}

// Stats はワーカーの稼働状態スナップショット。caller (cmd/docdb の health check 等) から
// 観測できるよう公開する。silent failure 禁止方針 (memory: no-silent-failure) に従い、
// 個別失敗をログのみで終わらせずプログラム的に取得可能にする。
type Stats struct {
	// LastRunErr は最後の runOnce が返したエラー（nil なら正常）。
	// runOnce 内のループでは個別 KEY 失敗は LastKeyErrors に記録され、
	// runOnce 自体は nil を返すため、本フィールドは TTL/LRU リスト取得失敗等の
	// "ループに入る前" の致命的エラーのみ。
	LastRunErr string
	// LastKeyErrors は直近の runOnce で発生した個別 KEY 削除失敗のリスト。
	// 次の runOnce 開始時にクリアされる。
	LastKeyErrors []KeyDeleteError
	// TotalRuns は Start 起動後の runOnce 累積実行回数。
	TotalRuns int
	// LastRunAtRF は最後の runOnce 完了時刻（RFC3339）。
	LastRunAtRF string
}

// Worker は TTL/LRU 廃棄ワーカー。
type Worker struct {
	st  storeForExpiry
	cfg Config

	mu    sync.Mutex
	stats Stats
}

// New は Config を使って Worker を生成する。
func New(st storeForExpiry, cfg Config) *Worker {
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

// Stats はワーカー稼働状態のスナップショットを返す。
// 個別 KEY 失敗を含む全エラー情報が観測可能（silent failure 禁止）。
func (w *Worker) Stats() Stats {
	w.mu.Lock()
	defer w.mu.Unlock()
	cp := w.stats
	if cp.LastKeyErrors != nil {
		cp.LastKeyErrors = append([]KeyDeleteError(nil), cp.LastKeyErrors...)
	}
	return cp
}

func (w *Worker) recordKeyError(phase, key string, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stats.LastKeyErrors = append(w.stats.LastKeyErrors, KeyDeleteError{
		Key:       key,
		Phase:     phase,
		Err:       err.Error(),
		OccurAtRF: time.Now().UTC().Format(time.RFC3339),
	})
}

func (w *Worker) resetKeyErrors() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stats.LastKeyErrors = nil
}

func (w *Worker) markRun(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stats.TotalRuns++
	w.stats.LastRunAtRF = time.Now().UTC().Format(time.RFC3339)
	if err != nil {
		w.stats.LastRunErr = err.Error()
	} else {
		w.stats.LastRunErr = ""
	}
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
			err := w.runOnce(ctx)
			w.markRun(err)
			if err != nil {
				slog.Error("expiry: チェック失敗", "error", err)
			}
		}
	}
}

// runOnce は TTL チェックと LRU チェックを 1 回実行する。
// 個別 KEY 失敗は Stats.LastKeyErrors に記録され、本関数の戻り値には含まれない
// （TTL/LRU 全体の継続性のため）。caller は Worker.Stats() で詳細を観測できる。
func (w *Worker) runOnce(ctx context.Context) error {
	w.resetKeyErrors()
	if err := w.runTTL(ctx); err != nil {
		return fmt.Errorf("expiry: TTL チェック失敗: %w", err)
	}
	if err := w.runLRU(ctx); err != nil {
		return fmt.Errorf("expiry: LRU チェック失敗: %w", err)
	}
	return nil
}

// runTTL は最終アクセスが TTLDays 日以上前の KEY を削除する（DES-001 §8.1 EXP-01）。
// keys.expiry_policy.ttl_days が設定されている KEY はその値を優先する（§8.4）。
func (w *Worker) runTTL(ctx context.Context) error {
	keys, err := w.st.ListExpiredKeysByTTL(ctx, w.cfg.TTLDays)
	if err != nil {
		return fmt.Errorf("list expired keys: %w", err)
	}
	if len(keys) == 0 {
		slog.Debug("expiry: TTL — 対象 KEY なし")
		return nil
	}

	for _, key := range keys {
		if err := w.st.DeleteKey(ctx, key); err != nil {
			// 個別の削除失敗はログ + Stats に記録して継続（silent failure 禁止）
			slog.Error("expiry: TTL 削除失敗", "key", key, "error", err)
			w.recordKeyError("ttl", key, err)
			continue
		}
		slog.Info("expiry: TTL で KEY を削除", "key", key)
	}
	return nil
}

// runLRU はシステム全体のチャンク数が MaxChunks を超えた場合、
// 最終アクセスが最も古い KEY から削除する（DES-001 §8.2 EXP-02）。
// 上限以下になるまで削除を繰り返す。
func (w *Worker) runLRU(ctx context.Context) error {
	total, err := w.st.TotalChunkCount(ctx)
	if err != nil {
		return fmt.Errorf("total chunk count: %w", err)
	}
	if total <= w.cfg.MaxChunks {
		slog.Debug("expiry: LRU — 上限以下", "total", total, "max", w.cfg.MaxChunks)
		return nil
	}

	keys, err := w.st.ListKeysByLRU(ctx)
	if err != nil {
		return fmt.Errorf("list keys by LRU: %w", err)
	}

	slog.Info("expiry: LRU 削除開始", "total_chunks", total, "max_chunks", w.cfg.MaxChunks, "candidates", len(keys))

	for _, info := range keys {
		if total <= w.cfg.MaxChunks {
			break
		}
		if err := w.st.DeleteKey(ctx, info.Key); err != nil {
			// 個別の削除失敗はログ + Stats に記録して継続（silent failure 禁止）
			slog.Error("expiry: LRU 削除失敗", "key", info.Key, "error", err)
			w.recordKeyError("lru", info.Key, err)
			continue
		}
		total -= info.ChunkCount
		slog.Info("expiry: LRU で KEY を削除", "key", info.Key, "chunks", info.ChunkCount, "remaining_total", total)
	}
	return nil
}
