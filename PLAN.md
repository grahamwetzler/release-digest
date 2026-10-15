# Release Digest — Planning Document

*Drafted 2026-10-15*

A Go service that watches a configured list of GitHub repositories for new versions and has an LLM condense each version's release notes into a structured TL;DR. It serves the results as one combined RSS feed. It runs as a single container in a Docker Compose stack, following the patterns of [feed-me](../feed-me).

---

## 1. Goals

1. **One summarized item per release.** Each new version of a watched repo becomes one feed item. The item has a short LLM-written TL;DR and a link to the full notes.
2. **Works on repos that don't do Releases well.** It uses GitHub Releases when a repo has them, falls back to tags when it doesn't, and pulls from `CHANGELOG.md` when the release body is empty or just says "see changelog".
3. **Any OpenAI-compatible LLM.** It speaks one protocol, `POST /v1/chat/completions`, so Ollama, LM Studio, vLLM, OpenRouter, OpenAI and Anthropic's OpenAI-compatible endpoint all work through config alone.
4. **Cheap and stable.** Each release is summarized **once** and cached by input hash, so re-running never re-bills the LLM. A published item's **identity** (GUID, link, pubDate) never changes. Its **content** changes only through the visible-state transitions in §3.3 or an explicit `resummarize`. Each change advances the entry's `updated` time (§5). The transitions are:

- `fallback` → `summarized`
- `fallback` → `no_notes`
- `no_notes` → `fallback` A published item is never pulled from the feed except by confirmed deletion (§3.1) or `max_items` rollover. GitHub is polled with conditional requests.
5. **Spec-compliant output.** RSS 2.0 passes the W3C validator, with stable GUIDs and timestamps. Atom 1.0 comes almost free by reusing feed-me's renderer.
6. **Easy to run.** It ships as one static binary in a distroless image, keeps its SQLite store on a volume, and takes config from a YAML file plus env overrides.
7. **Secrets stay out of env and config.** API keys can come from Docker secrets (files under `/run/secrets/`), so an OpenRouter key never appears in `docker inspect`, compose files, or the process environment (§4.3).

### Non-goals (for v1)

- **Private repositories.** v1 is public-only (§3.1). Private release notes would otherwise be sent to a third-party LLM and published in an unauthenticated feed.
- Per-repo or grouped feeds. There is one combined feed, though the store is keyed so that adding them later is trivial (§12).
- Periodic rollup digests (daily or weekly). The item model doesn't preclude them.
- GitHub Projects (v2) boards, GitLab, PyPI, npm and other non-GitHub sources.
- A web UI. The only HTTP surface is the feed and a health endpoint.
- Native Anthropic, Gemini or other non-OpenAI-compatible SDKs.

---

## 2. Prior art (surveyed 2026-10-15)

Nothing found combines **self-hosted Compose stack + GitHub-aware release ingestion + LLM summaries + RSS output**.

