# Architecture

```
            ┌──────────── scheduler (interval, jitter) ────────────┐
            ▼                                                       │
  config ─► Poll ──► Resolve notes ──► Summarize ──► Store ──► Render ──► HTTP
  (repos)   GitHub   release body /     LLM (once,    SQLite    RSS/Atom   /feed.xml
            API      CHANGELOG.md /     hash-keyed)             bytes      /feed.atom
                     compare commits
```

Release Digest is one long-lived process. Each scheduler tick runs its stages
in order:

1. a **visibility sweep** that confirms every repo is still public
   ([Public repositories only](public-only.md))
2. a **poll** stage that discovers releases and resolves their notes
   ([Polling GitHub](polling.md), [Release notes](notes.md))
3. a **summarize** stage that calls the LLM ([Summaries](summaries.md))
4. an **alert** stage that recomputes urgency and queues alerts
   ([Security alerts](security-alerts.md))
5. a **render** of the feed ([The feed](feed.md))

The HTTP server only reads the rendered bytes. The alert outbox worker is the
one other writer to the store; SQLite transactions take the write lock when
they begin, so the two never fail on a lock upgrade.

## Bounded passes

No single slow repo or flaky upstream can stall the feed.

- **Each stage has its own budget**, so neither can starve the other:
  `schedule.poll_budget` and `schedule.summarize_budget` (7m each by default).
  A stage that hits its budget stops, and its unfinished work stays persisted
  for the next tick. Summarize always runs after poll, whatever poll's
  outcome, so the LLM makes progress even if GitHub eats its full allowance
  every tick.
- **Every request has its own timeout**: `github.request_timeout` (20s) and
  `llm.timeout` (120s).
- **Budgets are checked between units of work**, so the most a stage overruns
  is one unit. For poll, the largest unit is one repo's atomic catch-up scan,
  at most (1 + `max_pages`) × `request_timeout`. Reconciliation, metadata and
  existence checks are checked page by page or item by item, and notes
  resolution between releases.
- **Render always runs**, outside both budgets, even when poll or summarize
  errored or ran out of time. Render is what promotes `pending` releases past
  `publish_after` to `fallback`, so publication never waits on the LLM.
- Render also runs after every 5 completed summaries, so a long summarize
  pass publishes as it goes.

## Visibility first

Every tick begins by checking *all* repos before any other work. Unchanged
repos return 304s, which cost no rate limit. The sweep runs 8 at a time, with
a 10s timeout per request and its own `schedule.sweep_budget` (2m). A repo not
confirmed when the budget runs out is **unverified** for this tick and is
simply retried next tick, so a few slow repos delay only themselves.

The sweep starts from a persisted cursor: the repo after the last one it
*attempted*, whatever the outcome. Repos that time out rotate to the back, so
every repo is reached within ⌈repos ÷ repos-per-sweep⌉ ticks even if some
always time out. A success is recorded as `visibility_confirmed_tick`.

## Fairness

After the sweep, repos' content is polled round-robin from a persisted
cursor, so a pass cut short by the budget or a rate limit picks up where it
stopped. Summaries are taken oldest `first_seen_at` first, at most
`llm.max_per_pass` (20) per pass, and releases flagged by `resummarize` come
after new ones.

## Store

SQLite through `modernc.org/sqlite`, which is pure Go, so builds need no CGO.
The schema and its migrations are in [`internal/store/store.go`](../internal/store/store.go).
The main tables:

| Table | Holds |
|---|---|
| `releases` | One row per discovered version: identity, state, notes and summary references, backoff, urgency, deletion bookkeeping |
| `notes` | Resolved notes and release bodies, keyed by SHA-256 |
| `summaries` | LLM answers, keyed by the hash of the request that produced them |
| `http_cache` | ETags and bodies for conditional GitHub requests |
| `repo_state` | Per-repo poll bookkeeping: initialization time, hold, visibility confirmation, scan boundary, reconciliation progress |
| `scheduler_state` | Round-robin cursors, the alert config fingerprint, the feed's first-publication time |
| `rendered` | The rendered RSS and Atom bytes, so a restart serves immediately |
| `advisories`, `release_advisories` | Cached GitHub advisories and their links to releases |
| `notifications` | The alert outbox |

Times are stored as Unix milliseconds.

## Package layout

```
cmd/release-digest/      the CLI: run, build, summarize, resummarize, notify, config lint, validate, healthcheck
internal/config/         YAML + env merge, defaults, validation
internal/secret/         secret.String (redacting), *_FILE / /run/secrets / env resolution
internal/github/         REST client: ETag cache, rate-limit pacing
internal/poll/           the visibility sweep and the poll stage
internal/version/        tag classification and predecessor selection
internal/notes/          notes resolution: body heuristics, changelog sections, compare fallback, truncation
internal/llm/            OpenAI-compatible client, response_format fallbacks, retries
internal/summarize/      prompt, schema validation and repair, cache key, the summarize stage
internal/advisory/       GHSA/CVE extraction, fix evidence, repo matching
internal/notify/         urgency decisions, the alert stage, payloads, the ntfy client, the outbox worker
internal/render/         stored releases to feed items
internal/feed/           RSS/Atom rendering and checks
internal/ident/          UUIDv5 GUIDs
internal/store/          SQLite and migrations
internal/scheduler/      interval + jitter, single-flight ticks
internal/server/         /feed.xml, /feed.atom, /healthz
```
