package summarize

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"release-digest/internal/config"
	"release-digest/internal/ident"
	"release-digest/internal/llm"
	"release-digest/internal/llm/llmtest"
	"release-digest/internal/store"
)

const good = `{"headline":"Adds streaming","breaking":[],"features":["Streaming responses"],"fixes":[],"security":[],"noise_only":false}`

func TestKey(t *testing.T) {
	e := Endpoint{BaseURL: "https://openrouter.ai/api/v1", Model: "m", Temperature: 0.2}
	in := Input{Repo: "o/r", Tag: "v2", PrevTag: "v1", Source: "release_body", Notes: "notes"}
	base := Key(e, Messages(DefaultPrompt, in))
	if again := Key(e, Messages(DefaultPrompt, in)); again != base {
		t.Error("identical requests have different keys")
	}
	vary := map[string]string{}
	add := func(name string, e Endpoint, prompt string, in Input) { vary[name] = Key(e, Messages(prompt, in)) }
	add("repo", e, DefaultPrompt, Input{"o/x", "v2", "v1", "release_body", "notes"})
	add("version", e, DefaultPrompt, Input{"o/r", "v3", "v1", "release_body", "notes"})
	add("prev_tag", e, DefaultPrompt, Input{"o/r", "v2", "v0", "release_body", "notes"})
	add("no prev_tag", e, DefaultPrompt, Input{"o/r", "v2", "", "release_body", "notes"})
	add("source", e, DefaultPrompt, Input{"o/r", "v2", "v1", "changelog", "notes"})
	add("notes", e, DefaultPrompt, Input{"o/r", "v2", "v1", "release_body", "notes!"})
	add("prompt", e, DefaultPrompt+" ", in)
	add("model", Endpoint{e.BaseURL, "m2", 0.2}, DefaultPrompt, in)
	add("base_url", Endpoint{"http://ollama:11434/v1", "m", 0.2}, DefaultPrompt, in)
	add("temperature", Endpoint{e.BaseURL, "m", 0.3}, DefaultPrompt, in)
	seen := map[string]string{base: "base"}
	for name, k := range vary {
		if prev, ok := seen[k]; ok {
			t.Errorf("%s has the same key as %s", name, prev)
		}
		seen[k] = name
	}
}

func TestLoadPromptReadsTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prompt.md")
	os.WriteFile(path, []byte("Be brief.\n"), 0o644)
	if p, err := LoadPrompt(path); err != nil || p != "Be brief." {
		t.Errorf("LoadPrompt = %q, %v", p, err)
	}
	os.WriteFile(path, []byte(" \n"), 0o644)
	if _, err := LoadPrompt(path); err == nil {
		t.Error("an empty prompt file was accepted")
	}
	if p, _ := LoadPrompt(""); p != DefaultPrompt {
		t.Error("no prompt file: not the built-in prompt")
	}
}

func TestParse(t *testing.T) {
	s, err := Parse("```json\n" + `{"headline":"  Faster   builds ","breaking":[],"features":["a"," ","b","c","d","e","f","g"],"fixes":[],"security":[],"noise_only":false}` + "\n```")
	if err != nil {
		t.Fatal(err)
	}
	if s.Headline != "Faster builds" || len(s.Features) != MaxItems || s.Features[1] != "b" || len(s.Breaking) != 0 {
		t.Errorf("parsed %+v", s)
	}
	for _, bad := range []string{
		`not json`,
		`{"headline":""}`,
		`{"headline":"` + strings.Repeat("x", MaxHeadline+1) + `"}`,
		`{"headline":"ok"} trailing`,
		`{"headline":1}`,
	} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%.40q) accepted", bad)
		}
	}
	if _, err := Parse(`{"headline":"ok","breaking":[],"features":[],"fixes":[],"security":[]}`); err == nil ||
		!strings.Contains(err.Error(), "lacks noise_only") {
		t.Errorf("a reply without noise_only: %v", err)
	}
	if _, err := Parse(`{"headline":"ok","breaking":null,"features":[],"fixes":[],"security":[],"noise_only":false}`); err == nil {
		t.Error("a null list was accepted")
	}
	s, err = Parse(`{"headline":"ok","breaking":["Drops X"],"features":[],"fixes":[],"security":[],"noise_only":true}`)
	if err != nil || s.NoiseOnly {
		t.Errorf("noise_only with a breaking change: %+v, %v", s, err)
	}
	if d, _ := Decode(`{"headline":"ok","security":["CVE-1"],"noise_only":true}`); d.NoiseOnly {
		t.Error("Decode kept a contradictory noise_only")
	}
	if _, err := Parse(`{"headline":"` + strings.Repeat("é", MaxHeadline) + `","breaking":[],"features":[],"fixes":[],"security":[],"noise_only":false}`); err != nil {
		t.Errorf("a headline of %d runes was rejected: %v", MaxHeadline, err)
	}
}

