package llm

import (
	"fmt"
	"regexp"
	"strings"
)

// TagPattern は LLM の応答から <tag>中身</tag> の中身を取る正規表現を作る。
// 応答が途中で切れても拾えるよう、閉じタグが無ければ末尾までをマッチさせる。
// タグの前後の空白は許容し、大文字小文字を無視して改行をまたいで読む。
func TagPattern(tag string) *regexp.Regexp {
	return regexp.MustCompile(fmt.Sprintf(`(?is)<%s>\s*(.*?)\s*(?:</%s>|$)`, tag, tag))
}

// TagValue は re（TagPattern で作ったもの）に合う最初のタグの中身を返す。無ければ空文字。
func TagValue(content string, re *regexp.Regexp) string {
	matches := re.FindStringSubmatch(content)
	if len(matches) > 1 {
		return strings.TrimSpace(matches[1])
	}
	return ""
}
