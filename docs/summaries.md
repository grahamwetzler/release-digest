# Summaries

The summarize stage turns resolved notes into a structured summary with any
OpenAI-compatible Chat Completions endpoint. The code is in
[`internal/llm`](../internal/llm) and [`internal/summarize`](../internal/summarize).

## Request and response

- **Protocol.** OpenAI Chat Completions. Config sets `base_url`, `model`,
  `temperature` (0.2) and `timeout`; the key is a [secret](secrets.md).
- **Structured output.** The request asks for JSON matching a fixed schema
  with `response_format: {type: "json_schema"}`. If the server rejects that, as
  some Ollama versions and older endpoints do, the client retries with
  `{type: "json_object"}` and then with plain prompt instructions. A rejection
  is a 400 or 422 whose message mentions the response format; any other 400 is
  an ordinary failure. The client remembers the format that worked. In
  prompt-only mode the schema goes in an extra user message, which isn't part
  of the cache key.
- **Schema.**

  ```json
  {
    "headline": "string, ≤ 100 chars, no version number",
    "breaking": ["string"],
    "features": ["string"],
    "fixes":    ["string"],
    "security": ["string"],
    "noise_only": false,
    "security_urgency": "none | advisory | urgent",
    "security_reason": "string"
  }
  ```

  The prompt says to omit dependency bumps, CI or docs-only changes and
  contributor thank-yous. `noise_only: true` means nothing user-facing, which
  renders as a one-line item rather than being hidden, so the feed still
  shows the version shipped. The urgency fields are explained in
  [Security alerts](security-alerts.md#llm-signal).
- **Validation.** A code fence around the JSON is tolerated. Every field is
  required; a missing or null one is invalid, as is an empty headline or one
  over 100 characters. List entries are trimmed, empty ones dropped, and each
  list is cut to 6. A reply that lists any change is never `noise_only`,
  whatever the field says. One repair retry is allowed: the invalid reply goes
  back to the model with the reason it failed.
- **No model, no summaries.** An empty `llm.model` turns the stage off. The
  feed then shows excerpts only, and startup logs a warning.

## Prompt

A default system prompt is built into the binary; `llm.prompt_file` replaces
it. The file is re-read at the start of every summarize stage, so an edit
applies on the next tick.

The user message gets the repo, the tag, the previous tag (or "unknown"), the
notes source and the notes inside a `<notes>` block. The system prompt tells
the model the notes are data, not instructions.

### Per-repo instructions

A repo's `instructions` (or the one in `defaults`; a repo's own value, even
`""`, replaces it) tell the model what matters to the feed's owner, such as
"lead with CLI changes". They go in an `<instructions>` block before the notes
and outside them.

They're limited to the headline and to choosing which breaking changes,
features and fixes to include and in what order. They never allow claims the
notes don't make, and they **don't apply to the security fields**: the model
lists every security fix and rates `security_urgency` as it would without
them. An LLM `urgent` counts only with a non-empty `security` list, so
instructions that narrowed that list could otherwise suppress an alert. The
advisory signal and the alert rules don't read them.

They come from the config, so they're trusted. The limit is 2000 characters.

## Caching

The summary key is the SHA-256 of a canonical JSON document holding
everything that shapes the output:

- `base_url` and `model`
- `temperature`
- the response schema version
- the fully rendered request messages, which embed the effective prompt text
  (built-in or the *contents* of `prompt_file`), the repo, the version,
  `prev_tag`, the repo's `instructions`, the notes source and the notes

Two releases share a summary only if the LLM would have seen byte-identical
requests. Editing the prompt, the instructions or the model changes the key
for *future* summaries only; published `summarized` items aren't re-billed
unless `resummarize` asks for it.

## Retries and backoff

- `llm.concurrency` (default 2) calls run at once.
- Within one attempt, the client retries 429 and 5xx up to 3 times with
  jittered exponential backoff, waiting at least as long as `Retry-After`. A
  server that asks for more than a minute fails the attempt instead.
