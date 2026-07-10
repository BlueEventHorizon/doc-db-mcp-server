// package trash は KEY 単位ゴミ箱（keys.trashed_at）および orphan record 削除予約
// （pending_deletions）の自動最終処分ワーカーを担う（internal/store に依存）。
// DES-001 §3.1/§8（§5.6 UC-5/UC-6 相当）: internal/expiry の TTL/LRU 自動削除ワーカーを置き換える。
//
// internal/store は「削除すべきかどうか」の判定ロジックを持たない（ADR-003）。
// 保持期間（retention）を用いた超過判定は本パッケージ（呼び出し元）の責務であり、
// Store 層のメソッド（ListTrashedKeys / IsTrashed 等）は trashed_at という事実のみを返す。
package trash

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/BlueEventHorizon/doc-db-mcp-server/internal/store"
)

// Config は trash.Worker の設定。
// 値の注入元（internal/config.TrashConfig からの配線）は TASK-005 のスコープであり、
// 本パッケージは retention/interval を Config フィールドとして受け取れる形にするに留める。
type Config struct {
	// IntervalSeconds は最終処分チェック間隔（秒）。
	// doc-db.yaml の trash.interval_seconds に対応（デフォルト: 3600）。
	IntervalSeconds int

	// RetentionDays はゴミ箱投入（trashed_at）・削除予約（marked_at）からの保持日数。
	// doc-db.yaml の trash.retention_days に対応（デフォルト: 3、DES-001 §9.2）。
	RetentionDays int
}

// storeForTrash は trash.Worker が必要とする store メソッドのサブセット。
// テスト時にモック実装で差し替え可能にするためにインターフェースとして定義する。
type storeForTrash interface {
	// ListTrashedKeys は現在ゴミ箱に入っている KEY 一覧を返す（trashed_at という事実のみ）。
	// 保持期間超過かどうかの判定は呼び出し元（Worker）が行う。
	ListTrashedKeys(ctx context.Context) ([]store.TrashedKeyInfo, error)
	// IsTrashed は key が現在ゴミ箱状態かどうかを返す（trashed_at という事実のみ）。
	// sweepTrashedKeys が WithKeyLock 内で DeleteKey 直前に再確認する TOCTOU 対策に使う
	// （ADR-003 §2「猶予期間中いつでも復活できる」保証。RestoreKey は WithKeyLock を
	// 内部で取得しない設計のため、ロック取得前に完了した復活操作を上書きしないための再検証）。
	IsTrashed(ctx context.Context, key string) (bool, error)
	// ListPendingDeletionsOlderThan は marked_at が cutoff より前の削除予約一覧を返す
	// 読み取り専用メソッド（cutoff 絞り込みは呼び出し元が算出して渡す）。
	ListPendingDeletionsOlderThan(ctx context.Context, cutoff time.Time) ([]store.PendingDeletionEntry, error)
	// SweepOnePendingDeletion は 1 件（1 KEY 分）の削除予約を物理削除し、予約を解除する。
	SweepOnePendingDeletion(ctx context.Context, entry store.PendingDeletionEntry) error
	// DeleteKey は指定 KEY のすべてのデータを削除する。
	DeleteKey(ctx context.Context, key string) error
	// WithKeyLock は KEY 単位排他ロック（DES-001 §4.3 SYN-08）。
	// DeleteKey / SweepOnePendingDeletion 自身はロックを取得しない規約のため、
	// 呼び出し元（本ワーカー）がこれらの呼び出し一式をこのロックで囲む。
	WithKeyLock(ctx context.Context, key string, fn func() error) error
}

// KeyDeleteError は個別 KEY・record の削除失敗を記録する（silent failure 禁止方針）。
type KeyDeleteError struct {
	Key       string
	Phase     string // "trashed_key" | "pending_deletion"
	Err       string
	OccurAtRF string // RFC3339 タイムスタンプ
}

// Stats はワーカーの稼働状態スナップショット。caller から観測できるよう公開する。
// silent failure 禁止方針に従い、個別失敗をログのみで終わらせずプログラム的に取得可能にする。
type Stats struct {
	// LastRunErr は最後の runOnce が返したエラー（nil なら正常）。
	// runOnce 内のループでは個別 KEY・record 失敗は LastKeyErrors に記録され、
	// runOnce 自体は nil を返すため、本フィールドは一覧取得失敗等の
	// "ループに入る前" の致命的エラーのみ。
	LastRunErr string
	// LastKeyErrors は直近の runOnce で発生した個別 KEY・record 削除失敗のリスト。
	// 次の runOnce 開始時にクリアされる。
	LastKeyErrors []KeyDeleteError
	// TotalRuns は Start 起動後の runOnce 累積実行回数。
	TotalRuns int
	// LastRunAtRF は最後の runOnce 完了時刻（RFC3339）。
	LastRunAtRF string
}

