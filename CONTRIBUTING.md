# Contributing to Crewlet

Thanks for your interest in contributing! This document covers everything you
need to get a development environment running and land a change.

## Development setup

Prerequisites: **Python 3.12+**, **[uv](https://docs.astral.sh/uv/)**, and
**Docker** (for the local Pulsar + PostgreSQL stack).

```bash
git clone https://github.com/crewlet/crewlet.git
cd crewlet
uv sync --all-extras          # installs crewlet + all extras + dev tools

# Infrastructure for integration-style runs (not needed for unit tests)
cp .env.example .env
docker compose up -d          # Pulsar + PostgreSQL (TimescaleDB + pgvector)
```

## Running tests and lint

```bash
uv run pytest                          # full test suite (no external services needed)
uv run ruff check src/ tests/          # lint
uv run ruff format --check src/ tests/ # formatting
```

CI runs exactly these three commands — please make sure all of them pass
before opening a pull request. `ruff check --fix` and `ruff format` fix most
issues automatically.

### Integration tests, and why a skip is not a pass

Some suites need real infrastructure and **skip silently without it** — a
green run on a laptop with nothing up has simply not exercised them:

```bash
docker compose up -d                                    # Pulsar + PostgreSQL
export CREWLET_TEST_DSN=postgresql://crewlet@localhost/crewlet
uv run pytest -m integration -s                         # -s to see measurements
```

- **`CREWLET_TEST_DSN`** adds a third parameterisation to the storage
  contract suites, running them against a real PostgreSQL alongside the
  memory twin and the SQL fake. The fake can only confirm that SQL means
  what its author thought; it cannot catch a statement PostgreSQL rejects.
  **CI runs this one**: the `test` job brings up the same PostgreSQL image
  the compose file uses, applies the migrations and sets the variable, so a
  statement the server rejects fails the pull request rather than the
  deployment. Setting it locally still matters — it is how you find that out
  before pushing.

  A new store belongs in that parameterisation from its first commit.
  Asserting that a statement *contains* some text, or that a fake returns
  what the test put in it, proves the author's intent and nothing about the
  server: it cannot tell a correct statement from one PostgreSQL refuses to
  parse, and it cannot test exclusivity at all — an at-most-once claim is a
  property of the statement, not of the dict standing in for it.
- **`node`** runs the dashboard's suites. `src/crewlet/static/dashboard/`
  is a zero-build ES-module app — no package.json, no node_modules, no test
  runner — so `tests/test_dashboard/js/*.test.mjs` execute under whatever
  `node` is on PATH, driven by a pytest wrapper. Without one they skip, and
  since the dashboard has almost no Python, that is a whole subsystem going
  quiet. **CI runs these too**, and unlike the others here it *fails* rather
  than skipping when node is missing: the `test` job installs one if the
  runner image does not ship it, and the wrapper asserts it independently.
  Any `node` recent enough for ES modules will do.

- **`tests/test_queue/test_broker_behavior.py`** measures the broker
  behaviours the multi-node design rests on (redelivery timing, cursor
  continuity across a subscription's change of owner, prefetch size) and
  prints the numbers under `-s`. Run it after any Pulsar upgrade: it is
  where a changed broker behaviour should fail, rather than in a
  production handoff. **CI runs this one too**, along with the real-broker
  half of `tests/test_fleet`: the `test` job starts a standalone broker with
  the same image and command the compose file uses. A service container
  cannot supply that command, so it is a plain `docker run` step.

  Between that and `CREWLET_TEST_DSN`, every backend the suite knows how to
  exercise is real on every pull request. What that buys is the part of the
  design that is a *claim about the broker* rather than about our code — a
  close-driven handoff returning a seat's mail at `redeliveryCount` 0, the
  cursor surviving a change of owner, a wedged consumer holding its prefetch
  for the ack timeout. The memory twin models all three, and a twin agreeing
  with itself proves nothing about Pulsar. Run it locally before a Pulsar
  upgrade anyway, under `-s`, where the measurements print.

CI also builds the distributions on every pull request, because a packaging
break is otherwise invisible until the tag that publishes it. If you touched
`pyproject.toml`, `README.md`, or anything under `src/crewlet/` that is not a
`.py` file, run that check too:

```bash
uv run python -m build && uv run twine check --strict dist/*
```

Tests never call real LLM APIs — use the mock providers in the test suite.
Test files mirror the source layout: `src/crewlet/queue/protocol.py` is
covered by `tests/test_queue/test_protocol.py`.

## Project conventions

- **Python 3.12+ syntax** — `X | Y` type unions, `match` where it helps.
- **Pydantic v2** for data models; `typing.Protocol` for provider interfaces.
- **asyncio everywhere** — any function that does I/O is `async def`.
- **Structured logging** via `structlog` (`from crewlet._logging import get_logger`);
  never stdlib `logging` directly. Event names are snake_case, dynamic data goes
  in keyword arguments.
- **Type annotations** on every public class and function.
- **Docs are part of the change** — any change to public APIs, config formats,
  CLI commands, or behavior must update the relevant page under `docs/`.
- **Diagrams are Mermaid** — architecture, flow, sequence, and state diagrams go
  in ` ```mermaid ` fences, not ASCII box art. GitHub renders them inline, and
  the docs site renders them in the Crewlet design system. Directory trees and
  file listings stay plain code fences — they are literal output, not diagrams.

## Dependency updates

Dependabot watches all three dependency surfaces this repository has, and
opens a pull request when any of them has a newer release:

| Surface | Ecosystem | What it covers |
|---|---|---|
| `.github/workflows/*.yml` | `github-actions` | the actions CI and releases run on |
| `pyproject.toml` | `pip` | the package's own requirements |
| `docker-compose.yml` | `docker-compose` | the images the local dev stack runs |

The configuration is [`.github/dependabot.yml`](.github/dependabot.yml): three
entries on a weekly schedule, and nothing else. Each pull request is reviewed
on its own merits, and CI runs on it — for the actions and Compose surfaces
that is most of the review. Two things are worth knowing:

- **Python pull requests are rare.** `pyproject.toml` declares open floors
  (`pydantic>=2.0`), which already admit new releases, so a routine upstream
  version needs no change here. A *capped* requirement is what produces one:
  `mcp>=2,<3` is capped so that the next major arrives as a pull request to
  migrate deliberately, with CI already run against it.
- **Some Compose images are pinned on purpose**, with the reason in a comment
  beside each — `plane-db` holds postgres on its major to match Plane's own
  compose file and to keep an existing volume readable. Closing that pull
  request is the right answer; Dependabot does not reopen one for a version
  you have already turned down.

Adding a new dependency surface — the first `Dockerfile`, a `package.json` —
means adding its `updates:` entry in the same change. Nothing reports the
omission: a manifest Dependabot has not been told about simply never produces
a pull request, which is indistinguishable from one that has nothing to
update.

### An extra carries its own surface's requirements

`crewlet[all]` is the union of the runtime extras, and `tests/test_packaging`
asserts that. What the union *cannot* tell you is whether each extra carries
what its own surface needs, because a package can reach `all` through some
other extra while the one that actually needs it still lacks it — and the
union check passes either way.

That is not hypothetical. `crewlet[api]` served a dashboard whose entire data
plane is a WebSocket while pinning no WebSocket implementation: `websockets`
reached `all` through the `mattermost` extra, so nothing failed. Bare
`uvicorn` answered the upgrade with a 404, the dashboard fell back to polling
`/stream/snapshot` every five seconds, and every `crewlet[api]` install ran
in degraded mode indefinitely with nothing on screen to say so.

So when an extra's surface needs a package to work at all, name it in that
extra — and pin the requirement with a test, since the union check will not.

Security updates are separate: they come from published advisories rather than
this file, are enabled in the repository's settings, and are held back by
neither the weekly schedule nor Dependabot's default release cooldown.

## Releasing

Releases are cut by pushing a `v*` tag; GitHub Actions builds, verifies, and
publishes to PyPI over OpenID Connect. The full process — including the
one-time trusted-publisher setup and how to rehearse on TestPyPI — is in
[RELEASING.md](RELEASING.md).

## Documentation

The pages under `docs/` are the source of truth and are published to
[docs.crewlet.ai](https://docs.crewlet.ai). Keep them plain CommonMark: relative
`.md` links between pages, figures in `docs/assets/`, one `# Heading` per page.
The site derives its navigation from `docs/index.md`, so **a new page must be
linked there** — the site build fails on a page nothing links to.

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
| `ci` | `.github/workflows/*`, `.github/dependabot.yml`, release tooling |
| `build` | `pyproject.toml`, packaging metadata, the Compose stack |
| `chore` | anything else with no user-visible effect (dependency bumps, tidying) |
| `revert` | reverting an earlier commit |

### Scope

The scope names the **component** the change lands in. For code that is the
package directory under `src/crewlet/` — `agent`, `sandbox`, `plane`, `api`,
`db`, `secrets`, `knowledge`, `notifications`, `schedule`, `tools`, `mcp`, and
so on — so a reader can go from a subject line to a directory without guessing.
Use `dashboard` for `src/crewlet/static/dashboard/`, which is the component
people call it. Outside the package, scope by area: `docs`, `deps`,
`packaging`, `examples`, `schema`, `scripts` — and for CI, the workflow's own
name (`ci(release)`, `ci(docs-publish)`).

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
fix(plane): relax the page-search query when the server returns no hits
perf(providers): cache the tool-definition prefix on Anthropic calls
docs(secret-store): document that the keyring is required
ci(docs-publish): rebuild the docs site when a release ships
chore(deps): bump ruff to 0.14
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
