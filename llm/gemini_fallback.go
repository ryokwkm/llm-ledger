// FallbackGeminiClient は「無料枠超過時の自動フォールバック付き Gemini クライアント」。
//
// 設計方針（共通ライブラリ化）:
//   - chain（試行順のモデル列）・RateLimits（モデル別最低インターバル）・DailyQuotas（モデル別 RPD）は
//     すべて呼び出し側注入。package 内に usecase 固有の chain 定数を持たない。
//   - rate limiter と日次クォータカウンタは API キー単位の singleton で共有。
//     同一 API キーから作った複数 FallbackGeminiClient（用途ごとに作ったもの）は
//     自然に直列化され、Gemini 無料枠の RPM / RPD プールを多重消費しない。
//   - 枠切れの捌き方は制限の性質で分ける:
//     RPD は投げる前に残数で判断してスキップ（daily_quota.go）。待っても当日は復活しないため。
//     RPM / TPM は 429 を受けてから下位モデルへフォールバックし、chain 全段が超過した場合は
//     Google API が返す google.rpc.RetryInfo の retry_delay を抽出して待機 → 先頭から再試行（cap 付き）。
//   - 無応答は 1 回の送信ごとに CallTimeout で打ち切り、下位モデルへフォールバックする。

package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// geminiChainClient は FallbackGeminiClient がチェイン要素として扱う最小契約。
// 本番では *GeminiClient、テストでは fake が満たす。
type geminiChainClient interface {
	InternalClient
	Close() error
}

// RateLimits はモデル別の最低リクエスト間隔。
// 同一 API キーで同じモデルへ連投するときに、この間隔だけ空けてから次を発射する。
// マップに存在しないモデルは間隔ゼロ（待機なし）扱い。
type RateLimits map[GeminiModel]time.Duration

// RateLimitsFreeAPIKey は Gemini 無料 API キーの RPM クォータに基づく最低インターバル。
// 値は「RPM から算出したインターバル + 1s 安全マージン」。
//
// 実クォータ（無料キーのコンソール確認値）:
//   - 3.8 / 3.6 Flash, 3 Flash(preview), 2.5 Flash: 5 RPM
//   - 3.5 / 3.1 Flash Lite: 15 RPM
//   - 2.5 Flash Lite: 10 RPM
//   - 2.5 Pro: 0/0（無料キーでは使えない。有料キーで使うときのために値は持つ）
//
// 日次側の上限は DailyQuotasFreeAPIKey（daily_quota.go）。
var RateLimitsFreeAPIKey = RateLimits{
	ModelGemini38Flash:       13 * time.Second,
	ModelGemini36Flash:       13 * time.Second,
	ModelGemini3FlashPreview: 13 * time.Second,
	ModelGemini25Flash:       13 * time.Second,
	ModelGemini25Pro:         13 * time.Second,
	ModelGemini35FlashLite:   5 * time.Second,
	ModelGemini31FlashLite:   5 * time.Second,
	ModelGemini25FlashLite:   7 * time.Second,
}

// FallbackConfig は FallbackGeminiClient の構築パラメータ。
// chain と RateLimits をすべて注入式で受け取り、package 内に usecase 固有の定数を残さない設計。
type FallbackConfig struct {
	// Chain は試行順のモデル列。先頭が起点（成功時は ChatResponse.ActualModel に詰める）、
	// 末尾が最終フォールバック先。空配列は構築エラー。
	Chain []GeminiModel
	// RateLimits はモデル別の最低リクエスト間隔。nil ならレート制御なし。
	// 同じモデルが Chain に複数回現れても、API キー × モデル単位で同一 limiter を共有する。
	RateLimits RateLimits
	// DailyQuotas はモデル別の1日あたりリクエスト上限（RPD）。nil なら日次の事前判定を行わず、
	// 429 を受けてからフォールバックする。
	DailyQuotas DailyQuotas
	// QuotaSeed は当 RPD 日にすでに消費した回数を外部から取り込むフック（nil 可）。
	// 常駐プロセスの再起動後も枠の消費実績を引き継ぐために使う。llm_logs から供給するなら
	// NewDBQuotaSeed を渡す。
	QuotaSeed QuotaSeed
	// APIKey は Gemini API キー。同一文字列の APIKey を持つ FallbackGeminiClient 間で
	// rate limiter と日次クォータカウンタが共有される（singleton レジストリにより）。
	APIKey string
	// CallTimeout は 1 モデルへの 1 回の送信を待つ上限で、超えたら下位モデルへフォールバックする。必須。
	// genai の既定は 600 秒で、無応答のまま張り付くと処理が止まり、その失敗が日次上限（llm_function_configs.daily_limit）の枠まで食う。
	// 正常な生成を切らない長さは用途で違う（長文の生成ほど長い）ので、呼び出し側が実測から決める。
	CallTimeout time.Duration
}

