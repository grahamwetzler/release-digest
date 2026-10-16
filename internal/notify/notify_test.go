package notify

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"release-digest/internal/config"
	"release-digest/internal/github"
	"release-digest/internal/github/githubtest"
	"release-digest/internal/ident"
	"release-digest/internal/secret"
	"release-digest/internal/store"
	"release-digest/internal/summarize"
)

var t0 = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

const ghsa = "GHSA-2345-6789-cfgh"

// advisoryJSON is a global advisory for o/r, vulnerable before 1.2.3.
func advisoryJSON(id, severity string, withdrawn bool) string {
	w := "null"
	if withdrawn {
		w = `"2026-10-20T00:00:00Z"`
	}
	return fmt.Sprintf(`{"ghsa_id":%q,"cve_id":"CVE-2026-1111","summary":"Remote code execution in the parser","severity":%q,
		"source_code_location":"https://github.com/o/r","withdrawn_at":%s,
		"vulnerabilities":[{"package":{"ecosystem":"go","name":"github.com/o/r"},"vulnerable_version_range":"< 1.2.3","first_patched_version":"1.2.3"}]}`,
		id, severity, w)
}

// fakeNtfy records alerts and answers with status (200 by default).
type fakeNtfy struct {
	*httptest.Server
	mu     sync.Mutex
	status int
	drop   bool // accept the request, then drop the connection
	got    []*http.Request
	bodies []string
	during func() // runs while a request is being handled
}

func newNtfy(t *testing.T) *fakeNtfy {
	f := &fakeNtfy{status: 200}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.got = append(f.got, r)
		f.bodies = append(f.bodies, string(b))
		status, drop, during := f.status, f.drop, f.during
		f.mu.Unlock()
		if during != nil {
			during()
		}
		if drop {
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		w.WriteHeader(status)
		fmt.Fprint(w, `{"code":40101,"http":401,"error":"unauthorized"}`)
	}))
	t.Cleanup(f.Close)
	return f
}

// set changes the fake's behavior under its lock.
func (f *fakeNtfy) set(fn func(f *fakeNtfy)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// request returns the i'th request and its body.
func (f *fakeNtfy) request(i int) (*http.Request, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.got[i], f.bodies[i]
}

func (f *fakeNtfy) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.got)
}

type harness struct {
	t    *testing.T
	st   *store.Store
	cfg  *config.Config
	gh   *githubtest.Server
	ntfy *fakeNtfy
	a    *Alerter
	o    *Outbox
	now  time.Time
	tick int64
}

