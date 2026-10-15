package github

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"release-digest/internal/github/githubtest"
	"release-digest/internal/secret"
	"release-digest/internal/store"
)

const testToken = "ghp_testtokenvalue"

func newClient(t *testing.T, base string, token string) (*Client, *[]time.Duration) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "rd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tok := secret.NewValue(secret.Loaded{})
	if token != "" {
		tok = secret.NewValue(loadToken(t, token))
	}
	var slept []time.Duration
	c := &Client{
		BaseURL: base, HTTP: http.DefaultClient, Cache: st, Timeout: 5 * time.Second, UserAgent: "test",
		Token: func() secret.String { return tok.Get().Value },
		Sleep: func(_ context.Context, d time.Duration) bool { slept = append(slept, d); return true },
	}
	return c, &slept
}

// loadToken makes a secret holding token, as if from GITHUB_TOKEN.
func loadToken(t *testing.T, token string) secret.Loaded {
	t.Helper()
	env := secret.Env{
		LookupEnv: func(k string) (string, bool) { return token, k == "GITHUB_TOKEN" },
		Stat:      func(string) (fs.FileInfo, error) { return nil, fs.ErrNotExist },
	}
	l, err := env.Load(secret.GitHubToken)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestConditionalRequestsUseTheCache(t *testing.T) {
	gh := githubtest.New()
	defer gh.Close()
	gh.Repo("o/r").Releases = []githubtest.Release{{Tag: "v1", Body: "notes", Published: time.Now()}}
	c, _ := newClient(t, gh.URL, testToken)
	ctx := context.Background()
	for range 2 {
		rels, err := c.Releases(ctx, "o/r", 1)
		if err != nil || len(rels) != 1 || rels[0].Body != "notes" {
			t.Fatalf("Releases = %+v, %v", rels, err)
		}
	}
	if gh.NotMod != 1 {
		t.Errorf("304s = %d, want 1", gh.NotMod)
	}
	if gh.Auth[0] != "Bearer "+testToken {
		t.Errorf("Authorization = %q", gh.Auth[0])
	}
}

func TestCheckPublic(t *testing.T) {
	gh := githubtest.New()
	defer gh.Close()
	gh.Repo("o/public")
	gh.Repo("o/private").Private = true
	gh.Repo("o/broken").Status = http.StatusBadGateway
	c, _ := newClient(t, gh.URL, testToken)
	ctx := context.Background()
	for _, tc := range []struct {
		repo    string
		want    Visibility
		wantErr bool
	}{
		{"o/public", Visibility{Public: true}, false},
		{"o/private", Visibility{Gone: true}, false},
		{"o/missing", Visibility{Gone: true}, false},
		{"o/broken", Visibility{}, true},
	} {
		v, err := c.CheckPublic(ctx, tc.repo, 0)
		if v != tc.want || (err != nil) != tc.wantErr {
			t.Errorf("%s: %+v, %v", tc.repo, v, err)
		}
	}

	// No private field at all is not public.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"full_name":"o/r"}`)) }))
	defer srv.Close()
	c2, _ := newClient(t, srv.URL, "")
	if v, err := c2.CheckPublic(ctx, "o/r", 0); v.Public || err == nil {
		t.Errorf("no private field: %+v, %v", v, err)
	}
}

func TestRateLimit(t *testing.T) {
	gh := githubtest.New()
	defer gh.Close()
	gh.Repo("o/r")
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	// A far reset fails at once, and later calls don't send requests.
	gh.RateLimited, gh.Reset = true, now.Add(30*time.Minute)
	c, slept := newClient(t, gh.URL, testToken)
	c.Now = func() time.Time { return now }
	_, err := c.Releases(ctx, "o/r", 1)
	var rl *RateLimitError
	if !errors.As(err, &rl) || !rl.Reset.Equal(gh.Reset) {
		t.Fatalf("err = %v", err)
	}
	if _, err := c.TagRefs(ctx, "o/r"); !IsRateLimit(err) {
		t.Fatalf("second call: %v", err)
	}
	if n := len(gh.Requests); n != 1 || len(*slept) != 0 {
		t.Errorf("requests = %d, slept %v; want 1 and no waiting", n, *slept)
	}

	// A near reset is waited out, then the request is retried.
	gh.ResetLog()
	gh.Reset = now.Add(90 * time.Second)
	c, slept = newClient(t, gh.URL, testToken)
	c.Now = func() time.Time { return now }
	c.Sleep = func(_ context.Context, d time.Duration) bool {
		*slept = append(*slept, d)
		gh.Mu.Lock()
		gh.RateLimited = false
		gh.Mu.Unlock()
		now = now.Add(d)
		return true
	}
	if _, err := c.Releases(ctx, "o/r", 1); err != nil {
		t.Fatal(err)
	}
	if len(*slept) != 1 || (*slept)[0] != 90*time.Second || len(gh.Requests) != 2 {
		t.Errorf("slept %v, requests %v", *slept, gh.Requests)
	}
}

func TestGuard(t *testing.T) {
	gh := githubtest.New()
	defer gh.Close()
	c, _ := newClient(t, gh.URL, "")
	ctx := context.Background()
	tok := loadToken(t, testToken).Value

	if err := c.Guard(ctx, secret.String{}); err != nil || len(gh.Requests) != 0 {
		t.Errorf("no token: %v, %d requests", err, len(gh.Requests))
	}
	if err := c.Guard(ctx, tok); err != nil {
		t.Errorf("clean token: %v", err)
	}
	var ge *GuardError
	gh.Scopes = "read:org, repo"
	if err := c.Guard(ctx, tok); !errors.As(err, &ge) || !strings.Contains(err.Error(), "repo scope") {
		t.Errorf("repo scope: %v", err)
	}
	gh.Scopes = "public_repo"
	if err := c.Guard(ctx, tok); err != nil {
		t.Errorf("public_repo scope: %v", err)
	}
	gh.PrivateRepos = 1
	if err := c.Guard(ctx, tok); !errors.As(err, &ge) {
		t.Errorf("lists private repos: %v", err)
	}
	if strings.Contains(strings.Join(gh.Auth, ""), testToken) == false {
		t.Error("guard did not send the token under test")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()
	c.BaseURL = srv.URL
	if err := c.Guard(ctx, tok); !errors.As(err, &ge) {
		t.Errorf("401: %v", err)
	} else if strings.Contains(err.Error(), testToken) {
		t.Errorf("error leaks the token: %v", err)
	}
	srv5 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv5.Close()
	c.BaseURL = srv5.URL
	if err := c.Guard(ctx, tok); err == nil || errors.As(err, &ge) {
		t.Errorf("5xx should be an inconclusive error, got %v", err)
	}
}

func TestErrorsNeverIncludeTheToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
	}))
	defer srv.Close()
	c, _ := newClient(t, srv.URL, testToken)
	_, err := c.Releases(context.Background(), "o/r", 1)
	if err == nil || strings.Contains(err.Error(), testToken) || !strings.Contains(err.Error(), "500") {
		t.Errorf("err = %v", err)
	}
}

