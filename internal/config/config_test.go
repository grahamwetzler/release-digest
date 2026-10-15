package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "release-digest.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func envOf(m map[string]string) LookupEnv {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

const minimal = "version: 1\npublic_base_url: https://rss.example.com/\n"

func TestDefaults(t *testing.T) {
	path := write(t, minimal)
	c, err := Load(path, envOf(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicBaseURL != "https://rss.example.com" {
		t.Errorf("trailing slash kept: %q", c.PublicBaseURL)
	}
	if c.StorePath != filepath.Join(filepath.Dir(path), DefaultStorePath) {
		t.Errorf("store_path not resolved against the config dir: %s", c.StorePath)
	}
	if c.Schedule.Interval.D != time.Hour || c.Schedule.Jitter.D != 5*time.Minute || c.Notify.MaxAge.D != 7*24*time.Hour {
		t.Errorf("schedule defaults: %+v, max_age %v", c.Schedule, c.Notify.MaxAge.D)
	}
	if c.LLM.BaseURL != DefaultLLMBaseURL || *c.LLM.Temperature != 0.2 || c.Feed.MaxItems != 200 || c.GitHub.VerifyPerTick != 10 {
		t.Errorf("defaults not applied: %+v %+v", c.LLM, c.Feed)
	}
	if *c.Defaults.Backfill != 3 || *c.Defaults.Changelog != "auto" || len(c.Defaults.Sources) != 2 {
		t.Errorf("repo defaults: %+v", c.Defaults)
	}
}

func TestReposInheritDefaults(t *testing.T) {
	c, err := Load(write(t, minimal+`
defaults:
  backfill: 1
  tag_pattern: '^v'
repos:
  - repo: golang/go
    sources: [tags]
    tag_pattern: '^go1\.\d+(\.\d+)?$'
  - repo: astral-sh/uv
    backfill: 0
    changelog: CHANGELOG.md
    packages:
      - { ecosystem: pip, name: uv }
`), envOf(nil))
	if err != nil {
		t.Fatal(err)
	}
	gogo, uv := c.Repos[0], c.Repos[1]
	if gogo.Sources[0] != "tags" || !gogo.TagPattern.Re.MatchString("go1.23.1") || *gogo.Backfill != 1 {
		t.Errorf("golang/go: %+v", gogo.RepoOptions)
	}
	if *uv.Backfill != 0 || *uv.Changelog != "CHANGELOG.md" || uv.TagPattern.Raw != "^v" || len(uv.Packages) != 1 {
		t.Errorf("astral-sh/uv: %+v", uv.RepoOptions)
	}
	if len(gogo.Packages) != 0 {
		t.Error("packages inherited")
	}
}

func TestEnvRepoMerge(t *testing.T) {
	path := write(t, minimal+"repos:\n  - repo: Astral-SH/uv\n    backfill: 9\n")
	c, err := Load(path, envOf(map[string]string{
		EnvRepos: "anthropics/anthropic-sdk-go, astral-sh/uv\n# a comment\nhashicorp/terraform+pre\n\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range c.Repos {
		names = append(names, r.Name())
	}
	if got := strings.Join(names, " "); got != "astral-sh/uv anthropics/anthropic-sdk-go hashicorp/terraform" {
		t.Fatalf("repos = %s", got)
	}
	if *c.Repos[0].Backfill != 9 || c.Repos[0].FromEnv {
		t.Error("the YAML entry did not keep its options")
	}
	if !*c.Repos[2].IncludePrereleases || *c.Repos[1].IncludePrereleases {
		t.Error("+pre not applied to exactly the flagged repo")
	}
	if *c.Repos[1].Backfill != DefaultBackfill {
		t.Error("env repo did not get defaults")
	}
}

func TestEnvOverrides(t *testing.T) {
	c, err := Load(write(t, "version: 1\n"), envOf(map[string]string{
		EnvPublicBaseURL: "https://rss.example.org", EnvScheduleInterval: "30m",
		EnvLLMBaseURL: "http://ollama:11434/v1", EnvLLMModel: "qwen3:14b",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicBaseURL != "https://rss.example.org" || c.Schedule.Interval.D != 30*time.Minute ||
		c.LLM.BaseURL != "http://ollama:11434/v1" || c.LLM.Model != "qwen3:14b" {
		t.Errorf("overrides not applied: %s %v %s %s", c.PublicBaseURL, c.Schedule.Interval.D, c.LLM.BaseURL, c.LLM.Model)
	}
}

func TestSecretsRejected(t *testing.T) {
	_, err := Load(write(t, minimal+"llm:\n  api_key: sk-or-v1-x\ngithub:\n  token: ghp_x\n"), envOf(nil))
	if err == nil {
		t.Fatal("secret keys accepted")
	}
	msg := err.Error()
	for _, want := range []string{":4: llm.api_key: secrets are never read", ":6: github.token: secrets are never read", "LLM_API_KEY_FILE"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "unknown key") {
		t.Errorf("secret key also reported as unknown:\n%s", msg)
	}
	if strings.Contains(msg, "sk-or-v1-x") || strings.Contains(msg, "ghp_x") {
		t.Errorf("secret value echoed:\n%s", msg)
	}
}

func TestValidationErrors(t *testing.T) {
	_, err := Load(write(t, `version: 2
listen: "8080"
schedule:
  interval: 1h
  jitter: 2h
llm:
  temperature: 3
  headers:
    Authorization: Bearer x
notify:
  min_severity: low
  ntfy:
    url: ntfy.sh/topic
defaults:
  sources: [releases, commits]
repos:
  - repo: not-a-repo
  - repo: o/r
    tag_pattern: '('
    changelog: ../etc/passwd
    packages:
      - { ecosystem: cargo, name: "" }
  - repo: O/R
  - bogus: 1
`), envOf(map[string]string{EnvRepos: "x, a/b+beta"}))
	if err == nil {
		t.Fatal("no error")
	}
	msg := err.Error()
	for _, want := range []string{
		":1: version: must be 1",
		"public_base_url: is required",
		":2: listen: must be host:port",
		":5: schedule.jitter: must be less than schedule.interval",
		":7: llm.temperature: must be between 0 and 2",
		":9: llm.headers.Authorization: carries a credential",
		":11: notify.min_severity",
		":13: notify.ntfy.url: must be an absolute http(s) URL",
		`:15: defaults.sources[1]: must be releases or tags, got "commits"`,
		`:17: repos[0].repo: must be owner/repo`,
		":19: repos[1].tag_pattern: invalid regex",
		":20: repos[1].changelog: must be auto, off or a path inside the repo",
		":22: repos[1].packages[0].ecosystem",
		":22: repos[1].packages[0].name: is required",
		`:23: repos[2].repo: duplicate repo "O/R" (also repos[1])`,
		`unknown key "bogus"`,
		`env RELEASE_DIGEST_REPOS: "x" is not owner/repo`,
		`env RELEASE_DIGEST_REPOS: "a/b+beta": unknown flag +beta`,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in:\n%s", want, msg)
		}
	}
}

func TestNullRepoEntry(t *testing.T) {
	_, err := Load(write(t, minimal+"repos:\n  - repo: o/r\n  -\n"), envOf(map[string]string{EnvRepos: "a/b"}))
	if err == nil || !strings.Contains(err.Error(), ":5: repos[1]: must be a mapping with a repo key") {
		t.Fatalf("err = %v", err)
	}
	if strings.Count(err.Error(), "\n") != 0 {
		t.Errorf("more than one error reported:\n%v", err)
	}
	_, err = Load(write(t, minimal+"repos: [null]\n"), envOf(nil))
	if err == nil || !strings.Contains(err.Error(), "repos[0]: must be a mapping") {
		t.Errorf("flow-style null: %v", err)
	}
}

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{"7d": 7 * 24 * time.Hour, "90s": 90 * time.Second, "1h30m": 90 * time.Minute} {
		if got, err := ParseDuration(in); err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v, %v", in, got, err)
		}
	}
	for _, in := range []string{"1.5d", "d", "7days", "soon"} {
		if _, err := ParseDuration(in); err == nil {
			t.Errorf("ParseDuration(%q) accepted", in)
		}
	}
}
