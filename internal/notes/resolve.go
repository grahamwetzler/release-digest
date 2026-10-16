package notes

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"release-digest/internal/github"
)

// ChangelogFiles are searched, in order and ignoring case, at the root of
// the repo when the changelog setting is auto (§3.2).
var ChangelogFiles = []string{"CHANGELOG.md", "CHANGES.md", "HISTORY.md", "NEWS.md"}

// Request is a release to resolve notes for.
type Request struct {
	Repo, Tag string
	Stream    string // the tag's stream prefix (§3.1.1)
	Version   string // normalized; "" for a date-ordered tag, which has no changelog section
	PrevTag   string // "" if unknown: no compare fallback
	Body      string // the release body; "" for a tag
	Changelog string // auto | off | a repo-relative path
}

// Result is resolved notes. An empty Source means the release has none.
type Result struct {
	Source, Text string
}

// Resolver resolves notes against GitHub. A Resolver remembers the default
// branch's changelog per repo, so use a new one for each pass.
type Resolver struct {
	GH       *github.Client
	MaxChars int // llm.max_input_chars

	heads map[string]string // repo and setting → default-branch changelog text
}

// FromBody resolves notes without any request when the body is substantive.
func FromBody(body string, maxChars int) (Result, bool) {
	body = strings.TrimSpace(body)
	if !Substantive(body) {
		return Result{}, false
	}
	return Result{SourceBody, Truncate(body, maxChars)}, true
}

// Resolve picks the notes for req (§3.2), in order: a substantive body; the
// changelog section; a body that pointed at a changelog without one, if it
// is long enough to stand alone; the commit log since the predecessor; a
// short body that isn't just a pointer; else none. Any error but a 404 is
// returned, so a transient failure never resolves to "no notes".
func (r *Resolver) Resolve(ctx context.Context, req Request) (Result, error) {
	body := strings.TrimSpace(req.Body)
	if res, ok := FromBody(body, r.MaxChars); ok {
		return res, nil
	}
	sec, err := r.changelog(ctx, req)
	if err != nil {
		return Result{}, err
	}
	if sec != "" {
		return Result{SourceChangelog, Truncate(sec, r.MaxChars)}, nil
	}
	s := substance(body)
	if utf8.RuneCountInString(s) >= MinBody {
		return Result{SourceBody, Truncate(body, r.MaxChars)}, nil
	}
	log, err := r.compare(ctx, req)
	if err != nil {
		return Result{}, err
	}
	if log != "" {
		return Result{SourceCompare, Truncate(log, r.MaxChars)}, nil
	}
	if s != "" && !pointer.MatchString(s) {
		return Result{SourceBody, Truncate(body, r.MaxChars)}, nil
	}
	return Result{}, nil
}

// versionText is the tag's version as a changelog would write it: the tag
// without its stream prefix or a leading "v".
func versionText(req Request) string {
	if req.Version == "" {
		return ""
	}
	rest := strings.TrimPrefix(req.Tag, req.Stream)
	return strings.TrimLeft(rest, "vV")
}

// changelog finds the release's changelog section at its tag, then at the
// default branch: some projects add the section after tagging, and a
// later recheck can then find it (§3.3, no_notes → fallback).
func (r *Resolver) changelog(ctx context.Context, req Request) (string, error) {
	v := versionText(req)
	if v == "" || req.Changelog == "off" {
		return "", nil
	}
	for _, ref := range []string{req.Tag, ""} {
		text, err := r.file(ctx, req.Repo, ref, req.Changelog)
		if err != nil {
			return "", err
		}
		if sec := Section(text, v, req.Tag); sec != "" {
			return sec, nil
		}
	}
	return "", nil
}

// file reads the changelog at ref ("" for the default branch); a missing
// file is "".
func (r *Resolver) file(ctx context.Context, repo, ref, setting string) (string, error) {
	key := repo + "\x00" + setting
	if ref == "" {
		if text, ok := r.heads[key]; ok {
			return text, nil
		}
	}
	path := setting
	if setting == "auto" {
		entries, err := r.GH.Dir(ctx, repo, ref)
		if errors.Is(err, github.ErrNotFound) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		path = pick(entries)
	}
	text := ""
	if path != "" {
		var err error
		text, err = r.GH.File(ctx, repo, path, ref)
		if errors.Is(err, github.ErrNotFound) {
			text = ""
		} else if err != nil {
			return "", err
		}
	}
	if ref == "" {
		if r.heads == nil {
			r.heads = map[string]string{}
		}
		r.heads[key] = text
	}
	return text, nil
}

// pick chooses the changelog among a root directory's entries.
func pick(entries []Entry) string {
	for _, name := range ChangelogFiles {
		for _, e := range entries {
			if e.Type == "file" && strings.EqualFold(e.Name, name) {
				return e.Path
			}
		}
	}
	return ""
}

// Entry is a directory entry, as the GitHub client returns it.
type Entry = github.Entry

// compare is the commit-log fallback. It is "" when there is no
// predecessor, either ref is gone, the range diverged, is empty or is too
// large, or every commit is noise (§3.2).
func (r *Resolver) compare(ctx context.Context, req Request) (string, error) {
	if req.PrevTag == "" {
		return "", nil
	}
	cmp, err := r.GH.Compare(ctx, req.Repo, req.PrevTag, req.Tag)
	if errors.Is(err, github.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if cmp.Status != "ahead" || cmp.TotalCommits > MaxCompareCommits {
		return "", nil
	}
	return CommitNotes(cmp.Commits), nil
}
