package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"release-digest/internal/github/githubtest"
	"release-digest/internal/ident"
	"release-digest/internal/llm/llmtest"
	"release-digest/internal/notify"
	"release-digest/internal/store"
)

const testGHSA = "GHSA-2345-6789-cfgh"

func TestNotifyReleaseCrosscheck(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			cleanEnv(t)
			gh := githubtest.New()
			defer gh.Close()
			nt := newNtfyServer(t)
			gh.Repo("o/r").Releases = []githubtest.Release{
				{Tag: "v1.1.0", Body: "Security: Fix RCE and path traversal", Published: time.Now().Add(-time.Hour)},
				{Tag: "v1.0.0", Body: "First release", Published: time.Now().Add(-48 * time.Hour)},
			}
			ai := llmtest.New(`{"headline":"Parser changes","breaking":[],"features":[],"fixes":["Parser changes"],"security":[],"noise_only":false,"security_urgency":"none","security_reason":""}`)
			defer ai.Close()
			cfg := writeConfig(t, fmt.Sprintf("github: {api_url: %s}\nllm: {base_url: %s, model: test/model}\nnotify: {crosscheck: %v}\nrepos: [{repo: o/r}]\n", gh.URL, ai.URL(), enabled))
			t.Setenv("NTFY_URL", nt.URL+"/alerts")
			var out, errb bytes.Buffer
			if code := run([]string{"notify", "test", "--config", cfg, "--release", "o/r@v1.1.0"}, &out, &errb); code != 0 {
				t.Fatalf("notify test --release: exit %d\n%s", code, errb.String())
			}
			want := "o/r@v1.1.0 (previous: v1.0.0), notes: release_body\n" +
				"advisory signal: no GHSA or CVE IDs in the notes\n" +
				"LLM signal: none (none)\ndecision: none\n"
			if enabled {
				row := &store.Release{Repo: "o/r", Tag: "v1.1.0", HTMLURL: "https://github.com/o/r/releases/tag/v1.1.0"}
				payload := notify.UnconfirmedPayload(row, []string{"remote code execution", "path traversal"})
				b, err := json.MarshalIndent(payload, "", "  ")
				if err != nil {
					t.Fatal(err)
				}
				want += "unconfirmed security signals: remote code execution, path traversal\n" +
					"crosscheck payload (not sent):\n" + string(b) + "\n"
			}
			if out.String() != want {
				t.Errorf("output =\n%s\nwant:\n%s", out.String(), want)
			}
			if nt.count() != 0 {
				t.Fatal("dry run sent an alert")
			}
		})
	}
}

// ntfyServer records the alerts it receives.
type ntfyServer struct {
	*httptest.Server
	mu    sync.Mutex
	paths []string
	auth  []string
}

func newNtfyServer(t *testing.T) *ntfyServer {
	s := &ntfyServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		s.mu.Lock()
		s.paths = append(s.paths, r.URL.Path)
		s.auth = append(s.auth, r.Header.Get("Authorization"))
		s.mu.Unlock()
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *ntfyServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.paths)
}

