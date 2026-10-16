# Polling GitHub

The poll stage discovers new versions of each repo and records them as
`release` rows. Every step below runs only for repos that passed this tick's
visibility check ([Public repositories only](public-only.md)).

## Releases

`GET /repos/{owner}/{repo}/releases?per_page=30` (page 1) is sent with
`If-None-Match`. When authenticated, a 304 doesn't count against the rate
limit.

### Catch-up pagination (atomic)

Each repo persists a **scan boundary**: the newest tag of its last completed
scan. If page 1 doesn't contain the boundary, which happens after downtime or
a burst of releases, pages 2…`max_pages` (default 5) are fetched back-to-back
until the boundary appears, the cap is reached, or the list ends.

- **All or nothing.** Results are buffered in memory and committed in one
  transaction only when the scan completes. Then the new rows are inserted and
  the boundary advances to the newest tag seen on page 1.
- **On interruption** (an error, a rate limit, a restart), nothing is written
  and the boundary stays put. The next tick starts again from page 1. With no
  stored mid-scan position, pagination shifts *between* ticks can't cause
  skips.
- **Forward progress comes from bounding the work, not from checkpoints.** A
  full scan is at most `max_pages` list requests. The poll budget is checked
  only *between* repos, so a scan, once started, runs to completion or
  failure. Only persistent GitHub failure can stall it, and that's reported in
  `repo_state.last_error`.
- Versions past the cap are **intentionally omitted** and a warning names the
  repo and the number of pages scanned.

An earlier design checkpointed mid-scan positions. It could stall or skip
entries, which is why catch-up is atomic.

### Daily reconciliation

Pages aren't a snapshot. If entries are deleted between page requests, an
unread entry can slide onto an already-read page and be missed. The scan
doesn't try to detect that. Instead, once a day per repo, a reconciliation
scan reads `max_pages + 5` pages (10 by default, 300 entries) regardless of
the boundary. It catches anything missed for any reason: shifts during a
scan, bugs, a repo that was misconfigured for a while.

Reconciliation only inserts missing rows, idempotently, and never moves the
boundary. So it isn't atomic: it checks the poll budget between pages,
persists `reconcile_next_page`, and resumes there next tick.

A version it finds that isn't stored is inserted with `origin = reconcile`.
It's `pending` only if it was published within the **publish window**
(`max(7d, notify.max_age)`) **and** after the repo's `initialized_at`.
Otherwise it's `skipped`. Releases that predate tracking never become pending
this way, so the first-sight backfill limit holds, and a release that's still
alert-eligible is never reconciled as `skipped`.

**Coverage bound (accepted).** A missed entry is lost only if a deletion
displaced it during a scan *and* more than 150 newer releases landed before
the next reconciliation. GitHub's list API has no cursor or snapshot, so no
client-side scheme can fully close this gap.

## Tags

If a repo has no releases, or `sources: [tags]` is set, the stage reads
`GET /repos/{owner}/{repo}/git/matching-refs/tags/` with `If-None-Match`. It
returns **every** tag in one unpaginated response (golang/go has about 500
tags; kubernetes about 1,250, 520 KB), and an unchanged list is a 304.

The paginated `/tags` endpoint can't be used: GitHub sorts it by name, not
date, so golang/go's page 1 is all `weekly.2011-*` tags and `v1.9.0` sorts
above `v1.10.0`.

Because the list is complete, any tag not stored is new, and tag-only repos
need no scan boundary, catch-up or reconciliation. On first sight, the
`backfill` versions queued are the **highest** by SemVer precedence, since the
list has no dates. Tags without a version can't be ranked then and are
skipped.

### Dating tags

New tag rows are inserted without a date. Dates are resolved afterwards from
a persisted **metadata queue**, drained within the poll budget, at most 30 per
tick:

- a lightweight tag gets its commit's committer date, from
  `GET /repos/{owner}/{repo}/commits/{sha}`;
- an annotated tag gets its tagger date, from
  `GET /repos/{owner}/{repo}/git/tags/{sha}`, which is when it was actually
  tagged.

The queue is ordered by last attempt, so lookups that keep failing rotate to
the back. A tag row isn't eligible for summarizing or rendering until its
date is resolved, and the date is frozen once set. Skipped rows with a parsed
version are never dated, since nothing shows them and SemVer predecessors
don't need dates.

### Switching lists

`repo_state.scan_mode` records which list the last completed scan read. When a
repo switches, because its releases were all deleted or it published its first
release, the new list gets first-sight treatment. Otherwise years of old tags,
or `max_pages` of old releases, would flood the feed.

## Filtering

Each version is classified ([Versions](versions.md)), then filtered by
`include_prereleases`, by drafts (always excluded) and by the optional
`tag_pattern` / `exclude_pattern` regexes, such as skipping `nightly-*`.
Filtered versions are recorded as `skipped`, which keeps them available as
predecessors.

Each unseen, unfiltered version is inserted in state `pending`. Every row
records its **origin**: `live` (normal discovery), `backfill` (first sight),
`catchup` or `reconcile`.

## First sight of a repo

Only page 1 is read, and only its newest `backfill` versions (default 3) are
queued. The rest of page 1 is recorded as `skipped`, so it's never summarized
but can serve as predecessors. That keeps the LLM bill for adding a repo
predictable.

`repo_state.initialized_at` is set to the first scan's **start time**,
captured before page 1 is requested, and only once that scan completes. A
release on page 1 published after the start time is `origin = live`: it
doesn't count toward the backfill quota and is alert-eligible like any normal
discovery. An interrupted first poll is redone from scratch.

## Deletion detection

Disappearing from a list response is never enough to mark a release deleted:
lists are paginated and windowed. Deletion is decided only by a **direct
existence check**, `GET /repos/{o}/{r}/releases/tags/{tag}` for releases or
`GET /repos/{o}/{r}/git/ref/tags/{tag}` for tags. Two things trigger one:

1. **Window candidates.** A stored release newer than the oldest entry on
   page 1 but absent from it should have been there, so it's checked in the
   same poll. If page 1 is empty, every stored published release is a
   candidate. For tag-only repos, every visible tag missing from the complete
   list is a candidate. At most 30 per poll.
2. **Rotating sweep.** Each tick, up to `github.verify_per_tick` (10)
   published releases across all repos are checked, ordered by the time of the
   last *attempt*, oldest first. With 200 items and an hourly tick, every item
   is checked within a day.

Outcomes:

- **404** sets `missing_since`.
- **Success** sets `last_verified_at` and clears `missing_since`.
- **Anything else** (an error, a rate limit, a timeout) only records the
  attempt. Because the sweep orders by attempt time, items that keep failing
  rotate to the back instead of hogging the sweep.

A list appearance also clears `missing_since`. A release missing for 7 days
drops out of the feed; the grace period allows for a re-publish in progress.

## Rate limits

A token is optional but strongly recommended: 60 requests an hour without one,
5,000 with. The client reads `X-RateLimit-Remaining`, `X-RateLimit-Reset` and
`Retry-After`. If the reset is under 2 minutes away, it waits. Otherwise it
ends the poll stage early, and the round-robin cursor resumes there next tick.
