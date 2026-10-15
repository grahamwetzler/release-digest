# Release Digest — Planning Document

*Drafted 2026-10-15*

A Go service that watches a configured list of GitHub repositories for new versions and has an LLM condense each version's release notes into a structured TL;DR. It serves the results as one combined RSS feed. It runs as a single container in a Docker Compose stack, following the patterns of [feed-me](../feed-me).

---

## 1. Goals

1. **One summarized item per release.** Each new version of a watched repo becomes one feed item. The item has a short LLM-written TL;DR and a link to the full notes.
2. **Works on repos that don't do Releases well.** It uses GitHub Releases when a repo has them, falls back to tags when it doesn't, and pulls from `CHANGELOG.md` when the release body is empty or just says "see changelog".
3. **Any OpenAI-compatible LLM.** It speaks one protocol, `POST /v1/chat/completions`, so Ollama, LM Studio, vLLM, OpenRouter, OpenAI and Anthropic's OpenAI-compatible endpoint all work through config alone.
4. **Cheap and stable.** Each release is summarized **once** and cached by input hash. Re-running never re-bills the LLM or rewrites a published item. GitHub is polled with conditional requests.
5. **Spec-compliant output.** RSS 2.0 passes the W3C validator, with stable GUIDs and timestamps. Atom 1.0 comes almost free by reusing feed-me's renderer.
6. **Easy to run.** It ships as one static binary in a distroless image, keeps its SQLite store on a volume, and takes config from a YAML file plus env overrides.
7. **Secrets stay out of env and config.** API keys can come from Docker secrets (files under `/run/secrets/`), so an OpenRouter key never appears in `docker inspect`, compose files, or the process environment (§4.3).

### Non-goals (for v1)

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

There is one long-lived process. The scheduler runs a **poll pass** over every repo, then a **summarize pass** over every pending release, then re-renders the feed if anything changed. The HTTP server only reads the rendered bytes. This matches feed-me: one writer, many readers.

### 3.1 Poll (GitHub)

For each repo:

1. `GET /repos/{owner}/{repo}/releases?per_page=30` with `If-None-Match`. When authenticated, a 304 doesn't count against the rate limit.
2. If the repo has **no releases** (or `sources: [tags]` is set), call `GET /repos/{owner}/{repo}/tags?per_page=30` and resolve each new tag's commit date with `GET /repos/{owner}/{repo}/commits/{sha}`, whose result is cached forever.
3. Filter by `include_prereleases`, `include_drafts` (always false), and the optional `tag_pattern` / `exclude_pattern` regexes, such as skipping `nightly-*`.
4. Insert each unseen version as a `release` row in state `pending`.

**First sight of a repo:** only the newest `backfill` releases are queued (default 3). Older ones are recorded as `skipped` so they're never summarized. That keeps the LLM bill for adding a repo predictable.

**Rate limits:** `GITHUB_TOKEN` is optional but strongly recommended (60 vs 5,000 req/h). The client reads `X-RateLimit-Remaining` and `Retry-After` and pauses the pass instead of failing it. A classic token with no scopes, or a fine-grained token with public read access, is enough. Private repos need `contents:read`.

### 3.2 Resolve notes

This step produces the raw text the LLM sees. It is deterministic and unit-tested against fixtures.

| Situation | Notes source |
|---|---|
| Release with a substantive body | The release body (Markdown) |
| Release body empty, under ~40 chars, or matching `see (the )?changelog\|CHANGELOG\.md` | The matching section of the changelog file |
| Tag only | The matching section of the changelog file |
| No changelog section found | Commit subjects from `GET /compare/{prev}...{tag}`, capped at 100, with merge commits and `chore(deps)`/bot authors dropped |

