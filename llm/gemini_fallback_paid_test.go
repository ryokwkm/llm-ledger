package llm

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// withPaidChain は課金キーで試す chain を c に付ける。本番の NewFallbackGeminiClient が PaidAPIKey から作るものの代わり。
func withPaidChain(c *FallbackGeminiClient, paid ...geminiChainClient) {
	c.paid = newFallbackGeminiClientFromChain(GeminiModel(paid[0].ModelName()), paid, nil)
}

func TestFallbackGeminiClient_無料のchainが全段とも日次の枠切れなら課金キーで送る(t *testing.T) {
	flash := &fakeGeminiClient{modelName: ModelGemini38Flash}
	lite := &fakeGeminiClient{modelName: ModelGemini31FlashLite}
	paidFlash := &fakeGeminiClient{modelName: ModelGemini38Flash}

	c := newFallbackGeminiClientFromChain(ModelGemini38Flash, []geminiChainClient{flash, lite}, RateLimitsFreeAPIKey)
	c.quota.MarkExhausted(ModelGemini38Flash)
	c.quota.MarkExhausted(ModelGemini31FlashLite)
	withPaidChain(c, paidFlash)

	resp, err := c.CreateChatCompletionWithTokens(context.Background(), &ChatRequest{Prompt: "x"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.ActualModel != "paid:gemini-3.8-flash" {
		t.Errorf("ActualModel = %q, want paid:gemini-3.8-flash（課金キーの印を付ける）", resp.ActualModel)
	}
	if flash.calls != 0 || lite.calls != 0 || paidFlash.calls != 1 {
		t.Errorf("calls: flash=%d lite=%d paid=%d, want 0/0/1", flash.calls, lite.calls, paidFlash.calls)
	}
}

func TestFallbackGeminiClient_無料のchainが全段とも429なら課金キーで送る(t *testing.T) {
	cases := map[string][]error{
		"retry_delay なし":         {quotaErr()},
		"retry_delay が待てる上限を超える": {quotaErrWithRetryDelay(maxTotalRetryWait + time.Second)},
	}
	for name, errs := range cases {
		t.Run(name, func(t *testing.T) {
			flash := &fakeGeminiClient{modelName: ModelGemini38Flash, errs: errs}
			preview := &fakeGeminiClient{modelName: ModelGemini3FlashPreview, errs: []error{geminiHTTPError(404)}}
			paidLite := &fakeGeminiClient{modelName: ModelGemini31FlashLite}

			c := newFallbackGeminiClientFromChain(ModelGemini38Flash, []geminiChainClient{flash, preview}, RateLimitsFreeAPIKey)
			withPaidChain(c, paidLite)

			resp, err := c.CreateChatCompletionWithTokens(context.Background(), &ChatRequest{Prompt: "x"})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if resp.ActualModel != "paid:gemini-3.1-flash-lite" {
				t.Errorf("ActualModel = %q, want paid:gemini-3.1-flash-lite", resp.ActualModel)
			}
			if paidLite.calls != 1 {
				t.Errorf("paid.calls = %d, want 1", paidLite.calls)
			}
		})
	}
}

// 無料キーで成功したら課金キーは使わない（Flash の枠切れで無料の Lite へ落ちるのも今までどおり）。
func TestFallbackGeminiClient_無料キーで答えられたら課金キーを使わない(t *testing.T) {
	flash := &fakeGeminiClient{modelName: ModelGemini38Flash, errs: []error{dailyQuotaErr()}}
	lite := &fakeGeminiClient{modelName: ModelGemini31FlashLite}
	paidFlash := &fakeGeminiClient{modelName: ModelGemini38Flash}

	c := newFallbackGeminiClientFromChain(ModelGemini38Flash, []geminiChainClient{flash, lite}, RateLimitsFreeAPIKey)
	withPaidChain(c, paidFlash)

	resp, err := c.CreateChatCompletionWithTokens(context.Background(), &ChatRequest{Prompt: "x"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.ActualModel != string(ModelGemini31FlashLite) {
		t.Errorf("ActualModel = %q, want %q", resp.ActualModel, ModelGemini31FlashLite)
	}
	if paidFlash.calls != 0 {
		t.Errorf("paid.calls = %d, want 0", paidFlash.calls)
	}
}

// 枠切れでない失敗（無応答・その他のエラー）では課金キーへ行かない。課金キーで送り直すのは回数の枠を使い切ったときだけ。
func TestFallbackGeminiClient_枠切れでない失敗では課金キーへ行かない(t *testing.T) {
	cases := map[string]struct {
		flash *fakeGeminiClient
		want  error
	}{
		"無応答が混ざる": {
			flash: &fakeGeminiClient{modelName: ModelGemini38Flash, hang: true},
		},
		"枠切れ以外のエラー": {
			flash: &fakeGeminiClient{modelName: ModelGemini38Flash, errs: []error{errors.New("network unreachable")}},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			lite := &fakeGeminiClient{modelName: ModelGemini31FlashLite, errs: []error{quotaErr()}}
			paidFlash := &fakeGeminiClient{modelName: ModelGemini38Flash}

			c := newFallbackGeminiClientFromChain(ModelGemini38Flash, []geminiChainClient{tc.flash, lite}, RateLimitsFreeAPIKey)
			c.callTimeout = 20 * time.Millisecond
			withPaidChain(c, paidFlash)

			if _, err := c.CreateChatCompletionWithTokens(context.Background(), &ChatRequest{Prompt: "x"}); err == nil {
				t.Fatal("expected error")
			}
			if paidFlash.calls != 0 {
				t.Errorf("paid.calls = %d, want 0", paidFlash.calls)
			}
		})
	}
}

func TestFallbackGeminiClient_課金キーでも失敗したら両方の原因を返す(t *testing.T) {
	flash := &fakeGeminiClient{modelName: ModelGemini38Flash}
	paidErr := errors.New("billing disabled")
	paidFlash := &fakeGeminiClient{modelName: ModelGemini38Flash, errs: []error{paidErr}}

	c := newFallbackGeminiClientFromChain(ModelGemini38Flash, []geminiChainClient{flash}, RateLimitsFreeAPIKey)
	c.quota.MarkExhausted(ModelGemini38Flash)
	withPaidChain(c, paidFlash)

	_, err := c.CreateChatCompletionWithTokens(context.Background(), &ChatRequest{Prompt: "x"})
	if !errors.Is(err, paidErr) {
		t.Fatalf("err = %v, want 課金キーのエラーを含む", err)
	}
	if !strings.Contains(err.Error(), ErrDailyQuotaExhausted.Error()) {
		t.Errorf("err = %v, want 無料キーの枠切れも分かる", err)
	}
}

func TestNewFallbackGeminiClient_課金キー(t *testing.T) {
	base := FallbackConfig{
		Chain:       []GeminiModel{ModelGemini38Flash},
		APIKey:      "dummy-free-key",
		CallTimeout: time.Minute,
	}

	noChain := base
	noChain.PaidAPIKey = "dummy-paid-key"
	if _, err := NewFallbackGeminiClient(context.Background(), noChain); err == nil {
		t.Error("PaidAPIKey があるのに PaidChain が無いまま作れた（単価がモデルで違うので明示させる）")
	} else if !strings.Contains(err.Error(), "PaidChain") {
		t.Errorf("error message = %q, want to contain 'PaidChain'", err.Error())
	}

	// 課金キーの chain は PaidChain・PaidAPIKey で作り、無料キーの枠の値を引き継がない（本物のクライアントは作ると外へ接続しに行くので、設定だけを確かめる）
	withKey := base
	withKey.RateLimits = RateLimitsFreeAPIKey
	withKey.DailyQuotas = DailyQuotasFreeAPIKey
	withKey.QuotaSeed = func(context.Context) (map[GeminiModel]int, error) { return nil, nil }
	withKey.PaidAPIKey = "dummy-paid-key"
	withKey.PaidChain = []GeminiModel{ModelGemini31FlashLite}
	got := paidConfig(withKey)
	if got.APIKey != "dummy-paid-key" || !slices.Equal(got.Chain, withKey.PaidChain) || got.CallTimeout != time.Minute {
		t.Errorf("課金キーの chain の設定 = %+v, want APIKey=dummy-paid-key・Chain=PaidChain・CallTimeout=1m", got)
	}
	if got.RateLimits != nil || got.DailyQuotas != nil || got.QuotaSeed != nil {
		t.Error("課金キーに無料キーの枠（RateLimits・DailyQuotas・QuotaSeed）を効かせている")
	}
}
