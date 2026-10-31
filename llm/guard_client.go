package llm

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var ErrDailyLimitExceeded = errors.New("daily limit exceeded")

// Logger はこのパッケージがログを出すのに使う最小のインターフェース。
type Logger interface {
	Warn(msg string, keyvals ...any)
	Error(msg string, keyvals ...any)
}

func IsDailyLimitExceeded(err error) bool {
	return errors.Is(err, ErrDailyLimitExceeded)
}

// SuppressIfLimitExceeded は日次上限の超過だけを WARN に落として nil にし、それ以外のエラーはそのまま返す。
// 上限で止まるのは想定内なので、待って次の日に続ける呼び出し元が失敗として数えないようにする。
func SuppressIfLimitExceeded(logger Logger, err error) error {
	if IsDailyLimitExceeded(err) {
		logger.Warn("API呼び出し制限に達したため、待機します", "err", err)
		return nil
	}
	return err
}

type GuardedClient struct {
	inner   InternalClient
	repo    Repository
	feature string
	logger  Logger
}

func NewGuardedClient(inner InternalClient, repo Repository, feature string, logger Logger) *GuardedClient {
	return &GuardedClient{
		inner:   inner,
		repo:    repo,
		feature: feature,
		logger:  logger,
	}
}

func (c *GuardedClient) CreateChatCompletion(ctx context.Context, prompt string) (string, error) {
	req := &ChatRequest{
		Prompt: prompt,
	}
	resp, _, err := c.CreateChatCompletionWithLog(ctx, req)
	return resp, err
}

func (c *GuardedClient) CreateChatCompletionWithLog(ctx context.Context, req *ChatRequest) (string, *LogHandle, error) {
	// actualModel が空でなければそれを model_name に使い、空なら inner.ModelName() を使う。
	// FallbackGeminiClient のように起点モデル名と実際に応答したモデル名が乖離するケースを正しくログに残すための分岐。
	saveLog := func(status LogStatus, inputTokens, outputTokens int, content, actualModel string) int64 {
		modelName := c.inner.ModelName()
		if actualModel != "" {
			modelName = actualModel
		}
		log := &LLMLog{
			FuncFeature:   c.feature,
			Memo:          req.Memo,
			Status:        status,
			InputTokens:   inputTokens,
			OutputTokens:  outputTokens,
			ModelName:     modelName,
			InputContent:  req.Prompt,
			OutputContent: content,
			AppStatus:     AppStatusUnknown,
			CreatedAt:     time.Now(),
		}
		id, err := c.repo.SaveLog(ctx, log)
		if err != nil {
			// 応答は使えるので呼び出しは失敗にしない。ID が 0 のままだと後の MarkAsInvalid/MarkAsValid が何もしないので、ここで気づけるようにする。
			c.logger.Error("LLM ログを保存できませんでした（このログへの検証結果も記録されません）",
				"feature", c.feature, "status", status, "err", err)
		}
		return id
	}

	config, refusal, err := c.checkQuota(ctx)
	if err != nil {
		if refusal != LogStatusUnknown {
			saveLog(refusal, 0, 0, "", "")
		}
		return "", NewNoopLogHandle(), err
	}

	if req.MaxTokens == 0 {
		req.MaxTokens = config.MaxTokens
	}

	result, err := c.inner.CreateChatCompletionWithTokens(ctx, req)
	if err != nil {
		saveLog(LogStatusFailed, 0, 0, fmt.Sprintf("Error: %v", err), "")
		return "", NewNoopLogHandle(), err
	}

	logID := saveLog(LogStatusSuccess, result.InputTokens, result.OutputTokens, result.Content, result.ActualModel)
	return result.Content, &LogHandle{ID: logID, repo: c.repo, logger: c.logger}, nil
}

// Available は今この機能を呼べるかを、LLM を呼ばず llm_logs にも書かずに返す。
// 今日の上限に達しているか緊急停止中なら false。設定が無い・回数を数えられないときはエラーを返す。
// 呼ぶ前にブラウザを開くなどの重い準備がある呼び出し元が、準備の前に断られると分かるようにするため。
func (c *GuardedClient) Available(ctx context.Context) (bool, error) {
	_, refusal, err := c.checkQuota(ctx)
	if refusal == LogStatusLimited || refusal == LogStatusDisabled {
		return false, nil
	}
	return err == nil, err
}

// checkQuota は呼び出し前の判定。呼べるなら設定を返し、呼べないならエラーと llm_logs に残すステータスを返す。
// 回数を数えられなかったときは断ったのではないので、ステータスは LogStatusUnknown（行を残さない）。
func (c *GuardedClient) checkQuota(ctx context.Context) (*LLMFunctionConfig, LogStatus, error) {
	config, err := c.repo.GetConfig(ctx, c.feature)
	if err != nil {
		return nil, LogStatusConfigMissing, fmt.Errorf("failed to get llm config for %s: %w", c.feature, err)
	}

	if !config.IsEnabled() {
		return nil, LogStatusDisabled, fmt.Errorf("feature %s is currently disabled in config", c.feature)
	}

	count, err := c.repo.GetDailyUsageCount(ctx, c.feature)
	if err != nil {
		return nil, LogStatusUnknown, fmt.Errorf("failed to check usage count: %w", err)
	}

	if config.IsLimitExceeded(count) {
		return nil, LogStatusLimited, fmt.Errorf("%w for %s: current=%d, limit=%d", ErrDailyLimitExceeded, c.feature, count, config.DailyLimit)
	}
	return config, LogStatusUnknown, nil
}
