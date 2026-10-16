# Development

Go 1.27 or later. SQLite is the pure-Go `modernc.org/sqlite`, so builds need
no CGO and cross-compile cleanly.

## Running locally

With no `--config`, the commands read `release-digest.yaml` from the working
directory. That file is gitignored, so create your own. A minimal one:

```yaml
version: 1
store_path: release-digest.db
listen: "127.0.0.1:8080"
public_base_url: http://localhost:8080
log:
  format: text
  level: debug

repos:
  - repo: astral-sh/uv
  - repo: golang/go
    sources: [tags]
    tag_pattern: '^go1\.\d+(\.\d+)?$'
```

Then:

```sh
export GITHUB_TOKEN=github_pat_…         # optional; see README "GitHub token"
export LLM_API_KEY=sk-or-v1-…            # omit for a local LLM
export RELEASE_DIGEST_LLM_MODEL=anthropic/claude-haiku-5.5
go run ./cmd/release-digest config lint  # check the config, GitHub and the LLM
go run ./cmd/release-digest run          # serves on 127.0.0.1:8080
```

Plain env vars are fine for development. In production, use files
([Secrets](secrets.md)). Without a model, the feed shows excerpts only.

`release-digest summarize owner/repo@tag` is a dry run for one release: it
resolves the notes from GitHub exactly as the poll stage does, calls the LLM,
and prints the notes source, the cache key, token counts, the summary and the
rendered item. It doesn't open the store. `--messages` also prints the request
messages.

## Make targets

```sh
make build      # bin/release-digest
make test       # go test ./...
make vet        # go vet ./...
make lint       # config lint --offline on deploy/release-digest.yaml
make validate   # W3C-validate a feed built with `release-digest build`
make docker     # the container image
```

## Testing strategy

Tests don't touch the network. External services are fakes:

- **GitHub:** an `httptest` server replaying recorded responses: pagination
  and catch-up, 304s and rate-limit headers, tag-only repos, drafts, an
  interrupted catch-up that writes nothing, and the visibility gate (a repo
  that returns private, 404 or 5xx gets no content requests, LLM calls,
  promotions or alert deliveries, and resumes when public again).
- **LLM:** a fake OpenAI-compatible server (`internal/llm/llmtest`) that
  exercises the `json_schema` → `json_object` → prompt-only fallback, malformed
  JSON with a repair retry, 429 backoff and timeouts.
- **ntfy:** a fake server checking headers, Bearer auth, retries through the
  alert window, expiry and the `/healthz` degradation, and at-least-once
  delivery when a server accepts a POST then drops the connection.

Table tests cover the deterministic parts: version classification and
predecessor selection, changelog section extraction over real heading styles,
the "see changelog" heuristics, advisory fix evidence (affirmative and
negated phrasings), payload byte budgets, and secret lookup and redaction
under `fmt`, `slog` and `json.Marshal`.

State-machine tests check the invariants the design depends on: GUIDs, pubDates
and feed membership survive every visible-state transition; the summary cache
key changes when any part of the request changes and matches for identical
ones; a scheduler with a hanging GitHub and a failing LLM still renders; each
cell of the outbox claim table; and `resummarize` and restarts never re-send an
alert.

Feed output is covered by golden RSS and Atom files and local spec checks.
`make validate` runs the W3C validator; it isn't part of every CI run.
