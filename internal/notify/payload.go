package notify

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"release-digest/internal/store"
	"release-digest/internal/summarize"
)

// Body limits (docs/security-alerts.md): ntfy turns a message over 4,096 bytes into an
// attachment, so the body stays well below.
const (
	BodyBudget  = 3500 // UTF-8 bytes
	ReasonLimit = 300  // bytes of escaped security_reason
	MaxActions  = 2    // advisory buttons
)

// AdvisoryURL is where an advisory's page is.
const AdvisoryURL = "https://github.com/advisories/"

// Payload is an alert as stored in the outbox and sent as is.
type Payload struct {
	Title    string   `json:"title"`
	Priority int      `json:"priority"`
	Tags     []string `json:"tags"`
	Click    string   `json:"click"`
	Actions  []Action `json:"actions,omitempty"`
	Body     string   `json:"body"`
}

// Action is a view button.
type Action struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

// Encode is the stored form of p.
func (p Payload) Encode() string {
	b, _ := json.Marshal(p) // strings, ints and slices always marshal
	return string(b)
}

// DecodePayload reads a stored payload.
func DecodePayload(s string) (Payload, error) {
	var p Payload
	err := json.Unmarshal([]byte(s), &p)
	return p, err
}

// BuildPayload is the alert for an urgent release (docs/security-alerts.md). The links are in
// Click and Actions, so they survive any truncation of the body.
func BuildPayload(r *store.Release, sum *summarize.Summary, d Decision) Payload {
	p := Payload{
		Title:    "🚨 " + r.Repo + " " + r.Tag + ": security release",
		Priority: d.Priority,
		Tags:     []string{"rotating_light", r.Repo},
		Click:    r.HTMLURL,
	}
	firing := d.Firing()
	var ghsas []string
	for _, l := range firing {
		p.Tags = append(p.Tags, l.ID)
		for _, a := range l.Matching {
			if id := a.ID(); a.GHSA != "" && !slices.Contains(ghsas, id) {
				ghsas = append(ghsas, id)
			}
		}
	}
	for _, id := range ghsas[:min(len(ghsas), MaxActions)] {
		p.Actions = append(p.Actions, Action{Label: "Advisory", URL: AdvisoryURL + id})
	}

	// Body sections, most important first; assembly stops at the budget.
	var parts []string
	switch {
	case sum != nil:
		parts = append(parts, "**"+truncate(escapeMarkdown(sum.Headline), BodyBudget-4)+"**")
	case len(firing) > 0 && len(firing[0].Matching) > 0:
		parts = append(parts, "**"+truncate(escapeMarkdown(firing[0].Matching[0].Summary), BodyBudget-4)+"**")
	}
	for _, l := range firing {
		prefix, suffix := "Severity: **", "** ("+escapeMarkdown(l.ID)+")"
		left := BodyBudget - len(strings.Join(parts, "\n\n"))
		if len(parts) > 0 {
			left -= len("\n\n")
		}
		contentBudget := left - len(prefix) - len(suffix)
		severity := escapeMarkdown(l.Severity)
		if len(severity) > contentBudget && contentBudget < len("…") {
			p.Body = assemble(parts, BodyBudget)
			return p
		}
		parts = append(parts, prefix+truncate(severity, contentBudget)+suffix)
	}
	if sum != nil {
		if reason := sum.SecurityReason; reason != "" {
			parts = append(parts, truncate(escapeMarkdown(reason), ReasonLimit))
		}
		for _, s := range sum.Security {
			parts = append(parts, "- "+escapeMarkdown(s))
		}
	}
	for _, l := range firing {
		for _, a := range l.Matching {
			if a.Summary != "" {
				parts = append(parts, escapeMarkdown(a.ID())+": "+escapeMarkdown(a.Summary))
			}
		}
	}
	p.Body = assemble(parts, BodyBudget)
	return p
}

var httpScheme = regexp.MustCompile(`(?i)https?://`)

// escapeMarkdown keeps a field on one line and renders its markup as text.
// Defang URL schemes too: clients can linkify URLs after rendering Markdown.
func escapeMarkdown(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	s = httpScheme.ReplaceAllStringFunc(s, func(s string) string {
		if len(s) == len("https://") {
			return "hxxps://"
		}
		return "hxxp://"
	})
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune("\\`*_{}[]()<>#+-.!|~&", r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// assemble joins parts with blank lines until the budget runs out: the
// first part that doesn't fit is truncated to what's left, and the rest
// are dropped.
func assemble(parts []string, budget int) string {
	var b strings.Builder
	for _, part := range parts {
		sep := ""
		if b.Len() > 0 {
			sep = "\n\n"
			if strings.HasPrefix(part, "- ") && strings.HasPrefix(lastLine(b.String()), "- ") {
				sep = "\n" // keep a list together
			}
		}
		left := budget - b.Len() - len(sep)
		if len(part) <= left {
			b.WriteString(sep + part)
			continue
		}
		if left > len("…")+8 {
			b.WriteString(sep + truncate(part, left))
		}
		break
	}
	return b.String()
}

func lastLine(s string) string {
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// truncate cuts escaped Markdown to at most n bytes, ending in "…" if cut.
// It preserves rune boundaries and complete backslash escapes.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n < len("…") {
		return ""
	}
	cut := n - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	start := cut
	for start > 0 && s[start-1] == '\\' {
		start--
	}
	if (cut-start)%2 != 0 {
		cut--
	}
	return s[:cut] + "…"
}
