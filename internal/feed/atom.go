package feed

import (
	"bytes"
	"encoding/xml"
	"strings"
	"time"
)

type atomFeed struct {
	XMLName   xml.Name     `xml:"feed"`
	NS        string       `xml:"xmlns,attr"`
	NSMedia   string       `xml:"xmlns:media,attr"`
	Lang      string       `xml:"xml:lang,attr,omitempty"`
	ID        string       `xml:"id"`
	Title     atomText     `xml:"title"`
	Subtitle  *atomText    `xml:"subtitle"`
	Updated   string       `xml:"updated"`
	Author    atomPerson   `xml:"author"`
	Links     []atomLinkEl `xml:"link"`
	Generator string       `xml:"generator,omitempty"`
	Logo      string       `xml:"logo,omitempty"`
	Entries   []atomEntry  `xml:"entry"`
}

type atomText struct {
	Type string `xml:"type,attr"`
	Text string `xml:",chardata"`
}

type atomPerson struct {
	Name string `xml:"name"`
}

type atomLinkEl struct {
	Rel  string `xml:"rel,attr"`
	Type string `xml:"type,attr,omitempty"`
	Href string `xml:"href,attr"`
}

type atomCategory struct {
	Term string `xml:"term,attr"`
}

type atomEntry struct {
	ID         string         `xml:"id"`
	Title      atomText       `xml:"title"`
	Link       atomLinkEl     `xml:"link"`
	Published  string         `xml:"published"`
	Updated    string         `xml:"updated"`
	Author     *atomPerson    `xml:"author"`
	Categories []atomCategory `xml:"category"`
	Summary    *atomText      `xml:"summary"`
	Content    atomText       `xml:"content"`
	Media      *mediaContent  `xml:"media:content"`
}

// AtomDate formats a time for Atom: RFC 3339 in UTC (§5.6).
func AtomDate(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// AtomID is an entry's Atom id: the permalink itself, or a urn:uuid: for a
// stable-hash GUID, which is a bare UUID and so not an IRI on its own.
func AtomID(it Item) string {
	if it.PermaLink {
		return it.GUID
	}
	return "urn:uuid:" + it.GUID
}

// Atom renders an Atom 1.0 document (§7.2). Items are written in the order
// given. ch.SelfURL is the Atom feed's own URL, which is also its id.
func Atom(ch Channel, items []Item) ([]byte, error) {
	f := atomFeed{
		NS:        NSAtom,
		NSMedia:   NSMedia,
		Lang:      ch.Language,
		ID:        ch.SelfURL,
		Title:     atomText{Type: "text", Text: clean(ch.Title)},
		Author:    atomPerson{Name: clean(ch.Author)},
		Generator: clean(ch.Generator),
		Logo:      ch.Image,
		Links: []atomLinkEl{
			{Rel: "self", Type: "application/atom+xml", Href: ch.SelfURL},
			{Rel: "alternate", Type: "text/html", Href: ch.Link},
		},
	}
	if f.Author.Name == "" {
		f.Author.Name = f.Title.Text // Atom requires a feed author when an entry lacks one
	}
	if ch.Description != "" {
		f.Subtitle = &atomText{Type: "text", Text: clean(ch.Description)}
	}
	updated := ch.LastBuild
	for _, it := range items {
		e := atomEntry{
			ID:        AtomID(it),
			Title:     atomText{Type: "text", Text: entryTitle(it)},
			Link:      atomLinkEl{Rel: "alternate", Type: "text/html", Href: it.Link},
			Published: AtomDate(it.Published),
			Content:   atomText{Type: "html", Text: clean(it.ContentHTML)},
		}
		up := it.Updated
		if up.Before(it.Published) {
			up = it.Published // never edited
		}
		e.Updated = AtomDate(up)
		if updated.IsZero() || up.After(updated) {
			updated = up
		}
		if it.Author != "" {
			e.Author = &atomPerson{Name: clean(it.Author)}
		}
		for _, cat := range it.Categories {
			e.Categories = append(e.Categories, atomCategory{Term: clean(cat)})
		}
		if it.Description != "" {
			e.Summary = &atomText{Type: "text", Text: clean(it.Description)}
		}
		if it.Image != "" {
			e.Media = &mediaContent{URL: it.Image, Medium: "image"}
		}
		f.Entries = append(f.Entries, e)
	}
	if updated.IsZero() {
		updated = time.Unix(0, 0) // an empty feed that has never changed
	}
	f.Updated = AtomDate(updated)

	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	enc := xml.NewEncoder(&buf)
	enc.Indent("", "  ")
	if err := enc.Encode(f); err != nil {
		return nil, err
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

// entryTitle is the item's title or, since Atom requires one where RSS
// accepts a description alone, the start of its description or its link.
// A description is cut like an excerpt: at most 80 runes plus an ellipsis.
func entryTitle(it Item) string {
	if t := strings.TrimSpace(clean(it.Title)); t != "" {
		return t
	}
	if d := strings.Join(strings.Fields(clean(it.Description)), " "); d != "" {
		return excerpt(d, 80)
	}
	return it.Link
}
