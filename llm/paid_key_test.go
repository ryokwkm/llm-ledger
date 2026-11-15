package llm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

var errTestExhausted = errors.New("429 RESOURCE_EXHAUSTED")

func isTestExhausted(err error) bool { return errors.Is(err, errTestExhausted) }

// keyRecorder は Call に渡す呼び出しの偽物。使われた API キーを順に残し、キーごとに決めたエラーを返す。
type keyRecorder struct {
	used []string
	errs map[string]error
}

func (r *keyRecorder) call(_ context.Context, apiKey string) error {
	r.used = append(r.used, apiKey)
	return r.errs[apiKey]
}

func TestPaidModelName(t *testing.T) {
	if got := PaidModelName(string(ModelGemini38Flash)); got != "paid:gemini-3.8-flash" {
		t.Errorf("PaidModelName = %q, want %q", got, "paid:gemini-3.8-flash")
	}
}

func TestNewPaidKeyFallback_InvalidConfig(t *testing.T) {
	if _, err := NewPaidKeyFallback(PaidKeyFallbackConfig{PaidKey: "paid", IsExhausted: isTestExhausted}); err == nil {
		t.Error("FreeKey が空なのに作れた")
	}
	if _, err := NewPaidKeyFallback(PaidKeyFallbackConfig{FreeKey: "free", PaidKey: "paid"}); err == nil {
		t.Error("PaidKey があるのに IsExhausted が無いまま作れた")
	}
}

func TestPaidKeyFallback_課金キーが無ければ無料キーだけで呼びエラーもそのまま返す(t *testing.T) {
	f, err := NewPaidKeyFallback(PaidKeyFallbackConfig{FreeKey: "free"})
	if err != nil {
		t.Fatalf("NewPaidKeyFallback: %v", err)
	}
	r := &keyRecorder{errs: map[string]error{"free": errTestExhausted}}

	paid, err := f.Call(context.Background(), "m", r.call)
	if !errors.Is(err, errTestExhausted) {
		t.Errorf("err = %v, want 無料キーのエラー", err)
	}
	if paid {
		t.Error("paid = true, want false")
	}
	if strings.Join(r.used, ",") != "free" {
		t.Errorf("使ったキー = %v, want [free]", r.used)
	}
}

func TestPaidKeyFallback_無料キーで成功したら課金キーを使わない(t *testing.T) {
	f := mustPaidKeyFallback(t, 0)
	r := &keyRecorder{}

	paid, err := f.Call(context.Background(), "m", r.call)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if paid {
		t.Error("paid = true, want false")
	}
	if strings.Join(r.used, ",") != "free" {
		t.Errorf("使ったキー = %v, want [free]", r.used)
	}
}

func TestPaidKeyFallback_無料キーが枠切れなら課金キーで呼び直す(t *testing.T) {
	f := mustPaidKeyFallback(t, 0)
	r := &keyRecorder{errs: map[string]error{"free": errTestExhausted}}

	paid, err := f.Call(context.Background(), "m", r.call)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !paid {
		t.Error("paid = false, want true")
	}
	if strings.Join(r.used, ",") != "free,paid" {
		t.Errorf("使ったキー = %v, want [free paid]", r.used)
	}
}

func TestPaidKeyFallback_枠切れ以外のエラーでは課金キーへ行かない(t *testing.T) {
	f := mustPaidKeyFallback(t, time.Hour)
	netErr := errors.New("network unreachable")
	r := &keyRecorder{errs: map[string]error{"free": netErr}}

	paid, err := f.Call(context.Background(), "m", r.call)
	if !errors.Is(err, netErr) {
		t.Errorf("err = %v, want %v", err, netErr)
	}
	if paid {
		t.Error("paid = true, want false")
	}
	if strings.Join(r.used, ",") != "free" {
		t.Errorf("使ったキー = %v, want [free]", r.used)
	}
}

func TestPaidKeyFallback_課金キーの失敗はそのまま返す(t *testing.T) {
	f := mustPaidKeyFallback(t, 0)
	paidErr := errors.New("billing disabled")
	r := &keyRecorder{errs: map[string]error{"free": errTestExhausted, "paid": paidErr}}

	paid, err := f.Call(context.Background(), "m", r.call)
	if !errors.Is(err, paidErr) {
		t.Errorf("err = %v, want 課金キーのエラー", err)
	}
	if !paid {
		t.Error("paid = false, want true（課金キーで送った）")
	}
}

// Cooldown の間は、枠切れを受けたモデルだけ無料キーを飛ばす。Gemini の枠はモデルごとなので、別のモデルは無料キーから試す。
func TestPaidKeyFallback_Cooldownの間は枠切れのモデルだけ無料キーを飛ばす(t *testing.T) {
	f := mustPaidKeyFallback(t, 30*time.Minute)
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	f.now = func() time.Time { return now }
	r := &keyRecorder{errs: map[string]error{"free": errTestExhausted}}

	if _, err := f.Call(context.Background(), "flash", r.call); err != nil {
		t.Fatalf("1 回目: %v", err)
	}
	now = now.Add(29 * time.Minute)
	paid, err := f.Call(context.Background(), "flash", r.call)
	if err != nil || !paid {
		t.Fatalf("2 回目: paid=%v err=%v, want 課金キーで成功", paid, err)
	}
	if got := strings.Join(r.used, ","); got != "free,paid,paid" {
		t.Errorf("使ったキー = %s, want free,paid,paid（2 回目は無料キーを飛ばす）", got)
	}

	r.used = nil
	if _, err := f.Call(context.Background(), "embedding", r.call); err != nil {
		t.Fatalf("別のモデル: %v", err)
	}
	if got := strings.Join(r.used, ","); got != "free,paid" {
		t.Errorf("別のモデルで使ったキー = %s, want free,paid（別のモデルは無料キーから試す）", got)
	}

	r.used = nil
	now = now.Add(time.Minute)
	if _, err := f.Call(context.Background(), "flash", r.call); err != nil {
		t.Fatalf("Cooldown の後: %v", err)
	}
	if got := strings.Join(r.used, ","); got != "free,paid" {
		t.Errorf("Cooldown の後に使ったキー = %s, want free,paid（無料キーを試し直す）", got)
	}
}

func TestPaidKeyFallback_Cooldownが0なら毎回無料キーから試す(t *testing.T) {
	f := mustPaidKeyFallback(t, 0)
	r := &keyRecorder{errs: map[string]error{"free": errTestExhausted}}

	for i := 0; i < 2; i++ {
		if _, err := f.Call(context.Background(), "flash", r.call); err != nil {
			t.Fatalf("#%d: %v", i+1, err)
		}
	}
	if got := strings.Join(r.used, ","); got != "free,paid,free,paid" {
		t.Errorf("使ったキー = %s, want free,paid,free,paid", got)
	}
}

func mustPaidKeyFallback(t *testing.T, cooldown time.Duration) *PaidKeyFallback {
	t.Helper()
	f, err := NewPaidKeyFallback(PaidKeyFallbackConfig{
		FreeKey:     "free",
		PaidKey:     "paid",
		IsExhausted: isTestExhausted,
		Cooldown:    cooldown,
	})
	if err != nil {
		t.Fatalf("NewPaidKeyFallback: %v", err)
	}
	return f
}
