package llm

import "context"

type ChatResponse struct {
	Content      string
	InputTokens  int
	OutputTokens int
	// ActualModel はフォールバック付きクライアント等で、実際にレスポンスを返したモデル名を呼び出し元に伝えるための受け渡し用フィールド。
	// 空文字なら GuardedClient は inner.ModelName() を使う。
	ActualModel string
}

type Client interface {
	CreateChatCompletion(ctx context.Context, prompt string) (string, error)
	CreateChatCompletionWithLog(ctx context.Context, req *ChatRequest) (string, *LogHandle, error)
}

type ChatRequest struct {
	Prompt    string
	MaxTokens int
	Memo      string
}

type InternalClient interface {
	CreateChatCompletionWithTokens(ctx context.Context, req *ChatRequest) (*ChatResponse, error)
	ModelName() string
}
