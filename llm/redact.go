package llm

import "regexp"

// apiKeyInURLPattern は API 呼び出しの URL に載る API キー。
//
// google の SDK が返すエラーはリクエスト URL をそのまま含むため、そのままログへ出すと
// キーが平文で残る。
var apiKeyInURLPattern = regexp.MustCompile(`(?i)([?&]key=)[^&"\s]+`)

// redactAPIKey は文字列に含まれる "key=..." の値を伏せる。
func redactAPIKey(s string) string {
	return apiKeyInURLPattern.ReplaceAllString(s, "${1}REDACTED")
}

// redactedError は Error() でだけ API キーを伏せ、元のエラーは Unwrap に残す。
//
// 元のエラーごと差し替えないのは、日次クォータの判定（isDailyQuotaExceeded）が
// errors.Unwrap で辿った先の gRPC status を読むため。チェーンを切ると RPD 超過を
// 取りこぼし、枠が切れたモデルへ投げ続けることになる。
type redactedError struct {
	err error
}

func (e *redactedError) Error() string { return redactAPIKey(e.err.Error()) }

func (e *redactedError) Unwrap() error { return e.err }
