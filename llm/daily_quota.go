// 日次クォータ（RPD）管理。
//
// Gemini 無料枠は RPM と RPD の二重制限で、性質がまったく違う:
//   - RPM: 待てば復活する。rateLimiter（gemini_fallback.go）が最低インターバルで回避する。
//   - RPD: 待っても当日は復活しない（リセットは Pacific Time 深夜0時＝JST 16時ごろ）。
//
// RPD を「429 を受けてから下位モデルへ落とす」方式で捌くと、枠が切れている間は毎コール必ず 1 往復を
// 無駄にする。上位 Flash の無料枠は 20/日と小さく、日中はほぼ常時この状態になる。そのため
// dailyQuotaTracker が送信数を API キー × モデル単位で数え、上限に達したモデルは投げる前にスキップする。
//
// なお、これは Gemini（Google 側）のクォータであって、アプリ側の
// llm_function_configs.daily_limit（guard_client.go・アプリのタイムゾーンの 0 時起点・func_feature 単位）とは別物。

package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // PT ロケーションを埋め込む。tzdata の無い実行環境でも RPD の日付境界を誤らせない

	"google.golang.org/genproto/googleapis/rpc/errdetails"
)

// ErrDailyQuotaExhausted は chain 上の全モデルが日次上限に達していて、1回も送信できなかったことを表す。
// リトライしても当日は復活しないため、この場合は待機せず即座に失敗させる。
var ErrDailyQuotaExhausted = errors.New("chain 上の全モデルが日次上限に達しています")

// DailyQuotas はモデル別の1日あたり最大リクエスト数（RPD）。
// マップに無いモデル、および 0 以下の値は「上限なし」扱い（＝429 を受けてからフォールバック）。
type DailyQuotas map[GeminiModel]int

// DailyQuotasFreeAPIKey は Gemini 無料 API キーの RPD（コンソール確認値）。
//
// 3.5 / 3.1 Flash Lite だけ RPD 500、それ以外（Flash 系と 2.5 Flash Lite）は 20 と極小。
// この非対称が「上位モデルは重要ステップへ専属で割り当て、量は Flash Lite が吸収する」という
// chain 設計の根拠になっている。
var DailyQuotasFreeAPIKey = DailyQuotas{
	ModelGemini38Flash:       20,
	ModelGemini36Flash:       20,
	ModelGemini3FlashPreview: 20,
	ModelGemini25Flash:       20,
	ModelGemini35FlashLite:   500,
	ModelGemini31FlashLite:   500,
	ModelGemini25FlashLite:   20,
}

// QuotaSeed はプロセス起動時に「今日すでに消費した回数」を外部（DB 等）から供給するフック。
// 常駐プロセスが再起動してもその日の実績を引き継ぎ、上位モデルの枠を二重に消費しないために使う。
type QuotaSeed func(ctx context.Context) (map[GeminiModel]int, error)

// geminiQuotaResetLocation は Gemini の RPD がリセットされるタイムゾーン。
// リセットは Pacific Time 深夜0時＝JST 16時ごろで、JST 0時ではない。日次カウンタは必ずこの基準で切る。
var geminiQuotaResetLocation = loadQuotaResetLocation()

func loadQuotaResetLocation() *time.Location {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		// time/tzdata を埋め込んでいるので通常は起きない。万一失敗しても日次カウント自体は
		// 続けたいので、夏時間を無視した PST 固定（UTC-8）で代用する。
		return time.FixedZone("PST", -8*60*60)
	}
	return loc
}

// QuotaDayStart は t が属する Gemini RPD 日（PT 深夜0時起点）の開始時刻を返す。
// 「今日すでに何回使ったか」を実績テーブルから数えるときの下限に使う。
func QuotaDayStart(t time.Time) time.Time {
	pt := t.In(geminiQuotaResetLocation)
	return time.Date(pt.Year(), pt.Month(), pt.Day(), 0, 0, 0, 0, geminiQuotaResetLocation)
}