// harness is a store with one confirmed repo, a fake LLM and a gate.
type harness struct {
	t     *testing.T
	st    *store.Store
	fake  *llmtest.Fake
	s     *Summarizer
	now   time.Time
	tick  int64
	gated atomic.Int32 // gate calls
	deny  atomic.Bool  // the gate fails
}

var t0 = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "release-digest.yaml")
	os.WriteFile(cfgPath, []byte("version: 1\npublic_base_url: https://rss.example.com\nllm:\n  model: test/model\n"), 0o644)
	cfg, err := config.Load(cfgPath, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), filepath.Join(dir, "rd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h := &harness{t: t, st: st, fake: llmtest.New(good), now: t0, tick: 1}
	t.Cleanup(h.fake.Close)
	h.s = &Summarizer{
		Store: st, Cfg: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		LLM: &llm.Client{BaseURL: h.fake.URL(), Model: cfg.LLM.Model, Temperature: *cfg.LLM.Temperature,
			Sleep: func(context.Context, time.Duration) bool { return true }},
		Now: func() time.Time { return h.now },
	}
	h.s.Gate = func(ctx context.Context, repo string) (bool, error) {
		h.gated.Add(1)
		if h.deny.Load() {
			return false, st.Visibility(ctx, repo, h.tick, false, false, h.now, "boom")
		}
		return true, nil
	}
	h.confirm("o/r")
	return h
}

func (h *harness) confirm(repo string) {
	if err := h.st.Visibility(context.Background(), repo, h.tick, true, false, h.now, ""); err != nil {
		h.t.Fatal(err)
	}
}

// add stores a release with resolved notes in state.
func (h *harness) add(repo, tag, state string, seen time.Time) *store.Release {
	h.t.Helper()
	ctx := context.Background()
	hash, _ := h.st.PutNotes(ctx, "notes for "+tag)
	r := &store.Release{Repo: repo, Tag: tag, GUID: ident.GUID(repo, tag), Kind: "release",
		HTMLURL: "https://github.com/" + repo + "/releases/tag/" + tag, Published: seen, FirstSeen: seen,
		State: state, Origin: store.OriginLive, NotesSource: "release_body", NotesHash: hash, NotesResolved: true}
	if state != store.StatePending {
		r.ContentUpdated = seen
	}
	if _, err := h.st.InsertRelease(ctx, r); err != nil {
		h.t.Fatal(err)
	}
	return r
}

func (h *harness) get(repo, tag string) *store.Release {
	h.t.Helper()
	r, err := h.st.Release(context.Background(), repo, tag)
	if err != nil || r == nil {
		h.t.Fatalf("%s@%s: %v, %v", repo, tag, r, err)
	}
	return r
}

func (h *harness) pass() {
	h.t.Helper()
	if err := h.s.Pass(context.Background(), h.tick); err != nil {
		h.t.Fatal(err)
	}
}

