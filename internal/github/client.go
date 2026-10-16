// Package github is the REST client for public repos: visibility checks,
// releases, tags, commit dates, existence checks, file contents and
// comparisons, with an ETag cache and rate-limit pacing (docs/polling.md, docs/notes.md).
package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"release-digest/internal/secret"
	"release-digest/internal/store"
)

// PerPage is the page size for list requests.
const PerPage = 30

// MaxWait is the longest the client waits for a rate limit to reset; a
// longer wait fails with a RateLimitError instead (docs/polling.md).
const MaxWait = 2 * time.Minute

const maxBody = 16 << 20

// Cache stores responses for conditional requests. *store.Store implements it.
type Cache interface {
	Cached(ctx context.Context, url string) (*store.CacheEntry, error)
	PutCached(ctx context.Context, url string, e *store.CacheEntry, at time.Time) error
}

// Client talks to one GitHub API base URL.
type Client struct {
	BaseURL   string               // e.g. https://api.github.com
	Token     func() secret.String // read at request time, so a SIGHUP rotation applies at once
	HTTP      *http.Client
	Cache     Cache // nil: no conditional requests
	Timeout   time.Duration
	UserAgent string

	// Now and Sleep are replaceable in tests. Sleep returns false if ctx ended first.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) bool

	mu           sync.Mutex
	blockedUntil time.Time // no requests before this: the rate limit is exhausted
}

// ErrNotFound is a 404.
var ErrNotFound = errors.New("not found")

// RateLimitError means the rate limit is exhausted until Reset, which is too
// far away to wait for.
type RateLimitError struct{ Reset time.Time }

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("GitHub rate limit exhausted until %s", e.Reset.UTC().Format(time.RFC3339))
}

// StatusError is an unexpected HTTP status. It never includes request headers.
type StatusError struct {
	URL     string
	Code    int
	Message string // GitHub's error message, if any
}

