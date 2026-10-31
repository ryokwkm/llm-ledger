package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/googleapis/gax-go/v2/apierror"
	"google.golang.org/api/googleapi"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

// geminiHTTPError は GeminiClient が HTTP エラーの応答を受けたときに返すのと同じ包み方のエラーを作る。
func geminiHTTPError(code int) error {
	ae, _ := apierror.FromError(&googleapi.Error{Code: code, Message: http.StatusText(code)})
	return fmt.Errorf("Gemini API error: %w", &redactedError{err: ae})
}

type fakeGeminiClient struct {
	modelName GeminiModel
	calls     int
	// 呼び出し回数ごとに使うエラー / レスポンスを与える。nil エラーなら resp を返す。
	errs  []error
	resps []*ChatResponse
	// hang が true なら ctx が終わるまで応答しない（Gemini が無応答で張り付く状態を再現する）。
	hang bool
}

func (f *fakeGeminiClient) ModelName() string { return string(f.modelName) }

func (f *fakeGeminiClient) Close() error { return nil }

func (f *fakeGeminiClient) CreateChatCompletionWithTokens(ctx context.Context, _ *ChatRequest) (*ChatResponse, error) {
	idx := f.calls
	f.calls++
	if f.hang {
		<-ctx.Done()
		return nil, fmt.Errorf("Gemini API error: %w", ctx.Err())
	}
	if idx < len(f.errs) && f.errs[idx] != nil {
		return nil, f.errs[idx]
	}
	if idx < len(f.resps) && f.resps[idx] != nil {
		return f.resps[idx], nil
	}
	return &ChatResponse{Content: "ok", ActualModel: string(f.modelName)}, nil
}

func quotaErr() error {
	return status.Error(codes.ResourceExhausted, "RESOURCE_EXHAUSTED: free quota exceeded")
}

// quotaErrWithRetryDelay は RetryInfo を Details に積んだ ResourceExhausted エラーを返す。
// extractRetryDelay の経路を踏ませるためのヘルパー。
func quotaErrWithRetryDelay(delay time.Duration) error {
	st := status.New(codes.ResourceExhausted, "RESOURCE_EXHAUSTED: free quota exceeded")
	withDetails, err := st.WithDetails(&errdetails.RetryInfo{RetryDelay: durationpb.New(delay)})
	if err != nil {
		panic(err)
	}
	return withDetails.Err()
}

