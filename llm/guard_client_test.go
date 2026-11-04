package llm

import (
	"context"
	"errors"
	"testing"
)

type MockInternalClient struct {
	Response *ChatResponse
	Err      error
	Calls    int
}

func (m *MockInternalClient) CreateChatCompletionWithTokens(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
	m.Calls++
	return m.Response, m.Err
}

func (m *MockInternalClient) ModelName() string {
	return "mock-model"
}

type MockRepository struct {
	Config     *LLMFunctionConfig
	ConfigErr  error
	Count      int
	CountErr   error
	SavedLogs  []*LLMLog
	SaveLogErr error
	UpdateErr  error
}

func (m *MockRepository) GetConfig(ctx context.Context, feature string) (*LLMFunctionConfig, error) {
	return m.Config, m.ConfigErr
}

func (m *MockRepository) GetDailyUsageCount(ctx context.Context, feature string) (int, error) {
	return m.Count, m.CountErr
}

func (m *MockRepository) SaveLog(ctx context.Context, log *LLMLog) (int64, error) {
	m.SavedLogs = append(m.SavedLogs, log)
	return 1, m.SaveLogErr
}

func (m *MockRepository) UpdateLogValidation(ctx context.Context, logID int64, appStatus AppStatus, appError string) error {
	return m.UpdateErr
}

// spyLogger は出したログの件数を数える。
type spyLogger struct {
	warns  []string
	errors []string
}

func (s *spyLogger) Warn(msg string, _ ...any)  { s.warns = append(s.warns, msg) }
func (s *spyLogger) Error(msg string, _ ...any) { s.errors = append(s.errors, msg) }

func TestGuardedClient_CreateChatCompletion(t *testing.T) {
	ctx := context.Background()
	feature := "test_feature"

	tests := []struct {
		name           string
		config         *LLMFunctionConfig
		count          int
		mockClientResp *ChatResponse
		mockClientErr  error
		expectErr      bool
		expectLog      LogStatus
	}{
		{
			name: "正常系: 制限内",
			config: &LLMFunctionConfig{
				FuncFeature: feature,
				DailyLimit:  10,
				IsActive:    true,
			},
			count:          5,
			mockClientResp: &ChatResponse{Content: "OK", InputTokens: 10, OutputTokens: 10},
			expectErr:      false,
			expectLog:      LogStatusSuccess,
		},
		{
			name: "異常系: 無効化されている",
			config: &LLMFunctionConfig{
				FuncFeature: feature,
				DailyLimit:  10,
				IsActive:    false,
			},
			count:     0,
			expectErr: true,
			expectLog: LogStatusDisabled,
		},
		{
			name: "異常系: 上限超過",
			config: &LLMFunctionConfig{
				FuncFeature: feature,
				DailyLimit:  10,
				IsActive:    true,
			},
			count:     10,
			expectErr: true,
			expectLog: LogStatusLimited,
		},
		{
			name: "正常系: 上限 -1 は無制限",
			config: &LLMFunctionConfig{
				FuncFeature: feature,
				DailyLimit:  DailyLimitUnlimited,
				IsActive:    true,
			},
			count:          10000,
			mockClientResp: &ChatResponse{Content: "OK", InputTokens: 10, OutputTokens: 10},
			expectErr:      false,
			expectLog:      LogStatusSuccess,
		},
		{
			name: "異常系: 上限 0 は 1 回も呼ばない",
			config: &LLMFunctionConfig{
				FuncFeature: feature,
				DailyLimit:  0,
				IsActive:    true,
			},
			count:     0,
			expectErr: true,
			expectLog: LogStatusLimited,
		},
		{
			name: "異常系: APIエラー",
			config: &LLMFunctionConfig{
				FuncFeature: feature,
				DailyLimit:  10,
				IsActive:    true,
			},
			count:         5,
			mockClientErr: errors.New("API Error"),
			expectErr:     true,
			expectLog:     LogStatusFailed,
		},
		{
			name:      "異常系: 設定なし",
			config:    nil,
			count:     0,
			expectErr: true,
			expectLog: LogStatusConfigMissing,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockRepo := &MockRepository{
				Config: tt.config,
				Count:  tt.count,
			}
			if tt.config == nil {
				mockRepo.ConfigErr = errors.New("record not found")
			}

			mockClient := &MockInternalClient{
				Response: tt.mockClientResp,
				Err:      tt.mockClientErr,
			}

			client := NewGuardedClient(mockClient, mockRepo, feature, &spyLogger{})
			_, err := client.CreateChatCompletion(ctx, "prompt")

			if (err != nil) != tt.expectErr {
				t.Errorf("expectErr %v, got %v", tt.expectErr, err)
			}

			if tt.expectLog == LogStatusUnknown {
				if len(mockRepo.SavedLogs) != 0 {
					t.Errorf("expected 0 logs, got %d", len(mockRepo.SavedLogs))
				}
			} else {
				if len(mockRepo.SavedLogs) != 1 {
					t.Errorf("expected 1 log, got %d", len(mockRepo.SavedLogs))
				} else if mockRepo.SavedLogs[0].Status != tt.expectLog {
					t.Errorf("expected log status %v, got %v", tt.expectLog, mockRepo.SavedLogs[0].Status)
				}
			}
		})
	}
}

