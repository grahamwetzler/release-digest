package notify

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"release-digest/internal/advisory"
	"release-digest/internal/store"
	"release-digest/internal/summarize"
)

func TestEscapeMarkdown(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"link", `[Update now](https://evil.example)`, `\[Update now\]\(hxxps://evil\.example\)`},
		{"image", `![Update](http://evil.example/a.png)`, `\!\[Update\]\(hxxp://evil\.example/a\.png\)`},
		{"heading", "# fake", `\# fake`},
		{"emphasis", "**urgent** _fix_ ~~safe~~", `\*\*urgent\*\* \_fix\_ \~\~safe\~\~`},
		{"code", "`run this`", "\\`run this\\`"},
		{"HTML", `<a href="https://evil.example">Update</a>`, `\<a href="hxxps://evil\.example"\>Update\</a\>`},
		{"bare URL", "https://evil.example http://evil.example", `hxxps://evil\.example hxxp://evil\.example`},
		{"mixed case URL", "HtTpS://evil.example HTTP://evil.example", `hxxps://evil\.example hxxp://evil\.example`},
		{"newline heading", "safe\n# fake", `safe \# fake`},
		{"CRLF heading", "safe\r\n# fake\rfake", `safe \# fake fake`},
		{"backslash", `a\b`, `a\\b`},
		{"non-ASCII", "安全 **修正** café 🚨", `安全 \*\*修正\*\* café 🚨`},
		{"blockquote", "> fake", `\> fake`},
		{"bullet", "- fake", `\- fake`},
		{"ordered list", "1. fake", `1\. fake`},
		{"table and braces", "| a | {b} + c!", `\| a \| \{b\} \+ c\!`},
		{"entity", "&#35; fake", `\&\#35; fake`},
		{"indentation", "\t    # fake", `\# fake`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := escapeMarkdown(tc.in); got != tc.want {
				t.Errorf("escapeMarkdown(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestPayloadEscapedBudgets(t *testing.T) {
	for _, tc := range []struct {
		name   string
		field  string
		in     string
		want   string
		budget int
	}{
		{"reason punctuation", "reason", strings.Repeat("*", 400), strings.Repeat(`\*`, 148) + "…", ReasonLimit},
		{"reason backslashes", "reason", strings.Repeat(`\`, 400), strings.Repeat(`\\`, 148) + "…", ReasonLimit},
		{"reason UTF-8", "reason", strings.Repeat("é*", 400), strings.Repeat(`é\*`, 74) + "…", ReasonLimit},
		{"body punctuation", "fix", strings.Repeat("*", 4000), strings.Repeat(`\*`, 1744) + "…", BodyBudget},
		{"body backslashes", "fix", strings.Repeat(`\`, 4000), strings.Repeat(`\\`, 1744) + "…", BodyBudget},
		{"body UTF-8", "fix", strings.Repeat("é*", 4000), strings.Repeat(`é\*`, 872) + "…", BodyBudget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sum := &summarize.Summary{Headline: "h"}
			prefix := "**h**\n\n"
			if tc.field == "reason" {
				sum.SecurityReason = tc.in
			} else {
				sum.Security = []string{tc.in}
				prefix += "- "
			}
			p := BuildPayload(&store.Release{}, sum, Decision{})
			if p.Body != prefix+tc.want {
				t.Errorf("body = %q, want %q", p.Body, prefix+tc.want)
			}
			got := strings.TrimPrefix(p.Body, prefix)
			if len(got) > tc.budget || len(p.Body) > BodyBudget || !utf8.ValidString(p.Body) {
				t.Errorf("field bytes %d, body bytes %d, valid UTF-8 %v", len(got), len(p.Body), utf8.ValidString(p.Body))
			}
			end := strings.TrimSuffix(got, "…")
			if (len(end)-len(strings.TrimRight(end, `\`)))%2 != 0 {
				t.Error("truncation left a lone backslash before the ellipsis")
			}
		})
	}
}

func TestPayloadHeadlineBudgets(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		llm  bool
	}{
		{"LLM punctuation", strings.Repeat("!", 1749), true},
		{"advisory punctuation", strings.Repeat("!", 1749), false},
		{"LLM UTF-8", strings.Repeat("é!", 1000), true},
		{"advisory UTF-8", strings.Repeat("é!", 1000), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sum *summarize.Summary
			d := Decision{}
			if tc.llm {
				sum = &summarize.Summary{Headline: tc.in}
			} else {
				d.Links = []LinkResult{{
					ID: ghsa, Fires: true, Severity: "high",
					Matching: []advisory.Advisory{{GHSA: ghsa, Summary: tc.in}},
				}}
			}
			p := BuildPayload(&store.Release{}, sum, d)
			headline := strings.SplitN(p.Body, "\n\n", 2)[0]
			if len(p.Body) > BodyBudget || !utf8.ValidString(p.Body) {
				t.Errorf("body bytes %d, valid UTF-8 %v", len(p.Body), utf8.ValidString(p.Body))
			}
			if !strings.HasPrefix(p.Body, "**") || !strings.HasSuffix(headline, "…**") {
				t.Errorf("headline = %q, want bold truncated content", headline)
			}
		})
	}
}

func TestPayloadSeverityBudgets(t *testing.T) {
	for _, tc := range []struct {
		name     string
		headline string
		omit     bool
	}{
		{"long severity", "h", false},
		{"remaining budget", strings.Repeat("h", 3400), false},
		{"wrappers cannot fit", strings.Repeat("h", 3470), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := Decision{Links: []LinkResult{{ID: ghsa, Fires: true, Severity: strings.Repeat("é!", 1000)}}}
			p := BuildPayload(&store.Release{}, &summarize.Summary{Headline: tc.headline}, d)
			if len(p.Body) > BodyBudget || !utf8.ValidString(p.Body) {
				t.Errorf("body bytes %d, valid UTF-8 %v", len(p.Body), utf8.ValidString(p.Body))
			}
			parts := strings.Split(p.Body, "\n\n")
			if tc.omit {
				if p.Body != "**"+tc.headline+"**" {
					t.Errorf("body = %q, want only headline when severity wrappers cannot fit", p.Body)
				}
				return
			}
			if len(parts) != 2 || !strings.HasPrefix(parts[1], "Severity: **") ||
				!strings.HasSuffix(parts[1], "…** ("+escapeMarkdown(ghsa)+")") {
				t.Errorf("body = %q, want closed bold severity with truncated content", p.Body)
			}
		})
	}
}

// A short severity that fits exactly is kept, even when there would be no
// room left to truncate it.
func TestPayloadSeverityFitsExactly(t *testing.T) {
	headline := strings.Repeat("h", 3453)
	d := Decision{Links: []LinkResult{{ID: ghsa, Fires: true, Severity: "hi"}}}
	p := BuildPayload(&store.Release{}, &summarize.Summary{Headline: headline}, d)
	want := "**" + headline + "**\n\nSeverity: **hi** (" + escapeMarkdown(ghsa) + ")"
	if p.Body != want {
		t.Errorf("body is %d bytes, ends %q; want %d bytes", len(p.Body), p.Body[len(p.Body)-20:], len(want))
	}
	if len(p.Body) != BodyBudget {
		t.Errorf("body is %d bytes, want exactly %d", len(p.Body), BodyBudget)
	}
}

func TestTruncateMarkdown(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"escape boundary", `ab\*cde`, 6, "ab…"},
		{"paired backslashes", `a\\bcde`, 6, "a\\\\…"},
		{"UTF-8 boundary", "éééé", 6, "é…"},
		{"zero budget", "abcd", 0, ""},
		{"tiny budget", "abcd", 2, ""},
		{"ellipsis budget", "abcd", 3, "…"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := truncate(tc.in, tc.n)
			if got != tc.want || len(got) > tc.n || !utf8.ValidString(got) {
				t.Errorf("truncate(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
			}
		})
	}
}

func TestPayloadMarkdownSent(t *testing.T) {
	f := newNtfy(t)
	r := &store.Release{Repo: "o/r", Tag: "v1.2.3", HTMLURL: "https://github.com/o/r/releases/tag/v1.2.3"}
	sum := &summarize.Summary{
		Headline:       "# [Update](https://evil.example)",
		SecurityReason: "**urgent**\n# fake",
		Security:       []string{"`run this`", "![image](http://evil.example)"},
	}
	d := Decision{Priority: 5, Links: []LinkResult{{
		ID: ghsa, Fires: true, Severity: "high**\n> fake",
		Matching: []advisory.Advisory{{GHSA: ghsa, Summary: "<a>bad</a> https://evil.example"}},
	}}}
	p := BuildPayload(r, sum, d)
	n := &Ntfy{URL: func() string { return f.URL + "/alerts" }}
	if err := n.Send(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	req, body := f.request(0)
	want := "**\\# \\[Update\\]\\(hxxps://evil\\.example\\)**\n\n" +
		"Severity: **high\\*\\* \\> fake** (GHSA\\-2345\\-6789\\-cfgh)\n\n" +
		"\\*\\*urgent\\*\\* \\# fake\n\n" +
		"- \\`run this\\`\n- \\!\\[image\\]\\(hxxp://evil\\.example\\)\n\n" +
		"GHSA\\-2345\\-6789\\-cfgh: \\<a\\>bad\\</a\\> hxxps://evil\\.example"
	if body != want {
		t.Errorf("body = %q, want %q", body, want)
	}
	if req.Header.Get("Markdown") != "yes" || req.Header.Get("Click") != r.HTMLURL ||
		req.Header.Get("Actions") != "view, Advisory, "+AdvisoryURL+ghsa {
		t.Errorf("headers = %v", req.Header)
	}

	// Without an LLM summary, the advisory supplies the bold headline too.
	p = BuildPayload(r, nil, d)
	if !strings.HasPrefix(p.Body, "**\\<a\\>bad\\</a\\> hxxps://evil\\.example**\n\n") {
		t.Errorf("advisory headline = %q", p.Body)
	}
}