func TestPendingAndFallbackAreSummarized(t *testing.T) {
	h := newHarness(t)
	h.s.Cfg.LLM.Concurrency = 1 // for a deterministic request order
	pend := h.add("o/r", "v2", store.StatePending, t0.Add(-time.Hour))
	fb := h.add("o/r", "v1", store.StateFallback, t0.Add(-48*time.Hour))
	h.pass()
	for _, want := range []*store.Release{pend, fb} {
		got := h.get(want.Repo, want.Tag)
		if got.State != store.StateSummarized || got.SummaryKey == "" || !got.ContentUpdated.Equal(t0) {
			t.Errorf("%s: state %s, key %q, updated %v", want.Tag, got.State, got.SummaryKey, got.ContentUpdated)
		}
		if got.GUID != want.GUID || !got.Published.Equal(want.Published) {
			t.Errorf("%s: identity changed", want.Tag)
		}
		sum, _ := h.st.Summary(context.Background(), got.SummaryKey)
		if sum == nil || sum.Model != "test/model" || !strings.Contains(sum.JSON, "Adds streaming") || sum.TokensIn != 100 {
			t.Errorf("%s: stored summary %+v", want.Tag, sum)
		}
	}
	reqs := h.fake.Requests()
	if len(reqs) != 2 || !strings.Contains(reqs[0].Messages[1].Content, "Version: v1") {
		t.Errorf("requests %d; the oldest first_seen_at goes first", len(reqs))
	}
	if h.gated.Load() != 4 {
		t.Errorf("gate checked %d times, want before and after each call", h.gated.Load())
	}
}

func TestOnlyEligibleRowsAreSummarized(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.add("o/r", "unresolved", store.StatePending, t0)
	h.st.SetBody(ctx, "o/r", "unresolved", "edited", "release_body", "edited") // unresolves it
	none := h.add("o/r", "none", store.StatePending, t0)
	h.st.ResolveNotes(ctx, "o/r", none.Tag, "", "", t0)
	h.add("o/r", "nonotes", store.StateNoNotes, t0)
	h.add("o/r", "skipped", store.StateSkipped, t0)
	h.add("o/other", "unconfirmed", store.StatePending, t0)
	later := h.add("o/r", "later", store.StateFallback, t0)
	h.st.SummaryFailed(ctx, "o/r", later.Tag, "earlier failure", t0) // not due for 15m
	h.pass()
	if n := len(h.fake.Requests()); n != 0 {
		t.Errorf("%d LLM requests, want none", n)
	}
	if h.gated.Load() != 0 {
		t.Errorf("gate checked for ineligible rows")
	}
}

func TestGateFailureStopsTheRepo(t *testing.T) {
	h := newHarness(t)
	h.s.Cfg.LLM.Concurrency = 1
	h.add("o/r", "v1", store.StatePending, t0.Add(-2*time.Hour))
	h.add("o/r", "v2", store.StatePending, t0.Add(-time.Hour))
	h.deny.Store(true)
	h.pass()
	if n := len(h.fake.Requests()); n != 0 {
		t.Errorf("%d LLM requests after a failed gate", n)
	}
	if h.gated.Load() != 1 {
		t.Errorf("gate checked %d times; the failure should invalidate the repo's confirmation", h.gated.Load())
	}
	for _, tag := range []string{"v1", "v2"} {
		if r := h.get("o/r", tag); r.State != store.StatePending || r.Attempts != 0 {
			t.Errorf("%s: state %s, attempts %d", tag, r.State, r.Attempts)
		}
	}
}

