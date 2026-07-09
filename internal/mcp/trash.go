// trash.go は KEY 単位ゴミ箱操作の MCP ツールハンドラ（trash_index / list_trashed_indexes /
// restore_index）を実装する。DES-001（§4.6/§5.5/§8.1）・FNC-007〜FNC-011 に対応。
//
// 旧 delete_index（即時物理削除）はこれらのツールに置換され廃止された（FNC-007）。
// KEY の削除経路は必ず trash_index（ゴミ箱投入）を経由し、猶予期間なしに即座に物理削除される
// 経路を残さない。
package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// -----------------------------------------------------------------------
// trash_index (FNC-009)
// -----------------------------------------------------------------------

// TrashIndexInput は trash_index の入力。
type TrashIndexInput struct {
	Key string `json:"key" jsonschema:"ゴミ箱に投入する KEY。指定 KEY の実データ (record/chunk/embedding) は物理削除されず保持期間経過後に自動最終処分される。"`
}

// TrashIndexResult は trash_index の出力。
type TrashIndexResult struct {
	Key       string `json:"key" jsonschema:"ゴミ箱に投入された KEY (入力の echo back)。"`
	Trashed   bool   `json:"trashed" jsonschema:"ゴミ箱投入に成功したか。"`
	TrashedAt string `json:"trashed_at" jsonschema:"ゴミ箱投入日時 (RFC3339)。"`
}

// handleTrashIndex は指定 KEY をゴミ箱状態にする（FNC-009）。
// KEY 単位排他（SYN-08）: store.TrashKey は WithKeyLock を内部で取得しない設計のため、
// 呼び出し元が WithKeyLock で囲む（DES-001 §4.3、schedule_delete_series と同パターン）。
func (h *Handlers) handleTrashIndex(
	ctx context.Context, _ *mcpsdk.CallToolRequest, in TrashIndexInput,
) (res *mcpsdk.CallToolResult, out TrashIndexResult, err error) {
	if in.Key == "" {
		return nil, TrashIndexResult{}, errors.New("key は必須")
	}
	start := time.Now()
	slog.Info("trash_index start", "key", in.Key)
	defer func() {
		logHandlerDone("trash_index done", err, start, "key", in.Key, "trashed", out.Trashed)
	}()

	var trashedAt string
	if lerr := h.store.WithKeyLock(ctx, in.Key, func() error {
		var terr error
		trashedAt, terr = h.store.TrashKey(ctx, in.Key)
		return terr
	}); lerr != nil {
		// 存在しない KEY・既にゴミ箱状態 (多重投入) はいずれも store.TrashKey が返すエラーを
		// そのまま caller に伝播する (silent failure 禁止)。
		err = fmt.Errorf("trash index: %w", lerr)
		return nil, TrashIndexResult{}, err
	}

	out = TrashIndexResult{Key: in.Key, Trashed: true, TrashedAt: trashedAt}
	return nil, out, nil
}

// -----------------------------------------------------------------------
// list_trashed_indexes (FNC-010)
// -----------------------------------------------------------------------

// TrashedIndexInfo は list_trashed_indexes の一覧要素。
type TrashedIndexInfo struct {
	Key              string `json:"key" jsonschema:"ゴミ箱状態の KEY。"`
	TrashedAt        string `json:"trashed_at" jsonschema:"ゴミ箱投入日時 (RFC3339)。"`
	RemainingSeconds int64  `json:"remaining_seconds" jsonschema:"自動最終処分までの残り秒数。trashed_at + trash.retention_days から算出。保持期間を過ぎていて internal/trash.Worker の次回実行がまだ来ていない場合は 0 (負値にはならない)。"`
}

// ListTrashedIndexesInput は list_trashed_indexes の入力（パラメータなし）。
type ListTrashedIndexesInput struct{}

// ListTrashedIndexesResult は list_trashed_indexes の出力。
type ListTrashedIndexesResult struct {
	Indexes []TrashedIndexInfo `json:"indexes" jsonschema:"現在ゴミ箱状態の KEY 一覧 (trashed_at 昇順、古い順)。orphan record は含まない (システム自動管理のため FNC-013)。"`
}