func TestFallbackGeminiClient_PrimarySucceedsImmediately(t *testing.T) {
	flash := &fakeGeminiClient{modelName: ModelGemini36Flash}
	preview := &fakeGeminiClient{modelName: ModelGemini3FlashPreview}
	lite := &fakeGeminiClient{modelName: ModelGemini31FlashLite}

	c := newFallbackGeminiClientFromChain(ModelGemini36Flash,
		[]geminiChainClient{flash, preview, lite}, RateLimitsFreeAPIKey)

	resp, err := c.CreateChatCompletionWithTokens(context.Background(), &ChatRequest{Prompt: "x"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.ActualModel != string(ModelGemini36Flash) {
		t.Errorf("ActualModel = %q, want %q", resp.ActualModel, ModelGemini36Flash)
	}
	if flash.calls != 1 {
		t.Errorf("flash.calls = %d, want 1", flash.calls)
	}
	if preview.calls != 0 || lite.calls != 0 {
		t.Errorf("preview/lite should not be called: preview=%d lite=%d", preview.calls, lite.calls)
	}
}

func TestFallbackGeminiClient_FallsBackOnQuotaExceeded(t *testing.T) {
	flash := &fakeGeminiClient{modelName: ModelGemini36Flash, errs: []error{quotaErr()}}
	preview := &fakeGeminiClient{modelName: ModelGemini3FlashPreview}
	lite := &fakeGeminiClient{modelName: ModelGemini31FlashLite}

	c := newFallbackGeminiClientFromChain(ModelGemini36Flash,
		[]geminiChainClient{flash, preview, lite}, RateLimitsFreeAPIKey)

	resp, err := c.CreateChatCompletionWithTokens(context.Background(), &ChatRequest{Prompt: "x"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.ActualModel != string(ModelGemini3FlashPreview) {
		t.Errorf("ActualModel = %q, want %q", resp.ActualModel, ModelGemini3FlashPreview)
	}
	if flash.calls != 1 || preview.calls != 1 {
		t.Errorf("calls: flash=%d preview=%d, want 1/1", flash.calls, preview.calls)
	}
	if lite.calls != 0 {
		t.Errorf("lite.calls = %d, want 0", lite.calls)
	}
}

func TestFallbackGeminiClient_FallsBackTwice(t *testing.T) {
	flash := &fakeGeminiClient{modelName: ModelGemini36Flash, errs: []error{quotaErr()}}
	preview := &fakeGeminiClient{modelName: ModelGemini3FlashPreview, errs: []error{quotaErr()}}
	lite := &fakeGeminiClient{modelName: ModelGemini31FlashLite}

	c := newFallbackGeminiClientFromChain(ModelGemini36Flash,
		[]geminiChainClient{flash, preview, lite}, RateLimitsFreeAPIKey)

	resp, err := c.CreateChatCompletionWithTokens(context.Background(), &ChatRequest{Prompt: "x"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.ActualModel != string(ModelGemini31FlashLite) {
		t.Errorf("ActualModel = %q, want %q", resp.ActualModel, ModelGemini31FlashLite)
	}
	if flash.calls != 1 || preview.calls != 1 || lite.calls != 1 {
		t.Errorf("calls: flash=%d preview=%d lite=%d, want 1/1/1", flash.calls, preview.calls, lite.calls)
	}
}

// 全段が無料枠超過、retry_delay も付いてこないケース。最終失敗する。
func TestFallbackGeminiClient_AllExhausted(t *testing.T) {
	flash := &fakeGeminiClient{modelName: ModelGemini36Flash, errs: []error{quotaErr()}}
	preview := &fakeGeminiClient{modelName: ModelGemini3FlashPreview, errs: []error{quotaErr()}}
	lite := &fakeGeminiClient{modelName: ModelGemini31FlashLite, errs: []error{quotaErr()}}

	c := newFallbackGeminiClientFromChain(ModelGemini36Flash,
		[]geminiChainClient{flash, preview, lite}, RateLimitsFreeAPIKey)

	_, err := c.CreateChatCompletionWithTokens(context.Background(), &ChatRequest{Prompt: "x"})
	if err == nil {
		t.Fatal("expected error when all models exhausted")
	}
	if flash.calls != 1 || preview.calls != 1 || lite.calls != 1 {
		t.Errorf("calls: flash=%d preview=%d lite=%d, want 1/1/1", flash.calls, preview.calls, lite.calls)
	}
}

func TestFallbackGeminiClient_NonQuotaErrorDoesNotFallback(t *testing.T) {
	netErr := errors.New("network unreachable")
	flash := &fakeGeminiClient{modelName: ModelGemini36Flash, errs: []error{netErr}}
	preview := &fakeGeminiClient{modelName: ModelGemini3FlashPreview}
	lite := &fakeGeminiClient{modelName: ModelGemini31FlashLite}

	c := newFallbackGeminiClientFromChain(ModelGemini36Flash,
		[]geminiChainClient{flash, preview, lite}, RateLimitsFreeAPIKey)

	_, err := c.CreateChatCompletionWithTokens(context.Background(), &ChatRequest{Prompt: "x"})
	if err == nil {
		t.Fatal("expected error to be propagated")
	}
	if !errors.Is(err, netErr) {
		t.Errorf("expected network error to be returned, got: %v", err)
	}
	if flash.calls != 1 {
		t.Errorf("flash.calls = %d, want 1", flash.calls)
	}
	if preview.calls != 0 || lite.calls != 0 {
		t.Errorf("non-quota error should not fall back: preview=%d lite=%d", preview.calls, lite.calls)
	}
}

// 無応答のモデルは上限時間で打ち切り、次のモデルへ落ちる（同じモデルへ投げ直すと張り付き続けるため）。
func TestFallbackGeminiClient_FallsBackOnCallTimeout(t *testing.T) {
	flash := &fakeGeminiClient{modelName: ModelGemini38Flash, hang: true}
	next := &fakeGeminiClient{modelName: ModelGemini36Flash}

	c := newFallbackGeminiClientFromChain(ModelGemini38Flash,
		[]geminiChainClient{flash, next}, RateLimitsFreeAPIKey)
	c.callTimeout = 20 * time.Millisecond

	resp, err := c.CreateChatCompletionWithTokens(context.Background(), &ChatRequest{Prompt: "x"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.ActualModel != string(ModelGemini36Flash) {
		t.Errorf("ActualModel = %q, want %q", resp.ActualModel, ModelGemini36Flash)
	}
	if flash.calls != 1 || next.calls != 1 {
		t.Errorf("calls: flash=%d next=%d, want 1/1", flash.calls, next.calls)
	}
}

// 全段が無応答なら待たずに失敗を返し、エラー文で無応答だったと分かる。
func TestFallbackGeminiClient_AllCallsTimeout(t *testing.T) {
	flash := &fakeGeminiClient{modelName: ModelGemini38Flash, hang: true}
	lite := &fakeGeminiClient{modelName: ModelGemini35FlashLite, hang: true}

	c := newFallbackGeminiClientFromChain(ModelGemini38Flash,
		[]geminiChainClient{flash, lite}, RateLimitsFreeAPIKey)
	c.callTimeout = 20 * time.Millisecond

	_, err := c.CreateChatCompletionWithTokens(context.Background(), &ChatRequest{Prompt: "x"})
	if !errors.Is(err, errCallTimedOut) {
		t.Fatalf("err = %v, want errCallTimedOut", err)
	}
	if flash.calls != 1 || lite.calls != 1 {
		t.Errorf("calls: flash=%d lite=%d, want 1/1", flash.calls, lite.calls)
	}
}

// 呼び出し元の ctx が終わった（停止シグナル等）ときは無応答と区別し、次のモデルへ落ちない。
func TestFallbackGeminiClient_ParentDeadlineDoesNotFallback(t *testing.T) {
	flash := &fakeGeminiClient{modelName: ModelGemini38Flash, hang: true}
	next := &fakeGeminiClient{modelName: ModelGemini36Flash}

	c := newFallbackGeminiClientFromChain(ModelGemini38Flash,
		[]geminiChainClient{flash, next}, RateLimitsFreeAPIKey)
	c.callTimeout = time.Minute

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := c.CreateChatCompletionWithTokens(ctx, &ChatRequest{Prompt: "x"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if errors.Is(err, errCallTimedOut) {
		t.Errorf("parent deadline must not be reported as errCallTimedOut: %v", err)
	}
	if next.calls != 0 {
		t.Errorf("next.calls = %d, want 0", next.calls)
	}
}

// chain 全段が retry_delay 付きの quota error → 待機 → 先頭から再試行で成功するケース。
func TestFallbackGeminiClient_RetryDelayLoop_RecoversOnSecondAttempt(t *testing.T) {
	flash := &fakeGeminiClient{
		modelName: ModelGemini36Flash,
		errs:      []error{quotaErrWithRetryDelay(10 * time.Millisecond)},
	}
	preview := &fakeGeminiClient{
		modelName: ModelGemini3FlashPreview,
		errs:      []error{quotaErrWithRetryDelay(10 * time.Millisecond)},
	}
	lite := &fakeGeminiClient{
		modelName: ModelGemini31FlashLite,
		errs:      []error{quotaErrWithRetryDelay(10 * time.Millisecond)},
	}

	c := newFallbackGeminiClientFromChain(ModelGemini36Flash,
		[]geminiChainClient{flash, preview, lite}, RateLimitsFreeAPIKey)

	resp, err := c.CreateChatCompletionWithTokens(context.Background(), &ChatRequest{Prompt: "x"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.ActualModel != string(ModelGemini36Flash) {
		t.Errorf("ActualModel = %q, want %q (2nd attempt should hit flash again)", resp.ActualModel, ModelGemini36Flash)
	}
	if flash.calls != 2 {
		t.Errorf("flash.calls = %d, want 2 (1 attempt + 1 retry)", flash.calls)
	}
	if preview.calls != 1 || lite.calls != 1 {
		t.Errorf("calls: preview=%d lite=%d, want 1/1 (only first attempt)", preview.calls, lite.calls)
	}
}

// retry budget を使い切るまで試行しても回復しないケース。最終的に失敗する。
func TestFallbackGeminiClient_RetryDelayLoop_GivesUp(t *testing.T) {
	// 各モデル「無限に quota error を返す」設定（errs を毎回 fresh で渡せないので大量に）。
	makeErrs := func() []error {
		errs := make([]error, 10)
		for i := range errs {
			errs[i] = quotaErrWithRetryDelay(5 * time.Millisecond)
		}
		return errs
	}
	flash := &fakeGeminiClient{modelName: ModelGemini36Flash, errs: makeErrs()}
	preview := &fakeGeminiClient{modelName: ModelGemini3FlashPreview, errs: makeErrs()}
	lite := &fakeGeminiClient{modelName: ModelGemini31FlashLite, errs: makeErrs()}

	c := newFallbackGeminiClientFromChain(ModelGemini36Flash,
		[]geminiChainClient{flash, preview, lite}, RateLimitsFreeAPIKey)

	_, err := c.CreateChatCompletionWithTokens(context.Background(), &ChatRequest{Prompt: "x"})
	if err == nil {
		t.Fatal("expected error when retry budget exhausted")
	}
	// maxRetryAttempts=3 → 各モデル 3 回呼ばれる
	if flash.calls != 3 || preview.calls != 3 || lite.calls != 3 {
		t.Errorf("calls: flash=%d preview=%d lite=%d, want 3/3/3 (maxRetryAttempts loops)",
			flash.calls, preview.calls, lite.calls)
	}
}

func TestIsFreeQuotaExceeded(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"gRPC ResourceExhausted", status.Error(codes.ResourceExhausted, "quota exceeded"), true},
		{"gRPC Unavailable", status.Error(codes.Unavailable, "unavailable"), false},
		{"string RESOURCE_EXHAUSTED", errors.New("error: RESOURCE_EXHAUSTED reached"), true},
		// 本番の Gemini は REST で、429 は gax が ResourceExhausted の status を持つ APIError へ包み、
		// さらに GeminiClient が redactedError と fmt.Errorf で包んで返す。
		{"本番と同じ包み方の HTTP 429", geminiHTTPError(http.StatusTooManyRequests), true},
		{"本番と同じ包み方の HTTP 500", geminiHTTPError(http.StatusInternalServerError), false},
		{"429 を含むだけの無関係なエラー", errors.New("unexpected end of JSON input at offset 4290"), false},
		{"unrelated", errors.New("network error"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isFreeQuotaExceeded(tt.err); got != tt.want {
				t.Errorf("isFreeQuotaExceeded(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestIsModelRetired(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"本番と同じ包み方の HTTP 404", geminiHTTPError(http.StatusNotFound), true},
		{"gRPC NotFound", status.Error(codes.NotFound, "model not found"), true},
		{"本番と同じ包み方の HTTP 429", geminiHTTPError(http.StatusTooManyRequests), false},
		{"本番と同じ包み方の HTTP 500", geminiHTTPError(http.StatusInternalServerError), false},
		{"unrelated", errors.New("network error"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isModelRetired(tt.err); got != tt.want {
				t.Errorf("isModelRetired(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// extractRetryDelay の単体テスト。RetryInfo Details が積まれていれば取れる、無ければ false。
func TestExtractRetryDelay(t *testing.T) {
	d, ok := extractRetryDelay(quotaErrWithRetryDelay(7 * time.Second))
	if !ok {
		t.Fatal("expected retry delay to be extracted")
	}
	if d != 7*time.Second {
		t.Errorf("delay = %v, want 7s", d)
	}

	if _, ok := extractRetryDelay(quotaErr()); ok {
		t.Error("expected no retry delay when Details is empty")
	}
	if _, ok := extractRetryDelay(errors.New("plain error")); ok {
		t.Error("expected no retry delay for non-gRPC error")
	}
}

// 日次枠を使い切ったモデルは送信せずにスキップし、下位モデルで応答すること（429 を踏む前の切替）。
func TestFallbackGeminiClient_SkipsModelAtDailyLimit(t *testing.T) {
	flash38 := &fakeGeminiClient{modelName: ModelGemini38Flash}
	flash36 := &fakeGeminiClient{modelName: ModelGemini36Flash}
	lite := &fakeGeminiClient{modelName: ModelGemini31FlashLite}

	c := newFallbackGeminiClientFromChain(ModelGemini38Flash,
		[]geminiChainClient{flash38, flash36, lite}, RateLimitsFreeAPIKey)
	c.dailyQuotas = DailyQuotas{ModelGemini38Flash: 1, ModelGemini36Flash: 1}

	first, err := c.CreateChatCompletionWithTokens(context.Background(), &ChatRequest{Prompt: "x"})
	if err != nil {
		t.Fatalf("1回目: %v", err)
	}
	if first.ActualModel != string(ModelGemini38Flash) {
		t.Errorf("1回目の ActualModel = %q, want %q", first.ActualModel, ModelGemini38Flash)
	}

	second, err := c.CreateChatCompletionWithTokens(context.Background(), &ChatRequest{Prompt: "x"})
	if err != nil {
		t.Fatalf("2回目: %v", err)
	}
	if second.ActualModel != string(ModelGemini36Flash) {
		t.Errorf("2回目の ActualModel = %q, want %q", second.ActualModel, ModelGemini36Flash)
	}
	if flash38.calls != 1 {
		t.Errorf("flash38.calls = %d, want 1（枠切れのモデルへは投げない）", flash38.calls)
	}

	third, err := c.CreateChatCompletionWithTokens(context.Background(), &ChatRequest{Prompt: "x"})
	if err != nil {
		t.Fatalf("3回目: %v", err)
	}
	if third.ActualModel != string(ModelGemini31FlashLite) {
		t.Errorf("3回目の ActualModel = %q, want %q（上限なしの Lite へ落ちる）", third.ActualModel, ModelGemini31FlashLite)
	}
}

// chain 全段が日次枠切れなら、1回も送信せず ErrDailyQuotaExhausted で即座に失敗すること
// （待っても当日は復活しないので retry_delay の待機に入ってはいけない）。
func TestFallbackGeminiClient_AllModelsAtDailyLimit(t *testing.T) {
	flash := &fakeGeminiClient{modelName: ModelGemini38Flash}
	lite := &fakeGeminiClient{modelName: ModelGemini31FlashLite}

	c := newFallbackGeminiClientFromChain(ModelGemini38Flash,
		[]geminiChainClient{flash, lite}, RateLimitsFreeAPIKey)
	c.dailyQuotas = DailyQuotas{ModelGemini38Flash: 1, ModelGemini31FlashLite: 1}

	for i := 0; i < 2; i++ {
		if _, err := c.CreateChatCompletionWithTokens(context.Background(), &ChatRequest{Prompt: "x"}); err != nil {
			t.Fatalf("枠内の呼び出し #%d が失敗: %v", i+1, err)
		}
	}

	_, err := c.CreateChatCompletionWithTokens(context.Background(), &ChatRequest{Prompt: "x"})
	if !errors.Is(err, ErrDailyQuotaExhausted) {
		t.Fatalf("err = %v, want ErrDailyQuotaExhausted", err)
	}
	if flash.calls != 1 || lite.calls != 1 {
		t.Errorf("calls: flash=%d lite=%d, want 1/1（枠切れ後は1回も送信しない）", flash.calls, lite.calls)
	}
	if !strings.Contains(err.Error(), string(ModelGemini31FlashLite)) {
		t.Errorf("エラーにスキップしたモデルが含まれていない: %v", err)
	}
}

// 日次超過の 429 を受けたモデルは、想定 RPD に届いていなくても当日は以後スキップすること。
func TestFallbackGeminiClient_MarksExhaustedOnDailyQuotaError(t *testing.T) {
	flash := &fakeGeminiClient{modelName: ModelGemini38Flash, errs: []error{dailyQuotaErr()}}
	lite := &fakeGeminiClient{modelName: ModelGemini31FlashLite}

	c := newFallbackGeminiClientFromChain(ModelGemini38Flash,
		[]geminiChainClient{flash, lite}, RateLimitsFreeAPIKey)
	c.dailyQuotas = DailyQuotas{ModelGemini38Flash: 20, ModelGemini31FlashLite: 500}

	for i := 0; i < 2; i++ {
		resp, err := c.CreateChatCompletionWithTokens(context.Background(), &ChatRequest{Prompt: "x"})
		if err != nil {
			t.Fatalf("#%d: %v", i+1, err)
		}
		if resp.ActualModel != string(ModelGemini31FlashLite) {
			t.Errorf("#%d の ActualModel = %q, want %q", i+1, resp.ActualModel, ModelGemini31FlashLite)
		}
	}
	if flash.calls != 1 {
		t.Errorf("flash.calls = %d, want 1（日次枯渇が判明したモデルへは再送しない）", flash.calls)
	}
}

// 提供を終えたモデル（404）は下位へ落とし、その日は再送しないこと。
// chain には preview のように予告なく止まりうるモデルが入っているので、停止された日にステップごと止めない。
func TestFallbackGeminiClient_FallsBackOnRetiredModel(t *testing.T) {
	retired := &fakeGeminiClient{modelName: ModelGemini3FlashPreview, errs: []error{geminiHTTPError(http.StatusNotFound)}}
	next := &fakeGeminiClient{modelName: ModelGemini31FlashLite}

	c := newFallbackGeminiClientFromChain(ModelGemini3FlashPreview,
		[]geminiChainClient{retired, next}, RateLimitsFreeAPIKey)

	for i := 0; i < 2; i++ {
		resp, err := c.CreateChatCompletionWithTokens(context.Background(), &ChatRequest{Prompt: "x"})
		if err != nil {
			t.Fatalf("#%d: %v", i+1, err)
		}
		if resp.ActualModel != string(ModelGemini31FlashLite) {
			t.Errorf("#%d の ActualModel = %q, want %q", i+1, resp.ActualModel, ModelGemini31FlashLite)
		}
	}
	if retired.calls != 1 {
		t.Errorf("retired.calls = %d, want 1（提供終了が判明したモデルへは再送しない）", retired.calls)
	}
}

// 分あたり（RPM）超過の 429 は待てば復活するので、打ち止め扱いにしないこと。
func TestFallbackGeminiClient_MinuteQuotaErrorDoesNotMarkExhausted(t *testing.T) {
	flash := &fakeGeminiClient{modelName: ModelGemini38Flash, errs: []error{minuteQuotaErr()}}
	lite := &fakeGeminiClient{modelName: ModelGemini31FlashLite}

	c := newFallbackGeminiClientFromChain(ModelGemini38Flash,
		[]geminiChainClient{flash, lite}, RateLimitsFreeAPIKey)
	c.dailyQuotas = DailyQuotas{ModelGemini38Flash: 20, ModelGemini31FlashLite: 500}

	if _, err := c.CreateChatCompletionWithTokens(context.Background(), &ChatRequest{Prompt: "x"}); err != nil {
		t.Fatalf("1回目: %v", err)
	}
	resp, err := c.CreateChatCompletionWithTokens(context.Background(), &ChatRequest{Prompt: "x"})
	if err != nil {
		t.Fatalf("2回目: %v", err)
	}
	if resp.ActualModel != string(ModelGemini38Flash) {
		t.Errorf("2回目の ActualModel = %q, want %q（RPM 超過で希少な上位枠を捨てない）", resp.ActualModel, ModelGemini38Flash)
	}
	if flash.calls != 2 {
		t.Errorf("flash.calls = %d, want 2", flash.calls)
	}
}

// RateLimitsFreeAPIKey に必要なモデルが登録されていること。
func TestRateLimitsFreeAPIKey_HasRequiredModels(t *testing.T) {
	required := []GeminiModel{
		ModelGemini38Flash,
		ModelGemini36Flash,
		ModelGemini3FlashPreview,
		ModelGemini35FlashLite,
		ModelGemini31FlashLite,
	}
	for _, m := range required {
		if _, ok := RateLimitsFreeAPIKey[m]; !ok {
			t.Errorf("RateLimitsFreeAPIKey missing %s", m)
		}
	}
}

// 同一 API キーで作った rateLimiter が singleton として共有されること。
func TestGetRateLimiter_SharedByAPIKey(t *testing.T) {
	a1 := getRateLimiter("test-key-shared")
	a2 := getRateLimiter("test-key-shared")
	if a1 != a2 {
		t.Error("expected same rateLimiter instance for same API key")
	}
	b := getRateLimiter("test-key-other")
	if a1 == b {
		t.Error("expected different rateLimiter instance for different API key")
	}
}

// rate limiter による直列化テスト: 同一 API キー + 同一モデルへ並列に Acquire すると
// それぞれの呼び出しは minInterval 以上空けて返ること。
func TestRateLimiter_SerializesSameModel(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping rate-limit serialization test in short mode")
	}
	rl := newRateLimiter()
	interval := 50 * time.Millisecond

	var wg sync.WaitGroup
	timestamps := make([]time.Time, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			if err := rl.Acquire(context.Background(), ModelGemini31FlashLite, interval, true); err != nil {
				t.Errorf("Acquire failed: %v", err)
				return
			}
			timestamps[idx] = time.Now()
		}(i)
	}
	wg.Wait()

	// timestamps をソートして連続差分を見る
	sorted := append([]time.Time{}, timestamps...)
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j].Before(sorted[i]) {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	for i := 1; i < len(sorted); i++ {
		gap := sorted[i].Sub(sorted[i-1])
		if gap < time.Duration(float64(interval)*0.9) {
			t.Errorf("gap[%d-%d] = %v, want >= ~%v (serialization failed)", i-1, i, gap, interval)
		}
	}
}

// 異なるモデルは並列に進める（同一モデルだけ直列化する）こと。
func TestRateLimiter_DifferentModelsParallel(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping rate-limit parallel test in short mode")
	}
	rl := newRateLimiter()
	interval := 100 * time.Millisecond

	// 同一モデルで一度 Acquire し、そのモデルの最後に呼んだ時刻を埋める
	if err := rl.Acquire(context.Background(), ModelGemini36Flash, interval, true); err != nil {
		t.Fatal(err)
	}
	if err := rl.Acquire(context.Background(), ModelGemini31FlashLite, interval, true); err != nil {
		t.Fatal(err)
	}

	// 別モデルなら待たずに通る（互いの interval 待ちを引き継がない）こと
	start := time.Now()
	if err := rl.Acquire(context.Background(), ModelGemini3FlashPreview, interval, true); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if elapsed > 20*time.Millisecond {
		t.Errorf("different-model Acquire should be immediate, but elapsed=%v", elapsed)
	}
}

// context cancel で待機が中断されること。
func TestRateLimiter_WaitCancelledByContext(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping rate-limit cancel test in short mode")
	}
	rl := newRateLimiter()
	if err := rl.Acquire(context.Background(), ModelGemini36Flash, 5*time.Second, true); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := rl.Acquire(ctx, ModelGemini36Flash, 5*time.Second, true)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected context error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("expected timely cancellation, but elapsed=%v", elapsed)
	}
}

// チェイン生成失敗のケース（空 chain、API キー無し、無応答の打ち切り時間なし）。
// 打ち切り時間は用途ごとに長さが違うので既定値を持たせず、渡し忘れを構築時に止める。
func TestNewFallbackGeminiClient_InvalidConfig(t *testing.T) {
	if _, err := NewFallbackGeminiClient(context.Background(), FallbackConfig{
		Chain:       []GeminiModel{ModelGemini36Flash},
		APIKey:      "",
		CallTimeout: time.Minute,
	}); err == nil {
		t.Error("expected error for empty API key")
	} else if !strings.Contains(err.Error(), "API key") {
		t.Errorf("error message = %q, want to contain 'API key'", err.Error())
	}

	if _, err := NewFallbackGeminiClient(context.Background(), FallbackConfig{
		Chain:       nil,
		APIKey:      "dummy-key",
		CallTimeout: time.Minute,
	}); err == nil {
		t.Error("expected error for empty chain")
	} else if !strings.Contains(err.Error(), "chain") {
		t.Errorf("error message = %q, want to contain 'chain'", err.Error())
	}

	if _, err := NewFallbackGeminiClient(context.Background(), FallbackConfig{
		Chain:  []GeminiModel{ModelGemini36Flash},
		APIKey: "dummy-key",
	}); err == nil {
		t.Error("expected error for missing CallTimeout")
	} else if !strings.Contains(err.Error(), "CallTimeout") {
		t.Errorf("error message = %q, want to contain 'CallTimeout'", err.Error())
	}
}