// FallbackGeminiClient は起点モデルから始め、無料枠超過・無応答時に下位モデルへ自動フォールバックする InternalClient 実装。
//
// 日次枠（RPD）を使い切ったモデルは送信前にスキップし、次のモデルへ落ちる。
// chain 全段が RPM/TPM 超過した場合は、Google API が返す google.rpc.RetryInfo の retry_delay を抽出し、
// 待機 → chain 先頭から再試行する。再試行は cap（試行回数 maxRetryAttempts / 総待機時間 maxTotalRetryWait）で抑える。
type FallbackGeminiClient struct {
	startModel  GeminiModel
	chain       []geminiChainClient
	rateLimits  RateLimits
	dailyQuotas DailyQuotas
	limiter     *rateLimiter
	quota       *dailyQuotaTracker
	waitEnabled bool
	callTimeout time.Duration
}

const (
	maxRetryAttempts  = 3
	maxTotalRetryWait = 30 * time.Second
)

// errCallTimedOut は 1 回の送信が CallTimeout 以内に返らなかったことを表す。
var errCallTimedOut = errors.New("Gemini が上限時間内に応答しませんでした")

// NewFallbackGeminiClient は FallbackConfig からチェインを構築する。
// 同じ APIKey で過去に構築された FallbackGeminiClient とは rate limiter を共有する。
func NewFallbackGeminiClient(ctx context.Context, cfg FallbackConfig) (*FallbackGeminiClient, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("API key is required")
	}
	if len(cfg.Chain) == 0 {
		return nil, fmt.Errorf("chain must not be empty")
	}
	if cfg.CallTimeout <= 0 {
		return nil, fmt.Errorf("CallTimeout is required")
	}

	chain := make([]geminiChainClient, 0, len(cfg.Chain))
	for _, m := range cfg.Chain {
		gc, err := NewGeminiClientWithAPIKey(ctx, m, cfg.APIKey)
		if err != nil {
			closeChain(chain)
			return nil, fmt.Errorf("create gemini client for %s: %w", m, err)
		}
		chain = append(chain, gc)
	}

	quota := getQuotaTracker(cfg.APIKey)
	if err := quota.Seed(ctx, cfg.QuotaSeed); err != nil {
		closeChain(chain)
		return nil, fmt.Errorf("日次クォータの消費実績の取り込みに失敗: %w", err)
	}

	return &FallbackGeminiClient{
		startModel:  cfg.Chain[0],
		chain:       chain,
		rateLimits:  cfg.RateLimits,
		dailyQuotas: cfg.DailyQuotas,
		limiter:     getRateLimiter(cfg.APIKey),
		quota:       quota,
		waitEnabled: true,
		callTimeout: cfg.CallTimeout,
	}, nil
}

// closeChain は構築途中で失敗したとき、作成済みの client を閉じる。後始末なので Close の失敗は捨てる。
func closeChain(chain []geminiChainClient) {
	for _, gc := range chain {
		_ = gc.Close()
	}
}

// newFallbackGeminiClientFromChain はテスト用の constructor。本番では NewFallbackGeminiClient を使う。
// 既定で waitEnabled=false / limiter・quota tracker はテスト専用のローカルインスタンス（singleton から切り離す）。
// 実時間 sleep を伴う挙動を検証するテストだけ個別に waitEnabled=true へ切り替える。
// 日次クォータを検証するテストは dailyQuotas フィールドを直接差し込む。
func newFallbackGeminiClientFromChain(startModel GeminiModel, chain []geminiChainClient, rateLimits RateLimits) *FallbackGeminiClient {
	return &FallbackGeminiClient{
		startModel:  startModel,
		chain:       chain,
		rateLimits:  rateLimits,
		limiter:     newRateLimiter(),
		quota:       newDailyQuotaTracker(),
		waitEnabled: false,
		callTimeout: time.Minute,
	}
}

// ModelName は起点モデル名を返す。実際に成功したモデル名は ChatResponse.ActualModel に詰める。
func (c *FallbackGeminiClient) ModelName() string {
	return string(c.startModel)
}