func newHarness(t *testing.T, extra string) *harness {
	t.Helper()
	h := &harness{t: t, gh: githubtest.New(), ntfy: newNtfy(t), now: t0, tick: 1}
	t.Cleanup(h.gh.Close)
	h.gh.Repo("o/r")
	h.gh.Repo("o/mono")
	h.gh.Advisories = map[string]string{}
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "release-digest.yaml")
	yaml := fmt.Sprintf(`version: 1
public_base_url: https://rss.example.com
github:
  api_url: %s
notify:
  ntfy:
    url: %s/alerts
%s
repos:
  - repo: o/r
  - repo: o/mono
    packages:
      - {ecosystem: npm, name: "@o/web"}
`, h.gh.URL, h.ntfy.URL, extra)
	os.WriteFile(cfgPath, []byte(yaml), 0o644)
	cfg, err := config.Load(cfgPath, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	h.cfg = cfg
	st, err := store.Open(context.Background(), filepath.Join(dir, "rd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h.st = st
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	gh := &github.Client{BaseURL: h.gh.URL, HTTP: &http.Client{}}
	h.o = &Outbox{Store: st, Log: log, Now: func() time.Time { return h.now },
		Check: func(ctx context.Context, repo string) (github.Visibility, error) { return gh.CheckPublic(ctx, repo, 0) },
		Ntfy:  &Ntfy{URL: func() string { return cfg.Notify.Ntfy.URL }, Token: func() secret.String { return secret.New("tk_secret") }}}
	h.a = &Alerter{Store: st, GH: gh, Cfg: cfg, Log: log, Now: func() time.Time { return h.now }, Enabled: true, Kick: h.o.Kick}
	if err := h.o.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, repo := range []string{"o/r", "o/mono"} {
		rs := &store.RepoState{Repo: repo, InitializedAt: t0.Add(-30 * 24 * time.Hour)}
		if err := st.PutRepoState(context.Background(), rs); err != nil {
			t.Fatal(err)
		}
	}
	h.confirm()
	return h
}

func (h *harness) confirm() {
	for _, repo := range []string{"o/r", "o/mono"} {
		if err := h.st.Visibility(context.Background(), repo, h.tick, true, false, h.now, ""); err != nil {
			h.t.Fatal(err)
		}
	}
}

// nextTick advances time by d and confirms the repos in a new tick.
func (h *harness) nextTick(d time.Duration) {
	h.now = h.now.Add(d)
	h.tick++
	h.confirm()
}

// add stores a release with notes, published age ago.
func (h *harness) add(repo, tag, prev, state, notes string, age time.Duration) *store.Release {
	h.t.Helper()
	return h.addWith(repo, tag, prev, state, notes, age, nil)
}

func (h *harness) addWith(repo, tag, prev, state, notes string, age time.Duration, edit func(*store.Release)) *store.Release {
	h.t.Helper()
	ctx := context.Background()
	hash, _ := h.st.PutNotes(ctx, notes)
	r := &store.Release{Repo: repo, Tag: tag, PrevTag: prev, GUID: ident.GUID(repo, tag), Kind: "release",
		HTMLURL: "https://github.com/" + repo + "/releases/tag/" + tag, Published: h.now.Add(-age), FirstSeen: h.now.Add(-age),
		State: state, Origin: store.OriginLive, NotesSource: "release_body", NotesHash: hash, NotesResolved: true,
		ContentUpdated: h.now.Add(-age)}
	if edit != nil {
		edit(r)
	}
	if _, err := h.st.InsertRelease(ctx, r); err != nil {
		h.t.Fatal(err)
	}
	return r
}

// summarize stores a summary for the release and publishes it as summarized.
func (h *harness) summarize(r *store.Release, s summarize.Summary) {
	h.t.Helper()
	ctx := context.Background()
	key := "k-" + r.Tag + s.SecurityUrgency
	if err := h.st.PutSummary(ctx, &store.Summary{Key: key, Model: "m", JSON: s.Encode(), Created: h.now}); err != nil {
		h.t.Fatal(err)
	}
	ok, err := h.st.Summarized(ctx, r.Repo, r.Tag, r.NotesHash, key, h.tick, h.now)
	if err != nil || !ok {
		h.t.Fatalf("Summarized: %v, %v", ok, err)
	}
}

func (h *harness) pass() {
	h.t.Helper()
	if err := h.a.Pass(context.Background(), h.tick); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) drain() {
	h.t.Helper()
	if err := h.o.Drain(context.Background()); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) get(repo, tag string) *store.Release {
	h.t.Helper()
	r, err := h.st.Release(context.Background(), repo, tag)
	if err != nil || r == nil {
		h.t.Fatalf("%s@%s: %v, %v", repo, tag, r, err)
	}
	return r
}

func (h *harness) alert(r *store.Release) *store.Notification {
	h.t.Helper()
	n, err := h.st.Notification(context.Background(), r.GUID)
	if err != nil {
		h.t.Fatal(err)
	}
	return n
}

// wait advances time to the worker's next wake that finds r's alert due:
// WakeInterval steps, skipping the idle ones.
func (h *harness) wait(r *store.Release) {
	next := h.now.Add(WakeInterval)
	if n := h.alert(r); n != nil && n.State == store.AlertQueued && n.NextAttempt.After(next) {
		next = next.Add(n.NextAttempt.Sub(next).Truncate(WakeInterval))
		if next.Before(n.NextAttempt) {
			next = next.Add(WakeInterval)
		}
	}
	h.now = next
}

func (h *harness) setAdvisory(id, js string) {
	h.gh.Mu.Lock()
	defer h.gh.Mu.Unlock()
	if js == "" {
		delete(h.gh.Advisories, id)
		return
	}
	h.gh.Advisories[id] = js
}

const fixNotes = "## Security\n- Fix " + ghsa + ": crafted input runs code\n"

func TestAdvisorySignalAlertsWithoutTheLLM(t *testing.T) {
	h := newHarness(t, "")
	h.setAdvisory(ghsa, advisoryJSON(ghsa, "high", false))
	r := h.add("o/r", "v1.2.3", "v1.2.2", store.StateFallback, fixNotes, time.Hour)
	h.pass()
	got := h.get("o/r", "v1.2.3")
	if got.Urgency != store.UrgencyUrgent || !got.ContentUpdated.Equal(t0) {
		t.Errorf("urgency %s, content updated %v", got.Urgency, got.ContentUpdated)
	}
	n := h.alert(r)
	if n == nil || n.State != store.AlertQueued || n.Reason != "advisory:"+ghsa || !n.Expires.Equal(r.Published.Add(7*24*time.Hour)) {
		t.Fatalf("alert = %+v", n)
	}
	h.drain()
	if n := h.alert(r); n.State != store.AlertSent {
		t.Fatalf("after a drain: %+v", n)
	}
	if h.ntfy.count() != 1 {
		t.Fatalf("%d POSTs", h.ntfy.count())
	}
	req, body := h.ntfy.request(0)
	title, _ := new(mime.WordDecoder).DecodeHeader(req.Header.Get("Title"))
	for k, want := range map[string]string{
		"path": "/alerts", "Title": "🚨 o/r v1.2.3: security release", "Priority": "4", "Markdown": "yes",
		"Tags": "rotating_light,o/r," + ghsa, "Click": "https://github.com/o/r/releases/tag/v1.2.3",
		"Actions": "view, Advisory, https://github.com/advisories/" + ghsa, "Authorization": "Bearer tk_secret",
	} {
		got := req.Header.Get(k)
		switch k {
		case "path":
			got = req.URL.Path
		case "Title":
			got = title
		}
		if got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if !strings.Contains(body, "Remote code execution in the parser") || !strings.Contains(body, "**high**") {
		t.Errorf("body = %q", body)
	}

	// Recomputing, a new summary or a restart never enqueues it again.
	h.summarize(r, summarize.Summary{Headline: "Fixes RCE", Security: []string{"Fix " + ghsa}, SecurityUrgency: "urgent"})
	h.pass()
	h.drain()
	if h.ntfy.count() != 1 || h.alert(r).State != store.AlertSent {
		t.Errorf("re-sent: %d POSTs, %+v", h.ntfy.count(), h.alert(r))
	}
}

func TestDecide(t *testing.T) {
	r := &store.Release{Repo: "o/r", Tag: "v1.2.3", PrevTag: "v1.2.2"}
	found := &store.Advisory{ID: ghsa, Found: true, FetchedAt: t0, JSON: "[" + advisoryJSON(ghsa, "high", false) + "]"}
	link := []store.Link{{AdvisoryID: ghsa, Advisory: found}}
	llm := func(u string, security ...string) *summarize.Summary {
		return &summarize.Summary{Headline: "h", Security: security, SecurityUrgency: u}
	}
	for _, tc := range []struct {
		name    string
		in      Inputs
		urgency string
		source  string
		prio    int
	}{
		{"a high advisory isn't talked down by an LLM none",
			Inputs{Release: r, Notes: fixNotes, Links: link, Summary: llm("none"), MinSeverity: "high"}, "urgent", "advisory:" + ghsa, 4},
		{"below min_severity is advisory",
			Inputs{Release: r, Notes: fixNotes, Links: link, MinSeverity: "critical"}, "advisory", "advisory", 0},
		{"LLM urgent with a security keyword",
			Inputs{Release: r, Notes: "Fixes a security hole", Summary: llm("urgent", "Fixes RCE"), MinSeverity: "high"}, "urgent", "llm", 5},
		{"LLM urgent without a keyword is downgraded",
			Inputs{Release: r, Notes: "Faster startup", Summary: llm("urgent", "Fixes RCE"), MinSeverity: "high"}, "advisory", "llm", 0},
		{"LLM urgent with an empty security list is downgraded",
			Inputs{Release: r, Notes: "security", Summary: llm("urgent"), MinSeverity: "high"}, "advisory", "llm", 0},
		{"an advisory for another repo never counts",
			Inputs{Release: &store.Release{Repo: "o/other", Tag: "v1.2.3", PrevTag: "v1.2.2"}, Notes: fixNotes, Links: link, MinSeverity: "medium"}, "none", "", 0},
		{"no fix evidence: the predecessor was already patched",
			Inputs{Release: &store.Release{Repo: "o/r", Tag: "v1.2.4", PrevTag: "v1.2.3"}, Notes: fixNotes, Links: link, MinSeverity: "high"}, "none", "", 0},
		{"unknown predecessor, text evidence",
			Inputs{Release: &store.Release{Repo: "o/r", Tag: "v1.2.3"}, Notes: fixNotes, Links: link, MinSeverity: "high"}, "urgent", "advisory:" + ghsa, 4},
		{"unknown predecessor, notes that don't fix it",
			Inputs{Release: &store.Release{Repo: "o/r", Tag: "v1.2.3"}, Notes: ghsa + " is not yet fixed", Links: link, MinSeverity: "high"}, "none", "", 0},
		{"withdrawn",
			Inputs{Release: r, Notes: fixNotes, MinSeverity: "high", Links: []store.Link{{AdvisoryID: ghsa,
				Advisory: &store.Advisory{Found: true, FetchedAt: t0, JSON: "[" + advisoryJSON(ghsa, "critical", true) + "]"}}}}, "none", "", 0},
		{"critical advisory is priority 5",
			Inputs{Release: r, Notes: fixNotes, MinSeverity: "high", Links: []store.Link{{AdvisoryID: ghsa,
				Advisory: &store.Advisory{Found: true, FetchedAt: t0, JSON: "[" + advisoryJSON(ghsa, "critical", false) + "]"}}}}, "urgent", "advisory:" + ghsa, 5},
	} {
		d := Decide(tc.in)
		if d.Urgency != tc.urgency || d.Source != tc.source || d.Priority != tc.prio {
			t.Errorf("%s: urgency %s, source %q, priority %d; links %+v", tc.name, d.Urgency, d.Source, d.Priority, d.Links)
		}
	}
	// A CVE ID names its GHSA advisory as the source.
	cve := Decide(Inputs{Release: r, Notes: "- Fixes CVE-2026-1111", MinSeverity: "high",
		Links: []store.Link{{AdvisoryID: "CVE-2026-1111", Advisory: found}}})
	if cve.Source != "advisory:"+ghsa {
		t.Errorf("CVE source = %q", cve.Source)
	}
}

func TestPackagesMatchAMonorepo(t *testing.T) {
	h := newHarness(t, "")
	const id = "GHSA-mpqr-2345-6789"
	h.setAdvisory(id, `{"ghsa_id":"`+id+`","severity":"critical","source_code_location":"https://github.com/someone/else",
		"vulnerabilities":[{"package":{"ecosystem":"npm","name":"@o/web"},"vulnerable_version_range":"< 2.0.0","first_patched_version":"2.0.0"}]}`)
	h.add("o/mono", "web/v2.0.0", "web/v1.9.0", store.StateFallback, "Fixes "+id, time.Hour)
	h.add("o/r", "v2.0.0", "v1.9.0", store.StateFallback, "Fixes "+id, time.Hour) // not its package
	h.pass()
	if u := h.get("o/mono", "web/v2.0.0").Urgency; u != store.UrgencyUrgent {
		t.Errorf("monorepo package: %s", u)
	}
	if u := h.get("o/r", "v2.0.0").Urgency; u != store.UrgencyNone {
		t.Errorf("unrelated repo: %s", u)
	}
}

func TestNoAlertForBackfillOldOrPrerelease(t *testing.T) {
	h := newHarness(t, "")
	h.setAdvisory(ghsa, advisoryJSON(ghsa, "high", false))
	// Tracking began 2h ago: a release from 3h ago is backfill, though recent.
	if err := h.st.PutRepoState(context.Background(), &store.RepoState{Repo: "o/r", InitializedAt: t0.Add(-2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	h.confirm()
	backfill := h.add("o/r", "v1.2.3", "v1.2.2", store.StateFallback, fixNotes, 3*time.Hour)
	old := h.add("o/r", "v1.3.0", "v1.2.2", store.StateFallback, fixNotes, 8*24*time.Hour)
	pre := h.addWith("o/r", "v1.2.4-rc.1", "v1.2.2", store.StateFallback, fixNotes, time.Hour, func(r *store.Release) { r.Prerelease = true })
	h.pass()
	for _, r := range []*store.Release{backfill, old, pre} {
		if u := h.get(r.Repo, r.Tag).Urgency; u != store.UrgencyUrgent {
			t.Errorf("%s: urgency %s; the feed is still marked", r.Tag, u)
		}
		if n := h.alert(r); n != nil {
			t.Errorf("%s: alert %+v", r.Tag, n)
		}
	}
}

func TestDeescalationCancelsAndReescalationRequeues(t *testing.T) {
	h := newHarness(t, "")
	h.ntfy.set(func(f *fakeNtfy) { f.status = 503 }) // ntfy is down
	h.setAdvisory(ghsa, advisoryJSON(ghsa, "high", false))
	r := h.add("o/r", "v1.2.3", "v1.2.2", store.StateFallback, fixNotes, time.Hour)
	h.pass()
	h.drain()
	if n := h.alert(r); n.State != store.AlertQueued || n.Attempts != 1 {
		t.Fatalf("after a failed POST: %+v", n)
	}

	// Withdrawn during the outage: the marking goes and the alert is cancelled.
	h.setAdvisory(ghsa, advisoryJSON(ghsa, "high", true))
	h.nextTick(RefreshRecent)
	h.pass()
	got := h.get("o/r", "v1.2.3")
	if got.Urgency != store.UrgencyNone || !got.ContentUpdated.Equal(h.now) {
		t.Errorf("withdrawn: urgency %s, updated %v", got.Urgency, got.ContentUpdated)
	}
	if n := h.alert(r); n.State != store.AlertCancelled {
		t.Errorf("withdrawn: alert %s", n.State)
	}

	// Re-escalated by another advisory: queued once more, rebuilt.
	const other = "GHSA-cfgh-mpqr-2345"
	h.setAdvisory(other, advisoryJSON(other, "critical", false))
	hash, _ := h.st.PutNotes(context.Background(), fixNotes+"- Fix "+other+"\n")
	if err := h.st.ResolveNotes(context.Background(), "o/r", "v1.2.3", "release_body", hash, h.now); err != nil {
		t.Fatal(err)
	}
	h.nextTick(time.Hour)
	h.ntfy.set(func(f *fakeNtfy) { f.status = 200 })
	h.pass()
	n := h.alert(r)
	if n.State != store.AlertQueued || n.Reason != "advisory:"+other || !strings.Contains(n.Payload, `"priority":5`) ||
		strings.Contains(n.Payload, ghsa+")") {
		t.Fatalf("re-escalated: %+v", n)
	}
	h.drain()
	if n := h.alert(r); n.State != store.AlertSent || h.ntfy.count() != 2 {
		t.Errorf("after re-escalation: %s, %d POSTs", n.State, h.ntfy.count())
	}
}

func TestNotFoundAdvisoryAppearsLater(t *testing.T) {
	h := newHarness(t, "")
	r := h.add("o/r", "v1.2.3", "v1.2.2", store.StateFallback, fixNotes, time.Hour)
	h.summarize(r, summarize.Summary{Headline: "Parser fix", Fixes: []string{"Parser"}, SecurityUrgency: "none"})
	h.pass()
	if a, _ := h.st.Advisory(context.Background(), ghsa); a == nil || a.Found || !a.NextFetchAt.Equal(h.now.Add(NotFoundRetry)) {
		t.Fatalf("not found: %+v", a)
	}
	h.setAdvisory(ghsa, advisoryJSON(ghsa, "high", false))
	h.nextTick(30 * time.Minute)
	h.pass()
	if u := h.get("o/r", "v1.2.3").Urgency; u != store.UrgencyNone {
		t.Errorf("looked up before the retry time: %s", u)
	}
	h.nextTick(30 * time.Minute)
	h.pass()
	if u := h.get("o/r", "v1.2.3").Urgency; u != store.UrgencyUrgent {
		t.Errorf("after the retry: %s", u)
	}
	if n := h.alert(r); n == nil || n.State != store.AlertQueued || !strings.Contains(n.Payload, "Parser fix") {
		t.Errorf("alert = %+v", n)
	}
}

func TestLateSeverityUpgradeMarksButDoesNotAlert(t *testing.T) {
	h := newHarness(t, "")
	h.setAdvisory(ghsa, advisoryJSON(ghsa, "medium", false))
	r := h.add("o/r", "v1.2.3", "v1.2.2", store.StateSummarized, fixNotes, time.Hour)
	h.pass()
	if u := h.get("o/r", "v1.2.3").Urgency; u != store.UrgencyAdvisory {
		t.Fatalf("medium: %s", u)
	}
	for range 10 { // ten days of daily refreshes, then weekly ones
		h.nextTick(24 * time.Hour)
		h.pass()
	}
	h.setAdvisory(ghsa, advisoryJSON(ghsa, "critical", false)) // raised on day 10
	for range 7 {
		h.nextTick(24 * time.Hour)
		h.pass()
	}
	if u := h.get("o/r", "v1.2.3").Urgency; u != store.UrgencyUrgent {
		t.Errorf("raised to critical: %s", u)
	}
	if n := h.alert(r); n != nil {
		t.Errorf("alerted past max_age: %+v", n)
	}
}

func TestClaimOutcomes(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (*harness, *store.Release) {
		h := newHarness(t, "")
		h.setAdvisory(ghsa, advisoryJSON(ghsa, "high", false))
		r := h.add("o/r", "v1.2.3", "v1.2.2", store.StateFallback, fixNotes, time.Hour)
		h.pass()
		return h, r
	}
	t.Run("cancelled while queued", func(t *testing.T) {
		h, r := setup(t)
		h.st.Cancel(ctx, r.GUID)
		h.drain()
		if h.ntfy.count() != 0 || h.alert(r).State != store.AlertCancelled {
			t.Errorf("%d POSTs, %s", h.ntfy.count(), h.alert(r).State)
		}
	})
	t.Run("cancelled after the claim, before the re-read", func(t *testing.T) {
		h, r := setup(t)
		check := h.o.Check
		h.o.Check = func(ctx context.Context, repo string) (github.Visibility, error) {
			h.st.Cancel(ctx, r.GUID)
			return check(ctx, repo)
		}
		h.drain()
		if h.ntfy.count() != 0 || h.alert(r).State != store.AlertCancelled {
			t.Errorf("%d POSTs, %s", h.ntfy.count(), h.alert(r).State)
		}
	})
	t.Run("cancelled during a POST that succeeds", func(t *testing.T) {
		h, r := setup(t)
		h.ntfy.set(func(f *fakeNtfy) { f.during = func() { h.st.Cancel(ctx, r.GUID) } })
		h.drain()
		if h.ntfy.count() != 1 || h.alert(r).State != store.AlertSent {
			t.Errorf("%d POSTs, %s", h.ntfy.count(), h.alert(r).State)
		}
	})
	t.Run("cancelled during a POST that fails", func(t *testing.T) {
		h, r := setup(t)
		h.ntfy.set(func(f *fakeNtfy) { f.status = 500 })
		h.ntfy.set(func(f *fakeNtfy) { f.during = func() { h.st.Cancel(ctx, r.GUID) } })
		h.drain()
		h.now = h.now.Add(time.Hour)
		h.drain()
		if h.ntfy.count() != 1 || h.alert(r).State != store.AlertCancelled {
			t.Errorf("%d POSTs, %s", h.ntfy.count(), h.alert(r).State)
		}
	})
	t.Run("de-escalated and re-escalated while sending, then the POST fails", func(t *testing.T) {
		h, r := setup(t)
		h.ntfy.set(func(f *fakeNtfy) { f.status = 500 })
		h.ntfy.set(func(f *fakeNtfy) {
			f.during = func() {
				h.st.Cancel(ctx, r.GUID)
				h.st.Rebuild(ctx, r.GUID, "llm", `{"title":"rebuilt","priority":5,"tags":[],"click":"","body":"new"}`, r.Published.Add(7*24*time.Hour), h.now)
			}
		})
		h.drain()
		if n := h.alert(r); n.State != store.AlertQueued || !strings.Contains(n.Payload, "rebuilt") {
			t.Errorf("%+v", n)
		}
	})
	t.Run("visibility check times out", func(t *testing.T) {
		h, r := setup(t)
		h.gh.Repo("o/r").Status = 502
		h.drain()
		n := h.alert(r)
		if h.ntfy.count() != 0 || n.State != store.AlertQueued || !n.NextAttempt.Equal(h.now.Add(RetryBase)) {
			t.Errorf("%d POSTs, %+v", h.ntfy.count(), n)
		}
	})
	t.Run("the repo went private", func(t *testing.T) {
		h, r := setup(t)
		h.gh.Repo("o/r").Private = true
		h.drain()
		rs, _ := h.st.RepoState(ctx, "o/r")
		if h.ntfy.count() != 0 || h.alert(r).State != store.AlertQueued || !rs.Held {
			t.Errorf("%d POSTs, %s, held %v", h.ntfy.count(), h.alert(r).State, rs.Held)
		}
		// Held: not claimed again until the sweep confirms it.
		h.gh.Repo("o/r").Private = false
		h.now = h.now.Add(2 * time.Hour)
		h.drain()
		if h.ntfy.count() != 0 {
			t.Error("sent for a held repo")
		}
		h.nextTick(0)
		h.drain()
		if h.ntfy.count() != 1 {
			t.Errorf("%d POSTs once public again", h.ntfy.count())
		}
	})
	t.Run("a crash while sending", func(t *testing.T) {
		h, r := setup(t)
		if n, _ := h.st.Claim(ctx, h.now); n == nil {
			t.Fatal("nothing to claim")
		}
		h.o.Recover(ctx) // the restart
		if n := h.alert(r); n.State != store.AlertQueued {
			t.Errorf("after restart: %s", n.State)
		}
	})
	t.Run("ntfy accepts, then drops the connection, then a restart", func(t *testing.T) {
		h, r := setup(t)
		h.ntfy.set(func(f *fakeNtfy) { f.drop = true })
		h.drain()
		h.ntfy.set(func(f *fakeNtfy) { f.drop = false })
		h.o.Recover(ctx)
		h.now = h.now.Add(time.Hour)
		h.drain()
		if h.ntfy.count() != 2 || h.alert(r).State != store.AlertSent {
			t.Errorf("%d POSTs, %s; want the at-least-once duplicate", h.ntfy.count(), h.alert(r).State)
		}
	})
}

func TestRetriesUntilExpiry(t *testing.T) {
	h := newHarness(t, "")
	h.ntfy.set(func(f *fakeNtfy) { f.status = 500 })
	h.setAdvisory(ghsa, advisoryJSON(ghsa, "high", false))
	r := h.add("o/r", "v1.2.3", "v1.2.2", store.StateFallback, fixNotes, time.Hour)
	h.pass()
	expires := r.Published.Add(7 * 24 * time.Hour)
	var attempts []time.Time
	for h.now.Before(expires.Add(time.Hour)) {
		before := h.ntfy.count()
		h.drain()
		if h.ntfy.count() > before {
			attempts = append(attempts, h.now)
		}
		h.wait(r)
	}
	n := h.alert(r)
	if n.State != store.AlertExpired {
		t.Fatalf("state %s", n.State)
	}
	for i := 1; i < len(attempts); i++ {
		if gap := attempts[i].Sub(attempts[i-1]); gap > RetryMax+WakeInterval {
			t.Errorf("attempts %v and %v are %v apart", attempts[i-1], attempts[i], gap)
		}
	}
	if last := attempts[len(attempts)-1]; last.Before(expires.Add(-FinalAttempt)) || !last.Before(expires) {
		t.Errorf("last attempt at %v; want the final one in the 2m before expiry %v", last, expires)
	}
	if c, _ := h.st.ExpiredUnacked(context.Background()); c != 1 {
		t.Errorf("expired unacked = %d", c)
	}
	acked, _ := h.st.AckExpired(context.Background(), h.now)
	if c, _ := h.st.ExpiredUnacked(context.Background()); len(acked) != 1 || c != 0 {
		t.Errorf("after ack: %d acked, %d unacked", len(acked), c)
	}
}

func TestOutageEndingBeforeExpiryDelivers(t *testing.T) {
	h := newHarness(t, "")
	h.ntfy.set(func(f *fakeNtfy) { f.status = 500 })
	h.setAdvisory(ghsa, advisoryJSON(ghsa, "high", false))
	r := h.add("o/r", "v1.2.3", "v1.2.2", store.StateFallback, fixNotes, time.Hour)
	h.pass()
	expires := r.Published.Add(7 * 24 * time.Hour)
	for h.now.Before(expires) {
		if !h.now.Before(expires.Add(-30 * time.Minute)) {
			h.ntfy.set(func(f *fakeNtfy) { f.status = 200 })
		}
		h.drain()
		h.wait(r)
	}
	if n := h.alert(r); n.State != store.AlertSent {
		t.Errorf("state %s; want the final pre-expiry attempt to deliver", n.State)
	}
}

func TestConfigChangeHoldsAndCancelsAStaleAlert(t *testing.T) {
	h := newHarness(t, "")
	h.setAdvisory(ghsa, advisoryJSON(ghsa, "high", false))
	r := h.add("o/r", "v1.2.3", "v1.2.2", store.StateFallback, fixNotes, time.Hour)
	if err := h.a.CheckConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.pass()
	if n := h.alert(r); n == nil || n.State != store.AlertQueued {
		t.Fatalf("alert = %+v", n)
	}
	// Restarted with min_severity: critical. Until the first tick
	// recomputes, the alert is held; then it's cancelled.
	h.cfg.Notify.MinSeverity = "critical"
	if err := h.a.CheckConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.drain()
	if h.ntfy.count() != 0 {
		t.Fatal("a stale alert was sent before its recompute")
	}
	h.pass()
	if n := h.alert(r); n.State != store.AlertCancelled || h.get("o/r", "v1.2.3").Urgency != store.UrgencyAdvisory {
		t.Errorf("after recompute: %s, urgency %s", n.State, h.get("o/r", "v1.2.3").Urgency)
	}
}

func TestPayloadBudget(t *testing.T) {
	r := &store.Release{Repo: "o/r", Tag: "v1.2.3", HTMLURL: "https://github.com/o/r/releases/tag/v1.2.3"}
	long := strings.Repeat("ünïcödé ", 120) // ~1,900 bytes each
	sum := &summarize.Summary{Headline: "Fixes remote code execution", SecurityReason: strings.Repeat("é", 400),
		Security: []string{long, long, long, long, long, long}, SecurityUrgency: "urgent"}
	d := Decision{Urgency: "urgent", Source: "llm", Priority: 5, Links: []LinkResult{{ID: ghsa, Fires: true, Severity: "critical"}}}
	p := BuildPayload(r, sum, d)
	if len(p.Body) > BodyBudget || !utf8.ValidString(p.Body) || !strings.HasSuffix(p.Body, "…") {
		t.Errorf("body is %d bytes, valid %v, ends %q", len(p.Body), utf8.ValidString(p.Body), p.Body[len(p.Body)-10:])
	}
	if !strings.HasPrefix(p.Body, "**Fixes remote code execution**\n\nSeverity: **critical** ("+ghsa+")") {
		t.Errorf("body starts %q", p.Body[:80])
	}
	if strings.Contains(p.Body, strings.Repeat("é", 151)) {
		t.Error("security_reason not capped at 300 bytes")
	}
}

func TestNtfyErrorsDontLeakTheTopicOrToken(t *testing.T) {
	f := newNtfy(t)
	f.set(func(f *fakeNtfy) { f.status = 401 })
	n := &Ntfy{URL: func() string { return f.URL + "/secret-topic-123" }, Token: func() secret.String { return secret.New("tk_abcdef") }}
	err := n.Send(context.Background(), Payload{Title: "t", Body: "b"})
	if err == nil || strings.Contains(err.Error(), "secret-topic-123") || strings.Contains(err.Error(), "tk_abcdef") {
		t.Errorf("err = %v", err)
	}
	down := &Ntfy{URL: func() string { return "http://127.0.0.1:1/secret-topic-123" }}
	if err := down.Send(context.Background(), Payload{}); err == nil || strings.Contains(err.Error(), "secret-topic") {
		t.Errorf("err = %v", err)
	}
}

func TestStaleAlertsWaitForRecompute(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (*harness, *store.Release) {
		h := newHarness(t, "")
		h.setAdvisory(ghsa, advisoryJSON(ghsa, "high", false))
		r := h.add("o/r", "v1.2.3", "v1.2.2", store.StateFallback, fixNotes, time.Hour)
		h.pass()
		if n := h.alert(r); n == nil || n.State != store.AlertQueued {
			t.Fatalf("alert = %+v", n)
		}
		return h, r
	}
	t.Run("held by another check after the worker's", func(t *testing.T) {
		h, r := setup(t)
		check := h.o.Check
		h.o.Check = func(ctx context.Context, repo string) (github.Visibility, error) {
			vis, err := check(ctx, repo)
			// A summarize worker's check finds the repo private meanwhile.
			h.st.Visibility(ctx, repo, h.tick, false, true, h.now, "private")
			return vis, err
		}
		h.drain()
		if h.ntfy.count() != 0 || h.alert(r).State != store.AlertQueued {
			t.Errorf("%d POSTs, %s", h.ntfy.count(), h.alert(r).State)
		}
	})
	t.Run("notes changed, not yet rescanned", func(t *testing.T) {
		h, r := setup(t)
		hash, _ := h.st.PutNotes(ctx, "Faster startup")
		if err := h.st.ResolveNotes(ctx, "o/r", "v1.2.3", "release_body", hash, h.now); err != nil {
			t.Fatal(err)
		}
		h.drain()
		if h.ntfy.count() != 0 {
			t.Fatal("sent an alert for notes that no longer fix the advisory")
		}
		h.pass()
		if n := h.alert(r); n.State != store.AlertCancelled {
			t.Errorf("after the rescan: %s", n.State)
		}
	})
	t.Run("deleted on GitHub", func(t *testing.T) {
		h, r := setup(t)
		if err := h.st.RecordVerify(ctx, "o/r", "v1.2.3", store.VerifyNotFound, h.now); err != nil {
			t.Fatal(err)
		}
		h.drain()
		if h.ntfy.count() != 0 {
			t.Fatal("alerted for a deleted release")
		}
		h.pass()
		if n := h.alert(r); n.State != store.AlertCancelled {
			t.Errorf("after recompute: %s", n.State)
		}
		// Back again: re-escalated and sent.
		h.st.RecordVerify(ctx, "o/r", "v1.2.3", store.VerifyFound, h.now)
		h.pass()
		h.drain()
		if n := h.alert(r); n.State != store.AlertSent {
			t.Errorf("after reappearing: %s", n.State)
		}
	})
}

func TestChangesDuringTheCheckStopTheSend(t *testing.T) {
	ctx := context.Background()
	for name, change := range map[string]func(h *harness){
		"notes changed": func(h *harness) {
			hash, _ := h.st.PutNotes(ctx, "Faster startup")
			h.st.ResolveNotes(ctx, "o/r", "v1.2.3", "release_body", hash, h.now)
		},
		"deleted": func(h *harness) { h.st.RecordVerify(ctx, "o/r", "v1.2.3", store.VerifyNotFound, h.now) },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, "")
			h.setAdvisory(ghsa, advisoryJSON(ghsa, "high", false))
			r := h.add("o/r", "v1.2.3", "v1.2.2", store.StateFallback, fixNotes, time.Hour)
			h.pass()
			check := h.o.Check
			h.o.Check = func(ctx context.Context, repo string) (github.Visibility, error) {
				change(h) // after the claim, before the POST
				return check(ctx, repo)
			}
			h.drain()
			if h.ntfy.count() != 0 {
				t.Fatal("sent a stale alert")
			}
			h.pass()
			if n := h.alert(r); n.State != store.AlertCancelled {
				t.Errorf("after recompute: %s", n.State)
			}
		})
	}
}
