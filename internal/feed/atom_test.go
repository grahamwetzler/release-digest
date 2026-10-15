package feed

import (
	"encoding/xml"
	"strings"
	"testing"
	"time"
)

func atomSample() (Channel, []Item) {
	ch, items := sample()
	ch.SelfURL = "https://rss.example.com/feeds/claude-blog.atom"
	items[0].Updated = time.Date(2026, 9, 25, 8, 0, 0, 0, time.UTC) // edited after LastBuild
	return ch, items
}

func TestAtom(t *testing.T) {
	ch, items := atomSample()
	out, err := Atom(ch, items)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		`<feed xmlns="http://www.w3.org/2005/Atom" xmlns:media="http://search.yahoo.com/mrss/" xml:lang="en-us">`,
		`<id>https://rss.example.com/feeds/claude-blog.atom</id>`,
		`<updated>2026-09-25T08:00:00Z</updated>`, // the newest entry update beats LastBuild
		`<author>` + "\n    <name>Claude Blog</name>",
		`<link rel="self" type="application/atom+xml" href="https://rss.example.com/feeds/claude-blog.atom"></link>`,
		`<link rel="alternate" type="text/html" href="https://claude.com/blog"></link>`,
		`<id>https://claude.com/blog/b</id>`,
		`<id>urn:uuid:5f0c7d0e-1111-5222-8333-444455556666</id>`,
		`<title type="text">Second &lt;post&gt;</title>`,
		`<published>2026-09-17T14:00:00Z</published>`,
		`<updated>2026-09-01T00:00:00Z</updated>`, // never edited: updated = published
		`<category term="News"></category>`,
		`<summary type="text">Summary &amp; more</summary>`,
		`<content type="html">&lt;p&gt;Body with ]]&gt; inside and a bad char&lt;/p&gt;</content>`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s\n%s", want, s)
		}
	}
	if strings.Contains(s, "\x01") {
		t.Error("control character in output")
	}
	if probs := CheckAtom(out); len(probs) > 0 {
		t.Errorf("CheckAtom: %v", probs)
	}
	var v struct {
		Entries []struct {
			Content string `xml:"http://www.w3.org/2005/Atom content"`
		} `xml:"http://www.w3.org/2005/Atom entry"`
	}
	if err := xml.Unmarshal(out, &v); err != nil || v.Entries[0].Content != "<p>Body with ]]> inside and a bad char</p>" {
		t.Errorf("content does not round-trip: %v %q", err, v.Entries)
	}
}

func TestAtomFeedAuthor(t *testing.T) {
	ch, items := atomSample()
	ch.Author = "Anthropic"
	out, _ := Atom(ch, items)
	if !strings.Contains(string(out), "<author>\n    <name>Anthropic</name>") {
		t.Errorf("channel author not used:\n%s", out)
	}
}

func TestCheckAtomFindsProblems(t *testing.T) {
	ch, items := atomSample()
	items[0], items[1] = items[1], items[0] // oldest first
	items[1].GUID, items[1].PermaLink = items[0].GUID, items[0].PermaLink
	items[0].Link = "/relative"
	ch.SelfURL = ""
	out, _ := Atom(ch, items)
	probs := strings.Join(CheckAtom(out), "\n")
	for _, want := range []string{
		"not sorted newest first", "duplicate id", `link rel=self: "" is not an absolute`,
		`entry 1 (urn:uuid:5f0c7d0e-1111-5222-8333-444455556666) alternate link: "/relative"`, "feed: missing <id>",
	} {
		if !strings.Contains(probs, want) {
			t.Errorf("missing %q in:\n%s", want, probs)
		}
	}
	rss, _ := RSS(sample())
	if p := CheckAtom(rss); len(p) == 0 || !strings.Contains(p[0], "not an Atom document") {
		t.Errorf("RSS passed as Atom: %v", p)
	}
}

func TestUntitledItemsPassBothChecks(t *testing.T) {
	ch, items := atomSample()
	items[0].Title = ""
	items[0].Description = strings.Repeat("word ", 30)
	// RSS accepts an item with a description but no title; Atom must not be stricter.
	rss, err := RSS(ch, items)
	if err != nil {
		t.Fatal(err)
	}
	atom, err := Atom(ch, items)
	if err != nil {
		t.Fatal(err)
	}
	if p := Check(rss); len(p) > 0 {
		t.Errorf("RSS check: %v", p)
	}
	if p := CheckAtom(atom); len(p) > 0 {
		t.Errorf("Atom check: %v", p)
	}
	if want := `<title type="text">` + strings.TrimSpace(strings.Repeat("word ", 16)) + `…</title>`; !strings.Contains(string(atom), want) {
		t.Errorf("missing %s\n%s", want, atom)
	}
	for desc, want := range map[string]string{
		strings.Repeat("abcdefgh ", 10):                      strings.TrimSpace(strings.Repeat("abcdefgh ", 8)) + "…",    // 80 runes end mid-word
		strings.Repeat("é", 100):                             strings.Repeat("é", 80) + "…",                              // one long word
		"See https://example.com/" + strings.Repeat("x", 80): "See https://example.com/" + strings.Repeat("x", 56) + "…", // no stub title
		"  short \n description  ":                           "short description",
	} {
		if got := entryTitle(Item{Description: desc}); got != want {
			t.Errorf("title from %q: %q, want %q", desc, got, want)
		}
	}
	if got := entryTitle(Item{Title: " ", Link: "https://x.example/a"}); got != "https://x.example/a" {
		t.Errorf("no title or description: %q", got)
	}
}
