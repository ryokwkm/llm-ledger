package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// LiteLLM の一覧の形を写した見本。先頭の sample_spec は数値のカラムに文字列が入っている。
const catalogFixture = `{
  "sample_spec": {"max_input_tokens": "max input tokens, if the provider specifies it", "mode": "one of chat"},
  "gemini/gemini-9-neon": {"mode": "chat", "supported_output_modalities": ["text"], "rpm": 2000.0, "source": "https://aistudio.google.com/docs/pricing#gemini-9-neon"},
  "gemini/gemini-4-argon": {"mode": "chat", "supported_output_modalities": ["text"], "rpm": 2000},
  "gemini/gemini-9-new": {"mode": "chat"},
  "gemini/gemini-3.8-flash": {"mode": "chat", "supported_output_modalities": ["text"]},
  "gemini/gemini-flash-latest": {"mode": "chat", "supported_output_modalities": ["text"]},
  "gemini/gemini-nano-banana-2.1": {"mode": "image_generation", "supported_output_modalities": ["text", "image"]},
  "gemini/lyria-3.5": {"mode": "chat", "supported_output_modalities": ["audio"]},
  "gemini/gemini-omni-1.2-flash": {"mode": "chat", "supported_output_modalities": ["text", "video", "audio"]},
  "gemini/nano-banana-latest": {},
  "openai/gpt-9": {"mode": "chat", "supported_output_modalities": ["text"]}
}`

func TestNewTextModels(t *testing.T) {
	catalog, err := decodeCatalog([]byte(catalogFixture))
	if err != nil {
		t.Fatalf("decodeCatalog: %v", err)
	}
	skip := map[string]bool{"gemini-3.8-flash": true, "gemini-flash-latest": true}

	got := newTextModels(catalog, skip)

	// 出力の種類が書かれていないもの（gemini-9-new）は見逃すより通知するほうへ倒す
	want := []string{"gemini-4-argon", "gemini-9-neon", "gemini-9-new"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestSkipSet(t *testing.T) {
	notified := []string{issueTitle("gemini-4-carbon"), "関係ない issue"}

	skip := skipSet("# コメント\n\ngemini-flash-latest  # エイリアス\n", notified)

	for _, id := range []string{
		"gemini-3.8-flash",    // DailyQuotasFreeAPIKey にある
		"gemini-2.5-pro",      // RateLimitsFreeAPIKey にだけある
		"gemini-flash-latest", // 除外リスト
		"gemini-4-carbon",     // 通知済み
	} {
		if !skip[id] {
			t.Errorf("%s が除外されていない", id)
		}
	}
	for _, id := range []string{"gemini-9-neon", "関係ない issue", "# コメント", ""} {
		if skip[id] {
			t.Errorf("%q が除外されている", id)
		}
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		want    verdict
		wantErr bool
	}{
		{"200 は無料枠あり", 200, `{"candidates":[]}`, verdictFree, false},
		{"429 で limit: 0 は無料枠なし", 429, quotaError("Quota exceeded for metric: generativelanguage.googleapis.com/generate_content_free_tier_requests, limit: 0, model: gemini-4-argon"), verdictNoFreeTier, false},
		{"429 で limit が 1 以上は枠あり（使い切っているだけ）", 429, quotaError("Quota exceeded for metric: generativelanguage.googleapis.com/generate_content_free_tier_requests, limit: 5, model: gemini-9-neon"), verdictFree, false},
		{"429 で limit が無いものは判定しない", 429, quotaError("You exceeded your current quota, please check your plan and billing details."), verdictUnknown, false},
		{"404 は提供なし", 404, `{"error":{"code":404,"message":"models/x is not found","status":"NOT_FOUND"}}`, verdictNotFound, false},
		{"503 は判定しない", 503, `{"error":{"code":503,"message":"overloaded","status":"UNAVAILABLE"}}`, verdictUnknown, false},
		{"キーが無効なら止める", 400, `{"error":{"code":400,"message":"API key not valid.","status":"INVALID_ARGUMENT","details":[{"reason":"API_KEY_INVALID"}]}}`, "", true},
		{"403 は止める", 403, `{"error":{"code":403,"message":"denied","status":"PERMISSION_DENIED"}}`, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := classify(tt.status, []byte(tt.body))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRun(t *testing.T) {
	var gotKeys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/catalog.json" {
			_, _ = w.Write([]byte(catalogFixture))
			return
		}
		gotKeys = append(gotKeys, r.Header.Get("x-goog-api-key"))
		switch r.URL.Path {
		case "/v1beta/models/gemini-9-neon:generateContent":
			_, _ = w.Write([]byte(`{"candidates":[]}`))
		case "/v1beta/models/gemini-4-argon:generateContent":
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(quotaError("Quota exceeded for metric: x, limit: 0, model: gemini-4-argon")))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":404,"message":"not found"}}`))
		}
	}))
	defer srv.Close()

	notified := filepath.Join(t.TempDir(), "notified.txt")
	if err := os.WriteFile(notified, []byte("関係ない issue\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config{
		catalogURL:   srv.URL + "/catalog.json",
		apiBase:      srv.URL,
		apiKey:       "test-key",
		notifiedPath: notified,
		ignoreList:   "gemini-flash-latest\n",
	}
	var out, log bytes.Buffer

	if err := run(context.Background(), srv.Client(), cfg, &out, &log); err != nil {
		t.Fatalf("run: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("通知は hydrogen の 1 件だけのはず: %q", out.String())
	}
	var n notice
	if err := json.Unmarshal([]byte(lines[0]), &n); err != nil {
		t.Fatalf("出力が JSON でない: %v", err)
	}
	if n.Title != issueTitle("gemini-9-neon") {
		t.Errorf("title = %q", n.Title)
	}
	if !strings.Contains(n.Body, "https://aistudio.google.com/docs/pricing#gemini-9-neon") {
		t.Errorf("本文に出典が無い: %q", n.Body)
	}
	if !strings.Contains(log.String(), "gemini-4-argon: 無料枠なし") {
		t.Errorf("argon の判定がログに無い: %q", log.String())
	}
	for _, k := range gotKeys {
		if k != "test-key" {
			t.Errorf("キーがヘッダで渡っていない: %q", k)
		}
	}
	if strings.Contains(log.String(), "test-key") {
		t.Error("ログにキーが出ている")
	}
}

func TestRunRequiresAPIKey(t *testing.T) {
	err := run(context.Background(), http.DefaultClient, config{}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("キーが空なのにエラーにならない")
	}
}

func quotaError(message string) string {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"code": 429, "message": message, "status": "RESOURCE_EXHAUSTED"}})
	return string(b)
}
