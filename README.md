# llm-ledger

LLM の呼び出しを 1 件ずつ台帳（`llm_logs`）に記録し、機能ごとの日次上限（`llm_function_configs`）で止める Go ライブラリです。Gemini の無料枠を、モデルを乗り換えながら使い切らずに回すクライアントも入っています。

```
go get github.com/ryokwkm/llm-ledger
```

```go
import "github.com/ryokwkm/llm-ledger/llm"
```

## できること

| 部品 | 役割 |
|---|---|
| `GuardedClient` | 呼ぶ前に機能の設定・緊急停止・当日の回数を見て、超えていれば LLM を呼ばずに断る。成功も失敗も断った呼び出しも `llm_logs` に残す |
| `GuardedClient.Available` | LLM を呼ばず、ログも書かずに「いま呼べるか」だけを返す。ブラウザを開くなど重い準備の前に、断られるかを知るために使う |
| `LogHandle` | 応答を使った側の検証結果（JSON が読めた・読めなかった）を、そのログ行へ書き戻す |
| `FallbackGeminiClient` | 試す順に並べたモデル（chain）を上から試し、無料枠の超過・提供終了・無応答なら次のモデルへ落ちる。1 日の上限（RPD）を使い切ったモデルは送る前に飛ばす。分あたりの上限（RPM）は API キーとモデルの組ごとに間隔を空ける |
| `NewDBQuotaSeed` | 常駐プロセスが再起動しても、その日にすでに使った回数を `llm_logs` から取り戻す |
| `OpenAICompatClient` | OpenAI 互換の API（DeepSeek など）を同じインターフェースで呼ぶ |
| `LLMRepository.PurgeOldContent` | 古いログの本文（プロンプトと応答）だけを消す。回数とトークン数は残す |

## 使い方

```go
repo := llm.NewLLMRepository(db) // *gorm.DB

gemini, err := llm.NewFallbackGeminiClient(ctx, llm.FallbackConfig{
	Chain:       []llm.GeminiModel{llm.ModelGemini38Flash, llm.ModelGemini36Flash, llm.ModelGemini31FlashLite},
	RateLimits:  llm.RateLimitsFreeAPIKey,
	DailyQuotas: llm.DailyQuotasFreeAPIKey,
	QuotaSeed:   llm.NewDBQuotaSeed(repo, "article"), // このキーを使う機能を全部並べる
	APIKey:      os.Getenv("GEMINI_API_KEY"),
	CallTimeout: 5 * time.Minute, // 必須。正常な生成を切らない長さを実測から決める
})
if err != nil {
	return err
}
defer gemini.Close()

client := llm.NewGuardedClient(gemini, repo, "article", logger) // logger は Warn / Error を持つもの

if ok, err := client.Available(ctx); err != nil || !ok {
	return err // 今日の上限か緊急停止。重い準備に入らない
}

text, handle, err := client.CreateChatCompletionWithLog(ctx, &llm.ChatRequest{Prompt: prompt, Memo: "user_id:1"})
if err != nil {
	return err
}
if err := json.Unmarshal([]byte(text), &out); err != nil {
	handle.MarkAsInvalid(ctx, err)
	return err
}
handle.MarkAsValid(ctx)
```

## テーブル

`schema.sql` を MySQL に流します。モデルの `TableName` は `system.llm_function_configs` と `system.llm_logs` なので、`system` データベースに作ります。

機能ごとに `llm_function_configs` へ 1 行入れてから呼びます。行が無い機能は呼ばずに断ります（`config_missing`）。

- `daily_limit`: 当日（アプリのタイムゾーンの 0 時起点）の成功と失敗の合計がこの値に達したら断ります。`-1` は無制限、`0` は 1 回も呼びません
- `is_active`: `0` にするとその機能だけ止まります（緊急停止）
- `max_tokens`: `ChatRequest.MaxTokens` を 0 にしたときに使われます

## 気をつけること

- Gemini の 1 日の上限は太平洋時間の 0 時（日本時間の 16〜17 時）に戻ります。`daily_limit` の数え方（アプリのタイムゾーンの 0 時）とは区切りが違います
- RPD は API キーごとに減ります。1 本のキーを複数の機能で使うなら、`NewDBQuotaSeed` にその機能を全部並べます。並べ漏れると残りを多く見積もり、429 を踏みに行きます
- `CallTimeout` には既定値がありません。長文の生成ほど正常でも時間がかかるので、用途ごとに実測から決めます

## テスト

```
go test ./...
```

`TestManual_*` と `*_Live` は実際の API を呼びます。キーの環境変数が無ければ飛ばされます。呼ぶとそのキーの無料枠を消費します。
