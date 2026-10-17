# The feed

The feed is rendered to bytes after any change and held in memory and in the
store, so a restart serves immediately. HTTP serves `/feed.xml` (RSS 2.0) and
`/feed.atom` (Atom 1.0) with `ETag` and `Last-Modified`, plus `/healthz`. The
code is in [`internal/render`](../internal/render), [`internal/feed`](../internal/feed)
and [`internal/server`](../internal/server).

## Identity and timestamps

- **GUID.** A UUIDv5 of `{owner}/{repo}@{tag}`, with the repo lowercased and
  the tag exact, under a fixed namespace compiled into the binary. RSS emits
  it with `isPermaLink="false"`; Atom emits `urn:uuid:{uuid}`. It's stable
  across release title changes, and it can't come out malformed whatever
  characters a tag contains (`/`, `+`, Unicode).
- **pubDate.** The release's `published_at`, or for a tag, the tagger date of
  an annotated tag or the committer date of a lightweight one. It's frozen
  once stored. A release deleted and re-created with the same tag keeps its
  original item.
- **Updated time.** `content_updated_at` is set when an item first becomes
  visible and advanced on every content change. It's the Atom `<updated>`, so
  readers that honor updates re-show an upgraded entry while `<published>`
  and the ID stay fixed. RSS 2.0 has no per-item updated element; readers that
  diff content see the new `<description>`.
- **Feed updated time.** The feed's Atom `<updated>` and RSS `lastBuildDate`
  are the newest item's `content_updated_at`. An empty feed uses the time the
  feed was first published, stored once, which keeps its bytes stable across
  ticks without an epoch date the W3C validator would call implausible.
- **Ordering.** By pubDate, newest first. Backfilled releases keep their real
  dates, so they sort into the past rather than flooding the top of the feed.

## What may change after publication

A published item's identity (GUID, link, pubDate) never changes. Its content
changes only in these cases, and each one advances its updated time:

- a visible-state transition ([Summaries](summaries.md#release-states)):
  `fallback` → `summarized`, `fallback` → `no_notes`, `no_notes` → `fallback`
- a change to its urgency marking ([Security alerts](security-alerts.md))
- an explicit `resummarize`
- a change to the item format: shortening summarized titles to `{repo}
  {version}` advanced every summarized item once

An item leaves the feed only through confirmed deletion (7 days after an
existence check returns 404, see [Polling GitHub](polling.md#deletion-detection))
or `feed.max_items` rollover.

## Item format

**Title:** `{repo} {version}`, for example `astral-sh/uv 0.9.4`. The
headline leads the description.

- A `⚠` prefix marks releases with breaking changes, and `🚨` urgent security
  releases.
- `(pre-release)` follows the version of a pre-release.
- A noise-only release shows its headline and "No user-facing changes."
  instead of the lists.

**Link:** the GitHub release page, or `…/releases/tag/{tag}` for a tag.

**Description** (sanitized HTML):

```html
<p><strong>Faster lockfile resolution for workspaces</strong></p>
<h4>Breaking changes</h4><ul><li>…</li></ul>
<h4>Features</h4><ul><li>…</li></ul>
<h4>Fixes</h4><ul><li>…</li></ul>
<h4>Security</h4><ul><li>…</li></ul>
<p><a href="…">Full release notes</a> · <a href="…/compare/v0.9.3...v0.9.4">Diff since 0.9.3</a></p>
<p><small>Summarized by qwen3:14b from the release notes.</small></p>
```

Empty sections are omitted. The footer names the model and the notes source
(release notes, `CHANGELOG.md` or commit log) for transparency.

- A `fallback` item shows an excerpt of the notes, the same links, and
  "Summary pending. Excerpt from the release notes" (or the changelog, or the
  commit log).
- A `no_notes` item says "{repo} {version} released — no release notes
  published" above the links.

**Categories:** `owner/repo`, plus `breaking`, `security`, `security-urgent`
and `prerelease` when they apply, so readers can filter the combined feed by
repo.

## Validation

`release-digest validate` checks the stored feed locally; `--w3c` also posts
it to the W3C Feed Validation Service. Golden RSS and Atom files cover the
renderer in tests.
