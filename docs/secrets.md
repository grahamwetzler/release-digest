# Secrets

Keys and tokens are kept out of the config and, in production, out of the
environment. The code is in [`internal/secret`](../internal/secret).

| Secret | Docker secret | File path in env | Plain env (development) |
|---|---|---|---|
| LLM API key | `/run/secrets/llm_api_key` | `LLM_API_KEY_FILE` | `LLM_API_KEY` |
| GitHub token | `/run/secrets/github_token` | `GITHUB_TOKEN_FILE` | `GITHUB_TOKEN` |
| ntfy access token | `/run/secrets/ntfy_token` | `NTFY_TOKEN_FILE` | `NTFY_TOKEN` |
| ntfy URL, when not in the YAML | `/run/secrets/ntfy_url` | `NTFY_URL_FILE` | `NTFY_URL` |

The ntfy URL is on the list because on public ntfy.sh the topic name acts as a
password.

## Rules

- **One source, or a startup error.** A source counts as present if its env
  var is set (even to an empty string) or, for the default path, if the file
  exists. If more than one is present, startup fails and names each one, for
  example `LLM_API_KEY and /run/secrets/llm_api_key are both set; remove one`.
  There's no precedence to remember, and a stale env var can't silently shadow
  a mounted secret. The same goes for an ntfy URL set both as a secret and in
  `notify.ntfy.url`.
- **No source is fine.** GitHub then runs unauthenticated at 60 requests an
  hour, with a startup warning. The LLM client sends no `Authorization`
  header, which suits a local Ollama. `config lint` warns when `llm.base_url`
  isn't a loopback or private address and there's no key.
- **`*_FILE` is the standard Docker convention**, the one the official
  Postgres and MySQL images use. The default `/run/secrets/` path means a
  compose `secrets:` entry works with no env wiring at all.
- **File handling.** A file is read at startup, and surrounding whitespace,
  including the trailing newline `echo > file` leaves, is trimmed. A file
  that's empty after trimming, unreadable, or (for `*_FILE`) missing is a
  fatal error naming the path. A plain env var set to an empty string is also
  fatal.
- **Not in YAML.** The config loader rejects `api_key` and `token` keys, so a
  key can't end up committed with a config file.

## Rotation

`SIGHUP`, or restarting the container, re-reads every secret file. There's no
polling. If any secret fails to load, none is replaced. A replacement GitHub
token must pass the [token guard](public-only.md#token-guard) before it
replaces the active one. A `SIGHUP` can rotate the ntfy URL but can't turn
alerts on or off.

## Redaction

Secret values are held in a `secret.String` whose `String()`, `GoString()`,
`MarshalJSON()` and `LogValue()` all return `[redacted]`. HTTP clients set
`Authorization` from it at request time, and the LLM and GitHub clients strip
request headers from errors before wrapping them. `config lint` and the
startup log report only each secret's **source**, for example
`llm api key: file /run/secrets/llm_api_key (51 bytes)`.

## File permissions

Compose file-based secrets are bind mounts that keep the host file's owner and
mode. The container runs as distroless `nonroot` (uid 65532), so the file must
be readable by that uid: either `chown 65532` with mode `0400`, or mode `0444`
inside a `0700` directory you own. `config lint` reports an unreadable secret
file with that hint. Swarm secrets can set `uid: "65532"` and `mode: 0400`
directly.
