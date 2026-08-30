# adr-901 — Releasing a binary: goreleaser, the tag as the version, one image

Status: **Accepted** · Implementation: `.goreleaser.yaml`, `Dockerfile`,
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
`dist/metadata.json`.

That comparison is equality against something derived, and deliberately so.
Its first spelling was a sentinel — the workflow searched the binary's output
for `dev` — and it went quiet without failing when the language moved
underneath it: since Go 1.24 an unstamped build inside a repository reports a
pseudo-version derived from the tags rather than `dev`, so a stamp that
applied to nothing produced a green release. A check whose failure mode is
silence has to compare against something the tree computes, not against a
constant somebody remembered to write down.

A second check sat beside it and no longer does. `internal/version` held a
test that split the `-X` target into its import path and its variable and
checked both against the tree — the module path from `go.mod` plus this
directory, and a package-level string var parsed out of the AST. It went with
the rest of this package's assertions over the release config; see *What is
deliberately not asserted* below. The runtime comparison above is what
remains, and on everything it can see it is the stronger of the two: the
rename, the move and the changed module path all end with the binary
reporting a version goreleaser did not build. What it cannot see is a stamp
that lands on a build whose artifacts are not the ones packaged — it verifies
the `crewlet` build's staged binary by path — so a second `builds:` entry is
the case to re-check by hand.

## Four targets

`linux` and `darwin` × `amd64` and `arm64`. All four cross-compile from one
machine with `CGO_ENABLED=0`, because everything underneath is pure Go — the
store driver (`turso.tech/database/tursogo`) and the embedded NATS server. That
is what keeps this a plain GOOS/GOARCH loop instead of a cross-toolchain
estate, and it is worth protecting: a dependency that needs cgo turns this
section into a zig toolchain and a build container. It is also, as the rest of
this section works through, the property a musl target would cost.

**"Pure Go" is not "self-contained", and that is what bounds the list.** The
driver's database engine is a native shared object embedded in its module and
extracted at run time. Upstream embeds it for linux/amd64, linux/arm64 (each
glibc and musl), darwin/amd64, darwin/arm64 and windows/amd64 — and for nothing
else. The compiler would happily produce a binary for any GOOS; that binary
would fail at its first query. `internal/store/platform.go` makes it fail at
build time instead, with a sentence.

**A static binary is not the answer, and the surprising part is why
`CGO_ENABLED=0` does not already give us one.** That is the normal Go rule and
it is the first thing anyone will reach for, so here is the whole chain,
measured on one machine:

| build | result |
|---|---|
| `CGO_ENABLED=0 go build` — a hello-world | **statically linked** |
| `CGO_ENABLED=0 go build` — crewlet | **dynamically linked**, interpreter `/lib64/ld-linux-x86-64.so.2` |
| `CGO_ENABLED=0` + `-extldflags "-static"` — crewlet | **dynamically linked** (unchanged) |
| `CGO_ENABLED=1 -linkmode external -extldflags "-static"` — crewlet | statically linked, and **SIGSEGV at the first query** |

Row 1 is the control: the toolchain makes static binaries perfectly well, so
row 2 is the dependency and not the environment. It is not a NEW dependency
either — `main` before the single-driver change was already dynamic, with the
same three `NEEDED` entries, because Turso was already the default driver.
adr-003 has that measurement and the counterfactual beside it. The cause is in purego, and
its build tag is the joke — `dlfcn_nocgo_linux.go` is `//go:build !cgo`, the
file that applies *precisely when cgo is off*, and it says so: "if there is no
Cgo we must link to each of the functions from dlfcn.h". It declares `dlopen`,
`dlsym`, `dlerror` and `dlclose` with `//go:cgo_import_dynamic`, which the Go
linker honours whether or not cgo is enabled, and the binary comes out with an
ELF interpreter and `DT_NEEDED` entries. `readelf --dyn-syms` on the shipped
artifact lists exactly those four as undefined.

Row 3 is why `CGO_ENABLED=1` appears at all in row 4, which otherwise reads
backwards. `-extldflags` is passed to the EXTERNAL linker, and a cgo-free build
links internally — the flag reaches nothing. External linking is the only way
to force a static ELF, and external linking requires cgo. So row 4 is not "the
normal way to build static", it is the only lever left after rows 2 and 3.

And it does not work:

```
/usr/bin/ld: warning: Using 'dlopen' in statically linked applications
  requires at runtime the shared libraries from the glibc version used
  for linking
file(1): "statically linked"
./crewlet migrate: SIGSEGV, signal arrived during cgo execution,
  inside purego.RegisterFunc -> the first call into the Turso library
```

On the machine that built it, against the glibc it linked against, with a
clean library cache. The identical run on the ordinary dynamic build applies
13 migrations. This is not a flag that wants tuning: the driver reaches its
engine with `dlopen`, and a statically linked program has no dynamic loader to
do that with. Static linking and a runtime-loaded shared object are mutually
exclusive, so a "static build" here is an artifact that looks more portable and
crashes at its first query. `ci.yml`'s `cross` job asserts the binary is still
dynamic for exactly this reason — the assertion is guarding against an
improvement that isn't one.

