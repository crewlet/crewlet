# Installation

## Prerequisites

**A glibc userland, on linux.** Crewlet is one binary with no runtime to
install: it embeds its event stream (a NATS JetStream server) and its database
is two local files it creates. There is no broker to operate and nothing to point
a DSN at.

It is not, however, a *static* binary on linux, and it is worth knowing why
before you pick a base image. The store's database engine is a native library
loaded with `dlopen`, so the linux binaries are dynamically linked against
glibc (`libc.so.6`) — a pure-Go build does not avoid that. They will not run on
`scratch`, on a distroless image without a C library, or on Alpine and other
musl systems, where the failure is `no such file or directory` about a file
that plainly exists. Use a glibc base, or the published container image, which
is `debian:trixie-slim` for exactly this reason. macOS binaries are unaffected.

Two things are worth having anyway, for what runs *around* it:

- **[uv](https://docs.astral.sh/uv/)** — many MCP servers are launched with
  `uvx`, so a company whose roles use one needs it on the engine's PATH.
- **Docker** — only for the local integration loops (GitLab,
  Mattermost) and for the `container` cell of the
  [code sandbox](../concepts/code-sandbox.md).

### A coding CLI

**Every model a seat runs on is reached through a coding CLI** that the engine
starts as a child process — it has no HTTP client to a model vendor of its own
(see [LLM Backends](../concepts/llm-backends.md#why-the-cli-is-the-only-backend)
for why). So the CLI each `providers.llm` entry names has to be installed on
the engine host, executable by the engine's user and on its `PATH` (or named by
path with `overrides.binary`):

| Models | `harness` | Binary | Install |
|---|---|---|---|
| Anthropic's | `claude-code` (the default) | `claude` | Claude Code — `@anthropic-ai/claude-code` on npm, or the vendor's own installer |
| OpenAI's | `codex` | `codex` | Codex — `@openai/codex` on npm, or the vendor's own installer |
| Open models on any provider, a local server included | `opencode` | `opencode` | **OpenCode 2** — `@opencode/cli` on npm. Not 1.x's `opencode-ai`, which cannot run the engine's profile |

[Supported CLIs](../concepts/llm-backends.md#supported-clis) lists the rest.
Only `claude-code` and `opencode` can run a seat's **executor** — its CLI's own
run in a [code sandbox](../concepts/code-sandbox.md) box — so every agent seat
needs one of them in its `llm` chain; the others serve the reviewer, workers
and side passes. The executor's CLI runs in the box rather than as the engine's
child: on the engine host for a `direct` cell, and inside the image of a
`container` cell or the template of an `e2b` one, which must carry it too.

In a fleet it is every node whose `node.roles` include `seats` (which run the
turns) or `workers` (which run the learning passes on each seat's auxiliary
model) — and the machine you run `crewlet llm login` and `crewlet llm doctor`
on, which drive the same binary.

Install the release line the engine's built-in profile was written for:
`crewlet llm doctor` prints it as `written for` beside the version it found,
and a profile that no longer matches its CLI fails there rather than at a
seat's first turn. Each CLI keeps its login and its per-seat homes under the
entry's `state_dir`, in a subdirectory named for the CLI — by default
`~/.crewlet/llm-harness/<key>/<harness>`, or under `CREWLET_LLM_HARNESS_HOME` — so that
directory belongs on storage that outlives the process.

**The container image ships no coding CLI.** It is the engine and the userland
it needs (see below), and a CLI is a Node or Rust runtime of a few hundred
megabytes that only the companies naming it want — the same reason it ships
no `node` or `uv` for MCP servers. Build a derived image with the ones your
company names:

```dockerfile
FROM ghcr.io/crewlet/crewlet:<tag>
USER root
RUN apt-get update \
    && apt-get install -y --no-install-recommends nodejs npm \
    && rm -rf /var/lib/apt/lists/* \
    && npm install -g @anthropic-ai/claude-code@2.1.263
USER crewlet
```

— `2.1.263`, a release pinned exactly: the hooks a seat's executor takes a
person's notes through need Claude Code 2.1.163 or later (an older release runs
without them) and were measured on 2.1.263 and 2.1.310, and the `claude-code` profile's flags, output paths and
limit sentences on 2.1.303 and 2.1.310; take a newer one when
`crewlet llm doctor` passes against it. Give the container a volume for the state directory
(`CREWLET_LLM_HARNESS_HOME=/var/lib/crewlet/llm-harness`, say), so a login survives a
restart. A login made on another machine reaches the container as a
credential bundle or a headless token in the secret store — see
[Authentication](../concepts/llm-backends.md#authentication).

## Install

```bash
go install github.com/crewlet/crewlet/cmd/crewlet@latest
```

Or take a signed release binary — every tag publishes archives for linux and
macOS on amd64 and arm64, plus a `checksums.txt` with a keyless
[Sigstore](https://www.sigstore.dev/) signature beside it
(`checksums.txt.sigstore.json`, which [cosign](https://docs.sigstore.dev/cosign/) 3 verifies):

> **There is no Windows build**, and no musl build. The store driver ships its
> database engine as a native library that upstream embeds for windows/amd64
> and *not* for windows/arm64, so half of the Windows matrix could never open a
> store; rather than ship one architecture and break the other, the target
> went. On linux use a glibc distribution (see the prerequisites above), the
> container image, or — on Windows — WSL.


```bash
# from https://github.com/crewlet/crewlet/releases
tar xzf crewlet_<version>_<os>_<arch>.tar.gz

cosign verify-blob checksums.txt \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity 'https://github.com/crewlet/crewlet/.github/workflows/release.yml@refs/tags/v<version>' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum -c checksums.txt --ignore-missing
```

Each archive holds the `crewlet` binary, `LICENSE`, `README.md` and the
third-party notices it redistributes: `THIRD_PARTY_NOTICES.txt` for the Go
toolchain and every Go module the binary links, and
`dashboard/THIRD_PARTY_NOTICES.txt` for the embedded dashboard: its npm
packages, the Geist and Geist Mono faces (OFL-1.1) and the Lucide glyphs (ISC,
with Feather's MIT text for the glyphs derived from it). The container image carries the same files under
`/usr/share/doc/crewlet/`, and a running engine serves the dashboard's at
`/static/dashboard/THIRD_PARTY_NOTICES.txt`.

Or run the container image, `ghcr.io/crewlet/crewlet` — a Debian userland
rather than a distroless one, because the engine spawns process trees (the
coding CLI every model runs through, stdio MCP servers, and the local sandbox's
coding agent) — and one that ships no coding CLI, so a company runs it as the
base of an image of its own ([above](#a-coding-cli)). Each release pushes it as
`ghcr.io/crewlet/crewlet:<tag>` (`v0.2.0`, the tag itself) for linux/amd64 and
linux/arm64, and a stable release also moves `latest`. The image is signed by
digest with the same keyless identity as the checksums, which names the release
workflow at that exact tag (cosign 3, or cosign 2 with `--new-bundle-format`):

```bash
cosign verify ghcr.io/crewlet/crewlet:<tag> \
  --certificate-identity 'https://github.com/crewlet/crewlet/.github/workflows/release.yml@refs/tags/<tag>' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

`main` is published too, **unreleased**, as one tag,
`ghcr.io/crewlet/crewlet:main`, for linux/amd64 and linux/arm64. It is rebuilt
on every push to `main` and moves to that build, and nothing else is published
from `main` — no tag per commit, and never `latest`. When pushes land close together only the newest
is built, and a commit that reaches `main` without starting a build — the
dependency bumps Dependabot merges with GitHub's workflow token start none —
is never built: `main` stays on the last pushed commit's image until the next
push. Its binary reports `main-` and the first twelve hex digits of the commit
it was built from, then the whole commit:

```console
$ docker run --rm ghcr.io/crewlet/crewlet:main version
crewlet main-4e45c6b992a2 (4e45c6b992a2133639abebeec271236969b4ead4)
```

It is not a release and is promised nothing: compatibility is promised only
between tags. The tag moves on every push to `main` and the image it moved from
is deleted within the hour, so a deployment that must keep running one build
runs a release rather than `main`. It is signed by digest like a release's image, by a different
workflow, so it verifies under a different identity (cosign 3, or cosign 2
with `--new-bundle-format`):

```bash
cosign verify ghcr.io/crewlet/crewlet:main \
  --certificate-identity 'https://github.com/crewlet/crewlet/.github/workflows/main-image.yml@refs/heads/main' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

Add `--certificate-github-workflow-sha` with a commit's **full** hash to check
that the image was built from that commit rather than from any commit on
`main`.

A binary from `go install` reports its module version, one from a release
reports its tag and one from the image of `main` reports `main-<commit>`, and
none ever claims to be something it is not.

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
  that outgrows one node either clusters those embedded servers together or
  points the same config slot at an external NATS server — the client code is
  identical, so it is a connection choice rather than a second backend. See
  [Fleet](../guides/fleet.md).
- the **store** is two local files this process owns *exclusively* — the
  node's own (`store.path`), and the replicated estate beside it
  (`store.replicated_path`). Not a shared database, and no DSN: two engines
  pointed at one file corrupt it. Coordination between nodes goes through a
  separate KV slot, never a file.

### The compose file is for the integrations

`docker-compose.yml` in a repo checkout starts **nothing by default** —
every service in it is behind a profile, because none of them is the engine:

```bash
docker compose --profile gitlab up -d             # local GitLab (code host)
docker compose --profile mattermost up -d --wait  # self-hosted Mattermost (chat)
```

Each of the two self-hostable integrations pairs with a bootstrap script under
`scripts/` that seeds the instance and provisions the agent seats. See
[GitLab § Local testing](../integrations/gitlab.md#local-testing) and
[Mattermost § Local testing](../integrations/mattermost.md#local-testing).
Jira and Confluence have no profile here — Atlassian is not something a
compose file can stand up. Nor is there a profile for the stream or the store:
the engine brings both up itself, which is the whole point of embedding them.

Running either of these on a **remote host** rather than your own machine? Each
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

and, for the coding CLI your company's entries name, its own version probe
(`claude --version`, `codex --version`, `opencode --version`) — `crewlet llm
doctor` runs the same probe against the configured entry once a company exists.

Next: the [Quickstart](quickstart.md) brings up a four-agent company, and
[Choosing your stack](choosing-your-stack.md) walks through the external
services (LLM, tracker, code host, chat, sandbox) and their alternatives.
