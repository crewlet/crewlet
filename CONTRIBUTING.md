# Contributing to Crewlet

Thanks for your interest in contributing! This document covers everything you
need to get a development environment running and land a change.

## Development setup

Prerequisites: **Go 1.27+**, **node** (any version with ES modules — the
dashboard's suites use `node:assert` and nothing else) and
**[golangci-lint](https://golangci-lint.run/welcome/install/)**, which
`make check` runs and CI's own job downloads for itself. **Docker** only for
the vendor loops below; the engine itself needs no services.

```bash
git clone https://github.com/crewlet/crewlet.git
cd crewlet
make build        # or: go build ./...
```

`make help` lists every target, and that is the whole setup — nothing to
install beyond the prerequisites above. The engine embeds its own event
stream (a NATS JetStream server) and its store is a local file it creates, so
`crewlet run` in an empty directory is a working company — there is no broker
to operate and nothing to point a DSN at.

The Docker stack is for the **self-hostable integrations**: Mattermost and
GitLab, each behind its own compose profile with a bootstrap script that
stands it up and provisions the example company's seats — `make mattermost-up`
and `make gitlab-up` are the profile and its script in one step, and take
`COMPANY=` to provision a config's seats in the same run. Jira and Confluence
have no profile — Atlassian is not something a compose file can stand up. See
[docs/integrations/](docs/integrations/).

## Running tests and lint

```bash
make check               # every gate CI runs on a pull request
```

That is one target because it is one question — *would CI accept this?* —
and answering it in pieces is how a push goes red on the piece you skipped.
`make help` lists the rest.

Every target runs the command [`ci.yml`](.github/workflows/ci.yml) runs, with
the same flags. Nothing asserts the two have not drifted — a test did, and it
was dropped — so a convenience target that quietly loses `-race` would report
a pass CI does not honour and nothing else would notice. Change a target and
its `ci.yml` step together, and read both. `make check` is:

```bash
gofmt -l .               # formatting — prints the files that need it
go mod tidy -diff        # go.mod / go.sum are already what tidy would write
go vet ./...
golangci-lint run        # what CI's lint job runs
go build ./...
go test ./... -race -count=1                                  # the full suite
# then, for each of CROSS_TARGETS (linux and darwin x amd64/arm64):
CGO_ENABLED=0 GOOS=$OS GOARCH=$ARCH go build ./...            # test-cross
```

The race detector is not optional here: the engine's concurrency model is
real parallelism, and every "atomic because it is single-threaded" assumption
is a data race until proven otherwise — so CI runs the *whole* suite under it
and so does `make test`. `-count=1` is the other half: without it a cached
PASS recorded before the change answers for the change. `make test-norace`
skips the detector when you want the faster loop, and says so.

`go mod tidy -diff` gates the half of tidiness nothing else notices. An
*under*-tidy module already fails loudly — a missing requirement or `go.sum`
entry stops `go build ./...` in every job — but the opposite direction is
silent: a `require` left behind when its last import was deleted, a stale
`go.sum` line or a wrong `// indirect` marker each build, test and
cross-compile green, then land as unrelated churn in whichever pull request
next runs `make tidy`, on top of whatever that one was for. `-diff` prints the
patch and exits non-zero *without* writing the files, so neither CI nor
`make check` rewrites the tree it is judging — run `make tidy` to apply it.

One thing `make check` does not cover, because it needs a service CI starts
for itself — it prints it when it passes, rather than letting a green run
imply more than it did: the [release pipeline](#releasing) (`make snapshot`).

The queue conformance suite is not a second gap. Both backends behind the
`EventQueue` contract — the in-memory twin and JetStream — run the single
suite in `internal/queue/queuetest` under a plain `go test`, because the
JetStream backend brings its own broker: `internal/queue/jetstream` starts an
embedded NATS server per queue under test and shuts it down with the test.
Per queue rather than per binary, deliberately — the suite asserts things like
"a subscription nobody created retains nothing", which a broker shared with
another subtest's streams could satisfy by accident. So there is no service to
start, no environment variable to set and no compose profile to remember:
`make check` certifies the backend the engine actually ships, on every run.

### A skip is not a pass

Some suites need something the machine may not have and **skip silently
without it** — a green run has simply not exercised them.

- **`node`** runs the dashboard's suites. `static/dashboard/` is a zero-build
  ES-module app — no package.json, no node_modules, no test runner — so
  `tests/dashboard/js/*.test.mjs` execute under whatever `node` is on PATH,
  driven from Go by `internal/api`. The dashboard has no Go code of its own,
  so without node a whole subsystem goes quiet. **CI fails rather than
  skipping** when node is missing: the suite itself fails the run outright
  when `CI` is set and node is off `PATH`, and the `test` job installs one if
  the runner image stops shipping it. That install step is a convenience, not
  a guard — nothing asserts it is still there, and nothing needs to, because
  the suite goes red either way. Locally, every `make` target that runs a
  suite refuses to start without node, for the same reason — a target cannot
  install it for you, but it can decline to hand you a green run that tested
  none of the dashboard.

  **Which dashboard the suites test is a parameter.** They resolve it once,
  in `tests/dashboard/js/dashboardRoot.mjs`, from `CREWLET_DASHBOARD_ROOT` —
  unset, it is `static/dashboard`, the tree the binary embeds. Spell the path
  in a suite instead and it becomes a fact that can only ever be corrected in
  most of the places it appears.

- **`TURSO_GO_CACHE_DIR`** is where the store driver's native library lives,
  and it is the one environment variable a store test can be defeated by.

  Turso is the only store driver — `CREWLET_STORE_DRIVER` and the Tier A
  `store.driver` field are both retired, so there is
  no dialect intersection to keep statements inside any more and no second
  suite run. Write `internal/store` against Turso.

  It is pure Go to *build* (no cgo), but its engine is a ~20 MB binary
  embedded in the driver and extracted at runtime into `$TURSO_GO_CACHE_DIR`
  (default `~/.cache/turso-go`), shared by every process on the machine.
  Upstream writes it without a rename and panics on what a concurrent reader
  sees, so `internal/store/turso.go` prepares it under a lock and heals a
  cache entry that will not verify. If a test binary ever dies with `unable to
  load turso library`, that cache is the place to look — `go test
  ./internal/store/ -run LibraryCache -count=1` covers it (the cache is not a
  test input, so without `-count=1` Go will happily serve the PASS from before
  it went bad).

  It also decides what this project can be built for. Upstream embeds the
  library for linux and darwin on amd64 and arm64 and for windows/amd64;
  Crewlet ships the first four, and `internal/store/platform.go` turns a build
  for anything else into a compile error that says why rather than a binary
  that fails at its first query.

  **The linux binary is not static, and must not be made static.** purego
  declares its `dlopen` imports with `//go:cgo_import_dynamic`, so
  `CGO_ENABLED=0 go build` still emits a dynamic executable — `interpreter
  /lib64/ld-linux-x86-64.so.2`, `NEEDED libc.so.6`. It needs glibc and does not
  run on musl, and `-tags musl` does NOT change that: the tag picks which
  shared object the driver embeds, not this binary's own linkage. That is why
  there is no musl archive.

  Note what that means: `CGO_ENABLED=0` does not give a static binary here,
  even though it does for any other Go program (a hello-world on the same
  machine comes out static). purego's `dlfcn_nocgo_linux.go` is
  `//go:build !cgo` — the file that applies precisely when cgo is OFF — and it
  is the one declaring the dynamic imports.

  Adding `-extldflags -static` to a cgo-free build changes nothing either: the
  flag is for the external linker and a cgo-free build links internally.
  Forcing a static ELF needs `-linkmode external`, which needs cgo — and that
  binary, which `file(1)` does call static, SIGSEGVs on its first query on the
  machine that built it, because a static program cannot `dlopen`.
  The `cross` CI job asserts the artifact is still dynamic, so this is a red
  build rather than a release.

  ```bash
  # What CI cross-compiles on every pull request.
  make test-cross
  ```

Tests never call real LLM APIs — use the fakes in `internal/providers`. Test
files sit beside what they cover, as Go expects.

### The end-to-end gates

`internal/e2e` runs a real engine, a real broker and the real API, then
replays the frames its socket produced through the dashboard's own
`store.js`. Both halves of the wire protocol are checked against each other
there and nowhere else, so it needs node too:

```bash
make test-e2e   # go test ./internal/e2e/... -race -count=1 -v
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

Dependabot keeps a version moving once it is in the tree; **choosing one in
the first place goes the same direction — take the newest release, and pin it
exactly.** Establish what the newest is from the registry or index rather than
from memory, and write it as a literal version (`4.2.4`), never a floating tag:
`latest` and `@main` leave Dependabot nothing to bump, and a floating tag turns
a green run into a claim about a build nobody can name afterwards.
Holding a version back is still fine where there is a reason — put the reason in
a comment at the pin, as the Compose stack's Postgres image already does.

The configuration is [`.github/dependabot.yml`](.github/dependabot.yml): one
entry per surface on a weekly schedule, plus the commit prefix that surface's
bumps carry. CI runs on each pull request, and — as below — CI is what decides
whether it lands. Three things are worth knowing:

- **A Compose image can be held back on purpose**, with the reason in a
  comment beside the pin — `mattermost-db` holds its Postgres major because an
  existing `mattermost-pgdata` volume will not open under a newer one without a
  `pg_upgrade` or a dump/restore. Closing that pull request is the right
  answer; Dependabot does not reopen one for a version you have already turned
  down.
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

### A bump merges itself

[`.github/workflows/dependabot-merge.yml`](.github/workflows/dependabot-merge.yml)
approves a pull request Dependabot opened and queues it with `gh pr merge
--auto --squash`, so a bump lands without waiting on a maintainer. It does not
land without review: CI *is* the review a dependency change gets here, and
`--auto` holds the merge until every check `main`'s protection rule requires
has passed. A bump CI rejects sits there red until a person looks at it, which
is exactly where it would have sat anyway.

Two settings hold that property up and neither of them is in this repository's
files. `main`'s protection rule must **require** the `ci` checks — `--auto`
waits for the checks a rule names and for nothing else, so with no rule the
queue is empty and the bump merges the instant it is mergeable. And **Allow
auto-merge** must be on under Settings → General, or the step fails outright;
that failure is loud, a red check on the bump, which is the right way for it to
fail.

The job runs only when Dependabot is both the pull request's author *and* the
actor that triggered the run. That second condition is what stops the workflow
approving a commit a person pushed onto a Dependabot branch — anyone with write
access can push one. Its visible cost is that clicking **Update branch**
yourself leaves the pull request unapproved, because the run your click
triggered is skipped; `@dependabot rebase` re-pushes as Dependabot and recovers
it.

Because nothing retitles a bump before it becomes a permanent subject line, each
entry in `.github/dependabot.yml` pins the prefix its commits carry, and every
surface carries the same one: `build(deps)`. A bump moves a pinned version, which
is a change to what the project builds against whichever file records it — so an
action bump reads `build(deps)` rather than `ci(deps)`, matching the bumps
already in `main`. Left unset, Dependabot infers the prefix from how recent
commits are written, and an inference that changes its mind writes a bare
`Bump x from 1 to 2` straight into `main` and into the release notes. Dependabot
capitalises the `Bump` and offers no way not to, so a bump is the one subject
here that does not start lowercase.

Nothing checks any of that for you. `internal/version` used to assert both
halves of the author/actor guard and the `--auto --squash` flags; those tests
were dropped, and no linter in this repository reads a workflow file. Every one
of them fails silently — the workflow keeps running, it just starts running on
the wrong pull requests or merging before a check has reported — and the job
holds `contents: write` and `pull-requests: write`. Read the `if:` and the merge
command on any diff that touches
[`.github/workflows/dependabot-merge.yml`](.github/workflows/dependabot-merge.yml).

## Releasing

Releases are cut by pushing a `v*` tag; GitHub Actions builds every target
with [goreleaser](https://goreleaser.com), signs the checksums with keyless
Sigstore, publishes the container image to GHCR and creates the GitHub
Release. **The tag is the version** — nothing in the tree records one, so
there is nothing for a tag to disagree with. The whole pipeline rehearses
locally with `make snapshot`, without a tag and without touching GitHub; the
full process is in [RELEASING.md](RELEASING.md).

### Nothing is released until a tag ships it

The tag is also the project's only compatibility boundary — it is the one
thing an operator can pin, pull and run. So a surface no `v*` tag has ever
shipped has nobody behind it, and **you may break it outright**: rename the
config field, reshape the struct, drop the CLI flag, move the route. Do not
leave a deprecated alias, a both-spellings fallback, an adapter or a
vestigial flag behind — there is nothing on the other side of them to be
compatible with, and code with no caller is indistinguishable from code
whose caller nobody found.

Check rather than assume; `git tag --merged` lists the tags, and
`git log <newest tag>..HEAD -- <path>` says whether what you are changing is
inside one. As of the only release, `v0.1.0`, that answer is *nothing*: the
tag sits on the initial commit, which contains no Go at all — so no package,
config field, CLI command, event type, API route or schema file in this tree
has yet been in a release.

Two things a missing tag does **not** excuse, because neither is about
releases:

- **An applied migration is history, not source.** `schema_migrations` is
  keyed on the filename, so editing a file that already ran silently never
  re-runs it: a database that applied it keeps the old shape while the code
  assumes the new one. Reshape with a new numbered migration under
  `internal/store/schema/`.
- **A rolling upgrade puts two builds on one stream.** The event envelope
  evolves additive-only, because an unknown type must round-trip losslessly
  in both directions — a contract between peers, not between releases. The
  same holds for anything two builds share in the coordination store.

A free break is still a complete one: the rename lands everywhere in the same
change — `docs/`, `examples/`, the dashboard, the tests — and `schema/` is
regenerated, never hand-edited.

## Documentation

The pages under `docs/` are the source of truth and are published to
[docs.crewlet.ai](https://docs.crewlet.ai). Keep them plain CommonMark: relative
`.md` links between pages, figures in `docs/assets/`, one `# Heading` per page.
The site derives its navigation from `docs/index.md`, so **a new page must be
linked there** — the site build fails on a page nothing links to.

`docs/` is written for people *running* Crewlet. Reasoning aimed at people
*changing* it goes in a package doc, where `go doc` surfaces it beside the
code — including the part that matters most: why the obvious alternative is
wrong, and what it cost when it was tried. A comment that only restates what
the code does has not earned its line.

None of it binds. If a rationale no longer matches the engine, change the code
and rewrite the comment in the same commit — a doc comment describing a design
the tree no longer has is worse than none.

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
| `build` | `go.mod`, the `Dockerfile`, `.goreleaser.yaml`, the Compose stack — and any dependency bump, whichever file records it |
| `chore` | anything else with no user-visible effect (tidying, repository maintenance) |
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
Both mark a change an operator has to act on, so they belong to a surface a
release actually shipped; changing one no tag has carried breaks nobody and
takes neither (see [Nothing is released until a tag ships
it](#nothing-is-released-until-a-tag-ships-it)).

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
build(deps): bump golangci-lint-action to v9
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
3. Run `make check`.
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
