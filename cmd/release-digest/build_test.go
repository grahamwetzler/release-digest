package main

import (
	"bytes"
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"release-digest/internal/feed"
	"release-digest/internal/store"
)

func TestBuildFailsAndSkipsLastTickOnRenderError(t *testing.T) {
	cleanEnv(t)
	cfg := writeConfig(t, "")
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(filepath.Dir(cfg), "rd.db"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	// A relative link fails the feed checks, so the render must fail.
	st.InsertRelease(ctx, &store.Release{Repo: "o/r", Tag: "v1", GUID: "g", Kind: "release", HTMLURL: "/relative",
		Published: now, FirstSeen: now, State: store.StateFallback, Origin: "live"})
	st.Close()

	var out, errb bytes.Buffer
	if code := run([]string{"build", "--config", cfg}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "failed its checks") {
		t.Fatalf("build: exit %d\nstdout: %s\nstderr: %s", code, out.String(), errb.String())
	}
	if strings.Contains(out.String(), "built") {
		t.Errorf("reported success: %s", out.String())
	}
	st, _ = store.Open(ctx, filepath.Join(filepath.Dir(cfg), "rd.db"))
	defer st.Close()
	if last, err := st.StateTime(ctx, stateLastTick); err != nil || !last.IsZero() {
		t.Errorf("last_tick = %v, %v; want unset after a failed tick", last, err)
	}
	if r, _ := st.Rendered(ctx, "feed.xml"); r != nil {
		t.Error("a failed render was stored")
	}
}

func TestValidateHonorsW3CVerdict(t *testing.T) {
	cleanEnv(t)
	cfg := writeConfig(t, "")
	var out, errb bytes.Buffer
	if code := run([]string{"build", "--config", cfg}, &out, &errb); code != 0 {
		t.Fatalf("build: %d %s", code, errb.String())
	}
	for _, tc := range []struct {
		res  feed.W3CResult
		want int
	}{
		{feed.W3CResult{Valid: true}, 0},
		{feed.W3CResult{Valid: false}, 1}, // invalid with no messages at all
		{feed.W3CResult{Valid: true, Warnings: []feed.W3CMessage{{Type: "Unexpected"}}}, 1},
		{feed.W3CResult{Valid: true, Warnings: []feed.W3CMessage{{Type: "SelfDoesntMatchLocation"}}}, 0},
	} {
		validateW3C = func(context.Context, *http.Client, string, []byte) (*feed.W3CResult, error) { r := tc.res; return &r, nil }
		out.Reset()
		errb.Reset()
		if code := run([]string{"validate", "--w3c", "--config", cfg}, &out, &errb); code != tc.want {
			t.Errorf("%+v: exit %d, want %d\n%s%s", tc.res, code, tc.want, out.String(), errb.String())
		}
	}
	validateW3C = feed.ValidateW3C
}
