package llm

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// dailyQuotaErr は QuotaFailure（日次超過）を Details に積んだ ResourceExhausted エラーを返す。
func dailyQuotaErr() error {
	st := status.New(codes.ResourceExhausted, "RESOURCE_EXHAUSTED: quota exceeded")
	withDetails, err := st.WithDetails(&errdetails.QuotaFailure{
		Violations: []*errdetails.QuotaFailure_Violation{{
			Subject:     "project:1234",
			Description: "Quota exceeded for quota metric 'Generate requests' and limit 'GenerateRequestsPerDayPerProjectPerModel-FreeTier'",
		}},
	})
	if err != nil {
		panic(err)
	}
	return withDetails.Err()
}

// minuteQuotaErr は分あたり（RPM）超過の 429。日次打ち止め扱いにしてはいけないケース。
func minuteQuotaErr() error {
	st := status.New(codes.ResourceExhausted, "RESOURCE_EXHAUSTED: quota exceeded")
	withDetails, err := st.WithDetails(&errdetails.QuotaFailure{
		Violations: []*errdetails.QuotaFailure_Violation{{
			Subject:     "project:1234",
			Description: "Quota exceeded for quota metric 'Generate requests' and limit 'GenerateRequestsPerMinutePerProjectPerModel-FreeTier'",
		}},
	})
	if err != nil {
		panic(err)
	}
	return withDetails.Err()
}

func TestDailyQuotaTracker_ReserveStopsAtLimit(t *testing.T) {
	tr := newDailyQuotaTracker()

	for i := 0; i < 3; i++ {
		if !tr.Reserve(ModelGemini38Flash, 3) {
			t.Fatalf("Reserve #%d = false, want true（上限内）", i+1)
		}
	}
	if tr.Reserve(ModelGemini38Flash, 3) {
		t.Error("Reserve #4 = true, want false（上限到達後は送信させない）")
	}
	if got := tr.Used(ModelGemini38Flash); got != 3 {
		t.Errorf("Used = %d, want 3（拒否した分はカウントしない）", got)
	}

	// 別モデルの枠は独立している。
	if !tr.Reserve(ModelGemini31FlashLite, 3) {
		t.Error("別モデルの Reserve = false, want true")
	}
}

// limit 0（DailyQuotas 未設定）は上限なし扱い＝429 を受けてからのフォールバックに委ねる。
func TestDailyQuotaTracker_NoLimitMeansUnlimited(t *testing.T) {
	tr := newDailyQuotaTracker()
	for i := 0; i < 50; i++ {
		if !tr.Reserve(ModelGemini36Flash, 0) {
			t.Fatalf("Reserve #%d = false, want true（上限なし）", i+1)
		}
	}
}

func TestDailyQuotaTracker_MarkExhaustedBlocksImmediately(t *testing.T) {
	tr := newDailyQuotaTracker()

	if !tr.Reserve(ModelGemini38Flash, 20) {
		t.Fatal("Reserve = false, want true")
	}
	tr.MarkExhausted(ModelGemini38Flash)

	if tr.Reserve(ModelGemini38Flash, 20) {
		t.Error("Reserve = true, want false（Google 側が枯渇を明示したモデルは以後投げない）")
	}
	if !tr.Reserve(ModelGemini36Flash, 20) {
		t.Error("他モデルまで巻き込んで遮断している")
	}
}

// RPD のリセットは PT 深夜0時。日付が変わればカウンタも打ち止めフラグも捨てる。
func TestDailyQuotaTracker_RotatesAtPacificMidnight(t *testing.T) {
	now := time.Date(2026, 8, 15, 15, 0, 0, 0, time.FixedZone("JST", 9*60*60)) // PT では 8/14 23:00
	tr := newDailyQuotaTracker()
	tr.now = func() time.Time { return now }

	if !tr.Reserve(ModelGemini38Flash, 1) {
		t.Fatal("Reserve = false, want true")
	}
	tr.MarkExhausted(ModelGemini36Flash)
	if tr.Reserve(ModelGemini38Flash, 1) {
		t.Fatal("Reserve = true, want false（同一 PT 日内は上限が効く）")
	}

	now = now.Add(2 * time.Hour) // JST 17時 ＝ PT 8/15 01:00（リセット後）

	if got := tr.Used(ModelGemini38Flash); got != 0 {
		t.Errorf("Used = %d, want 0（PT 日が変わればカウンタは捨てる）", got)
	}
	if !tr.Reserve(ModelGemini38Flash, 1) {
		t.Error("Reserve = false, want true（リセット後は再び使える）")
	}
	if !tr.Reserve(ModelGemini36Flash, 1) {
		t.Error("MarkExhausted の打ち止めが翌 RPD 日へ持ち越されている")
	}
}

