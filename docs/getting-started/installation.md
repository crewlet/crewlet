# Installation

## Prerequisites

**A glibc userland, on linux.** Crewlet is one binary with no runtime to
install: it embeds its event stream (a NATS JetStream server) and its database
is two local files it creates. There is no broker to operate and no DSN.

The linux binaries are not static: the store's database engine is a native
library loaded with `dlopen`, so they link against glibc (`libc.so.6`). They
will not run on `scratch`, on a distroless image without a C library, or on
Alpine and other musl systems (the failure is `no such file or directory` about
a file that exists). Use a glibc base, or the published container image
(`debian:trixie-slim`). macOS binaries are unaffected.

Also worth having, for what runs around it:

- **[uv](https://docs.astral.sh/uv/)**: many MCP servers launch with `uvx`, so
  a company whose roles use one needs it on the engine's PATH.
- **Docker**: only for the local integration loops (GitLab, Mattermost) and
  the `container` cell of the [code sandbox](../concepts/code-sandbox.md).

### A harness

**Every model a seat runs on is reached through a harness** the engine starts
as a child process; it has no HTTP client to a model vendor (see
[LLM Backends](../concepts/llm-backends.md#why-the-harness-is-the-only-backend)).
So the harness each `providers.llm` entry names must be installed on the engine
host, executable by the engine's user and on its `PATH` (or named by path with
`overrides.binary`):

| Models | `harness` | Binary | Install |
|---|---|---|---|
| Anthropic's | `claude-code` (the default) | `claude` | Claude Code: `@anthropic-ai/claude-code` on npm, or the vendor's installer |
| OpenAI's | `codex` | `codex` | Codex: `@openai/codex` on npm, or the vendor's installer |
| Open models on any provider, a local server included | `opencode` | `opencode` | **OpenCode 2.3.2**: `@opencode/cli` on npm, the one release supported. Not 1.x's `opencode-ai`, which cannot run the engine's profile |

[Supported harnesses](../concepts/llm-backends.md#supported-harnesses) lists the rest.
Only `claude-code`, `codex`, `gemini-cli`, `qwen-code`, `opencode`, `cursor-agent`, `copilot`, `grok`, `muse-code`, `kimi-code`, `hermes` and `pi` can run a seat's **executor** (its harness's
own run in a [code sandbox](../concepts/code-sandbox.md) box), so every agent
seat needs one of them in its `llm` chain; the others serve the reviewer,
workers and side passes. The executor's harness runs in the box, not as the
engine's child: on the engine host for a `direct` cell, inside the image of a
`container` cell, which must carry it too.

In a fleet, install it on every node whose `node.roles` include `seats` (run
the turns) or `workers` (run the learning passes on each seat's auxiliary
model), and on the machine where you run `crewlet llm login` and
`crewlet llm doctor`, which drive the same binary.

Install the release line the engine's built-in profile was written for:
`crewlet llm doctor` prints it as `written for` beside the version it found,
and a profile that no longer matches fails there, not at a seat's first turn.
Each harness keeps its login and per-seat homes under the entry's `state_dir`,
in a subdirectory named for the harness (default
`~/.crewlet/llm-harness/<key>/<harness>`, or under `CREWLET_LLM_HARNESS_HOME`),
so that directory belongs on storage that outlives the process.

**The container image ships no harness**: a harness is a Node or Rust runtime
of a few hundred megabytes that only some companies want (likewise no `node` or
`uv` for MCP servers). Build a derived image with the ones your company names:

```dockerfile
FROM ghcr.io/crewlet/crewlet:<tag>
USER root
RUN apt-get update \
    && apt-get install -y --no-install-recommends nodejs npm \
    && rm -rf /var/lib/apt/lists/* \
    && npm install -g @anthropic-ai/claude-code@2.1.263
USER crewlet
```

`2.1.263` is pinned exactly. Claude Code constraints:

- The hooks a seat's executor takes a person's notes through need 2.1.163 or
  later (a run on an older release is refused); they were measured on 2.1.263
  and 2.1.310.
- The `claude-code` profile's flags, output paths and limit sentences were
  measured on 2.1.303 and 2.1.310.
- Take a newer release when `crewlet llm doctor` passes against it.
- A box continues a Claude Code session packed in another box only on 2.1.310
  or later.

OpenCode: 2.3.2 is the one release supported, its newest. A box on an older
release is refused at the run's start, naming its release; one on a newer
release runs, a session carried in from another box starting fresh there while
the box's own copy continues in place. A box continues a session packed in another
box only where both are exactly 2.3.2, and a session is counted for its launch's
floor only as 2.3.2 writes it; a session another release wrote is held to its
newest request's input. Its first request is judged only in a box of the release that
wrote it; every other continuation of it is told as one that may be a summary.

A box packs and restores a session with `tar`, `gzip` and a POSIX userland:
`find`, `ls`, `wc`, `sed`, `grep`, `head`, `tail`, `cksum` and `touch` among its
tools, and a `sleep` that takes a tenth of a second. The published image
(`debian:trixie-slim`) carries all of them for a `direct` cell; a `container`
image must too
([Bring your own image](../concepts/code-sandbox.md#bring-your-own-image)).
Give the container a volume for the state directory
(`CREWLET_LLM_HARNESS_HOME=/var/lib/crewlet/llm-harness`, say) so a login
survives a restart. A login made on another machine reaches the container as a
credential bundle or a headless token in the secret store; see
[Authentication](../concepts/llm-backends.md#authentication).

## Install

```bash
go install github.com/crewlet/crewlet/cmd/crewlet@latest
```

A binary from `go install` carries no note helper, the small program a seat's
executor writes into its box, so every executor launch is refused until it finds
one: put the two `notehelper-linux-*` files from the release archive of the same
version beside it, or in a directory `CREWLET_NOTE_HELPER_DIR` names.

Or take a signed release binary: every tag publishes archives for linux and
macOS on amd64 and arm64, plus a `checksums.txt` with a keyless
[Sigstore](https://www.sigstore.dev/) signature beside it
(`checksums.txt.sigstore.json`, which [cosign](https://docs.sigstore.dev/cosign/) 3 verifies):

> **There is no Windows build**, and no musl build. The store driver's native
> database library is embedded upstream for windows/amd64 but not windows/arm64,
> so half the Windows matrix could never open a store. On linux use a glibc
> distribution (see the prerequisites above), the container image, or, on
> Windows, WSL.


```bash
# from https://github.com/crewlet/crewlet/releases
tar xzf crewlet_<version>_<os>_<arch>.tar.gz

cosign verify-blob checksums.txt \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity 'https://github.com/crewlet/crewlet/.github/workflows/release.yml@refs/tags/v<version>' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum -c checksums.txt --ignore-missing
```

Each archive holds the `crewlet` binary, beside it the note helper's builds for
a linux/amd64 and a linux/arm64 box (`notehelper-linux-amd64`,
`notehelper-linux-arm64`; keep them beside the binary), `LICENSE`, `NOTICE`, `README.md` and
the third-party notices it redistributes: `THIRD_PARTY_NOTICES.txt` for the Go
toolchain and every Go module the binary links, and
`dashboard/THIRD_PARTY_NOTICES.txt` for the embedded dashboard: its npm
packages with the NOTICE files they ship, the Geist and Geist Mono faces (OFL-1.1) and the Lucide glyphs (ISC,
with Feather's MIT text for the glyphs derived from it). The container image carries the same files under
`/usr/share/doc/crewlet/`, and a running engine serves the dashboard's at
`/static/dashboard/THIRD_PARTY_NOTICES.txt`.
Or run the container image, `ghcr.io/crewlet/crewlet`. It is Debian rather than distroless because the engine spawns process trees (the harness every model runs through, stdio MCP servers, the local sandbox's harness). It ships no harness, so a company runs it as the base of an image of its own ([above](#a-harness)). Each release pushes `ghcr.io/crewlet/crewlet:<tag>` (`v0.2.0`, the tag itself) for linux/amd64 and linux/arm64; a stable release also moves `latest`. The image is signed by digest with the same keyless identity as the checksums, which names the release workflow at that tag (cosign 3, or cosign 2 with `--new-bundle-format`):

```bash
cosign verify ghcr.io/crewlet/crewlet:<tag> \
  --certificate-identity 'https://github.com/crewlet/crewlet/.github/workflows/release.yml@refs/tags/<tag>' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

`main` is published too, **unreleased**, as one tag, `ghcr.io/crewlet/crewlet:main`, for linux/amd64 and linux/arm64. It is rebuilt on every push to `main`; nothing else is published from `main` (no tag per commit, never `latest`). When pushes land close together only the newest is built, and a commit that starts no build (the dependency bumps Dependabot merges with GitHub's workflow token) is never built: `main` stays on the last pushed commit's image until the next push. Its binary reports `main-` and the first twelve hex digits of the commit, then the whole commit:

```console
$ docker run --rm ghcr.io/crewlet/crewlet:main version
crewlet main-4e45c6b992a2 (4e45c6b992a2133639abebeec271236969b4ead4)
```

It is promised nothing: compatibility is promised only between tags. The tag moves on every push to `main` and the image it moved from is deleted within the hour, so a deployment that must keep running one build runs a release. It is signed by digest by a different workflow, so it verifies under a different identity (cosign 3, or cosign 2 with `--new-bundle-format`):

```bash
cosign verify ghcr.io/crewlet/crewlet:main \
  --certificate-identity 'https://github.com/crewlet/crewlet/.github/workflows/main-image.yml@refs/heads/main' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

Add `--certificate-github-workflow-sha` with a commit's **full** hash to check the image was built from that commit rather than any commit on `main`.

A binary from `go install` reports its module version, a release's reports its tag, the image of `main` reports `main-<commit>`; none claims to be something it is not.

## Install from source

```bash
git clone https://github.com/crewlet/crewlet.git
cd crewlet
go build ./cmd/crewlet
make notehelper && export CREWLET_NOTE_HELPER_DIR="$PWD/build/notehelper"
```

The second line builds the note helper an executor writes into its box, which
the engine finds through `CREWLET_NOTE_HELPER_DIR`. That is the whole setup; see [CONTRIBUTING.md](https://github.com/crewlet/crewlet/blob/main/CONTRIBUTING.md) for test and lint commands.

## Infrastructure

**The engine needs none.** `crewlet run` in a directory with a config is a working company:

- the **event stream** is a JetStream server inside the process. A deployment that outgrows one node either clusters those embedded servers or points the same config slot at an external NATS server; the client code is identical, so it is a connection choice, not a second backend. See [Fleet](../guides/fleet.md).
- the **store** is two local files this process owns *exclusively*: the node's own (`store.path`) and the replicated estate beside it (`store.replicated_path`). No shared database, no DSN: two engines pointed at one file corrupt it. Coordination between nodes goes through a separate KV slot, never a file.

### The compose file is for the integrations

`docker-compose.yml` in a repo checkout starts **nothing by default**: every service is behind a profile, because none is the engine:

```bash
docker compose --profile gitlab up -d             # local GitLab (code host)
docker compose --profile mattermost up -d --wait  # self-hosted Mattermost (chat)
```

Each of the two self-hostable integrations pairs with a bootstrap script under `scripts/` that seeds the instance and provisions the agent seats. See [GitLab § Local testing](../integrations/gitlab.md#local-testing) and [Mattermost § Local testing](../integrations/mattermost.md#local-testing). Jira and Confluence have no profile (Atlassian can't be stood up by a compose file), nor do the stream or the store: the engine brings both up itself.

On a **remote host**, each integration must be told the address browsers reach it on (`MATTERMOST_PUBLIC_URL`, and GitLab's external URL) before the stack comes up. For Mattermost that setting also gates live updates, so a wrong value looks like a working install where messages only appear on refresh; the bootstrap script sets it, and [The Site URL](../integrations/mattermost.md#the-site-url) explains why.

`--wait` is safe for the Mattermost profile: every service there has a healthcheck.

## Verify Installation

```bash
crewlet --version
```

For the harness your company's entries name, run its own version probe (`claude --version`, `codex --version`, `opencode --version`); `crewlet llm doctor` runs the same probe against the configured entry once a company exists.

Next: the [Quickstart](quickstart.md) brings up a four-agent company, and [Choosing your stack](choosing-your-stack.md) covers the external services (LLM, tracker, code host, chat, sandbox) and their alternatives.
