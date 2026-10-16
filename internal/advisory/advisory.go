// Package advisory finds security advisory IDs in release notes, reads
// GitHub's global advisories, and decides whether an advisory belongs to a
// repo and is fixed by a release (PLAN.md §3.4). It does no I/O.
package advisory

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"release-digest/internal/config"
	"release-digest/internal/version"
)

// MaxPerRelease bounds the advisory IDs taken from one release's notes, so
// a release that lists hundreds of upstream CVEs can't flood the lookup
// queue. The first ones in the notes are kept.
const MaxPerRelease = 20

var (
	ghsaID = regexp.MustCompile(`(?i)\bGHSA(-[23456789cfghjmpqrvwx]{4}){3}\b`)
	cveID  = regexp.MustCompile(`(?i)\bCVE-\d{4}-\d{4,7}\b`)
)

// Extract returns the advisory IDs in notes, in order of first mention and
// normalized: GHSA-xxxx-xxxx-xxxx with a lowercase body, CVE-YYYY-N in
// uppercase. IDs inside Markdown emphasis (_CVE-…_) count. At most
// MaxPerRelease are returned.
func Extract(notes string) []string {
	notes = emphasis.Replace(notes)
	type hit struct {
		at int
		id string
	}
	var hits []hit
	for _, m := range ghsaID.FindAllStringIndex(notes, -1) {
		hits = append(hits, hit{m[0], Normalize(notes[m[0]:m[1]])})
	}
	for _, m := range cveID.FindAllStringIndex(notes, -1) {
		hits = append(hits, hit{m[0], Normalize(notes[m[0]:m[1]])})
	}
	slices.SortFunc(hits, func(a, b hit) int { return a.at - b.at })
	var out []string
	for _, h := range hits {
		if !slices.Contains(out, h.id) && len(out) < MaxPerRelease {
			out = append(out, h.id)
		}
	}
	return out
}

// Normalize is the canonical form of an advisory ID.
func Normalize(id string) string {
	if len(id) > 5 && strings.EqualFold(id[:5], "GHSA-") {
		return "GHSA-" + strings.ToLower(id[5:])
	}
	return strings.ToUpper(id)
}

// IsGHSA reports whether a normalized ID is a GHSA ID (else a CVE ID).
func IsGHSA(id string) bool { return strings.HasPrefix(id, "GHSA-") }

// Severities, lowest first. "unknown" ranks below low.
var severityRank = map[string]int{"unknown": 0, "low": 1, "medium": 2, "moderate": 2, "high": 3, "critical": 4}

// AtLeast reports whether severity meets min (critical, high or medium).
func AtLeast(severity, min string) bool {
	s, ok := severityRank[strings.ToLower(severity)]
	return ok && s >= severityRank[min]
}

// Rank orders severities: 4 for critical down to 0 for unknown or none.
func Rank(severity string) int { return severityRank[strings.ToLower(severity)] }

// Advisory is the part of a global advisory (GET /advisories/{id}) that
// detection reads.
type Advisory struct {
	GHSA               string          `json:"ghsa_id"`
	CVE                string          `json:"cve_id"`
	Summary            string          `json:"summary"`
	Severity           string          `json:"severity"`
	SourceCodeLocation string          `json:"source_code_location"`
	WithdrawnAt        *string         `json:"withdrawn_at"`
	Vulnerabilities    []Vulnerability `json:"vulnerabilities"`
}

// Vulnerability is one affected package of an advisory.
type Vulnerability struct {
	Package struct {
		Ecosystem string `json:"ecosystem"`
		Name      string `json:"name"`
	} `json:"package"`
	VulnerableRange string          `json:"vulnerable_version_range"`
	FirstPatched    json.RawMessage `json:"first_patched_version"` // a string, or {"identifier": …} in some APIs
}

// Patched is the first patched version, or "".
func (v Vulnerability) Patched() string {
	var s string
	if json.Unmarshal(v.FirstPatched, &s) == nil {
		return s
	}
	var o struct {
		Identifier string `json:"identifier"`
	}
	json.Unmarshal(v.FirstPatched, &o)
	return o.Identifier
}

// Withdrawn reports whether the advisory was withdrawn.
func (a *Advisory) Withdrawn() bool { return a.WithdrawnAt != nil && *a.WithdrawnAt != "" }

// ID is the advisory's GHSA ID.
func (a *Advisory) ID() string { return Normalize(a.GHSA) }

// Parse reads stored advisory JSON: an array of advisories (a CVE can have
// more than one).
func Parse(data string) ([]Advisory, error) {
	var out []Advisory
	if data == "" {
		return nil, nil
	}
	if err := json.Unmarshal([]byte(data), &out); err != nil {
		return nil, fmt.Errorf("stored advisory: %w", err)
	}
	return out, nil
}

