package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"release-digest/internal/advisory"
	"release-digest/internal/config"
	"release-digest/internal/github"
	"release-digest/internal/llm"
	"release-digest/internal/notify"
	"release-digest/internal/secret"
	"release-digest/internal/store"
	"release-digest/internal/summarize"
)

// cmdNotify is "notify test" and "notify ack" (§3.4).
func cmdNotify(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "test":
			return cmdNotifyTest(args[1:], stdout, stderr)
		case "ack":
			return cmdNotifyAck(args[1:], stdout, stderr)
		}
	}
	fmt.Fprintln(stderr, "usage: release-digest notify test [--release owner/repo@tag] | notify ack")
	return 2
}

// cmdNotifyAck acknowledges expired alerts, clearing /healthz's degraded state.
func cmdNotifyAck(args []string, stdout, stderr io.Writer) int {
	fs, cfgPath := newFlags("notify ack", stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(*cfgPath, os.LookupEnv)
	if err != nil {
		return reportErrors(err, stderr)
	}
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.StorePath)
	if err != nil {
		fmt.Fprintf(stderr, "release-digest: %v\n", err)
		return 1
	}
	defer st.Close()
	acked, err := st.AckExpired(ctx, time.Now())
	if err != nil {
		fmt.Fprintf(stderr, "release-digest: %v\n", err)
		return 1
	}
	for _, n := range acked {
		fmt.Fprintf(stdout, "acknowledged: %s@%s (expired %s after %d attempt(s): %s)\n",
			n.Repo, n.Tag, n.Expires.Format(time.RFC3339), n.Attempts, orNone(n.LastError))
	}
	fmt.Fprintf(stdout, "%d expired alert(s) acknowledged\n", len(acked))
	return 0
}

