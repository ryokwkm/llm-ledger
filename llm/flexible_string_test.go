package llm

import (
	"encoding/json"
	"testing"
)

func TestFlexibleString_UnmarshalJSON(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{
			name:  "通常の文字列",
			input: `"- 【導入】...\n- 【本論】..."`,
			want:  "- 【導入】...\n- 【本論】...",
		},
		{
			name:  "文字列配列は改行で結合",
			input: `["- 【導入】背景", "- 【本論】要点", "- 【結論】まとめ"]`,
			want:  "- 【導入】背景\n- 【本論】要点\n- 【結論】まとめ",
		},
		{
			name:  "空文字列",
			input: `""`,
			want:  "",
		},
		{
			name:  "空配列",
			input: `[]`,
			want:  "",
		},
		{
			name:    "数値はエラー",
			input:   `123`,
			wantErr: true,
		},
		{
			name:    "オブジェクトはエラー",
			input:   `{"a": 1}`,
			wantErr: true,
		},
		{
			name:    "数値配列はエラー",
			input:   `[1, 2]`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got FlexibleString
			err := json.Unmarshal([]byte(tt.input), &got)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("エラーを期待したが nil が返った (got: %q)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("予期しないエラー: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFlexibleString_UnmarshalJSON_StructField(t *testing.T) {
	type draft struct {
		Plot FlexibleString `json:"plot"`
	}

	t.Run("構造体フィールドで配列を受理", func(t *testing.T) {
		var d draft
		if err := json.Unmarshal([]byte(`{"plot": ["a", "b"]}`), &d); err != nil {
			t.Fatalf("予期しないエラー: %v", err)
		}
		if string(d.Plot) != "a\nb" {
			t.Errorf("got %q, want %q", d.Plot, "a\nb")
		}
	})
}