func (e *StatusError) Error() string {
	s := fmt.Sprintf("GET %s: %d %s", e.URL, e.Code, http.StatusText(e.Code))
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// IsRateLimit reports whether err is a RateLimitError.
func IsRateLimit(err error) bool {
	var rl *RateLimitError
	return errors.As(err, &rl)
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) sleep(ctx context.Context, d time.Duration) bool {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// response is a completed GET.
type response struct {
	Body   []byte
	Header http.Header
	Cached bool // a 304 answered from the cache
}

// get fetches path (relative to BaseURL) as JSON, conditionally when cache
// is set. A 404 is ErrNotFound; any other non-2xx is a StatusError or
// RateLimitError.
func (c *Client) get(ctx context.Context, path string, timeout time.Duration, token secret.String, cache bool) (*response, error) {
	return c.getAs(ctx, path, mediaJSON, timeout, token, cache)
}

// Media types for the Accept header.
const (
	mediaJSON = "application/vnd.github+json"
	mediaRaw  = "application/vnd.github.raw+json" // a file's contents, as is
)

// getAs is get with the response media type set by accept.
func (c *Client) getAs(ctx context.Context, path, accept string, timeout time.Duration, token secret.String, cache bool) (*response, error) {
	u := c.BaseURL + path
	var entry *store.CacheEntry
	if cache && c.Cache != nil {
		var err error
		if entry, err = c.Cache.Cached(ctx, u); err != nil {
			return nil, err
		}
	}
	for attempt := 0; ; attempt++ {
		if err := c.waitForLimit(ctx); err != nil {
			return nil, err
		}
		resp, err := c.do(ctx, u, accept, timeout, token, entry)
		if err != nil {
			return nil, err
		}
		if wait, limited := c.noteLimit(resp); limited {
			if attempt == 0 && wait <= MaxWait {
				if !c.sleep(ctx, wait) {
					return nil, ctx.Err()
				}
				continue
			}
			return nil, &RateLimitError{Reset: c.now().Add(wait)}
		}
		switch {
		case resp.code == http.StatusNotModified && entry != nil:
			return &response{Body: entry.Body, Header: resp.header, Cached: true}, nil
		case resp.code == http.StatusNotFound:
			return nil, fmt.Errorf("GET %s: %w", u, ErrNotFound)
		case resp.code < 200 || resp.code > 299:
			return nil, &StatusError{URL: u, Code: resp.code, Message: errorMessage(resp.body)}
		}
		if etag := resp.header.Get("ETag"); cache && c.Cache != nil && etag != "" {
			if err := c.Cache.PutCached(ctx, u, &store.CacheEntry{ETag: etag, Body: resp.body}, c.now()); err != nil {
				return nil, err
			}
		}
		return &response{Body: resp.body, Header: resp.header}, nil
	}
}

type rawResponse struct {
	code   int
	header http.Header
	body   []byte
}

func (c *Client) do(ctx context.Context, u, accept string, timeout time.Duration, token secret.String, entry *store.CacheEntry) (*rawResponse, error) {
	if timeout <= 0 {
		timeout = c.Timeout
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", c.UserAgent)
	if !token.IsZero() {
		req.Header.Set("Authorization", "Bearer "+token.Reveal())
	}
	if entry != nil && entry.ETag != "" {
		req.Header.Set("If-None-Match", entry.ETag)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err // a *url.Error: method, URL and cause; never headers
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("GET %s: reading the response: %w", u, err)
	}
	return &rawResponse{code: resp.StatusCode, header: resp.Header, body: body}, nil
}

// waitForLimit waits out an exhausted rate limit if the reset is near, and
// fails without sending a request otherwise.
func (c *Client) waitForLimit(ctx context.Context) error {
	c.mu.Lock()
	until := c.blockedUntil
	c.mu.Unlock()
	wait := until.Sub(c.now())
	if wait <= 0 {
		return nil
	}
	if wait > MaxWait {
		return &RateLimitError{Reset: until}
	}
	if !c.sleep(ctx, wait) {
		return ctx.Err()
	}
	return nil
}

// noteLimit reads the rate-limit headers. It reports whether resp was
// refused by a rate limit and, if so, how long until it resets. A success
// that used the last request blocks further requests until the reset.
func (c *Client) noteLimit(resp *rawResponse) (time.Duration, bool) {
	now := c.now()
	remaining := resp.header.Get("X-RateLimit-Remaining")
	var reset time.Time
	if s, err := strconv.ParseInt(resp.header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
		reset = time.Unix(s, 0)
	}
	retryAfter := -1 * time.Second
	if s, err := strconv.Atoi(resp.header.Get("Retry-After")); err == nil && s >= 0 {
		retryAfter = time.Duration(s) * time.Second
	}
	// A 429 is always a rate limit. A 403 is one when the headers say so or,
	// for a secondary limit sent without them, when its message does.
	limited := resp.code == http.StatusTooManyRequests ||
		(resp.code == http.StatusForbidden && (remaining == "0" || retryAfter >= 0 ||
			strings.Contains(strings.ToLower(errorMessage(resp.body)), "rate limit")))
	until := time.Time{}
	switch {
	case retryAfter >= 0 && limited:
		until = now.Add(retryAfter)
	case remaining == "0" && !reset.IsZero():
		until = reset
	case limited:
		until = now.Add(time.Minute) // a secondary limit with no hint: back off a little
	}
	if !until.IsZero() {
		c.mu.Lock()
		if until.After(c.blockedUntil) {
			c.blockedUntil = until
		}
		c.mu.Unlock()
	}
	if !limited {
		return 0, false
	}
	return max(until.Sub(now), 0), true
}

func errorMessage(body []byte) string {
	var v struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &v) != nil {
		return ""
	}
	if len(v.Message) > 200 {
		return v.Message[:200] + "…"
	}
	return v.Message
}

func (c *Client) token() secret.String {
	if c.Token == nil {
		return secret.String{}
	}
	return c.Token()
}

func (c *Client) getJSON(ctx context.Context, path string, timeout time.Duration, v any) (*response, error) {
	resp, err := c.get(ctx, path, timeout, c.token(), true)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(resp.Body, v); err != nil {
		return nil, fmt.Errorf("GET %s%s: decoding: %w", c.BaseURL, path, err)
	}
	return resp, nil
}

// repoPath is /repos/owner/repo with each part escaped.
func repoPath(repo string) string {
	owner, name, _ := strings.Cut(repo, "/")
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
}

// refPath escapes each segment of a tag, keeping its slashes.
func refPath(tag string) string {
	parts := strings.Split(tag, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// Visibility is the outcome of a public check.
type Visibility struct {
	Public bool
	Gone   bool // private, or a 404 (a repo gone private looks like one)
}

// CheckPublic runs the public check (docs/public-only.md). Only an explicit
// private: false (with a public visibility, where reported) is public.
// A 404 is Gone, not an error. Any error means "not confirmed".
func (c *Client) CheckPublic(ctx context.Context, repo string, timeout time.Duration) (Visibility, error) {
	var v struct {
		Private    *bool  `json:"private"`
		Visibility string `json:"visibility"`
	}
	_, err := c.getJSON(ctx, repoPath(repo), timeout, &v)
	switch {
	case errors.Is(err, ErrNotFound):
		return Visibility{Gone: true}, nil
	case err != nil:
		return Visibility{}, err
	case v.Private == nil:
		return Visibility{}, fmt.Errorf("%s: the response has no private field", repo)
	case *v.Private || (v.Visibility != "" && v.Visibility != "public"):
		return Visibility{Gone: true}, nil
	}
	return Visibility{Public: true}, nil
}

// Release is one entry of the releases list.
type Release struct {
	TagName     string     `json:"tag_name"`
	Name        string     `json:"name"`
	Body        string     `json:"body"`
	Draft       bool       `json:"draft"`
	Prerelease  bool       `json:"prerelease"`
	PublishedAt *time.Time `json:"published_at"`
	CreatedAt   time.Time  `json:"created_at"`
	HTMLURL     string     `json:"html_url"`
}

// Releases returns one page (from 1) of the repo's releases, newest first.
func (c *Client) Releases(ctx context.Context, repo string, page int) ([]Release, error) {
	var out []Release
	_, err := c.getJSON(ctx, fmt.Sprintf("%s/releases?per_page=%d&page=%d", repoPath(repo), PerPage, page), 0, &out)
	return out, err
}

// ReleaseByTag returns the published release for tag. A tag without a
// release, or a draft, is ErrNotFound.
func (c *Client) ReleaseByTag(ctx context.Context, repo, tag string) (*Release, error) {
	var r Release
	if _, err := c.getJSON(ctx, repoPath(repo)+"/releases/tags/"+refPath(tag), 0, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Advisories looks an advisory up in GitHub's global advisory database
// (docs/security-alerts.md): GET /advisories/{ghsa} for a GHSA ID, or /advisories?cve_id= for
// a CVE ID. It returns the advisories as a JSON array, the form they are
// stored in; none found is ErrNotFound. The lookup names only the ID, never
// a repo.
func (c *Client) Advisories(ctx context.Context, id string) (json.RawMessage, error) {
	if strings.HasPrefix(id, "GHSA-") {
		var one json.RawMessage
		if _, err := c.getJSON(ctx, "/advisories/"+url.PathEscape(id), 0, &one); err != nil {
			return nil, err
		}
		return json.RawMessage("[" + string(one) + "]"), nil
	}
	var list []json.RawMessage
	if _, err := c.getJSON(ctx, "/advisories?cve_id="+url.QueryEscape(id), 0, &list); err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("advisory %s: %w", id, ErrNotFound)
	}
	return json.Marshal(list)
}

// TagRef is one tag ref: the tag and the object it points at, a commit for
// a lightweight tag or a tag object for an annotated one.
type TagRef struct {
	Name string
	SHA  string
	Type string // commit | tag
}

// TagRefs returns every tag of the repo in one request. Unlike the tags
// list, which is sorted by name and paginated, this is complete, so new and
// deleted tags can be found without depending on order (docs/polling.md).
func (c *Client) TagRefs(ctx context.Context, repo string) ([]TagRef, error) {
	var refs []struct {
		Ref    string `json:"ref"`
		Object struct {
			SHA  string `json:"sha"`
			Type string `json:"type"`
		} `json:"object"`
	}
	if _, err := c.getJSON(ctx, repoPath(repo)+"/git/matching-refs/tags/", 0, &refs); err != nil {
		return nil, err
	}
	out := make([]TagRef, 0, len(refs))
	for _, r := range refs {
		name, ok := strings.CutPrefix(r.Ref, "refs/tags/")
		if !ok || name == "" || r.Object.SHA == "" {
			continue
		}
		out = append(out, TagRef{Name: name, SHA: r.Object.SHA, Type: r.Object.Type})
	}
	return out, nil
}

// TagDate returns when a tag was made: the tagger date of an annotated tag
// (refType "tag"), or the committer date of a lightweight tag's commit.
func (c *Client) TagDate(ctx context.Context, repo, sha, refType string) (time.Time, error) {
	if refType == "tag" {
		var v struct {
			Tagger struct {
				Date time.Time `json:"date"`
			} `json:"tagger"`
		}
		if _, err := c.getJSON(ctx, repoPath(repo)+"/git/tags/"+url.PathEscape(sha), 0, &v); err != nil {
			return time.Time{}, err
		}
		if v.Tagger.Date.IsZero() {
			return time.Time{}, fmt.Errorf("%s tag object %s: no tagger date", repo, sha)
		}
		return v.Tagger.Date, nil
	}
	var v struct {
		Commit struct {
			Committer struct {
				Date time.Time `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	if _, err := c.getJSON(ctx, repoPath(repo)+"/commits/"+url.PathEscape(sha), 0, &v); err != nil {
		return time.Time{}, err
	}
	if v.Commit.Committer.Date.IsZero() {
		return time.Time{}, fmt.Errorf("%s commit %s: no committer date", repo, sha)
	}
	return v.Commit.Committer.Date, nil
}

// Exists runs a direct existence check (docs/polling.md#deletion-detection): the
// release by tag, or for kind "tag" the tag ref. A 404 is (false, nil).
func (c *Client) Exists(ctx context.Context, repo, kind, tag string) (bool, error) {
	p := repoPath(repo) + "/releases/tags/" + refPath(tag)
	if kind == "tag" {
		p = repoPath(repo) + "/git/ref/tags/" + refPath(tag)
	}
	_, err := c.get(ctx, p, 0, c.token(), true)
	switch {
	case errors.Is(err, ErrNotFound):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// Entry is one entry of a directory listing.
type Entry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Type string `json:"type"` // file | dir | symlink | submodule
}

// contentsPath is the contents URL of path at ref; an empty ref is the
// default branch.
func contentsPath(repo, path, ref string) string {
	p := repoPath(repo) + "/contents"
	if path != "" {
		p += "/" + refPath(path)
	}
	if ref != "" {
		p += "?ref=" + url.QueryEscape(ref)
	}
	return p
}

// Dir lists the repo's root directory at ref, or at the default branch if
// ref is empty. Only the default branch, which moves, uses the ETag cache;
// a tag's tree never changes and is read once.
func (c *Client) Dir(ctx context.Context, repo, ref string) ([]Entry, error) {
	resp, err := c.get(ctx, contentsPath(repo, "", ref), 0, c.token(), ref == "")
	if err != nil {
		return nil, err
	}
	var out []Entry
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, fmt.Errorf("%s: listing the root directory: %w", repo, err)
	}
	return out, nil
}

// File returns the contents of the file at path and ref (the default branch
// if ref is empty), caching like Dir.
func (c *Client) File(ctx context.Context, repo, path, ref string) (string, error) {
	resp, err := c.getAs(ctx, contentsPath(repo, path, ref), mediaRaw, 0, c.token(), ref == "")
	if err != nil {
		return "", err
	}
	return string(resp.Body), nil
}

// Comparison is the commit range between two refs.
type Comparison struct {
	Status       string // ahead | behind | diverged | identical
	TotalCommits int
	Commits      []Commit // oldest first; the API returns at most 250
}

// Commit is one commit of a comparison.
type Commit struct {
	SHA        string
	Message    string
	Login      string // the GitHub author, if linked
	AuthorType string // User | Bot, if linked
	AuthorName string // the git author name
	Parents    int
}

// Compare compares base...head (docs/notes.md#commit-log-fallback).
func (c *Client) Compare(ctx context.Context, repo, base, head string) (*Comparison, error) {
	var v struct {
		Status       string `json:"status"`
		TotalCommits int    `json:"total_commits"`
		Commits      []struct {
			SHA    string `json:"sha"`
			Commit struct {
				Message string `json:"message"`
				Author  struct {
					Name string `json:"name"`
				} `json:"author"`
			} `json:"commit"`
			Author *struct {
				Login string `json:"login"`
				Type  string `json:"type"`
			} `json:"author"`
			Parents []json.RawMessage `json:"parents"`
		} `json:"commits"`
	}
	resp, err := c.get(ctx, repoPath(repo)+"/compare/"+refPath(base)+"..."+refPath(head), 0, c.token(), false)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(resp.Body, &v); err != nil {
		return nil, fmt.Errorf("%s: comparing %s...%s: %w", repo, base, head, err)
	}
	out := &Comparison{Status: v.Status, TotalCommits: v.TotalCommits}
	for _, cm := range v.Commits {
		x := Commit{SHA: cm.SHA, Message: cm.Commit.Message, AuthorName: cm.Commit.Author.Name, Parents: len(cm.Parents)}
		if cm.Author != nil {
			x.Login, x.AuthorType = cm.Author.Login, cm.Author.Type
		}
		out.Commits = append(out.Commits, x)
	}
	return out, nil
}

// GuardError is a token that fails the token guard: it can obviously read
// private repos (docs/public-only.md).
type GuardError struct{ Reason string }

func (e *GuardError) Error() string { return "github token rejected: " + e.Reason }

// Guard runs the best-effort token guard on token (docs/public-only.md). A *GuardError is a
// definite failure; any other error means the check couldn't run. A zero
// token passes: unauthenticated requests can't read private repos.
func (c *Client) Guard(ctx context.Context, token secret.String) error {
	if token.IsZero() {
		return nil
	}
	resp, err := c.get(ctx, "/user/repos?visibility=private&per_page=1", 0, token, false)
	var se *StatusError
	switch {
	case errors.As(err, &se) && (se.Code == http.StatusUnauthorized || se.Code == http.StatusForbidden):
		return &GuardError{Reason: fmt.Sprintf("GitHub refused it (%d %s)", se.Code, http.StatusText(se.Code))}
	case errors.Is(err, ErrNotFound):
		return &GuardError{Reason: "GET /user/repos returned 404"}
	case err != nil:
		return err
	}
	for _, s := range strings.Split(resp.Header.Get("X-OAuth-Scopes"), ",") {
		if strings.TrimSpace(s) == "repo" {
			return &GuardError{Reason: "it is a classic token with the repo scope, which can read private repos; use a fine-grained token with \"Public repositories (read-only)\" access"}
		}
	}
	var repos []json.RawMessage
	if err := json.Unmarshal(resp.Body, &repos); err != nil {
		return fmt.Errorf("token guard: decoding /user/repos: %w", err)
	}
	if len(repos) > 0 {
		return &GuardError{Reason: "it can list private repos; use a fine-grained token with \"Public repositories (read-only)\" access"}
	}
	return nil
}
