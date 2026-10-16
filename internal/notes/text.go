// Package notes resolves the raw text the LLM sees for a release: its body,
// the matching changelog section, or the commit log since its predecessor,
// truncated to the input budget (PLAN.md §3.2).
package notes

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"release-digest/internal/github"
)

// Notes sources, as stored in releases.notes_source. "" is no notes.
const (
	SourceBody      = "release_body"
	SourceChangelog = "changelog"
	SourceCompare   = "compare"
)

// MinBody is how long a release body must be, once boilerplate is removed,
// to count as substantive.
const MinBody = 40

var (
	// pointer marks a body that defers to the changelog.
	pointer = regexp.MustCompile(`(?i)see (the )?changelog|changelog\.md`)
	// fullChangelog is GitHub's generated "**Full Changelog**: <compare URL>"
	// line, all a generated body has when no pull requests were merged.
	fullChangelog = regexp.MustCompile(`(?im)^[ \t]*\**full changelog\**:?\**[ \t]*\S*[ \t]*$`)
)

// substance is the body without the generated compare-link line.
func substance(body string) string {
	return strings.TrimSpace(fullChangelog.ReplaceAllString(body, ""))
}

// Substantive reports whether a release body stands on its own as notes: at
// least MinBody characters besides boilerplate, and not a pointer to the
// changelog (§3.2).
func Substantive(body string) bool {
	s := substance(body)
	return utf8.RuneCountInString(s) >= MinBody && !pointer.MatchString(s)
}

// sentenceEnd splits text into sentences: a line break, or ., ! or ?
// before a space or the end ("CHANGELOG.md" stays whole).
var sentenceEnd = regexp.MustCompile(`[.!?]+(\s|$)|\n`)

// beyondPointer reports whether text says anything besides pointing at the
// changelog: some sentence with a letter in it doesn't mention it. "Bug
// fixes. See CHANGELOG.md" does; "See CHANGELOG.md for details." doesn't.
func beyondPointer(text string) bool {
	for _, s := range sentenceEnd.Split(text, -1) {
		if !pointer.MatchString(s) && strings.IndexFunc(s, unicode.IsLetter) >= 0 {
			return true
		}
	}
	return false
}

// TruncatedMarker ends notes cut to the input budget.
const TruncatedMarker = "\n\n[truncated]"

var headingStart = regexp.MustCompile(`(?m)^ {0,3}#{1,6}[ \t]`)

// Truncate cuts text to at most n characters, including the marker. The cut
// falls at the last heading in the second half of the budget, or else at
// the last line break.
func Truncate(text string, n int) string {
	if n <= 0 || utf8.RuneCountInString(text) <= n {
		return text
	}
	limit := n - utf8.RuneCountInString(TruncatedMarker)
	head := string([]rune(text)[:max(limit, 0)])
	cut := -1
	for _, loc := range headingStart.FindAllStringIndex(head, -1) {
		if loc[0] > len(head)/2 {
			cut = loc[0]
		}
	}
	if cut < 0 {
		cut = strings.LastIndexByte(head, '\n')
	}
	if cut > 0 {
		head = head[:cut]
	}
	return strings.TrimRight(head, " \t\r\n") + TruncatedMarker
}

// Compare-fallback limits (§3.2).
const (
	MaxCompareCommits = 250 // a larger range likely has an unrelated predecessor
	MaxCommits        = 100 // subjects kept, newest first
)

// CommitNotes lists commit subjects newest first, dropping merges,
// dependency bumps and bot commits. It is "" if nothing is left.
func CommitNotes(commits []github.Commit) string {
	var lines []string
	for i := len(commits) - 1; i >= 0 && len(lines) < MaxCommits; i-- {
		c := commits[i]
		subject, _, _ := strings.Cut(strings.TrimSpace(c.Message), "\n")
		subject = strings.TrimSpace(subject)
		if subject == "" || noise(c, subject) {
			continue
		}
		lines = append(lines, "- "+subject)
	}
	return strings.Join(lines, "\n")
}

func noise(c github.Commit, subject string) bool {
	s := strings.ToLower(subject)
	return c.Parents > 1 ||
		strings.HasPrefix(s, "chore(deps") || strings.HasPrefix(s, "build(deps") ||
		c.AuthorType == "Bot" || strings.HasSuffix(c.Login, "[bot]") || strings.HasSuffix(c.AuthorName, "[bot]")
}
