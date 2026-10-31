package llm

import (
	"encoding/json"
	"testing"
)

func TestRepairStrayLineHeads(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "行頭の数字 1 字が落ちる（本番で観測した形）",
			input: "{\n  \"category\": \"A\",\n5      \"rating\": \"高\"\n}",
			want:  "{\n  \"category\": \"A\",\n      \"rating\": \"高\"\n}",
		},
		{
			name:  "行頭の記号 * が落ちる",
			input: "{\n  \"a\": 1,\n*  \"b\": 2\n}",
			want:  "{\n  \"a\": 1,\n  \"b\": 2\n}",
		},
		{
			name:  "インデントの後ろの > が落ちる",
			input: "{\n  \"a\": 1,\n  >\"b\": 2\n}",
			want:  "{\n  \"a\": 1,\n  \"b\": 2\n}",
		},
		{
			name:  "閉じ括弧の前の . が落ちる",
			input: "{\n  \"a\": [1, 2\n  .]\n}",
			want:  "{\n  \"a\": [1, 2\n  ]\n}",
		},
		{
			name:  "UTF-8 破損の ã が落ちる",
			input: "{\n  \"a\": 1,\nã  \"b\": 2\n}",
			want:  "{\n  \"a\": 1,\n  \"b\": 2\n}",
		},
		{
			name:  "キーの開き引用符が _ に置き換わり閉じ引用符が落ちる（本番で観測した形）",
			input: "{\n  \"summary\": \"x\",\n      _status: \"通常\",\n  \"result\": []\n}",
			want:  "{\n  \"summary\": \"x\",\n      \"status\": \"通常\",\n  \"result\": []\n}",
		},
		{
			name:  "キーの引用符が * に置き換わる（プロンプトの **key** 表記のなぞり）",
			input: "{\n  \"expected_win_rate\": 15,\n      *expected_top3_rate*: 45,\n  \"reasoning\": \"x\"\n}",
			want:  "{\n  \"expected_win_rate\": 15,\n      \"expected_top3_rate\": 45,\n  \"reasoning\": \"x\"\n}",
		},
		{
			name:  "正しい JSON は触らない（行頭の数値・負数・true も残る）",
			input: "{\n  \"a\": [\n    1,\n    -2,\n    true\n  ],\n  \"b\": \"x\"\n}",
			want:  "{\n  \"a\": [\n    1,\n    -2,\n    true\n  ],\n  \"b\": \"x\"\n}",
		},
		{
			name:  "直しても壊れたままなら元の文字列を返す",
			input: "{\n  \"a\": \n}",
			want:  "{\n  \"a\": \n}",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RepairStrayLineHeads(tt.input)
			if got != tt.want {
				t.Errorf("RepairStrayLineHeads() =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

func TestRepairStrayLineHeads_直した結果はパースできる(t *testing.T) {
	input := "{\n  \"category\": \"A\",\n5      \"rating\": \"高\"\n}"
	var v map[string]string
	if err := json.Unmarshal([]byte(RepairStrayLineHeads(input)), &v); err != nil {
		t.Fatalf("修復後もパースできない: %v", err)
	}
	if v["rating"] != "高" {
		t.Errorf("course_aptitude = %q, want 良", v["course_aptitude"])
	}
}

func TestEscapeNewlinesInJSONStrings(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{
			name: "生の改行を含むJSON文字列",
			input: `{
  "title": "テスト",
  "body": "一行目
二行目"
}`,
		},
		{
			name:  "既にエスケープ済みの改行",
			input: `{"title": "テスト", "body": "一行目\n二行目"}`,
		},
		{
			name: "複数の生の改行",
			input: `{
  "title": "タイトル",
  "body": "セクション1

セクション2

セクション3"
}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := EscapeNewlinesInJSONStrings(tt.input)

			var data map[string]interface{}
			if err := json.Unmarshal([]byte(result), &data); err != nil {
				t.Errorf("JSON parse failed after escaping: %v\ninput: %s\nresult: %s", err, tt.input, result)
			}
		})
	}
}
