package poll

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"release-digest/internal/config"
	"release-digest/internal/github"
	"release-digest/internal/github/githubtest"
	"release-digest/internal/store"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

type harness struct {
	t   *testing.T
	gh  *githubtest.Server
	st  *store.Store
	p   *Poller
	now time.Time
	log *bytes.Buffer
	ctx context.Context
}

// newHarness loads a config (repos and options in extra) pointed at a fake GitHub.
func newHarness(t *testing.T, extra string) *harness {
	t.Helper()
	gh := githubtest.New()
	t.Cleanup(gh.Close)
	dir := t.TempDir()
	path := filepath.Join(dir, "release-digest.yaml")
	body := fmt.Sprintf("version: 1\npublic_base_url: https://rss.example.com\ngithub: {api_url: %q}\n%s", gh.URL, extra)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(dir, "rd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h := &harness{t: t, gh: gh, st: st, now: t0, log: &bytes.Buffer{}, ctx: ctx}
	// The sweep reads the clock from several goroutines, and request hooks
	// (which run under gh.Mu) advance it; between ticks the test sets it directly.
	clock := func() time.Time {
		gh.Mu.Lock()
		defer gh.Mu.Unlock()
		return h.now
	}
	h.p = &Poller{
		GH:    &github.Client{BaseURL: gh.URL, HTTP: http.DefaultClient, Cache: st, Timeout: 5 * time.Second, UserAgent: "test", Now: clock},
		Store: st, Cfg: cfg, Now: clock,
		Log: slog.New(slog.NewTextHandler(h.log, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	return h
}

// tick runs the GitHub stages and the promotion, like the scheduler does.
func (h *harness) tick() int64 {
	h.t.Helper()
	id, err := h.p.NextTick(h.ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.p.Sweep(h.ctx, id); err != nil {
		h.t.Fatal(err)
	}
	if err := h.p.Poll(h.ctx, id); err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.p.Promote(h.ctx, id); err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *harness) row(repo, tag string) *store.Release {
	h.t.Helper()
	r, err := h.st.Release(h.ctx, repo, tag)
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

func (h *harness) count(repo, state string) int {
	h.t.Helper()
	n := 0
	for i := 0; i < 1000; i++ {
		if r := h.row(repo, fmt.Sprintf("v1.0.%d", i)); r != nil && (state == "" || r.State == state) {
			n++
		}
	}
	return n
}

// releases makes n releases v1.0.<from+n-1> … v1.0.<from>, newest first, a day apart, the newest at newest.
func releases(from, n int, newest time.Time) []githubtest.Release {
	out := make([]githubtest.Release, n)
	for i := range out {
		v := from + n - 1 - i
		out[i] = githubtest.Release{Tag: fmt.Sprintf("v1.0.%d", v), Body: fmt.Sprintf("Notes for %d", v), Published: newest.Add(-time.Duration(i) * time.Hour)}
	}
	return out
}

func prepend(r *githubtest.Repo, rels []githubtest.Release) {
	r.Releases = append(rels, r.Releases...)
}

func TestFirstSightBackfillsOnlyPageOne(t *testing.T) {
	h := newHarness(t, "repos: [{repo: Owner/Repo}]")
	h.gh.Repo("owner/repo").Releases = releases(0, 40, t0.Add(-time.Hour))
	h.tick()

	if n := h.gh.Count("/repos/owner/repo/releases"); n != 1 {
		t.Errorf("release list requests = %d, want page 1 only", n)
	}
	if got := h.count("owner/repo", ""); got != 30 {
		t.Errorf("stored %d rows, want page 1's 30", got)
	}
	if got := h.count("owner/repo", store.StatePending); got != 3 {
		t.Errorf("pending = %d, want backfill 3", got)
	}
	newest := h.row("owner/repo", "v1.0.39")
	if newest.State != store.StatePending || newest.Origin != store.OriginBackfill || newest.PrevTag != "v1.0.38" || newest.NotesSource != "release_body" {
		t.Errorf("newest = %+v", newest)
	}
	if r := h.row("owner/repo", "v1.0.36"); r.State != store.StateSkipped {
		t.Errorf("4th newest = %s, want skipped", r.State)
	}
	rs, _ := h.st.RepoState(h.ctx, "owner/repo")
	if !rs.InitializedAt.Equal(t0) || rs.ScanBoundary != "v1.0.39" {
		t.Errorf("repo state = %+v", rs)
	}
}

func TestLiveAndCatchup(t *testing.T) {
	h := newHarness(t, "repos: [{repo: o/r}]")
	repo := h.gh.Repo("o/r")
	repo.Releases = releases(0, 5, t0.Add(-time.Hour))
	h.tick()

	// 45 releases land between ticks: page 1 is live, pages 2+ catch-up.
	h.now = t0.Add(time.Hour)
	prepend(repo, releases(5, 45, h.now.Add(-time.Minute)))
	h.gh.ResetLog()
	h.tick()
	if n := h.gh.Count("/repos/o/r/releases"); n != 2 {
		t.Errorf("list requests = %d, want 2 (stop at the boundary)", n)
	}
	if r := h.row("o/r", "v1.0.49"); r.Origin != store.OriginLive || r.State != store.StatePending {
		t.Errorf("newest = %+v", r)
	}
	if r := h.row("o/r", "v1.0.5"); r == nil || r.Origin != store.OriginCatchup || r.State != store.StatePending || r.PrevTag != "v1.0.4" {
		t.Errorf("oldest new = %+v", r)
	}
	rs, _ := h.st.RepoState(h.ctx, "o/r")
	if rs.ScanBoundary != "v1.0.49" {
		t.Errorf("boundary = %q", rs.ScanBoundary)
	}
}

func TestCatchupCapOmitsOlderVersions(t *testing.T) {
	h := newHarness(t, "defaults: {max_pages: 2}\nrepos: [{repo: o/r}]")
	repo := h.gh.Repo("o/r")
	repo.Releases = releases(0, 1, t0.Add(-time.Hour))
	h.tick()
	h.now = t0.Add(time.Hour)
	prepend(repo, releases(1, 70, h.now.Add(-time.Minute)))
	h.tick()
	if r := h.row("o/r", "v1.0.10"); r != nil {
		t.Errorf("a version past the cap was recorded: %+v", r)
	}
	if r := h.row("o/r", "v1.0.11"); r == nil {
		t.Error("the oldest version within the cap is missing")
	}
	if !strings.Contains(h.log.String(), "reached max_pages") {
		t.Errorf("no cap warning:\n%s", h.log)
	}
}

func TestInterruptedCatchupWritesNothing(t *testing.T) {
	h := newHarness(t, "repos: [{repo: o/r}]")
	repo := h.gh.Repo("o/r")
	repo.Releases = releases(0, 10, t0.Add(-time.Hour))
	h.tick()

	h.now = t0.Add(time.Hour)
	prepend(repo, releases(10, 80, h.now.Add(-time.Minute))) // v1.0.10 … v1.0.89
	fail := true
	h.gh.Before = func(r *http.Request) {
		if fail && r.URL.Path == "/repos/o/r/releases" && r.URL.Query().Get("page") == "3" {
			repo.Status = http.StatusBadGateway
		}
	}
	h.tick()
	repo.Status = 0
	if r := h.row("o/r", "v1.0.89"); r != nil {
		t.Fatalf("an interrupted scan wrote rows: %+v", r)
	}
	if rs, _ := h.st.RepoState(h.ctx, "o/r"); rs.ScanBoundary != "v1.0.9" || rs.LastError == "" {
		t.Fatalf("boundary moved or no error recorded: %+v", rs)
	}

	// Between ticks: 5 more on top, and 35 of the earlier new entries deleted.
	fail = false
	h.now = h.now.Add(time.Hour)
	repo.Releases = append(releases(90, 5, h.now.Add(-time.Minute)), repo.Releases[35:]...)
	h.tick()
	for _, tag := range []string{"v1.0.94", "v1.0.54", "v1.0.10"} {
		if r := h.row("o/r", tag); r == nil || r.State != store.StatePending {
			t.Errorf("%s = %+v", tag, r)
		}
	}
	if r := h.row("o/r", "v1.0.60"); r != nil {
		t.Errorf("a deleted entry was recorded: %+v", r)
	}
	if rs, _ := h.st.RepoState(h.ctx, "o/r"); rs.ScanBoundary != "v1.0.94" {
		t.Errorf("boundary = %q", rs.ScanBoundary)
	}
}

func TestFilters(t *testing.T) {
	h := newHarness(t, `defaults: {backfill: 10}
repos:
  - {repo: o/r, exclude_pattern: '^nightly'}
  - {repo: o/pre, include_prereleases: true}
`)
	h.gh.Repo("o/r").Releases = []githubtest.Release{
		{Tag: "v3.0.0", Draft: true, Published: t0},
		{Tag: "v2.0.0-rc.1", Published: t0.Add(-1 * time.Hour)},
		{Tag: "v1.1.0", Prerelease: true, Published: t0.Add(-2 * time.Hour)},
		{Tag: "nightly-1.0", Published: t0.Add(-3 * time.Hour)},
		{Tag: "latest", Published: t0.Add(-4 * time.Hour)},
		{Tag: "v1.0.0", Published: t0.Add(-5 * time.Hour)},
	}
	h.gh.Repo("o/pre").Releases = []githubtest.Release{{Tag: "v2.0.0-rc.1", Published: t0.Add(-time.Hour)}}
	h.tick()
	if r := h.row("o/r", "v3.0.0"); r != nil {
		t.Errorf("a draft was recorded: %+v", r)
	}
	for tag, want := range map[string]string{"v2.0.0-rc.1": store.StateSkipped, "v1.1.0": store.StateSkipped,
		"nightly-1.0": store.StateSkipped, "latest": store.StateSkipped, "v1.0.0": store.StatePending} {
		if r := h.row("o/r", tag); r == nil || r.State != want {
			t.Errorf("%s = %+v, want %s", tag, r, want)
		}
	}
	if r := h.row("o/r", "v1.1.0"); !r.Prerelease {
		t.Error("the prerelease flag was not recorded")
	}
	if r := h.row("o/pre", "v2.0.0-rc.1"); r.State != store.StatePending || !r.Prerelease {
		t.Errorf("include_prereleases: %+v", r)
	}
}

func TestTagsWaitForTheirDate(t *testing.T) {
	h := newHarness(t, "llm: {publish_after: 1h}\nrepos: [{repo: o/tags}]")
	h.gh.Repo("o/tags").Tags = []githubtest.Tag{
		{Name: "v1.1.0", SHA: "bbb", Date: t0.Add(-time.Hour)},
		{Name: "v1.0.0", SHA: "aaa", Date: t0.Add(-48 * time.Hour)},
	}
	h.gh.Before = func(r *http.Request) {
		if strings.Contains(r.URL.Path, "/commits/") {
			h.gh.Repos["o/tags"].Status = http.StatusBadGateway // the metadata queue fails this tick
		}
	}
	h.tick()
	h.gh.Repo("o/tags").Status = 0
	h.gh.Before = nil
	r := h.row("o/tags", "v1.1.0")
	if r == nil || r.Kind != "tag" || !r.Published.IsZero() || r.HTMLURL != "https://127.0.0.1/o/tags/releases/tag/v1.1.0" && !strings.HasSuffix(r.HTMLURL, "/o/tags/releases/tag/v1.1.0") {
		t.Fatalf("tag row = %+v", r)
	}
	// Due for promotion by age, but undated: not published.
	h.now = t0.Add(2 * time.Hour)
	h.gh.Repo("o/tags").Status = http.StatusBadGateway
	h.tick() // sweep fails: nothing resolves or promotes
	h.gh.Repo("o/tags").Status = 0
	if r := h.row("o/tags", "v1.1.0"); r.State != store.StatePending || !r.Published.IsZero() {
		t.Fatalf("after a failed tick: %+v", r)
	}
	h.tick()
	r = h.row("o/tags", "v1.1.0")
	if !r.Published.Equal(t0.Add(-time.Hour)) || r.State != store.StateFallback || r.PrevTag != "v1.0.0" {
		t.Errorf("after the date resolved: %+v", r)
	}
}

func TestTagsFirstSightBackfillsTheHighestVersions(t *testing.T) {
	h := newHarness(t, `defaults: {backfill: 2}
repos:
  - {repo: golang/go, sources: [tags], tag_pattern: '^go1\.\d+(\.\d+)?$'}
  - {repo: o/semver}
`)
	// Both lists come back sorted by name, not date, as GitHub sorts them.
	var gotags []githubtest.Tag
	for i := 0; i < 40; i++ {
		gotags = append(gotags, githubtest.Tag{Name: fmt.Sprintf("weekly.2011-%02d", i), SHA: fmt.Sprintf("w%d", i)})
	}
	gotags = append(gotags, githubtest.Tag{Name: "go1.9.7", SHA: "a"}, githubtest.Tag{Name: "go1.25.1", SHA: "b"},
		githubtest.Tag{Name: "go1.25.0", SHA: "c"}, githubtest.Tag{Name: "go1.26rc1", SHA: "d"})
	h.gh.Repo("golang/go").Tags = gotags
	h.gh.Repo("o/semver").Tags = []githubtest.Tag{{Name: "v1.9.0", SHA: "x"}, {Name: "v1.10.0", SHA: "y"}, {Name: "v1.8.0", SHA: "z"}}
	for _, r := range h.gh.Repos {
		for i := range r.Tags {
			r.Tags[i].Date = t0.Add(-time.Duration(i+1) * time.Hour)
		}
	}
	h.tick()
	for repo, want := range map[string]map[string]string{
		"golang/go": {"go1.25.1": store.StatePending, "go1.25.0": store.StatePending, "go1.9.7": store.StateSkipped,
			"go1.26rc1": store.StateSkipped, "weekly.2011-05": store.StateSkipped},
		"o/semver": {"v1.10.0": store.StatePending, "v1.9.0": store.StatePending, "v1.8.0": store.StateSkipped},
	} {
		for tag, state := range want {
			if r := h.row(repo, tag); r == nil || r.State != state || r.Origin != store.OriginBackfill {
				t.Errorf("%s %s = %+v, want %s backfill", repo, tag, r, state)
			}
		}
	}
	if r := h.row("golang/go", "go1.25.1"); r.PrevTag != "go1.25.0" {
		t.Errorf("prev_tag = %q", r.PrevTag)
	}

	// Later, a new tag anywhere in the list is live; a removed visible tag is checked at once.
	h.now = t0.Add(7 * time.Hour)
	h.tick() // promotes the backfill (publish_after 6h) once dated
	if r := h.row("o/semver", "v1.10.0"); r.State != store.StateFallback {
		t.Fatalf("not promoted: %+v", r)
	}
	h.gh.Repo("o/semver").Tags = []githubtest.Tag{{Name: "v1.11.0", SHA: "w", Date: h.now}, {Name: "v1.9.0", SHA: "x"}, {Name: "v1.8.0", SHA: "z"}}
	h.now = h.now.Add(time.Hour)
	h.tick()
	if r := h.row("o/semver", "v1.11.0"); r == nil || r.Origin != store.OriginLive || r.State != store.StatePending || r.PrevTag != "v1.10.0" {
		t.Errorf("new tag = %+v", r)
	}
	if r := h.row("o/semver", "v1.10.0"); !r.MissingSince.Equal(h.now) {
		t.Errorf("removed tag not marked missing: %+v", r)
	}
	if n := h.gh.Count("/repos/golang/go/commits/w"); n != 0 {
		t.Errorf("dated %d skipped weekly tags", n)
	}
}

func TestSwitchingListsBackfillsInsteadOfFlooding(t *testing.T) {
	h := newHarness(t, "defaults: {backfill: 2}\nrepos: [{repo: o/r}]")
	repo := h.gh.Repo("o/r")
	repo.Releases = []githubtest.Release{{Tag: "v2.0.0", Published: t0.Add(-time.Hour)}}
	for i := 0; i < 20; i++ {
		repo.Tags = append(repo.Tags, githubtest.Tag{Name: fmt.Sprintf("v1.%d.0", i), SHA: fmt.Sprintf("s%d", i)})
	}
	h.tick()
	// Every release is deleted: the repo falls back to its tags, which are old.
	repo.Releases = nil
	h.now = t0.Add(time.Hour)
	h.tick()
	pending := 0
	for i := 0; i < 20; i++ {
		r := h.row("o/r", fmt.Sprintf("v1.%d.0", i))
		if r == nil || r.Origin != store.OriginBackfill {
			t.Fatalf("v1.%d.0 = %+v, want a backfill row", i, r)
		}
		if r.State == store.StatePending {
			pending++
		}
	}
	if pending != 2 {
		t.Errorf("%d old tags pending after the switch, want backfill 2", pending)
	}
}

func TestGateHoldsPrivateAndFailingRepos(t *testing.T) {
	h := newHarness(t, "llm: {publish_after: 1h}\nrepos: [{repo: o/r}]")
	repo := h.gh.Repo("o/r")
	repo.Releases = releases(0, 2, t0.Add(-time.Hour))
	h.tick()
	h.now = t0.Add(2 * time.Hour)
	h.tick() // promotes the backfill
	if r := h.row("o/r", "v1.0.1"); r.State != store.StateFallback || !r.ContentUpdated.Equal(h.now) {
		t.Fatalf("not promoted: %+v", r)
	}

	next := 2
	for _, tc := range []struct {
		name string
		set  func()
		held bool
	}{
		{"private", func() { repo.Private = true }, true},
		{"404", func() { repo.Status = http.StatusNotFound }, true},
		{"5xx", func() { repo.Status = http.StatusInternalServerError }, false},
	} {
		h.now = h.now.Add(time.Hour)
		tag := fmt.Sprintf("v1.0.%d", next)
		prepend(repo, releases(next, 1, h.now.Add(-time.Minute)))
		next++
		tc.set()
		h.gh.ResetLog()
		h.tick()
		for _, req := range h.gh.Requests {
			if !strings.HasPrefix(req, "/repos/o/r?") {
				t.Errorf("%s: content requested: %s", tc.name, req)
			}
		}
		rs, _ := h.st.RepoState(h.ctx, "o/r")
		if rs.Held != tc.held || rs.ConfirmedTick != 0 || rs.LastError == "" {
			t.Errorf("%s: repo state = %+v", tc.name, rs)
		}
		if r := h.row("o/r", tag); r != nil {
			t.Errorf("%s: a release was discovered while gated: %+v", tc.name, r)
		}
		if r := h.row("o/r", "v1.0.1"); r.State != store.StateFallback {
			t.Errorf("%s: a published item changed: %+v", tc.name, r)
		}

		// Recovery: un-held, and the release from the gated period is discovered.
		repo.Private, repo.Status = false, 0
		h.now = h.now.Add(time.Hour)
		h.tick()
		if rs, _ := h.st.RepoState(h.ctx, "o/r"); rs.Held {
			t.Errorf("%s: still held after a public check", tc.name)
		}
		if r := h.row("o/r", tag); r == nil || r.State != store.StatePending {
			t.Errorf("%s: after recovery %s = %+v", tc.name, tag, r)
		}
	}
	h.now = h.now.Add(2 * time.Hour)
	h.tick()
	if got := h.count("o/r", store.StateFallback); got != next {
		t.Errorf("published %d of %d after recovery", got, next)
	}
}

func TestPromotionNeedsAJustInTimeCheck(t *testing.T) {
	h := newHarness(t, "llm: {publish_after: 1h}\nrepos: [{repo: o/r}]")
	repo := h.gh.Repo("o/r")
	repo.Releases = releases(0, 1, t0.Add(-time.Hour))
	h.tick()
	h.now = t0.Add(2 * time.Hour)
	// The repo turns private after the sweep and poll, just before promotion.
	id, _ := h.p.NextTick(h.ctx)
	h.p.Sweep(h.ctx, id)
	h.p.Poll(h.ctx, id)
	repo.Private = true
	if n, err := h.p.Promote(h.ctx, id); n != 0 || err != nil {
		t.Fatalf("promoted %d, %v", n, err)
	}
	if r := h.row("o/r", "v1.0.0"); r.State != store.StatePending {
		t.Errorf("state = %s", r.State)
	}
	if ok, _ := h.st.Confirmed(h.ctx, "o/r", id); ok {
		t.Error("the failed check did not invalidate the tick's confirmation")
	}
}

func TestDeletionDetection(t *testing.T) {
	h := newHarness(t, "llm: {publish_after: 1h}\ndefaults: {backfill: 30}\nrepos: [{repo: o/r}]")
	repo := h.gh.Repo("o/r")
	repo.Releases = releases(0, 5, t0.Add(-time.Hour))
	h.tick()
	h.now = t0.Add(2 * time.Hour)
	h.tick() // all 5 visible

	// v1.0.3 vanishes from page 1 while newer than its oldest entry: checked at once.
	repo.Releases = append(repo.Releases[:1], repo.Releases[2:]...)
	h.now = h.now.Add(time.Hour)
	h.tick()
	if r := h.row("o/r", "v1.0.3"); !r.MissingSince.Equal(h.now) {
		t.Errorf("window candidate not marked: %+v", r)
	}

	// It reappears in a list: cleared.
	repo.Releases = releases(0, 5, t0.Add(-time.Hour))
	h.now = h.now.Add(time.Hour)
	h.tick()
	if r := h.row("o/r", "v1.0.3"); !r.MissingSince.IsZero() {
		t.Errorf("a list appearance did not clear missing_since: %+v", r)
	}

	// An empty page 1 makes every stored release a candidate.
	repo.Releases = nil
	h.now = h.now.Add(time.Hour)
	h.tick()
	for i := 0; i < 5; i++ {
		if r := h.row("o/r", fmt.Sprintf("v1.0.%d", i)); r.MissingSince.IsZero() {
			t.Errorf("v1.0.%d not marked after an empty page 1", i)
		}
	}
}

func TestVerifySweepRotates(t *testing.T) {
	h := newHarness(t, "llm: {publish_after: 1h}\ndefaults: {backfill: 30}\nrepos: [{repo: o/r}]")
	h.p.Cfg.GitHub.VerifyPerTick = 3
	repo := h.gh.Repo("o/r")
	repo.Releases = releases(0, 30, t0.Add(-time.Hour))
	h.tick()
	// 30 newer releases push the first 30 off page 1, out of the window checks.
	h.now = t0.Add(48 * time.Hour)
	prepend(repo, releases(30, 30, h.now.Add(-time.Minute)))
	h.tick()
	h.now = h.now.Add(2 * time.Hour)
	h.tick()

	// The six oldest always fail their check; v1.0.10, long off page 1, is deleted.
	failing := map[string]bool{}
	for i := 0; i < 6; i++ {
		failing[fmt.Sprintf("v1.0.%d", i)] = true
	}
	h.gh.Before = func(r *http.Request) {
		h.gh.Repos["o/r"].Status = 0
		if failing[strings.TrimPrefix(r.URL.Path, "/repos/o/r/releases/tags/")] {
			h.gh.Repos["o/r"].Status = http.StatusBadGateway
		}
	}
	for i, rel := range repo.Releases {
		if rel.Tag == "v1.0.10" {
			repo.Releases = append(repo.Releases[:i], repo.Releases[i+1:]...)
			break
		}
	}
	h.tick()
	if r := h.row("o/r", "v1.0.10"); !r.MissingSince.IsZero() {
		t.Fatal("v1.0.10 was checked as a window candidate; it should only be reachable by the sweep")
	}
	marked := false
	for i := 0; i < 25 && !marked; i++ {
		h.now = h.now.Add(time.Hour)
		h.tick()
		marked = !h.row("o/r", "v1.0.10").MissingSince.IsZero()
	}
	if !marked {
		t.Error("the rotating sweep never reached v1.0.10 past the always-failing items")
	}
	if r := h.row("o/r", "v1.0.3"); !r.MissingSince.IsZero() || r.VerifyAttempt.IsZero() || !r.LastVerified.IsZero() {
		t.Errorf("a failing check changed more than the attempt time: %+v", r)
	}
}

func TestRateLimitStopsAndResumes(t *testing.T) {
	h := newHarness(t, "repos: [{repo: o/a}, {repo: o/b}, {repo: o/c}]")
	for _, n := range []string{"o/a", "o/b", "o/c"} {
		h.gh.Repo(n).Releases = releases(0, 1, t0.Add(-time.Hour))
	}
	h.gh.Before = func(r *http.Request) {
		if r.URL.Path == "/repos/o/b/releases" {
			h.gh.RateLimited, h.gh.Reset = true, h.now.Add(30*time.Minute)
		}
	}
	h.tick()
	if h.row("o/a", "v1.0.0") == nil || h.row("o/b", "v1.0.0") != nil || h.row("o/c", "v1.0.0") != nil {
		t.Fatal("the stage did not stop at the rate limit")
	}
	if c, _ := h.st.State(h.ctx, statePollCursor); c != "o/b" {
		t.Errorf("cursor = %q, want o/b", c)
	}
	h.gh.Before = nil
	h.gh.RateLimited = false
	h.now = h.now.Add(time.Hour)
	h.gh.ResetLog()
	h.tick()
	if h.row("o/b", "v1.0.0") == nil || h.row("o/c", "v1.0.0") == nil {
		t.Error("the next tick did not resume")
	}
	var first string
	for _, r := range h.gh.Requests {
		if strings.HasSuffix(strings.SplitN(r, "?", 2)[0], "/releases") {
			first = r
			break
		}
	}
	if !strings.HasPrefix(first, "/repos/o/b/") {
		t.Errorf("first content request = %s, want o/b", first)
	}
}

func TestBudgetFinishesTheRepoInProgress(t *testing.T) {
	h := newHarness(t, "schedule: {poll_budget: 1m}\nrepos: [{repo: o/a}, {repo: o/b}]")
	a := h.gh.Repo("o/a")
	a.Releases = releases(0, 1, t0.Add(-time.Hour))
	h.gh.Repo("o/b").Releases = releases(0, 1, t0.Add(-time.Hour))
	h.tick()
	prepend(a, releases(1, 80, t0))
	h.now = t0.Add(time.Hour)
	// Each request takes 30s of the 1m budget; o/a's catch-up needs 4.
	h.gh.Before = func(*http.Request) { h.now = h.now.Add(30 * time.Second) }
	h.gh.Repo("o/b").Releases = append(releases(1, 1, t0), h.gh.Repo("o/b").Releases...)
	h.tick()
	if r := h.row("o/a", "v1.0.1"); r == nil {
		t.Error("the catch-up in progress did not finish")
	}
	if r := h.row("o/b", "v1.0.1"); r != nil {
		t.Error("the next repo started after the budget ran out")
	}
}

func TestReconciliation(t *testing.T) {
	h := newHarness(t, "defaults: {max_pages: 1}\nschedule: {poll_budget: 10m}\nrepos: [{repo: o/r}]")
	repo := h.gh.Repo("o/r")
	// 40 releases in the last week before tracking starts.
	repo.Releases = releases(0, 40, t0.Add(-time.Hour))
	h.tick()

	// A release published after initialization that the scans missed (it sits below page 1).
	missed := githubtest.Release{Tag: "v1.0.500", Published: t0.Add(time.Minute)}
	repo.Releases = append(repo.Releases[:35], append([]githubtest.Release{missed}, repo.Releases[35:]...)...)

	// The day's reconciliation is interrupted by the budget after page 1.
	h.now = t0.Add(25 * time.Hour)
	h.gh.Before = func(r *http.Request) {
		if r.URL.Query().Get("page") == "1" && r.URL.Path == "/repos/o/r/releases" {
			h.now = h.now.Add(6 * time.Minute)
		}
	}
	h.tick()
	rs, _ := h.st.RepoState(h.ctx, "o/r")
	if rs.ReconcileNextPage != 2 {
		t.Fatalf("reconcile_next_page = %d, want 2", rs.ReconcileNextPage)
	}
	h.gh.Before = nil
	h.now = h.now.Add(time.Hour)
	h.gh.ResetLog()
	h.tick()
	if n := h.gh.Count("/repos/o/r/releases?per_page=30&page=1"); n != 1 {
		t.Errorf("page 1 requests = %d (the scan only; reconciliation resumes at page 2)", n)
	}
	if r := h.row("o/r", "v1.0.500"); r == nil || r.State != store.StatePending || r.Origin != store.OriginReconcile {
		t.Errorf("post-initialization release = %+v, want pending", r)
	}
	if r := h.row("o/r", "v1.0.1"); r == nil || r.State != store.StateSkipped || r.Origin != store.OriginReconcile {
		t.Errorf("pre-initialization release = %+v, want skipped", r)
	}
	rs, _ = h.st.RepoState(h.ctx, "o/r")
	if rs.ReconcileNextPage != 0 || !rs.LastReconciledAt.Equal(h.now) {
		t.Errorf("repo state = %+v", rs)
	}
}

func TestReconciledReleaseWithinALongAlertWindow(t *testing.T) {
	h := newHarness(t, "notify: {max_age: 14d}\ndefaults: {max_pages: 1}\nrepos: [{repo: o/r}]")
	repo := h.gh.Repo("o/r")
	repo.Releases = releases(0, 30, t0.Add(-time.Hour))
	h.tick()
	missed := githubtest.Release{Tag: "v1.0.500", Published: t0.Add(time.Hour)}
	repo.Releases = append(repo.Releases, missed) // below page 1 from the start
	h.now = t0.Add(10 * 24 * time.Hour)
	h.tick()
	if r := h.row("o/r", "v1.0.500"); r == nil || r.State != store.StatePending {
		t.Errorf("day-10 reconciliation = %+v, want pending", r)
	}
}

func TestSweepBudget(t *testing.T) {
	var b strings.Builder
	b.WriteString("schedule: {sweep_budget: 300ms}\nrepos:\n")
	for i := 0; i < 3; i++ {
		fmt.Fprintf(&b, "  - repo: o/slow%d\n", i)
	}
	for i := 0; i < 12; i++ {
		fmt.Fprintf(&b, "  - repo: o/fast%d\n", i)
	}
	h := newHarness(t, b.String())
	for i := 0; i < 3; i++ {
		h.gh.Repo(fmt.Sprintf("o/slow%d", i)).Hang = true
	}
	for i := 0; i < 12; i++ {
		h.gh.Repo(fmt.Sprintf("o/fast%d", i))
	}
	start := time.Now()
	id, _ := h.p.NextTick(h.ctx)
	if err := h.p.Sweep(h.ctx, id); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("sweep took %s with a 300ms budget", d)
	}
	for i := 0; i < 12; i++ {
		if ok, _ := h.st.Confirmed(h.ctx, fmt.Sprintf("o/fast%d", i), id); !ok {
			t.Errorf("o/fast%d not confirmed despite 3 hanging repos", i)
		}
	}
	if ok, _ := h.st.Confirmed(h.ctx, "o/slow0", id); ok {
		t.Error("a hanging repo was confirmed")
	}
}

func TestSweepCursorReachesEveryRepo(t *testing.T) {
	var b strings.Builder
	b.WriteString("schedule: {sweep_budget: 200ms}\nrepos:\n")
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&b, "  - repo: o/slow%d\n", i)
	}
	for i := 0; i < 5; i++ {
		fmt.Fprintf(&b, "  - repo: o/fast%d\n", i)
	}
	h := newHarness(t, b.String())
	for i := 0; i < 20; i++ {
		h.gh.Repo(fmt.Sprintf("o/slow%d", i)).Hang = true
	}
	for i := 0; i < 5; i++ {
		h.gh.Repo(fmt.Sprintf("o/fast%d", i))
	}
	confirmed := map[string]bool{}
	ticks := (25 + SweepConcurrency - 1) / SweepConcurrency // every repo within ⌈repos ÷ per-sweep⌉ ticks
	for tick := 0; tick < ticks; tick++ {
		id, _ := h.p.NextTick(h.ctx)
		if err := h.p.Sweep(h.ctx, id); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 5; i++ {
			name := fmt.Sprintf("o/fast%d", i)
			if ok, _ := h.st.Confirmed(h.ctx, name, id); ok {
				confirmed[name] = true
			}
		}
	}
	if len(confirmed) != 5 {
		t.Errorf("confirmed %d of 5 healthy repos within %d ticks behind 20 hanging ones", len(confirmed), ticks)
	}
}

func TestRotate(t *testing.T) {
	repos := []*config.Repo{{Repo: "a/a"}, {Repo: "b/b"}, {Repo: "c/c"}}
	names := func(rs []*config.Repo) string {
		var s []string
		for _, r := range rs {
			s = append(s, r.Name())
		}
		return strings.Join(s, ",")
	}
	for _, tc := range []struct {
		cursor string
		after  bool
		want   string
	}{
		{"", false, "a/a,b/b,c/c"},
		{"b/b", false, "b/b,c/c,a/a"},
		{"b/b", true, "c/c,a/a,b/b"},
		{"c/c", true, "a/a,b/b,c/c"},
		{"gone/x", true, "a/a,b/b,c/c"},
	} {
		if got := names(rotate(repos, tc.cursor, tc.after)); got != tc.want {
			t.Errorf("rotate(%q, %v) = %s, want %s", tc.cursor, tc.after, got, tc.want)
		}
	}
}
