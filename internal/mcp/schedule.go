// schedule.go は schedule_delete_series（series 削除予約）の MCP ツールハンドラを実装する。
// DES-001 §4.3 / §4.5、APP-001 FNC-006 GC-01 に対応。
//
// schedule_delete_series は指定 key・series を即座に削除せず、pending_deletions に
// 削除予約（path=” センチネル行）として記録するだけの軽量な操作である（GC-01）。
// 物理削除はサーバー起動時のスイープ、または internal/trash.Worker の定期実行が
// ListPendingDeletionsOlderThan + SweepOnePendingDeletion（内部で既存 DeleteSeriesAll を
// 使用）で行う。予約は物理削除まで完全に無害で、同一 key・series への sync_documents
// 呼び出しで解除される（SYN-04 の自己修復）。
package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ScheduleDeleteSeriesInput は schedule_delete_series の入力。
type ScheduleDeleteSeriesInput struct {
	Key    string `json:"key" jsonschema:"対象インデックスの KEY。"`
	Series string `json:"series" jsonschema:"削除予約する series 名 (Git branch 名等)。即時削除はされず、起動時スイープまたは internal/trash.Worker の定期実行で物理削除される。"`
}

// ScheduleDeleteSeriesResult は schedule_delete_series の出力（APP-001 FNC-006 出力仕様）。
type ScheduleDeleteSeriesResult struct {
	Key              string `json:"key" jsonschema:"予約対象の KEY (入力の echo back)。"`
	Series           string `json:"series" jsonschema:"予約対象の series (入力の echo back)。"`
	AlreadyScheduled bool   `json:"already_scheduled" jsonschema:"呼び出し時点で既に同一 series の削除予約が存在した場合 true (冪等呼び出しの判別用)。"`
}

// handleScheduleDeleteSeries は指定 key・series の削除予約を記録する（GC-01）。
// 即座に削除しない: MarkSeriesForDeletion で pending_deletions に予約行を upsert するだけで、
// record / chunks / embeddings には一切触れない。物理削除は起動時スイープまたは
// internal/trash.Worker の定期実行（保持期間経過後）が行う。
//
// KEY 単位排他（SYN-08）: sync_documents の desired-state 判定（fn 冒頭の
// ListPendingDeletions で series 全体予約の有無を読む）と直列化するため、
// MarkSeriesForDeletion を WithKeyLock で囲む（DES-001 §4.3）。
// 予約書き込みは軽量なため goroutine は使わず同期的に取得する。
func (h *Handlers) handleScheduleDeleteSeries(
	ctx context.Context, _ *mcpsdk.CallToolRequest, in ScheduleDeleteSeriesInput,
) (res *mcpsdk.CallToolResult, out ScheduleDeleteSeriesResult, err error) {
	if in.Key == "" || in.Series == "" {
		return nil, ScheduleDeleteSeriesResult{}, errors.New("key / series は必須")
	}
	if terr := h.rejectIfTrashed(ctx, in.Key); terr != nil {
		return nil, ScheduleDeleteSeriesResult{}, terr
	}
	start := time.Now()
	slog.Info("schedule_delete_series start", "key", in.Key, "series", in.Series)
	defer func() {
		logHandlerDone("schedule_delete_series done", err, start,
			"key", in.Key, "series", in.Series, "already_scheduled", out.AlreadyScheduled)
	}()

	var alreadyScheduled bool
	if lerr := h.store.WithKeyLock(ctx, in.Key, func() error {
		// ロック取得前の rejectIfTrashed 判定は TOCTOU の余地があるため、ロック内で再判定する。
		if terr := h.rejectIfTrashed(ctx, in.Key); terr != nil {
			return terr
		}
		var merr error
		alreadyScheduled, merr = h.store.MarkSeriesForDeletion(ctx, in.Key, in.Series)
		return merr
	}); lerr != nil {
		err = fmt.Errorf("schedule delete series: %w", lerr)
		return nil, ScheduleDeleteSeriesResult{}, err
	}

	return nil, ScheduleDeleteSeriesResult{
		Key:              in.Key,
		Series:           in.Series,
		AlreadyScheduled: alreadyScheduled,
	}, nil
}
