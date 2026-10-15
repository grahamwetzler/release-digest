// Package render builds the combined feed from the store and publishes it
// as RSS and Atom bytes (PLAN.md §3.6, §6).
package render

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html"
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

// Item turns a stored release into a feed item. Until summaries exist
// (milestone 4) the body is a link to the release notes.
func Item(r *store.Release) feed.Item {
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
	notes := "Release notes"
	if r.Kind == "tag" {
		notes = "Tag"
	}
	body := fmt.Sprintf(`<p><a href="%s">%s</a> for %s %s.</p>`,
		html.EscapeString(r.HTMLURL), notes, html.EscapeString(r.Repo), html.EscapeString(r.Tag))
	return feed.Item{
		Title:       title,
		Link:        r.HTMLURL,
		GUID:        r.GUID,
		Description: fmt.Sprintf("%s for %s %s.", notes, r.Repo, r.Tag),
		ContentHTML: body,
		Categories:  cats,
		Published:   r.Published,
		Updated:     r.ContentUpdated,
	}
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
		items[i] = Item(r)
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