func (c *FallbackGeminiClient) Close() error {
	var errs []error
	for _, gc := range c.chain {
		if err := gc.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// CreateChatCompletionWithTokens はチェインを順に試行し、枠切れ・無応答なら次モデルへフォールバックする。
//
// 日次枠（RPD）を使い切ったモデルは送信せずにスキップする（429 を踏む前の切替）。
// 送信して RPM/TPM 超過の 429 を受けた場合や、CallTimeout 以内に応答が無かった場合は、
// そのまま次モデルへ落ちる。chain 全段が超過したら
// Google API の retry_delay を抽出して待機 → 先頭から再試行し、(maxRetryAttempts, maxTotalRetryWait) で打ち切る。
// chain 全段が日次枠切れで1回も送信できなかった場合は、待っても当日は復活しないため即座に失敗させる。
//
// 各モデル呼び出し前に、API キー × モデル単位の rate limiter を取得して必要分待機する。
// 同一 API キーを共有する他 FallbackGeminiClient と自然に直列化される。
func (c *FallbackGeminiClient) CreateChatCompletionWithTokens(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
	var totalWait time.Duration

	for attempt := 0; attempt < maxRetryAttempts; attempt++ {
		round, err := c.tryChain(ctx, req)
		if err != nil {
			return nil, err
		}
		if round.resp != nil {
			return round.resp, nil
		}
		lastErr := round.lastErr

		if lastErr == nil {
			// 1回も送信していない ＝ chain 全段が日次枠切れ。待機・再試行しても当日は復活しない。
			return nil, fmt.Errorf("%w (start=%s, skipped=%s)", ErrDailyQuotaExhausted, c.startModel, strings.Join(round.skipped, ","))
		}

		// chain 全段が無料枠超過。retry_delay が取れれば待って再試行、取れなければ最終失敗。
		delay, ok := extractRetryDelay(lastErr)
		if !ok || delay <= 0 {
			return nil, fmt.Errorf("all gemini models in fallback chain exhausted (start=%s): %w", c.startModel, lastErr)
		}
		if totalWait+delay > maxTotalRetryWait {
			return nil, fmt.Errorf("retry budget exhausted after %v wait (start=%s): %w", totalWait, c.startModel, lastErr)
		}
		// 最後の試行の後は再試行しないので待たない。
		if c.waitEnabled && attempt < maxRetryAttempts-1 {
			if err := sleepContext(ctx, delay); err != nil {
				return nil, err
			}
		}
		totalWait += delay
	}
	return nil, fmt.Errorf("all retry attempts exhausted (start=%s)", c.startModel)
}

// chainRound は chain を先頭から 1 周した結果。resp が非 nil なら成功。
// resp が nil で lastErr も nil なら、1回も送信できなかった（全段が日次枠切れ）。
type chainRound struct {
	resp    *ChatResponse
	lastErr error    // 最後に受けた無料枠超過か無応答のエラー
	skipped []string // 日次枠切れで送信しなかったモデル
}

// tryChain は chain を先頭から順に試し、無料枠超過・提供終了・無応答なら次のモデルへ落ちる。
// それ以外のエラーと rate limiter の待機中断は、フォールバックせずそのまま返す。
func (c *FallbackGeminiClient) tryChain(ctx context.Context, req *ChatRequest) (chainRound, error) {
	var round chainRound
	for _, gc := range c.chain {
		model := GeminiModel(gc.ModelName())
		if !c.quota.Reserve(model, c.dailyQuotas[model]) {
			// 日次枠切れ。投げれば確実に 429 なので送信せず次モデルへ。
			round.skipped = append(round.skipped, string(model))
			continue
		}
		if err := c.limiter.Acquire(ctx, model, c.rateLimits[model], c.waitEnabled); err != nil {
			return chainRound{}, err
		}
		resp, err := c.callWithTimeout(ctx, gc, req)
		if err == nil {
			return chainRound{resp: resp}, nil
		}
		switch {
		case errors.Is(err, errCallTimedOut):
			// 無応答は Gemini 側の一時的な不調で、同じモデルへ投げ直しても張り付き続けることがあるため次モデルへ。
		case isModelRetired(err):
			// モデルが提供を終えた（preview は予告なく止まりうる）。投げても 404 なので、日次枠切れと同じく当日は投げない。
			c.quota.MarkExhausted(model)
		case isFreeQuotaExceeded(err):
			if isDailyQuotaExceeded(err) {
				// 想定 RPD より早く枯れた（他プロセスとの枠共有・失敗分の消費）。以後は投げずにスキップする。
				c.quota.MarkExhausted(model)
			}
		default:
			// それ以外のエラーは即時返却（フォールバックしない、再試行もしない）。
			return chainRound{}, err
		}
		round.lastErr = err
	}
	return round, nil
}

// callWithTimeout は 1 モデルへ 1 回送信し、callTimeout 以内に返らなければ errCallTimedOut で包んで返す。
// 呼び出し元の ctx が先に終わった（停止シグナル等）ときは無応答と区別し、そのままのエラーを返す。
func (c *FallbackGeminiClient) callWithTimeout(ctx context.Context, gc geminiChainClient, req *ChatRequest) (*ChatResponse, error) {
	callCtx, cancel := context.WithTimeout(ctx, c.callTimeout)
	defer cancel()

	resp, err := gc.CreateChatCompletionWithTokens(callCtx, req)
	if err != nil && ctx.Err() == nil && errors.Is(callCtx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("%w（%s・%v）: %w", errCallTimedOut, gc.ModelName(), c.callTimeout, err)
	}
	return resp, err
}

// sleepContext は d だけ待つ。待機中に ctx が終われば、その時点で ctx.Err() を返す。
func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		timer.Stop()
		return ctx.Err()
	}
}

