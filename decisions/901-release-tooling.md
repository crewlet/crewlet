# d-901 — Releasing a binary: goreleaser, the tag as the version, one image

Status: **decided** · Implementation: `.goreleaser.yaml`, `Dockerfile`,
`.github/workflows/release.yml`, `internal/version/` · Docs: `RELEASING.md`

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
touching nothing remote. The PyPI pipeline it replaced could only rehearse by
uploading to TestPyPI — which is why it carried a `workflow_dispatch` branch
and a whole second publish job whose only purpose was to find out whether the
real one would have worked.

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
than being caught. `scripts/release_metadata.py` and its packaging tests went
with the Python tree.

A binary built WITHOUT that stamp — `go install`, a local `go build` —
reports the module's own build info instead, so it names itself honestly
rather than claiming to be a release it is not. That fallback lives in
`internal/version`.

The stamp is the one part of this pipeline with no other symptom: rename the
variable or move the package and it silently stops applying, with every
release reporting the build-info fallback. So both release jobs run the built
binary and compare what it reports against the version goreleaser recorded in
`dist/metadata.json`, and a test in `internal/version` splits the `-X` target
into its import path and its variable and checks both against the tree.

Both of those are equality against something derived, and deliberately so.
The first spelling of each was a sentinel and a literal — the workflow
searched the binary's output for `dev`, and the test searched the config for
the import path written out longhand — and both went quiet without failing.
The literal is a second copy of the thing this section exists to keep from
having two copies of, so it stayed green through exactly the renames it was
watching for. The sentinel stopped matching when the language moved
underneath it: since Go 1.24 an unstamped build inside a repository reports a
pseudo-version derived from the tags rather than `dev`, so a stamp that
applied to nothing produced a green release. A check whose failure mode is
silence has to compare against something the tree computes, not against a
constant somebody remembered to write down.

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

## One workflow owns the tag, and the pipeline rehearses without one

Exactly one workflow carries `push: tags: ["v*"]`, and `internal/version`
asserts that there is only one. Two workflows triggered by the same tag both
try to create one GitHub Release for it, and the loser fails the run after its
artifacts are already built — a race with no symptom until the day it
publishes.

The whole pipeline — every target, the archives, the checksums, the image —
also runs in snapshot mode on demand and on any pull request that touches the
release surface. That is what stops it rotting unnoticed and then running for
the first time on the tag that publishes.

## Dependency surfaces

Two were added and one was already missing.

`Dockerfile` is a new surface, and CLAUDE.md's rule is that a new surface
ships with its `updates:` entry — nothing reports the omission, because a
manifest with no entry looks exactly like a manifest with nothing to update.

`go.mod` was that omission. The Go module had been in this repository without
a Dependabot entry for as long as it had existed, so every vendor SDK, both
broker clients and both store drivers were watched by nothing. Fixed in the
same change.
