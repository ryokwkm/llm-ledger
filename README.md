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
| `FallbackGeminiClient` | 試す順に並べたモデル（chain）を上から試し、無料枠の超過・提供終了・無応答なら次のモデルへ落ちる。1 日の上限（RPD）を使い切ったモデルは送る前に飛ばす。分あたりの上限（RPM）は API キーとモデルの組ごとに間隔を空ける。`PaidAPIKey` を渡すと、無料キーの chain を全段使い切ったときに課金キーで送り直す |
| `PaidKeyFallback` | 通信の方式を問わない課金キーへの切り替え。自前の REST クライアントのように、呼び出しごとに API キーを渡せるもの向け |
| `NewDBQuotaSeed` | 常駐プロセスが再起動しても、その日にすでに使った回数を `llm_logs` から取り戻す |
| `OpenAICompatClient` | OpenAI 互換の API（DeepSeek など）を同じインターフェースで呼ぶ |
| `LLMRepository.Purge` | 古いログを条件（期間・`group_id`・`func_feature`）でチャンクに分けて消す。レコードごとか本文だけかを選べ、動いてよい時間帯の外では動かない |

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

## 課金キーへの切り替え

無料キーの回数の枠（1 日・1 分）を使い切ったとき、課金した API キーで同じ要求を送り直して処理を止めません。課金キーを渡さなければ、動きは今までと同じです。

### FallbackGeminiClient で使う

`FallbackConfig` に `PaidAPIKey` と `PaidChain` を足します。

```go
gemini, err := llm.NewFallbackGeminiClient(ctx, llm.FallbackConfig{
	// ...（無料キーの設定は今までどおり）
	PaidAPIKey: os.Getenv("GEMINI_API_KEY_PAID"), // 空なら切り替えない
	PaidChain:  []llm.GeminiModel{llm.ModelGemini31FlashLite}, // 課金キーで試すモデル。単価が違うので明示する
})
```

- 切り替えるのは、無料キーの chain が**全段とも**枠切れ（日次の枠切れで送れない・429・提供終了）で答えられなかったときだけです。上位モデルの枠切れで無料の Lite へ落ちるのは今までどおりで、課金キーは使いません
- 分あたりの枠切れは、今までどおり retry_delay を最大 30 秒待って無料キーで試し直し、それでも通らなければ課金キーへ切り替えます
- 無応答（`CallTimeout`）が混ざったとき・枠切れ以外のエラーのときは切り替えません
- 課金キーには `RateLimits`・`DailyQuotas`・`QuotaSeed` を効かせません。`CallTimeout` は同じ値を使います

### 自前のクライアントで使う（PaidKeyFallback）

呼び出しごとに API キーを渡せるクライアント（REST を直接呼ぶものなど）は、`PaidKeyFallback` をプロセスで 1 つ作って使い回します。呼び出しごとに作ると `Cooldown` が効きません。

```go
keys, err := llm.NewPaidKeyFallback(llm.PaidKeyFallbackConfig{
	FreeKey:     os.Getenv("GEMINI_API_KEY"),
	PaidKey:     os.Getenv("GEMINI_API_KEY_PAID"), // 空なら切り替えない
	IsExhausted: func(err error) bool { /* 自分のエラーの型で 429 かを判定する */ },
	Cooldown:    30 * time.Minute, // 枠切れを受けたモデルで、無料キーを試し直すまでの間
})

paid, err := keys.Call(ctx, model, func(ctx context.Context, apiKey string) error {
	body, err = send(ctx, apiKey, model, req)
	return err
})
// paid が true なら、ChatResponse.ActualModel に llm.PaidModelName(model) を入れて GuardedClient へ返す
```

- 無料キーの枠切れはモデルごとに覚えます。生成のモデルが枠切れでも、埋め込みのモデルは無料キーから試します
- `FallbackGeminiClient` は送る前に日次の枠切れを判定して飛ばすので、`Cooldown` を持ちません。`PaidKeyFallback` は判定の手段が 429 しかないため、`Cooldown` が無いと呼ぶたびに無料キーで 429 を 1 往復受けます

### 課金キーで呼んだ回数を数える

課金キーで呼んだ記録は、`llm_logs.model_name` が `paid:` で始まります（例: `paid:gemini-3.1-flash-lite`。カラムは足していません）。

