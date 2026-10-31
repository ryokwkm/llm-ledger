package llm

import (
	"time"
)

type LLMLog struct {
	ID            int64 `gorm:"primaryKey"`
	FuncFeature   string
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
