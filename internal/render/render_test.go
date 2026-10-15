package render

import (
	"bytes"
	"context"
	"encoding/xml"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"release-digest/internal/config"
	"release-digest/internal/ident"
	"release-digest/internal/store"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func setup(t *testing.T) (*store.Store, *config.Config) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "release-digest.yaml")
	os.WriteFile(cfgPath, []byte("version: 1\npublic_base_url: https://rss.example.com\n"), 0o644)
	c, err := config.Load(cfgPath, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), filepath.Join(dir, "rd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st, c
}

var now = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

func TestEmptyFeedIsValid(t *testing.T) {
	st, c := setup(t)
	out, err := Build(context.Background(), st, c, "release-digest/test", now)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range Formats {
		if probs := f.Check(out[f.Path]); len(probs) > 0 {
			t.Errorf("%s: %v", f.Path, probs)
		}
	}
}

// uuidURN is a well-formed Atom id for a UUIDv5 GUID.
var uuidURN = regexp.MustCompile(`^urn:uuid:[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestGolden(t *testing.T) {
	ctx := context.Background()
	st, c := setup(t)
	day := 24 * time.Hour
	for i, r := range []struct{ repo, tag, name, kind string }{
		{"astral-sh/uv", "0.9.4", "0.9.4", "release"},
		{"aws/aws-sdk-go-v2", "service/s3/v1.2.3", "", "tag"},
		{"o/semver", "1.0.0+build.5", "Build <5> & more", "release"},
		{"o/unicode", "版本-1", "", "tag"},
	} {
		pub := now.Add(-time.Duration(i) * day)
		_, err := st.InsertRelease(ctx, &store.Release{
			Repo: r.repo, Tag: r.tag, GUID: ident.GUID(r.repo, r.tag), Kind: r.kind, Name: r.name,
			HTMLURL: "https://github.com/" + r.repo + "/releases/tag/" + r.tag, Prerelease: i == 2,
			Published: pub, FirstSeen: pub, ContentUpdated: pub.Add(time.Hour), State: store.StateFallback, Origin: "live",
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	out, err := Build(ctx, st, c, "release-digest/test", now)
	if err != nil {
		t.Fatal(err)
	}

	var atom struct {
		Entries []struct {
			ID string `xml:"id"`
		} `xml:"http://www.w3.org/2005/Atom entry"`
	}
	if err := xml.Unmarshal(out[Atom.Path], &atom); err != nil {
		t.Fatal(err)
	}
	if len(atom.Entries) != 4 {
		t.Fatalf("%d entries, want 4", len(atom.Entries))
	}
	for _, e := range atom.Entries {
		if !uuidURN.MatchString(e.ID) {
			t.Errorf("Atom id %q is not a urn:uuid: UUIDv5", e.ID)
		}
	}

	for _, f := range Formats {
		golden := filepath.Join("testdata", "golden."+filepath.Ext(f.Path)[1:])
		if *update {
			if err := os.WriteFile(golden, out[f.Path], 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatalf("%v (run go test ./internal/render -update)", err)
		}
		if !bytes.Equal(out[f.Path], want) {
			t.Errorf("%s differs from %s (run with -update if intended):\n%s", f.Path, golden, out[f.Path])
		}
	}
}

func TestPublishKeepsModifiedWhenUnchanged(t *testing.T) {
	ctx := context.Background()
	st, c := setup(t)
	first, err := Publish(ctx, st, c, "g", now)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Publish(ctx, st, c, "g", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !again[RSS.Path].Modified.Equal(first[RSS.Path].Modified) {
		t.Error("Modified moved without a byte change")
	}
	changed, err := Publish(ctx, st, c, "g2", now.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !changed[RSS.Path].Modified.Equal(now.Add(2*time.Hour)) || changed[RSS.Path].ETag == first[RSS.Path].ETag {
		t.Error("Modified or ETag did not move with the bytes")
	}
}
