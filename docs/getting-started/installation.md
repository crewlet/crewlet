# Installation

## Prerequisites

**None, to run the engine.** Crewlet is one static binary with no runtime to
install: it embeds its event stream (a NATS JetStream server) and its database
is a local file it creates. There is no broker to operate and nothing to point
a DSN at.

Two things are worth having anyway, for what runs *around* it:

- **[uv](https://docs.astral.sh/uv/)** — many MCP servers are launched with
  `uvx`, so a company whose roles use one needs it on the engine's PATH.
- **Docker** — only for the local integration loops (GitLab,
  Mattermost) and for the container mode of the
  [code sandbox](../concepts/code-sandbox.md).

## Install

```bash
go install github.com/crewlet/crewlet/cmd/crewlet@latest
```

Or take a signed release binary — every tag publishes archives for linux,
macOS and Windows on amd64 and arm64, plus a `checksums.txt` with a keyless
[Sigstore](https://www.sigstore.dev/) signature beside it:

```bash
# from https://github.com/crewlet/crewlet/releases
tar xzf crewlet_<version>_<os>_<arch>.tar.gz

cosign verify-blob checksums.txt \
  --certificate checksums.txt.pem --signature checksums.txt.sig \
  --certificate-identity-regexp '^https://github\.com/crewlet/crewlet/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum -c checksums.txt --ignore-missing
```

Or run the container image, `ghcr.io/crewlet/crewlet` — a Debian userland
rather than a distroless one, because the engine spawns process trees (stdio
MCP servers, and the local sandbox's coding agent).

A binary from `go install` reports its module version and one from a release
reports its tag; neither ever claims to be something it is not.

## Install from source

```bash
git clone https://github.com/crewlet/crewlet.git
cd crewlet
go build ./cmd/crewlet
```

That is the whole setup — see [CONTRIBUTING.md](https://github.com/crewlet/crewlet/blob/main/CONTRIBUTING.md)
for the test and lint commands.

## Infrastructure

**The engine needs none.** `crewlet run` in a directory with a config is a
working company:

- the **event stream** is a JetStream server inside the process. A deployment
  that outgrows one node points the same config slot at an external NATS or
  an Apache Pulsar cluster instead — see [Fleet](../guides/fleet.md).
- the **store** is one local file this process owns *exclusively*. Not a
  shared database, and no DSN: two engines pointed at one file corrupt it.
  Coordination between nodes goes through a separate KV slot, never the file.

### The compose file is for the integrations

`docker-compose.yml` in a repo checkout starts **nothing by default** —
every service in it is behind a profile, because none of them is the engine:

```bash
docker compose --profile gitlab up -d             # local GitLab (code host)
docker compose --profile mattermost up -d --wait  # self-hosted Mattermost (chat)
docker compose --profile pulsar up -d             # Pulsar + Dekaf, for the external-stream backend
```

Each of the two self-hostable integrations pairs with a bootstrap script under
`scripts/` that seeds the instance and provisions the agent seats. See
[GitLab § Local testing](../integrations/gitlab.md#local-testing) and
[Mattermost § Local testing](../integrations/mattermost.md#local-testing).
Jira and Confluence have no profile here — Atlassian is not something a
compose file can stand up.

The `pulsar` profile is for developing against the external stream backend,
and for running its conformance suite locally — that suite skips without
`CREWLET_TEST_PULSAR_URL`, and skipping is not passing. Dekaf, the web UI the
Pulsar docs recommend, comes up with it on <http://localhost:8090>.

Running any of these on a **remote host** rather than your own machine? Each
one has to be told the address browsers reach it on —
`MATTERMOST_PUBLIC_URL`, and GitLab's external URL — before the stack comes
up. For Mattermost that
setting also gates live updates, so getting it wrong looks like a working
install where messages only appear on refresh; the bootstrap script settles
it for you, and [The Site
URL](../integrations/mattermost.md#the-site-url) explains why.

(`--wait` is safe for the Mattermost profile — every service there has a
healthcheck.)

## Verify Installation

```bash
crewlet --version
```

Next: the [Quickstart](quickstart.md) brings up a four-agent company, and
[Choosing your stack](choosing-your-stack.md) walks through the external
services (LLM, tracker, code host, chat, sandbox) and their alternatives.
