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
	"regexp"
	"strings"
	"syscall"
	"time"

	"release-digest/internal/config"
	"release-digest/internal/github"
	"release-digest/internal/llm"
	"release-digest/internal/notes"
	"release-digest/internal/poll"
	"release-digest/internal/render"
	"release-digest/internal/secret"
	"release-digest/internal/store"
	"release-digest/internal/summarize"
	semver "release-digest/internal/version"
)

// cmdSummarize is a dry run for one release (§7): it resolves the notes
// from GitHub, calls the LLM and prints the result. It writes nothing: the
// store isn't opened, GitHub responses aren't cached, and the predecessor
// is worked out from GitHub's lists rather than the stored history.
func cmdSummarize(args []string, stdout, stderr io.Writer) int {
	fs, cfgPath := newFlags("summarize", stderr)
	showPrompt := fs.Bool("messages", false, "also print the request messages sent to the LLM")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: release-digest summarize [--config file] [--messages] owner/repo@tag")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	repo, tag, ok := strings.Cut(fs.Arg(0), "@")
	if fs.NArg() != 1 || !ok || tag == "" || strings.Count(repo, "/") != 1 {
		fs.Usage()
		return 2
	}
	repo = strings.ToLower(repo)
	cfg, err := config.Load(*cfgPath, os.LookupEnv)
	if err != nil {
		return reportErrors(err, stderr)
	}
	if cfg.LLM.Model == "" {
		fmt.Fprintln(stderr, "release-digest: llm.model is not set (RELEASE_DIGEST_LLM_MODEL)")
		return 1
	}
	loaded, err := loadSecrets(secret.OS)
	if err != nil {
		fmt.Fprintf(stderr, "release-digest: %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	gh := &github.Client{
		BaseURL:   cfg.GitHub.APIURL,
		Token:     func() secret.String { return loaded[secGitHub].Value },
		HTTP:      &http.Client{},
		Timeout:   cfg.GitHub.RequestTimeout.D,
		UserAgent: generator(),
	}
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
	fmt.Fprintf(stdout, "%s@%s (previous: %s)\n", repo, tag, orNone(row.PrevTag))
	if in.Source == "" {
		fmt.Fprintln(stdout, "no notes found: the release would be published as having no release notes")
		return 0
	}
	fmt.Fprintf(stdout, "notes: %s, %d characters\n", in.Source, len(in.Notes))

	prompt, err := summarize.LoadPrompt(cfg.LLM.PromptFile)
	if err != nil {
		fmt.Fprintf(stderr, "release-digest: %v\n", err)
		return 1
	}
	s := &summarize.Summarizer{Cfg: cfg, LLM: &llm.Client{
		BaseURL:     cfg.LLM.BaseURL,
		Model:       cfg.LLM.Model,
		Key:         func() secret.String { return loaded[secLLM].Value },
		Headers:     cfg.LLM.Headers,
		Temperature: *cfg.LLM.Temperature,
		Timeout:     cfg.LLM.Timeout.D,
		HTTP:        &http.Client{},
		UserAgent:   generator(),
		Sleep:       llmSleep,
	}}
	if *showPrompt {
		for _, m := range summarize.Messages(prompt, in) {
			fmt.Fprintf(stdout, "\n--- %s ---\n%s\n", m.Role, m.Content)
		}
		fmt.Fprintln(stdout)
	}
	// Resolving the notes took several requests: check again just before
	// the notes go to the LLM (§3.1).
	if vis, err := gh.CheckPublic(ctx, repo, 0); err != nil || !vis.Public {
		fmt.Fprintf(stderr, "release-digest: %s is no longer confirmed public; nothing was sent to the LLM (%v)\n", repo, err)
		return 1
	}
	res, err := s.Summarize(ctx, prompt, in, func(ctx context.Context) (bool, error) {
		vis, err := gh.CheckPublic(ctx, repo, 0)
		return vis.Public, err
	})
	if err != nil {
		fmt.Fprintf(stderr, "release-digest: %v\n", err)
		return 1
	}
	out, _ := json.MarshalIndent(res.Summary, "", "  ")
	fmt.Fprintf(stdout, "model: %s, tokens: %d in, %d out\nkey: %s\n%s\n", cfg.LLM.Model, res.TokensIn, res.TokensOut, res.Key, out)
	row.State = store.StateSummarized
	it := render.Item(row, in.Notes, &store.Summary{Model: cfg.LLM.Model, JSON: res.Summary.Encode()})
	fmt.Fprintf(stdout, "\ntitle: %s\n%s\n", it.Title, it.ContentHTML)
	return 0
}

// dryRunNotes finds the release on GitHub and resolves its notes as the
// poll stage would (§3.2), returning the summarize input and a release row
// for rendering. The repo must pass the public check (§3.1).
//
// warn, if set, says the predecessor could not be determined.
func dryRunNotes(ctx context.Context, gh *github.Client, cfg *config.Config, repo, tag string) (in summarize.Input, row *store.Release, warn string, err error) {
	vis, err := gh.CheckPublic(ctx, repo, 0)
	if err != nil {
		return in, nil, "", fmt.Errorf("checking %s is public: %w", repo, err)
	}
	if !vis.Public {
		return in, nil, "", fmt.Errorf("%s is private or not found; only public repos are summarized", repo)
	}
	opts := cfg.Options(repo)
	var pre *regexp.Regexp
	if opts.PrereleasePattern != nil {
		pre = opts.PrereleasePattern.Re
	}
	parse := func(t string) semver.Version { return semver.Parse(t, pre) }

	// Every tag is a candidate predecessor: the tag list is complete, where
	// the releases list is paginated. Dates come from the newest releases,
	// for date-ordered tags.
	row = &store.Release{Repo: repo, Tag: tag, Kind: "release"}
	refs, err := gh.TagRefs(ctx, repo)
	if err != nil {
		return in, nil, "", err
	}
	var cands []semver.Candidate
	found := false
	for _, r := range refs {
		found = found || r.Name == tag
		v := parse(r.Name)
		cands = append(cands, semver.Candidate{Tag: r.Name, Stream: v.Stream, Version: v.String(), Prerelease: v.Prerelease})
	}
	rel, err := gh.ReleaseByTag(ctx, repo, tag)
	switch {
	case errors.Is(err, github.ErrNotFound):
		if !found {
			return in, nil, "", fmt.Errorf("%s has no release or tag %q", repo, tag)
		}
		row.Kind = "tag"
		row.HTMLURL = poll.TagURL(cfg.GitHub.APIURL, repo, tag)
	case err != nil:
		return in, nil, "", err
	default:
		row.Name, row.HTMLURL, row.Prerelease = rel.Name, rel.HTMLURL, rel.Prerelease
		list, err := gh.Releases(ctx, repo, 1)
		if err != nil {
			return in, nil, "", err
		}
		for _, r := range list {
			if r.Draft {
				continue
			}
			v := parse(r.TagName)
			c := semver.Candidate{Tag: r.TagName, Stream: v.Stream, Version: v.String(), Prerelease: r.Prerelease || v.Prerelease}
			if r.PublishedAt != nil {
				c.Published = *r.PublishedAt
			}
			cands = append(cands, c)
		}
	}
	v := parse(tag)
	row.Stream, row.Version = v.Stream, v.String()
	row.Prerelease = row.Prerelease || v.Prerelease
	this := semver.Candidate{Tag: tag, Stream: v.Stream, Version: v.String(), Prerelease: row.Prerelease}
	if rel != nil && rel.PublishedAt != nil {
		this.Published = *rel.PublishedAt
	}
	if this.Version == "" {
		complete, err := datePredecessors(ctx, gh, repo, refs, &this, cands)
		if err != nil {
			return in, nil, "", err
		}
		if !complete {
			// A guess from some of the dates could be the wrong tag.
			this.Published = time.Time{}
			warn = fmt.Sprintf("%s has more than %d date-ordered tags in its stream; the previous version was not determined", tag, MaxDateLookups)
		}
	}
	row.PrevTag = semver.Predecessor(this, cands)

	body := ""
	if rel != nil {
		body = rel.Body
	}
	res := &notes.Resolver{GH: gh, MaxChars: cfg.LLM.MaxInputChars}
	out, err := res.Resolve(ctx, notes.Request{Repo: repo, Tag: tag, Stream: row.Stream, Version: row.Version,
		PrevTag: row.PrevTag, Body: body, Changelog: *opts.Changelog})
	if err != nil {
		return in, nil, "", fmt.Errorf("resolving notes: %w", err)
	}
	row.NotesSource = out.Source
	return summarize.Input{Repo: repo, Tag: tag, PrevTag: row.PrevTag, Source: out.Source, Notes: out.Text,
		Instructions: *opts.Instructions}, row, warn, nil
}

// MaxDateLookups bounds the tag date lookups for a date-ordered tag's
// predecessor in a dry run.
const MaxDateLookups = 30

// datePredecessors fills in the dates a date-ordered tag's predecessor
// needs (§3.2.1): its own, unless its release gave one, and those of the
// other date-ordered tags in its stream that the releases list didn't
// date. It dates at most MaxDateLookups of them and reports false if more
// were needed: then the predecessor can't be determined.
func datePredecessors(ctx context.Context, gh *github.Client, repo string, refs []github.TagRef, this *semver.Candidate, cands []semver.Candidate) (bool, error) {
	byName := map[string]github.TagRef{}
	for _, r := range refs {
		byName[r.Name] = r
	}
	dated := map[string]time.Time{}
	for _, c := range cands {
		if !c.Published.IsZero() {
			dated[c.Tag] = c.Published
		}
	}
	lookup := func(tag string) (time.Time, error) {
		if t, ok := dated[tag]; ok {
			return t, nil
		}
		ref, ok := byName[tag]
		if !ok {
			return time.Time{}, nil
		}
		t, err := gh.TagDate(ctx, repo, ref.SHA, ref.Type)
		if err == nil {
			dated[tag] = t
		}
		return t, err
	}
	var err error
	if this.Published.IsZero() {
		if this.Published, err = lookup(this.Tag); err != nil {
			return false, fmt.Errorf("dating %s: %w", this.Tag, err)
		}
	}
	looked := 0
	for i := range cands {
		c := &cands[i]
		if c.Tag == this.Tag || c.Stream != this.Stream || c.Version != "" || !c.Published.IsZero() {
			continue
		}
		if _, ok := dated[c.Tag]; !ok {
			if looked == MaxDateLookups {
				return false, nil
			}
			looked++
		}
		if c.Published, err = lookup(c.Tag); err != nil {
			return false, fmt.Errorf("dating %s: %w", c.Tag, err)
		}
	}
	return true, nil
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
