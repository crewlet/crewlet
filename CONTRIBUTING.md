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

## Pull requests

1. Fork and create a feature branch.
2. Make your change, including tests and doc updates.
3. Run the lint + test commands above.
4. Open a PR with a clear description of what changed and why. **The title
   matters more than anywhere else**: GitHub generates each release's notes from
   the titles of the pull requests merged since the previous tag, and those
   notes are the *only* record of what a release contains. Yours is read by
   people deciding whether to upgrade, so write it as a release note — what
   changed for someone running Crewlet — rather than as a commit log. Labels
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