// cmdNotifyTest sends a sample alert through the configured ntfy path, or
// with --release runs detection on a real release and prints the decision
// without sending anything. Like summarize, it writes nothing.
func cmdNotifyTest(args []string, stdout, stderr io.Writer) int {
	fs, cfgPath := newFlags("notify test", stderr)
	rel := fs.String("release", "", "owner/repo@tag: run detection on this release and print the decision; sends nothing")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	cfg, err := config.Load(*cfgPath, os.LookupEnv)
	if err != nil {
		return reportErrors(err, stderr)
	}
	loaded, err := loadSecrets(secret.OS)
	if err == nil {
		err = checkNtfyURL(cfg, loaded[secNtfyURL])
	}
	if err != nil {
		fmt.Fprintf(stderr, "release-digest: %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *rel != "" {
		return detect(ctx, cfg, loaded, *rel, stdout, stderr)
	}

	url := loaded[secNtfyURL].Value.Reveal()
	if n := cfg.Notify.Ntfy; n != nil && n.URL != "" {
		url = n.URL
	}
	if url == "" {
		fmt.Fprintln(stderr, "release-digest: no ntfy URL is configured (notify.ntfy.url or NTFY_URL_FILE)")
		return 1
	}
	nt := &notify.Ntfy{URL: func() string { return url }, Token: func() secret.String { return loaded[secNtfy].Value },
		HTTP: &http.Client{}, UserAgent: generator()}
	p := notify.Payload{
		Title:    "🚨 owner/repo v1.2.3: security release (test)",
		Priority: 3,
		Tags:     []string{"rotating_light", "test"},
		Click:    "https://github.com/advisories",
		Body:     "**This is a test alert from release-digest.**\n\nReal alerts look like this one, with the release's headline, the advisory's severity and the security fixes.",
	}
	if err := nt.Send(ctx, p); err != nil {
		fmt.Fprintf(stderr, "release-digest: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "sent a test alert")
	return 0
}

// detect is notify test --release: resolve the release's notes as the
// poll would, look its advisories up, summarize it if a model is
// configured, and print the decision and the alert it would send.
func detect(ctx context.Context, cfg *config.Config, loaded []secret.Loaded, target string, stdout, stderr io.Writer) int {
	repo, tag, ok := strings.Cut(target, "@")
	if !ok || tag == "" || strings.Count(repo, "/") != 1 {
		fmt.Fprintln(stderr, "release-digest: --release must be owner/repo@tag")
		return 2
	}
	repo = strings.ToLower(repo)
	gh := &github.Client{BaseURL: cfg.GitHub.APIURL, Token: func() secret.String { return loaded[secGitHub].Value },
		HTTP: &http.Client{}, Timeout: cfg.GitHub.RequestTimeout.D, UserAgent: generator()}
	if err := gh.Guard(ctx, loaded[secGitHub].Value); err != nil {
		fmt.Fprintf(stderr, "release-digest: %v\n", err)
		return 1
	}
	in, row, warn, err := dryRunNotes(ctx, gh, cfg, repo, tag)
	if err != nil {
		fmt.Fprintf(stderr, "release-digest: %v\n", err)
		return 1
	}
	if warn != "" {
		fmt.Fprintf(stderr, "warning: %s\n", warn)
	}
	fmt.Fprintf(stdout, "%s@%s (previous: %s), notes: %s\n", repo, tag, orNone(row.PrevTag), orNone(in.Source))

	opts := notify.RepoOptions(cfg, repo)
	inputs := notify.Inputs{Release: row, Notes: in.Notes, Packages: opts.Packages, MinSeverity: cfg.Notify.MinSeverity}
	if opts.PrereleasePattern != nil {
		inputs.Prerelease = opts.PrereleasePattern.Re
	}
	for _, id := range advisory.Extract(in.Notes) {
		a := &store.Advisory{ID: id, FetchedAt: time.Now()}
		data, err := gh.Advisories(ctx, id)
		switch {
		case err == nil:
			a.Found, a.JSON = true, string(data)
		case !errors.Is(err, github.ErrNotFound):
			fmt.Fprintf(stderr, "release-digest: looking up %s: %v\n", id, err)
			return 1
		}
		inputs.Links = append(inputs.Links, store.Link{AdvisoryID: id, Advisory: a})
	}

	if cfg.LLM.Model != "" && in.Source != "" {
		prompt, err := summarize.LoadPrompt(cfg.LLM.PromptFile)
		if err != nil {
			fmt.Fprintf(stderr, "release-digest: %v\n", err)
			return 1
		}
		s := &summarize.Summarizer{Cfg: cfg, LLM: &llm.Client{BaseURL: cfg.LLM.BaseURL, Model: cfg.LLM.Model,
			Key: func() secret.String { return loaded[secLLM].Value }, Headers: cfg.LLM.Headers,
			Temperature: *cfg.LLM.Temperature, Timeout: cfg.LLM.Timeout.D, HTTP: &http.Client{}, UserAgent: generator(), Sleep: llmSleep}}
		recheck := func(ctx context.Context) (bool, error) {
			vis, err := gh.CheckPublic(ctx, repo, 0)
			return vis.Public, err
		}
		// Resolving took several requests: check again before the notes go out (§3.1).
		if public, err := recheck(ctx); err != nil || !public {
			fmt.Fprintf(stderr, "release-digest: %s is no longer confirmed public; nothing was sent to the LLM (%v)\n", repo, err)
			return 1
		}
		res, err := s.Summarize(ctx, prompt, in, recheck)
		if err != nil {
			fmt.Fprintf(stderr, "release-digest: %v\n", err)
			return 1
		}
		inputs.Summary = res.Summary
	}

	d := notify.Decide(inputs)
	if len(d.Links) == 0 {
		fmt.Fprintln(stdout, "advisory signal: no GHSA or CVE IDs in the notes")
	}
	for _, l := range d.Links {
		switch {
		case l.Fires:
			fmt.Fprintf(stdout, "advisory %s: fires (severity %s, %s evidence)\n", l.ID, l.Severity, l.Evidence)
		default:
			fmt.Fprintf(stdout, "advisory %s: doesn't fire: %s\n", l.ID, l.Note)
		}
	}
	switch {
	case inputs.Summary == nil:
		fmt.Fprintln(stdout, "LLM signal: not run (no llm.model or no notes)")
	case d.Downgraded:
		fmt.Fprintln(stdout, "LLM signal: urgent, downgraded to advisory (no security change listed or no security keyword in the notes)")
	default:
		fmt.Fprintf(stdout, "LLM signal: %s (%s)\n", d.LLM, orNone(inputs.Summary.SecurityReason))
	}
	fmt.Fprintf(stdout, "decision: %s", d.Urgency)
	if d.Source != "" {
		fmt.Fprintf(stdout, " (%s)", d.Source)
	}
	fmt.Fprintln(stdout)
	if d.Urgency != store.UrgencyUrgent {
		return 0
	}
	out, _ := json.MarshalIndent(notify.BuildPayload(row, inputs.Summary, d), "", "  ")
	fmt.Fprintf(stdout, "alert payload (not sent):\n%s\n", out)
	return 0
}
