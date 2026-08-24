# d-901 — Releasing a binary: goreleaser, the tag as the version, one image

Status: **decided** · Phase: 9 · Spec: `.github/workflows/release.yml`,
`RELEASING.md`, `scripts/release_metadata.py` ·
Implementation: `.goreleaser.yaml`, `Dockerfile`,
`.github/workflows/go-release.yml`, `internal/version/`

## The question

The product stops being a Python package on an index and becomes a binary.
Everything the PyPI pipeline provided has to be provided again by something
else: a build, a place to put it, a version, notes, and the property that no
long-lived credential exists anywhere in this repository.

## What is kept, because it was never about Python

Three properties survive the change of language, and two of them are the
reason this decision is short.

**The tag is the whole release.** `git tag v1.2.0 && git push origin v1.2.0`
still does everything. Nothing else is a release step.

**The pull request title is the release note.** There is no `CHANGELOG.md`;
GitHub generates each Release body from the titles of the pull requests
merged since the previous tag, grouped by `.github/release.yml`. goreleaser
is configured with `changelog.use: github-native`, which asks GitHub for
exactly that body rather than writing a competing one out of commit
subjects. A goreleaser-authored changelog would have silently replaced the
only record a release has.

**No credential in the repository.** Trusted Publishing gave this by minting
a short-lived token from the workflow's OIDC identity. The replacement keeps
it two ways: the GitHub Release and the GHCR push both use the workflow's own
token, and artifact signing is keyless Sigstore — the certificate is minted
from the same OIDC identity and logged to Rekor, so there is no private key
to leak because there is no private key.

## goreleaser, not a build matrix

The alternative is a `strategy.matrix` over six GOOS/GOARCH pairs plus steps
for archives, checksums, a container manifest and the Release. That is the
same work, written worse, in a file that can only be executed by pushing a
tag.

What decides it is the rehearsal. `goreleaser release --snapshot --clean`
builds every target, every archive and the container image on a laptop,
touching nothing remote. The pipeline it replaces could only rehearse by
uploading to TestPyPI — which is why `release.yml` carries a
`workflow_dispatch` branch and a whole second publish job whose only purpose
is to find out whether the real one would have worked.

Against it: one more tool in the release path. It is pinned by major version
in the workflow, its config is validated by `goreleaser check` on every pull
request that touches the release surface, and the config is a hundred lines
that a person can read.

## The tag is the version, and there is no second copy

Python's rule was the opposite: `__version__` in `src/crewlet/__init__.py`
was the truth, the tag had to agree, and `scripts/release_metadata.py verify`
refused to build when they did not. That check existed because there were two
places, and it caught the case where one was bumped and the other mistyped.

Here there is one place. The tag is stamped into
`internal/version.value` at link time
(`-X github.com/crewlet/crewlet/internal/version.value={{ .Version }}`), so
nothing in the tree records a version that a tag could disagree with, and the
class of failure the verify step existed for stops being reachable rather
than being caught. `scripts/release_metadata.py` and its packaging tests go
with the Python tree.

A binary built WITHOUT that stamp — `go install`, a local `go build` —
reports the module's own build info instead, so it names itself honestly
rather than claiming to be a release it is not. That fallback is already in
`internal/version`.

The stamp is the one part of this pipeline with no other symptom: rename the
variable or move the package and it silently stops applying, with every
release reporting `dev`. So the workflow runs the built binary and fails on
that string, and a test asserts the config names the current import path.

## Six targets

`linux`, `darwin` and `windows` × `amd64` and `arm64`. All six cross-compile
from one machine with `CGO_ENABLED=0`, because everything underneath is pure
Go — both certified store drivers (`turso.tech/database/tursogo`,
`modernc.org/sqlite`) and the embedded NATS server. That is what keeps this a
plain GOOS/GOARCH loop instead of a cross-toolchain estate, and it is worth
protecting: a dependency that needs cgo turns this section into a zig
toolchain and a build container.

**Windows ships with a caveat, deliberately.** The local sandbox backend is
POSIX-only and says so at construction
(`internal/sandbox/process_other.go`): every containment property it
offers — process groups and `killpg` to reach a coding agent's whole tree,
`SIGSTOP`/`SIGCONT` for the clarification pause, `/proc` start times for the
pid-reuse guard — is a POSIX primitive with no equivalent. So a Windows
operator gets an engine that runs a company and refuses `type: local` code
work, naming the reason, rather than no binary at all. The alternative
readings — ship a partial port, or ship nothing — are worse in both
directions.

## One image, with a userland

Multi-arch (`linux/amd64`, `linux/arm64`) to GHCR, because the release
workflow already holds a token scoped to this repository and a second
registry would mean a second account and a second credential.

Base: `debian:trixie-slim`, **not** distroless or scratch. A static pure-Go
binary would run on `scratch` and that is the right image for most Go
services. Not for this one, because the engine SPAWNS things:

- the local sandbox backend runs a coding agent as a child process tree and
  applies setup steps that are shell commands out of the company config;
- those recipes are config, not engine code, and the one this repo ships
  (`examples/nimbus.company.yaml`) drives `git`;
- stdio MCP servers are child processes launched by whatever the config
  names.

On `scratch` all three fail at the moment they are used, from an image that
started perfectly happily. The ~80 MB the base costs buys nothing an operator
wanted. `tini` is PID 1 for the same reason `local.go` passes `--init` to the
container sandbox backend: an engine that spawns process trees under a PID 1
with no reaper collects zombies until the pid table fills.

stdio MCP servers whose runtime is not in that list (node, python, uv) still
need a derived image. That is deliberate — an image guessing at three
runtimes would be wrong for everyone and large for everyone.

## What is deliberately not built

**No Homebrew tap, no apt repository, no Scoop bucket.** Each is a second
repository with its own release step and its own way to be stale. Archives, a
container image and `go install` cover every way anyone has asked to install
this. A tap is cheap to add later and expensive to maintain unused.

**No SBOM attestation yet.** goreleaser can emit one, and the value of an
SBOM is in being consumed — by a scanner an operator runs, against a policy
they have. Emitting one because the tool offers it produces a file nobody
reads and a claim nobody checks. It lands when there is a consumer.

**No per-archive signature.** One signature over `checksums.txt`, which
covers every artifact, is the same proof as eight signatures with one fetch
instead of eight.

## Sequencing

`.goreleaser.yaml` and the `Dockerfile` live at the repository root NOW, with
a single `builds[].dir: go` pointing into the module. That line is the only
thing the root replacement deletes from them.

`go-release.yml` ships WITHOUT its `push: tags: ["v*"]` trigger, and that is
not a deferral. `release.yml` still owns that tag and still creates the
GitHub Release for it; two workflows creating one Release for one tag is a
race whose loser fails the run. The trigger arrives in the same commit that
deletes `release.yml`. Until then the whole pipeline — every target, the
archives, the checksums, the image — runs in snapshot mode on demand and on
any pull request that touches the release surface, so it cannot rot into the
move and then run for the first time on the tag that publishes.

## Dependency surfaces

Two were added and one was already missing.

`Dockerfile` is a new surface, and CLAUDE.md's rule is that a new surface
ships with its `updates:` entry — nothing reports the omission, because a
manifest with no entry looks exactly like a manifest with nothing to update.

`go/go.mod` was that omission. The Go module has been in this repository for
the length of the rewrite with no Dependabot entry, so every vendor SDK, both
broker clients and both store drivers were watched by nothing. Fixed in the
same change.