**Changelog lookup:** the path is configurable per repo, with a default search of `CHANGELOG.md`, `CHANGES.md`, `HISTORY.md` and `NEWS.md` at the tag's ref. The section is found by scanning Markdown headings (`#`–`###`) for the version with or without a leading `v`. It runs to the next heading of the same or higher level. Fetched files are cached per (repo, ref).

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
    "noise_only": false
  }
  ```
  Each list is capped at 6 entries. The prompt says to omit dependency bumps, CI or docs-only changes, and contributor thank-yous. `noise_only: true` means "nothing user-facing", which renders as a one-line item instead of being hidden, so the feed still shows the version shipped.
- **Prompt:** a built-in default ships in the binary, and `llm.prompt_file` can override it. The prompt gets the repo, the version, the previous version (when known) and the notes source.
- **Caching:** the summary key is `sha256(prompt_version ‖ model ‖ notes_hash)`. If a key already exists, for example after a force-pushed tag with identical notes or when two repos share notes, it's reused. Changing the model or prompt does **not** re-summarize published items unless `release-digest resummarize` is run explicitly.
- **Concurrency and retries:** `llm.concurrency` (default 2), exponential backoff on 429 and 5xx. A release that keeps failing stays `pending`, and its `attempts` and `last_error` are recorded.
- **Fallback:** if a release is still unsummarized `llm.publish_after` (default 6h) after it was first seen, it is published with a deterministic fallback: the first ~600 chars of the notes as plain text, plus a note that the summary failed. When the LLM later succeeds, the item's content is updated, but its GUID and pubDate stay the same.

### 3.4 Store

SQLite through `modernc.org/sqlite` (pure Go), reusing feed-me's migration approach. See §8 for the schema.

### 3.5 Render and serve

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

github:
  api_url: https://api.github.com          # GHES works too
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
  max_input_chars: 24000
  publish_after: 6h
  # prompt_file: /config/prompt.md

defaults:                                  # applied to every repo
  sources: [releases, tags]                # tags used only if no releases exist
  include_prereleases: false
  backfill: 3
  changelog: auto                          # or a path, or "off"

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

There are two secrets, and each has the same lookup order. The first source that is set wins:

| Secret | 1. File path in env | 2. Default Docker secret file | 3. Plain env (dev convenience) |
|---|---|---|---|
| LLM API key (e.g. OpenRouter) | `LLM_API_KEY_FILE` | `/run/secrets/llm_api_key` | `LLM_API_KEY` |
| GitHub token | `GITHUB_TOKEN_FILE` | `/run/secrets/github_token` | `GITHUB_TOKEN` |

Rules:

- **`*_FILE` is the standard Docker convention**, the same one the official Postgres and MySQL images use. Setting both `X` and `X_FILE` is a startup error, so it's never ambiguous which one is in use.
- **Default secret path:** if neither env var is set but `/run/secrets/<name>` exists, it's used. A compose `secrets:` entry therefore works without any env wiring at all.
- **File handling:** the file is read once at startup. Surrounding whitespace and the trailing newline (which `echo > file` leaves) are trimmed. A file that's missing, empty or unreadable when its path was set explicitly is a fatal error naming the path. The value is never echoed.
- **Rotation:** `SIGHUP`, or restarting the container, re-reads secret files. No polling.
- **Never leaked:** secret values are held in a `secret.String` type whose `String()`, `GoString()`, `MarshalJSON()` and `LogValue()` (for slog) all return `[redacted]`. HTTP clients set `Authorization` from it at request time. Error messages from the LLM and GitHub clients strip the request headers before wrapping. `config lint` and the startup log report only the **source** of each secret, for example `llm api key: file /run/secrets/llm_api_key (51 bytes)`.
- **Not in YAML:** the config loader rejects `api_key` / `token` keys in the YAML with a message pointing to §4.3, so a key can't end up committed to `deploy/`.
- **Permissions:** compose file-based secrets are bind mounts that keep the host file's owner and mode. The container runs as distroless `nonroot` (uid 65532), so the host file must be readable by that uid. Either `chown 65532 secrets/*` with mode `0400`, or mode `0444` inside a `0700` directory owned by you. `config lint` reports an unreadable secret file with that hint. Swarm secrets can set `uid: "65532"` and `mode: 0400` directly.

`release-digest config lint` validates the merged config: it checks that repos exist and are reachable with the token, that regexes compile, and that the LLM endpoint answers a tiny test prompt.

---

## 5. Identity and timestamps

- **GUID:** `urn:release-digest:{owner}/{repo}@{tag}`, with `isPermaLink="false"`. It is stable across renames of the release title.
- **pubDate:** the release's `published_at`, or the tag commit's committer date for tag-only repos. Once stored, it is frozen (feed-me §5). A release that's deleted and re-created with the same tag keeps its original item.
- **Ordering:** by pubDate descending. Backfilled releases keep their real dates, so they sort into the past rather than flooding the top of the feed.
- **Deleted releases:** these are marked `missing_since` and dropped from the feed after 7 days, in case the deletion is a re-publish in progress.

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
    secrets: [llm_api_key, github_token]
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
  kind           TEXT NOT NULL,            -- release | tag
  name           TEXT NOT NULL DEFAULT '',
  html_url       TEXT NOT NULL,
  prev_tag       TEXT NOT NULL DEFAULT '',
  prerelease     INTEGER NOT NULL DEFAULT 0,
  published_at   INTEGER NOT NULL,         -- Unix ms, frozen on insert
  first_seen_at  INTEGER NOT NULL,
  state          TEXT NOT NULL,            -- pending | summarized | fallback | skipped
  notes_source   TEXT NOT NULL DEFAULT '', -- release_body | changelog | compare
  notes_hash     TEXT NOT NULL DEFAULT '',
  summary_key    TEXT,                     -- → summaries.key
  attempts       INTEGER NOT NULL DEFAULT 0,
  last_error     TEXT NOT NULL DEFAULT '',
  missing_since  INTEGER,
  PRIMARY KEY (repo, tag)
);
CREATE INDEX releases_feed ON releases (state, published_at DESC);

CREATE TABLE notes (hash TEXT PRIMARY KEY, text TEXT NOT NULL);

CREATE TABLE summaries (
  key          TEXT PRIMARY KEY,           -- sha256(prompt_version‖model‖notes_hash)
  model        TEXT NOT NULL,
  json         TEXT NOT NULL,
  created_at   INTEGER NOT NULL,
  tokens_in    INTEGER, tokens_out INTEGER
);

CREATE TABLE http_cache (url TEXT PRIMARY KEY, etag TEXT, body BLOB, fetched_at INTEGER);
CREATE TABLE repo_state (repo TEXT PRIMARY KEY, initialized_at INTEGER, last_success_at INTEGER, last_error TEXT DEFAULT '');
CREATE TABLE rendered (path TEXT PRIMARY KEY, bytes BLOB, etag TEXT, modified_at INTEGER);
```

---

## 9. Testing strategy

- **github:** `httptest` server replaying recorded responses, covering pagination, 304s, rate-limit headers, repos with no releases and repos with draft releases.
- **notes:** table tests over real changelog fixtures with different heading styles (Keep a Changelog, `## v1.2.3 (2026-01-01)`, `# [1.2.3](link)`, setext headings) and the "see changelog" heuristics.
- **llm:** a fake OpenAI-compatible server that exercises the `json_schema` → `json_object` → prompt-only fallback, malformed JSON with a repair retry, 429 backoff and timeouts.
- **summarize:** cache-key reuse, `publish_after` fallback, then a later upgrade that keeps the GUID and pubDate.
- **secret:** a table test of the lookup order, both `X` and `X_FILE` set is an error, trailing-newline trimming, missing or empty files, and redaction under `fmt` `%v`/`%+v`/`%#v`, `slog` and `json.Marshal`. A test also asserts that an LLM 401 error message doesn't contain the key.
- **feed:** golden RSS/Atom files, local spec checks, and `make validate` against W3C (not in every CI run).
- **End-to-end:** `build` against a fixture GitHub and a fake LLM produces the golden feed byte for byte.

---

## 10. Milestones

1. **Skeleton.** Copy in feed-me's config loading, store, feed, scheduler, server and Dockerfile, renamed. `run` serves an empty valid feed.
2. **GitHub poll.** Releases, tags, filtering, backfill, ETag cache, rate-limit pacing. Items are published with raw notes and no LLM yet.
3. **Notes resolution.** Changelog section extraction, compare fallback, truncation.
4. **LLM summaries.** Client, schema, fallbacks, cache, `summarize` dry-run command, rendered item format.
5. **Hardening.** `config lint`, `resummarize`, deleted-release handling, W3C validation, compose with the Ollama profile, README.

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
- Secrets are supported through Docker secrets: `*_FILE` env vars and `/run/secrets/<name>` defaults, with plain env as a fallback. Secrets in YAML are rejected. OpenRouter is the documented default LLM endpoint.

## 12. Open questions

1. **Should the env repo list *merge* with YAML or *replace* it?** The plan says merge. Replace is simpler to reason about when the env var is set from compose.
2. **Should noise-only releases** (pure dependency bumps) appear as one-liners (current plan) or be hidden entirely, perhaps behind a per-repo `hide_noise: true`?
3. **Which OpenRouter model** should be the example default? `anthropic/claude-haiku-5.5` is a placeholder. Pick whichever slug you'll actually run. This only affects docs and examples.
4. **Should one GitHub release that bundles several packages** be split per package? Monorepos like `aws-sdk-go-v2` tag dozens of modules at once. For v1 the plan uses `tag_pattern` to pick one, and per-package splitting is deferred.
5. **Should there be per-repo feeds later?** The GUID and categories already allow it, and it would be a renderer-only change.