// TestGuardedClient_Available は、呼ぶ前の判定が呼び出し時の判定と同じ結果になり、LLM も llm_logs も触らないことを保証する。
// ブラウザを開く前の計画で何度も呼ぶので、ここで行を残すと判定のたびに limited の行が積み上がる。
func TestGuardedClient_Available(t *testing.T) {
	ctx := context.Background()
	active := func(limit int) *LLMFunctionConfig {
		return &LLMFunctionConfig{FuncFeature: "test_feature", DailyLimit: limit, IsActive: true}
	}

	tests := []struct {
		name      string
		config    *LLMFunctionConfig
		configErr error
		count     int
		countErr  error
		want      bool
		wantErr   bool
	}{
		{name: "上限に余裕があれば呼べる", config: active(10), count: 9, want: true},
		{name: "今日の上限に達していれば呼べない", config: active(10), count: 10, want: false},
		{name: "上限 -1 は無制限", config: active(DailyLimitUnlimited), count: 10000, want: true},
		{name: "上限 0 は呼べない", config: active(0), count: 0, want: false},
		{name: "緊急停止中は呼べない", config: &LLMFunctionConfig{FuncFeature: "test_feature", DailyLimit: 10, IsActive: false}, want: false},
		{name: "設定が無ければエラー", configErr: errors.New("record not found"), want: false, wantErr: true},
		{name: "回数を数えられなければエラー", config: active(10), countErr: errors.New("db down"), want: false, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &MockRepository{Config: tt.config, ConfigErr: tt.configErr, Count: tt.count, CountErr: tt.countErr}
			inner := &MockInternalClient{Response: &ChatResponse{Content: "OK"}}

			got, err := NewGuardedClient(inner, repo, "test_feature", &spyLogger{}).Available(ctx)

			if got != tt.want {
				t.Errorf("want %v, got %v", tt.want, got)
			}
			if (err != nil) != tt.wantErr {
				t.Errorf("wantErr %v, got %v", tt.wantErr, err)
			}
			if len(repo.SavedLogs) != 0 {
				t.Errorf("判定だけで llm_logs に %d 行書いた", len(repo.SavedLogs))
			}
			if inner.Calls != 0 {
				t.Errorf("判定だけで LLM を %d 回呼んだ", inner.Calls)
			}
		})
	}
}

