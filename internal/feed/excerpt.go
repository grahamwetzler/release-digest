package feed

import (
	"strings"
	"unicode/utf8"
)

// excerpt cuts text to at most n runes plus an ellipsis, at a word boundary
// when one falls past halfway.
func excerpt(text string, n int) string {
	if utf8.RuneCountInString(text) <= n {
		return text
	}
	head := string([]rune(text)[:n])
	// Cut at a word only past halfway, so an early space can't leave a stub.
	if i := strings.LastIndexByte(head, ' '); i > 0 && utf8.RuneCountInString(head[:i]) > n/2 {
		return strings.TrimRight(head[:i], " ,;:.-") + "…"
	}
	return head + "…"
}
