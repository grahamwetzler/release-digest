package advisory

import (
	"regexp"
	"strings"
)

// Text evidence (§3.4): a line that mentions an advisory counts as fixing it
// only in an allowlisted affirmative form and with no negation, hedge or
// future marker anywhere on it. The rule is deliberately conservative: a
// false negative only drops this signal, a false positive sends a wrong
// urgent alert.
var (
	// listPrefix is list markers, Markdown emphasis and an optional
	// "Security:" or "[security]" prefix before a line's first word.
	listPrefix = regexp.MustCompile(`(?i)^(\s*([-*+>]|\d+[.)])\s+)*\s*(\[security\]\s*:?\s*|security\s*:\s*)?`)
	fixVerb    = regexp.MustCompile(`(?i)^(fix|fixes|patch|patches|resolve|resolves|address|addresses|mitigate|mitigates)\b`)
	completed  = regexp.MustCompile(`(?i)\b(fixed|patched|resolved|addressed|mitigated)\b`)
	denied     = regexp.MustCompile(`(?i)\b(not|no|never|don't|doesn't|won't|cannot|can't|without|unfixed|still|partially|partial|incomplete|pending|upcoming|planned|will|would|should|going\s+to|next|future|later|soon|known\s+issues?|upstream|workaround|isnt|wasnt|arent|hasnt|hadnt|didnt|doesnt|dont|wont|cant|couldnt|havent)\b|\w+n't\b`)
	emphasis   = strings.NewReplacer("*", "", "_", "", "`", "", "’", "'", "‘", "'")
)

// TextFixes reports whether notes affirm that this release fixes id: some
// line mentioning id passes both the allowlist and the denylist.
func TextFixes(notes, id string) bool {
	for _, line := range strings.Split(notes, "\n") {
		if strings.Contains(strings.ToUpper(line), strings.ToUpper(id)) && lineFixes(line) {
			return true
		}
	}
	return false
}

func lineFixes(line string) bool {
	l := emphasis.Replace(line)
	if denied.MatchString(l) {
		return false
	}
	rest := listPrefix.ReplaceAllString(l, "")
	return fixVerb.MatchString(rest) || completed.MatchString(l)
}
