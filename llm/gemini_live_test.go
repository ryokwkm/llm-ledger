package llm_test

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/ryokwkm/llm-ledger/llm"
)

// TestManual_GeminiModels_Live は無料 API キーで各モデルが実際に叩けるかを1本ずつ確認する手動テスト。
// モデルを増やしたとき・切替の前に「そのモデル ID が有効か」「そのキーで権限があるか」を見るために使う。
//
// 実行（.env を読み込んでから走らせる）:
//
//	GEMINI_API_KEY=... go test -v -run TestManual_GeminiModels_Live ./llm/ -timeout 240s
//
// キーを別の環境変数から読むときは GEMINI_LIVE_KEY_ENV にその名前を入れる。
//
// ⚠️ 各モデルの RPD を1回ずつ消費する（Flash 系は 20/日しかない）。日に何度も回さないこと。
// 失敗しても他モデルの確認は続ける（1つ落ちても全体像が見たいテストのため）。
func TestManual_GeminiModels_Live(t *testing.T) {
	if testing.Short() {
		t.Skip("ライブAPIテストはスキップ（-short）")
	}

	// 本番の処理が使っているキーで回すと、確認の呼び出しが本番の RPM・RPD と取り合う（1 回目から RPM 超過が返ることがある）。
	// 確認には本番の負荷が小さいキーを選んで GEMINI_LIVE_KEY_ENV で渡す。
	keyEnv := "GEMINI_API_KEY"
	if v := os.Getenv("GEMINI_LIVE_KEY_ENV"); v != "" {
		keyEnv = v
	}
	apiKey := os.Getenv(keyEnv)
	if apiKey == "" {
		t.Skipf("ライブAPIテストはスキップ（%s が未セット）", keyEnv)
	}
	t.Logf("使用キー: %s（このキーの RPD を各モデル1回ずつ消費する）", keyEnv)

	// 思考（thinking）を使うモデルは短い MaxTokens だと本文へ到達せず FinishReason=MaxTokens で落ちる。
	// 既定 2000 は記事生成のような長文の max_tokens（4000〜8000）より小さいので、疑わしいときは GEMINI_LIVE_MAX_TOKENS で上げて再確認する。
	maxTokens := 2000
	if v := os.Getenv("GEMINI_LIVE_MAX_TOKENS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("GEMINI_LIVE_MAX_TOKENS が数値でない: %q", v)
		}
		maxTokens = n
	}

	// 運用中の chain に出てくるモデルを性能降順で並べる。
	models := []llm.GeminiModel{
		llm.ModelGemini38Flash,
		llm.ModelGemini36Flash,
		llm.ModelGemini3FlashPreview,
		llm.ModelGemini35FlashLite,
		llm.ModelGemini31FlashLite,
	}

	for _, model := range models {
		t.Run(string(model), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			client, err := llm.NewGeminiClientWithAPIKey(ctx, model, apiKey)
			if err != nil {
				t.Errorf("クライアント構築に失敗: %v", err)
				return
			}
			defer client.Close()

			start := time.Now()
			resp, err := client.CreateChatCompletionWithTokens(ctx, &llm.ChatRequest{
				Prompt:    "あなたのモデル名を1行で答えてください。",
				MaxTokens: maxTokens,
			})
			if err != nil {
				t.Errorf("❌ %s: %v", model, err)
				return
			}

			t.Logf("✅ %s: %.1fs / tokens in=%d out=%d / %q",
				model, time.Since(start).Seconds(), resp.InputTokens, resp.OutputTokens, resp.Content)
		})
	}
}