// handleListTrashedIndexes はゴミ箱状態の KEY 一覧を返す（FNC-010）。
//
// remaining_seconds の算出（DES-001 §3.2 設計判断・ADR-003）:
// internal/store は「trashed_at という事実」のみを返し、保持期間 (trash.retention_days)
// を用いた残り時間の計算ロジックは持たない（Store 層は判定を持たない）。
// この計算は設定値にアクセスできる呼び出し元、つまり本ハンドラの責務とする
// （internal/trash.Worker の retentionCutoff と同じ考え方を、残り時間側に適用したもの）。
func (h *Handlers) handleListTrashedIndexes(
	ctx context.Context, _ *mcpsdk.CallToolRequest, _ ListTrashedIndexesInput,
) (res *mcpsdk.CallToolResult, out ListTrashedIndexesResult, err error) {
	start := time.Now()
	defer func() {
		logHandlerDone("list_trashed_indexes done", err, start, "count", len(out.Indexes))
	}()

	trashed, lerr := h.store.ListTrashedKeys(ctx)
	if lerr != nil {
		err = fmt.Errorf("list trashed keys: %w", lerr)
		return nil, ListTrashedIndexesResult{}, err
	}

	now := time.Now()
	retention := time.Duration(h.trashRetentionDays) * 24 * time.Hour
	indexes := make([]TrashedIndexInfo, 0, len(trashed))
	for _, info := range trashed {
		var remaining int64
		trashedAt, perr := time.Parse(time.RFC3339, info.TrashedAt)
		if perr != nil {
			// trashed_at が不正な形式の場合も silent failure にせず記録して継続する
			// (internal/trash.Worker.sweepTrashedKeys と同方針)。
			slog.Error("list_trashed_indexes: trashed_at のパース失敗",
				"key", info.Key, "trashed_at", info.TrashedAt, "error", perr)
			remaining = 0
		} else {
			deadline := trashedAt.Add(retention)
			d := deadline.Sub(now)
			if d > 0 {
				remaining = int64(d.Seconds())
			}
		}
		indexes = append(indexes, TrashedIndexInfo{
			Key:              info.Key,
			TrashedAt:        info.TrashedAt,
			RemainingSeconds: remaining,
		})
	}

	return nil, ListTrashedIndexesResult{Indexes: indexes}, nil
}

// -----------------------------------------------------------------------
// ゴミ箱 KEY への書き込み系操作拒否 (FNC-009, DES-001 §5.7 UC-7)
// -----------------------------------------------------------------------

// rejectIfTrashed は key がゴミ箱状態（trashed_at が非 NULL）の場合、操作を
// 拒否するエラーを返す。upsert_documents / sync_documents / delete_documents /
// delete_series / schedule_delete_series の書き込み系 5 ツールがそれぞれの入力必須項目
// チェックの直後・実処理（WithKeyLock 取得や DB 書き込み）の前に呼ぶ（DES-001 §4.6/§5.7 UC-7）。
// さらに各ツールは WithKeyLock 内（mutation 直前）でも再度呼ぶ（TOCTOU 対策。ロック取得前の
// 判定とロック取得の間に trash_index が完了する余地があるため）。
// query も KeyExists 確認の直後・検索実行前に呼ぶ（TASK-009）。
// trashed_at は変更しない（黙って復活させない。復活は restore_index の明示操作のみ、FNC-011）。
// 存在しない KEY は h.store.IsTrashed が false を返すため拒否対象にならない
// （「存在しない KEY」と「ゴミ箱状態」を区別する。存在しない KEY は各ツールの後続処理が
// 別途エラーにする、または新規作成として扱う）。
func (h *Handlers) rejectIfTrashed(ctx context.Context, key string) error {
	trashed, err := h.store.IsTrashed(ctx, key)
	if err != nil {
		return fmt.Errorf("check trashed: %w", err)
	}
	if trashed {
		return fmt.Errorf("key %q はゴミ箱に入っています。restore_index で復活してから操作してください", key)
	}
	return nil
}

// -----------------------------------------------------------------------
// restore_index (FNC-011)
// -----------------------------------------------------------------------

// RestoreIndexInput は restore_index の入力。
type RestoreIndexInput struct {
	Key string `json:"key" jsonschema:"復活させる KEY。ゴミ箱状態 (trash_index 済み) であること。"`
}

// RestoreIndexResult は restore_index の出力。
type RestoreIndexResult struct {
	Key      string `json:"key" jsonschema:"復活した KEY (入力の echo back)。"`
	Restored bool   `json:"restored" jsonschema:"復活に成功したか。"`
}

// handleRestoreIndex はゴミ箱状態の KEY を利用可能な状態へ戻す（FNC-011）。
// KEY 単位排他（SYN-08）: store.RestoreKey は WithKeyLock を内部で取得しない設計のため、
// 呼び出し元が WithKeyLock で囲む（trash_index と同パターン）。
// store.RestoreKey が返すエラー（存在しない KEY・未ゴミ箱化 KEY への復活）はそのまま
// caller に伝播する（silent failure 禁止）。
func (h *Handlers) handleRestoreIndex(
	ctx context.Context, _ *mcpsdk.CallToolRequest, in RestoreIndexInput,
) (res *mcpsdk.CallToolResult, out RestoreIndexResult, err error) {
	if in.Key == "" {
		return nil, RestoreIndexResult{}, errors.New("key は必須")
	}
	start := time.Now()
	slog.Info("restore_index start", "key", in.Key)
	defer func() {
		logHandlerDone("restore_index done", err, start, "key", in.Key, "restored", out.Restored)
	}()

	if lerr := h.store.WithKeyLock(ctx, in.Key, func() error {
		return h.store.RestoreKey(ctx, in.Key)
	}); lerr != nil {
		err = fmt.Errorf("restore index: %w", lerr)
		return nil, RestoreIndexResult{}, err
	}

	out = RestoreIndexResult{Key: in.Key, Restored: true}
	return nil, out, nil
}
