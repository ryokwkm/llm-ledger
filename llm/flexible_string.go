package llm

import (
	"encoding/json"
	"fmt"
	"strings"
)

// FlexibleString は LLM が文字列・文字列配列のどちらで返しても受理する文字列型。
// 配列で返された場合は改行で結合して単一文字列として扱う。
type FlexibleString string

func (f *FlexibleString) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*f = FlexibleString(s)
		return nil
	}

	var arr []string
	if err := json.Unmarshal(b, &arr); err != nil {
		return fmt.Errorf("文字列または文字列配列として解釈できません: %w", err)
	}
	*f = FlexibleString(strings.Join(arr, "\n"))
	return nil
}
