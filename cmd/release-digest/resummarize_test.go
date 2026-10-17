package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"release-digest/internal/github/githubtest"
	"release-digest/internal/llm/llmtest"
	"release-digest/internal/store"
)

func TestResummarizeThenBuild(t *testing.T) {
	cleanEnv(t)
	gh := githubtest.New()
	defer gh.Close()
	now := time.Now()
	gh.Repo("o/r").Releases = []githubtest.Release{
		{Tag: "v1.1.0", Body: "## Fixes\n- Faster startup", Published: now.Add(-time.Hour)},
		{Tag: "v1.0.0", Body: "## Features\n- First release", Published: now.Add(-48 * time.Hour)},
	}
	reply := func(headline string) string {
		return `{"headline":"` + headline + `","breaking":[],"features":[],"fixes":["Faster startup"],"security":[],"noise_only":false,"security_urgency":"none","security_reason":""}`
	}
	ai := llmtest.New(reply("Starts faster"))
	defer ai.Close()
	cfg := writeConfig(t, "github: {api_url: "+gh.URL+"}\nllm: {base_url: "+ai.URL()+", model: test/model}\nrepos: [{repo: o/r}]\n")

	var out, errb bytes.Buffer
	if code := run([]string{"build", "--config", cfg}, &out, &errb); code != 0 {
		t.Fatalf("build: exit %d\n%s", code, errb.String())
	}
	out.Reset()
	if code := run([]string{"resummarize", "--config", cfg, "--repo", "O/R", "--since", "1d"}, &out, &errb); code != 0 {
		t.Fatalf("resummarize: exit %d\n%s", code, errb.String())
	}
	if s := out.String(); !strings.Contains(s, "o/r@v1.1.0 (summarized)") || strings.Contains(s, "v1.0.0") ||
		!strings.Contains(s, "1 release(s) queued") {
		t.Errorf("resummarize output:\n%s", s)
	}

	// The same request again, so only the flag forces the call.
	ai.SetDefault(func(llmtest.Request) llmtest.Reply { return llmtest.Reply{Content: reply("Starts much faster")} })
	if code := run([]string{"build", "--config", cfg}, &out, &errb); code != 0 {
		t.Fatalf("second build: exit %d\n%s", code, errb.String())
	}
	if n := len(ai.Requests()); n != 3 {
		t.Errorf("%d LLM requests, want 3: two summaries, then one resummary", n)
	}
	st, err := store.Open(context.Background(), filepath.Join(filepath.Dir(cfg), "rd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rss, _ := st.Rendered(context.Background(), "feed.xml")
	if rss == nil || !strings.Contains(string(rss.Bytes), "<title>o/r v1.1.0</title>\n      <link>https://github.com/o/r/releases/tag/v1.1.0</link>\n      <description>Starts much faster") ||
		!strings.Contains(string(rss.Bytes), "<description>Starts faster") {
		t.Errorf("feed after resummarize:\n%s", rss.Bytes)
	}
}

func TestResummarizeRejects(t *testing.T) {
	cleanEnv(t)
	withModel := writeConfig(t, "llm: {model: m}\nrepos: [{repo: o/r}]\n")
	noModel := writeConfig(t, "repos: [{repo: o/r}]\n")
	for _, c := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"--config", withModel, "--repo", "o/other"}, 2, "not a configured repo"},
		{[]string{"--config", withModel, "--since", "soon"}, 2, "must be a positive duration"},
		{[]string{"--config", withModel, "--since", "0d"}, 2, "must be a positive duration"},
		{[]string{"--config", withModel, "o/r"}, 2, "Usage"},
		{[]string{"--config", noModel}, 1, "llm.model is not set"},
	} {
		var out, errb bytes.Buffer
		if code := run(append([]string{"resummarize"}, c.args...), &out, &errb); code != c.code || !strings.Contains(errb.String(), c.want) {
			t.Errorf("%v: exit %d, stderr %q; want %d and %q", c.args, code, errb.String(), c.code, c.want)
		}
	}
}
