// Package render builds the combined feed from the store and publishes it
// as RSS and Atom bytes (PLAN.md §3.6, §6).
package render

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strings"
	"time"

	"release-digest/internal/config"
	"release-digest/internal/feed"
	"release-digest/internal/store"
)

// Format is one served rendering of the feed.
type Format struct {
	Path        string // below public_base_url, e.g. feed.xml
	ContentType string
	render      func(feed.Channel, []feed.Item) ([]byte, error)
	check       func([]byte) []string
}

var (
	RSS     = Format{"feed.xml", "application/rss+xml; charset=utf-8", feed.RSS, feed.Check}
	Atom    = Format{"feed.atom", "application/atom+xml; charset=utf-8", feed.Atom, feed.CheckAtom}
	Formats = []Format{RSS, Atom}
)

// Check runs the format's offline checks on data.
func (f Format) Check(data []byte) []string { return f.check(data) }

// Channel is the feed-level metadata for format f.
func Channel(c *config.Config, f Format, generator string) feed.Channel {
	return feed.Channel{
		Title:       c.Feed.Title,
		Link:        c.PublicBaseURL + "/",
		Description: c.Feed.Description,
		Language:    "en",
		Author:      c.Feed.Title,
		TTL:         c.Feed.TTL,
		SelfURL:     c.PublicBaseURL + "/" + f.Path,
		Generator:   generator,
	}
}

// ExcerptChars is how much of the notes a fallback item shows (§3.3).
const ExcerptChars = 600

// Item turns a stored release into a feed item. notes is the release's
// notes text, if any. Until summaries exist (milestone 4) a visible item is
// a fallback, an excerpt of the notes, or a no_notes item saying there are
// none (§3.2, §3.3, §6).
func Item(r *store.Release, notes string) feed.Item {
	version := r.Tag
	if r.Name != "" && r.Name != r.Tag {
		version = r.Name
	}
	title := r.Repo + " " + version
	cats := []string{r.Repo}
	if r.Prerelease {
		title += " (pre-release)"
		cats = append(cats, "prerelease")
	}
	link := "Release notes"
	if r.Kind == "tag" {
		link = "Tag"
	}
	excerpt := feed.Excerpt(PlainText(notes), ExcerptChars)
	desc := fmt.Sprintf("%s for %s %s.", link, r.Repo, r.Tag)
	var body strings.Builder
	switch {
	case r.State == store.StateNoNotes:
		desc = fmt.Sprintf("%s %s released — no release notes published.", r.Repo, version)
		fmt.Fprintf(&body, "<p>%s</p>\n", html.EscapeString(desc))
	case excerpt != "":
		desc = excerpt
		fmt.Fprintf(&body, "<p>%s</p>\n", html.EscapeString(excerpt))
		link = "Full release notes"
	}
	fmt.Fprintf(&body, `<p><a href="%s">%s</a>`, html.EscapeString(r.HTMLURL), link)
	if diff := DiffURL(r); diff != "" {
		fmt.Fprintf(&body, ` · <a href="%s">Diff since %s</a>`, html.EscapeString(diff), html.EscapeString(r.PrevTag))
	}
	body.WriteString("</p>")
	if r.State == store.StateFallback {
		body.WriteString("\n<p><small>Summary pending.")
		if from := sourceNames[r.NotesSource]; from != "" && excerpt != "" {
			body.WriteString(" Excerpt from " + from + ".")
		}
		body.WriteString("</small></p>")
	}
	return feed.Item{
		Title:       title,
		Link:        r.HTMLURL,
		GUID:        r.GUID,
		Description: desc,
		ContentHTML: body.String(),
		Categories:  cats,
		Published:   r.Published,
		Updated:     r.ContentUpdated,
	}
}

// sourceNames name each notes source for readers (§6).
var sourceNames = map[string]string{
	"release_body": "the release notes",
	"changelog":    "the changelog",
	"compare":      "the commit log",
}

// DiffURL is the GitHub compare page from the release's predecessor, or ""
// if it has none. It is built from the release's page URL, which is always
// <repo URL>/releases/tag/<tag>.
func DiffURL(r *store.Release) string {
	repoURL, _, ok := strings.Cut(r.HTMLURL, "/releases/tag/")
	if r.PrevTag == "" || !ok {
		return ""
	}
	return repoURL + "/compare/" + refPath(r.PrevTag) + "..." + refPath(r.Tag)
}

