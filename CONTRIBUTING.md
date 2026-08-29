# Contributing to Crewlet

Thanks for your interest in contributing! This document covers everything you
need to get a development environment running and land a change.

## Development setup

Prerequisites: **Go 1.27+** and **node** (any version with ES modules — the
dashboard's suites use `node:assert` and nothing else). **Docker** only for
the vendor loops below; the engine itself needs no services.

```bash
git clone https://github.com/crewlet/crewlet.git
cd crewlet
go build ./...
```

That is the whole setup. The engine embeds its own event stream (a NATS
JetStream server) and its store is a local file it creates, so `crewlet run`
in an empty directory is a working company — there is no broker to operate
and nothing to point a DSN at.

The Docker stack is for the **self-hostable integrations**: Mattermost and
GitLab, each behind its own compose profile with a bootstrap script that
stands it up and provisions the example company's seats. Jira and Confluence
have no profile — Atlassian is not something a compose file can stand up. See
[docs/integrations/](docs/integrations/).

## Running tests and lint

```bash
go test ./...            # the full suite
gofmt -l .               # formatting — prints the files that need it
go vet ./...
golangci-lint run        # what CI's lint job runs
```

CI runs those, plus the suite again **under the race detector**
(`go test ./... -race`). That is not optional here: the engine's concurrency
model is real parallelism, and every "atomic because it is single-threaded"
assumption is a data race until proven otherwise. Run it locally before
pushing anything that touches the seat host, the queue or the turn engine.

### A skip is not a pass

Some suites need something the machine may not have and **skip silently
without it** — a green run has simply not exercised them.

- **`node`** runs the dashboard's suites. `static/dashboard/` is a zero-build
  ES-module app — no package.json, no node_modules, no test runner — so
  `tests/dashboard/js/*.test.mjs` execute under whatever `node` is on PATH,
  driven from Go by `internal/api`. The dashboard has no Go code of its own,
  so without node a whole subsystem goes quiet. **CI fails rather than
  skipping** when node is missing: the job installs one if the runner image
  does not ship it, and the test asserts the workflow asks for it.

  **Which dashboard the suites test is a parameter.** They resolve it once,
  in `tests/dashboard/js/dashboardRoot.mjs`, from `CREWLET_DASHBOARD_ROOT` —
  unset, it is `static/dashboard`, the tree the binary embeds. Spell the path
  in a suite instead and it becomes a fact that can only ever be corrected in
  most of the places it appears.

- **`CREWLET_STORE_DRIVER`** selects the store implementation. Two are
  certified — `turso` (the default) and `sqlite` (mainline SQLite, pure Go) —
  and **every statement in `internal/store` must parse on both**. CI runs the
  store suites twice, once per driver, because Turso's dialect is currently
  the narrower of the two and nothing else catches a statement that only one
  of them accepts.

  Turso also carries a native library — pure Go to *build* (no cgo), but its
  engine is a ~20 MB binary embedded in the driver and extracted at runtime
  into `$TURSO_GO_CACHE_DIR` (default `~/.cache/turso-go`), shared by every
  process on the machine. Upstream writes it without a rename and panics on
  what a concurrent reader sees, so `internal/store/turso.go` prepares it
  under a lock and heals a cache entry that will not verify. If a test binary
  ever dies with `unable to load turso library`, that cache is the place to
  look — `go test ./internal/store/ -run LibraryCache` covers it.

  ```bash
  CREWLET_STORE_DRIVER=sqlite go test ./internal/store/...
  ```

- **`CREWLET_TEST_PULSAR_URL`** and **`CREWLET_TEST_PULSAR_ADMIN_URL`** run
  the Pulsar conformance suite, which **skips entirely** without them — and
  skipping is not passing, since that job is the only place the Pulsar
  backend is certified at all. CI starts a standalone broker for it, with the
  reapers that would otherwise eat an unowned seat's mailbox turned OFF:
  certifying against a broker configured differently from a real deployment
  certifies the wrong broker.

  What this buys is the part of the design that is a *claim about the broker*
  rather than about our code — a close-driven handoff returning a seat's mail
  at redelivery count 0, a cursor surviving a change of owner, a wedged
  consumer holding its prefetch for the ack timeout. The memory twin models
  all three, and a twin agreeing with itself proves nothing.

Tests never call real LLM APIs — use the fakes in `internal/providers`. Test
files sit beside what they cover, as Go expects.

### The end-to-end gates

`internal/e2e` runs a real engine, a real broker and the real API, then
replays the frames its socket produced through the dashboard's own
`store.js`. Both halves of the wire protocol are checked against each other
there and nowhere else, so it needs node too:

```bash
go test ./internal/e2e/... -race -v
```

## Project conventions

- **Go 1.27+**, and the module pins its own toolchain — `GOTOOLCHAIN=auto`
  fetches it rather than failing on a mismatch.
- **Interfaces are defined by the consumer**, in the package that calls them,
  and kept to what that caller needs.
- **`context.Context` first, always**, and threaded rather than stored. The
  exception is a rollback, which takes `context.WithoutCancel`: the failure
  it is undoing is often the cancellation itself, and a cleanup that inherits
  a dead context does nothing at all.
- **Structured logging** via `internal/logging` (`log := logging.Get("component")`);
  never the bare `slog` default. Event names are snake_case, dynamic data
  goes in key/value pairs, and a message is a name rather than a sentence.
- **Errors say what to do about it.** An error a person will read names the
  field, the file or the variable they have to change — not just what failed.
- **Comments explain WHY**, and especially why an obvious alternative is
  wrong. The diff shows what the code does.
- **Docs are part of the change** — any change to public APIs, config
  formats, CLI commands, or behavior must update the relevant page under
  `docs/`.
- **Diagrams are Mermaid** — architecture, flow, sequence, and state diagrams
  go in ` ```mermaid ` fences, not ASCII box art. GitHub renders them inline,
  and the docs site renders them in the Crewlet design system. Directory
  trees and file listings stay plain code fences — they are literal output,
  not diagrams.

## A build that compiles is not a backend that works

Go's compiler proves the types line up. It cannot prove a backend is wired,
and this codebase has two shapes where it is not:

- **A config value with no implementation.** `providers.sandbox.type: e2b`
  parses, validates, appears in the generated schema — and the engine refuses
  it at construction, because that backend is not built. That refusal is the
  right behaviour; what would be wrong is accepting it and failing later.
- **A knob nothing reads.** `provisioning.group_webhook` was validated,
  documented and consulted by no code at all, so an operator setting it got
  the default and no error. A config field ships with the code that reads it,
  or it does not ship.

The test for both is the same: a field added to a config struct needs a test
that a value in it changes what the engine *does*, not merely that it parses.

## Dependency updates

Dependabot watches every dependency surface this repository has, and opens a
pull request when any of them has a newer release:

| Surface | Ecosystem | What it covers |
|---|---|---|
| `.github/workflows/*.yml` | `github-actions` | the actions CI and releases run on |
| `go.mod` | `gomod` | the engine's own Go dependencies |
| `Dockerfile` | `docker` | the base image a release ships |
| `docker-compose.yml` | `docker-compose` | the images the local dev stack runs |

The configuration is [`.github/dependabot.yml`](.github/dependabot.yml): one
entry per surface on a weekly schedule, and nothing else. Each pull request is
reviewed on its own merits, and CI runs on it. Three things are worth knowing:

- **Some Compose images are pinned on purpose**, with the reason in a comment
  beside each — a database image holds its major to keep an existing volume
  readable, and Dekaf publishes no floating tag. Closing that pull request is
  the right answer; Dependabot does not reopen one for a version you have
  already turned down.
- **`docker` and `docker-compose` are two ecosystems, not one.** The first
  reads `Dockerfile`s and the second reads Compose files; neither sees the
  other's manifests, so a repository with both needs both entries.
- **An action pinned to a non-version ref is invisible.** A branch pointer
  (`@release/v1`) or `@main` yields no update pull requests at all, so pin
  actions to a version tag or a full SHA. That matters most for the one
  handed an OIDC token: pinned, its updates arrive as reviewable pull
  requests instead of moving under the workflow unannounced.

Adding a new dependency surface — a `package.json`, a second module — means
adding its `updates:` entry in the same change. Nothing reports the omission:
a manifest Dependabot has not been told about simply never produces a pull
request, which is indistinguishable from one that has nothing to update. The
Go module itself went the length of the rewrite that way.

Security updates are separate: they come from published advisories rather than
this file, are enabled in the repository's settings, and are held back by
neither the weekly schedule nor Dependabot's default release cooldown.

## Releasing

Releases are cut by pushing a `v*` tag; GitHub Actions builds every target
with [goreleaser](https://goreleaser.com), signs the checksums with keyless
Sigstore, publishes the container image to GHCR and creates the GitHub
Release. **The tag is the version** — nothing in the tree records one, so
there is nothing for a tag to disagree with. The full process, including how
to rehearse the whole pipeline locally without touching GitHub, is in
[RELEASING.md](RELEASING.md).

## Documentation

The pages under `docs/` are the source of truth and are published to
[docs.crewlet.ai](https://docs.crewlet.ai). Keep them plain CommonMark: relative
`.md` links between pages, figures in `docs/assets/`, one `# Heading` per page.
The site derives its navigation from `docs/index.md`, so **a new page must be
linked there** — the site build fails on a page nothing links to.

`docs/` is written for people *running* Crewlet. Reasoning aimed at people
*changing* it goes in a package doc, where `go doc` surfaces it beside the
code — or, when a change makes a call a future reader would plausibly reverse,
in [`decisions/`](decisions/README.md). Cite a decision from the code it
governs: one nothing points at is one nobody will find.

## Commit messages

Commit subjects follow the [Conventional Commits](https://www.conventionalcommits.org/)
shape — a type, an optional component scope, then the summary:

```
type(scope): summary

optional body explaining why, wrapped at 72 columns

Fixes #123
```

### Type

Required, and drawn from this set:

| Type | Use for |
|---|---|
| `feat` | a new capability — a config field, CLI command, tool, endpoint, provider |
| `fix` | a bug fix, including correcting a tuning value that was wrong |
| `docs` | pages under `docs/`, the root markdown files, docstrings |
| `refactor` | a change that neither fixes a bug nor adds a capability |
| `perf` | a change made for speed, memory, or token cost |
| `test` | tests only |
| `ci` | `.github/workflows/*`, `.github/dependabot.yml` |
| `build` | `go.mod`, the `Dockerfile`, `.goreleaser.yaml`, the Compose stack |
| `chore` | anything else with no user-visible effect (dependency bumps, tidying) |
| `revert` | reverting an earlier commit |

### Scope

The scope names the **component** the change lands in. For code that is the
package directory under `internal/` — `agent`, `api`, `coord`, `engine`,
`gitlab`, `knowledge`, `mattermost`, `mcp`, `notify`, `queue`,
`sandbox`, `schedule`, `seat`, `secrets`, `store`, `tools`, and so on — so a
reader can go from a subject line to a directory without guessing. Two scopes
name a component by the name people use rather than by its import path:
`dashboard` for `static/dashboard/`, and `cli` for `cmd/crewlet/`. Outside
`internal/`, scope by area: `docs`, `deps`, `examples`, `schema`, `scripts` —
and for CI, the workflow's own name (`ci(release)`, `ci(docs-publish)`).

Drop the scope only when a change genuinely spans the whole repository
(`chore: normalise line endings`). The type is never optional.

### Summary

- Imperative present tense — "add", not "added" or "adds".
- Lowercase first word (unless it is an identifier), no trailing period.
- 72 characters or fewer, prefix included.
- Say what changed for someone using the code, not which file you edited.

Put the *why* in the body; the diff already shows the *what*. Reference issues
in a footer (`Fixes #123`). A breaking change gets a `!` before the colon —
`feat(config)!: …` — and a `BREAKING CHANGE:` footer describing the migration.

### One change per commit

Keep unrelated work in separate commits, each with its own scope. A bug you
fixed in passing, or a tuning value you corrected while doing something else,
belongs in its own commit — that keeps it reviewable, and revertable, on its
own.

### Examples

```
feat(sandbox): reuse a paused box when a clarification is answered
fix(confluence): relax the CQL query when the server returns no hits
perf(providers): cache the tool-definition prefix on Anthropic calls
docs(secret-store): document that the keyring is required
build: stamp the version into the binary at link time
ci(docs-publish): rebuild the docs site when a release ships
chore(deps): bump golangci-lint to 2.6
```

Nothing enforces any of this — there is no commit-message hook and CI does not
check subjects. Git history is permanent and public, so the convention holds
only because contributors apply it.

**This applies to pull request titles too.** A pull request lands on `main` as a
single squashed commit whose subject is the pull request title, so the title
takes the same `type(scope): summary` form — and everything after the colon is
what readers see in the generated release notes. See below.

## Pull requests

1. Fork and create a feature branch.
2. Make your change, including tests and doc updates.
3. Run the lint + test commands above.
4. Open a PR with a clear description of what changed and why. **The title
   matters more than anywhere else**: GitHub generates each release's notes from
   the titles of the pull requests merged since the previous tag, and those
   notes are the *only* record of what a release contains. Yours is read by
   people deciding whether to upgrade, so the summary after the
   `type(scope):` prefix has to read as a release note — what changed for
   someone running Crewlet — rather than as a commit log. Labels
   group it: Dependabot's `dependencies` label sorts bumps to the bottom (see
   `.github/release.yml`).

Small, focused PRs are much easier to review than large ones. If you are
planning a substantial change — a new subsystem, or a reshaping of the config
format — please open an issue first to discuss the approach.

## Reporting bugs and requesting features

Use [GitHub issues](https://github.com/crewlet/crewlet/issues). For bugs,
include the engine version (`crewlet --version`), your config shape (redact
secrets!), and relevant structured-log output.

## Security issues

Please do **not** open public issues for security vulnerabilities — see
[SECURITY.md](SECURITY.md).
