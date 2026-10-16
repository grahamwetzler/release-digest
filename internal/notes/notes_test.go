package notes

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"release-digest/internal/github"
	"release-digest/internal/github/githubtest"
)

const keepAChangelog = `# Changelog

All notable changes to this project will be documented in this file.

## [Unreleased]

### Added
- Something not yet released

## [1.2.3] - 2026-01-01

### Added
- New flag ` + "`--fast`" + `

### Fixed
- Crash on empty input

## [1.2.2] - 2025-12-01

### Fixed
- Typo

[1.2.3]: https://github.com/o/r/compare/v1.2.2...v1.2.3
`

func TestSection(t *testing.T) {
	for _, tc := range []struct {
		name, md, version, want string
	}{
		{"keep a changelog", keepAChangelog, "1.2.3",
			"## [1.2.3] - 2026-01-01\n\n### Added\n- New flag `--fast`\n\n### Fixed\n- Crash on empty input"},
		{"last section runs to the end", keepAChangelog, "1.2.2",
			"## [1.2.2] - 2025-12-01\n\n### Fixed\n- Typo\n\n[1.2.3]: https://github.com/o/r/compare/v1.2.2...v1.2.3"},
		{"v prefix and date", "## v1.2.3 (2026-01-01)\n\n- a\n\n## v1.2.2 (2025-01-01)\n\n- b\n", "1.2.3",
			"## v1.2.3 (2026-01-01)\n\n- a"},
		{"linked heading; links don't match", "# [1.2.3](https://x/compare/v1.2.2...v1.2.3) (2026-01-01)\n\n* a\n\n# [1.2.2](https://x/compare/v1.2.1...v1.2.2)\n\n* b\n", "1.2.2",
			"# [1.2.2](https://x/compare/v1.2.1...v1.2.2)\n\n* b"},
		{"setext ===", "Changes\n=======\n\n1.2.3\n=====\n\n- a\n\n1.2.2\n=====\n\n- b\n", "1.2.3",
			"1.2.3\n=====\n\n- a"},
		{"setext --- runs past deeper ATX", "1.2.3\n-----\n\n### Fixed\n\n- a\n\n1.2.2\n-----\n\n- b\n", "1.2.3",
			"1.2.3\n-----\n\n### Fixed\n\n- a"},
		{"not a prefix of a longer version", "## 1.2.30\n\n- x\n\n## 1.2.3\n\n- y\n", "1.2.3", "## 1.2.3\n\n- y"},
		{"not a pre-release", "## 1.2.3-rc.1\n\n- x\n\n## 1.2.3\n\n- y\n", "1.2.3", "## 1.2.3\n\n- y"},
		{"not inside another number", "## 11.2.3\n\n- x\n", "1.2.3", ""},
		{"pre-release itself", "## 2.0.0-rc.1\n\n- x\n", "2.0.0-rc.1", "## 2.0.0-rc.1\n\n- x"},
		{"headings in code fences don't count", "## 1.2.3\n\n```\n## 1.2.2\n```\n- a\n\n## 1.2.2\n\n- b\n", "1.2.3",
			"## 1.2.3\n\n```\n## 1.2.2\n```\n- a"},
		{"a shorter fence inside a longer one doesn't close it", "````md\n```\n## 1.2.3\n\n- example\n```\n````\n\n## 1.2.3\n\n- real\n", "1.2.3",
			"## 1.2.3\n\n- real"},
		{"tildes don't close backticks", "```\n~~~\n## 1.2.3\n\n- example\n```\n\n## 1.2.3\n\n- real\n", "1.2.3", "## 1.2.3\n\n- real"},
		{"level 4 doesn't match", "#### 1.2.3\n\n- a\n", "1.2.3", ""},
		{"empty section", "## 1.2.3\n\n## 1.2.2\n\n- b\n", "1.2.3", ""},
		{"thematic break isn't a heading", "## 1.2.3\n\n- a\n\n---\n\n- b\n\n## 1.2.2\n", "1.2.3", "## 1.2.3\n\n- a\n\n---\n\n- b"},
		{"CRLF", "## 1.2.3\r\n\r\n- a\r\n", "1.2.3", "## 1.2.3\n\n- a"},
		{"no version", keepAChangelog, "", ""},
		{"whole tag", "## go1.25.1\n\n- a\n", "go1.25.1", "## go1.25.1\n\n- a"},
		{"missing", keepAChangelog, "9.9.9", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Section(tc.md, tc.version); got != tc.want {
				t.Errorf("Section(%q) = %q, want %q", tc.version, got, tc.want)
			}
		})
	}
}