// Matches reports whether a belongs to repo (§3.4): its
// source_code_location is the repo's GitHub URL, or one of its packages is
// one of the repo's configured packages. It returns the vulnerabilities that
// apply: all of them for a source_code_location match, else those of the
// matching packages.
func (a *Advisory) Matches(repo string, packages []config.Package) (bool, []Vulnerability) {
	var vulns []Vulnerability
	for _, v := range a.Vulnerabilities {
		for _, p := range packages {
			if strings.EqualFold(p.Ecosystem, v.Package.Ecosystem) && packageName(p.Ecosystem, p.Name) == packageName(v.Package.Ecosystem, v.Package.Name) {
				vulns = append(vulns, v)
				break
			}
		}
	}
	if sameRepo(a.SourceCodeLocation, repo) {
		return true, a.Vulnerabilities
	}
	return len(vulns) > 0, vulns
}

// packageName compares package names as their ecosystem does: pip
// normalizes case and separators (PEP 503); most others are case-sensitive
// but GitHub reports them as published, so only pip and npm-style lowercase
// names are folded.
func packageName(ecosystem, name string) string {
	switch strings.ToLower(ecosystem) {
	case "pip":
		return strings.ToLower(pipSeparators.ReplaceAllString(name, "-"))
	case "npm", "go", "rubygems", "rust", "composer":
		return strings.ToLower(name)
	}
	return name
}

var pipSeparators = regexp.MustCompile(`[-_.]+`)

// sameRepo reports whether loc is github.com/owner/repo, ignoring scheme,
// case, a trailing slash or .git, and a www. prefix.
func sameRepo(loc, repo string) bool {
	s := strings.ToLower(strings.TrimSpace(loc))
	for _, p := range []string{"https://", "http://", "git+https://", "git://"} {
		s = strings.TrimPrefix(s, p)
	}
	s = strings.TrimPrefix(s, "www.")
	s = strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git")
	return s == "github.com/"+strings.ToLower(repo)
}

// VersionEvidence is the outcome of the version check (§3.4).
type VersionEvidence int

const (
	Unavailable VersionEvidence = iota // can't tell: text evidence applies
	Fixes                              // this release crossed from vulnerable to patched
	NotFixes                           // the versions say this release isn't the fix
)

// CheckVersions decides from versions whether the release (tag) fixes the
// advisory, given its predecessor (prev, "" if unknown) and the
// vulnerabilities that apply. It is Fixes when, for some vulnerability, the
// release is at or above first_patched_version and prev is inside
// vulnerable_version_range. It is Unavailable when prev is unknown, either
// tag has no version, or no vulnerability has both a patched version and a
// range.
func CheckVersions(tag, prev string, vulns []Vulnerability, pre *regexp.Regexp) VersionEvidence {
	if prev == "" {
		return Unavailable
	}
	rel, before := version.Parse(tag, pre), version.Parse(prev, pre)
	if !rel.Parsed() || !before.Parsed() {
		return Unavailable
	}
	out := Unavailable
	for _, v := range vulns {
		patched := version.Parse(v.Patched(), nil)
		r, ok := parseRange(v.VulnerableRange)
		if !patched.Parsed() || !ok {
			continue
		}
		if version.Compare(rel, patched) >= 0 && r.contains(before) {
			return Fixes
		}
		out = NotFixes
	}
	return out
}

// versionRange is a conjunction of constraints such as ">= 1.0, < 1.2.3".
type versionRange []constraint

type constraint struct {
	op string
	v  version.Version
}

// parseRange reads a GitHub vulnerable_version_range.
func parseRange(s string) (versionRange, bool) {
	var r versionRange
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		op := ""
		for _, o := range []string{">=", "<=", "=", "<", ">"} {
			if strings.HasPrefix(part, o) {
				op = o
				break
			}
		}
		if op == "" {
			return nil, false
		}
		v := version.Parse(strings.TrimSpace(part[len(op):]), nil)
		if !v.Parsed() {
			return nil, false
		}
		r = append(r, constraint{op, v})
	}
	return r, len(r) > 0
}

func (r versionRange) contains(v version.Version) bool {
	for _, c := range r {
		d := version.Compare(v, c.v)
		ok := false
		switch c.op {
		case ">=":
			ok = d >= 0
		case "<=":
			ok = d <= 0
		case "=":
			ok = d == 0
		case "<":
			ok = d < 0
		case ">":
			ok = d > 0
		}
		if !ok {
			return false
		}
	}
	return true
}