```sql
SELECT DATE(created_at), func_feature, model_name, COUNT(*), SUM(input_tokens), SUM(output_tokens)
FROM `system`.`llm_logs`
WHERE model_name LIKE 'paid:%' AND status = 1
GROUP BY DATE(created_at), func_feature, model_name;
```

- `NewDBQuotaSeed` は `paid:` の記録を無料キーの消費に数えません
- `daily_limit` は課金キーの呼び出しも数えます。課金キーで呼ぶ回数も機能ごとに `daily_limit` で止まるので、1 日の費用の上限は「`daily_limit` × 1 回の最大の料金」です
- 課金キーでも失敗した呼び出しは、今までどおり起点のモデル名で `failed` として残ります（エラー文に課金キーで失敗したことが入ります）
- `GuardedClient` を通さない呼び出し（埋め込みなど）は `llm_logs` に残りません

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

## 古いログを消す

いつ・どの条件で消すかはサービスごとに違うので、使う側の usecase が `PurgeRule` を組み立てて `Purge` を呼びます。起動は使う側の cron などで、`Purge` は渡された時間帯の外では何もせず、時間帯の終わりが来たらチャンクの間で止まります。止まっても、残りは次に呼んだときに条件に当たって消えます。

```go
res, err := repo.Purge(ctx, llm.PurgeRule{
	Before:      time.Now().AddDate(-1, 0, 0), // created_at がこれより前のものを消す
	Target:      llm.PurgeRecords,             // レコードごと消す。本文だけなら llm.PurgeContent
	GroupPrefix: "myapp-",                     // 任意。GroupIDs（一致の一覧）・Features（func_feature の一覧）でも絞れる
	ChunkSize:   1000,                         // 任意。省くと 500
	Window:      llm.PurgeWindow{From: "02:00", To: "05:00"}, // 任意。日をまたぐなら From を後に（23:00〜05:00）
})
// res.Affected は消した件数、res.Reason は止まった理由（PurgeCompleted / PurgeOutsideWindow / PurgeWindowEnded）
```

- 消す前に件数だけ知りたいときは、同じ `PurgeRule` を `CountPurgeTargets` に渡します（時間帯は見ません）
- `llm_logs` は製品をまたいで共有するので、`GroupPrefix` か `Features` で自分の製品の行に絞ります
- 条件は全部 AND でつなぎます。時間帯はプロセスのタイムゾーン（`TZ`）で比べます
- `group_id` を条件にしなければ、`group_id` の列が無い v0.1.0 の DB でも動きます
- `PurgeOldContent(ctx, before)` は `Purge` の `PurgeContent` を条件なし・時間帯なしで呼ぶのと同じです

## 版を上げるとき

| 版 | DB に流すもの |
|---|---|
| v0.2.0 | `group_id` を使うなら `migrations/v0.2.0.sql`（`llm_logs` に `group_id` を足す）。使わないなら何も流さなくてよい。`GroupID` が空の呼び出しは `group_id` を INSERT に含めないので、列の無い DB でもそのまま動く |
| v0.3.0 | `Purge` を使うなら `migrations/v0.3.0.sql`（`llm_logs` に `created_at` の index を足す）。流さなくても動くが、期間だけで古いログを探す検索が全件を読む。`PurgeOldContent` は `before` がゼロ値だとエラーを返すようになった |
| v0.4.0 | 何も流さなくてよい（課金キーの記録は既存の `model_name` に載る） |

## 気をつけること

- Gemini の 1 日の上限は太平洋時間の 0 時（日本時間の 16〜17 時）に戻ります。`daily_limit` の数え方（アプリのタイムゾーンの 0 時）とは区切りが違います
- RPD は API キーごとに減ります。1 本のキーを複数の機能で使うなら、`NewDBQuotaSeed` にその機能を全部並べます。並べ漏れると残りを多く見積もり、429 を踏みに行きます
- `CallTimeout` には既定値がありません。長文の生成ほど正常でも時間がかかるので、用途ごとに実測から決めます

## テスト

```
go test ./...
```

`TestManual_*` と `*_Live` は実際の API を呼びます。キーの環境変数が無ければ飛ばされます。呼ぶとそのキーの無料枠を消費します。
