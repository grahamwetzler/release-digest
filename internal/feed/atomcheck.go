package feed

import (
	"encoding/xml"
	"fmt"
	"time"
)

// CheckAtom verifies an Atom document without the network, as Check does for
// RSS: well-formed XML in the Atom namespace, the required feed and entry
// elements, RFC 3339 dates, unique ids, absolute links, and entries sorted
// newest first.
func CheckAtom(data []byte) []string {
	probs := wellFormed(data)
	if len(probs) > 0 {
		return probs
	}
	add := func(format string, args ...any) { probs = append(probs, fmt.Sprintf(format, args...)) }

	type link struct {
		Rel  string `xml:"rel,attr"`
		Href string `xml:"href,attr"`
	}
	var doc struct {
		XMLName xml.Name `xml:"http://www.w3.org/2005/Atom feed"`
		ID      string   `xml:"id"`
		Title   string   `xml:"title"`
		Updated string   `xml:"updated"`
		Authors []struct {
			Name string `xml:"name"`
		} `xml:"author"`
		Links   []link `xml:"link"`
		Entries []struct {
			ID        string `xml:"id"`
			Title     string `xml:"title"`
			Links     []link `xml:"link"`
			Published string `xml:"published"`
			Updated   string `xml:"updated"`
			Content   string `xml:"http://www.w3.org/2005/Atom content"` // not media:content
		} `xml:"entry"`
	}
	if err := xml.Unmarshal(data, &doc); err != nil {
		add("not an Atom document: %v", err)
		return probs
	}
	checkURL := func(where, u string) {
		if !absURL(u) {
			add("%s: %q is not an absolute http(s) URL", where, u)
		}
	}
	checkDate := func(where, name, v string) time.Time {
		if v == "" {
			add("%s: missing <%s>", where, name)
			return time.Time{}
		}
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			add("%s: <%s> %q is not RFC 3339", where, name, v)
		}
		return t
	}
	alternate := func(where string, links []link) {
		for _, l := range links {
			if l.Rel == "alternate" || l.Rel == "" {
				checkURL(where+" alternate link", l.Href)
				return
			}
		}
		add("%s: missing <link rel=\"alternate\">", where)
	}

	if doc.ID == "" {
		add("feed: missing <id>")
	}
	if doc.Title == "" {
		add("feed: missing <title>")
	}
	checkDate("feed", "updated", doc.Updated)
	if len(doc.Authors) == 0 || doc.Authors[0].Name == "" {
		add("feed: missing <author><name>")
	}
	self := false
	for _, l := range doc.Links {
		if l.Rel == "self" {
			self = true
			checkURL("link rel=self", l.Href)
		}
	}
	if !self {
		add("feed: missing <link rel=\"self\">")
	}
	alternate("feed", doc.Links)

	ids := map[string]int{}
	var prev time.Time
	for i, e := range doc.Entries {
		where := fmt.Sprintf("entry %d (%s)", i+1, e.ID)
		if e.ID == "" {
			add("%s: missing <id>", where)
		} else if j, dup := ids[e.ID]; dup {
			add("%s: duplicate id (also entry %d)", where, j)
		} else {
			ids[e.ID] = i + 1
		}
		if e.Title == "" {
			add("%s: missing <title>", where)
		}
		alternate(where, e.Links)
		if e.Content == "" {
			add("%s: empty <content>", where)
		}
		checkDate(where, "updated", e.Updated)
		t := checkDate(where, "published", e.Published)
		if !prev.IsZero() && t.After(prev) {
			add("%s: not sorted newest first (%s after %s)", where, e.Published, AtomDate(prev))
		}
		if !t.IsZero() {
			prev = t
		}
	}
	return probs
}
