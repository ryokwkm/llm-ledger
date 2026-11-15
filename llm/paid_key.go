// 課金キーへの切り替え。
//
// 無料キーの回数の枠（1 日・1 分）を使い切ったとき、課金した API キーで同じ要求を送り直して処理を止めない。
// 入口は 2 つで、切り替えの条件と記録の印は同じ:
//   - FallbackConfig.PaidAPIKey: FallbackGeminiClient（genai）の無料キーの chain が全段とも枠切れのとき、課金キーの chain で送る
//   - PaidKeyFallback: 通信の方式を問わない部品。自前の REST クライアント（埋め込みを含む）のように、呼び出しごとに API キーを渡せるもの向け
//
// 課金キーで呼んだ記録は llm_logs.model_name に印を付けて残す（PaidModelName）。llm_logs にはカラムを足さない決まりなので、既存のカラムに載せる。
// 印の付いた名前は無料キーのモデル名と一致しないので、NewDBQuotaSeed は課金キーの呼び出しを無料の RPD に数えない。
// 一方 daily_limit（GetDailyUsageCount）は func_feature で数えるので課金キーの呼び出しも数え、そのまま課金の回数の上限になる。

package llm

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// PaidModelName は課金キーで呼んだ model を llm_logs.model_name に残す名前にする（例: paid:gemini-3.8-flash）。
// 課金キーの呼び出しを ChatResponse.ActualModel で返すときに使う。
func PaidModelName(model string) string {
	return "paid:" + model
}

// PaidKeyFallbackConfig は PaidKeyFallback の構築パラメータ。
type PaidKeyFallbackConfig struct {
	// FreeKey は先に使う無料キー。必須。
	FreeKey string
	// PaidKey は無料キーが枠切れのときに使う課金キー。空なら切り替えない（無料キーだけで呼び、エラーもそのまま返す）。
	PaidKey string
	// IsExhausted は無料キーで受けたエラーが回数の枠切れかを返す。PaidKey を渡すなら必須。
	// 通信の方式（REST の 429・gRPC の ResourceExhausted）でエラーの型が違うので、呼ぶ側が自分のエラーの型で判定する。
	IsExhausted func(error) bool
	// Cooldown は枠切れを受けたモデルで、無料キーを試し直すまでの間。0 なら毎回無料キーから試す。
	// 1 日の枠は日付が変わるまで戻らないので、送る前に枠切れを判定する仕組み（FallbackGeminiClient の RPD の数え）が無い呼び出しでは、
	// これが無いと課金キーへ切り替えた後も呼ぶたびに無料キーで 429 を 1 往復受ける。
	Cooldown time.Duration
}

// PaidKeyFallback は 1 回の呼び出しを無料キーで送り、回数の枠切れなら課金キーで送り直す。
// 無料キーの枠切れはモデルごとに覚える（Gemini の枠はモデルごとなので、生成の枠切れで埋め込みまで課金キーへ寄せない）。
// 状態を持つので、プロセスの間ずっと同じインスタンスを使う。呼び出しごとに作ると Cooldown が効かない。
type PaidKeyFallback struct {
	cfg PaidKeyFallbackConfig
	now func() time.Time

	mu               sync.Mutex
	freeBlockedUntil map[string]time.Time
}

func NewPaidKeyFallback(cfg PaidKeyFallbackConfig) (*PaidKeyFallback, error) {
	if cfg.FreeKey == "" {
		return nil, fmt.Errorf("FreeKey is required")
	}
	if cfg.PaidKey != "" && cfg.IsExhausted == nil {
		return nil, fmt.Errorf("IsExhausted is required when PaidKey is set")
	}
	return &PaidKeyFallback{
		cfg:              cfg,
		now:              time.Now,
		freeBlockedUntil: map[string]time.Time{},
	}, nil
}

// Call は call を無料キーで呼び、回数の枠切れなら同じ call を課金キーで呼び直す。最後に課金キーで呼んだら paid が true。
// model は枠切れを覚える単位で、Gemini に送るモデル名を渡す。
func (f *PaidKeyFallback) Call(ctx context.Context, model string, call func(ctx context.Context, apiKey string) error) (paid bool, err error) {
	if f.cfg.PaidKey == "" {
		return false, call(ctx, f.cfg.FreeKey)
	}
	if !f.freeBlocked(model) {
		err := call(ctx, f.cfg.FreeKey)
		if err == nil || !f.cfg.IsExhausted(err) {
			return false, err
		}
		f.blockFree(model)
	}
	return true, call(ctx, f.cfg.PaidKey)
}

func (f *PaidKeyFallback) freeBlocked(model string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now().Before(f.freeBlockedUntil[model])
}

func (f *PaidKeyFallback) blockFree(model string) {
	if f.cfg.Cooldown <= 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.freeBlockedUntil[model] = f.now().Add(f.cfg.Cooldown)
}
