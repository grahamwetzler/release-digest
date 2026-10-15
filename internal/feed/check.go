package feed

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"time"
)

// Check verifies an RSS document without the network (§7.3, schema checks):
// well-formed XML, required elements, RFC 822 dates, unique GUIDs, absolute
// links, and items sorted newest first. It returns one message per problem.
func Check(data []byte) []string {
	probs := wellFormed(data)
	if len(probs) > 0 {
		return probs
	}
	add := func(format string, args ...any) { probs = append(probs, fmt.Sprintf(format, args...)) }

	var doc struct {
		XMLName xml.Name `xml:"rss"`
		Version string   `xml:"version,attr"`
		Channel struct {
			Title       string `xml:"title"`
			Description string `xml:"description"`
			// A tag without a namespace matches <atom:link> too, so decode every
			// link together and tell them apart by namespace.
			Links []struct {
				XMLName xml.Name
				Href    string `xml:"href,attr"`
				Rel     string `xml:"rel,attr"`
				Text    string `xml:",chardata"`
			} `xml:"link"`
			PubDate       string `xml:"pubDate"`
			LastBuildDate string `xml:"lastBuildDate"`
			Items         []struct {
				Title       string `xml:"title"`
				Link        string `xml:"link"`
				Description string `xml:"description"`
				Content     string `xml:"http://purl.org/rss/1.0/modules/content/ encoded"`
				PubDate     string `xml:"pubDate"`
				GUID        struct {
					Value       string `xml:",chardata"`
					IsPermaLink string `xml:"isPermaLink,attr"`
				} `xml:"guid"`
			} `xml:"item"`
		} `xml:"channel"`
	}
	if err := xml.Unmarshal(data, &doc); err != nil {
		add("not an RSS document: %v", err)
		return probs
	}
	ch := &doc.Channel
	var link string
	var self []string
	for _, l := range ch.Links {
		switch {
		case l.XMLName.Space == NSAtom && l.Rel == "self":
			self = append(self, l.Href)
		case l.XMLName.Space == "" && link == "":
			link = l.Text
		}
	}
	if doc.Version != "2.0" {
		add("rss version is %q, want 2.0", doc.Version)
	}
	for name, v := range map[string]string{"title": ch.Title, "link": link, "description": ch.Description} {
		if v == "" {
			add("channel: missing <%s>", name)
		}
	}
	checkURL := func(where, u string) {
		if !absURL(u) {
			add("%s: %q is not an absolute http(s) URL", where, u)
		}
	}
	checkURL("channel link", link)
	for _, href := range self {
		checkURL("atom:link rel=self", href)
	}
	if len(self) == 0 {
		add("channel: missing <atom:link rel=\"self\">")
	}
	checkDate := func(where, v string, required bool) time.Time {
		if v == "" {
			if required {
				add("%s: missing <pubDate>", where)
			}
			return time.Time{}
		}
		t, err := time.Parse(time.RFC1123Z, v)
		if err != nil {
			add("%s: date %q is not RFC 822 with a 4-digit year and numeric zone", where, v)
		}
		return t
	}
	checkDate("channel pubDate", ch.PubDate, false)
	checkDate("channel lastBuildDate", ch.LastBuildDate, false)

	guids := map[string]int{}
	var prev time.Time
	for i, it := range ch.Items {
		where := fmt.Sprintf("item %d (%s)", i+1, it.Link)
		if it.Title == "" && it.Description == "" {
			add("%s: needs a <title> or <description>", where)
		}
		checkURL(where+" link", it.Link)
		if it.Content == "" {
			add("%s: empty <content:encoded>", where)
		}
		if it.GUID.Value == "" {
			add("%s: missing <guid>", where)
		} else if j, dup := guids[it.GUID.Value]; dup {
			add("%s: duplicate guid %q (also item %d)", where, it.GUID.Value, j)
		} else {
			guids[it.GUID.Value] = i + 1
		}
		if it.GUID.IsPermaLink != "false" {
			checkURL(where+" guid", it.GUID.Value)
		}
		t := checkDate(where, it.PubDate, true)
		if !prev.IsZero() && t.After(prev) {
			add("%s: not sorted newest first (%s after %s)", where, it.PubDate, RSSDate(prev))
		}
		if !t.IsZero() {
			prev = t
		}
	}
	return probs
}

// wellFormed checks well-formedness, independent of any feed format. The
// decoder accepts fragments, so it also requires exactly one root element and
// nothing but whitespace, comments and processing instructions outside it. A
// UTF-8 byte-order mark may precede the document; anywhere else it is text.
func wellFormed(data []byte) []string {
	dec := xml.NewDecoder(bytes.NewReader(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))))
	depth, roots := 0, 0
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return []string{fmt.Sprintf("not well-formed XML: %v", err)}
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if depth == 0 {
				roots++
			}
			depth++
		case xml.EndElement:
			depth--
		case xml.CharData:
			if depth == 0 && len(bytes.TrimSpace(t)) > 0 {
				return []string{"not well-formed XML: text outside the root element"}
			}
		}
	}
	if roots != 1 {
		return []string{fmt.Sprintf("not well-formed XML: %d root elements, want 1", roots)}
	}
	return nil
}

// absURL reports whether u is an absolute http(s) URL.
func absURL(u string) bool {
	p, err := url.Parse(u)
	return err == nil && (p.Scheme == "http" || p.Scheme == "https") && p.Host != ""
}
