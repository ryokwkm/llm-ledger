package llm

import (
	"time"
)

type LLMFunctionConfig struct {
	FuncFeature string `gorm:"primaryKey"`
	DailyLimit  int
	MaxTokens   int
	IsActive    bool
	Description string
	UpdatedAt   time.Time
}

func (LLMFunctionConfig) TableName() string {
	return "system.llm_function_configs"
}

func (c *LLMFunctionConfig) IsEnabled() bool {
	return c.IsActive
}

// DailyLimitUnlimited は daily_limit に入れると日次上限を掛けない値。
// 0 は「1 回も呼ばない」のまま残す。入れ忘れや構造体のゼロ値が呼び放題に倒れないようにするため。
const DailyLimitUnlimited = -1

func (c *LLMFunctionConfig) IsLimitExceeded(currentUsage int) bool {
	if c.DailyLimit == DailyLimitUnlimited {
		return false
	}
	return currentUsage >= c.DailyLimit
}
