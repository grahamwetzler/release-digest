package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// cmdBuild runs one tick and exits, for cron-style use and testing.
func cmdBuild(args []string, stdout, stderr io.Writer) int {
	fs, cfgPath := newFlags("build", stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	e, code := open(ctx, *cfgPath, stderr)
	if e == nil {
		return code
	}
	defer e.store.Close()
	if err := e.startGuard(ctx); err != nil {
		fmt.Fprintf(stderr, "release-digest: %v\n", err)
		return 1
	}
	if err := e.tick(ctx, nil); err != nil {
		fmt.Fprintf(stderr, "release-digest: %v\n", err)
		return 1
	}
	n, err := e.store.FeedReleases(ctx, e.cfg.Feed.MaxItems, time.Now())
	if err != nil {
		fmt.Fprintf(stderr, "release-digest: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "built: %d item(s) in the feed\n", len(n))
	return 0
}
