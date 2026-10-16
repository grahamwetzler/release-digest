# Public repositories only

Release Digest only handles public repositories. Content from a private repo
would otherwise be sent to a third-party LLM and published in an
unauthenticated feed. The rule is enforced by checking the repo itself,
fail-closed, before anything is fetched or exposed. The GitHub token is not
relied on.

## The per-poll check

Before every poll of a repo, `GET /repos/{owner}/{repo}` runs with
`If-None-Match`. The tick's visibility sweep ([Architecture](architecture.md#visibility-first))
is this check: a repo is polled only if the sweep confirmed it in the same
tick. When authenticated, an unchanged repo is a 304 and costs no rate limit.

The repo is used only if this tick's response (or its validated 304) says
`private: false`. If it says `private: true`, or the check fails for any
reason (an error, a timeout, a rate limit), nothing else is fetched for that
repo this tick.

GitHub reports `private: true` for a private repo whatever the token's
permissions, so a repo that turns private is caught before any of its content
is requested or exposed.

## Holding a repo

On `private: true` **or a 404** (a repo that's gone private looks like a 404 to
a token without access), the repo is marked **held** and an error is logged.
`config lint` fails on it.

Holding isn't terminal. Rows keep their state; nothing is skipped or
cancelled. Already-published items stay in the feed as they are, since they
were public when published. When a held repo passes a check again it's
un-held: its `pending` rows resume, queued alerts are delivered if still
within `max_age`, and the scan picks up anything released meanwhile, because
the scan boundary didn't advance while the repo was held.

## The processing gate

Any action that would newly expose a repo's content has to pass two checks.

1. **Tick confirmation** (scheduler stages only). The repo's
   `visibility_confirmed_tick` equals the running tick's ID: it passed this
   tick's sweep. A confirmation from an earlier tick never counts. The outbox
   worker is exempt, because it runs between ticks; its just-in-time check,
   taken seconds before the send, is stricter on its own.
2. **Just-in-time check.** Immediately before each exposing action, a
   conditional `GET /repos/{o}/{r}` must return `private: false`. That means
   before each LLM call, before each render's first publication of a repo's
   items (one check per repo per render), and before each ntfy send.

A just-in-time check that fails, returns 404 or says private **invalidates**
the tick confirmation at once. If it says private or 404, it also holds the
repo. Nothing else for that repo runs this tick, and a public answer recorded
later in the same tick doesn't restore the confirmation.

The gate covers:

- GitHub content fetches, notes resolution, LLM calls, advisory lookups,
  metadata and existence checks
- new alert decisions
- first publication: render-time promotion out of `pending`
- outbox delivery

The rule is the same whether the check failed, timed out, returned 404 or
said private: no fresh confirmation, no exposure.

**Trade-off.** A transient GitHub outage delays first publication and alerts
until a check succeeds. That's preferred to risking disclosure of a repo that
just went private.

**Residual exposure.** A repo can flip to private in the seconds between a
just-in-time check and the request that follows it. Content fetched in that
window was public moments earlier. This is accepted.

## Token guard

GitHub has no API that proves a token is public-only. A fine-grained token
scoped to "selected repositories" that happen to be public today looks the
same as a public-only token. So the token guard is defense in depth: it catches
obvious misconfiguration and claims nothing more.

- For a classic token, if the `X-OAuth-Scopes` header includes `repo`,
  startup fails.
- For any token, `GET /user/repos?visibility=private&per_page=1` returning
  anything fails startup.
- The startup log says the token was checked for *obvious* private access
  only.
- `SIGHUP` runs the guard on a replacement token *before* it's activated. If
  the new token fails, the old one stays in use and an error is logged.

The recommended token is fine-grained, with the **Public repositories
(read-only)** access level and no permissions, which can't read private repos
even by mistake.
