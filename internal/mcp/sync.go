// sync.go は sync_documents（desired-state 同期ジョブ）の MCP ツールハンドラと
// ジョブ状態管理を実装する。DES-001 §4.3 / §5.4、APP-001 FNC-006 SYN-01〜08 / GC-05 に対応。
//
// 処理の骨格:
//   - handleSyncDocuments は job_id を即座に返し（SYN-05）、実処理は rootCtx 由来の
//     独立 context で goroutine 実行する（GC-05。MCP リクエスト context には依存しない）
//   - goroutine 内では store.WithKeyLock(ctx, key, fn) を 1 回だけ呼び、fn 内で
//     desired-state 判定全体（documents 処理 → 既存 path 一覧取得 → series 切り離し →
//     削除予約の記録・解除）を直接実行する（SYN-08、DES-001 §4.3。fn 内で
//     WithKeyLock を再度呼ばない）
package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// -----------------------------------------------------------------------
// ジョブ状態管理（DES-001 §5.4、SYN-06/07）
// -----------------------------------------------------------------------

// SyncJobStatus は sync_documents ジョブ 1 件の進捗状態。
// Handlers.syncJobsMu で保護され、メモリ上にのみ保持される（SYN-07、永続化しない）。
// get_sync_status（TASK-011）がこの内容を読んで返す。
type SyncJobStatus struct {
	Status             string // "running" | "done" | "failed"
	Processed          int    // 新規 / 内容変更で処理されたドキュメント数
	Skipped            int    // DIF-02 で embedding 再利用スキップされたドキュメント数
	Failed             int    // 完全に失敗したドキュメント数
	DeletedPathsMarked int    // 物理削除予約（MarkDocumentForDeletion）を記録した path 数
	Errors             []string

	// createdAt はジョブ登録時刻。完了済みジョブの保持上限超過時の
	// 追い出し順（古い順）の判定に使う（TBD-101）。
	createdAt time.Time
}

// maxCompletedSyncJobs は完了済み（done / failed）ジョブの保持上限。
// APP-001 TBD-101（保持ポリシー: 件数上限 or 経過時間）に対し、件数上限方式を採用する。
// 根拠: ジョブ投入はクライアント主導のポーリング前提（SYN-06）であり、完了直後に
// get_sync_status で読まれる運用が通常。時間ベースはタイマー管理が必要になる一方、
// 件数上限は新規ジョブ登録時の同期的な追い出しだけで済み、メモリ上限も確定的になる。
// running のジョブは追い出し対象にしない。
const maxCompletedSyncJobs = 100

// registerSyncJob はジョブを "running" で登録し、完了済みジョブが保持上限を
// 超えていれば古い順に追い出す。
func (h *Handlers) registerSyncJob(jobID string) {
	h.syncJobsMu.Lock()
	defer h.syncJobsMu.Unlock()
	if h.syncJobs == nil {
		h.syncJobs = make(map[string]*SyncJobStatus)
	}

	// 完了済みジョブの追い出し（TBD-101: 件数上限方式）
	type completed struct {
		id string
		at time.Time
	}
	var done []completed
	for id, st := range h.syncJobs {
		if st.Status != "running" {
			done = append(done, completed{id: id, at: st.createdAt})
		}
	}
	for len(done) >= maxCompletedSyncJobs {
		oldest := 0
		for i := range done {
			if done[i].at.Before(done[oldest].at) {
				oldest = i
			}
		}
		delete(h.syncJobs, done[oldest].id)
		done = append(done[:oldest], done[oldest+1:]...)
	}

	h.syncJobs[jobID] = &SyncJobStatus{Status: "running", createdAt: time.Now()}
}

// updateSyncJob はジョブ状態を mutex 保護下で更新する。
// 存在しない jobID は no-op（追い出し済みの場合。running 中は追い出されないため通常起きない）。
func (h *Handlers) updateSyncJob(jobID string, fn func(*SyncJobStatus)) {
	h.syncJobsMu.Lock()
	defer h.syncJobsMu.Unlock()
	if st, ok := h.syncJobs[jobID]; ok {
		fn(st)
	}
}

