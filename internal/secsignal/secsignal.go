// Package secsignal finds security signals in release-note text.
package secsignal

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Whitespace includes Unicode spaces as well as ASCII whitespace.
const space = `[\t-\r \x{0085}\p{Z}]+`

var patterns = []struct {
	re        *regexp.Regexp
	canonical string
}{
	{regexp.MustCompile(`(?i)CVE-[0-9]{4}-[0-9]{4,}`), ""},
	{regexp.MustCompile(`(?i)GHSA-[23456789cfghjmpqrvwx]{4}-[23456789cfghjmpqrvwx]{4}-[23456789cfghjmpqrvwx]{4}`), ""},
	{regexp.MustCompile(`(?i)vulnerabilit(?:y|ies)`), "vulnerability"},
	{regexp.MustCompile(`(?i)security` + space + `fix(?:es)?`), "security fix"},
	{regexp.MustCompile(`(?i)security` + space + `advisor(?:y|ies)`), "security advisory"},
	{regexp.MustCompile(`(?i)security` + space + `issues?`), "security issue"},
	{regexp.MustCompile(`(?i)remote` + space + `code` + space + `execution|rce`), "remote code execution"},
	{regexp.MustCompile(`(?i)privilege` + space + `escalation`), "privilege escalation"},
	{regexp.MustCompile(`(?i)cross(?:-|` + space + `)site` + space + `scripting|xss`), "cross-site scripting"},
	{regexp.MustCompile(`(?i)sql` + space + `injection`), "sql injection"},
	{regexp.MustCompile(`(?i)(?:path|directory)` + space + `traversal`), "path traversal"},
	{regexp.MustCompile(`(?i)server(?:-|` + space + `)side` + space + `request` + space + `forgery|ssrf`), "server-side request forgery"},
	{regexp.MustCompile(`(?i)(?:authentication|auth)` + space + `bypass`), "authentication bypass"},
}

// Scan returns the security signals found in text, in order of first
// appearance, without duplicates. It returns nil when there are none.
func Scan(text string) []string {
	type match struct {
		start, end int
		signal     string
	}
	var matches []match
	for _, p := range patterns {
		for _, loc := range p.re.FindAllStringIndex(text, -1) {
			before, _ := utf8.DecodeLastRuneInString(text[:loc[0]])
			after, _ := utf8.DecodeRuneInString(text[loc[1]:])
			if unicode.IsLetter(before) || unicode.IsDigit(before) || unicode.IsLetter(after) || unicode.IsDigit(after) {
				continue
			}
			signal := p.canonical
			if signal == "" {
				signal = strings.ToUpper(text[loc[0]:loc[1]])
				if strings.HasPrefix(signal, "GHSA-") {
					signal = "GHSA-" + strings.ToLower(signal[5:])
				}
			}
			matches = append(matches, match{loc[0], loc[1], signal})
		}
	}
	slices.SortFunc(matches, func(a, b match) int {
		if a.start != b.start {
			return a.start - b.start
		}
		return b.end - a.end
	})
	var out []string
	seen := make(map[string]bool)
	for _, m := range matches {
		if !seen[m.signal] {
			out = append(out, m.signal)
			seen[m.signal] = true
		}
	}
	return out
}