// refPath escapes each segment of a tag, keeping its slashes.
func refPath(tag string) string {
	parts := strings.Split(tag, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// Markdown syntax PlainText removes.
var (
	mdFence    = regexp.MustCompile("(?m)^\\s*(```|~~~).*$")
	mdImage    = regexp.MustCompile(`!\[([^\]]*)\]\([^)]*\)`)
	mdLink     = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	mdRefLink  = regexp.MustCompile(`\[([^\]]+)\]\[[^\]]*\]`)
	mdTag      = regexp.MustCompile(`</?[A-Za-z!][^>]{0,500}>`)
	mdLineHead = regexp.MustCompile(`(?m)^\s{0,3}(#{1,6}\s+|>\s?)`)
	mdListItem = regexp.MustCompile(`(?m)^\s*([-*+]|\d+[.)])\s+`)
	mdRule     = regexp.MustCompile(`(?m)^\s{0,3}([-*_=]\s*){3,}$`)
	mdEmphasis = regexp.MustCompile("(\\*\\*|\\*|`)")
)

// PlainText reduces Markdown release notes to one line of plain text for
// an excerpt: markup, HTML tags and line structure are dropped, and list
// items are marked with bullets so they stay apart.
func PlainText(md string) string {
	s := mdFence.ReplaceAllString(md, "")
	s = mdImage.ReplaceAllString(s, "$1")
	s = mdLink.ReplaceAllString(s, "$1")
	s = mdRefLink.ReplaceAllString(s, "$1")
	s = mdTag.ReplaceAllString(s, " ")
	s = mdRule.ReplaceAllString(s, "")
	s = mdLineHead.ReplaceAllString(s, "")
	s = mdListItem.ReplaceAllString(s, "• ")
	s = mdEmphasis.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	return strings.Join(strings.Fields(s), " ")
}

// Build renders every format from the store and checks it. It writes nothing.
func Build(ctx context.Context, st *store.Store, c *config.Config, generator string, now time.Time) (map[string][]byte, error) {
	rels, err := st.FeedReleases(ctx, c.Feed.MaxItems, now)
	if err != nil {
		return nil, err
	}
	items := make([]feed.Item, len(rels))
	var lastBuild time.Time
	for i, r := range rels {
		notes, err := st.Notes(ctx, r.NotesHash)
		if err != nil {
			return nil, err
		}
		items[i] = Item(r, notes)
		if u := r.ContentUpdated; u.After(lastBuild) {
			lastBuild = u
		}
	}
	out := map[string][]byte{}
	for _, f := range Formats {
		ch := Channel(c, f, generator)
		ch.LastBuild = lastBuild
		data, err := f.render(ch, items)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.Path, err)
		}
		if probs := f.check(data); len(probs) > 0 {
			return nil, fmt.Errorf("%s failed its checks: %s", f.Path, strings.Join(probs, "; "))
		}
		out[f.Path] = data
	}
	return out, nil
}

// Publish renders every format and stores it. Modified moves only when a
// feed's bytes change, so Last-Modified survives restarts and no-op ticks.
// Every changed format is stored in one transaction: on error nothing is
// stored and the last good feed keeps being served.
func Publish(ctx context.Context, st *store.Store, c *config.Config, generator string, now time.Time) (map[string]*store.Rendered, error) {
	built, err := Build(ctx, st, c, generator, now)
	if err != nil {
		return nil, err
	}
	out := map[string]*store.Rendered{}
	changed := map[string]*store.Rendered{}
	for path, data := range built {
		sum := sha256.Sum256(data)
		r := &store.Rendered{Bytes: data, ETag: `"` + hex.EncodeToString(sum[:16]) + `"`, Modified: now.UTC()}
		prev, err := st.Rendered(ctx, path)
		if err != nil {
			return nil, err
		}
		if prev != nil && prev.ETag == r.ETag {
			out[path] = prev
			continue
		}
		changed[path] = r
		out[path] = r
	}
	if err := st.PutRenderedAll(ctx, changed); err != nil {
		return nil, err
	}
	return out, nil
}
