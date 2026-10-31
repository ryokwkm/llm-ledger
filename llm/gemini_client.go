package llm

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/generative-ai-go/genai"
	"google.golang.org/api/option"
)

type GeminiModel string

const (
	ModelGemini25Pro       GeminiModel = "gemini-2.5-pro"
	ModelGemini25Flash     GeminiModel = "gemini-2.5-flash"
	ModelGemini25FlashLite GeminiModel = "gemini-2.5-flash-lite"
	// 無料枠で実用に耐えるモデル。性能降順に並べている（3 Flash は preview のみだが無料枠表に存在する）。
	// RPD は Flash 系が 20 / Lite 系が 500（DailyQuotasFreeAPIKey）。
	// 3.7 / 3.5 Flash は廃止され、要求しても 3.8 / 3.6 Flash が答えるので定数を持たない。
	ModelGemini38Flash       GeminiModel = "gemini-3.8-flash"
	ModelGemini36Flash       GeminiModel = "gemini-3.6-flash"
	ModelGemini3FlashPreview GeminiModel = "gemini-3-flash-preview"
	ModelGemini35FlashLite   GeminiModel = "gemini-3.5-flash-lite"
	ModelGemini31FlashLite   GeminiModel = "gemini-3.1-flash-lite"
)

type GeminiClient struct {
	client    *genai.Client
	model     *genai.GenerativeModel
	modelName GeminiModel
}

func (c *GeminiClient) ModelName() string {
	return string(c.modelName)
}

// NewGeminiClientWithAPIKey creates a new GeminiClient with a specific API key.
// グローバル環境変数を直接読まず API キーを明示的に渡す。呼び出し側で用途別の無料キーを選別する。
func NewGeminiClientWithAPIKey(ctx context.Context, modelName GeminiModel, apiKey string) (*GeminiClient, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("API key is required")
	}

	client, err := genai.NewClient(ctx, option.WithAPIKey(apiKey))
	if err != nil {
		return nil, fmt.Errorf("failed to create Gemini client: %w", err)
	}

	model := client.GenerativeModel(string(modelName))

	// 安全設定を緩和（ブロックされにくくする）
	for _, category := range []genai.HarmCategory{
		genai.HarmCategoryHarassment,
		genai.HarmCategoryHateSpeech,
		genai.HarmCategorySexuallyExplicit,
		genai.HarmCategoryDangerousContent,
	} {
		model.SafetySettings = append(model.SafetySettings, &genai.SafetySetting{
			Category:  category,
			Threshold: genai.HarmBlockNone,
		})
	}

	return &GeminiClient{
		client:    client,
		model:     model,
		modelName: modelName,
	}, nil
}

func (c *GeminiClient) CreateChatCompletionWithTokens(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
	resp, err := c.modelFor(req.MaxTokens).GenerateContent(ctx, genai.Text(req.Prompt))
	if err != nil {
		return nil, fmt.Errorf("Gemini API error: %w", &redactedError{err: err})
	}

	if len(resp.Candidates) == 0 {
		return nil, fmt.Errorf("no candidates in response")
	}

	candidate := resp.Candidates[0]
	if err := validateFinishReason(candidate.FinishReason); err != nil {
		return nil, fmt.Errorf("%s: %w", c.modelName, err)
	}

	// SAFETY ブロック時などは Content 自体が nil になることがある
	if candidate.Content == nil || len(candidate.Content.Parts) == 0 {
		return nil, fmt.Errorf("no content parts in response")
	}

	inputTokens := 0
	outputTokens := 0
	if resp.UsageMetadata != nil {
		inputTokens = int(resp.UsageMetadata.PromptTokenCount)
		outputTokens = int(resp.UsageMetadata.CandidatesTokenCount)
	}

	content, err := joinTextParts(candidate.Content.Parts)
	if err != nil {
		return nil, err
	}

	return &ChatResponse{
		Content:      content,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		ActualModel:  string(c.modelName),
	}, nil
}

// modelFor は 1 回の呼び出しに使うモデルを返す。maxTokens が正なら、その値を持つ複製を返す。
// 共有のモデルへ書くと、指定の無い次の呼び出しに値が残り、並行に呼ぶと値を取り合う。
func (c *GeminiClient) modelFor(maxTokens int) *genai.GenerativeModel {
	if maxTokens <= 0 {
		return c.model
	}
	m := *c.model
	m.MaxOutputTokens = genai.Ptr(int32(maxTokens))
	return &m
}

// validateFinishReason は候補が自然終了（FinishReasonStop）で完了したかを検証する。
// Gemini は MaxTokens 切断や SAFETY ブロックでも Parts に途中までのテキストを残すことがあり、
// FinishReason を見ずに成功扱いすると尻切れ本文がそのまま下流へ流れる。
// Stop 以外は Unspecified も含めすべてエラーにする（完了の保証がないため安全側に倒す）。
func validateFinishReason(reason genai.FinishReason) error {
	if reason == genai.FinishReasonStop {
		return nil
	}
	return fmt.Errorf("出力が完了前に停止しました（FinishReason=%s）", reason)
}

// joinTextParts は Parts に含まれるテキストを出現順に連結して返す。
// テキストが1つも無ければエラー（画像だけの応答などは本文として扱えない）。
//
// 先頭 Part だけを読むと本文が断片になる。Gemini 3.8 Flash は本文を複数 Part に分けて返すことがあり、
// しかも FinishReason=Stop で返るため validateFinishReason では捕まらない（出力トークンは千を超えるのに
// 本文が数文字しか残らない形で観測した）。
// 区切り文字は挟まない —— 分割は文の途中で起きるので、足すと本文が壊れる。
func joinTextParts(parts []genai.Part) (string, error) {
	var sb strings.Builder
	found := false
	for _, part := range parts {
		txt, ok := part.(genai.Text)
		if !ok {
			continue
		}
		found = true
		sb.WriteString(string(txt))
	}
	if !found {
		return "", fmt.Errorf("unexpected content type in response")
	}
	return sb.String(), nil
}

func (c *GeminiClient) Close() error {
	return c.client.Close()
}
