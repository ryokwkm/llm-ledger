package llm

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestRedactAPIKey(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			// 警告ログに実際に出ていた形。
			name: "URL のクエリに載った API キーを伏せる",
			in:   `Post "https://generativelanguage.googleapis.com/v1beta/models/gemini-3.8-flash:generateContent?%24alt=json%3Benum-encoding%3Dint&key=AQ.dummy-api-key": context deadline exceeded`,
			want: `Post "https://generativelanguage.googleapis.com/v1beta/models/gemini-3.8-flash:generateContent?%24alt=json%3Benum-encoding%3Dint&key=REDACTED": context deadline exceeded`,
		},
		{
			name: "クエリの先頭にあっても伏せる",
			in:   "https://example.com/v1beta/models?key=secret123",
			want: "https://example.com/v1beta/models?key=REDACTED",
		},
		{
			name: "キーが無ければそのまま返す",
			in:   "429: GenerateRequestsPerDayPerProjectPerModel-FreeTier",
			want: "429: GenerateRequestsPerDayPerProjectPerModel-FreeTier",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := redactAPIKey(tt.in); got != tt.want {
				t.Errorf("redactAPIKey() = %v, want %v", got, tt.want)
			}
		})
	}
}

// Error() だけを差し替えてチェーンを残す設計の要。キーを伏せた結果 RPD 超過を
// 取りこぼすと、枠が切れたモデルへ投げ続けて毎回 1 往復を無駄にする。
func TestRedactedError_日次クォータの判定を壊さない(t *testing.T) {
	tests := []struct {
		name string
		base error
	}{
		{"gRPC の QuotaFailure から判定する経路", dailyQuotaErr()},
		{"エラー文字列から判定する経路", errors.New(`Post "https://x/v1?key=AQ.secret": Daily Limit exceeded`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := fmt.Errorf("Gemini API error: %w", &redactedError{err: tt.base})
			if !isDailyQuotaExceeded(err) {
				t.Errorf("isDailyQuotaExceeded() = false, want true")
			}
			if strings.Contains(err.Error(), "AQ.secret") {
				t.Errorf("API キーが伏せられていない: %v", err)
			}
		})
	}
}

func TestRedactedError_元のエラーへ辿れる(t *testing.T) {
	base := errors.New("原因のエラー")
	err := fmt.Errorf("Gemini API error: %w", &redactedError{err: base})

	if !errors.Is(err, base) {
		t.Errorf("errors.Is() = false, want true（Unwrap が繋がっていない）")
	}
}
