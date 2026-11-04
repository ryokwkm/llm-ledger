package llm

import (
	"time"

	"gorm.io/gorm"
)

type LLMLog struct {
	ID            int64 `gorm:"primaryKey"`
	FuncFeature   string
	GroupID       string // 集計用の値（ChatRequest.GroupID）。空なら DB には NULL が入る
	Memo          string
	Status        LogStatus
	InputTokens   int
	OutputTokens  int
	ModelName     string
	InputContent  string
	OutputContent string
	AppStatus     AppStatus
	AppError      string
	CreatedAt     time.Time
}

func (LLMLog) TableName() string {
	return "system.llm_logs"
}

// BeforeCreate は GroupID が空のとき、この INSERT から group_id の列を外す（DB の DEFAULT NULL が入る）。
// group_id の列が無い v0.1.0 の schema.sql の DB へ、SaveLog を通さず Create するコードからも、版を上げただけで今までどおり書けるようにするため。
// tx.Omit は新しい Statement を作って効かないので、この操作の Statement へ直に足す。
// タグの default:null は使えない（SQLite では INSERT の後に RETURNING group_id を付けて、列が無いと落ちる）
func (l *LLMLog) BeforeCreate(tx *gorm.DB) error {
	if l.GroupID == "" {
		tx.Statement.Omits = append(tx.Statement.Omits, "group_id")
	}
	return nil
}
