package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sub dir", "rd?#%.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func TestMigrateIsIdempotent(t *testing.T) {
	s, path := open(t)
	s.Close()
	s2, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	s2.Close()
}

func TestFeedReleases(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	add := func(tag, state string, published time.Time, missing time.Time) {
		t.Helper()
		ok, err := s.InsertRelease(ctx, &Release{Repo: "o/r", Tag: tag, GUID: "g-" + tag, Kind: "release",
			HTMLURL: "https://github.com/o/r/releases/tag/" + tag, Published: published, FirstSeen: now.Add(-30 * day),
			State: state, Origin: "live", MissingSince: missing})
		if err != nil || !ok {
			t.Fatalf("insert %s: %v %v", tag, ok, err)
		}
	}
	add("v1", StateSummarized, now.Add(-3*day), time.Time{})
	add("v2", StateFallback, now.Add(-1*day), time.Time{})
	add("v3", StateNoNotes, now.Add(-2*day), now.Add(-6*day))    // deleted, still in its grace period
	add("v4", StateSummarized, now.Add(-4*day), now.Add(-8*day)) // deleted past the grace period
	add("v5", StatePending, now, time.Time{})                    // not published yet
	add("v6", StateSkipped, now, time.Time{})
	add("v7", StateFallback, time.Time{}, time.Time{}) // tag date unresolved

	got, err := s.FeedReleases(ctx, 10, now)
	if err != nil {
		t.Fatal(err)
	}
	var tags []string
	for _, r := range got {
		tags = append(tags, r.Tag)
	}
	if want := "v2 v3 v1"; strings.Join(tags, " ") != want {
		t.Errorf("feed = %s, want %s", strings.Join(tags, " "), want)
	}
	if lim, _ := s.FeedReleases(ctx, 1, now); len(lim) != 1 {
		t.Errorf("limit ignored: %d", len(lim))
	}

	// A re-discovered release keeps its frozen fields.
	ok, err := s.InsertRelease(ctx, &Release{Repo: "o/r", Tag: "v1", GUID: "other", Kind: "release", HTMLURL: "x",
		Published: now, FirstSeen: now, State: StatePending, Origin: "live"})
	if err != nil || ok {
		t.Fatalf("re-insert: %v %v", ok, err)
	}
	r, _ := s.Release(ctx, "o/r", "v1")
	if r.GUID != "g-v1" || !r.Published.Equal(now.Add(-3*day)) || r.NextAttempt != r.FirstSeen || r.Urgency != "none" {
		t.Errorf("stored release changed or defaults missing: %+v", r)
	}
}

func TestRenderedAndState(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	if r, err := s.Rendered(ctx, "feed.xml"); r != nil || err != nil {
		t.Fatalf("empty store: %v %v", r, err)
	}
	at := time.Date(2026, 10, 15, 1, 2, 3, 0, time.UTC)
	if err := s.PutRendered(ctx, "feed.xml", &Rendered{Bytes: []byte("<rss/>"), ETag: `"a"`, Modified: at}); err != nil {
		t.Fatal(err)
	}
	r, err := s.Rendered(ctx, "feed.xml")
	if err != nil || string(r.Bytes) != "<rss/>" || r.ETag != `"a"` || !r.Modified.Equal(at) {
		t.Errorf("rendered = %+v, %v", r, err)
	}
	if err := s.SetStateTime(ctx, "last_tick", at); err != nil {
		t.Fatal(err)
	}
	if got, err := s.StateTime(ctx, "last_tick"); err != nil || !got.Equal(at) {
		t.Errorf("state time = %v, %v", got, err)
	}
	if got, err := s.StateTime(ctx, "unset"); err != nil || !got.IsZero() {
		t.Errorf("unset state time = %v, %v", got, err)
	}
}

func TestPutRenderedAllIsAtomic(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	at := time.Date(2026, 10, 15, 1, 0, 0, 0, time.UTC)
	old := &Rendered{Bytes: []byte("old"), ETag: `"o"`, Modified: at}
	if err := s.PutRenderedAll(ctx, map[string]*Rendered{"feed.xml": old, "feed.atom": old}); err != nil {
		t.Fatal(err)
	}
	// A nil Bytes violates NOT NULL, so the second write in the batch fails.
	err := s.PutRenderedAll(ctx, map[string]*Rendered{
		"feed.xml":  {Bytes: []byte("new"), ETag: `"n"`, Modified: at},
		"feed.atom": {Bytes: nil, ETag: `"n"`, Modified: at},
	})
	if err == nil {
		t.Fatal("expected the batch to fail")
	}
	for _, path := range []string{"feed.xml", "feed.atom"} {
		if r, _ := s.Rendered(ctx, path); string(r.Bytes) != "old" {
			t.Errorf("%s = %q after a failed batch, want old", path, r.Bytes)
		}
	}
}
