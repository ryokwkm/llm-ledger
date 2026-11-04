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

text, handle, err := client.CreateChatCompletionWithLog(ctx, &llm.ChatRequest{Prompt: prompt, Memo: "user_id:1", GroupID: "myapp-c7"}) // GroupID は省略してよい
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

## group_id（集計用の 1 列）

`llm_logs.group_id` は、呼び出しを使う側の単位（企業・サイトなど）で数えるための 1 列です。`ChatRequest.GroupID` で呼び出しごとに渡します。空なら NULL です。

中身の意味は llm-ledger は持ちません。使う側の製品が、集計用のテーブルを `system` に自由なカラムで作り、`group_id` で JOIN して数えます。llm-ledger はこのテーブルを読みも書きもしません。

```sql
-- 例: 企業とサイトで数える製品
CREATE TABLE IF NOT EXISTS `system`.`myapp_llm_groups` (
    `group_id` VARCHAR(64) NOT NULL,   -- 例: myapp-c7-s3
    `company_id` BIGINT UNSIGNED NOT NULL,
    `site_id` BIGINT UNSIGNED NULL,
    PRIMARY KEY (`group_id`)
);

SELECT g.company_id, COUNT(*), SUM(l.input_tokens), SUM(l.output_tokens)
FROM `system`.`llm_logs` l JOIN `system`.`myapp_llm_groups` g ON g.group_id = l.group_id
GROUP BY g.company_id;
```

- `llm_logs` には今後カラムを足しません。数えたい属性が増えたら、その製品の集計用のテーブルにカラムを足します
- 集計用のテーブルの名前には製品名を入れます。カラムは製品ごとに違うので、同じ名前だと `CREATE TABLE IF NOT EXISTS` が先に作った製品の定義のまま黙って通ります
- `group_id` の値は製品名で始めます（`func_feature` と同じ）。`llm_logs` は製品をまたいで共有するので、同じ値を使うと JOIN で他の製品の行が混ざります。64 文字までです
- 値は中身から一意に決まる文字列（例 `myapp-c7-s3`）にすると、呼ぶ前に連番を取りに行かずに済みます。集計用のテーブルの行は、企業やサイトを作るとき（か初めて呼ぶとき）に INSERT IGNORE で入れます
- 呼び出しごとに変わる値（記事 ID・利用者 ID など）は入れません。集計用のテーブルが呼び出しの数だけ増えます。そうした値は `memo` に書きます

## 版を上げるとき

| 版 | DB に流すもの |
|---|---|
| v0.2.0 | `group_id` を使うなら `migrations/v0.2.0.sql`（`llm_logs` に `group_id` を足す）。使わないなら何も流さなくてよい。`GroupID` が空の呼び出しは `group_id` を INSERT に含めないので、列の無い DB でもそのまま動く |

## 気をつけること

- Gemini の 1 日の上限は太平洋時間の 0 時（日本時間の 16〜17 時）に戻ります。`daily_limit` の数え方（アプリのタイムゾーンの 0 時）とは区切りが違います
- RPD は API キーごとに減ります。1 本のキーを複数の機能で使うなら、`NewDBQuotaSeed` にその機能を全部並べます。並べ漏れると残りを多く見積もり、429 を踏みに行きます
- `CallTimeout` には既定値がありません。長文の生成ほど正常でも時間がかかるので、用途ごとに実測から決めます

## テスト

```
go test ./...
```

`TestManual_*` と `*_Live` は実際の API を呼びます。キーの環境変数が無ければ飛ばされます。呼ぶとそのキーの無料枠を消費します。
