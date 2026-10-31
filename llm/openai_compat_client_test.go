package llm_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/ryokwkm/llm-ledger/llm"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newMockOpenAICompatServer(t *testing.T, finishReason string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{
			"choices": [{"message": {"role": "assistant", "content": "途中まで出力された内容"}, "finish_reason": %q}],
			"usage": {"prompt_tokens": 100, "completion_tokens": 50}
		}`, finishReason)
	}))
}

func newClientForMockServer(t *testing.T, serverURL string) *llm.OpenAICompatClient {
	t.Helper()
	t.Setenv("MOCK_LLM_API_KEY", "dummy")
	client, err := llm.NewOpenAICompatClient(llm.OpenAICompatProvider{
		BaseURL:   serverURL + "/v1",
		APIKeyEnv: "MOCK_LLM_API_KEY",
		ModelName: "mock-model",
	})
	require.NoError(t, err)
	return client
}

func TestCreateChatCompletionWithTokens_FinishReasonLength(t *testing.T) {
	server := newMockOpenAICompatServer(t, "length")
	defer server.Close()

	client := newClientForMockServer(t, server.URL)
	resp, err := client.CreateChatCompletionWithTokens(context.Background(), &llm.ChatRequest{
		Prompt:    "テスト",
		MaxTokens: 50,
	})

	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Contains(t, err.Error(), "max_tokens=50 で切断")
}

func TestCreateChatCompletionWithTokens_FinishReasonStop(t *testing.T) {
	server := newMockOpenAICompatServer(t, "stop")
	defer server.Close()

	client := newClientForMockServer(t, server.URL)
	resp, err := client.CreateChatCompletionWithTokens(context.Background(), &llm.ChatRequest{
		Prompt: "テスト",
	})

	require.NoError(t, err)
	assert.Equal(t, "途中まで出力された内容", resp.Content)
	assert.Equal(t, 100, resp.InputTokens)
	assert.Equal(t, 50, resp.OutputTokens)
}

func testOpenAICompatLive(t *testing.T, provider llm.OpenAICompatProvider) {
	t.Helper()

	if testing.Short() {
		t.Skip("ライブAPIテストはスキップ（-short）")
	}
	if os.Getenv(provider.APIKeyEnv) == "" {
		t.Skipf("ライブAPIテストはスキップ（%s 未セット）", provider.APIKeyEnv)
	}

	client, err := llm.NewOpenAICompatClient(provider)
	require.NoError(t, err)

	resp, err := client.CreateChatCompletionWithTokens(context.Background(), &llm.ChatRequest{
		Prompt: "こんにちは。一言で自己紹介してください。",
	})
	require.NoError(t, err)

	assert.NotEmpty(t, resp.Content)
	assert.Greater(t, resp.InputTokens, 0)
	assert.Greater(t, resp.OutputTokens, 0)

	t.Logf("モデル: %s", client.ModelName())
	t.Logf("レスポンス: %s", resp.Content)
	t.Logf("トークン: input=%d output=%d", resp.InputTokens, resp.OutputTokens)
}

func TestDeepSeekV3_Live(t *testing.T) {
	testOpenAICompatLive(t, llm.ProviderDeepSeekV3)
}

func TestDeepSeekV4Flash_Live(t *testing.T) {
	testOpenAICompatLive(t, llm.ProviderDeepSeekV4Flash)
}

func TestNemotronSuper49B_Live(t *testing.T) {
	testOpenAICompatLive(t, llm.ProviderNemotronSuper49B)
}
