# Installation

## Prerequisites

- **Python 3.12+**
- **[uv](https://docs.astral.sh/uv/)** — used to run MCP servers via `uvx` (and the easiest way to install Crewlet from source)
- **Docker** — for the local Apache Pulsar + PostgreSQL stack (and the optional local Plane / GitLab profiles)

## Install from PyPI

```bash
pip install crewlet
```

Crewlet's optional dependencies are split into extras — install the ones your
deployment uses:

| Extra | Pulls in | Needed for |
|---|---|---|
| `postgresql` | `asyncpg` | Database-backed operation (the default): the per-agent diary vector store, the episodic store, token tracking, the TimescaleDB event store, and versioned company config |
| `api` | `starlette`, `uvicorn` | The REST API + dashboard + webhook receiver (embedded or standalone) |
| `sandbox` | `e2b` | The [code sandbox](../concepts/code-sandbox.md) — lets sandbox-enabled roles author code with a coding agent in an isolated E2B sandbox |
| `forge` | `pyjwt[crypto]` | Verifying Atlassian **Forge** webhook signatures (Jira/Confluence Cloud) |
| `all` | everything above | Kitchen sink |

```bash
pip install "crewlet[postgresql,api]"     # the typical minimum
pip install "crewlet[all]"                # everything
```

## Install from Source

```bash
git clone https://github.com/crewlet/crewlet.git
cd crewlet
uv sync --all-extras
```

This installs Crewlet with all extras plus dev tools (pytest, ruff). Prefix
commands with `uv run` (e.g. `uv run crewlet --version`) or activate the
`.venv` it creates.

## Install uv

uv is required at runtime to launch stdio MCP servers via `uvx` (e.g. the
Plane MCP server), and recommended for source installs:

```bash
curl -LsSf https://astral.sh/uv/install.sh | sh
```

## Infrastructure

Crewlet requires two services:

- **Apache Pulsar** — the persistent event queue every subsystem communicates
  through.
- **PostgreSQL with the TimescaleDB + pgvector extensions** — one database
  holds operational state, the per-agent diary vector store, the episodic
  store, the versioned company config, and the event store.

### Docker Compose (recommended)

The included `docker-compose.yml` provides both, plus web UIs:

```bash
cp .env.example .env    # copy default env vars (first time only)
docker compose up -d    # start Pulsar + PostgreSQL (+ UIs)
docker compose down     # stop and remove containers
```

| Service | Port | Details |
|---------|------|---------|
| Pulsar | 6650, 8080 | `apachepulsar/pulsar` image, standalone mode. 6650 = broker binary protocol (the engine connects here); 8080 = admin/REST |
| Dekaf (Pulsar UI) | 8090 | Pulsar web UI — topics, subscriptions, backlog, message browse |
| PostgreSQL | 5432 | TimescaleDB image — TimescaleDB + pgvector preloaded. User/pass: `crewlet/crewlet` |
| pgweb | 8150 | PostgreSQL web UI, auto-connected |

Pulsar runs in standalone mode (`bin/pulsar standalone --no-functions-worker
--no-stream-storage`). The web UI is
[Dekaf](https://pulsar.apache.org/docs/next/administration-dekaf-ui/) at
<http://localhost:8090> (auto-wired to the broker; Apache-2.0, no account
needed). The CLI works too, e.g.:

```bash
docker compose exec pulsar bin/pulsar-admin topics list public/default
```

Optional **profiles** in the same compose file bring up local instances of
the bigger integrations for end-to-end testing (none start by default):

```bash
docker compose --profile plane up -d              # self-hosted Plane fork (tracker + knowledge base)
docker compose --profile gitlab up -d             # local GitLab (code host)
docker compose --profile mattermost up -d --wait  # self-hosted Mattermost (chat)
```

Each pairs with a bootstrap script under `scripts/` that seeds the instance
and provisions the agent seats. See
[Plane § Local testing](../integrations/plane.md#local-testing),
[GitLab § Local testing](../integrations/gitlab.md#local-testing) and
[Mattermost § Local testing](../integrations/mattermost.md#local-testing).

(`--wait` is safe for the Mattermost profile — every service there has a
healthcheck. Do not add it to the Plane profile: its migrator is a one-shot
job whose clean exit `--wait` treats as a failure.)

### Bring your own infrastructure

Any reachable Pulsar cluster and PostgreSQL server work — point the Tier A
bootstrap config at them (`providers.queue.url`, `providers.database.dsn`).
The PostgreSQL server must have the TimescaleDB and pgvector extensions
available; migrations run automatically at engine start. To keep Crewlet's
topics off a shared Pulsar cluster's defaults, or to authenticate against the
broker, see [Deployment](../guides/deployment.md).

## Verify Installation

```bash
crewlet --version
```

Next: the [Quickstart](quickstart.md) brings up a four-agent company, and
[Choosing your stack](choosing-your-stack.md) walks through the external
services (LLM, tracker, code host, chat, sandbox) and their alternatives.