The version that WOULD work is upstream's to make: `turso-go-platform-libs`
ships `libturso_sync_sdk_kit.a` beside the `.so` for both musl targets, and a
driver that linked the archive through cgo would need no loader at all. But
`tursogo` has no such path — every entry point goes through
`InitLibrary` -> `LoadTursoLibrary` -> `dlopen`, and there is no build tag,
no `#cgo LDFLAGS` and no `import "C"` anywhere in it. Using those archives
means a fork or an upstream change, not a flag here.

**There is no musl archive, and `-tags musl` is not what would produce one.**
This was nearly shipped and the measurement stopped it. The tag selects which
shared object the driver embeds; it does not change the binary's own linkage,
and the binary is not static: purego declares its dlopen imports with
`//go:cgo_import_dynamic`, so `CGO_ENABLED=0 go build` still emits
`interpreter /lib64/ld-linux-x86-64.so.2` with `NEEDED libc.so.6` — identical
with and without the tag. A `_musl` archive built that way fails at `execve`
on Alpine, which is worse than no archive: it is a signed artifact promising a
platform it cannot run on, the same defect as the windows/arm64 binary below.

A real musl artifact needs a musl toolchain to build on (`zig cc -target
x86_64-linux-musl`, or Alpine's own gcc) and a musl host to test on. That is a
genuine option and it is not this change: it trades the plain GOOS/GOARCH loop
for a cross-toolchain on one target, which is the property this section exists
to protect, so it deserves its own decision rather than being smuggled in.
Until then the honest position is the one the docs take: the linux binaries
require glibc.

**Windows was dropped, and it is worth being exact about why.** Not "Turso has
no Windows build" — it has one, for amd64. The release published windows/amd64
AND windows/arm64, and the second had no embedded library at all: it started
fine and failed at its first query unless the operator knew to set
`CREWLET_STORE_DRIVER=sqlite`, the fallback driver that adr-003 has since
removed. Shipping one architecture of an operating system and silently breaking
the other is worse than shipping neither. A Windows build was in any case the
one that refused `providers.sandbox: {type: local}`: the local sandbox backend
is POSIX-only and says so at construction (`internal/sandbox/process_other.go`),
because every containment property it offers — process groups and `killpg` to
reach a coding agent's whole tree, `SIGSTOP`/`SIGCONT` for the clarification
pause, `/proc` start times for the pid-reuse guard — is a POSIX primitive with
no equivalent.

## One image, with a userland

Multi-arch (`linux/amd64`, `linux/arm64`) to GHCR, because the release
workflow already holds a token scoped to this repository and a second
registry would mean a second account and a second credential.

Base: `debian:trixie-slim`, **not** distroless or scratch. The binary is not
static on linux at all (see above), so `scratch` does not merely lose the
userland below — the process would not start. Even setting that aside: a
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

## What is deliberately not asserted

`internal/version` used to carry eleven tests that read the release
configuration itself — `.goreleaser.yaml`, the `Dockerfile`,
`.github/dependabot.yml`, `.github/release.yml` and the workflow files — and
asserted the properties above against their contents. They were dropped, and
this section records why, because the shape is one a future reader would
plausibly re-add.

The invariants are unchanged and still real. What did not hold up was the
mechanism. Every one of those tests except the `-X` stamp check was a
`strings.Contains` over a whole YAML file, with no notion of which key the
match sat under, which job it belonged to, or whether it was live config or a
comment. Measured by mutation — 105 breaking edits applied to the real files —
they killed 50 and let 55 through, and the survivors were not exotic: `&&`
swapped for `||` in the auto-merge guard, the whole `if:` commented out,
`--admin` appended after `--auto --squash`, the catch-all `*` moved from
`labels:` into `exclude:`, `CGO_ENABLED=1` with the old value left in a
comment beside it, a second `v*` workflow written as a block sequence instead
of an inline one. A check that stays green through the edit it exists to catch
is the failure mode this file already names two sections up, and it is worse
than no check because it reads as coverage.

They also cost 32 false positives on harmless edits, several of them
anti-correlated with the invariant: the stamp check went red on goreleaser's
own documented `- -s -w -X …` single-entry idiom, and the sibling check in
`internal/api` went red on an upgrade to `actions/setup-node`.

So the properties are now maintained by reading, and `RELEASING.md` lists them
under *What nothing guards* for exactly that purpose. **If they are asserted
again, they must be asserted structurally** — decode the file with
`gopkg.in/yaml.v3`, which is already a direct dependency, and assert the node
under the key that matters. Restoring the substring checks would restore the
false confidence with them.

## One workflow owns the tag, and the pipeline rehearses without one

Exactly one workflow carries `push: tags: ["v*"]`. Two workflows triggered by
the same tag both try to create one GitHub Release for it, and the loser fails
the run after its artifacts are already built — a race with no symptom until
the day it publishes. `internal/version` used to assert there was only one;
nothing does now, so adding a workflow means reading the trigger blocks of the
ones already there.

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
