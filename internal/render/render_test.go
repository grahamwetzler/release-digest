package render

import (
	"bytes"
	"context"
	"encoding/xml"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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
		hash, state, prev := "", store.StateFallback, ""
		if i == 1 {
			state, prev = store.StateNoNotes, "service/s3/v1.2.2"
		}
		if i == 0 {
			prev = "0.9.3"
			hash, _ = st.PutNotes(ctx, "## Highlights\n\n- **Faster** lockfile resolution for [workspaces](https://docs.astral.sh/uv/) & <b>more</b>\n- Fixed `uv sync` on Windows")
		}
		_, err := st.InsertRelease(ctx, &store.Release{
			NotesHash: hash, NotesSource: "release_body",
			Repo: r.repo, Tag: r.tag, GUID: ident.GUID(r.repo, r.tag), Kind: r.kind, Name: r.name,
			HTMLURL: "https://github.com/" + r.repo + "/releases/tag/" + r.tag, Prerelease: i == 2,
			Published: pub, FirstSeen: pub, ContentUpdated: pub.Add(time.Hour), State: state, Origin: "live", PrevTag: prev,
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

func TestPlainText(t *testing.T) {
	for in, want := range map[string]string{
		"## Features\n\n* Add **bold** and `code`\n* See [docs](https://x.example)": "Features • Add bold and code • See docs",
		"Setext\n======\n\n> quoted\n\n1. first\n2) second":                         "Setext quoted • first • second",
		"```go\nfmt.Println(1)\n```\n![logo](x.png) a < b > c &amp; d":              "fmt.Println(1) logo a < b > c & d",
		"Keep __init__ and snake_case<br/>end":                                      "Keep __init__ and snake_case end",
		"":                                                                          "",
	} {
		if got := PlainText(in); got != want {
			t.Errorf("PlainText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFallbackItem(t *testing.T) {
	r := &store.Release{Repo: "o/r", Tag: "v1", HTMLURL: "https://github.com/o/r/releases/tag/v1", State: store.StateFallback, Kind: "release", NotesSource: "changelog"}
	it := Item(r, strings.Repeat("word ", 200))
	if n := utf8.RuneCountInString(it.Description); n > ExcerptChars+1 || !strings.HasSuffix(it.Description, "…") {
		t.Errorf("description is %d runes: %q", n, it.Description)
	}
	for _, want := range []string{"<p>word word", `<a href="https://github.com/o/r/releases/tag/v1">Full release notes</a>`, "Summary pending. Excerpt from the changelog."} {
		if !strings.Contains(it.ContentHTML, want) {
			t.Errorf("missing %q in %s", want, it.ContentHTML)
		}
	}
	empty := Item(r, "")
	if empty.Description != "Release notes for o/r v1." || !strings.Contains(empty.ContentHTML, ">Release notes</a>") {
		t.Errorf("no notes: %+v", empty)
	}
}

func TestNoNotesItem(t *testing.T) {
	r := &store.Release{Repo: "o/r", Tag: "pkg/v2", PrevTag: "pkg/v1", Kind: "tag", State: store.StateNoNotes,
		HTMLURL: "https://github.com/o/r/releases/tag/pkg/v2"}
	it := Item(r, "")
	if it.Description != "o/r pkg/v2 released — no release notes published." {
		t.Errorf("description = %q", it.Description)
	}
	want := `<p><a href="https://github.com/o/r/releases/tag/pkg/v2">Tag</a> · <a href="https://github.com/o/r/compare/pkg/v1...pkg/v2">Diff since pkg/v1</a></p>`
	if !strings.Contains(it.ContentHTML, want) || strings.Contains(it.ContentHTML, "pending") {
		t.Errorf("content = %s", it.ContentHTML)
	}
}

func TestDiffURL(t *testing.T) {
	for _, tc := range []struct{ html, prev, tag, want string }{
		{"https://github.com/o/r/releases/tag/v2", "v1", "v2", "https://github.com/o/r/compare/v1...v2"},
		{"https://ghe.example/o/r/releases/tag/b%20c", "a b", "b c", "https://ghe.example/o/r/compare/a%20b...b%20c"},
		{"https://github.com/o/r/releases/tag/v1", "", "v1", ""},
		{"https://example.com/elsewhere", "v1", "v2", ""},
	} {
		if got := DiffURL(&store.Release{HTMLURL: tc.html, PrevTag: tc.prev, Tag: tc.tag}); got != tc.want {
			t.Errorf("DiffURL(%q, %q) = %q, want %q", tc.prev, tc.tag, got, tc.want)
		}
	}
}
