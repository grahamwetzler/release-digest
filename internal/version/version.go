// Package version classifies tags into a stream prefix and a lenient SemVer
// version, and picks each version's predecessor (docs/versions.md).
package version

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultPrerelease matches a pre-release marker in the text after a tag's
// numeric components, such as "rc1" in go1.28rc1 or "beta2" in 1.0.0beta2.
var DefaultPrerelease = regexp.MustCompile(`(?i)^[-.]?(alpha|beta|rc|pre|preview|dev|nightly|snapshot|canary)\.?\d*`)

// Version is a classified tag.
type Version struct {
	Stream     string // everything before the version, e.g. "service/api/" or "go"; "v" is not part of it; "" if unparsed
	Core       []int  // 1–4 numeric components; nil if the tag has none (date-ordered)
	Pre        string // pre-release identifiers, e.g. "rc.1" or "beta2"; "" for a stable version
	Suffix     string // any other trailing text that is not a pre-release marker
	Prerelease bool
}

// Parsed reports whether the tag had a numeric version.
func (v Version) Parsed() bool { return len(v.Core) > 0 }

// String is the normalized version: core, then -pre, then +suffix. It is ""
// for an unparsed tag.
func (v Version) String() string {
	if !v.Parsed() {
		return ""
	}
	parts := make([]string, len(v.Core))
	for i, n := range v.Core {
		parts[i] = strconv.Itoa(n)
	}
	s := strings.Join(parts, ".")
	if v.Pre != "" {
		s += "-" + v.Pre
	}
	if v.Suffix != "" {
		s += "+" + v.Suffix
	}
	return s
}

// Parse classifies tag. pre is the pre-release pattern for text after the
// numeric components that isn't a SemVer "-" segment; nil means DefaultPrerelease.
func Parse(tag string, pre *regexp.Regexp) Version {
	if pre == nil {
		pre = DefaultPrerelease
	}
	start := versionStart(tag)
	if start < 0 {
		// Date-ordered: such tags (latest, stable…) share one stream, the
		// empty one, and are ordered among themselves by date (docs/versions.md).
		return Version{}
	}
	v := Version{Stream: tag[:start]}
	if n := len(v.Stream); n > 0 && (v.Stream[n-1] == 'v' || v.Stream[n-1] == 'V') && (n == 1 || !isLetter(v.Stream[n-2])) {
		v.Stream = v.Stream[:n-1]
	}
	rest := tag[start:]
	for len(v.Core) < 4 {
		i := 0
		for i < len(rest) && isDigit(rest[i]) {
			i++
		}
		if i == 0 {
			break
		}
		n, err := strconv.Atoi(rest[:i])
		if err != nil { // too long to be a version component
			break
		}
		v.Core = append(v.Core, n)
		rest = rest[i:]
		if len(rest) < 2 || rest[0] != '.' || !isDigit(rest[1]) || len(v.Core) == 4 {
			break
		}
		rest = rest[1:]
	}
	rest, build, _ := strings.Cut(rest, "+")
	switch {
	case rest == "":
	case rest[0] == '-' && len(rest) > 1:
		v.Pre = rest[1:]
	case pre.MatchString(rest):
		v.Pre = strings.TrimLeft(rest, "-.")
	default:
		v.Suffix = rest
	}
	if build != "" {
		if v.Suffix != "" {
			v.Suffix += "+"
		}
		v.Suffix += build
	}
	v.Prerelease = v.Pre != ""
	return v
}

// versionStart is the index of the first digit that begins a run of digits,
// or -1 if tag has none.
func versionStart(tag string) int {
	for i := 0; i < len(tag); i++ {
		if isDigit(tag[i]) {
			return i
		}
	}
	return -1
}

func isDigit(c byte) bool  { return c >= '0' && c <= '9' }
func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// Compare orders two parsed versions by SemVer precedence: core components
// numerically (missing ones count as 0), then a pre-release before its
// stable version, then pre-release identifiers. Suffixes are ignored, as
// SemVer ignores build metadata.
func Compare(a, b Version) int {
	for i := 0; i < max(len(a.Core), len(b.Core)); i++ {
		x, y := at(a.Core, i), at(b.Core, i)
		if x != y {
			return cmpInt(x, y)
		}
	}
	switch {
	case a.Pre == b.Pre:
		return 0
	case a.Pre == "":
		return 1
	case b.Pre == "":
		return -1
	}
	return comparePre(a.Pre, b.Pre)
}

func at(s []int, i int) int {
	if i < len(s) {
		return s[i]
	}
	return 0
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// comparePre compares pre-release identifiers. Besides SemVer's dot-separated
// identifiers it splits letter/digit runs, so rc2 sorts before rc10.
func comparePre(a, b string) int {
	ta, tb := preTokens(a), preTokens(b)
	for i := 0; i < min(len(ta), len(tb)); i++ {
		x, y := ta[i], tb[i]
		xn, xerr := strconv.Atoi(x)
		yn, yerr := strconv.Atoi(y)
		switch {
		case xerr == nil && yerr == nil:
			if xn != yn {
				return cmpInt(xn, yn)
			}
		case xerr == nil:
			return -1 // numeric identifiers sort before alphanumeric ones
		case yerr == nil:
			return 1
		default:
			if c := strings.Compare(strings.ToLower(x), strings.ToLower(y)); c != 0 {
				return c
			}
		}
	}
	return cmpInt(len(ta), len(tb))
}

func preTokens(s string) []string {
	var out []string
	cur := ""
	flush := func() {
		if cur != "" {
			out = append(out, cur)
			cur = ""
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '.' || c == '-' || c == '_' {
			flush()
			continue
		}
		if cur != "" && isDigit(cur[len(cur)-1]) != isDigit(c) {
			flush()
		}
		cur += string(c)
	}
	flush()
	return out
}

// Candidate is a stored version considered for predecessor selection.
type Candidate struct {
	Tag        string
	Stream     string
	Version    string // normalized (Version.String); "" for a date-ordered tag
	Prerelease bool
	Published  time.Time // zero if not yet known
}

// Predecessor picks this version's predecessor from cands, which may include
// this version itself and skipped versions (docs/versions.md):
//   - only the same stream counts;
//   - a parsed version takes the highest lower version; a stable one skips pre-releases;
//   - a date-ordered tag takes the latest one in its stream published before it.
//
// It returns "" when there is no candidate.
func Predecessor(this Candidate, cands []Candidate) string {
	if this.Version != "" {
		tv := Parse(this.Version, nil)
		best, bestTag := Version{}, ""
		for _, c := range cands {
			if c.Tag == this.Tag || c.Stream != this.Stream || c.Version == "" || (!this.Prerelease && c.Prerelease) {
				continue
			}
			cv := Parse(c.Version, nil)
			if Compare(cv, tv) >= 0 {
				continue
			}
			if bestTag == "" || Compare(cv, best) > 0 || (Compare(cv, best) == 0 && c.Tag > bestTag) {
				best, bestTag = cv, c.Tag
			}
		}
		return bestTag
	}
	if this.Published.IsZero() {
		return ""
	}
	var bestTag string
	var bestAt time.Time
	for _, c := range cands {
		if c.Tag == this.Tag || c.Stream != this.Stream || c.Version != "" || c.Published.IsZero() || !c.Published.Before(this.Published) {
			continue
		}
		if bestTag == "" || c.Published.After(bestAt) || (c.Published.Equal(bestAt) && c.Tag > bestTag) {
			bestTag, bestAt = c.Tag, c.Published
		}
	}
	return bestTag
}