// dailyQuotaTracker は API キー単位で「モデル別に今日何回投げたか」を数える。
// 同一 API キーから作った複数の FallbackGeminiClient はこのインスタンスを共有し、RPD プールを
// 多重消費しない（rateLimiter と同じ singleton 方式）。
type dailyQuotaTracker struct {
	mu        sync.Mutex
	now       func() time.Time // テストで日付境界をまたがせるための差し替え口
	day       string
	used      map[GeminiModel]int
	exhausted map[GeminiModel]bool
	seeded    bool
}

func newDailyQuotaTracker() *dailyQuotaTracker {
	return &dailyQuotaTracker{now: time.Now}
}

// Reserve は model の日次枠を1回分確保する。枠が残っていなければ false を返し、
// 呼び出し側は送信せずに次のモデルへ落ちる（＝429 を踏む前の切替）。
// limit が 0 以下なら上限なしとして常に true を返す（カウントは進める）。
func (t *dailyQuotaTracker) Reserve(model GeminiModel, limit int) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rotateLocked()

	if t.exhausted[model] {
		return false
	}
	if limit > 0 && t.used[model] >= limit {
		return false
	}
	t.used[model]++
	return true
}

// MarkExhausted は「日次枠を使い切った」と Google 側が明示したモデルを当日打ち止めにする。
// 上限値の想定がズレていても（他プロセスとの枠共有・失敗リトライによる消費）、以後は投げずに
// 下位モデルへ落ちる。
func (t *dailyQuotaTracker) MarkExhausted(model GeminiModel) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rotateLocked()
	t.exhausted[model] = true
}

// Used は当 RPD 日に model へ投げた回数を返す（テスト・診断用）。
func (t *dailyQuotaTracker) Used(model GeminiModel) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rotateLocked()
	return t.used[model]
}