| Project | Overlap | Gap |
|---|---|---|
| [anywaylupin/upstream](https://github.com/anywaylupin/upstream) | GitHub releases → LLM → digest with breaking changes flagged | Next.js and Neon on Vercel, no Docker, web/email output, multi-user SaaS shape |
| [rkholoniuk/digest4u](https://github.com/rkholoniuk/digest4u) | GitHub releases + RSS → OpenAI → weekly digest | Python on GitHub Actions, Markdown files, OpenAI-only |
| [CondenseIt](https://github.com/salehihassen/condenseit) | GitHub releases as one of many sources, OpenAI-compatible LLMs | General news reader with a web UI; RSS is input only |
| [RSSbrew](https://github.com/yinan-c/RSSbrew) | AI-summarized digests of arbitrary feeds | Not release-aware: no tags, changelogs, pre-release filtering or version grouping |
| Banditore, ReleaseBell, GitHub Release Monitor, NewReleases | Release tracking, some with RSS | No LLM |

The workaround of pointing RSSbrew at `github.com/<repo>/releases.atom` URLs misses tag-only repos and changelog-only notes, and it handles the GitHub API's rate limits poorly.

---

## 3. Architecture

```
            ┌──────────── scheduler (interval, jitter) ────────────┐
            ▼                                                       │
  config ─► Poll ──► Resolve notes ──► Summarize ──► Store ──► Render ──► HTTP
  (repos)   GitHub   release body /     LLM (once,    SQLite    RSS/Atom   /feed.xml
            API      CHANGELOG.md /     hash-keyed)             bytes      /feed.atom
                     compare commits
```

There is one long-lived process. Each scheduler tick runs three stages: a **poll pass**, then a **summarize pass**, then a **render**. The HTTP server only reads the rendered bytes. This matches feed-me: one writer, many readers.

**Bounded passes:** no single slow repo or flaky upstream can stall the feed.

- **Each stage has its own budget**, so neither can starve the other: `schedule.poll_budget` (default 7m) and `schedule.summarize_budget` (default 7m). A stage that hits its budget stops, and its unfinished work stays persisted for the next tick. The summarize stage always runs after poll, whatever poll's outcome, so the LLM makes progress even if GitHub eats its full allowance every tick. Render runs outside both budgets.
- **Render always runs**, even when poll or summarize errored or ran out of budget. The render step promotes `pending` releases past `publish_after` to `fallback` (§3.3), so fallback publication never waits on GitHub or the LLM.
- Render also runs after every 5 completed summaries, so a long summarize pass publishes as it goes.

**Fairness:** repos are polled round-robin from a persisted cursor, so a pass cut short by the budget or a rate limit picks up where it stopped. Summaries are taken oldest `first_seen_at` first, at most `llm.max_per_pass` (default 20) per pass.

### 3.1 Poll (GitHub)

For each repo:

0. **Public check, every poll, fail-closed.** `GET /repos/{owner}/{repo}` runs with `If-None-Match` **before every poll** of that repo. When authenticated, an unchanged repo is a 304 and costs no rate limit. The repo is fetched only if this tick's response (or its validated 304) says `private: false`. If the response says `private: true`, or the check fails for any reason (error, timeout, rate limit), nothing else is fetched for that repo this tick.
   - On `private: true`, the repo is marked refused in `repo_state` and an error is logged. Its queued `pending` releases move to `skipped` so they never reach the LLM. `config lint` fails on it.
   - A repo that turns private keeps its already-published items, since they were public when fetched.

**Where public-only is actually enforced.** The guarantee rests on the per-poll visibility check above, **not** on the token. GitHub reports `private: true` for a private repo whatever the token's permissions, so a private repo is refused before any of its content is requested.

The only residual exposure is a repo flipping to private in the seconds between its visibility check and its content requests within one poll. Content fetched in that window was public moments earlier. This is accepted and documented in the README.

**Token guard (best-effort, defense in depth).** GitHub provides no API that proves a token is public-only. For example, a fine-grained token scoped to "selected repositories" that happen to be public today looks identical to a public-only token. So the guard catches obvious misconfiguration and claims nothing more:

- For a classic token, if the `X-OAuth-Scopes` header includes `repo`, startup fails.
- For any token, `GET /user/repos?visibility=private&per_page=1` returning anything fails startup.
- The startup log states that the token was checked for *obvious* private access only. The README recommends a fine-grained token with **"Public repositories (read-only)"** access.
- **SIGHUP re-runs the guard** on a replacement token *before* it's activated. If the new token fails, the old one stays in use and an error is logged.
1. `GET /repos/{owner}/{repo}/releases?per_page=30` (page 1) with `If-None-Match`. When authenticated, a 304 doesn't count against the rate limit.
2. If the repo has **no releases** (or `sources: [tags]` is set), call `GET /repos/{owner}/{repo}/tags?per_page=30` and resolve each new tag's commit date with `GET /repos/{owner}/{repo}/commits/{sha}`, whose result is cached forever.
3. **Catch-up pagination (atomic).** Each repo persists a **scan boundary**: the newest tag of its last completed scan, stored in `repo_state.scan_boundary`. If page 1 doesn't contain the boundary, which happens after downtime or a burst of releases, pages 2…`max_pages` (default 5) are fetched back-to-back until the boundary appears, the cap is reached, or the list ends.
   - **All or nothing.** Results are buffered in memory and committed in one transaction only when the scan completes. Then the new rows are inserted and the boundary advances to the newest tag seen on page 1.
   - **On interruption** (an error, a rate limit, a restart), nothing is written and the boundary stays put. The next tick starts the scan again from page 1. With no stored mid-scan position, pagination shifts between ticks (new releases, deletions of any size) can't cause skips.
   - **Forward progress is guaranteed by bounding the work, not by checkpoints.** A full scan is at most `max_pages` requests, a few seconds. The stage budget (§3) is checked only *between* repos, so a repo's poll, once started, always runs to completion or failure. The budget can't starve a catch-up. Only persistent GitHub failure can, and that is reported in `repo_state.last_error`.
   - Versions past the cap are **intentionally omitted**: never fetched, never recorded. A warning names the repo and the number of pages scanned.
4. Classify each version (§3.1.1). Then filter by `include_prereleases`, by drafts (always excluded) and by the optional `tag_pattern` / `exclude_pattern` regexes, such as skipping `nightly-*`. Filtered versions are recorded as `skipped`, which keeps them available for predecessor selection (§3.2.1).
5. Insert each unseen, unfiltered version as a `release` row in state `pending`.

**First sight of a repo:** only page 1 is read, and only its newest `backfill` versions are queued (default 3). The rest of page 1 is recorded as `skipped` so they're never summarized but can serve as predecessors. That keeps the LLM bill for adding a repo predictable. `repo_state.initialized_at` is set only after this completes without error. An interrupted first poll is redone from scratch, not treated as initialized.

**Deletion detection.** Disappearing from a list response is never enough to mark a release deleted: lists are paginated and windowed. Deletion is decided only by a **direct existence check**: `GET /repos/{o}/{r}/releases/tags/{tag}` for releases, or `GET /repos/{o}/{r}/git/ref/tags/{tag}` for tags, sent conditionally and recorded in `last_verified_at`. Two things trigger a check:

1. **Window candidates (immediate).** A stored release newer than the oldest entry on page 1 but absent from page 1 should have been there, so it's checked in the same poll. If page 1 is empty, every stored published release of that repo is a candidate.
2. **Rotating sweep (bounded).** Each tick, up to `github.verify_per_tick` (default 10) published releases across all repos are checked. They're ordered by `verify_attempted_at`, the time of the last *attempt* whatever its outcome, oldest first. This eventually catches deletions of releases that have aged out of the page-1 window. With 200 items and an hourly tick, every item gets an attempt within a day.

**Outcomes:**

- **404:** sets `missing_since`.
- **Success:** sets `last_verified_at` and clears `missing_since`.
- **Anything else** (error, rate limit, timeout): only `verify_attempted_at` is updated. The release itself is left untouched. Because the sweep orders by attempt time, persistently failing items rotate to the back instead of hogging the sweep, and healthy repos keep getting checked.

A list appearance also clears `missing_since`.

**Rate limits:** `GITHUB_TOKEN` is optional but strongly recommended (60 vs 5,000 req/h). The client reads `X-RateLimit-Remaining`, `X-RateLimit-Reset` and `Retry-After`. If the reset is under 2 minutes away, it waits. Otherwise it ends the poll stage early, and the round-robin cursor (§3) resumes there next tick. Recommended token: a fine-grained token with **"Public repositories (read-only)"** access and no other permissions. It can't read private repos even by mistake.

#### 3.1.1 Version classification

Each version gets one `Version` value, computed once and stored. Both filtering and rendering use it.

- **Stream prefix + version:** the tag is split into an optional stream prefix and a version. The prefix is anything before the version, for example `service/api/` in `service/api/v1.4.0`, or `go` in `go1.28.1`. The version is parsed leniently as SemVer: an optional `v`, then 1–4 numeric components, then an optional pre-release suffix and build metadata.
- **Pre-release:**
  - For GitHub Releases, the `prerelease` flag **or** a parsed pre-release suffix marks it, since some projects forget the flag.
  - For tags, a SemVer pre-release segment (`-rc.1`) marks it, or a match of `prerelease_pattern` against the part after the numeric components. The default pattern is `(?i)^[-.]?(alpha|beta|rc|pre|preview|dev|nightly|snapshot|canary)\.?\d*`, so it also matches `go1.28rc1` and `1.0.0beta2`.
  - `prerelease_pattern` is overridable per repo.
- **Unrecognized tags** with no numeric version component, such as `latest` or `stable`, are `skipped`, unless the repo's `tag_pattern` explicitly matches them. Those tags are then ordered by date only.

### 3.2 Resolve notes

This step produces the raw text the LLM sees. It is deterministic and unit-tested against fixtures.

| Situation | Notes source |
|---|---|
| Release with a substantive body | The release body (Markdown) |
| Release body empty, under ~40 chars, or matching `see (the )?changelog\|CHANGELOG\.md` | The matching section of the changelog file |
| Tag only | The matching section of the changelog file |
| No changelog section found, predecessor known | Commit subjects from `GET /compare/{prev}...{tag}`, capped at 100, with merge commits and `chore(deps)`/bot authors dropped |
| None of the above | **No notes**: no LLM call. The item is published deterministically as "{repo} {version} released — no release notes published", with links (state `no_notes`) |

The compare fallback is also treated as "none of the above" in four cases: the predecessor is unknown (a repo's first version), either ref is gone (404), the comparison is diverged or has more than 250 commits (an unrelated predecessor is likely), or every commit is filtered out as noise.

**Changelog lookup:** the path is configurable per repo, with a default search of `CHANGELOG.md`, `CHANGES.md`, `HISTORY.md` and `NEWS.md` at the tag's ref. The section is found by scanning Markdown headings for the version, with or without a leading `v`. Both ATX headings (`#`–`###`) and setext headings (a line underlined with `===` or `---`, treated as levels 1 and 2) count. The section runs to the next heading of the same or higher level. Fetched files are cached per (repo, ref).

#### 3.2.1 Predecessor selection

`prev_tag` is used for the compare fallback, the "Diff since" link and the prompt's context. Its value is chosen deterministically from stored rows, including `skipped` ones:

1. Candidates are only versions in the **same stream prefix** (§3.1.1). That keeps monorepo package tags from crossing.
2. For SemVer-parsed versions, the predecessor is the **highest version lower than this one** by SemVer precedence. That handles maintenance branches: the predecessor of `1.2.4` is `1.2.3` even when `2.0.0` shipped in between. A stable release skips pre-release candidates, so `1.3.0` compares against `1.2.x`, not `1.3.0-rc.2`. A pre-release considers every candidate.
3. For date-ordered (unrecognized) tags, the predecessor is the latest one in the same stream published before this one.
4. If there is no candidate, `prev_tag` is empty: no compare fallback and no diff link.

**Input budget:** notes are truncated to `llm.max_input_chars` (default 24,000) at a heading or line boundary, with a "[truncated]" marker. Some changelogs, Kubernetes for example, are enormous.

The resolved text, its source (`release_body` / `changelog` / `compare`) and its SHA-256 are stored on the release row.

### 3.3 Summarize (LLM)

- **Protocol:** OpenAI Chat Completions. Config sets `base_url`, `model` and `api_key` (from env), plus `temperature` (default 0.2) and `timeout`.
- **Structured output:** the request asks for JSON matching a fixed schema, using `response_format: {type: "json_schema"}`. If the server rejects that (some Ollama and older endpoints do), the client retries once with `{type: "json_object"}` and then with plain prompt instructions. The reply is validated against the schema; one repair retry is allowed.
- **Schema:**
  ```json
  {
    "headline": "string, ≤ 100 chars, no version number",
    "breaking": ["string"],
    "features": ["string"],
    "fixes":    ["string"],
    "security": ["string"],
    "security_urgency": "none | advisory | urgent",
    "security_reason": "string",
    "noise_only": false
  }
  ```
  Each list is capped at 6 entries. The prompt says to omit dependency bumps, CI or docs-only changes, and contributor thank-yous. `noise_only: true` means "nothing user-facing", which renders as a one-line item instead of being hidden, so the feed still shows the version shipped.
- **Prompt:** a built-in default ships in the binary, and `llm.prompt_file` can override it. The prompt gets the repo, the version, the previous version (when known) and the notes source.
- **Caching:** the summary key is the SHA-256 of a canonical JSON document holding everything that shapes the output:
  - `base_url` and `model`
  - `temperature`
  - the response schema version
  - the **fully rendered request messages**, which embed the effective prompt text (built-in or the *contents* of `prompt_file`), the repo, the version, `prev_tag`, the notes source and the notes themselves

  Two releases share a summary only if the LLM would have seen byte-identical requests. Editing `prompt_file` or switching model changes the key for *future* summaries. The scheduler only summarizes releases that are eligible (see the state table below), so published `summarized` items aren't re-billed. `release-digest resummarize` forces it.
- **Concurrency and retries:** `llm.concurrency` (default 2). Within one attempt, the client retries 429 and 5xx up to 3 times with jittered exponential backoff. A failed attempt increments `attempts` and records `last_error`. It also sets `next_attempt_at = now + min(15m × 2^attempts, 24h)`, so a broken LLM isn't hammered every tick. Backoff state is persisted and survives restarts.

**Release states.** Eligibility for the summarize pass comes from this table, and nowhere else:

| State | In feed? | Summarize-eligible? | Transitions |
|---|---|---|---|
| `skipped` | no | no | terminal: backfill, filter or catch-up overflow |
| `pending` | no | yes, when `next_attempt_at ≤ now` | → `summarized` on success; → `fallback` at render time once `now − first_seen_at ≥ llm.publish_after` (default 6h); → `no_notes` when notes resolve to none |
| `fallback` | yes, excerpt | yes, when `next_attempt_at ≤ now` (no age cutoff) | → `summarized` on success; content updates, identity doesn't. → `no_notes` when resolution (§3.2) completes and finds no usable notes. This happens when render promoted the item before resolution had finished. A *transient* resolution failure (an error, a rate limit) is not "no notes": the item stays `fallback` and retries |
| `summarized` | yes | no | → re-summarized only by `resummarize` |
| `no_notes` | yes, deterministic | no | → `fallback` when a later poll finds notes (the release body was edited or a changelog section appeared). It stays in the feed and becomes eligible immediately |

**Rules:**

- **Visible states.** `fallback`, `summarized` and `no_notes` are all visible. Every transition between them keeps the item in the feed and sets `content_updated_at`.
- **No age cutoff.** `fallback` items have none, because the backoff cap already limits a long LLM outage to at most one attempt per item per day. When the LLM comes back, every fallback is upgraded, oldest first, within the `max_per_pass` limit.
- **Fallback content.** A fallback item shows the first ~600 chars of the notes as plain text, plus a note that the summary is pending.
- **Milestone 2.** Milestone 2 (§10) publishes through this same `fallback` path. Items from before LLM support, however old, are upgraded automatically once milestone 4 lands, with no migration.

### 3.4 Urgent security alerts (ntfy)

When a new release fixes a serious security issue, the feed item alone is too easy to miss. Release Digest can also push an alert to an [ntfy](https://ntfy.sh) topic, either self-hosted or ntfy.sh. It's off unless `notify.ntfy` is configured.

**Detection.** A release is **urgent** when either signal fires. The deterministic signal can fire without the LLM, so `fallback` items are covered too.

1. **Advisory signal (deterministic, preferred).** GHSA IDs (`GHSA-xxxx-xxxx-xxxx`) and CVE IDs in the resolved notes are looked up through GitHub's global advisory API: `GET /advisories/{ghsa_id}`, or `GET /advisories?cve_id=…`. The responses are cached forever by ID. The release is urgent if any advisory has a severity ≥ `notify.min_severity` (default `high`) **and** affects this repo's ecosystem package. This signal is authoritative: an LLM can't talk it down.
2. **LLM signal.** The summary schema gains `"security_urgency": "none" | "advisory" | "urgent"` and `"security_reason": "string"`. The prompt defines `urgent` narrowly: the notes describe a fixed vulnerability that is remote code execution, an authentication or authorization bypass, known exploitation, or an explicit "upgrade immediately". Ordinary hardening and dependency CVE bumps are `advisory`.

   To limit false positives, `urgent` only counts if the item's `security` list is non-empty **and** the resolved notes contain a security keyword (`security`, `vulnerab`, `CVE-`, `GHSA-`, `exploit`). Otherwise it's downgraded to `advisory` and logged.

**What gets sent.** One `POST {notify.ntfy.url}` per urgent release, where the URL is server plus topic:

| ntfy header | Value |
|---|---|
| `Title` | `🚨 {repo} {version}: security release` |
| `Priority` | `5` (max) for critical advisories or LLM `urgent`, otherwise `4` |
| `Tags` | `rotating_light,{repo}`, plus advisory IDs |
| `Click` | the GitHub release URL |
| `Actions` | `view, Advisory, https://github.com/advisories/{ghsa}` for each advisory (max 2) |
| `Markdown` | `yes` |

The body is the headline, the `security` bullets, the advisory severity and summary when one exists, and `security_reason`. It never includes the full notes.

**When it's sent.** Alerts are queued in an outbox table, `notifications`, and delivered by the render step. Delivery runs outside the poll and summarize budgets, so a slow LLM or GitHub never delays an alert that's already decided.

- **At most once per release.** Rows are keyed by GUID, so `resummarize`, restarts and state transitions never re-send.
- **Retries.** A failed delivery retries with backoff (1m → 1h cap) for 24h, then is marked `failed` and logged.
- **Not on backfill.** Releases queued during a repo's first sight (§3.1) or a catch-up scan don't alert by default (`notify.include_backfill: false`). Neither do releases older than `notify.max_age` (default 7d) by pubDate. Adding a repo or restarting after downtime doesn't produce an alert storm.
- **Pre-releases** alert only if `notify.include_prereleases: true`.
- **Late detection still counts.** An item already published as `fallback` can turn urgent later when its LLM summary lands. It still alerts once, if it's within `max_age`.
- **Feed marking.** Urgent items also get a `🚨` title prefix and a `security-urgent` category in the feed, whether or not ntfy is configured.

**Auth.** An ntfy access token is a third secret, loaded exactly like the others (§4.3): `NTFY_TOKEN_FILE`, `/run/secrets/ntfy_token` or `NTFY_TOKEN`. It's sent as `Authorization: Bearer`. On public ntfy.sh, the topic name itself acts as a password. `config lint` warns when the URL is on `ntfy.sh` with no token, and the topic can be kept out of the YAML with `NTFY_URL_FILE` (same rules as a secret).

**Testing it.** `release-digest notify test` sends a sample alert through the configured path. `release-digest notify test --release owner/repo@tag` runs detection on a real release and prints the decision, the signals and the payload, without sending.

### 3.5 Store

SQLite through `modernc.org/sqlite` (pure Go), reusing feed-me's migration approach. See §8 for the schema.

### 3.6 Render and serve

After any change, the feed is rendered to bytes and held in memory and in the store, so a restart serves immediately. HTTP serves `/feed.xml` (RSS 2.0) and `/feed.atom` with `ETag` and `Last-Modified`, plus `/healthz`. The renderer and validator are lifted from `feed-me/internal/feed`.

---

## 4. Configuration

### 4.1 Files and precedence

1. The `release-digest.yaml` path is set by `--config` or `RELEASE_DIGEST_CONFIG`.
2. Env overrides for global settings follow the pattern `RELEASE_DIGEST_<SECTION>_<KEY>` for the ones listed below.
3. `RELEASE_DIGEST_REPOS` is a comma- or newline-separated list of `owner/repo` that is **merged** with the YAML `repos:` list. A repo in both places takes its options from YAML. Env-only repos get the defaults. Optional inline flags cover the common case without YAML: `owner/repo+pre` includes pre-releases.

Secrets (`GITHUB_TOKEN`, `LLM_API_KEY`) never come from the YAML file. They're loaded as described in §4.3, and Docker secrets are the recommended way to supply them.

### 4.2 Schema (v1)

```yaml
version: 1

store_path: /data/release-digest.db
listen: ":8080"
public_base_url: https://rss.example.com   # RELEASE_DIGEST_PUBLIC_BASE_URL

feed:
  title: Release Digest
  description: Condensed release notes for the projects I depend on.
  max_items: 200
  ttl: 60

schedule:
  interval: 1h       # RELEASE_DIGEST_SCHEDULE_INTERVAL
  jitter: 5m
  poll_budget: 7m       # per-stage deadlines; render always runs (§3)
  summarize_budget: 7m

github:
  api_url: https://api.github.com          # GHES works too
  verify_per_tick: 10                      # rotating deletion checks (§3.1)
  # token: see §4.3 (GITHUB_TOKEN_FILE / GITHUB_TOKEN)

llm:
  base_url: https://openrouter.ai/api/v1   # RELEASE_DIGEST_LLM_BASE_URL; or http://ollama:11434/v1
  model: anthropic/claude-haiku-5.5        # RELEASE_DIGEST_LLM_MODEL; placeholder, any OpenRouter slug
  # api_key: see §4.3 (LLM_API_KEY_FILE / LLM_API_KEY); optional for local servers
  headers:                                 # extra request headers; non-secret only
    HTTP-Referer: https://rss.example.com  # OpenRouter app attribution (optional)
    X-Title: Release Digest
  temperature: 0.2
  timeout: 120s
  concurrency: 2
  max_per_pass: 20
  max_input_chars: 24000
  publish_after: 6h
  # prompt_file: /config/prompt.md

notify:                                    # optional; omit to disable alerts (§3.4)
  ntfy:
    url: https://ntfy.example.com/release-alerts   # server + topic; or NTFY_URL_FILE
    # token: see §4.3 (NTFY_TOKEN_FILE / NTFY_TOKEN)
  min_severity: high                       # advisory threshold: critical | high | medium
  max_age: 7d
  include_backfill: false
  include_prereleases: false

defaults:                                  # applied to every repo
  sources: [releases, tags]                # tags used only if no releases exist
  include_prereleases: false
  backfill: 3
  max_pages: 5                             # catch-up pagination cap (§3.1)
  changelog: auto                          # or a path, or "off"
  # prerelease_pattern: overrides the default tag classifier (§3.1.1)

repos:
  - repo: golang/go
    sources: [tags]
    tag_pattern: '^go1\.\d+(\.\d+)?$'      # skip weekly/rc tags
  - repo: anthropics/anthropic-sdk-go
  - repo: hashicorp/terraform
    include_prereleases: true
  - repo: astral-sh/uv
    changelog: CHANGELOG.md
```

### 4.3 Secrets

Each secret can come from exactly **one** of three sources. The ntfy topic URL can be supplied the same way (`NTFY_URL_FILE`, `/run/secrets/ntfy_url`, or `notify.ntfy.url` in YAML), since on ntfy.sh the topic itself acts as a password.

| Secret | File path in env | Default Docker secret file | Plain env (dev convenience) |
|---|---|---|---|
| LLM API key (e.g. OpenRouter) | `LLM_API_KEY_FILE` | `/run/secrets/llm_api_key` | `LLM_API_KEY` |
| GitHub token | `GITHUB_TOKEN_FILE` | `/run/secrets/github_token` | `GITHUB_TOKEN` |
| ntfy access token (§3.4) | `NTFY_TOKEN_FILE` | `/run/secrets/ntfy_token` | `NTFY_TOKEN` |

Rules:

- **One source, or a startup error.** A source counts as present if its env var is set (even to an empty string) or, for the default path, if the file exists. If more than one source is present, startup fails with a message naming each one, for example `LLM_API_KEY and /run/secrets/llm_api_key are both set; remove one`. There's no precedence to remember, and a stale env var can't silently shadow a mounted secret.
- **No source** is fine for both secrets. GitHub then runs unauthenticated at 60 req/h with a startup warning, and the LLM sends no `Authorization` header, which suits local Ollama. `config lint` warns when `base_url` isn't a loopback or private address and there's no key.
- **`*_FILE` is the standard Docker convention**, the same one the official Postgres and MySQL images use. The default path means a compose `secrets:` entry works without any env wiring at all.
- **File handling** is the same for explicit and default paths. The file is read once at startup, and surrounding whitespace and the trailing newline (which `echo > file` leaves) are trimmed. A file that's empty after trimming, unreadable, or (for `*_FILE`) missing is a fatal error naming the path. Plain env set to an empty string is also fatal. The value is never echoed.
- **Rotation:** `SIGHUP`, or restarting the container, re-reads secret files. No polling. A replacement GitHub token must pass the token guard (§3.1) before it replaces the active one.
- **Never leaked:** secret values are held in a `secret.String` type whose `String()`, `GoString()`, `MarshalJSON()` and `LogValue()` (for slog) all return `[redacted]`. HTTP clients set `Authorization` from it at request time. Error messages from the LLM and GitHub clients strip the request headers before wrapping. `config lint` and the startup log report only the **source** of each secret, for example `llm api key: file /run/secrets/llm_api_key (51 bytes)`.
- **Not in YAML:** the config loader rejects `api_key` / `token` keys in the YAML with a message pointing to §4.3, so a key can't end up committed to `deploy/`.
- **Permissions:** compose file-based secrets are bind mounts that keep the host file's owner and mode. The container runs as distroless `nonroot` (uid 65532), so the host file must be readable by that uid. Either `chown 65532 secrets/*` with mode `0400`, or mode `0444` inside a `0700` directory owned by you. `config lint` reports an unreadable secret file with that hint. Swarm secrets can set `uid: "65532"` and `mode: 0400` directly.

`release-digest config lint` validates the merged config: it checks that repos exist and are reachable with the token, that regexes compile, and that the LLM endpoint answers a tiny test prompt.

---

## 5. Identity and timestamps

- **GUID:** a UUIDv5 of `{owner}/{repo}@{tag}`, with the repo lowercased and the tag exact. It uses a fixed namespace UUID compiled into the binary. RSS emits it with `isPermaLink="false"`, and Atom emits `urn:uuid:{uuid}`. It is stable across renames of the release title, and it's exactly the shape feed-me's `feed.AtomID` already produces (`feed-me/internal/feed/atom.go:65`), so the renderer ports unchanged. A UUID can't come out malformed whatever characters a tag contains (`/`, `+`, unicode). The golden-feed tests include such tags.
- **pubDate:** the release's `published_at`, or the tag commit's committer date for tag-only repos. Once stored, it is frozen (feed-me §5). A release that's deleted and re-created with the same tag keeps its original item.
- **Updated time:** `content_updated_at` is set when an item first becomes visible, and advanced on every content change (§1, §3.3). It's passed to the renderer as `Item.Updated`, so Atom `<updated>` moves while `<published>` and the ID stay fixed, and readers that honor updates re-show the upgraded entry. RSS 2.0 has no per-item updated element. The new `<description>` is enough for readers that diff content.
- **Ordering:** by pubDate descending. Backfilled releases keep their real dates, so they sort into the past rather than flooding the top of the feed.
- **Deleted releases:** these are marked `missing_since` only after a confirmed 404 (§3.1, Deletion detection). They're dropped from the feed after 7 days, in case the deletion is a re-publish in progress.

---

## 6. Feed item format

**Title:** `{repo} {version} — {headline}`, for example `astral-sh/uv 0.9.4 — Faster lockfile resolution for workspaces`. A `⚠` prefix marks releases with breaking changes, and a `(pre-release)` suffix marks pre-releases.

**Link:** the GitHub release page, or `…/releases/tag/{tag}` for tags, or `…/compare/{prev}...{tag}`.

**Description (HTML, sanitized):**

```html
<p><strong>Faster lockfile resolution for workspaces</strong></p>
<h4>Breaking changes</h4><ul><li>…</li></ul>
<h4>Features</h4><ul><li>…</li></ul>
<h4>Fixes</h4><ul><li>…</li></ul>
<h4>Security</h4><ul><li>…</li></ul>
<p><a href="…">Full release notes</a> · <a href="…/compare/v0.9.3...v0.9.4">Diff since 0.9.3</a></p>
<p><small>Summarized by qwen3:14b from the release notes.</small></p>
```

Empty sections are omitted. The footer names the model and notes source (`release notes` / `CHANGELOG.md` / `commit log`) for transparency.

**Categories:** `owner/repo`, plus `breaking`, `security` and `prerelease` when they apply. Feed readers can then filter one combined feed by repo.

---

## 7. CLI and deployment

```
release-digest run                       # scheduler + HTTP (container default)
release-digest build                     # one poll+summarize+render pass, then exit
release-digest summarize owner/repo@tag  # dry run: resolve notes, call LLM, print result; writes nothing
release-digest resummarize [--repo r] [--since 30d]
release-digest config lint
release-digest validate [--w3c]
release-digest healthcheck
```

### 7.1 Compose

```yaml
name: release-digest
services:
  release-digest:
    image: release-digest:latest
    build: { context: ., args: { VERSION: "${VERSION:-dev}" } }
    restart: unless-stopped
    environment:
      RELEASE_DIGEST_PUBLIC_BASE_URL: https://rss.example.com
      RELEASE_DIGEST_REPOS: |
        anthropics/anthropic-sdk-go
        astral-sh/uv
      RELEASE_DIGEST_LLM_BASE_URL: https://openrouter.ai/api/v1
      RELEASE_DIGEST_LLM_MODEL: anthropic/claude-haiku-5.5
      # No key env vars: both secrets are picked up from /run/secrets/ (§4.3).
    secrets: [llm_api_key, github_token, ntfy_token]
    volumes:
      - release-digest-data:/data
      - ./deploy:/config:ro
    expose: ["8080"]

  ollama:                       # optional local LLM: `docker compose --profile ollama up`
    profiles: [ollama]
    image: ollama/ollama:<pinned>   # pin an exact released tag when this file is written
    volumes: [ollama:/root/.ollama]

secrets:
  llm_api_key:
    file: ./secrets/llm_api_key     # OpenRouter key; secrets/ is gitignored
  github_token:
    file: ./secrets/github_token
  ntfy_token:
    file: ./secrets/ntfy_token     # only if notify.ntfy is configured

volumes:
  release-digest-data:
  ollama:
```

Setting up the secrets once on the host:

```sh
mkdir -p secrets && chmod 700 secrets
printf '%s' 'sk-or-v1-…' > secrets/llm_api_key
printf '%s' 'github_pat_…' > secrets/github_token
sudo chown 65532 secrets/* && chmod 400 secrets/*
```

The repo ships `secrets/.gitignore` containing `*` and `!.gitignore`, plus a `.dockerignore` entry for `secrets/`, so keys can't be committed or baked into the image. On a host using the 1Password CLI, `op read` can write these files instead.

The Dockerfile is the same two-stage, cross-compiled, distroless-nonroot build as feed-me. Host-specific settings go in an untracked `compose.override.yaml`.

---

## 8. Project layout

```
cmd/release-digest/      main, run, build, summarize, config lint, healthcheck
internal/config/         YAML + env merge, defaults, validation
internal/github/         REST client: releases, tags, commits, compare, contents; ETag cache; rate-limit pacing
internal/notes/          resolve notes: body heuristics, changelog section extraction, compare fallback, truncation
internal/secret/         secret.String (redacting), *_FILE / /run/secrets / env resolution, SIGHUP reload
internal/llm/            OpenAI-compatible client, response_format fallbacks, schema validation, retries
internal/summarize/      prompt, cache key, pending-queue worker, publish_after fallback
internal/advisory/       GHSA/CVE extraction from notes, GitHub advisory lookup + cache
internal/notify/         urgency decision, ntfy client, outbox delivery + retries
internal/feed/           RSS/Atom render + checks (ported from feed-me)
internal/store/          SQLite + migrations
internal/scheduler/      interval + jitter, single-flight passes (ported)
internal/server/         /feed.xml, /feed.atom, /healthz (ported)
testdata/                recorded GitHub API responses, changelog fixtures, golden feeds
```

### 8.1 Store schema (sketch)

```sql
CREATE TABLE releases (
  repo           TEXT NOT NULL,            -- owner/repo, lowercased
  tag            TEXT NOT NULL,
  guid           TEXT NOT NULL UNIQUE,     -- UUIDv5 (§5)
  kind           TEXT NOT NULL,            -- release | tag
  name           TEXT NOT NULL DEFAULT '',
  html_url       TEXT NOT NULL,
  stream         TEXT NOT NULL DEFAULT '', -- tag prefix (§3.1.1)
  version        TEXT NOT NULL DEFAULT '', -- normalized SemVer, '' if date-ordered
  prev_tag       TEXT NOT NULL DEFAULT '', -- §3.2.1
  prerelease     INTEGER NOT NULL DEFAULT 0,
  published_at   INTEGER NOT NULL,         -- Unix ms, frozen on insert
  first_seen_at  INTEGER NOT NULL,
  state          TEXT NOT NULL,            -- skipped | pending | fallback | summarized | no_notes (§3.3)
  notes_source   TEXT NOT NULL DEFAULT '', -- release_body | changelog | compare
  notes_hash     TEXT NOT NULL DEFAULT '',
  summary_key    TEXT,                     -- → summaries.key
  attempts       INTEGER NOT NULL DEFAULT 0,
  next_attempt_at INTEGER,
  last_error     TEXT NOT NULL DEFAULT '',
  content_updated_at INTEGER,              -- Atom <updated> (§5)
  last_verified_at   INTEGER,              -- last successful existence check (§3.1)
  verify_attempted_at INTEGER,             -- last attempt, any outcome; sweep order
  missing_since  INTEGER,
  PRIMARY KEY (repo, tag)
);
CREATE INDEX releases_feed ON releases (state, published_at DESC);
CREATE INDEX releases_work ON releases (state, next_attempt_at, first_seen_at);
CREATE INDEX releases_stream ON releases (repo, stream);

CREATE TABLE notes (hash TEXT PRIMARY KEY, text TEXT NOT NULL);

CREATE TABLE summaries (
  key          TEXT PRIMARY KEY,           -- sha256 of canonical request (§3.3)
  model        TEXT NOT NULL,
  json         TEXT NOT NULL,
  created_at   INTEGER NOT NULL,
  tokens_in    INTEGER, tokens_out INTEGER
);

CREATE TABLE http_cache (url TEXT PRIMARY KEY, etag TEXT, body BLOB, fetched_at INTEGER);
CREATE TABLE repo_state (repo TEXT PRIMARY KEY, initialized_at INTEGER, last_success_at INTEGER,
                         refused_private INTEGER NOT NULL DEFAULT 0, visibility_checked_at INTEGER,
                         scan_boundary TEXT,          -- newest tag of last completed scan (§3.1)
                         last_error TEXT DEFAULT '');
CREATE TABLE scheduler_state (key TEXT PRIMARY KEY, value TEXT);  -- e.g. poll round-robin cursor
CREATE TABLE rendered (path TEXT PRIMARY KEY, bytes BLOB, etag TEXT, modified_at INTEGER);

CREATE TABLE advisories (id TEXT PRIMARY KEY, severity TEXT, summary TEXT, json TEXT, fetched_at INTEGER);
CREATE TABLE notifications (               -- outbox (§3.4); one row per release ever alerted
  guid        TEXT PRIMARY KEY,            -- releases.guid: at-most-once
  channel     TEXT NOT NULL,               -- ntfy
  reason      TEXT NOT NULL,               -- advisory:GHSA-… | llm
  payload     TEXT NOT NULL,
  state       TEXT NOT NULL,               -- queued | sent | failed
  attempts    INTEGER NOT NULL DEFAULT 0,
  next_attempt_at INTEGER,
  created_at  INTEGER NOT NULL,
  sent_at     INTEGER
);
```

---

## 9. Testing strategy

- **github:** `httptest` server replaying recorded responses, covering:
  - pagination and catch-up past page 1
  - 304s and rate-limit headers, including an early stop and cursor resume
  - repos with no releases and repos with draft releases
  - a catch-up interrupted on page 3 writes nothing. Between ticks, new releases are added at the top and more than 30 earlier entries are deleted. The retried scan then finds every remaining entry within the cap, and the boundary advances exactly once.
  - a poll stage whose budget runs out mid-catch-up still finishes the repo it's on
  - private repos being refused, a visibility check that errors (fail-closed: no content requests), the token guard rejecting a `repo`-scoped token, and SIGHUP with a bad replacement token keeping the old one
  - deletion detection: a release that ages off page 1 is **not** a window candidate but is caught by the rotating sweep, an empty page 1, a candidate with a 404 is marked, a candidate whose check errors or hits a rate limit is left alone, and 10 always-failing items don't starve the rest of the sweep
- **version:** table tests of classification and predecessor selection:
  - `v2.0.0-rc.1`, `go1.28rc1`, `1.0.0beta2`, `service/api/v1.4.0`, `latest`
  - a maintenance-branch `1.2.4` after `2.0.0`
  - a stable release after its RCs
  - a repo's first version
- **notes:** table tests over real changelog fixtures with different heading styles (Keep a Changelog, `## v1.2.3 (2026-01-01)`, `# [1.2.3](link)`, setext `===`/`---`) and the "see changelog" heuristics. Also the compare-fallback edge cases: no predecessor, 404, diverged or too large, all noise → `no_notes`.
- **llm:** a fake OpenAI-compatible server that exercises the `json_schema` → `json_object` → prompt-only fallback, malformed JSON with a repair retry, 429 backoff and timeouts.
- **summarize:** the cache key differs when only the repo, version, `prev_tag` or `prompt_file` *contents* differ, and matches for byte-identical requests. Also the state machine: `pending` → `fallback` at render time even while the LLM is down, `fallback` → `summarized` after the LLM recovers across a restart, keeping the GUID and pubDate. `next_attempt_at` backoff is persisted.
- **scheduler:** with a fake GitHub that hangs and a fake LLM that fails, the tick still renders, expired fallbacks are published, and the next tick resumes polling at the cursor. With a fake GitHub that always uses all of `poll_budget`, summaries still complete every tick.
- **notify:**
  - The advisory signal fires without the LLM, and a `high` advisory isn't downgraded by an LLM `none`.
  - LLM `urgent` with no security keyword in the notes is downgraded.
  - Backfill, too-old and pre-release items are suppressed.
  - A `fallback` item that turns urgent later alerts exactly once.
  - `resummarize` and restarts never re-send.
  - Against a fake ntfy server: correct headers and Bearer auth, 5xx retry, then `failed` after 24h.
- **identity:** a `fallback` → `summarized` upgrade, a `fallback` → `no_notes` transition and a `no_notes` → `fallback` transition keep the GUID, the pubDate and feed membership, and advance Atom `<updated>`.
- **secret:** a table test of the lookup order, both `X` and `X_FILE` set is an error, trailing-newline trimming, missing or empty files, and redaction under `fmt` `%v`/`%+v`/`%#v`, `slog` and `json.Marshal`. A test also asserts that an LLM 401 error message doesn't contain the key.
- **feed:** golden RSS/Atom files, local spec checks, and `make validate` against W3C (not in every CI run).
- **End-to-end:** `build` against a fixture GitHub and a fake LLM produces the golden feed byte for byte.

---

## 10. Milestones

1. **Skeleton.** Copy in feed-me's config loading, store, feed, scheduler, server and Dockerfile, renamed. Add the secret loader (§4.3) and UUIDv5 GUIDs (§5). `run` serves an empty valid feed, and a golden test confirms the Atom IDs are well-formed `urn:uuid:` values.
2. **GitHub poll.** Token guard, per-poll public check, releases, tags, version classification, predecessor selection, filtering, backfill, atomic catch-up, deletion detection (window + sweep), ETag cache, rate-limit pacing and stage budgets. Releases go through the `pending` → `fallback` path (§3.3), so items appear with raw-note excerpts and get upgraded automatically in milestone 4.
3. **Notes resolution.** Changelog section extraction, compare fallback, truncation.
4. **LLM summaries.** Client, schema, fallbacks, cache, `summarize` dry-run command, rendered item format.
5. **Security alerts.** Summary schema urgency fields, GHSA/CVE extraction and advisory lookup, the ntfy outbox and client, the `notify test` command, and the feed's `🚨` marking.
6. **Hardening.** `config lint`, `resummarize`, deleted-release handling, W3C validation, compose with the Ollama profile, README.

Milestone 2 alone is already a useful aggregated releases feed, which makes it a natural checkpoint.

---

## 11. Decisions (2026-10-15)

- One item per release (not periodic rollups).
- OpenAI-compatible Chat Completions only.
- Sources: GitHub Releases, tags without releases, and `CHANGELOG.md`-style files.
- YAML config plus `RELEASE_DIGEST_REPOS` env override, merged.
- One combined feed.
- A structured TL;DR (headline / breaking / features / fixes / security) with a link to the full notes.
- Project name `release-digest`, a Go module, living in `~/dev/release-digest`.
- Secrets are supported through Docker secrets: `*_FILE` env vars and `/run/secrets/<name>` defaults, with plain env for dev. Exactly one source may be present, otherwise it's a startup error. Secrets in YAML are rejected. OpenRouter is the documented default LLM endpoint.
- **Public repositories only** in v1. Enforcement is a fail-closed visibility check on every poll. The token guard is best-effort defense in depth, since GitHub can't prove a token is public-only (§3.1).
- After the third and fourth roborev reviews: the token-guard claims are scoped honestly and re-checked on SIGHUP, `fallback` → `no_notes` is allowed and listed in §1, the deletion sweep orders by attempt time, and catch-up is **atomic** (buffered, committed only on completion, budget checked only between repos). That replaces the resumable-checkpoint design, which could stall or skip entries.
- **Urgent security alerts via ntfy** (§3.4): a deterministic GHSA/CVE advisory signal plus a narrowly defined LLM signal, delivered at most once per release through an outbox. Alerts are suppressed for backfill, old releases and pre-releases.
- After the second roborev review: published items are never hidden by state transitions, fallbacks have no age cutoff, deletion checks rotate across all published items, poll and summarize have separate budgets, and `content_updated_at` drives Atom `<updated>`.
- After the first roborev review: the summary cache is keyed on the full canonical request, there's an explicit release state machine with fallback retries, deletions need a confirmed 404, tag pre-releases and predecessors are classified within a stream, scheduler passes are bounded and fair, and GUIDs are UUIDv5.

## 12. Open questions

1. **Should the env repo list *merge* with YAML or *replace* it?** The plan says merge. Replace is simpler to reason about when the env var is set from compose.
2. **Should noise-only releases** (pure dependency bumps) appear as one-liners (current plan) or be hidden entirely, perhaps behind a per-repo `hide_noise: true`?
3. **Which OpenRouter model** should be the example default? `anthropic/claude-haiku-5.5` is a placeholder. Pick whichever slug you'll actually run. This only affects docs and examples.
4. **Should one GitHub release that bundles several packages** be split per package? Monorepos like `aws-sdk-go-v2` tag dozens of modules at once. In v1, stream prefixes (§3.1.1) keep predecessors correct, and `tag_pattern` picks which streams to follow. Splitting one release *body* across packages is deferred.
5. **Should there be per-repo feeds later?** The GUID and categories already allow it, and it would be a renderer-only change.
6. **Should security alerts get a daily "advisory" digest too?** In v1, only `urgent` releases push to ntfy. `advisory`-level items are visible in the feed only.
