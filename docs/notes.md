# Release notes

Notes resolution produces the raw text the LLM sees. It's deterministic and
unit-tested against fixtures. The code is in [`internal/notes`](../internal/notes).

## Sources

The rows are tried in order:

| Situation | Notes source |
|---|---|
| Release with a substantive body | The release body (Markdown) |
| Release body empty, under 40 chars, or matching `see (the )?changelog\|CHANGELOG\.md` | The matching section of the changelog file |
| Tag only | The matching section of the changelog file |
| No changelog section, but a body of 40+ chars that pointed at one | The release body |
| No changelog section, predecessor known | Commit subjects from `GET /compare/{prev}...{tag}` |
| A short body that isn't only a pointer, such as "Bug fixes." | The release body |
| None of the above | **No notes.** No LLM call; the item is published as `no_notes` |

Lengths are measured after removing GitHub's generated
`**Full Changelog**: <compare URL>` line, which is all a generated body has
when no pull requests were merged.

A body is "only a pointer" when every sentence in it mentions the changelog:
"See CHANGELOG.md for details." is, "Bug fixes. See CHANGELOG.md" isn't.

### Changelog lookup

The path is configurable per repo (`changelog`). The default, `auto`, lists
the repo's root and takes the first of `CHANGELOG.md`, `CHANGES.md`,
`HISTORY.md` and `NEWS.md`, ignoring case. `off` disables it.

The file is read at the tag's ref and, if it has no section there, at the
default branch, since some projects write the section after tagging. A tag's
tree never changes, so it's read once without the ETag cache; the default
branch's listing and changelog use the cache and are read at most once per
pass per repo.

The section is found by scanning Markdown headings outside fenced code for the
version, with or without a leading `v`, or for the whole tag (`go1.25.1`):

- The match must be a whole version: `1.2.3` doesn't match `1.2.30`, `11.2.3`
  or `1.2.3-rc.1`.
- Link targets in a heading are ignored, so `[1.2.3](…/compare/v1.2.2...v1.2.3)`
  isn't a `1.2.2` heading.
- ATX headings (`#`–`###`) and setext headings (underlined with `===` or
  `---`, levels 1 and 2) both count.
- A fence closes only on a run of the same character at least as long as the
  opening one.
- The section runs to the next heading of the same or higher level. An empty
  section counts as none.

Date-ordered tags have no version, so they skip the changelog.

### Commit-log fallback

Commit subjects come newest first, capped at 100, with merge commits,
`chore(deps)` / `build(deps)` subjects and bot authors dropped. The fallback
counts as "none of the above" when:

- the predecessor is unknown (a repo's first version),
- either ref is gone (404),
- the comparison is diverged, behind, identical or has more than 250 commits
  (an unrelated predecessor is likely), or
- every commit is filtered out as noise.

## Input budget

Notes are truncated to `llm.max_input_chars` (default 24,000) at a heading or
line boundary, with a `[truncated]` marker. Some changelogs, such as
Kubernetes', are enormous.

## Storage

Resolved text is stored once in `notes`, keyed by its SHA-256. The release row
records the hash and the source (`release_body`, `changelog` or `compare`).
The release body is stored the same way (`body_hash`), separately from the
notes chosen for it, so an edit can be detected and a later resolution can
still use it.

## The resolution queue

A substantive body is resolved when the row is inserted, with no request.
Every other dated `pending` or `fallback` row is queued. Within the poll
budget, after tag dating, up to 30 queued rows per tick are resolved in repos
confirmed this tick: never-attempted rows first, then the least recently
attempted.

- **Transient failures don't mean "no notes".** Any failure other than a 404,
  such as a 5xx or a timeout, records the attempt and leaves the row queued. A
  rate limit ends the stage without counting as an attempt.
- **Provisional notes.** Until it resolves, a release body that isn't
  substantive serves as provisional notes for a fallback excerpt. It's never
  summarized, so a body that only points at a changelog never reaches the LLM.
- **Edits.** A release body edited on a `pending`, `fallback` or `no_notes`
  row queues the row again, whatever its age. A `summarized` row only changes
  through `resummarize`. Visible rows keep their content until the new
  resolution completes.
- **Rechecks.** A `no_notes` release within the publish window
  (`max(7d, notify.max_age)`) is rechecked once a day, in case a changelog
  section appeared.
- **Outcomes.** Notes on a `no_notes` row move it to `fallback`. No notes on a
  `fallback` row move it to `no_notes`. A `pending` row with no notes is
  published as `no_notes` at the next render without waiting for
  `publish_after`, since there's no LLM call to wait for. New notes make a row
  due for summarizing at once.