// newSyncJobID は一意な job_id を stdlib のみで生成する（crypto/rand 16 バイト + hex）。
func newSyncJobID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate job_id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// -----------------------------------------------------------------------
// get_sync_status (SYN-06)
// -----------------------------------------------------------------------

// GetSyncStatusInput は get_sync_status の入力。
type GetSyncStatusInput struct {
	JobID string `json:"job_id" jsonschema:"sync_documents が返したジョブ識別子。"`
}

// GetSyncStatusResult は get_sync_status の出力（APP-001 FNC-006 出力仕様）。
type GetSyncStatusResult struct {
	Status             string   `json:"status" jsonschema:"ジョブの状態。'running' = 処理中 / 'done' = 完了 / 'failed' = 失敗 (シャットダウン中断含む)。"`
	Processed          int      `json:"processed" jsonschema:"新規 / 内容変更で処理されたドキュメント数。"`
	Skipped            int      `json:"skipped" jsonschema:"同一ハッシュ既存で embedding 再利用スキップされたドキュメント数 (DIF-02)。"`
	Failed             int      `json:"failed" jsonschema:"完全に失敗したドキュメント数。"`
	DeletedPathsMarked int      `json:"deleted_paths_marked" jsonschema:"desired-state から欠落し、物理削除予約が記録された path 数。"`
	Errors             []string `json:"errors,omitempty" jsonschema:"個別ドキュメントの失敗やジョブ中断の詳細。空なら全件正常。"`
}

// handleGetSyncStatus は job_id からジョブの進捗・完了有無を返す（SYN-06）。
// 読み取りのみのため WithKeyLock は不要で、syncJobsMu の保護だけで足りる。
// 存在しない job_id（未発行・保持上限超過で追い出し済み・サーバー再起動で
// 消失 = SYN-07 のいずれか）にはエラーを返す。保持ポリシーは件数上限方式
// （maxCompletedSyncJobs = 100、TBD-101 の解決値。根拠は定数コメント参照）で、
// 追い出しは registerSyncJob が新規ジョブ登録時に行う。
func (h *Handlers) handleGetSyncStatus(
	_ context.Context, _ *mcpsdk.CallToolRequest, in GetSyncStatusInput,
) (res *mcpsdk.CallToolResult, out GetSyncStatusResult, err error) {
	if in.JobID == "" {
		return nil, GetSyncStatusResult{}, errors.New("job_id は必須")
	}

	start := time.Now()
	defer func() {
		logHandlerDone("get_sync_status done", err, start, "job_id", in.JobID, "status", out.Status)
	}()

	h.syncJobsMu.Lock()
	defer h.syncJobsMu.Unlock()

	st, ok := h.syncJobs[in.JobID]
	if !ok {
		return nil, GetSyncStatusResult{}, fmt.Errorf(
			"job_id %q が見つかりません（未発行・保持上限超過・サーバー再起動のいずれか。再度 sync_documents を呼べば冪等に補われる）", in.JobID)
	}

	// Errors はジョブ goroutine が updateSyncJob 経由で append し続けるため、
	// mutex 保護の外へ内部スライスを漏らさないようコピーして返す。
	var errsCopy []string
	if len(st.Errors) > 0 {
		errsCopy = make([]string, len(st.Errors))
		copy(errsCopy, st.Errors)
	}

	return nil, GetSyncStatusResult{
		Status:             st.Status,
		Processed:          st.Processed,
		Skipped:            st.Skipped,
		Failed:             st.Failed,
		DeletedPathsMarked: st.DeletedPathsMarked,
		Errors:             errsCopy,
	}, nil
}

// -----------------------------------------------------------------------
// sync_documents (SYN-01〜08)
// -----------------------------------------------------------------------

