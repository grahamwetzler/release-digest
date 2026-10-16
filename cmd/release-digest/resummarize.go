package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"release-digest/internal/config"
	"release-digest/internal/store"
)

// cmdResummarize flags releases for a fresh summary (§3.3). It only marks
// them: the summarize stage of the next tick (run's, or build's) makes the
// calls, within llm.max_per_pass and the public check like any summary.
// Each release keeps its current summary until the new one is stored.
func cmdResummarize(args []string, stdout, stderr io.Writer) int {
	fs, cfgPath := newFlags("resummarize", stderr)
	repo := fs.String("repo", "", "only this owner/repo (default: every repo)")
	since := fs.String("since", "30d", `only releases published within this long, such as 30d or 12h; "all" for every release`)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	from := time.Time{}
	now := time.Now()
	if *since != "all" {
		d, err := config.ParseDuration(*since)
		if err != nil || d <= 0 {
			fmt.Fprintf(stderr, "release-digest: --since: %s must be a positive duration such as 30d, or all\n", *since)
			return 2
		}
		from = now.Add(-d)
	}
	cfg, err := config.Load(*cfgPath, os.LookupEnv)
	if err != nil {
		return reportErrors(err, stderr)
	}
	if cfg.LLM.Model == "" {
		fmt.Fprintln(stderr, "release-digest: llm.model is not set, so there is nothing to summarize with")
		return 1
	}
	name := strings.ToLower(*repo)
	if name != "" && !configured(cfg, name) {
		fmt.Fprintf(stderr, "release-digest: %s is not a configured repo\n", *repo)
		return 2
	}
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.StorePath)
	if err != nil {
		fmt.Fprintf(stderr, "release-digest: %v\n", err)
		return 1
	}
	defer st.Close()
	rows, err := st.RequestResummarize(ctx, name, from, now)
	if err != nil {
		fmt.Fprintf(stderr, "release-digest: %v\n", err)
		return 1
	}
	for _, r := range rows {
		fmt.Fprintf(stdout, "  %s@%s (%s)\n", r.Repo, r.Tag, r.State)
	}
	fmt.Fprintf(stdout, "%d release(s) queued for a fresh summary; the next tick summarizes up to %d of them per pass\n",
		len(rows), cfg.LLM.MaxPerPass)
	return 0
}

func configured(cfg *config.Config, name string) bool {
	for _, r := range cfg.Repos {
		if r.Name() == name {
			return true
		}
	}
	return false
}