- A failed attempt increments `attempts`, records `last_error`, and sets
  `next_attempt_at = now + min(15m × 2^attempts, 24h)`, so a broken LLM isn't
  hammered every tick. Backoff survives restarts. A success resets it. A call
  cut short by shutdown isn't counted.
- Errors are redacted with the key the request was sent with, so a `SIGHUP`
  rotation mid-request can't leak the old key.

## Eligibility and the gate

The stage takes `pending` and `fallback` rows whose notes are resolved and
non-empty, that are dated, not marked deleted, due by `next_attempt_at`, and
in repos confirmed this tick.

The repo must pass the just-in-time public check
([Public repositories only](public-only.md)) before each LLM HTTP request
(including retries, format fallbacks and the repair retry), again after the
call, which can take minutes, and before publishing a summary that's already
stored. The final state update is conditional, in the same statement, on the
repo still being confirmed and not held, so a concurrent worker's failed check
stops it. A summary whose second check fails stays stored, unpublished, and is
published without a new call once the repo is confirmed again. A gate failure
is not an attempt.

A summarized `pending` row is published at once, without waiting for
`publish_after`. The update is conditional on the notes hash it summarized,
so notes that changed during the call are summarized again rather than
overwritten.

## Release states

Eligibility comes from this table and nowhere else.

| State | In feed? | Summarize-eligible? | Transitions |
|---|---|---|---|
| `skipped` | no | no | Terminal: backfill, filter or catch-up overflow |
| `pending` | no | yes, once `next_attempt_at ≤ now`; new rows are due immediately | → `summarized` on success; → `fallback` at render once `now − first_seen_at ≥ llm.publish_after` (6h); → `no_notes` at render once notes resolve to none |
| `fallback` | yes, excerpt | yes, once due; no age cutoff | → `summarized` on success; → `no_notes` when resolution completes and finds no notes (render promoted it before resolution finished). A transient resolution failure isn't "no notes" |
| `summarized` | yes | only when flagged by `resummarize` | Re-summarized only by `resummarize` |
| `no_notes` | yes, deterministic | no | → `fallback` when a later poll finds notes |

- **Visible states.** `fallback`, `summarized` and `no_notes` are all visible.
  Every transition between them keeps the item in the feed and advances its
  updated time ([The feed](feed.md)).
- **No age cutoff for fallbacks.** The backoff cap already limits a long LLM
  outage to one attempt per item per day. When the LLM comes back, every
  fallback is upgraded, oldest first, within `max_per_pass`.
- **Fallback content.** About the first 600 characters of the notes as plain
  text, plus a note that the summary is pending.

## Resummarize

`release-digest resummarize [--repo owner/repo] [--since 30d|all]` records the
request time on matching `summarized`, `fallback` and `pending` rows that are
dated and not deleted, and resets their backoff. It only marks rows; the CLI
makes no calls and needs neither GitHub nor the LLM.

The summarize stage takes flagged rows after unflagged ones, so a large
resummarize doesn't delay new releases. The gate, budgets and `max_per_pass`
still apply.

- **A separate key.** The new answer is stored under the request's key plus
  the request time, not the published key. The feed reads a row's summary
  through `summary_key`, so overwriting the published summary would expose new
  content before the final gate. The row switches atomically with the gate, and
  until then the published summary stays exactly as it is.
- **Gated answers aren't re-billed.** A fresh summary whose publication was
  gated is found again under the same derived key and published later without
  a second call.
- **Requests aren't lost.** Publishing clears the request only if it's still
  the one the pass served, so a second request that arrives during the call
  gets its own call. A failure keeps the request and backs off.
- **Alerts aren't re-sent.** The new summary marks urgency for recompute like
  any other, and the outbox's once-per-GUID key means a sent alert is never
  sent again.