// SyncInput は sync_documents の入力。documents は当該 key・series の完全な現在状態（SYN-01）。
type SyncInput struct {
	Key       string           `json:"key" jsonschema:"対象インデックスの KEY。"`
	Series    string           `json:"series" jsonschema:"対象の時系列キー (Git branch 名等)。"`
	Documents []UpsertDocument `json:"documents" jsonschema:"当該 key・series の完全な現在状態 (desired-state) を表すドキュメントのリスト。要素形式は upsert_documents と同一 (content / url / local_path 排他)。このリストに含まれない既存 path は series から即時切り離される。空リストは『この series に現存ファイルがない』という desired-state として受理され、既存 path が全て切り離される (削除予約は自己修復可能で、同一内容の再 sync で Embedding 再計算なしに復元できる)。"`
}

// SyncResult は sync_documents の出力（SYN-05: job_id の即時返却）。
type SyncResult struct {
	JobID string `json:"job_id" jsonschema:"ジョブ識別子。get_sync_status にこの値を渡して進捗をポーリングする。"`
}

func (h *Handlers) handleSyncDocuments(
	_ context.Context, _ *mcpsdk.CallToolRequest, in SyncInput,
) (res *mcpsdk.CallToolResult, out SyncResult, err error) {
	if in.Key == "" || in.Series == "" {
		return nil, SyncResult{}, errors.New("key と series は必須")
	}
	// documents が空のリストであっても拒否しない: APP-001 FNC-006 SYN-01 は documents を「完全な現在状態」
	// と定義しており、空は「この series に現存ファイルがない」という正当な desired-state。
	// この場合、既存 path は全て切り離し + orphan 予約となり（SYN-03）、誤送信でも同一内容の
	// 再 sync で Embedding 再計算なしに復元できる（SYN-04 の自己修復。即時物理削除する
	// delete_series とはこの点が異なる）。

	jobID, err := newSyncJobID()
	if err != nil {
		return nil, SyncResult{}, err
	}
	h.registerSyncJob(jobID)

	slog.Info("sync_documents accepted",
		"job_id", jobID, "key", in.Key, "series", in.Series, "count", len(in.Documents))

	// GC-05: MCP リクエストの context には依存せず、rootCtx（サーバーシャットダウンで
	// cancel される長寿命 context）から派生した独立 context でジョブを実行する。
	// job_id 返却後にクライアントが切断してもジョブは継続する。
	go h.runSyncJob(jobID, in.Key, in.Series, in.Documents)

	return nil, SyncResult{JobID: jobID}, nil
}

// runSyncJob は sync_documents のバックグラウンド処理本体。
// WithKeyLock を 1 回だけ取得し（SYN-08）、fn 内で desired-state 判定全体を実行する。
func (h *Handlers) runSyncJob(jobID, key, series string, documents []UpsertDocument) {
	ctx, cancel := context.WithCancel(h.rootCtx)
	defer cancel()

	start := time.Now()

	// WithKeyLock はロック取得の待機中に ctx（rootCtx 由来）がキャンセルされると
	// fn を実行せず ctx.Err() を返す（GC-05: 待機中のシャットダウン応答）。
	// fn 実行中のキャンセルは fn 内の各段階で ctx.Err() を監視して中断する。
	lockErr := h.store.WithKeyLock(ctx, key, func() error {
		return h.syncLocked(ctx, jobID, key, series, documents)
	})

	if lockErr != nil {
		// キャンセル（シャットダウン）・ロック待機失敗・fn 内の中断のいずれも
		// ジョブを "failed" にする（GC-05。進行中のまま応答不能になることを避ける）。
		slog.Error("sync_documents job failed",
			"job_id", jobID, "key", key, "series", series,
			"duration_ms", time.Since(start).Milliseconds(), "error", lockErr)
		h.updateSyncJob(jobID, func(st *SyncJobStatus) {
			st.Status = "failed"
			st.Errors = append(st.Errors, lockErr.Error())
		})
		return
	}

	h.updateSyncJob(jobID, func(st *SyncJobStatus) {
		st.Status = "done"
	})
	slog.Info("sync_documents job done",
		"job_id", jobID, "key", key, "series", series,
		"duration_ms", time.Since(start).Milliseconds())
}