func TestSubstantive(t *testing.T) {
	for body, want := range map[string]bool{
		"":           false,
		"Bug fixes.": false,
		"See CHANGELOG.md for details on this release and everything in it.": false,
		"Please see the changelog.": false,
		"**Full Changelog**: https://github.com/o/r/compare/v1.2.2...v1.2.3":                                                                                         false,
		"## What's Changed\n* Fix crash on empty input by @a in https://github.com/o/r/pull/1\n\n**Full Changelog**: https://github.com/o/r/compare/v1.2.2...v1.2.3": true,
		"This release adds a --fast flag and fixes a crash on empty input.":                                                                                          true,
	} {
		if got := Substantive(body); got != want {
			t.Errorf("Substantive(%q) = %v, want %v", body, got, want)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := Truncate("short", 100); got != "short" {
		t.Errorf("under the limit: %q", got)
	}
	long := "## 1.0\n\n" + strings.Repeat("- line\n", 20) + "## Part two\n\n" + strings.Repeat("- more\n", 20)
	got := Truncate(long, 220)
	if !strings.HasSuffix(got, TruncatedMarker) || utf8.RuneCountInString(got) > 220 {
		t.Fatalf("Truncate = %q (%d chars)", got, utf8.RuneCountInString(got))
	}
	if strings.Contains(got, "Part two") || !strings.HasSuffix(strings.TrimSuffix(got, TruncatedMarker), "- line") {
		t.Errorf("want a cut at the heading: %q", got)
	}
	got = Truncate(strings.Repeat("é line\n", 50), 60)
	if utf8.RuneCountInString(got) > 60 || !utf8.ValidString(got) || !strings.HasSuffix(strings.TrimSuffix(got, TruncatedMarker), "é line") {
		t.Errorf("want a cut at a line: %q", got)
	}
}

func TestCommitNotes(t *testing.T) {
	got := CommitNotes([]github.Commit{
		{Message: "Add --fast flag\n\nLong description", Parents: 1},
		{Message: "Merge pull request #2 from a/b", Parents: 2},
		{Message: "chore(deps): bump x from 1 to 2", Parents: 1},
		{Message: "Update lockfile", Login: "renovate[bot]", AuthorType: "Bot", Parents: 1},
		{Message: "Regenerate", AuthorName: "github-actions[bot]", Parents: 1},
		{Message: "Fix crash (#3)", Parents: 1},
	})
	if want := "- Fix crash (#3)\n- Add --fast flag"; got != want {
		t.Errorf("CommitNotes = %q, want %q", got, want)
	}
	if got := CommitNotes([]github.Commit{{Message: "Merge branch 'main'", Parents: 2}}); got != "" {
		t.Errorf("all noise = %q", got)
	}
	var many []github.Commit
	for range 150 {
		many = append(many, github.Commit{Message: "x", Parents: 1})
	}
	if n := strings.Count(CommitNotes(many), "\n") + 1; n != MaxCommits {
		t.Errorf("%d subjects, want %d", n, MaxCommits)
	}
}

func resolver(t *testing.T) (*githubtest.Server, *Resolver) {
	gh := githubtest.New()
	t.Cleanup(gh.Close)
	return gh, &Resolver{GH: &github.Client{BaseURL: gh.URL, HTTP: http.DefaultClient, Timeout: 5 * time.Second, UserAgent: "test"}, MaxChars: 24000}
}

func TestResolve(t *testing.T) {
	ctx := context.Background()
	gh, res := resolver(t)
	repo := gh.Repo("o/r")
	repo.Files = map[string]map[string]string{
		"v1.2.3": {"changelog.md": keepAChangelog, "README.md": "# r"},
		"":       {"CHANGELOG.md": keepAChangelog + "\n## [1.2.4]\n\n- added after tagging\n"},
	}
	repo.Compares = map[string]githubtest.Comparison{
		"v1.2.4...v1.2.5":  {Status: "ahead", Commits: []githubtest.Commit{{Message: "Fix a"}, {Message: "Add b"}}},
		"v1.2.5...v1.2.6":  {Status: "diverged", Commits: []githubtest.Commit{{Message: "Fix a"}}},
		"v1.2.6...v1.2.7":  {Status: "ahead", Total: 251, Commits: []githubtest.Commit{{Message: "Fix a"}}},
		"v1.2.7...v1.2.8":  {Status: "ahead", Commits: []githubtest.Commit{{Message: "Merge", Parents: 2}}},
		"v1.2.8...v1.2.9":  {Status: "ahead", Commits: []githubtest.Commit{{Message: "Fix a"}}},
		"v1.2.9...v1.2.10": {Status: "ahead", Commits: []githubtest.Commit{{Message: "Fix a"}}},
	}
	req := func(tag, prev, body string) Request {
		return Request{Repo: "o/r", Tag: tag, Version: strings.TrimPrefix(tag, "v"), PrevTag: prev, Body: body, Changelog: "auto"}
	}
	long := "This release adds a --fast flag and fixes a crash on empty input."
	for _, tc := range []struct {
		name string
		req  Request
		src  string
		has  string
	}{
		{"substantive body", req("v1.2.3", "v1.2.2", long), SourceBody, "--fast flag and"},
		{"changelog at the tag, any case", req("v1.2.3", "v1.2.2", "See CHANGELOG.md"), SourceChangelog, "Crash on empty input"},
		{"changelog added after tagging", req("v1.2.4", "v1.2.3", ""), SourceChangelog, "added after tagging"},
		{"long body pointing nowhere", req("v1.2.5", "v1.2.4", long+" See the changelog."), SourceBody, "--fast"},
		{"compare", req("v1.2.5", "v1.2.4", ""), SourceCompare, "- Add b\n- Fix a"},
		{"diverged", req("v1.2.6", "v1.2.5", ""), "", ""},
		{"too large", req("v1.2.7", "v1.2.6", ""), "", ""},
		{"all noise", req("v1.2.8", "v1.2.7", ""), "", ""},
		{"no predecessor", req("v1.2.9", "", ""), "", ""},
		{"predecessor gone", req("v1.2.9", "v0.0.1", ""), "", ""},
		{"short body is the last resort", req("v1.2.9", "", "Bug fixes."), SourceBody, "Bug fixes."},
		{"a bare pointer is not", req("v1.2.9", "", "See CHANGELOG.md"), "", ""},
		{"nor a pointer sentence", req("v1.2.9", "", "See CHANGELOG.md for details."), "", ""},
		{"a short body with a pointer", req("v1.2.9", "", "Bug fixes. See CHANGELOG.md"), SourceBody, "Bug fixes."},
		{"changelog off", Request{Repo: "o/r", Tag: "v1.2.3", Version: "1.2.3", PrevTag: "v1.2.2", Changelog: "off"}, "", ""},
		{"date-ordered tags have no section", Request{Repo: "o/r", Tag: "1.2.3", Changelog: "auto"}, "", ""},
		{"explicit path", Request{Repo: "o/r", Tag: "v1.2.3", Version: "1.2.3", Changelog: "README.md"}, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := res.Resolve(ctx, tc.req)
			if err != nil {
				t.Fatal(err)
			}
			if got.Source != tc.src || !strings.Contains(got.Text, tc.has) {
				t.Errorf("Resolve = %+v, want source %q containing %q", got, tc.src, tc.has)
			}
		})
	}
}

func TestResolveMonorepoTag(t *testing.T) {
	gh, res := resolver(t)
	gh.Repo("o/r").Files = map[string]map[string]string{"api/v1.4.0": {"CHANGES.md": "# api 1.4.0\n\n- new endpoint\n"}}
	got, err := res.Resolve(context.Background(), Request{Repo: "o/r", Tag: "api/v1.4.0", Stream: "api/", Version: "1.4.0", Changelog: "auto"})
	if err != nil || got.Source != SourceChangelog || got.Text != "# api 1.4.0\n\n- new endpoint" {
		t.Errorf("Resolve = %+v, %v", got, err)
	}
}

func TestResolveReadsTheDefaultBranchOncePerPass(t *testing.T) {
	ctx := context.Background()
	gh, res := resolver(t)
	gh.Repo("o/r").Files = map[string]map[string]string{"": {"CHANGELOG.md": "## 1.0.1\n\n- b\n\n## 1.0.0\n\n- a\n"}}
	for _, v := range []string{"1.0.0", "1.0.1"} {
		got, err := res.Resolve(ctx, Request{Repo: "o/r", Tag: "v" + v, Version: v, Changelog: "auto"})
		if err != nil || got.Source != SourceChangelog {
			t.Fatalf("%s: %+v, %v", v, got, err)
		}
	}
	if n := gh.Count("/repos/o/r/contents/CHANGELOG.md"); n != 1 {
		t.Errorf("default-branch changelog read %d times, want 1", n)
	}
}

func TestResolveFailuresAreErrors(t *testing.T) {
	gh, res := resolver(t)
	gh.Repo("o/r").Status = http.StatusBadGateway
	_, err := res.Resolve(context.Background(), Request{Repo: "o/r", Tag: "v1.0.0", Version: "1.0.0", PrevTag: "v0.9.0", Changelog: "auto"})
	var se *github.StatusError
	if !errors.As(err, &se) {
		t.Errorf("err = %v, want the 502", err)
	}
}

func TestResolveTruncates(t *testing.T) {
	_, res := resolver(t)
	res.MaxChars = 1000
	got, err := res.Resolve(context.Background(), Request{Body: strings.Repeat("A substantive line of notes.\n", 100)})
	if err != nil || utf8.RuneCountInString(got.Text) > 1000 || !strings.HasSuffix(got.Text, TruncatedMarker) {
		t.Errorf("Resolve = %d chars, %v", utf8.RuneCountInString(got.Text), err)
	}
}
