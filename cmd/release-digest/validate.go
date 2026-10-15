package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"release-digest/internal/feed"
	"release-digest/internal/render"
)

// cmdValidate checks the stored feed offline and, with --w3c, with the W3C
// Feed Validation Service. Run it after `build`, not on every commit.
func cmdValidate(args []string, stdout, stderr io.Writer) int {
	fs, cfgPath := newFlags("validate", stderr)
	w3c := fs.Bool("w3c", false, "also post each format to the W3C Feed Validation Service")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx := context.Background()
	e, code := open(ctx, *cfgPath, stderr)
	if e == nil {
		return code
	}
	defer e.store.Close()

	failed := false
	hc := &http.Client{Timeout: 60 * time.Second}
	for _, f := range render.Formats {
		r, err := e.store.Rendered(ctx, f.Path)
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", f.Path, err)
			return 1
		}
		if r == nil {
			fmt.Fprintf(stderr, "%s: not built yet (run release-digest build)\n", f.Path)
			failed = true
			continue
		}
		if probs := f.Check(r.Bytes); len(probs) > 0 {
			for _, p := range probs {
				fmt.Fprintf(stderr, "%s: %s\n", f.Path, p)
			}
			failed = true
			continue
		}
		fmt.Fprintf(stdout, "%s: ok (offline checks)\n", f.Path)
		if !*w3c {
			continue
		}
		res, err := feed.ValidateW3C(ctx, hc, generator(), r.Bytes)
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", f.Path, err)
			failed = true
			continue
		}
		bad := res.Unexpected()
		for _, m := range bad {
			fmt.Fprintf(stderr, "%s: W3C %s\n", f.Path, m)
		}
		if len(bad) > 0 {
			failed = true
		} else {
			fmt.Fprintf(stdout, "%s: ok (W3C)\n", f.Path)
		}
	}
	if failed {
		return 1
	}
	return 0
}
