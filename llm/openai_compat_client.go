package llm

import (
	"context"
	"fmt"
	"os"

	"github.com/sashabaranov/go-openai"
)

type OpenAICompatProvider struct {
	BaseURL   string
	APIKeyEnv string
	ModelName string
}

var (
	ProviderDeepSeekV3 = OpenAICompatProvider{
		BaseURL:   "https://api.deepseek.com/v1",
		APIKeyEnv: "DEEPSEEK_API_KEY",
		ModelName: "deepseek-chat",
	}
	ProviderDeepSeekV4Flash = OpenAICompatProvider{
		BaseURL:   "https://api.deepseek.com/v1",
		APIKeyEnv: "DEEPSEEK_API_KEY",
		ModelName: "deepseek-v4-flash",
	}
	ProviderNemotronSuper49B = OpenAICompatProvider{
		BaseURL:   "https://integrate.api.nvidia.com/v1",
		APIKeyEnv: "NVIDIA_API_KEY",
		ModelName: "nvidia/llama-3.3-nemotron-super-49b-v1.5",
	}
)

type OpenAICompatClient struct {
	client    *openai.Client
	modelName string
}

func NewOpenAICompatClient(provider OpenAICompatProvider) (*OpenAICompatClient, error) {
	apiKey := os.Getenv(provider.APIKeyEnv)
	if apiKey == "" {
		return nil, fmt.Errorf("%s environment variable is not set", provider.APIKeyEnv)
	}
	cfg := openai.DefaultConfig(apiKey)
	cfg.BaseURL = provider.BaseURL
	return &OpenAICompatClient{
		client:    openai.NewClientWithConfig(cfg),
		modelName: provider.ModelName,
	}, nil
}

func (c *OpenAICompatClient) ModelName() string {
	return c.modelName
}

func (c *OpenAICompatClient) CreateChatCompletionWithTokens(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
	chatReq := openai.ChatCompletionRequest{
		Model: c.modelName,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleUser, Content: req.Prompt},
		},
	}
	if req.MaxTokens > 0 {
		chatReq.MaxTokens = req.MaxTokens
	}

	resp, err := c.client.CreateChatCompletion(ctx, chatReq)
	if err != nil {
		return nil, fmt.Errorf("%s API error: %w", c.modelName, err)
	}

	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("no choices in response")
	}

	if resp.Choices[0].FinishReason == openai.FinishReasonLength {
		return nil, fmt.Errorf("%s: 出力が max_tokens=%d で切断されました（finish_reason=length）。llm_function_configs の max_tokens を増やしてください", c.modelName, chatReq.MaxTokens)
	}

	return &ChatResponse{
		Content:      resp.Choices[0].Message.Content,
		InputTokens:  resp.Usage.PromptTokens,
		OutputTokens: resp.Usage.CompletionTokens,
	}, nil
}
