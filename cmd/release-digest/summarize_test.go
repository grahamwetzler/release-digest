package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"release-digest/internal/github/githubtest"
	"release-digest/internal/llm/llmtest"
)

func TestSummarizeDryRun(t *testing.T) {
	cleanEnv(t)
	gh := githubtest.New()
	defer gh.Close()
	now := time.Now()
	gh.Repo("o/r").Releases = []githubtest.Release{
		{Tag: "v1.1.0", Body: "## Fixes\n- **Faster** startup on large workspaces", Published: now.Add(-time.Hour)},
		{Tag: "v1.0.0", Body: "First release", Published: now.Add(-48 * time.Hour)},
	}
	gh.Repo("o/secret").Private = true
	ai := llmtest.New(`{"headline":"Starts faster","breaking":[],"features":[],"fixes":["Faster startup"],"security":[],"noise_only":false,"security_urgency":"none","security_reason":""}`)
	defer ai.Close()
	cfg := writeConfig(t, "github: {api_url: "+gh.URL+"}\nllm: {base_url: "+ai.URL()+", model: test/model}\n")
	t.Setenv("GITHUB_TOKEN", "ghp_x")

	var out, errb bytes.Buffer
	if code := run([]string{"summarize", "--config", cfg, "--messages", "O/R@v1.1.0"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d\n%s", code, errb.String())
	}
	for _, want := range []string{
		"o/r@v1.1.0 (previous: v1.0.0)",
		"notes: release_body",
		"--- system ---",
		"Previous version: v1.0.0",
		`"headline": "Starts faster"`,
		"title: o/r v1.1.0 — Starts faster",
		"Summarized by test/model from the release notes.",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(cfg), "rd.db")); !os.IsNotExist(err) {
		t.Errorf("the dry run created the store: %v", err)
	}

	out.Reset()
	if code := run([]string{"summarize", "--config", cfg, "o/secret@v1"}, &out, &errb); code != 1 ||
		!strings.Contains(errb.String(), "only public repos") {
		t.Errorf("private repo: exit %d\n%s", code, errb.String())
	}
	if n := len(ai.Requests()); n != 1 {
		t.Errorf("%d LLM requests, want 1", n)
	}
	for _, bad := range [][]string{{"summarize", "--config", cfg}, {"summarize", "--config", cfg, "o/r"}, {"summarize", "--config", cfg, "r@v1"}} {
		if code := run(bad, &out, &errb); code != 2 {
			t.Errorf("%v: exit %d, want 2", bad, code)
		}
	}
}

func TestSummarizeDryRunDatesADateOrderedTag(t *testing.T) {
	cleanEnv(t)
	gh := githubtest.New()
	defer gh.Close()
	now := time.Now()
	repo := gh.Repo("o/nightly")
	repo.Tags = []githubtest.Tag{
		{Name: "nightly-b", SHA: "c2", Date: now.Add(-time.Hour)},
		{Name: "nightly-a", SHA: "c1", Date: now.Add(-48 * time.Hour)},
	}
	repo.Compares = map[string]githubtest.Comparison{"nightly-a...nightly-b": {Status: "ahead",
		Commits: []githubtest.Commit{{Message: "Add the streaming parser", Login: "dev", Type: "User", Parents: 1}}}}
	ai := llmtest.New(`{"headline":"Streaming parser","breaking":[],"features":["Streaming parser"],"fixes":[],"security":[],"noise_only":false,"security_urgency":"none","security_reason":""}`)
	defer ai.Close()
	cfg := writeConfig(t, "github: {api_url: "+gh.URL+"}\nllm: {base_url: "+ai.URL()+", model: m}\n")

	var out, errb bytes.Buffer
	if code := run([]string{"summarize", "--config", cfg, "o/nightly@nightly-b"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d\n%s", code, errb.String())
	}
	for _, want := range []string{"(previous: nightly-a)", "notes: compare", "Diff since nightly-a"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestSummarizeDryRunReportsAnUndeterminedPredecessor(t *testing.T) {
	cleanEnv(t)
	gh := githubtest.New()
	defer gh.Close()
	now := time.Now()
	repo := gh.Repo("o/nightly")
	for i := range MaxDateLookups + 2 {
		repo.Tags = append(repo.Tags, githubtest.Tag{Name: fmt.Sprintf("nightly-%c%c", 'a'+i/26, 'a'+i%26), SHA: fmt.Sprintf("c%d", i),
			Date: now.Add(-time.Duration(i) * time.Hour)})
	}
	ai := llmtest.New(`{"headline":"x","breaking":[],"features":[],"fixes":[],"security":[],"noise_only":true,"security_urgency":"none","security_reason":""}`)
	defer ai.Close()
	cfg := writeConfig(t, "github: {api_url: "+gh.URL+"}\nllm: {base_url: "+ai.URL()+", model: m}\n")
	var out, errb bytes.Buffer
	if code := run([]string{"summarize", "--config", cfg, "o/nightly@nightly-aa"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d\n%s", code, errb.String())
	}
	if !strings.Contains(out.String(), "(previous: none)") || !strings.Contains(errb.String(), "was not determined") {
		t.Errorf("stdout:\n%s\nstderr:\n%s", out.String(), errb.String())
	}
}
