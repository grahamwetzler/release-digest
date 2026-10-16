// Package server serves the rendered feed over HTTP (docs/feed.md): RSS,
// Atom, an index page and a health probe. Handlers only read; the scheduler
// replaces the served bytes with Set after each render.
package server

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"sync"

	"release-digest/internal/render"
	"release-digest/internal/store"
)

// CacheControl lets a reverse proxy or CDN cache the feed briefly.
const CacheControl = "public, max-age=300"

// Server holds the last good rendering of each format.
type Server struct {
	title, baseURL string
	log            *slog.Logger

	mu    sync.RWMutex
	feeds map[string]*store.Rendered // by path, e.g. feed.xml

	// Health, if set, reports a problem that makes /healthz answer 503.
	Health func(ctx context.Context) error
}

func New(title, publicBaseURL string, log *slog.Logger) *Server {
	return &Server{title: title, baseURL: publicBaseURL, log: log, feeds: map[string]*store.Rendered{}}
}

// Load serves whatever the store last published, so a restart serves at once.
func (s *Server) Load(ctx context.Context, st *store.Store) error {
	for _, f := range render.Formats {
		r, err := st.Rendered(ctx, f.Path)
		if err != nil {
			return err
		}
		if r != nil {
			s.Set(map[string]*store.Rendered{f.Path: r})
		}
	}
	return nil
}

// Set replaces the served bytes of the given paths.
func (s *Server) Set(feeds map[string]*store.Rendered) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for path, r := range feeds {
		s.feeds[path] = r
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, f := range render.Formats {
		mux.HandleFunc("GET /"+f.Path, func(w http.ResponseWriter, r *http.Request) { s.serveFeed(w, r, f) })
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if s.Health != nil {
			if err := s.Health(r.Context()); err != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				fmt.Fprintln(w, err)
				return
			}
		}
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /{$}", s.serveIndex)
	return mux
}

func (s *Server) serveFeed(w http.ResponseWriter, r *http.Request, f render.Format) {
	s.mu.RLock()
	fd := s.feeds[f.Path]
	s.mu.RUnlock()
	if fd == nil {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "feed not built yet", http.StatusServiceUnavailable)
		return
	}
	h := w.Header()
	h.Set("Content-Type", f.ContentType)
	h.Set("ETag", fd.ETag)
	h.Set("Cache-Control", CacheControl)
	// ServeContent answers conditional requests with 304 and handles HEAD.
	http.ServeContent(w, r, "", fd.Modified, bytes.NewReader(fd.Bytes))
}

var index = template.Must(template.New("index").Parse(`<!doctype html>
<html lang="en">
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<link rel="alternate" type="application/rss+xml" title="{{.Title}}" href="{{.RSS}}">
<link rel="alternate" type="application/atom+xml" title="{{.Title}}" href="{{.Atom}}">
<style>
:root{color-scheme:light dark}
body{margin:0;font:16px/1.6 ui-sans-serif,system-ui,-apple-system,"Segoe UI",Roboto,sans-serif}
main{max-width:40rem;margin:0 auto;padding:4rem 1rem}
h1{margin:0 0 1rem;font-size:1.75rem}
</style>
<main>
<h1>{{.Title}}</h1>
<p>Subscribe with <a href="{{.RSS}}">RSS</a> or <a href="{{.Atom}}">Atom</a>.</p>
</main>
`))

func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	data := struct{ Title, RSS, Atom string }{s.title, s.baseURL + "/" + render.RSS.Path, s.baseURL + "/" + render.Atom.Path}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := index.Execute(w, data); err != nil {
		s.log.Error("rendering index", "err", err)
	}
}
