package llm

import "context"

type LogHandle struct {
	ID     int64
	repo   Repository
	logger Logger
}

func NewNoopLogHandle() *LogHandle {
	return &LogHandle{ID: 0, repo: nil}
}

// MarkAsInvalid: JSONパースエラー時などに呼ぶ
func (h *LogHandle) MarkAsInvalid(ctx context.Context, err error) {
	if h.ID == 0 || h.repo == nil {
		return
	}
	h.updateValidation(ctx, AppStatusInvalid, err.Error())
}

// MarkAsValid: 成功時に明示したい場合（基本はUnknown=0で運用しても良い）
func (h *LogHandle) MarkAsValid(ctx context.Context) {
	if h.ID == 0 || h.repo == nil {
		return
	}
	h.updateValidation(ctx, AppStatusValid, "")
}

// 書き込めなくても LLM の呼び出し側は処理を続ける。ただ黙って落とすと app_status / app_error の痕跡ごと消えるので WARN に残す。
func (h *LogHandle) updateValidation(ctx context.Context, appStatus AppStatus, appError string) {
	if err := h.repo.UpdateLogValidation(ctx, h.ID, appStatus, appError); err != nil {
		h.logger.Warn("LLM ログへ検証結果を記録できませんでした",
			"log_id", h.ID, "app_status", appStatus, "err", err)
	}
}
