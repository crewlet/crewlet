# Installation

## Prerequisites

**A glibc userland, on linux.** Crewlet is one binary with no runtime to
install: it embeds its event stream (a NATS JetStream server) and its database
is a local file it creates. There is no broker to operate and nothing to point
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
  Mattermost) and for the container mode of the
  [code sandbox](../concepts/code-sandbox.md).

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
rather than a distroless one, because the engine spawns process trees (stdio
MCP servers, and the local sandbox's coding agent). Each release pushes it as
`ghcr.io/crewlet/crewlet:<tag>` (`v0.2.0`, the tag itself) for linux/amd64 and
linux/arm64, and a stable release also moves `latest`. The image is signed by
digest with the same keyless identity as the checksums, which names the release
workflow at that exact tag (cosign 3, or cosign 2 with `--new-bundle-format`):

```bash
cosign verify ghcr.io/crewlet/crewlet:<tag> \
  --certificate-identity 'https://github.com/crewlet/crewlet/.github/workflows/release.yml@refs/tags/<tag>' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

Commits on `main` are published too, **unreleased** — the commit at the head
of every push to `main`, and `main`'s tip at least hourly — as
`ghcr.io/crewlet/crewlet:<version>`, where the version is the commit's Go module
pseudo-version (`v0.1.1-0.20261010171839-4e45c6b992a2`, exactly what
`go get github.com/crewlet/crewlet@<commit>` resolves and what `crewlet version`
in that image prints). A commit that only arrived inside a merge (a merged
branch's own commits) or together with later commits has a module version but no
image: pin the merge commit, or the pull request's squash commit.

`ghcr.io/crewlet/crewlet:main` follows `main`'s newest commit: it moves to that
commit's image once the image is published — usually within minutes of a merge,
within the hour for a merge that started no run of its own — and until then
stays on the previous one. When `main`'s newest commit is a tagged release,
`main` names that release's image, which verifies as a release (above). The
per-commit tag is written once and never rebuilt, so pin that one (or its
digest) rather than `main`. Neither is a release and neither ever becomes
`latest`; compatibility is promised only between tags. They are signed by a
different workflow, so they verify under a different identity, and
`--certificate-github-workflow-sha` with the commit's **full** hash proves the
image was built from the commit you pinned rather than from any commit on
`main` (cosign 3, or cosign 2 with `--new-bundle-format`):

```bash
cosign verify ghcr.io/crewlet/crewlet:<version> \
  --certificate-identity 'https://github.com/crewlet/crewlet/.github/workflows/main-image.yml@refs/heads/main' \
  --certificate-github-workflow-sha <full commit hash> \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

A binary from `go install` reports its module version, one from a release
reports its tag and one from an image of `main` reports that commit's module
version — the same string Go uses for that version, however it was
installed — and none ever claims to be something it is not.

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
- the **store** is one local file this process owns *exclusively*. Not a
  shared database, and no DSN: two engines pointed at one file corrupt it.
  Coordination between nodes goes through a separate KV slot, never the file.

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

Next: the [Quickstart](quickstart.md) brings up a four-agent company, and
[Choosing your stack](choosing-your-stack.md) walks through the external
services (LLM, tracker, code host, chat, sandbox) and their alternatives.
