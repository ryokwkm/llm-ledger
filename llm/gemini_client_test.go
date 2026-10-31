package llm

import (
	"strings"
	"testing"

	"github.com/google/generative-ai-go/genai"
)

// validateFinishReason: Stop 以外の FinishReason はすべて途中切れ・ブロック扱いでエラーにする。
// MaxTokens で止まった本文を成功扱いすると、尻切れのまま保存される。
func TestValidateFinishReason(t *testing.T) {
	tests := []struct {
		name    string
		reason  genai.FinishReason
		wantErr bool
	}{
		{name: "Stopは正常終了", reason: genai.FinishReasonStop, wantErr: false},
		{name: "MaxTokensは切断でエラー", reason: genai.FinishReasonMaxTokens, wantErr: true},
		{name: "Safetyはブロックでエラー", reason: genai.FinishReasonSafety, wantErr: true},
		{name: "Recitationはブロックでエラー", reason: genai.FinishReasonRecitation, wantErr: true},
		{name: "Otherはエラー", reason: genai.FinishReasonOther, wantErr: true},
		{name: "Unspecifiedは完了保証がないためエラー", reason: genai.FinishReasonUnspecified, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateFinishReason(tt.reason)
			if tt.wantErr && err == nil {
				t.Error("エラーを期待したが nil が返った")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("正常終了を期待したがエラー: %v", err)
			}
			if tt.wantErr && err != nil && !strings.Contains(err.Error(), tt.reason.String()) {
				t.Errorf("エラーメッセージに FinishReason 名（%s）が含まれない: %v", tt.reason, err)
			}
		})
	}
}

// joinTextParts: Candidate の Parts を全走査してテキストを連結する。
// Gemini 3.8 Flash は本文を複数 Part に分割して返すことがあり、先頭だけを読むと断片になる。
// FinishReason=Stop で返るので validateFinishReason ではすり抜ける。
func TestJoinTextParts(t *testing.T) {
	tests := []struct {
		name    string
		parts   []genai.Part
		want    string
		wantErr bool
	}{
		{
			name:  "単一Partはそのまま返す",
			parts: []genai.Part{genai.Text("本文です")},
			want:  "本文です",
		},
		{
			name:  "複数Partは区切りを入れずに連結する",
			parts: []genai.Part{genai.Text("今日は朝から"), genai.Text("雨が降っている。"), genai.Text("傘を持って出かけた。")},
			want:  "今日は朝から雨が降っている。傘を持って出かけた。",
		},
		{
			name:  "先頭が非テキストPartでも後続のテキストを拾う",
			parts: []genai.Part{genai.Blob{MIMEType: "image/png", Data: []byte{0x89, 0x50}}, genai.Text("本文です")},
			want:  "本文です",
		},
		{
			name:  "テキストPartが混在していても非テキストは飛ばす",
			parts: []genai.Part{genai.Text("前半"), genai.Blob{MIMEType: "image/png", Data: []byte{0x89}}, genai.Text("後半")},
			want:  "前半後半",
		},
		{
			name:    "テキストPartが1つも無ければエラー",
			parts:   []genai.Part{genai.Blob{MIMEType: "image/png", Data: []byte{0x89, 0x50}}},
			wantErr: true,
		},
		{
			name:    "Partが空ならエラー",
			parts:   []genai.Part{},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := joinTextParts(tt.parts)
			if tt.wantErr {
				if err == nil {
					t.Errorf("エラーを期待したが nil が返った（got=%q）", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("予期しないエラー: %v", err)
			}
			if got != tt.want {
				t.Errorf("連結結果が違う\n got: %q\nwant: %q", got, tt.want)
			}
		})
	}
}

// modelFor: MaxTokens の指定はその呼び出しにだけ効く。共有のモデルへ書くと、
// 指定の無い次の呼び出しに前の値が残り、並行に呼ぶと値を取り合う。
func TestGeminiClient_ModelFor(t *testing.T) {
	c := &GeminiClient{model: &genai.GenerativeModel{}}

	if got := c.modelFor(100).MaxOutputTokens; got == nil || *got != 100 {
		t.Fatalf("modelFor(100).MaxOutputTokens = %v, want 100", got)
	}
	if got := c.modelFor(0).MaxOutputTokens; got != nil {
		t.Errorf("modelFor(0).MaxOutputTokens = %d, want nil（前の呼び出しの値が残っている）", *got)
	}
	if got := c.model.MaxOutputTokens; got != nil {
		t.Errorf("共有のモデルの MaxOutputTokens が %d に書き換わった", *got)
	}
}
