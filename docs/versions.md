# Versions

Each tag is classified once, when its row is inserted, and the result is
stored. Filtering, predecessor selection, advisory fix evidence and rendering
all use it. The code is in [`internal/version`](../internal/version).

## Classification

- **Stream prefix and version.** A tag is split into an optional stream prefix
  and a version. The prefix is anything before the version: `service/api/` in
  `service/api/v1.4.0`, or `go` in `go1.28.1`. The version is parsed leniently
  as SemVer: an optional `v`, then 1–4 numeric components, then an optional
  pre-release suffix and build metadata.
- **Pre-release.**
  - For GitHub Releases, the `prerelease` flag **or** a parsed pre-release
    suffix marks it, since some projects forget the flag.
  - For tags, a SemVer pre-release segment (`-rc.1`) marks it, or a match of
    `prerelease_pattern` against the part after the numeric components. The
    default pattern is
    `(?i)^[-.]?(alpha|beta|rc|pre|preview|dev|nightly|snapshot|canary)\.?\d*`,
    so it also matches `go1.28rc1` and `1.0.0beta2`. It's overridable per repo.
- **Unrecognized tags** with no numeric version, such as `latest` or `stable`,
  are `skipped` unless the repo's `tag_pattern` explicitly matches them. Those
  tags are then ordered by date only.

## Predecessor selection

A release's predecessor (`prev_tag`) is used for the commit-log fallback in
[notes resolution](notes.md), the "Diff since" link, the LLM prompt's context
and advisory version evidence. It's chosen deterministically from stored rows,
including `skipped` ones:

1. Only versions in the **same stream prefix** are candidates, so monorepo
   package tags never cross.
2. For SemVer versions, the predecessor is the **highest version lower than
   this one** by SemVer precedence. That handles maintenance branches: the
   predecessor of `1.2.4` is `1.2.3` even when `2.0.0` shipped in between. A
   stable release skips pre-release candidates, so `1.3.0` compares against
   `1.2.x`, not `1.3.0-rc.2`. A pre-release considers every candidate.
3. For date-ordered tags, the predecessor is the latest one in the same stream
   published before this one.
4. With no candidate, `prev_tag` is empty: no commit-log fallback and no diff
   link.

## Monorepos

Stream prefixes keep predecessors correct when one repo tags many packages,
and `tag_pattern` picks which streams to follow. One GitHub release whose body
covers several packages is still one item.