func TestDailyQuotaTracker_SeedAppliesOnlyOnce(t *testing.T) {
	tr := newDailyQuotaTracker()
	calls := 0
	seed := func(context.Context) (map[GeminiModel]int, error) {
		calls++
		return map[GeminiModel]int{ModelGemini38Flash: 18}, nil
	}

	for i := 0; i < 3; i++ {
		if err := tr.Seed(context.Background(), seed); err != nil {
			t.Fatalf("Seed: %v", err)
		}
	}
	if calls != 1 {
		t.Errorf("seed 呼び出し = %d回, want 1回（クライアントを何本作っても取り込みは1回）", calls)
	}
	if got := tr.Used(ModelGemini38Flash); got != 18 {
		t.Errorf("Used = %d, want 18", got)
	}

	// 残り2回で上限 20 に達する。
	if !tr.Reserve(ModelGemini38Flash, 20) || !tr.Reserve(ModelGemini38Flash, 20) {
		t.Fatal("残枠 2 を使えていない")
	}
	if tr.Reserve(ModelGemini38Flash, 20) {
		t.Error("Reserve = true, want false（seed 分を含めて上限判定する）")
	}
}

func TestDailyQuotaTracker_SeedError(t *testing.T) {
	tr := newDailyQuotaTracker()
	wantErr := errors.New("db down")
	seed := func(context.Context) (map[GeminiModel]int, error) { return nil, wantErr }

	if err := tr.Seed(context.Background(), seed); !errors.Is(err, wantErr) {
		t.Errorf("Seed error = %v, want %v（握りつぶさない）", err, wantErr)
	}
	if err := tr.Seed(context.Background(), nil); err != nil {
		t.Errorf("Seed(nil) = %v, want nil", err)
	}
}

// QuotaDayStart は PT 深夜0時を返す。JST 16時ごろが境界になる。
func TestQuotaDayStart(t *testing.T) {
	jst := time.FixedZone("JST", 9*60*60)

	beforeReset := time.Date(2026, 8, 15, 15, 0, 0, 0, jst)
	afterReset := time.Date(2026, 8, 15, 17, 0, 0, 0, jst)

	got1 := QuotaDayStart(beforeReset)
	got2 := QuotaDayStart(afterReset)

	if !got2.After(got1) {
		t.Fatalf("JST 15時と17時で RPD 日が切り替わっていない: %v / %v", got1, got2)
	}
	if d := got2.Sub(got1); d != 24*time.Hour {
		t.Errorf("RPD 日の間隔 = %v, want 24h", d)
	}
	if h := got2.In(geminiQuotaResetLocation).Hour(); h != 0 {
		t.Errorf("PT での時刻 = %d時, want 0時", h)
	}
	if !beforeReset.Before(got2) || !afterReset.After(got2) {
		t.Errorf("境界が JST 15〜17時の間に無い: boundary=%v", got2.In(jst))
	}
}

