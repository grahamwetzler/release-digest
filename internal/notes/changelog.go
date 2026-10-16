package notes

import (
	"regexp"
	"slices"
	"strings"
)

// heading is one Markdown heading: its first line, the line its section's
// body starts on, and its level.
type heading struct {
	line, body, level int
	text              string
}

var (
	atx      = regexp.MustCompile(`^ {0,3}(#{1,6})(?:[ \t]+(.*?))?(?:[ \t]+#+)?[ \t]*$`)
	setext1  = regexp.MustCompile(`^ {0,3}=+[ \t]*$`)
	setext2  = regexp.MustCompile(`^ {0,3}-+[ \t]*$`)
	notPara  = regexp.MustCompile(`^ {0,3}(([-*+]|\d{1,9}[.)])([ \t]|$)|>|` + "```" + `|~~~)`) // list, quote or fence
	linkDest = regexp.MustCompile(`\]\([^)]*\)`)
)

// headings finds the ATX and setext headings of md outside fenced code.
func headings(lines []string) []heading {
	var out []heading
	fence := "" // the open fence's run of ` or ~, if any
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		t := strings.TrimLeft(l, " ")
		if run := fenceRun(t); len(l)-len(t) <= 3 && run != "" {
			switch {
			case fence == "":
				fence = run
			// Only a run of the same character, at least as long and with
			// nothing after it, closes the fence (CommonMark).
			case run[0] == fence[0] && len(run) >= len(fence) && strings.TrimSpace(t[len(run):]) == "":
				fence = ""
			}
			continue
		}
		if fence != "" {
			continue
		}
		if m := atx.FindStringSubmatch(l); m != nil {
			out = append(out, heading{line: i, body: i + 1, level: len(m[1]), text: m[2]})
			continue
		}
		if i+1 < len(lines) && strings.TrimSpace(l) != "" && !notPara.MatchString(l) && (i == 0 || strings.TrimSpace(lines[i-1]) == "") {
			switch {
			case setext1.MatchString(lines[i+1]):
				out = append(out, heading{line: i, body: i + 2, level: 1, text: strings.TrimSpace(l)})
				i++
			case setext2.MatchString(lines[i+1]):
				out = append(out, heading{line: i, body: i + 2, level: 2, text: strings.TrimSpace(l)})
				i++
			}
		}
	}
	return out
}

// fenceRun is the run of three or more backticks or tildes that starts t,
// or "".
func fenceRun(t string) string {
	if t == "" || (t[0] != '`' && t[0] != '~') {
		return ""
	}
	n := 0
	for n < len(t) && t[n] == t[0] {
		n++
	}
	if n < 3 {
		return ""
	}
	return t[:n]
}

// Section returns the changelog section for a version: the first level
// 1–3 heading that mentions any of names (the version as a changelog writes
// it, or the whole tag), through to the next heading of the same or a
// higher level. It returns "" if there is no such heading or the section
// is empty.
func Section(md string, names ...string) string {
	names = slices.DeleteFunc(slices.Clone(names), func(n string) bool { return n == "" })
	if len(names) == 0 {
		return ""
	}
	lines := strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n")
	hs := headings(lines)
	for i, h := range hs {
		text := linkDest.ReplaceAllString(h.text, "]")
		if h.level > 3 || !slices.ContainsFunc(names, func(n string) bool { return mentions(text, n) }) {
			continue
		}
		end := len(lines)
		for _, next := range hs[i+1:] {
			if next.level <= h.level {
				end = next.line
				break
			}
		}
		if strings.TrimSpace(strings.Join(lines[min(h.body, end):end], "\n")) == "" {
			return ""
		}
		return strings.TrimSpace(strings.Join(lines[h.line:end], "\n"))
	}
	return ""
}

// mentions reports whether text contains version as a whole version,
// optionally after a "v": "1.2.3" matches "v1.2.3 (2026-01-01)" and
// "[1.2.3]", but not "1.2.30", "11.2.3" or "1.2.3-rc.1".
func mentions(text, version string) bool {
	text, version = strings.ToLower(text), strings.ToLower(version)
	for from := 0; ; {
		i := strings.Index(text[from:], version)
		if i < 0 {
			return false
		}
		start, end := from+i, from+i+len(version)
		from = start + 1
		b := start
		if b > 0 && text[b-1] == 'v' {
			b--
		}
		if b > 0 && (alnum(text[b-1]) || text[b-1] == '.') {
			continue
		}
		rest := text[end:]
		if rest != "" && (alnum(rest[0]) || len(rest) > 1 && strings.ContainsRune(".-+", rune(rest[0])) && alnum(rest[1])) {
			continue
		}
		return true
	}
}

func alnum(c byte) bool { return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
