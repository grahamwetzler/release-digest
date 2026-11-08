# Security alerts

When a release fixes a serious security issue, a feed item is too easy to
miss. Release Digest marks such releases **urgent** in the feed and, if
`notify.ntfy` is configured, pushes an alert to an [ntfy](https://ntfy.sh)
topic. The code is in [`internal/advisory`](../internal/advisory) and
[`internal/notify`](../internal/notify).

## Detection

A release is urgent when either of two signals fires. The advisory signal
works without the LLM, so `fallback` items are covered too.

### Advisory signal

GHSA IDs and CVE IDs in the resolved notes, at most 20 per release in order of
mention, are looked up through GitHub's global advisory API
(`GET /advisories/{ghsa_id}` or `GET /advisories?cve_id=…`). The signal fires
if any linked advisory:

- has severity ≥ `notify.min_severity` (default `high`),
- isn't withdrawn,
- **matches this repo**, and
- is **fixed by this release**.

An LLM can't talk this signal down.

**Matching a repo.** An advisory matches when its `source_code_location`
normalizes to `https://github.com/{owner}/{repo}`, or when one of the repo's
configured `packages` (`{ecosystem, name}` pairs) appears in the advisory's
`vulnerabilities[].package`. `packages` covers packages named differently from
the repo, and monorepos. An unmatched advisory is logged and doesn't fire, so
a CVE in an unrelated dependency never marks the release.

**Fixed by this release.** Notes also cite unfixed and upstream CVEs, so a
mention isn't enough. A matched advisory counts only on positive evidence:

1. **Version evidence (preferred).** The release is ≥ the matched package's
   `first_patched_version`, **and** its predecessor is known and falls inside
   `vulnerable_version_range`: this is the release that crossed from
   vulnerable to patched. If the predecessor is unknown, either tag has no
   version, or the advisory gives no patched version and range, version
   evidence is unavailable. When it's available and says no, text evidence
   doesn't apply.
2. **Text evidence (only when version evidence is unavailable).** The bullet
   or line containing the whole advisory ID (`CVE-2026-12345` doesn't mention
   `CVE-2026-1234`; IDs in Markdown emphasis count) must pass an allowlist and
   then a denylist.
   - **Allowlist.** The line either **starts**, after list markers and an
     optional `Security:` / `[security]` prefix, with a fix verb (`Fix`,
     `Fixes`, `Patch`, `Patches`, `Resolve`, `Resolves`, `Address`,
     `Addresses`, `Mitigate`, `Mitigates`), or contains a completed form
     linked to the ID (`fixed`, `patched`, `resolved`, `addressed`,
     `mitigated`). Any other phrasing fails closed.
   - **Denylist.** No negation, hedge or future marker appears anywhere on the
     line: `not`, `no`, `never`, `without`, `unfixed`, `still`, `partially`,
     `pending`, `planned`, `will`, `should`, `next`, `upstream`,
     `workaround`, `known issue` and others; any word ending in `n't`; and the
     apostrophe-less forms such as `isnt` and `dont`. Curly apostrophes are
     normalized first.

   That rejects "We will fix CVE-… in the next release", "a fix is planned for
   GHSA-…" and "CVE-… isn't fixed yet". The rule is deliberately conservative:
   a false negative only drops this signal, while a false positive sends a
   wrong alert.

The result is stored on the link (`release_advisories.matched` and `fixes`)
and re-evaluated on every recompute, since both the advisory and the config
can change.

**Advisory cache.** Advisories are cached with a fetch time and refreshed from
their own bounded queue, up to 20 lookups a tick, never-fetched IDs first:

- every 24h while a linked release is within `max_age`;
- then weekly for as long as a linked release is visible in the feed,
  whatever its current urgency, so a later severity upgrade or withdrawal
  still updates the marking;
- a not-found advisory (a brand-new GHSA) is retried after 1h;
- transient errors back off from 5m to 1h.

Notes are scanned for IDs when they first resolve and again whenever they
change; links to IDs no longer mentioned are removed.

### LLM signal

The summary includes `security_urgency` (`none`, `advisory` or `urgent`) and
`security_reason`. The prompt defines `urgent` narrowly: the notes describe a
fixed vulnerability that is remote code execution, an authentication or
authorization bypass, known exploitation, or an explicit "upgrade
immediately". Ordinary hardening and dependency CVE bumps are `advisory`.

`urgent` only counts if the summary's `security` list is non-empty **and** the
notes contain a security keyword (`security`, `vulnerab`, `CVE-`, `GHSA-`,
`exploit`). Otherwise it's downgraded to `advisory` and logged.

### Deterministic cross-check

When no advisory link fires, a summary exists, its `security` list is empty,
and its urgency is `none`, a deterministic scanner checks the resolved notes.
It recognizes whole CVE and GHSA IDs and vulnerability phrases, including
security fixes, advisories and issues, remote code execution (RCE), privilege
escalation, cross-site scripting (XSS), SQL injection, path or directory
traversal, server-side request forgery (SSRF), and authentication or auth bypass.
Matching is case-insensitive, with Unicode letter/digit boundaries and flexible
whitespace. Results are canonicalized, deduplicated, and kept in mention order.
The scanner does not consult the LLM, so instructions in the notes cannot
persuade it to suppress a match.

These signals are **unconfirmed**: this is a keyword check, so it can
false-positive on notes that merely mention security, an unrelated issue, or
an unfixed vulnerability. It does not establish severity or that this release
fixes anything. It never changes urgency, its source, priority, or the existing
LLM downgrade rules; the feed marking stays the same. Each mismatch is logged
as a structured warning with `repo`, `tag`, `signals`, and `crosscheck`, even
when cross-check alerts are disabled.

`notify.crosscheck: true` opts in to ntfy messages for these mismatches. It is
**off by default** and still requires an ntfy URL. The title is
`Unconfirmed security signal: {owner/repo} {tag}`, priority is `3` (below urgent
alerts), and the plain body lists the canonical signals within the existing
body budget. The normal eligibility, visibility checks, retries, expiry,
and cancellation rules apply. Changing this option triggers recomputation at
startup, including cancellation of queued cross-check alerts when disabled.

Unconfirmed and urgent alerts share the existing outbox row keyed by release
GUID. Repeated recomputation queues no duplicate, and an unsent payload can be
rebuilt when the signals change. If an unconfirmed release becomes urgent,
the row is rebuilt into an urgent alert even if the unconfirmed alert was
already sent. If an urgent release is later de-escalated while its notes
still carry signals, a queued urgent alert is rebuilt as the priority-3
cross-check alert, so it neither delivers the stale urgent payload nor goes
silent. A sending urgent alert is left alone, since it is already being
posted and its completion must record an urgent delivery. A sent, cancelled
or expired urgent alert is left alone too; a cross-check never re-sends a
release. Further urgent decisions follow normal
deduplication.

## Urgency on the item

A release's `urgency` and its source are stored apart from its summary state.
The feed marking (a `🚨` title prefix and a `security-urgent` category)
follows `urgency`, whether or not ntfy is configured, and any change to it
advances the item's updated time.

Urgency is the maximum of the two signals, and it's **recomputed** whenever an
input changes. Each change sets `urgency_dirty` on the releases it affects:

- an advisory lookup, on every linked release
- a new summary, on its release
- a notes change, through the rescan of its advisory IDs
- a change to the config that urgency reads (`min_severity`, a repo's
  `packages`), on every release

The config is fingerprinted and the fingerprint stored. The config is read
only at startup, so a changed fingerprint at startup marks every release dirty
before the outbox worker starts.

The alert stage recomputes dirty releases in repos confirmed for the tick,
right after summarize, so a summary's decision lands in the tick that produced
it. Urgency can go down as well as up, for example when the only qualifying
advisory is downgraded or withdrawn.

## The outbox

A decision is written to the `notifications` outbox in the same transaction
as `urgency`. A separate worker goroutine delivers it, independent of the
scheduler tick.

- **Timing.** The worker wakes every 30s, or immediately when a row is
  enqueued, and sends at most 10 per wake, each with a 10s timeout.
- **Once per release.** Rows are keyed by GUID, so `resummarize`, restarts
  and state transitions deduplicate alerts. Escalation from an unconfirmed
  cross-check to urgent reuses the row and permits one urgent delivery,
  even after the unconfirmed alert was sent.
- **Claiming.** The worker moves a row from `queued` to `sending` in its own
  transaction. It skips rows whose release is dirty, has notes not yet scanned
  for advisory IDs, or is confirmed deleted, and rows whose repo is held. It
  then runs the just-in-time visibility check
  ([Public repositories only](public-only.md)). In a second transaction just
  before the POST, it re-reads the row and proceeds only if it's still
  `sending`, not cancelled, its repo isn't held, and its release still meets
  the claim's conditions. It sends the stored payload as is.
- **Every claimed row ends in exactly one state:**

  | Outcome | not cancelled | cancel requested |
  |---|---|---|
  | Visibility check fails, times out or is private | → `queued` with backoff (private or 404 also holds the repo) | → `cancelled` |
  | Pre-POST re-read finds it cancelled | — | → `cancelled`, no send |
  | POST returns 2xx | → `sent` | → `sent`; the race was lost, and it's logged |
  | POST fails or is ambiguous | → `queued` with backoff | → `cancelled`, no retry; an ambiguous POST may have been delivered, and that's logged |

- **De-escalation cancels unsent alerts** unless an enabled unconfirmed
  cross-check still qualifies the release; then a queued urgent alert is
  rebuilt as the cross-check alert, and a sending or terminal one is left
  unchanged. A
  `queued` row becomes
  `cancelled`; a `sending` row gets `cancel_requested`, and the table above
  decides.
- **Re-escalation** within `max_age` rebuilds the row's reason, payload and
  priority from the current signals. A `cancelled` row returns to `queued`; a
  `sending` row has its cancel request cleared. A re-queued alert never
  carries a withdrawn advisory's text. A `sent` urgent row is never touched.
  An unconfirmed cross-check in any state can become urgent, with fresh retry
  bookkeeping. If its POST is already in flight, completing that cross-check
  leaves the urgent alert queued; if escalation precedes the POST, only the
  urgent payload is delivered.
- **Crash safety.** A row left in `sending` goes back to `queued` at startup,
  or to `cancelled` if a cancel was requested.

### Delivery guarantee

Delivery is **at-least-once**. A row is marked `sent` only after a 2xx. If
ntfy accepted the POST but the response was lost, the retry can deliver a
duplicate. For an urgent security alert, a rare duplicate beats a lost one.

A failed delivery retries with backoff from 1m to a 1h cap until it succeeds
or the release passes `notify.max_age` (default 7d, from its publication
date). The next attempt is scheduled at `min(now + backoff, expiry − 2m)`, so
there's always a final attempt just before expiry. After that the row is
marked `expired`, logged at error level, and `/healthz` returns 503 until
`release-digest notify ack`. An outage that covers the whole window drops the
alert, but never silently.

### Who gets alerts

- A release alerts only if it was published after the repo's
  `initialized_at` and is within `max_age`, however it was found: a live poll,
  a catch-up or reconciliation. Adding a repo never produces an alert storm.
  `notify.include_backfill: true` lifts the first rule.
- Pre-releases alert only with `notify.include_prereleases: true`.
- Skipped and deleted releases never alert.
- A `fallback` item that turns urgent when its summary lands still alerts
  once, if it's within `max_age`.
- `build` decides urgency and queues alerts; only `run` delivers them.

## The ntfy message

One `POST {notify.ntfy.url}` per urgent release:

| Header | Value |
|---|---|
| `Title` | `🚨 {repo} {tag}: security release` (RFC 2047-encoded) |
| `Priority` | `5` for a critical advisory or an LLM `urgent`, otherwise `4` |
| `Tags` | `rotating_light`, the repo, and advisory IDs |
| `Click` | the GitHub release URL |
| `Actions` | a `view` action for each firing advisory, at most 2 |
| `Markdown` | `yes` |

The body is Markdown within a **3,500-byte budget**, below ntfy's 4,096-byte
limit, past which ntfy turns the message into an attachment. It's assembled in
priority order and stops before the budget is exceeded: the headline, the
advisory severity and ID, `security_reason` (capped at 300 bytes), the
`security` bullets, then advisory summaries. Truncation is at a rune boundary
with `…`. The links are in headers, so they always survive. The full notes are
never sent.

An ntfy access token is sent as `Authorization: Bearer`. On public ntfy.sh the
topic name acts as a password, so the URL can be kept out of the YAML as a
[secret](secrets.md), and it never appears in logs or errors. `config lint`
warns when the URL is on ntfy.sh with no token.

## Testing it

`release-digest notify test` sends a sample alert through the configured path.
`release-digest notify test --release owner/repo@tag` runs detection on a real
release and prints the decision, the signals and the payload, without sending.
It resolves the notes from GitHub, writes nothing, and with a model configured
also runs the LLM, under the same just-in-time checks.
