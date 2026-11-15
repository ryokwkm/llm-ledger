// gemini-model-watch は LiteLLM のモデル一覧から、llm-ledger が無料枠の値を持っていない Gemini のテキストモデルを探し、
// 無料キーで 1 回呼んで無料枠があるものだけを通知の下書き（JSON Lines）として標準出力へ出す。
// issue と Slack への送信は .github/workflows/gemini-model-watch.yml が行う。
package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ryokwkm/llm-ledger/llm"
)

const (
	defaultCatalogURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"
	defaultAPIBase    = "https://generativelanguage.googleapis.com"
	issueTitlePrefix  = "Gemini の新しいモデル（無料枠あり）: "
	catalogKeyPrefix  = "gemini/"
)

//go:embed ignore.txt
var ignoreList string

type config struct {
	catalogURL   string
	apiBase      string
	apiKey       string
	notifiedPath string
	ignoreList   string
}

type catalogEntry struct {
	Mode             string   `json:"mode"`
	OutputModalities []string `json:"supported_output_modalities"`
	Source           string   `json:"source"`
}

type notice struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

type verdict string

const (
	verdictFree       verdict = "無料枠あり"
	verdictNoFreeTier verdict = "無料枠なし"
	verdictNotFound   verdict = "提供なし"
	verdictUnknown    verdict = "判定できず"
)

func main() {
	notifiedPath := flag.String("notified", "", "通知済みの issue のタイトル一覧（1 行 1 件）")
	flag.Parse()

	cfg := config{
		catalogURL:   defaultCatalogURL,
		apiBase:      defaultAPIBase,
		apiKey:       os.Getenv("GEMINI_FREE_API_KEY"),
		notifiedPath: *notifiedPath,
		ignoreList:   ignoreList,
	}
	// 応答の頭が 2 分近く返らないことがあるので、短く切りすぎない
	client := &http.Client{Timeout: 3 * time.Minute}
	if err := run(context.Background(), client, cfg, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "gemini-model-watch:", err)
		os.Exit(1)
	}
}

// run は新しいモデルを探して無料枠を確かめ、無料枠があるものを out へ 1 行 1 件の JSON で書く。
// 判定の経過は log へ書く（公開リポジトリの Actions のログになるので、キーや応答の details は出さない）。
func run(ctx context.Context, client *http.Client, cfg config, out, log io.Writer) error {
	if cfg.apiKey == "" {
		return errors.New("GEMINI_FREE_API_KEY が空です")
	}
	catalog, err := fetchCatalog(ctx, client, cfg.catalogURL)
	if err != nil {
		return err
	}
	notified, err := readLines(cfg.notifiedPath)
	if err != nil {
		return err
	}

	enc := json.NewEncoder(out)
	for _, id := range newTextModels(catalog, skipSet(cfg.ignoreList, notified)) {
		v, err := probe(ctx, client, cfg, id, log)
		if err != nil {
			return fmt.Errorf("%s: %w", id, err)
		}
		if v != verdictFree {
			continue
		}
		if err := enc.Encode(notice{Title: issueTitle(id), Body: issueBody(id, catalog[id])}); err != nil {
			return err
		}
	}
	return nil
}

func fetchCatalog(ctx context.Context, client *http.Client, url string) (map[string]catalogEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("モデル一覧の取得に失敗: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("モデル一覧の取得に失敗: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("モデル一覧の読み込みに失敗: %w", err)
	}
	return decodeCatalog(body)
}

// decodeCatalog は gemini/ のエントリだけを読み、キーから gemini/ を外した ID で引けるようにする。
// 一覧の先頭の sample_spec などはカラムの型が違うので、全体を 1 つの型では読めない。
func decodeCatalog(body []byte) (map[string]catalogEntry, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("モデル一覧が JSON として読めない: %w", err)
	}
	catalog := make(map[string]catalogEntry)
	for key, v := range raw {
		id, ok := strings.CutPrefix(key, catalogKeyPrefix)
		if !ok {
			continue
		}
		var e catalogEntry
		if err := json.Unmarshal(v, &e); err != nil {
			return nil, fmt.Errorf("モデル一覧の %s が読めない: %w", key, err)
		}
		catalog[id] = e
	}
	return catalog, nil
}

func readLines(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("通知済みの一覧が読めない: %w", err)
	}
	return strings.Split(string(b), "\n"), nil
}

