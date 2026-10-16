package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"release-digest/internal/store"
)

func get(t *testing.T, h http.Handler, path string, hdr map[string]string) *http.Response {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Result()
}

func TestServe(t *testing.T) {
	s := New("Release Digest", "https://rss.example.com", slog.New(slog.DiscardHandler))
	h := s.Handler()

	if resp := get(t, h, "/feed.xml", nil); resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Errorf("before the first render: %s", resp.Status)
	}

	mod := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	s.Set(map[string]*store.Rendered{"feed.xml": {Bytes: []byte("<rss/>"), ETag: `"e1"`, Modified: mod}})
	resp := get(t, h, "/feed.xml", nil)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "<rss/>" || resp.Header.Get("ETag") != `"e1"` ||
		resp.Header.Get("Content-Type") != "application/rss+xml; charset=utf-8" ||
		resp.Header.Get("Last-Modified") != "Thu, 15 Oct 2026 12:00:00 GMT" {
		t.Errorf("feed: %s %q %v", resp.Status, body, resp.Header)
	}
	if resp := get(t, h, "/feed.xml", map[string]string{"If-None-Match": `"e1"`}); resp.StatusCode != http.StatusNotModified {
		t.Errorf("conditional GET: %s", resp.Status)
	}
	if resp := get(t, h, "/healthz", nil); resp.StatusCode != 200 {
		t.Errorf("healthz: %s", resp.Status)
	}
	resp = get(t, h, "/", nil)
	body, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `href="https://rss.example.com/feed.atom"`) {
		t.Errorf("index:\n%s", body)
	}
	if resp := get(t, h, "/nope", nil); resp.StatusCode != 404 {
		t.Errorf("unknown path: %s", resp.Status)
	}
}

func TestHealthzReportsDegraded(t *testing.T) {
	s := New("Release Digest", "https://rss.example.com", slog.New(slog.DiscardHandler))
	var problem error
	s.Health = func(context.Context) error { return problem }
	if resp := get(t, s.Handler(), "/healthz", nil); resp.StatusCode != 200 {
		t.Errorf("healthy: %s", resp.Status)
	}
	problem = errors.New("degraded: 1 security alert(s) expired undelivered")
	resp := get(t, s.Handler(), "/healthz", nil)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "expired undelivered") {
		t.Errorf("degraded: %s %q", resp.Status, body)
	}
}