// isFreeQuotaExceeded は Gemini 無料枠（RPM/RPD/TPM）超過に該当するエラーかを判定する。
// gRPC ステータスの ResourceExhausted を一次判定とし、文字列マッチで保険を掛ける。
func isFreeQuotaExceeded(err error) bool {
	if err == nil {
		return false
	}
	if st, ok := status.FromError(err); ok && st.Code() == codes.ResourceExhausted {
		return true
	}
	return strings.Contains(err.Error(), "RESOURCE_EXHAUSTED")
}

// isModelRetired はモデル ID が提供を終えた（404 NOT_FOUND）エラーかを判定する。
// 本番の REST の 404 は gax が NotFound の status を持つ APIError へ包むので、gRPC ステータスで判定できる。
func isModelRetired(err error) bool {
	return status.Code(err) == codes.NotFound
}

// extractRetryDelay は Google API エラーから google.rpc.RetryInfo の retry_delay を取り出す。
// gRPC status の Details に RetryInfo が積まれているケース、生 error の Unwrap 連鎖の
// どこかに gRPC status が現れるケースの両方をサポートする。
func extractRetryDelay(err error) (time.Duration, bool) {
	for _, d := range statusDetails(err) {
		if ri, ok := d.(*errdetails.RetryInfo); ok && ri.RetryDelay != nil {
			return ri.RetryDelay.AsDuration(), true
		}
	}
	return 0, false
}

// statusDetails は err の Unwrap 連鎖に現れる gRPC status の Details を、外側から順に集めて返す。
func statusDetails(err error) []any {
	var details []any
	for cur := err; cur != nil; cur = errors.Unwrap(cur) {
		if st, ok := status.FromError(cur); ok {
			details = append(details, st.Details()...)
		}
	}
	return details
}

// --- rate limiter (API キー × モデル単位の singleton) ---

// rateLimiter は API キー単位のリクエスト間隔制御。
// モデルごとに別ロックを持ち、同じモデルへの呼び出しは直列化され、
// 異なるモデルへの呼び出しは並列に進める。
type rateLimiter struct {
	models sync.Map // map[GeminiModel]*modelLimit
}

// modelLimit は 1 モデル分の直列化ロックと、最後に呼んだ時刻。
type modelLimit struct {
	mu   sync.Mutex
	last time.Time
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{}
}

func (r *rateLimiter) modelLimit(m GeminiModel) *modelLimit {
	v, _ := r.models.LoadOrStore(m, &modelLimit{})
	return v.(*modelLimit)
}

// Acquire は model の最低インターバル分だけ前回呼び出しから時間が経過するまで待つ。
// 待機完了時刻を即 last に記録し、その状態のまま呼び出し元へ制御を返す（ロックは解放される）。
//
// minInterval==0 / waitEnabled==false の場合は待機をスキップするが、last の更新だけは行う。
// （厳密な「現在時刻 = 直前呼び出し」を必要とする後続テストのためではなく、再試行と共有 limiter の整合のため。）
func (r *rateLimiter) Acquire(ctx context.Context, model GeminiModel, minInterval time.Duration, waitEnabled bool) error {
	ml := r.modelLimit(model)
	ml.mu.Lock()
	defer ml.mu.Unlock()

	if waitEnabled && minInterval > 0 && !ml.last.IsZero() {
		wait := minInterval - time.Since(ml.last)
		if wait > 0 {
			if err := sleepContext(ctx, wait); err != nil {
				return err
			}
		}
	}
	ml.last = time.Now()
	return nil
}

// rateLimiterRegistry は API キーごとの rateLimiter を保持する process-wide singleton。
// 同じ API キーから複数の FallbackGeminiClient を構築すると、内部 limiter がここを介して共有される。
var rateLimiterRegistry sync.Map // map[string]*rateLimiter

func getRateLimiter(apiKey string) *rateLimiter {
	if v, ok := rateLimiterRegistry.Load(apiKey); ok {
		return v.(*rateLimiter)
	}
	rl := newRateLimiter()
	actual, _ := rateLimiterRegistry.LoadOrStore(apiKey, rl)
	return actual.(*rateLimiter)
}