func TestIsDailyQuotaExceeded(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"QuotaFailure に PerDay", dailyQuotaErr(), true},
		{"QuotaFailure に PerMinute", minuteQuotaErr(), false},
		{"Details 無しの 429", quotaErr(), false},
		// 実測の RPM 超過は violation が空で retry_delay だけが手掛かりになる（41〜56秒）。
		{"retry_delay 41秒（RPM 相当）", quotaErrWithRetryDelay(41 * time.Second), false},
		{"retry_delay 5分（RPM の窓を超える）", quotaErrWithRetryDelay(5 * time.Minute), true},
		{"文字列に PerDay", errors.New("429: GenerateRequestsPerDayPerProjectPerModel-FreeTier"), true},
		{"文字列に daily limit", errors.New("Daily Limit for read operations exceeded"), true},
		{"wrap された日次超過", fmt.Errorf("Gemini API error: %w", dailyQuotaErr()), true},
		{"無関係なエラー", errors.New("network error"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isDailyQuotaExceeded(tt.err); got != tt.want {
				t.Errorf("isDailyQuotaExceeded(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// 同一 API キーで作った dailyQuotaTracker が singleton として共有されること。
func TestGetQuotaTracker_SharedByAPIKey(t *testing.T) {
	a1 := getQuotaTracker("quota-key-shared")
	a2 := getQuotaTracker("quota-key-shared")
	if a1 != a2 {
		t.Error("同一 API キーで別インスタンスが返っている（RPD プールを多重消費する）")
	}
	if a1 == getQuotaTracker("quota-key-other") {
		t.Error("別 API キーで同一インスタンスが返っている（枠はキーごとに独立）")
	}
}

type fakeUsageRepository struct {
	usage     map[string]int
	err       error
	gotSince  time.Time
	gotPrefix string
}

func (f *fakeUsageRepository) GetModelUsageSince(_ context.Context, since time.Time, featurePrefix string) (map[string]int, error) {
	f.gotSince = since
	f.gotPrefix = featurePrefix
	if f.err != nil {
		return nil, f.err
	}
	return f.usage, nil
}

func TestNewDBQuotaSeed(t *testing.T) {
	repo := &fakeUsageRepository{usage: map[string]int{
		string(ModelGemini38Flash):     12,
		string(ModelGemini31FlashLite): 130,
	}}

	got, err := NewDBQuotaSeed(repo, "race-")(context.Background())
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got[ModelGemini38Flash] != 12 || got[ModelGemini31FlashLite] != 130 {
		t.Errorf("usage = %v, want 3.8=12 / 3.1 lite=130", got)
	}
	if repo.gotPrefix != "race-" {
		t.Errorf("featurePrefix = %q, want %q", repo.gotPrefix, "race-")
	}
	if want := QuotaDayStart(time.Now()); !repo.gotSince.Equal(want) {
		t.Errorf("since = %v, want %v（PT 深夜0時起点で数える）", repo.gotSince, want)
	}

	failing := &fakeUsageRepository{err: errors.New("db down")}
	if _, err := NewDBQuotaSeed(failing, "")(context.Background()); err == nil {
		t.Error("リポジトリのエラーが握りつぶされている")
	}
}

// DailyQuotasFreeAPIKey に運用中のモデルが揃っていること。
// 抜けがあると上限なし扱いになり、事前切替が効かず 429 を踏み続ける。
func TestDailyQuotasFreeAPIKey_HasRequiredModels(t *testing.T) {
	required := []GeminiModel{
		ModelGemini38Flash,
		ModelGemini36Flash,
		ModelGemini3FlashPreview,
		ModelGemini35FlashLite,
		ModelGemini31FlashLite,
	}
	for _, m := range required {
		limit, ok := DailyQuotasFreeAPIKey[m]
		if !ok {
			t.Errorf("DailyQuotasFreeAPIKey に %s が無い", m)
			continue
		}
		if limit <= 0 {
			t.Errorf("%s の上限 = %d, want > 0", m, limit)
		}
	}
}

// perPrefixUsageRepository は func_feature ごとに別々の実績を返す。
type perPrefixUsageRepository struct {
	usage       map[string]map[string]int
	gotPrefixes []string
}

func (f *perPrefixUsageRepository) GetModelUsageSince(_ context.Context, _ time.Time, featurePrefix string) (map[string]int, error) {
	f.gotPrefixes = append(f.gotPrefixes, featurePrefix)
	return f.usage[featurePrefix], nil
}

func TestNewDBQuotaSeed_同じキーを使う機能の消費を合算する(t *testing.T) {
	repo := &perPrefixUsageRepository{usage: map[string]map[string]int{
		"article": {
			string(ModelGemini38Flash):     8,
			string(ModelGemini31FlashLite): 20,
		},
		"recommend": {
			string(ModelGemini31FlashLite): 5,
		},
		// 別キーの機能。並べていないので数えてはいけない。
		"reply": {
			string(ModelGemini31FlashLite): 100,
		},
	}}

	got, err := NewDBQuotaSeed(repo, "article", "recommend")(context.Background())
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	if got[ModelGemini38Flash] != 8 {
		t.Errorf("3.8 Flash = %d, want 8", got[ModelGemini38Flash])
	}
	if got[ModelGemini31FlashLite] != 25 {
		t.Errorf("3.1 Flash Lite = %d, want 25（20+5。別キーの 100 は含めない）", got[ModelGemini31FlashLite])
	}
	if len(repo.gotPrefixes) != 2 {
		t.Errorf("問い合わせた func_feature = %v, want 2 件", repo.gotPrefixes)
	}
}

func TestNewDBQuotaSeed_リポジトリのエラーを返す(t *testing.T) {
	failing := &fakeUsageRepository{err: errors.New("db down")}
	if _, err := NewDBQuotaSeed(failing, "article")(context.Background()); err == nil {
		t.Error("リポジトリのエラーが握りつぶされている")
	}
}