// Seed は当日すでに消費した回数を外部から取り込む。
//
// 同一 API キーの FallbackGeminiClient は tracker を共有するため、クライアントを5本作れば5回呼ばれる。
// 取り込みは RPD 日ごとに最初の1回だけ効かせる。構築時の1回きりの処理なので、外部 I/O を
// ロック内で実行することを許容している。
func (t *dailyQuotaTracker) Seed(ctx context.Context, seed QuotaSeed) error {
	if seed == nil {
		return nil
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	t.rotateLocked()
	if t.seeded {
		return nil
	}

	used, err := seed(ctx)
	if err != nil {
		return err
	}
	for model, count := range used {
		if count > t.used[model] {
			t.used[model] = count
		}
	}
	t.seeded = true
	return nil
}

// rotateLocked は RPD 日が変わっていたらカウンタを捨てる。mu を保持した状態で呼ぶこと。
func (t *dailyQuotaTracker) rotateLocked() {
	now := time.Now
	if t.now != nil {
		now = t.now
	}
	day := now().In(geminiQuotaResetLocation).Format("2006-01-02")
	if t.day == day && t.used != nil {
		return
	}
	t.day = day
	t.used = make(map[GeminiModel]int)
	t.exhausted = make(map[GeminiModel]bool)
	t.seeded = false
}

// quotaTrackerRegistry は API キーごとの dailyQuotaTracker を保持する process-wide singleton。
var quotaTrackerRegistry sync.Map // map[string]*dailyQuotaTracker

func getQuotaTracker(apiKey string) *dailyQuotaTracker {
	if v, ok := quotaTrackerRegistry.Load(apiKey); ok {
		return v.(*dailyQuotaTracker)
	}
	actual, _ := quotaTrackerRegistry.LoadOrStore(apiKey, newDailyQuotaTracker())
	return actual.(*dailyQuotaTracker)
}

// UsageRepository は QuotaSeed へモデル別の消費実績を供給するリポジトリ。
// LLM 呼び出しログ（llm_logs）を持つプロジェクトなら LLMRepository がそのまま満たす。
type UsageRepository interface {
	GetModelUsageSince(ctx context.Context, since time.Time, featurePrefix string) (map[string]int, error)
}

// NewDBQuotaSeed は LLM 呼び出しログの実績から、当 RPD 日（PT 基準）の消費回数をモデル別に
// 集計する QuotaSeed を返す。
//
// featurePrefixes は func_feature の前方一致（例: "article-"）で、並べた分の消費を合算する。
// API キーはドメインごとに分けているため、枠もドメインごとに数える。空文字なら絞り込まない。
//
// 1 本の API キーを複数の機能が共有するときは、その機能を全部並べる。RPD はキー単位で減るので、片方の
// 消費だけを数えると残枠を多く見積もり、429 を踏みに行くことになる。
// 逆に、キーを分けた機能まで同じ前方一致で拾うと残枠を少なく見積もる。どちらも避けるため、
// 「このキーを使う機能」を呼び出し側が明示的に並べる。
//
// 🔴 注意: model_name に入るのは「実際に応答したモデル」（失敗時は起点モデル名）なので、上位モデルで
// 429 を踏んで下位で成功した分は上位側に計上されない ＝ 実消費よりやや少なめに出る。
// それでも 0 から数え直すよりは実態に近く、再起動直後の二重消費を大きく減らせる。
func NewDBQuotaSeed(repo UsageRepository, featurePrefixes ...string) QuotaSeed {
	return func(ctx context.Context) (map[GeminiModel]int, error) {
		since := QuotaDayStart(time.Now())

		used := map[GeminiModel]int{}
		for _, prefix := range featurePrefixes {
			counts, err := repo.GetModelUsageSince(ctx, since, prefix)
			if err != nil {
				return nil, fmt.Errorf("モデル別の当日消費数の取得に失敗: %w", err)
			}
			for name, count := range counts {
				used[GeminiModel(name)] += count
			}
		}
		return used, nil
	}
}

// dailyQuotaRetryThreshold は「この retry_delay を超えたら日次超過とみなす」閾値。
// RPM の窓は 60 秒なので、それを明確に超える待ちを指示されたら分あたりの超過では説明が付かない。
// 実測（無料キー）では RPM 超過の retry_delay は 41〜56 秒だった。
const dailyQuotaRetryThreshold = 90 * time.Second

// isDailyQuotaExceeded は 429 が「日次（RPD）」の超過かを判定する。
//
// 分あたり（RPM）やトークン（TPM）の超過は待てば復活するので、打ち止め扱いにしてはいけない
// （そのモデルを丸一日使わなくなり、希少な上位枠を捨てることになる）。
//
// 🔴 判別材料は当てにならないことがある。実測の RPM 超過レスポンスは
// QuotaFailure の Subject / Description がともに空で、violation からは分次か日次かを判別できなかった
// （metric 名は日次と共通の generate_content_free_tier_requests、区別が付くのはメッセージ本文の
// "limit: 5" と retry_delay だけ）。そのため三段構えで見る:
//  1. QuotaFailure の violation に日次の目印があるか
//  2. retry_delay が RPM の窓（60秒）を明確に超えているか
//  3. エラー文字列に日次の目印があるか
//
// どれも外した場合でも、本命の防御は送信前の Reserve カウンタ側にある。
func isDailyQuotaExceeded(err error) bool {
	if err == nil {
		return false
	}

	for _, d := range statusDetails(err) {
		qf, ok := d.(*errdetails.QuotaFailure)
		if !ok {
			continue
		}
		for _, v := range qf.GetViolations() {
			if hasDailyQuotaMarker(v.GetSubject()) || hasDailyQuotaMarker(v.GetDescription()) {
				return true
			}
		}
	}

	if delay, ok := extractRetryDelay(err); ok && delay > dailyQuotaRetryThreshold {
		return true
	}
	return hasDailyQuotaMarker(err.Error())
}

// hasDailyQuotaMarker は quota_id / quota_metric / 説明文に現れる日次クォータの目印を探す。
// 実際の Gemini 429 では "GenerateRequestsPerDayPerProjectPerModel-FreeTier" のような quota_id や
// "Daily Limit ... exceeded" のような説明文が入る。
func hasDailyQuotaMarker(s string) bool {
	lower := strings.ToLower(s)
	return strings.Contains(lower, "perday") ||
		strings.Contains(lower, "per day") ||
		strings.Contains(lower, "per_day") ||
		strings.Contains(lower, "daily")
}