// Worker は KEY ゴミ箱・orphan record 削除予約の自動最終処分ワーカー（DES-001 §8）。
type Worker struct {
	st  storeForTrash
	cfg Config

	mu    sync.Mutex
	stats Stats

	// done は Start の goroutine が終了すると close される（レビュー指摘対応）。
	// 呼び出し元（cmd/docdb）は Done() でこれを待ってから Store.Close() すること。
	// runOnce 実行中に ctx がキャンセルされても、Start の select は runOnce の
	// 完了まで ctx.Done() を評価できない。Done() を待たずに Store を閉じると、
	// 実行中の DB 操作とクローズが競合しうる。
	done chan struct{}
}

// New は Config を使って Worker を生成する。
func New(st storeForTrash, cfg Config) *Worker {
	if cfg.IntervalSeconds <= 0 {
		cfg.IntervalSeconds = 3600
	}
	if cfg.RetentionDays <= 0 {
		cfg.RetentionDays = 3
	}
	return &Worker{st: st, cfg: cfg, done: make(chan struct{})}
}

// Done は Start の goroutine が終了すると close される channel を返す。
// シャットダウン時、呼び出し元はこれを待ってから Store.Close() を呼ぶことで、
// 実行中の最終処分処理と DB クローズの競合を防ぐ。
func (w *Worker) Done() <-chan struct{} {
	return w.done
}

// Stats はワーカー稼働状態のスナップショットを返す。
// 個別 KEY・record 失敗を含む全エラー情報が観測可能（silent failure 禁止）。
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

// Start はバックグラウンドゴルーチンとして最終処分チェックを定期実行する。
// ctx がキャンセルされると終了する（DES-001 §5.6 UC-5/UC-6 は定期実行前提）。
// エラーはログ出力して継続する（サーバー停止はしない: DES-001 §10 と同方針）。
//
// cmd/docdb/main.go への起動配線（go trashWorker.Start(ctx) 等）は TASK-005 のスコープ。
// 終了時は必ず done channel を close する（defer）。呼び出し元は Done() で待機できる。
func (w *Worker) Start(ctx context.Context) {
	defer close(w.done)

	interval := time.Duration(w.cfg.IntervalSeconds) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	slog.Info("trash: ワーカー起動", "interval", interval, "retention_days", w.cfg.RetentionDays)

	for {
		select {
		case <-ctx.Done():
			slog.Info("trash: ワーカー停止")
			return
		case <-ticker.C:
			err := w.runOnce(ctx)
			w.markRun(err)
			if err != nil {
				slog.Error("trash: チェック失敗", "error", err)
			}
		}
	}
}

// retentionCutoff は現在時刻から RetentionDays 日分遡った時刻を返す。
// trashed_at / marked_at がこの時刻より前（＝より古い）であれば保持期間超過とみなす。
func (w *Worker) retentionCutoff() time.Time {
	return time.Now().Add(-time.Duration(w.cfg.RetentionDays) * 24 * time.Hour)
}

// runOnce は KEY ゴミ箱の最終処分チェックと orphan record 削除予約の最終処分チェックを
// 1 回実行する（DES-001 §5.6 UC-5/UC-6）。
// 個別 KEY・record 失敗は Stats.LastKeyErrors に記録され、本関数の戻り値には含まれない
// （両チェックの継続性のため）。caller は Worker.Stats() で詳細を観測できる。
func (w *Worker) runOnce(ctx context.Context) error {
	w.resetKeyErrors()
	if err := w.sweepTrashedKeys(ctx); err != nil {
		return fmt.Errorf("trash: ゴミ箱 KEY の最終処分チェック失敗: %w", err)
	}
	if err := w.sweepPendingDeletions(ctx); err != nil {
		return fmt.Errorf("trash: 削除予約の最終処分チェック失敗: %w", err)
	}
	return nil
}

