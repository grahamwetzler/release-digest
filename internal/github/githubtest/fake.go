// Package githubtest is an in-memory fake of the GitHub REST endpoints the
// poller uses, with ETags, pagination and failure injection.
package githubtest

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Release is a fake release; Draft and Prerelease mirror the API.
type Release struct {
	Tag, Name, Body   string
	Draft, Prerelease bool
	Published         time.Time
}

// Tag is a fake tag. SHA is its commit, dated Date. An annotated tag points
// at a tag object ("t" + SHA) tagged at TaggerDate.
type Tag struct {
	Name       string
	SHA        string
	Date       time.Time
	Annotated  bool
	TaggerDate time.Time
}

// Commit is a fake commit of a comparison.
type Commit struct {
	Message, Login, Type, Name string
	Parents                    int
}

// Comparison is a fake compare result; Commits are oldest first. Total
// defaults to len(Commits).
type Comparison struct {
	Status  string
	Total   int
	Commits []Commit
}

// Repo is a fake repository. Releases and Tags are newest first.
type Repo struct {
	Private  bool
	Releases []Release
	Tags     []Tag
	// Files maps a ref ("" for the default branch) to its files by path.
	// A ref with no entry has an empty tree.
	Files map[string]map[string]string
	// Compares maps "base...head" to its comparison; any other is a 404.
	Compares map[string]Comparison
	// Status, when set, is returned for every request to the repo.
	Status int
	// Hang, when set, blocks every request to the repo until the client gives up.
	Hang bool
}

// Server is the fake. Lock Mu to change Repos while it runs.
type Server struct {
	*httptest.Server
	Mu    sync.Mutex
	Repos map[string]*Repo // by lowercase owner/repo

	// Token guard inputs.
	Scopes       string // X-OAuth-Scopes for authenticated requests
	PrivateRepos int    // how many repos /user/repos?visibility=private lists

	// Advisories are the global advisories, by GHSA ID, as JSON objects.
	// Lock Mu to change them while the server runs.
	Advisories map[string]string

	// RateLimited, when set, refuses every request with 403 and remaining 0 until Reset.
	RateLimited bool
	Reset       time.Time

	// Before, when set, runs at the start of every request (with Mu held).
	Before func(r *http.Request)

	Requests []string // "path?query" of every request, in order
	Auth     []string // Authorization header of every request
	NotMod   int      // 304s served
}