func TestFailuresBackOff(t *testing.T) {
	h := newHarness(t)
	h.add("o/r", "v1", store.StateFallback, t0.Add(-time.Hour))
	h.fake.SetDefault(func(llmtest.Request) llmtest.Reply { return llmtest.Reply{Status: 401, Message: "no key"} })
	for i, wait := range []time.Duration{15 * time.Minute, 30 * time.Minute, time.Hour} {
		h.pass()
		r := h.get("o/r", "v1")
		if r.State != store.StateFallback || r.Attempts != i+1 || !strings.Contains(r.LastError, "no key") ||
			!r.NextAttempt.Equal(h.now.Add(wait)) {
			t.Fatalf("failure %d: state %s, attempts %d, error %q, next %v", i+1, r.State, r.Attempts, r.LastError, r.NextAttempt.Sub(h.now))
		}
		n := len(h.fake.Requests())
		h.now = h.now.Add(wait - time.Second)
		h.pass()
		if len(h.fake.Requests()) != n {
			t.Fatalf("failure %d: retried before the backoff ended", i+1)
		}
		h.now = h.now.Add(time.Second)
	}
	for range 10 {
		h.st.SummaryFailed(context.Background(), "o/r", "v1", "x", h.now)
	}
	if r := h.get("o/r", "v1"); !r.NextAttempt.Equal(h.now.Add(store.BackoffMax)) {
		t.Errorf("backoff not capped: %v", r.NextAttempt.Sub(h.now))
	}

	// The LLM recovers: the fallback is upgraded and the backoff reset.
	h.now = h.now.Add(store.BackoffMax)
	h.fake.SetDefault(func(llmtest.Request) llmtest.Reply { return llmtest.Reply{Content: good} })
	h.pass()
	if r := h.get("o/r", "v1"); r.State != store.StateSummarized || r.Attempts != 0 || r.LastError != "" {
		t.Errorf("after recovery: %s, attempts %d, error %q", r.State, r.Attempts, r.LastError)
	}
}

func TestRepairRetry(t *testing.T) {
	h := newHarness(t)
	h.add("o/r", "v1", store.StatePending, t0)
	h.fake.Queue(llmtest.Reply{Content: `{"headline":"","breaking":[],"features":[],"fixes":[],"security":[],"noise_only":true}`})
	h.pass()
	reqs := h.fake.Requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests, want a repair retry", len(reqs))
	}
	last := reqs[1].Messages
	if last[2].Role != "assistant" || !strings.Contains(last[3].Content, "headline is empty") {
		t.Errorf("repair messages = %+v", last[2:])
	}
	r := h.get("o/r", "v1")
	if r.State != store.StateSummarized {
		t.Fatalf("state %s", r.State)
	}
	// The summary is stored under the key of the original request.
	if r.SummaryKey != Key(h.s.endpoint(), reqs[0].Messages) {
		t.Error("summary key is not the original request's")
	}

	h.add("o/r", "v2", store.StatePending, t0)
	h.fake.Queue(llmtest.Reply{Content: "nope"}, llmtest.Reply{Content: "still nope"})
	h.pass()
	if r := h.get("o/r", "v2"); r.State != store.StatePending || r.Attempts != 1 || !strings.Contains(r.LastError, "repair") {
		t.Errorf("two invalid replies: %s, attempts %d, error %q", r.State, r.Attempts, r.LastError)
	}
}

func TestStoredSummaryIsReused(t *testing.T) {
	h := newHarness(t)
	r := h.add("o/r", "v1", store.StateFallback, t0)
	key := Key(h.s.endpoint(), Messages(DefaultPrompt, Input{Repo: "o/r", Tag: "v1", Source: "release_body", Notes: "notes for v1"}))
	h.st.PutSummary(context.Background(), &store.Summary{Key: key, Model: "test/model", JSON: good, Created: t0})
	h.pass()
	if n := len(h.fake.Requests()); n != 0 {
		t.Errorf("%d LLM requests for a stored summary", n)
	}
	if got := h.get(r.Repo, r.Tag); got.State != store.StateSummarized || got.SummaryKey != key {
		t.Errorf("state %s, key %q", got.State, got.SummaryKey)
	}
	if h.gated.Load() != 1 {
		t.Error("publishing a stored summary skipped the gate")
	}
}

