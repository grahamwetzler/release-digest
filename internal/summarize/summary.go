// Package summarize turns resolved release notes into structured summaries
// with an LLM (PLAN.md §3.3): the prompt, the cache key, validation, and the
// summarize pass of a tick.
package summarize

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"release-digest/internal/llm"
)

// SchemaVersion identifies the response schema. It is part of the cache
// key, so changing the schema never reuses a summary of the old shape.
// Version 2 added the urgency fields (§3.4).
const SchemaVersion = 2

// Limits the schema sets (§3.3).
const (
	MaxHeadline = 100 // runes
	MaxItems    = 6   // per list
	MaxReason   = 300 // runes of security_reason
)

// Security urgencies (§3.4), lowest first.
const (
	UrgencyNone     = "none"
	UrgencyAdvisory = "advisory"
	UrgencyUrgent   = "urgent"
)

// Summary is the structured summary of one release.
type Summary struct {
	Headline  string   `json:"headline"`
	Breaking  []string `json:"breaking"`
	Features  []string `json:"features"`
	Fixes     []string `json:"fixes"`
	Security  []string `json:"security"`
	NoiseOnly bool     `json:"noise_only"`

	SecurityUrgency string `json:"security_urgency"` // none | advisory | urgent; "" in a version 1 summary
	SecurityReason  string `json:"security_reason"`
}

// Urgency is the summary's security urgency; a version 1 summary has none.
func (s *Summary) Urgency() string {
	if s.SecurityUrgency == "" {
		return UrgencyNone
	}
	return s.SecurityUrgency
}

// Schema is the JSON Schema of a Summary, for response_format. Limits that
// strict structured output doesn't support (lengths, counts) are in the
// descriptions and enforced by Parse.
var Schema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["headline", "breaking", "features", "fixes", "security", "noise_only", "security_urgency", "security_reason"],
  "properties": {
    "headline": {"type": "string", "description": "The most important change in one plain sentence, at most 100 characters, without the version number."},
    "breaking": {"type": "array", "items": {"type": "string"}, "description": "Breaking changes, at most 6, most important first."},
    "features": {"type": "array", "items": {"type": "string"}, "description": "New features and improvements, at most 6, most important first."},
    "fixes": {"type": "array", "items": {"type": "string"}, "description": "Bug fixes, at most 6, most important first."},
    "security": {"type": "array", "items": {"type": "string"}, "description": "Security fixes, at most 6, most important first."},
    "noise_only": {"type": "boolean", "description": "True when nothing in the release is user-facing."},
    "security_urgency": {"type": "string", "enum": ["none", "advisory", "urgent"], "description": "urgent only for a fixed vulnerability that is remote code execution, an authentication or authorization bypass, known to be exploited, or comes with an explicit upgrade-immediately warning; advisory for other security fixes; otherwise none."},
    "security_reason": {"type": "string", "description": "Why, in one sentence of at most 300 characters; empty when security_urgency is none."}
  }
}`)

// Parse validates an LLM reply as a Summary. A reply may be wrapped in a
// code fence (servers without structured output often do that). Lists are
// trimmed of empty entries and cut to MaxItems; a missing or overlong
// headline, or a missing field, is an error, which earns the reply one
// repair retry.
func Parse(reply string) (*Summary, error) {
	s := strings.TrimSpace(reply)
	if rest, ok := strings.CutPrefix(s, "```"); ok {
		rest = strings.TrimPrefix(rest, "json")
		s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(rest), "```"))
	}
	// Pointers tell a missing (or null) field from an empty one: every
	// field is required, as in Schema.
	var raw struct {
		Headline  *string   `json:"headline"`
		Breaking  *[]string `json:"breaking"`
		Features  *[]string `json:"features"`
		Fixes     *[]string `json:"fixes"`
		Security  *[]string `json:"security"`
		NoiseOnly *bool     `json:"noise_only"`
		Urgency   *string   `json:"security_urgency"`
		Reason    *string   `json:"security_reason"`
	}
	dec := json.NewDecoder(strings.NewReader(s))
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("the reply is not the JSON object asked for: %v", err)
	}
	if dec.More() {
		return nil, errors.New("the reply has text after the JSON object")
	}
	var missing []string
	for name, set := range map[string]bool{"headline": raw.Headline != nil, "breaking": raw.Breaking != nil,
		"features": raw.Features != nil, "fixes": raw.Fixes != nil, "security": raw.Security != nil,
		"noise_only": raw.NoiseOnly != nil, "security_urgency": raw.Urgency != nil, "security_reason": raw.Reason != nil} {
		if !set {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return nil, fmt.Errorf("the reply lacks %s", strings.Join(missing, ", "))
	}
	sum := Summary{Headline: *raw.Headline, Breaking: *raw.Breaking, Features: *raw.Features, Fixes: *raw.Fixes,
		Security: *raw.Security, NoiseOnly: *raw.NoiseOnly, SecurityUrgency: *raw.Urgency,
		SecurityReason: strings.Join(strings.Fields(*raw.Reason), " ")}
	sum.Headline = strings.Join(strings.Fields(sum.Headline), " ")
	switch n := utf8.RuneCountInString(sum.Headline); {
	case n == 0:
		return nil, errors.New("headline is empty")
	case n > MaxHeadline:
		return nil, fmt.Errorf("headline is %d characters; the limit is %d", n, MaxHeadline)
	}
	switch sum.SecurityUrgency {
	case UrgencyNone, UrgencyAdvisory, UrgencyUrgent:
	default:
		return nil, fmt.Errorf("security_urgency is %q; it must be none, advisory or urgent", sum.SecurityUrgency)
	}
	if n := utf8.RuneCountInString(sum.SecurityReason); n > MaxReason {
		return nil, fmt.Errorf("security_reason is %d characters; the limit is %d", n, MaxReason)
	}
	for _, l := range []*[]string{&sum.Breaking, &sum.Features, &sum.Fixes, &sum.Security} {
		*l = clean(*l)
	}
	sum.normalize()
	return &sum, nil
}