// New starts a fake with no repos.
func New() *Server {
	s := &Server{Repos: map[string]*Repo{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// Repo returns the named repo, creating it.
func (s *Server) Repo(name string) *Repo {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	r := s.Repos[strings.ToLower(name)]
	if r == nil {
		r = &Repo{}
		s.Repos[strings.ToLower(name)] = r
	}
	return r
}

// Count returns how many requests had a path starting with prefix.
func (s *Server) Count(prefix string) int {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	n := 0
	for _, r := range s.Requests {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

// Reset clears the request log.
func (s *Server) ResetLog() {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	s.Requests, s.Auth, s.NotMod = nil, nil, 0
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.Mu.Lock()
	s.Requests = append(s.Requests, r.URL.EscapedPath()+"?"+r.URL.RawQuery)
	s.Auth = append(s.Auth, r.Header.Get("Authorization"))
	if s.Before != nil {
		s.Before(r)
	}
	if s.RateLimited {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(s.Reset.Unix(), 10))
		s.Mu.Unlock()
		http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
		return
	}
	status, body, hang := s.route(r)
	if r.Header.Get("Authorization") != "" {
		w.Header().Set("X-OAuth-Scopes", s.Scopes)
	}
	s.Mu.Unlock()
	if hang {
		<-r.Context().Done()
		return
	}
	w.Header().Set("X-RateLimit-Remaining", "4999")
	w.Header().Set("Content-Type", "application/json")
	if status != http.StatusOK {
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"message":%q}`, http.StatusText(status))
		return
	}
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		s.Mu.Lock()
		s.NotMod++
		s.Mu.Unlock()
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Write(body)
}

// route answers a request; called with Mu held.
func (s *Server) route(r *http.Request) (int, []byte, bool) {
	path, _ := url.PathUnescape(r.URL.EscapedPath())
	q := r.URL.Query()
	if path == "/user/repos" {
		repos := make([]map[string]any, s.PrivateRepos)
		for i := range repos {
			repos[i] = map[string]any{"full_name": fmt.Sprintf("me/private%d", i), "private": true}
		}
		return ok(repos)
	}
	if id, found := strings.CutPrefix(path, "/advisories/"); found {
		if a, ok := s.Advisories[id]; ok {
			return http.StatusOK, []byte(a), false
		}
		return http.StatusNotFound, nil, false
	}
	if path == "/advisories" {
		list := []json.RawMessage{}
		for _, a := range s.Advisories {
			var v struct {
				CVE string `json:"cve_id"`
			}
			json.Unmarshal([]byte(a), &v)
			if v.CVE != "" && v.CVE == q.Get("cve_id") {
				list = append(list, json.RawMessage(a))
			}
		}
		return ok(list)
	}
	parts := strings.SplitN(strings.TrimPrefix(path, "/repos/"), "/", 3)
	if !strings.HasPrefix(path, "/repos/") || len(parts) < 2 {
		return http.StatusNotFound, nil, false
	}
	name := strings.ToLower(parts[0] + "/" + parts[1])
	repo := s.Repos[name]
	if repo == nil {
		return http.StatusNotFound, nil, false
	}
	if repo.Hang {
		return 0, nil, true
	}
	if repo.Status != 0 {
		return repo.Status, nil, false
	}
	if repo.Private && r.Header.Get("Authorization") == "" {
		return http.StatusNotFound, nil, false // what an outsider sees
	}
	rest := ""
	if len(parts) == 3 {
		rest = parts[2]
	}
	switch {
	case rest == "":
		vis := "public"
		if repo.Private {
			vis = "private"
		}
		return ok(map[string]any{"full_name": name, "private": repo.Private, "visibility": vis})
	case rest == "releases":
		var list []map[string]any
		for _, rel := range repo.Releases {
			list = append(list, releaseJSON(name, rel))
		}
		return ok(page(list, q))
	case rest == "git/matching-refs/tags/":
		list := []map[string]any{}
		for _, t := range repo.Tags {
			obj := map[string]any{"sha": t.SHA, "type": "commit"}
			if t.Annotated {
				obj = map[string]any{"sha": "t" + t.SHA, "type": "tag"}
			}
			list = append(list, map[string]any{"ref": "refs/tags/" + t.Name, "object": obj})
		}
		return ok(list)
	case strings.HasPrefix(rest, "git/tags/"):
		sha := strings.TrimPrefix(rest, "git/tags/")
		for _, t := range repo.Tags {
			if t.Annotated && "t"+t.SHA == sha {
				return ok(map[string]any{"sha": sha, "tag": t.Name, "tagger": map[string]any{"date": t.TaggerDate.UTC().Format(time.RFC3339)},
					"object": map[string]any{"sha": t.SHA, "type": "commit"}})
			}
		}
	case strings.HasPrefix(rest, "releases/tags/"):
		tag := strings.TrimPrefix(rest, "releases/tags/")
		for _, rel := range repo.Releases {
			if rel.Tag == tag && !rel.Draft {
				return ok(releaseJSON(name, rel))
			}
		}
	case strings.HasPrefix(rest, "git/ref/tags/"):
		tag := strings.TrimPrefix(rest, "git/ref/tags/")
		for _, t := range repo.Tags {
			if t.Name == tag {
				return ok(map[string]any{"ref": "refs/tags/" + t.Name, "object": map[string]any{"sha": t.SHA}})
			}
		}
	case rest == "contents" || rest == "contents/":
		list := []map[string]any{}
		for path := range repo.Files[q.Get("ref")] {
			if !strings.Contains(path, "/") {
				list = append(list, map[string]any{"name": path, "path": path, "type": "file"})
			}
		}
		return ok(list)
	case strings.HasPrefix(rest, "contents/"):
		if text, found := repo.Files[q.Get("ref")][strings.TrimPrefix(rest, "contents/")]; found {
			return http.StatusOK, []byte(text), false
		}
	case strings.HasPrefix(rest, "compare/"):
		if c, found := repo.Compares[strings.TrimPrefix(rest, "compare/")]; found {
			var commits []map[string]any
			for i, cm := range c.Commits {
				parents := make([]map[string]any, max(cm.Parents, 1))
				for j := range parents {
					parents[j] = map[string]any{"sha": fmt.Sprintf("p%d", j)}
				}
				x := map[string]any{"sha": fmt.Sprintf("c%d", i), "commit": map[string]any{"message": cm.Message,
					"author": map[string]any{"name": cm.Name}}, "parents": parents, "author": nil}
				if cm.Login != "" {
					x["author"] = map[string]any{"login": cm.Login, "type": cmp.Or(cm.Type, "User")}
				}
				commits = append(commits, x)
			}
			return ok(map[string]any{"status": c.Status, "total_commits": cmp.Or(c.Total, len(c.Commits)), "commits": commits})
		}
	case strings.HasPrefix(rest, "commits/"):
		sha := strings.TrimPrefix(rest, "commits/")
		for _, t := range repo.Tags {
			if t.SHA == sha {
				return ok(map[string]any{"sha": sha, "commit": map[string]any{"committer": map[string]any{"date": t.Date.UTC().Format(time.RFC3339)}}})
			}
		}
	}
	return http.StatusNotFound, nil, false
}

func releaseJSON(repo string, rel Release) map[string]any {
	m := map[string]any{
		"tag_name": rel.Tag, "name": rel.Name, "body": rel.Body, "draft": rel.Draft, "prerelease": rel.Prerelease,
		"html_url":   "https://github.com/" + repo + "/releases/tag/" + rel.Tag,
		"created_at": rel.Published.UTC().Format(time.RFC3339), "published_at": nil,
	}
	if !rel.Draft {
		m["published_at"] = rel.Published.UTC().Format(time.RFC3339)
	}
	return m
}

func page(list []map[string]any, q url.Values) []map[string]any {
	per, _ := strconv.Atoi(q.Get("per_page"))
	if per <= 0 {
		per = 30
	}
	p, _ := strconv.Atoi(q.Get("page"))
	if p <= 0 {
		p = 1
	}
	start := (p - 1) * per
	if start >= len(list) {
		return []map[string]any{}
	}
	return list[start:min(start+per, len(list))]
}

func ok(v any) (int, []byte, bool) {
	b, _ := json.Marshal(v)
	return http.StatusOK, b, false
}
