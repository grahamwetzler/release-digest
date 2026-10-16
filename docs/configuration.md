# Configuration

The config is one YAML file, read once at startup. Restart the service to
apply a change; `SIGHUP` re-reads [secrets](secrets.md), not the config. The
code is in [`internal/config`](../internal/config).

## Sources and precedence

1. The file path comes from `--config`, else `RELEASE_DIGEST_CONFIG`, else
   `release-digest.yaml` in the working directory. The image sets
   `RELEASE_DIGEST_CONFIG=/config/release-digest.yaml`, which compose mounts
   from `deploy/`. Relative paths in the file resolve against its directory.
2. A few global settings can be overridden from the environment:

   | Setting | Env var |
   |---|---|
   | `public_base_url` | `RELEASE_DIGEST_PUBLIC_BASE_URL` |
   | `schedule.interval` | `RELEASE_DIGEST_SCHEDULE_INTERVAL` |
   | `llm.base_url` | `RELEASE_DIGEST_LLM_BASE_URL` |
   | `llm.model` | `RELEASE_DIGEST_LLM_MODEL` |

3. `RELEASE_DIGEST_REPOS` is a comma- or newline-separated list of
   `owner/repo` that's **merged** with the YAML `repos:` list. A repo in both
   takes its options from the YAML; env-only repos get the defaults. The
   inline flag `owner/repo+pre` includes pre-releases.

Secrets never come from the YAML; the loader rejects `api_key` and `token`
keys, and LLM headers that carry a credential. See [Secrets](secrets.md).

## Schema

Every setting, with its default:

```yaml
version: 1

store_path: release-digest.db        # relative to the config file
listen: ":8080"
public_base_url: https://rss.example.com   # required; RELEASE_DIGEST_PUBLIC_BASE_URL

log:
  format: json                       # json | text
  level: info                        # debug | info | warn | error

feed:
  title: Release Digest
  description: Condensed release notes for the projects I depend on.
  max_items: 200
  ttl: 60                            # minutes

schedule:
  interval: 1h                       # RELEASE_DIGEST_SCHEDULE_INTERVAL
  jitter: 5m
  sweep_budget: 2m                   # the visibility sweep at tick start
  poll_budget: 7m
  summarize_budget: 7m

github:
  api_url: https://api.github.com    # GitHub Enterprise Server works too
  verify_per_tick: 10                # rotating deletion checks
  request_timeout: 20s

llm:
  base_url: https://openrouter.ai/api/v1   # RELEASE_DIGEST_LLM_BASE_URL
  model: ""                          # RELEASE_DIGEST_LLM_MODEL; empty turns summaries off
  headers: {}                        # extra request headers; non-secret only
  temperature: 0.2
  timeout: 120s
  concurrency: 2
  max_per_pass: 20
  max_input_chars: 24000
  publish_after: 6h                  # publish an excerpt if no summary by then
  # prompt_file: prompt.md           # replaces the built-in system prompt

notify:                              # alerts are off without an ntfy URL
  ntfy:
    url: https://ntfy.example.com/release-alerts   # or the ntfy_url secret
  min_severity: high                 # critical | high | medium
  max_age: 7d
  include_backfill: false
  include_prereleases: false

defaults:                            # applied to every repo
  sources: [releases, tags]          # tags are used only if there are no releases
  include_prereleases: false
  backfill: 3                        # releases summarized when a repo is added
  max_pages: 5                       # catch-up pagination cap
  changelog: auto                    # auto | off | a repo-relative path
  # tag_pattern: only tags matching this are followed
  # exclude_pattern: tags matching this are skipped
  # prerelease_pattern: overrides the default pre-release classifier
  # instructions: what matters to you, for the LLM; a repo's own replaces it

repos:
  - repo: golang/go
    sources: [tags]
    tag_pattern: '^go1\.\d+(\.\d+)?$'      # skip weekly and rc tags
  - repo: hashicorp/terraform
    include_prereleases: true
  - repo: astral-sh/uv
    changelog: CHANGELOG.md
    packages:                              # advisory matching when names differ
      - { ecosystem: pip, name: uv }
    instructions: Lead with lockfile and resolver changes; I use uv in CI.
```

Durations are Go durations (`90m`, `1h30m`) or a whole number of days (`7d`).

Some values are derived rather than configured:

- **Publish window:** `max(7d, notify.max_age)`. Reconciled releases older
  than this are skipped, and `no_notes` releases inside it are rechecked daily.
  See [Polling GitHub](polling.md#daily-reconciliation).
- **Reconciliation depth:** `max_pages + 5` pages.

Where each setting is used:

- `schedule.*`: [Architecture](architecture.md)
- `github.*`, `sources`, `backfill`, `max_pages`, the patterns:
  [Polling GitHub](polling.md), [Versions](versions.md)
- `changelog`, `llm.max_input_chars`: [Release notes](notes.md)
- `llm.*`, `instructions`: [Summaries](summaries.md)
- `notify.*`, `packages`: [Security alerts](security-alerts.md)
- `feed.*`: [The feed](feed.md)

## Checking a config

`release-digest config lint` validates the merged config, including that every
regex compiles, and reports where each secret came from (never its value). It
then runs the token guard and the public check on every repo, failing on one
that's private or missing, lists which repos have `instructions`, and sends
the LLM a tiny fixed request that contains no release data, reporting which
response format the server accepted. `--offline` skips the GitHub and LLM
checks.
