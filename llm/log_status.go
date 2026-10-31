package llm

// LogStatus はLLM実行ログのステータスを表す型
type LogStatus int

const (
	LogStatusUnknown       LogStatus = 0
	LogStatusSuccess       LogStatus = 1
	LogStatusFailed        LogStatus = 2
	LogStatusLimited       LogStatus = 3
	LogStatusDisabled      LogStatus = 4
	LogStatusConfigMissing LogStatus = 5
)

// AppStatus はアプリケーションロジックレベルの評価ステータス
type AppStatus int

const (
	AppStatusUnknown AppStatus = 0 // 初期状態 (Valid扱いでも可)
	AppStatusValid   AppStatus = 1 // 正常 (明示的にマークする場合)
	AppStatusInvalid AppStatus = 2 // 不正 (JSONパース失敗、必須項目欠落など)
)