func TestBuildMarksAndQueuesAnUrgentRelease(t *testing.T) {
	cleanEnv(t)
	gh := githubtest.New()
	defer gh.Close()
	nt := newNtfyServer(t)
	gh.Advisories = map[string]string{testGHSA: `{"ghsa_id":"` + testGHSA + `","severity":"critical","summary":"RCE in the parser",
		"source_code_location":"https://github.com/o/r",
		"vulnerabilities":[{"package":{"ecosystem":"go","name":"github.com/o/r"},"vulnerable_version_range":"< 1.2.3","first_patched_version":"1.2.3"}]}`}
	gh.Repo("o/r").Releases = []githubtest.Release{{Tag: "v1.2.2", Body: "First", Published: time.Now().Add(-time.Hour)}}
	cfg := writeConfig(t, "github: {api_url: "+gh.URL+"}\nllm: {publish_after: 1ms}\nrepos: [{repo: o/r}]\n")
	t.Setenv("NTFY_URL", nt.URL+"/alerts")
	t.Setenv("NTFY_TOKEN", "tk_buildtoken")

	var out, errb bytes.Buffer
	if code := run([]string{"build", "--config", cfg}, &out, &errb); code != 0 {
		t.Fatalf("first build: exit %d\n%s", code, errb.String())
	}
	// Released after tracking began: alert-eligible.
	gh.Mu.Lock()
	gh.Repos["o/r"].Releases = append([]githubtest.Release{{Tag: "v1.2.3", Body: "## Security\n- Fix " + testGHSA + " (#12)",
		Published: time.Now().Add(time.Second)}}, gh.Repos["o/r"].Releases...) // the API has second precision
	gh.Mu.Unlock()
	if code := run([]string{"build", "--config", cfg}, &out, &errb); code != 0 {
		t.Fatalf("second build: exit %d\n%s", code, errb.String())
	}

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(filepath.Dir(cfg), "rd.db"))
	if err != nil {
		t.Fatal(err)
	}
	rss, _ := st.Rendered(ctx, "feed.xml")
	if rss == nil || !strings.Contains(string(rss.Bytes), "🚨 o/r v1.2.3") || !strings.Contains(string(rss.Bytes), "<category>security-urgent</category>") {
		t.Errorf("feed not marked:\n%s", rss.Bytes)
	}
	n, err := st.Notification(ctx, ident.GUID("o/r", "v1.2.3"))
	st.Close()
	if err != nil || n == nil || n.State != store.AlertQueued || n.Reason != "advisory:"+testGHSA {
		t.Fatalf("alert = %+v, %v", n, err)
	}
	if nt.count() != 0 {
		t.Error("build sent an alert; only run delivers")
	}
	if strings.Contains(errb.String(), "tk_buildtoken") || strings.Contains(errb.String(), "/alerts") {
		t.Errorf("ntfy secret logged:\n%s", errb.String())
	}

	// The dry run decides the same, and sends nothing.
	out.Reset()
	if code := run([]string{"notify", "test", "--config", cfg, "--release", "o/r@v1.2.3"}, &out, &errb); code != 0 {
		t.Fatalf("notify test --release: exit %d\n%s", code, errb.String())
	}
	for _, want := range []string{"advisory " + testGHSA + ": fires (severity critical, version evidence)",
		"decision: urgent (advisory:" + testGHSA + ")", `"priority": 5`, "(not sent)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("notify test --release lacks %q:\n%s", want, out.String())
		}
	}
	if nt.count() != 0 {
		t.Error("the dry run sent an alert")
	}

	out.Reset()
	if code := run([]string{"notify", "test", "--config", cfg}, &out, &errb); code != 0 {
		t.Fatalf("notify test: exit %d\n%s", code, errb.String())
	}
	if nt.count() != 1 || nt.paths[0] != "/alerts" || nt.auth[0] != "Bearer tk_buildtoken" {
		t.Errorf("test alert: %v %v", nt.paths, nt.auth)
	}
}

func TestNtfyURLInBothPlacesIsAnError(t *testing.T) {
	cleanEnv(t)
	cfg := writeConfig(t, "notify: {ntfy: {url: https://ntfy.example.com/topic}}\n")
	t.Setenv("NTFY_URL", "https://ntfy.example.com/other")
	var out, errb bytes.Buffer
	if code := run([]string{"build", "--config", cfg}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "both set") {
		t.Errorf("exit %d\n%s", code, errb.String())
	}
	cleanEnv(t)
	t.Setenv("NTFY_URL", "not a url")
	cfg = writeConfig(t, "")
	errb.Reset()
	if code := run([]string{"build", "--config", cfg}, &out, &errb); code != 1 || strings.Contains(errb.String(), "not a url") {
		t.Errorf("exit %d\n%s", code, errb.String())
	}
}