func TestExists(t *testing.T) {
	gh := githubtest.New()
	defer gh.Close()
	r := gh.Repo("o/r")
	r.Releases = []githubtest.Release{{Tag: "service/api/v1.0.0", Published: time.Now()}}
	r.Tags = []githubtest.Tag{{Name: "v2+build", SHA: "abc"}}
	c, _ := newClient(t, gh.URL, "")
	ctx := context.Background()
	for _, tc := range []struct {
		kind, tag string
		want      bool
	}{
		{"release", "service/api/v1.0.0", true},
		{"release", "v9", false},
		{"tag", "v2+build", true},
		{"tag", "gone", false},
	} {
		if ok, err := c.Exists(ctx, "o/r", tc.kind, tc.tag); ok != tc.want || err != nil {
			t.Errorf("%s %s: %v, %v", tc.kind, tc.tag, ok, err)
		}
	}
	if !strings.Contains(strings.Join(gh.Requests, " "), "/releases/tags/service/api/v1.0.0") {
		t.Errorf("slashes in the tag were escaped: %v", gh.Requests)
	}
}

func TestTagRefsAndDates(t *testing.T) {
	gh := githubtest.New()
	defer gh.Close()
	d1, d2 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	gh.Repo("o/r").Tags = []githubtest.Tag{
		{Name: "v1.0.0", SHA: "c1", Date: d1},
		{Name: "api/v2.0.0", SHA: "c2", Date: d1, Annotated: true, TaggerDate: d2},
	}
	c, _ := newClient(t, gh.URL, "")
	ctx := context.Background()
	refs, err := c.TagRefs(ctx, "o/r")
	if err != nil || len(refs) != 2 || refs[1] != (TagRef{Name: "api/v2.0.0", SHA: "tc2", Type: "tag"}) {
		t.Fatalf("TagRefs = %+v, %v", refs, err)
	}
	if d, err := c.TagDate(ctx, "o/r", refs[0].SHA, refs[0].Type); err != nil || !d.Equal(d1) {
		t.Errorf("lightweight tag date = %v, %v; want the commit date", d, err)
	}
	if d, err := c.TagDate(ctx, "o/r", refs[1].SHA, refs[1].Type); err != nil || !d.Equal(d2) {
		t.Errorf("annotated tag date = %v, %v; want the tagger date", d, err)
	}
	if refs, err := c.TagRefs(ctx, "o/empty"); err == nil || len(refs) != 0 {
		t.Errorf("missing repo: %v, %v", refs, err)
	}
}

func TestSecondaryRateLimitsWithoutHeaders(t *testing.T) {
	for _, tc := range []struct {
		name    string
		code    int
		body    string
		limited bool
	}{
		{"429", http.StatusTooManyRequests, `{"message":"slow down"}`, true},
		{"403 secondary", http.StatusForbidden, `{"message":"You have exceeded a secondary rate limit."}`, true},
		{"403 forbidden", http.StatusForbidden, `{"message":"Resource not accessible"}`, false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.code)
			w.Write([]byte(tc.body))
		}))
		c, _ := newClient(t, srv.URL, "")
		c.Sleep = func(context.Context, time.Duration) bool { return true }
		_, err := c.Releases(context.Background(), "o/r", 1)
		if IsRateLimit(err) != tc.limited {
			t.Errorf("%s: err = %v, rate limit = %v", tc.name, err, tc.limited)
		}
		srv.Close()
	}
}
