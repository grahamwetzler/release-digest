# Release Digest design

These documents explain how Release Digest works and why. The
[top-level README](../README.md) covers installing and running it.

| Document | Covers |
|---|---|
| [Architecture](architecture.md) | The pipeline, the scheduler tick, stage budgets, fairness, the store, the package layout |
| [Public repositories only](public-only.md) | The per-poll visibility check, holding a repo, the processing gate, the token guard |
| [Polling GitHub](polling.md) | Releases and tags, catch-up, reconciliation, first sight and backfill, deletion detection, rate limits |
| [Versions](versions.md) | How tags are parsed into streams and versions, and how a release's predecessor is chosen |
| [Release notes](notes.md) | Where the text sent to the LLM comes from: release body, changelog, commit log |
| [Summaries](summaries.md) | The LLM client, the summary schema, caching, retries, the release state machine, `resummarize` |
| [Security alerts](security-alerts.md) | Urgency detection, advisory matching, the ntfy outbox and its delivery guarantees |
| [The feed](feed.md) | Item identity and timestamps, the item format, what may change after publication |
| [Configuration](configuration.md) | The YAML schema, env overrides and defaults |
| [Secrets](secrets.md) | Where secrets come from, rotation and redaction |
| [Development](development.md) | Running locally, the test strategy |

## Goals

1. **One summarized item per release.** Each new version of a watched repo
   becomes one feed item with a short LLM-written summary and a link to the
   full notes.
2. **Works on repos that don't do Releases well.** It uses GitHub Releases
   when a repo has them, falls back to tags when it doesn't, and pulls from
   `CHANGELOG.md` when the release body is empty or only points at the
   changelog.
3. **Any OpenAI-compatible LLM.** It speaks one protocol,
   `POST /v1/chat/completions`, so Ollama, LM Studio, vLLM, OpenRouter and
   OpenAI all work through config alone.
4. **Cheap and stable.** Each release is summarized once and cached by its
   exact request, so re-running never re-bills the LLM. GitHub is polled with
   conditional requests. A published item's identity never changes, and its
   content changes only for the reasons listed in [The feed](feed.md#what-may-change-after-publication).
5. **Spec-compliant output.** RSS 2.0 and Atom 1.0 that pass the W3C
   validator, with stable GUIDs and timestamps.
6. **Easy to run.** One static binary in a distroless image, a SQLite store on
   a volume, YAML config plus env overrides.
7. **Secrets stay out of env and config.** Keys come from Docker secrets, so
   they never appear in `docker inspect`, compose files or the process
   environment.

## Non-goals

- **Private repositories.** Private release notes would otherwise be sent to
  a third-party LLM and published in an unauthenticated feed. See
  [Public repositories only](public-only.md).
- Per-repo or grouped feeds. There's one combined feed, though GUIDs and
  categories would allow per-repo feeds as a renderer-only change.
- Periodic rollup digests (daily or weekly).
- GitLab, PyPI, npm and other non-GitHub sources.
- A web UI. The only HTTP surface is the feed and a health endpoint.
- Native Anthropic, Gemini or other non-OpenAI-compatible SDKs.