// skipSet は確かめなくてよい ID の集合を作る。llm-ledger が無料枠の値を持っているもの・除外リスト・通知済みの 3 つ。
// 通知済みは issue のタイトルから取るので、閉じた issue のモデルも二度と通知しない。
func skipSet(ignore string, notifiedTitles []string) map[string]bool {
	skip := make(map[string]bool)
	for m := range llm.DailyQuotasFreeAPIKey {
		skip[string(m)] = true
	}
	for m := range llm.RateLimitsFreeAPIKey {
		skip[string(m)] = true
	}
	for _, line := range strings.Split(ignore, "\n") {
		id, _, _ := strings.Cut(line, "#")
		if id = strings.TrimSpace(id); id != "" {
			skip[id] = true
		}
	}
	for _, title := range notifiedTitles {
		if id, ok := strings.CutPrefix(strings.TrimSpace(title), issueTitlePrefix); ok {
			skip[id] = true
		}
	}
	return skip
}

// newTextModels はテキストを返すチャットのモデルのうち skip に無いものを ID 順に返す。
// 出力の種類が書かれていないエントリも通す（一覧に載ったばかりのモデルはカラムが欠けがちで、見逃すほうが重い）。
func newTextModels(catalog map[string]catalogEntry, skip map[string]bool) []string {
	var ids []string
	for id, e := range catalog {
		if e.Mode != "chat" || skip[id] {
			continue
		}
		if slices.ContainsFunc(e.OutputModalities, func(m string) bool { return m != "text" }) {
			continue
		}
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// probe は無料キーでモデルを 1 回呼び、応答から無料枠の有無を判定する。
// キーが使えないときだけ error を返す（全モデルで同じ結果になり、続けても意味が無い）。
func probe(ctx context.Context, client *http.Client, cfg config, id string, log io.Writer) (verdict, error) {
	url := fmt.Sprintf("%s/v1beta/models/%s:generateContent", cfg.apiBase, id)
	payload := []byte(`{"contents":[{"parts":[{"text":"Reply with OK."}]}]}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	// ?key= で渡すと、通信エラーの文言に URL ごとキーが出て公開のログに残る
	req.Header.Set("x-goog-api-key", cfg.apiKey)

	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(log, "::warning::%s: %s（通信エラー: %v）\n", id, verdictUnknown, err)
		return verdictUnknown, nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("応答の読み込みに失敗: %w", err)
	}

	v, err := classify(resp.StatusCode, body)
	if err != nil {
		return "", err
	}
	line := fmt.Sprintf("%s: %s (HTTP %d)", id, v, resp.StatusCode)
	if resp.StatusCode != http.StatusOK {
		line += " " + errorMessage(body)
	}
	if v == verdictUnknown {
		line = "::warning::" + line
	}
	fmt.Fprintln(log, line)
	return v, nil
}

var quotaLimitPattern = regexp.MustCompile(`limit: (\d+)`)

// classify は generateContent の応答を判定する。
// 無料枠の無いモデルを無料キーで呼ぶと 429 になり、本文に "limit: 0" が出る。limit が 1 以上なら枠はあり、
// いまは使い切っているだけ。limit の無い 429（grounding など、無料枠で使えない機能でも出る）は判定しない。
func classify(status int, body []byte) (verdict, error) {
	switch {
	case status == http.StatusOK:
		return verdictFree, nil
	case status == http.StatusTooManyRequests:
		limits := quotaLimitPattern.FindAllSubmatch(body, -1)
		if len(limits) == 0 {
			return verdictUnknown, nil
		}
		for _, m := range limits {
			if n, _ := strconv.Atoi(string(m[1])); n > 0 {
				return verdictFree, nil
			}
		}
		return verdictNoFreeTier, nil
	case status == http.StatusNotFound:
		return verdictNotFound, nil
	case status == http.StatusUnauthorized || status == http.StatusForbidden || bytes.Contains(body, []byte("API_KEY_INVALID")):
		return "", fmt.Errorf("キーが使えません (HTTP %d): %s", status, errorMessage(body))
	default:
		return verdictUnknown, nil
	}
}

// errorMessage は応答の error.message だけを短く取り出す。details にはプロジェクトの情報が入りうるので出さない。
func errorMessage(body []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil || e.Error.Message == "" {
		return ""
	}
	// 改行が入ると ::warning:: の行が割れるので 1 行に詰める
	return strings.Join(strings.Fields(e.Error.Message), " ")
}

func issueTitle(id string) string {
	return issueTitlePrefix + id
}

func issueBody(id string, e catalogEntry) string {
	source := e.Source
	if source == "" {
		source = "（一覧に出典なし）"
	}
	return fmt.Sprintf(`LiteLLM のモデル一覧に `+"`%s`"+` が載り、無料キーでの呼び出しが通りました（無料枠あり）。

- 出典: %s
- 使うなら: RPM・RPD を AI Studio のレート制限の画面で確かめ、`+"`llm/gemini_client.go`"+` の定数・`+"`RateLimitsFreeAPIKey`"+`・`+"`DailyQuotasFreeAPIKey`"+` に足す
- 要らないなら: この issue を閉じる（閉じた issue のモデルも通知済みとして扱い、二度と通知しない）

.github/workflows/gemini-model-watch.yml が自動で立てた issue です。
`, id, source)
}
