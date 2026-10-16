package main

import (
	"bytes"
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"release-digest/internal/feed"
	"release-digest/internal/github/githubtest"
	"release-digest/internal/store"
)

func TestBuildFailsAndSkipsLastTickOnRenderError(t *testing.T) {
	cleanEnv(t)
	cfg := writeConfig(t, "")
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(filepath.Dir(cfg), "rd.db"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	// A relative link fails the feed checks, so the render must fail.
	st.InsertRelease(ctx, &store.Release{Repo: "o/r", Tag: "v1", GUID: "g", Kind: "release", HTMLURL: "/relative",
		Published: now, FirstSeen: now, State: store.StateFallback, Origin: "live"})
	st.Close()

	var out, errb bytes.Buffer
	if code := run([]string{"build", "--config", cfg}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "failed its checks") {
		t.Fatalf("build: exit %d\nstdout: %s\nstderr: %s", code, out.String(), errb.String())
	}
	if strings.Contains(out.String(), "built") {
		t.Errorf("reported success: %s", out.String())
	}
	st, _ = store.Open(ctx, filepath.Join(filepath.Dir(cfg), "rd.db"))
	defer st.Close()
	if last, err := st.StateTime(ctx, stateLastTick); err != nil || !last.IsZero() {
		t.Errorf("last_tick = %v, %v; want unset after a failed tick", last, err)
	}
	if r, _ := st.Rendered(ctx, "feed.xml"); r != nil {
		t.Error("a failed render was stored")
	}
}

func TestValidateHonorsW3CVerdict(t *testing.T) {
	cleanEnv(t)
	cfg := writeConfig(t, "")
	var out, errb bytes.Buffer
	if code := run([]string{"build", "--config", cfg}, &out, &errb); code != 0 {
		t.Fatalf("build: %d %s", code, errb.String())
	}
	for _, tc := range []struct {
		res  feed.W3CResult
		want int
	}{
		{feed.W3CResult{Valid: true}, 0},
		{feed.W3CResult{Valid: false}, 1}, // invalid with no messages at all
		{feed.W3CResult{Valid: true, Warnings: []feed.W3CMessage{{Type: "Unexpected"}}}, 1},
		{feed.W3CResult{Valid: true, Warnings: []feed.W3CMessage{{Type: "SelfDoesntMatchLocation"}}}, 0},
	} {
		validateW3C = func(context.Context, *http.Client, string, []byte) (*feed.W3CResult, error) {
			r := tc.res
			return &r, nil
		}
		out.Reset()
		errb.Reset()
		if code := run([]string{"validate", "--w3c", "--config", cfg}, &out, &errb); code != tc.want {
			t.Errorf("%+v: exit %d, want %d\n%s%s", tc.res, code, tc.want, out.String(), errb.String())
		}
	}
	validateW3C = feed.ValidateW3C
}

func TestBuildPollsGitHubAndPublishesFallbacks(t *testing.T) {
	cleanEnv(t)
	gh := githubtest.New()
	defer gh.Close()
	now := time.Now()
	gh.Repo("o/r").Releases = []githubtest.Release{
		{Tag: "v1.1.0", Body: "## Fixes\n- **Faster** startup", Published: now.Add(-time.Hour)},
		{Tag: "v1.0.0", Body: "First release", Published: now.Add(-48 * time.Hour)},
	}
	cfg := writeConfig(t, "github: {api_url: "+gh.URL+"}\nllm: {publish_after: 1ms}\nrepos: [{repo: o/r}]\n")
	t.Setenv("GITHUB_TOKEN", "ghp_buildtoken")

	var out, errb bytes.Buffer
	for i := 0; i < 2; i++ { // the first build discovers; the second publishes what's due
		time.Sleep(5 * time.Millisecond)
		out.Reset()
		if code := run([]string{"build", "--config", cfg}, &out, &errb); code != 0 {
			t.Fatalf("build %d: exit %d\n%s", i, code, errb.String())
		}
	}
	if !strings.Contains(out.String(), "built: 2 item(s)") {
		t.Errorf("stdout: %s\nstderr: %s", out.String(), errb.String())
	}
	st, err := store.Open(context.Background(), filepath.Join(filepath.Dir(cfg), "rd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rss, _ := st.Rendered(context.Background(), "feed.xml")
	if rss == nil || !strings.Contains(string(rss.Bytes), "Fixes • Faster startup") {
		t.Errorf("feed lacks the notes excerpt:\n%s", rss.Bytes)
	}
	if !strings.Contains(errb.String(), "checked for obvious private-repo access only") {
		t.Errorf("guard result not logged:\n%s", errb.String())
	}
	if strings.Contains(errb.String(), "ghp_buildtoken") {
		t.Error("token logged")
	}
}

func TestBuildRejectsAPrivateCapableToken(t *testing.T) {
	cleanEnv(t)
	gh := githubtest.New()
	defer gh.Close()
	gh.Scopes = "repo"
	gh.Repo("o/r")
	cfg := writeConfig(t, "github: {api_url: "+gh.URL+"}\nrepos: [{repo: o/r}]\n")
	t.Setenv("GITHUB_TOKEN", "ghp_classic")
	var out, errb bytes.Buffer
	if code := run([]string{"build", "--config", cfg}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "repo scope") {
		t.Fatalf("exit %d\n%s", code, errb.String())
	}
	if gh.Count("/repos/") != 0 {
		t.Errorf("repos were requested with a rejected token: %v", gh.Requests)
	}
}