func TestGuardedClient_CreateChatCompletionWithLog(t *testing.T) {
	ctx := context.Background()
	feature := "test_with_log"

	t.Run("Memoがログに保存されること", func(t *testing.T) {
		mockRepo := &MockRepository{
			Config: &LLMFunctionConfig{
				FuncFeature: feature,
				DailyLimit:  100,
				IsActive:    true,
			},
			Count: 0,
		}
		mockClient := &MockInternalClient{
			Response: &ChatResponse{Content: "OK"},
		}

		client := NewGuardedClient(mockClient, mockRepo, feature, &spyLogger{})
		req := &ChatRequest{Prompt: "hello", Memo: "test-memo"}
		_, _, err := client.CreateChatCompletionWithLog(ctx, req)

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(mockRepo.SavedLogs) != 1 {
			t.Fatalf("expected 1 log, got %d", len(mockRepo.SavedLogs))
		}
		if mockRepo.SavedLogs[0].Memo != "test-memo" {
			t.Errorf("expected memo 'test-memo', got '%s'", mockRepo.SavedLogs[0].Memo)
		}
		if mockRepo.SavedLogs[0].InputContent != "hello" {
			t.Errorf("expected input 'hello', got '%s'", mockRepo.SavedLogs[0].InputContent)
		}
	})

	groupTests := map[string]struct {
		want    string
		groupID string
	}{
		"GroupID を渡せばログに保存する":    {want: "helpful-c7", groupID: "helpful-c7"},
		"GroupID を渡さなければ空（NULL）": {want: "", groupID: ""},
	}
	for name, tc := range groupTests {
		t.Run(name, func(t *testing.T) {
			mockRepo := &MockRepository{Config: &LLMFunctionConfig{FuncFeature: feature, DailyLimit: 100, IsActive: true}}
			client := NewGuardedClient(&MockInternalClient{Response: &ChatResponse{Content: "OK"}}, mockRepo, feature, &spyLogger{})

			if _, _, err := client.CreateChatCompletionWithLog(ctx, &ChatRequest{Prompt: "hello", GroupID: tc.groupID}); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(mockRepo.SavedLogs) != 1 {
				t.Fatalf("expected 1 log, got %d", len(mockRepo.SavedLogs))
			}
			got := mockRepo.SavedLogs[0].GroupID
			if got != tc.want {
				t.Errorf("GroupID = %q, want %q", got, tc.want)
			}
		})
	}

	t.Run("上限で断った呼び出しのログにも GroupID を残す", func(t *testing.T) {
		mockRepo := &MockRepository{Config: &LLMFunctionConfig{FuncFeature: feature, DailyLimit: 0, IsActive: true}}
		client := NewGuardedClient(&MockInternalClient{}, mockRepo, feature, &spyLogger{})

		_, _, err := client.CreateChatCompletionWithLog(ctx, &ChatRequest{Prompt: "hello", GroupID: "helpful-c7"})

		if !IsDailyLimitExceeded(err) {
			t.Fatalf("err = %v, want daily limit exceeded", err)
		}
		if len(mockRepo.SavedLogs) != 1 || mockRepo.SavedLogs[0].GroupID != "helpful-c7" {
			t.Errorf("limited のログの GroupID が残っていない: %+v", mockRepo.SavedLogs)
		}
	})
}

func TestGuardedClient_SaveLogFailure(t *testing.T) {
	ctx := context.Background()
	feature := "test_save_log_failure"

	mockRepo := &MockRepository{
		Config:     &LLMFunctionConfig{FuncFeature: feature, DailyLimit: 100, IsActive: true},
		SaveLogErr: errors.New("db is down"),
	}
	mockClient := &MockInternalClient{Response: &ChatResponse{Content: "OK"}}
	spy := &spyLogger{}

	client := NewGuardedClient(mockClient, mockRepo, feature, spy)
	got, err := client.CreateChatCompletion(ctx, "prompt")

	if err != nil {
		t.Fatalf("ログの保存に失敗しても応答は返す: err = %v", err)
	}
	if got != "OK" {
		t.Errorf("got = %q, want %q", got, "OK")
	}
	if len(spy.errors) != 1 {
		t.Errorf("ERROR ログ = %d 件, want 1 件: %v", len(spy.errors), spy.errors)
	}
}

func TestLogHandle_UpdateValidationFailure(t *testing.T) {
	markInvalid := func(ctx context.Context, h *LogHandle) { h.MarkAsInvalid(ctx, errors.New("json parse")) }
	markValid := func(ctx context.Context, h *LogHandle) { h.MarkAsValid(ctx) }

	tests := []struct {
		name       string
		updateErr  error
		mark       func(ctx context.Context, h *LogHandle)
		expectWarn int
	}{
		{name: "MarkAsInvalid の書き込み失敗は WARN に出す", updateErr: errors.New("db is down"), mark: markInvalid, expectWarn: 1},
		{name: "MarkAsValid の書き込み失敗は WARN に出す", updateErr: errors.New("db is down"), mark: markValid, expectWarn: 1},
		{name: "書き込めたときは何も出さない", updateErr: nil, mark: markInvalid, expectWarn: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			feature := "test_update_validation"
			mockRepo := &MockRepository{
				Config:    &LLMFunctionConfig{FuncFeature: feature, DailyLimit: 100, IsActive: true},
				UpdateErr: tt.updateErr,
			}
			spy := &spyLogger{}
			client := NewGuardedClient(&MockInternalClient{Response: &ChatResponse{Content: "OK"}}, mockRepo, feature, spy)

			_, handle, err := client.CreateChatCompletionWithLog(ctx, &ChatRequest{Prompt: "prompt"})
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			tt.mark(ctx, handle)

			if len(spy.warns) != tt.expectWarn {
				t.Errorf("WARN ログ = %d 件, want %d 件: %v", len(spy.warns), tt.expectWarn, spy.warns)
			}
		})
	}
}