// syncLocked は WithKeyLock の fn として実行される desired-state 判定の本体。
// この中では WithKeyLock を再度呼ばない（DES-001 §4.3 の禁止規約）。
// 戻り値が非 nil の場合、呼び出し元（runSyncJob）がジョブを "failed" にする。
func (h *Handlers) syncLocked(ctx context.Context, jobID, key, series string, documents []UpsertDocument) error {
	// fn 冒頭: 当該 key+series の削除予約（path 一覧 + series 全体予約の有無）を 1 回で取得する
	// （DES-001 §4.5 [MANDATORY]。補償 + 予約解除の対象 path と SYN-04 の series 全体予約解除の
	// 要否を判定する）。
	pendingPaths, seriesWide, err := h.store.ListPendingDeletions(ctx, key, series)
	if err != nil {
		return fmt.Errorf("list pending deletions: %w", err)
	}
	pendingSet := make(map[string]bool, len(pendingPaths))
	for _, p := range pendingPaths {
		pendingSet[p] = true
	}

	// 1. documents を既存 upsertOne で処理する（DIF-01〜03 無改造、SYN-02）。
	// succeeded は upsertOne が成功（processed または skipped。部分 embedding 失敗も
	// processed 扱い）した path の集合。SYN-04 の予約解除はこの集合に限定する。
	var result UpsertResult
	succeeded := make(map[string]bool, len(documents))
	for _, doc := range documents {
		// fn 実行中のシャットダウン監視（GC-05）
		if cerr := ctx.Err(); cerr != nil {
			return fmt.Errorf("sync interrupted during document processing: %w", cerr)
		}
		if uerr := h.upsertOne(ctx, key, series, doc, &result); uerr != nil {
			// upsertOne が result.Failed / result.Errors に記録済み。処理は継続する
			// （既存 upsert_documents と同じ部分失敗継続方針）。
			slog.Warn("sync_documents: document failed",
				"job_id", jobID, "path", doc.Path, "error", uerr)
		} else {
			succeeded[doc.Path] = true
		}
		// 進捗をジョブ状態へ反映（ポーリング中のクライアントに途中経過が見える）
		h.updateSyncJob(jobID, func(st *SyncJobStatus) {
			st.Processed = result.Processed
			st.Skipped = result.Skipped
			st.Failed = result.Failed
		})
	}
	// upsertOne が蓄積した個別エラーをジョブ状態へ反映する（silent failure 禁止）
	if len(result.Errors) > 0 {
		h.updateSyncJob(jobID, func(st *SyncJobStatus) {
			for _, e := range result.Errors {
				st.Errors = append(st.Errors, fmt.Sprintf("path %q: %s", e.Path, e.Error))
			}
		})
	}

	// 2. 既存 path 一覧を取得し、documents に無い path を series から即時切り離す（SYN-03）。
	desired := make(map[string]bool, len(documents))
	for _, doc := range documents {
		desired[doc.Path] = true
	}
	existing, err := h.store.ListPaths(ctx, key, series)
	if err != nil {
		return fmt.Errorf("list existing paths: %w", err)
	}
	for _, p := range existing {
		if desired[p] {
			continue
		}
		if cerr := ctx.Err(); cerr != nil {
			return fmt.Errorf("sync interrupted during detach: %w", cerr)
		}
		orphaned, derr := h.store.DetachSeriesFromPath(ctx, key, series, p)
		if derr != nil {
			// 個別失敗は記録して継続する（silent failure 禁止: ログ + ジョブ状態の双方へ）。
			// 切り離せなかった path は次回 sync で再検出されるため回収漏れにはならない。
			slog.Error("sync_documents: detach failed",
				"job_id", jobID, "key", key, "series", series, "path", p, "error", derr)
			h.updateSyncJob(jobID, func(st *SyncJobStatus) {
				st.Errors = append(st.Errors, fmt.Sprintf("detach %q: %v", p, derr))
			})
			continue
		}
		if !orphaned {
			// 他 series が参照する record はその series の下で生き続けるため予約不要
			continue
		}
		if merr := h.store.MarkDocumentForDeletion(ctx, key, series, p); merr != nil {
			// 予約に失敗した orphan は次回 sync の DetachSeriesFromPath（冪等）で再検出される。
			slog.Error("sync_documents: mark for deletion failed",
				"job_id", jobID, "key", key, "series", series, "path", p, "error", merr)
			h.updateSyncJob(jobID, func(st *SyncJobStatus) {
				st.Errors = append(st.Errors, fmt.Sprintf("mark %q: %v", p, merr))
			})
			continue
		}
		h.updateSyncJob(jobID, func(st *SyncJobStatus) {
			st.DeletedPathsMarked++
		})
	}

	// 3. 自己修復（SYN-04）: documents に含まれ upsertOne が成功し、かつ削除予約が存在する
	// path のみ、DeleteOrphanRecords（CleanOtherSeries 個別失敗の補償）→ ClearPendingDeletion
	// の 2 段階で予約を解除する（DES-001 §4.5 [MANDATORY]）。
	// 失敗（failed）した path の予約は保持する（新 record が無く、解除すると旧 orphan の
	// 回収手段が失われるため）。
	for _, p := range pendingPaths {
		if !succeeded[p] {
			continue
		}
		if cerr := ctx.Err(); cerr != nil {
			return fmt.Errorf("sync interrupted during self-repair: %w", cerr)
		}
		if _, oerr := h.store.DeleteOrphanRecords(ctx, key, p); oerr != nil {
			// 手順 1 が失敗した場合は Clear せず予約を保持する（起動時スイープが
			// orphan-only で安全に再試行する）。silent failure 禁止のためログ + 状態へ記録。
			slog.Error("sync_documents: orphan compensation failed (予約を保持)",
				"job_id", jobID, "key", key, "series", series, "path", p, "error", oerr)
			h.updateSyncJob(jobID, func(st *SyncJobStatus) {
				st.Errors = append(st.Errors, fmt.Sprintf("delete orphan %q: %v", p, oerr))
			})
			continue
		}
		if cerr := h.store.ClearPendingDeletion(ctx, key, series, p); cerr != nil {
			// 解除失敗の予約残置は無害（スイープは orphan-only の 0 件処理で冪等）だが記録する。
			slog.Error("sync_documents: clear pending deletion failed",
				"job_id", jobID, "key", key, "series", series, "path", p, "error", cerr)
			h.updateSyncJob(jobID, func(st *SyncJobStatus) {
				st.Errors = append(st.Errors, fmt.Sprintf("clear pending %q: %v", p, cerr))
			})
		}
	}

	// series 全体の削除予約（GC-01 由来）があれば解除する（SYN-04:
	// sync_documents が呼ばれた時点でその series はまだ使われていると判断する）。
	if seriesWide {
		if cerr := h.store.ClearPendingDeletion(ctx, key, series, ""); cerr != nil {
			slog.Error("sync_documents: clear series-wide pending deletion failed",
				"job_id", jobID, "key", key, "series", series, "error", cerr)
			h.updateSyncJob(jobID, func(st *SyncJobStatus) {
				st.Errors = append(st.Errors, fmt.Sprintf("clear series-wide pending: %v", cerr))
			})
		} else {
			slog.Info("sync_documents: series 全体の削除予約を解除（自己修復）",
				"job_id", jobID, "key", key, "series", series)
		}
	}

	return nil
}
