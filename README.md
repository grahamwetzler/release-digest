# Release Digest

Release Digest follows the GitHub releases of the projects you depend on and
publishes one combined RSS and Atom feed. An LLM condenses each release's notes
into a headline plus short lists of breaking changes, features, fixes and
security fixes. Any OpenAI-compatible endpoint works: OpenRouter, a local
Ollama, LM Studio, vLLM or OpenAI.

```
⚠ astral-sh/uv 0.9.4 — Faster lockfile resolution for workspaces
  Breaking changes · Features · Fixes · Full release notes · Diff since 0.9.3
  Summarized by anthropic/claude-haiku-5.5 from the release notes.
```

- **One item per release.** It uses the release body, the matching section of
  `CHANGELOG.md`, or the commit log, in that order. Tag-only repos work too.
- **Nothing waits long on the LLM.** A new release is published as soon as its
  summary is ready. If the LLM hasn't answered within `llm.publish_after`
  (default 6h), the release is published with an excerpt of its notes and
  upgraded in place once the summary arrives. The item's GUID and date don't
  change.
- **Urgent security alerts** can be sent to [ntfy](https://ntfy.sh). See
  [Security alerts](#security-alerts).
- **Public repositories only.** Every poll checks that the repo is public
  before any of its content is fetched or sent to the LLM. A repo that turns
  private is held until it's public again.

The design, including every state transition and failure mode, is in
[PLAN.md](PLAN.md).

## Quick start (Docker Compose)

1. **Create the secrets.** Keys are kept in files that compose mounts as
   [Docker secrets](https://docs.docker.com/compose/how-tos/use-secrets/),
   never in environment variables or the config:

   ```sh
   mkdir -p secrets && chmod 700 secrets
   printf '%s' 'sk-or-v1-…' > secrets/llm_api_key      # your OpenRouter key
   printf '%s' 'github_pat_…' > secrets/github_token   # see "GitHub token" below
   chmod 400 secrets/* && sudo chown 65532 secrets/*   # readable only by the container's nonroot user
   ```

   `secrets/` is in `.gitignore` and `.dockerignore`, so keys can't be
   committed or baked into the image.

2. **List your repos** in `compose.yaml` (`RELEASE_DIGEST_REPOS`, one per
   line), and set `RELEASE_DIGEST_PUBLIC_BASE_URL` to the URL the feed will be
   served from. Per-repo options go in `deploy/release-digest.yaml`.
   Host-specific settings, such as a published port, go in an untracked
   `compose.override.yaml`:

   ```yaml
   services:
     release-digest:
       ports: ["127.0.0.1:8080:8080"]
   ```

3. **Check and start it:**

   ```sh
   make docker
   docker compose run --rm release-digest config lint
   docker compose up -d
   ```

   `config lint` checks the config, says where each secret came from (never
   its value), and confirms that every repo is public and that the LLM
   answers.

The feed is served at `/feed.xml` (RSS) and `/feed.atom` (Atom). `/healthz`
reports health. Put a TLS-terminating reverse proxy in front of it.

### GitHub token

The service works without a token, but GitHub then allows only 60 requests an
hour. That's enough to keep a handful of repos up to date, but the first poll
of a new repo also backfills its recent releases, and two repos can use a whole
hour's quota. The service then waits for the limit to reset and carries on.
Use a **fine-grained personal
access token** with the **Public repositories (read-only)** access level and no
permissions. At startup, the token guard rejects a token that can list private
repos. GitHub can't prove that a token is public-only, so the guard is defense
in depth; the per-poll public check is what enforces the rule.

### Local LLM with Ollama

The `ollama` compose profile runs a local Ollama next to the service:

```sh
docker compose --profile ollama up -d
docker compose exec ollama ollama pull qwen3:8b
```

Then point the service at it in `compose.yaml`:

```yaml
      RELEASE_DIGEST_LLM_BASE_URL: http://ollama:11434/v1
      RELEASE_DIGEST_LLM_MODEL: qwen3:8b
```

A local server needs no key. Leave `llm_api_key` out of the service's
`secrets:` list in `compose.override.yaml`, so no key is sent. Servers that
don't support structured output are handled: the client falls back from
`json_schema` to `json_object` to prompt instructions. `config lint` reports
which format the server accepted.

## Configuration

The config is YAML (`deploy/release-digest.yaml` in the image, or `--config`).
A few settings can also come from the environment:

| Setting | Env var |
|---|---|
| `public_base_url` | `RELEASE_DIGEST_PUBLIC_BASE_URL` |
| `schedule.interval` | `RELEASE_DIGEST_SCHEDULE_INTERVAL` |
| `llm.base_url`, `llm.model` | `RELEASE_DIGEST_LLM_BASE_URL`, `RELEASE_DIGEST_LLM_MODEL` |
| repos (added to the YAML's list) | `RELEASE_DIGEST_REPOS` |

A repo with options:

```yaml
repos:
  - repo: golang/go
    sources: [tags]
    tag_pattern: '^go1\.\d+(\.\d+)?$'   # skip weekly and rc tags
  - repo: hashicorp/terraform
    include_prereleases: true
  - repo: astral-sh/uv
    changelog: CHANGELOG.md
    packages: [{ ecosystem: pip, name: uv }]   # for matching security advisories
    instructions: |
      I use uv for Python version management and lockfiles in CI.
      Lead with anything that changes the lockfile format or resolver behavior.
```

`instructions` tell the LLM what matters to you in a repo's releases. Put a
default for every repo under `defaults:`; a repo's own `instructions` replace
it. They steer what the summary includes and puts first, never what it claims,
and they don't affect security alerts. Editing them applies to new releases;
run `release-digest resummarize --repo owner/repo` to redo existing ones. Up to
2000 characters.

PLAN.md §4.2 lists every setting with its default. The config is read once at
startup; restart the service to apply a change.

### Secrets

Each secret comes from exactly one source. If more than one is present,
startup fails and names them, so a stale env var can't shadow a mounted
secret.

| Secret | Docker secret | File path in env | Plain env (development) |
|---|---|---|---|
| LLM API key | `/run/secrets/llm_api_key` | `LLM_API_KEY_FILE` | `LLM_API_KEY` |
| GitHub token | `/run/secrets/github_token` | `GITHUB_TOKEN_FILE` | `GITHUB_TOKEN` |
| ntfy access token | `/run/secrets/ntfy_token` | `NTFY_TOKEN_FILE` | `NTFY_TOKEN` |
| ntfy URL (if not in the YAML) | `/run/secrets/ntfy_url` | `NTFY_URL_FILE` | `NTFY_URL` |

The config rejects keys and tokens in the YAML. Secret values are redacted
from logs and errors. To rotate a secret, replace its file and send
`docker compose kill -s HUP release-digest`. A replacement GitHub token must
pass the token guard before it's used.

## Security alerts

A release is **urgent** when it fixes a GitHub-reviewed advisory for the repo
at or above `notify.min_severity` (default `high`), or when the LLM rates it
urgent and the notes back that up. Urgent items are marked `🚨` in the feed with
a `security-urgent` category. If ntfy is configured, they're also pushed once
per release:

```yaml
notify:
  ntfy:
    url: https://ntfy.example.com/release-alerts
  min_severity: high
  max_age: 7d      # only releases this recent alert; delivery is retried until then
```

On public ntfy.sh, the topic name works as a password. Keep the URL in the
`ntfy_url` secret instead of the config, and add it (and `ntfy_token`, if your
server uses tokens) in `compose.override.yaml`:

```yaml
services:
  release-digest:
    secrets: [llm_api_key, github_token, ntfy_url]   # compose replaces the list
secrets:
  ntfy_url:
    file: ./secrets/ntfy_url
```

Alerts from the initial backfill and for pre-releases are off by default.
Delivery is at least once: an ambiguous failure can deliver an alert twice. If
every attempt in the alert window fails, the alert expires, it's logged as an
error, and `/healthz` returns 503 until you run `release-digest notify ack`.

Test the path with `release-digest notify test`. To see what detection would
decide for a real release without sending anything, use
`release-digest notify test --release pallets/werkzeug@3.0.6`.

## Commands

```
release-digest run                     scheduler + HTTP server (the container default)
release-digest build                   one pass, then exit
release-digest summarize owner/repo@tag
                                       dry run: resolve the notes and call the LLM; writes nothing
release-digest resummarize [--repo owner/repo] [--since 30d|all]
                                       queue fresh summaries, for example after changing the model or prompt
release-digest notify test [--release owner/repo@tag]
release-digest notify ack              acknowledge expired alerts
release-digest config lint [--offline] check the config, the secrets, GitHub and the LLM
release-digest validate [--w3c]        check the stored feed, optionally with the W3C validator
release-digest healthcheck             probe /healthz (the image's HEALTHCHECK)
```

In the container, run them with
`docker compose exec release-digest /release-digest <command>`.

Summaries are cached by their exact request, so a summarized release is never
billed twice. `resummarize` only marks releases. The running service's next
tick re-summarizes them, at most `llm.max_per_pass` per tick and after any new
releases. Each one keeps its current summary until the new one is ready.

## Development

```sh
go run ./cmd/release-digest run   # uses ./release-digest.yaml and serves on 127.0.0.1:8080
make test                         # go test ./...
make lint                         # config lint --offline on both configs
make validate                     # W3C-validate a feed built with `release-digest build`
```

Go 1.27 or later. SQLite is the pure-Go `modernc.org/sqlite`, so builds need no
CGO and cross-compile cleanly.