// normalize settles a contradictory reply: a release with any listed
// change is not noise-only, whatever noise_only says, so the item never
// says "no user-facing changes" while marked breaking or security.
func (s *Summary) normalize() {
	if len(s.Breaking)+len(s.Features)+len(s.Fixes)+len(s.Security) > 0 {
		s.NoiseOnly = false
	}
}

// clean drops empty entries, joins each one's whitespace and cuts the list
// to MaxItems.
func clean(items []string) []string {
	var out []string
	for _, it := range items {
		if it = strings.Join(strings.Fields(it), " "); it != "" && len(out) < MaxItems {
			out = append(out, it)
		}
	}
	return out
}

// Decode reads a stored summary.
func Decode(data string) (*Summary, error) {
	var s Summary
	if err := json.Unmarshal([]byte(data), &s); err != nil {
		return nil, err
	}
	s.normalize()
	return &s, nil
}

// Encode is the stored form of a summary.
func (s *Summary) Encode() string {
	b, _ := json.Marshal(s) // a struct of strings and bools always marshals
	return string(b)
}

// Endpoint is what of the LLM configuration shapes a reply, besides the
// messages.
type Endpoint struct {
	BaseURL     string
	Model       string
	Temperature float64
}

// Key is the summary cache key (§3.3): the SHA-256 of a canonical JSON
// document of everything that shapes the reply. Two releases share a
// summary only if the LLM would see byte-identical requests.
func Key(e Endpoint, msgs []llm.Message) string {
	doc := struct {
		BaseURL       string        `json:"base_url"`
		Model         string        `json:"model"`
		Temperature   float64       `json:"temperature"`
		SchemaVersion int           `json:"schema_version"`
		Messages      []llm.Message `json:"messages"`
	}{e.BaseURL, e.Model, e.Temperature, SchemaVersion, msgs}
	b, _ := json.Marshal(doc)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