func TestPromptFileChangesTheKey(t *testing.T) {
	h := newHarness(t)
	path := filepath.Join(t.TempDir(), "prompt.md")
	os.WriteFile(path, []byte("Custom prompt."), 0o644)
	h.s.Cfg.LLM.PromptFile = path
	h.add("o/r", "v1", store.StatePending, t0)
	h.pass()
	reqs := h.fake.Requests()
	if len(reqs) != 1 || reqs[0].Messages[0].Content != "Custom prompt." {
		t.Fatalf("requests = %+v", reqs)
	}
	def := Key(h.s.endpoint(), Messages(DefaultPrompt, Input{Repo: "o/r", Tag: "v1", Source: "release_body", Notes: "notes for v1"}))
	if h.get("o/r", "v1").SummaryKey == def {
		t.Error("the prompt file's contents are not in the key")
	}
}

func TestPassLimits(t *testing.T) {
	h := newHarness(t)
	h.s.Cfg.LLM.MaxPerPass = 7
	h.s.Cfg.LLM.Concurrency = 3
	for i := range 12 {
		h.add("o/r", "v"+string(rune('a'+i)), store.StatePending, t0.Add(time.Duration(i-12)*time.Minute))
	}
	var progress atomic.Int32
	h.s.Progress = func(context.Context) { progress.Add(1) }
	h.pass()
	if n := len(h.fake.Requests()); n != 7 {
		t.Errorf("%d requests, want max_per_pass", n)
	}
	if progress.Load() != 1 {
		t.Errorf("progress called %d times for 7 summaries, want 1", progress.Load())
	}
	if r := h.get("o/r", "va"); r.State != store.StateSummarized {
		t.Error("the oldest release was not summarized first")
	}
	if r := h.get("o/r", "vl"); r.State != store.StatePending {
		t.Error("the newest release was summarized past the limit")
	}

	// A used-up budget starts nothing more.
	h.s.Cfg.Schedule.SummarizeBudget.D = time.Minute
	var calls atomic.Int64
	h.s.Now = func() time.Time { return h.now.Add(time.Duration(calls.Add(1)) * 40 * time.Second) }
	h.s.Cfg.LLM.Concurrency = 1
	before := len(h.fake.Requests())
	h.pass()
	if n := len(h.fake.Requests()) - before; n == 0 || n >= 5 {
		t.Errorf("%d requests within a 1m budget at 40s per check", n)
	}
}

func TestDisabledWithoutAModel(t *testing.T) {
	h := newHarness(t)
	h.s.Cfg.LLM.Model = ""
	h.add("o/r", "v1", store.StatePending, t0)
	h.pass()
	if n := len(h.fake.Requests()); n != 0 {
		t.Errorf("%d requests with no model", n)
	}
}

func TestRepoGoingPrivateDuringTheCallIsNotPublished(t *testing.T) {
	h := newHarness(t)
	h.add("o/r", "v1", store.StatePending, t0)
	h.fake.SetDefault(func(llmtest.Request) llmtest.Reply {
		h.deny.Store(true) // the repo turns private while the call runs
		return llmtest.Reply{Content: good}
	})
	h.pass()
	r := h.get("o/r", "v1")
	if r.State != store.StatePending || r.Attempts != 0 {
		t.Fatalf("state %s, attempts %d; want unpublished and not a failure", r.State, r.Attempts)
	}
	// Public again next tick: the stored summary is published without a call.
	h.deny.Store(false)
	h.tick++
	h.confirm("o/r")
	h.pass()
	if r := h.get("o/r", "v1"); r.State != store.StateSummarized {
		t.Errorf("state %s after the repo is public again", r.State)
	}
	if n := len(h.fake.Requests()); n != 1 {
		t.Errorf("%d LLM requests, want the one", n)
	}
}
