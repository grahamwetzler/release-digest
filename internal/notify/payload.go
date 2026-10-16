package notify

import (
	"encoding/json"
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
	ReasonLimit = 300  // bytes of security_reason
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
		parts = append(parts, "**"+sum.Headline+"**")
	case len(firing) > 0 && len(firing[0].Matching) > 0:
		parts = append(parts, "**"+firing[0].Matching[0].Summary+"**")
	}
	for _, l := range firing {
		parts = append(parts, "Severity: **"+l.Severity+"** ("+l.ID+")")
	}
	if sum != nil {
		if reason := sum.SecurityReason; reason != "" {
			parts = append(parts, truncate(reason, ReasonLimit))
		}
		for _, s := range sum.Security {
			parts = append(parts, "- "+s)
		}
	}
	for _, l := range firing {
		for _, a := range l.Matching {
			if a.Summary != "" {
				parts = append(parts, a.ID()+": "+a.Summary)
			}
		}
	}
	p.Body = assemble(parts, BodyBudget)
	return p
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

// truncate cuts s to at most n bytes at a rune boundary, ending in "…" if cut.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
