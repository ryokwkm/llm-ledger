package llm

import (
	"encoding/json"
	"regexp"
	"strings"
)

// reStrayLineHead は行頭（インデントの直後）に 1 文字だけ紛れ込んだ異物にマッチする。
// Gemini は構造の正しい JSON の行頭へ `5` `*` `.` `>` や UTF-8 破損の `ã` を 1 字混ぜることがあり、
// 整形済み JSON で「1 字 + キーの引用符・括弧」が並ぶ行は正しい JSON には現れない。
var reStrayLineHead = regexp.MustCompile(`(?m)^([ \t]*)[^\s"{}\[\],:]([ \t]*["{}\[\]])`)

// reBrokenKeyQuote は引用符が落ちた・異物に置き換わったキーにマッチする（`_status:` `*total_score*:`）。
// `*key*` はプロンプトの `**key**` 表記のなぞり。整形済み JSON のキーは必ず引用符で始まるので、
// 行頭の snake_case の語に `:` が続く行は正しい JSON には現れない。
var reBrokenKeyQuote = regexp.MustCompile(`(?m)^([ \t]*)[^\s"{}\[\],:a-z0-9]?([a-z][a-z0-9_]*[a-z0-9])[^\s"{}\[\],:a-z0-9]?([ \t]*:)`)

// RepairStrayLineHeads は LLM 出力の JSON から行頭の崩れ（異物 1 字・キーの引用符の欠落）を直す。
// 元から正しい JSON と、直してもパースできないものは元の文字列のまま返す。
func RepairStrayLineHeads(s string) string {
	if json.Valid([]byte(s)) {
		return s
	}
	repaired := reStrayLineHead.ReplaceAllString(s, "$1$2")
	repaired = reBrokenKeyQuote.ReplaceAllString(repaired, `$1"$2"$3`)
	if !json.Valid([]byte(repaired)) {
		return s
	}
	return repaired
}

// EscapeNewlinesInJSONStrings は JSON の文字列値の中に生で入った改行（\n・\r）をエスケープする。
// LLM は本文のような長い値を改行入りのまま返すことがあり、そのままでは JSON としてパースできない。
func EscapeNewlinesInJSONStrings(s string) string {
	var result strings.Builder
	inString := false
	prevChar := rune(0)

	for _, ch := range s {
		if ch == '"' && prevChar != '\\' {
			inString = !inString
			result.WriteRune(ch)
		} else if inString && ch == '\n' {
			result.WriteString("\\n")
		} else if inString && ch == '\r' {
			result.WriteString("\\r")
		} else {
			result.WriteRune(ch)
		}
		prevChar = ch
	}
	return result.String()
}