// sweepTrashedKeys は trashed_at が保持期間を超過した KEY を検出し、KEY 単位排他ロック内で
// DeleteKey により最終処分する（DES-001 §5.6 UC-5）。
// 保持期間超過の判定（trashed_at と RetentionDays からの計算）は本メソッドの責務であり、
// internal/store 側は判定ロジックを持たない（ADR-003）。
func (w *Worker) sweepTrashedKeys(ctx context.Context) error {
	keys, err := w.st.ListTrashedKeys(ctx)
	if err != nil {
		return fmt.Errorf("list trashed keys: %w", err)
	}
	if len(keys) == 0 {
		slog.Debug("trash: ゴミ箱 KEY — 対象なし")
		return nil
	}

	cutoff := w.retentionCutoff()
	for _, info := range keys {
		trashedAt, err := time.Parse(time.RFC3339, info.TrashedAt)
		if err != nil {
			// trashed_at が不正な形式の場合も silent failure にせず記録して継続する
			slog.Error("trash: trashed_at のパース失敗", "key", info.Key, "trashed_at", info.TrashedAt, "error", err)
			w.recordKeyError("trashed_key", info.Key, err)
			continue
		}
		if !trashedAt.Before(cutoff) {
			// 保持期間内（未超過）: 触れない
			continue
		}

		// KEY 単位排他ロック内で削除する（DES-001 §4.3 SYN-08）。
		// DeleteKey 直前に IsTrashed を再確認し、ListTrashedKeys のスナップショット取得後に
		// ユーザーが復活操作（RestoreKey、trashed_at=NULL）を完了させていた場合はスキップする
		// （TOCTOU 対策、ADR-003 §2「猶予期間中いつでも復活できる」保証）。
		restoredBeforeDelete := false
		err = w.st.WithKeyLock(ctx, info.Key, func() error {
			trashed, isTrashedErr := w.st.IsTrashed(ctx, info.Key)
			if isTrashedErr != nil {
				return fmt.Errorf("is trashed check: %w", isTrashedErr)
			}
			if !trashed {
				restoredBeforeDelete = true
				return nil
			}
			return w.st.DeleteKey(ctx, info.Key)
		})
		if err != nil {
			// 個別の削除失敗（ロック取得失敗含む）はログ + Stats に記録して継続（silent failure 禁止）
			slog.Error("trash: ゴミ箱 KEY 最終処分失敗", "key", info.Key, "error", err)
			w.recordKeyError("trashed_key", info.Key, err)
			continue
		}
		if restoredBeforeDelete {
			slog.Info("trash: KEY は最終処分前に復活済みのためスキップ", "key", info.Key)
			continue
		}
		slog.Info("trash: KEY を最終処分", "key", info.Key, "trashed_at", info.TrashedAt,
			"deleted_at", time.Now().UTC().Format(time.RFC3339))
	}
	return nil
}

// sweepPendingDeletions は marked_at が保持期間を超過した削除予約（series 全体予約 /
// orphan record 予約）を検出し、予約 1 件ごとに KEY 単位排他ロック内で SweepOnePendingDeletion
// により最終処分する（DES-001 §5.6 UC-6、cmd/docdb/main.go の startupSweep とほぼ同じパターン）。
//
// ゴミ箱投入中の KEY への削除予約はスキップする（レビュー指摘対応）:
// pending_deletions の marked_at は KEY の trashed_at と無関係に進行するため、対策なしでは
// 「schedule_delete_series 済みの series を持つ KEY を trash_index し、KEY の保持期間内に
// restore_index しても、古い series 全体予約が独立して sweep されて series データが消える」
// という、ADR-003 の「猶予期間中いつでも復活できる」という保証に反する事故が起き得る。
// KEY がゴミ箱状態の間は該当 KEY の予約処理を先送りし、KEY 自身の最終処分（DeleteKey が
// 同一トランザクションで当該 KEY の全予約行を除去する。§4.5 [MANDATORY]）または復活後の
// 通常のスイープに委ねる。
func (w *Worker) sweepPendingDeletions(ctx context.Context) error {
	entries, err := w.st.ListPendingDeletionsOlderThan(ctx, w.retentionCutoff())
	if err != nil {
		return fmt.Errorf("list pending deletions: %w", err)
	}
	if len(entries) == 0 {
		slog.Debug("trash: 削除予約 — 対象なし")
		return nil
	}

	for _, entry := range entries {
		skippedTrashed := false
		err := w.st.WithKeyLock(ctx, entry.Key, func() error {
			trashed, isTrashedErr := w.st.IsTrashed(ctx, entry.Key)
			if isTrashedErr != nil {
				return fmt.Errorf("is trashed check: %w", isTrashedErr)
			}
			if trashed {
				skippedTrashed = true
				return nil
			}
			return w.st.SweepOnePendingDeletion(ctx, entry)
		})
		if err != nil {
			// 個別の削除失敗（ロック取得失敗含む）はログ + Stats に記録して継続（silent failure 禁止）
			slog.Error("trash: 削除予約の最終処分失敗", "key", entry.Key, "series", entry.Series, "path", entry.Path, "error", err)
			w.recordKeyError("pending_deletion", entry.Key, err)
			continue
		}
		if skippedTrashed {
			slog.Info("trash: KEY がゴミ箱投入中のため削除予約の処理を先送り", "key", entry.Key,
				"series", entry.Series, "path", entry.Path, "marked_at", entry.MarkedAt)
			continue
		}
		slog.Info("trash: orphan record / series 削除予約を最終処分", "key", entry.Key, "series", entry.Series,
			"path", entry.Path, "marked_at", entry.MarkedAt, "deleted_at", time.Now().UTC().Format(time.RFC3339))
	}
	return nil
}
