package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"release-digest/internal/config"
	"release-digest/internal/github"
	"release-digest/internal/llm"
	"release-digest/internal/secret"
)

// cmdConfigLint validates the merged config and the secrets' sources. It
// reports where each secret comes from, never its value (§4.3). Unless
// --offline, it also checks that every repo is public and reachable with
// the token, and that the LLM answers a tiny fixed prompt (§4.3).
func cmdConfigLint(args []string, stdout, stderr io.Writer) int {
	fs, cfgPath := newFlags("config lint", stderr)
	offline := fs.Bool("offline", false, "skip the GitHub and LLM checks")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	c, err := config.Load(*cfgPath, os.LookupEnv)
	if err != nil {
		return reportErrors(err, stderr)
	}
	loaded, err := loadSecrets(secret.OS)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "ok: %s, %d repo(s)\n", *cfgPath, len(c.Repos))
	for _, r := range c.Repos {
		from := "yaml"
		if r.FromEnv {
			from = config.EnvRepos
		}
		if n := utf8.RuneCountInString(*r.Instructions); n > 0 {
			from += fmt.Sprintf(", instructions: %d characters", n)
		}
		fmt.Fprintf(stdout, "  %s (%s)\n", r.Name(), from)
	}
	for i, l := range loaded {
		fmt.Fprintf(stdout, "%s: %s\n", secretSpecs[i].Label, l.Source)
	}
	if !loaded[secGitHub].Present() {
		fmt.Fprintln(stdout, "warning: no GitHub token; requests are limited to 60 per hour")
	}
	ntfyURL := loaded[secNtfyURL].Value.Reveal()
	if n := c.Notify.Ntfy; n != nil && n.URL != "" {
		ntfyURL = n.URL
	}
	if err := checkNtfyURL(c, loaded[secNtfyURL]); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if ntfyURL == "" {
		fmt.Fprintln(stdout, "alerts: off (no ntfy URL); urgent security releases are still marked in the feed")
	} else if u, err := url.Parse(ntfyURL); err == nil && strings.EqualFold(u.Hostname(), "ntfy.sh") && !loaded[secNtfy].Present() {
		fmt.Fprintln(stdout, "warning: ntfy.sh without a token: anyone who knows the topic can read the alerts; keep the URL in NTFY_URL_FILE")
	}
	if c.LLM.Model == "" {
		fmt.Fprintln(stdout, "warning: llm.model is not set; releases are published as excerpts, without summaries")
	}
	if c.LLM.Model != "" && !loaded[secLLM].Present() && !localURL(c.LLM.BaseURL) {
		fmt.Fprintf(stdout, "warning: llm.base_url %s is not a local address and there is no llm api key\n", c.LLM.BaseURL)
	}
	if *offline {
		return 0
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	problems := lintGitHub(ctx, c, loaded, stdout, stderr) + lintLLM(ctx, c, loaded, stdout, stderr)
	if problems > 0 {
		fmt.Fprintf(stderr, "%d problem(s)\n", problems)
		return 1
	}
	return 0
}

// lintGitHub runs the token guard and the public check on every repo. It
// returns the number of problems.
func lintGitHub(ctx context.Context, c *config.Config, loaded []secret.Loaded, stdout, stderr io.Writer) int {
	gh := &github.Client{BaseURL: c.GitHub.APIURL, Token: func() secret.String { return loaded[secGitHub].Value },
		HTTP: &http.Client{}, Timeout: c.GitHub.RequestTimeout.D, UserAgent: generator()}
	if err := gh.Guard(ctx, loaded[secGitHub].Value); err != nil {
		// Without a guard pass, the service doesn't poll: don't probe either.
		fmt.Fprintf(stderr, "error: github: %v\n", err)
		return 1
	}
	problems := 0
	for _, r := range c.Repos {
		vis, err := gh.CheckPublic(ctx, r.Name(), 0)
		switch {
		case vis.Public:
			fmt.Fprintf(stdout, "github: %s: public\n", r.Name())
		case vis.Gone:
			fmt.Fprintf(stderr, "error: github: %s is private or doesn't exist; only public repos are followed\n", r.Name())
			problems++
		default:
			fmt.Fprintf(stderr, "error: github: %s: %v\n", r.Name(), err)
			problems++
		}
		if ctx.Err() != nil {
			return problems + 1
		}
	}
	return problems
}

// lintPrompt is the LLM check's whole request: fixed text, no release data.
var lintPrompt = []llm.Message{
	{Role: "system", Content: "You are a health check. Reply with only the JSON object {\"ok\": true}."},
	{Role: "user", Content: "Are you there?"},
}

var lintSchema = json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}`)

// lintLLM sends the configured model a tiny structured request. It returns
// the number of problems.
func lintLLM(ctx context.Context, c *config.Config, loaded []secret.Loaded, stdout, stderr io.Writer) int {
	if c.LLM.Model == "" {
		fmt.Fprintln(stdout, "llm: not checked (no llm.model)")
		return 0
	}
	cl := &llm.Client{BaseURL: c.LLM.BaseURL, Model: c.LLM.Model, Key: func() secret.String { return loaded[secLLM].Value },
		Headers: c.LLM.Headers, Temperature: *c.LLM.Temperature, Timeout: c.LLM.Timeout.D, HTTP: &http.Client{},
		UserAgent: generator(), Sleep: llmSleep}
	start := time.Now()
	resp, err := cl.Complete(ctx, llm.Request{Messages: lintPrompt, Name: "health_check", Schema: lintSchema})
	if err != nil {
		fmt.Fprintf(stderr, "error: llm: %s at %s: %v\n", c.LLM.Model, c.LLM.BaseURL, err)
		return 1
	}
	var v struct{ OK *bool }
	if err := json.Unmarshal([]byte(stripFence(resp.Content)), &v); err != nil || v.OK == nil {
		// It answered, so the endpoint and key work; the summaries' repair
		// retry may still cope.
		fmt.Fprintf(stdout, "warning: llm: %s answered, but not with the requested JSON: %.80q\n", c.LLM.Model, resp.Content)
	}
	fmt.Fprintf(stdout, "llm: %s answered in %s (response format: %s)\n", c.LLM.Model,
		time.Since(start).Round(10*time.Millisecond), cl.Format())
	if cl.Format() == "prompt" {
		fmt.Fprintln(stdout, "warning: llm: the server rejected structured output; summaries rely on prompt instructions and the repair retry")
	}
	return 0
}

// stripFence removes a Markdown code fence around a reply.
func stripFence(s string) string {
	s = strings.TrimSpace(s)
	if rest, ok := strings.CutPrefix(s, "```"); ok {
		rest = strings.TrimPrefix(rest, "json")
		s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(rest), "```"))
	}
	return s
}

// localURL reports whether u's host is loopback, private, or a bare
// single-label name such as a compose service ("ollama").
func localURL(u string) bool {
	p, err := url.Parse(u)
	if err != nil {
		return false
	}
	host := p.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
	}
	return host == "localhost" || !strings.Contains(host, ".")
}
