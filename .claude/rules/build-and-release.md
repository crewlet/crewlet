---
paths:
  - "{.goreleaser.yaml,Dockerfile,Makefile,RELEASING.md,.github/release.yml}"
  - ".github/workflows/{release,main-image,docs-publish}.yml"
  - "internal/version/**"
  - "internal/store/{turso,platform}.go"
  - ".claude/worktrees/*/{.goreleaser.yaml,Dockerfile,Makefile,RELEASING.md,.github/release.yml}"
  - ".claude/worktrees/*/.github/workflows/{release,main-image,docs-publish}.yml"
  - ".claude/worktrees/*/internal/version/**"
  - ".claude/worktrees/*/internal/store/{turso,platform}.go"
---

# Build and Release
## The build
- **`CGO_ENABLED=0`** (`CLAUDE.md`, "Tech Stack") makes cross-compiling a plain `GOOS`/`GOARCH` loop over the four platforms the store driver has a database engine for. It does NOT make the linux artifact static; see Turso
- **Turso** is the only driver, so use what it has. It needs no cgo, but its engine is a ~20 MB native library embedded in the driver and extracted at runtime into a shared per-user cache; `internal/store/turso.go` prepares it under a lock, because upstream writes it non-atomically and panics on what a concurrent reader sees. The library:
  - bounds the release matrix to linux and darwin on amd64 and arm64, no Windows; `internal/store/platform.go` makes any other build a compile error
  - links the linux binary dynamically against glibc, so it does not run on musl or `scratch`. Do NOT "fix" that with `-extldflags -static`: a static program cannot `dlopen`, so that build segfaults on its first query (ci.yml's `cross` job asserts the artifact stays dynamic). `-tags musl` picks the embedded `.so`, not the binary's linkage, so it gives no Alpine build either

- **Dictation's recognizer is a native library the binary does NOT embed**: sherpa-onnx's official shared-library archive per platform, pinned in `internal/speech/native.go` (URL, size, SHA-256 and its members'), fetched into `internal/nativelib`'s cache like Turso's is extracted there. The Dockerfile's `speech` stage runs the target's own `crewlet speech prepare` and bakes the library and the model beside the binary. A bump changes the four pins, `SherpaVersion` and the notices under `internal/speech/third_party/` together

- **The note helper a box runs is built beside the engine, never committed**: goreleaser's before hook and `make image-context` run `scripts/notehelper` (`internal/notehelper/helperbuild`'s recipe) for linux/amd64 and linux/arm64; every archive carries both beside the binary and the image under `/usr/local/share/crewlet/`. The executor looks only in `CREWLET_NOTE_HELPER_DIR` while it is set, else beside its executable, then there; an install missing them refuses every executor launch, and `scripts/notices` covers the helper's modules

## Releases
Signed binaries and a container image, built by goreleaser from a `v*` tag.
- **The tag is the version** (`CLAUDE.md`): `main`'s image is stamped `main-` plus twelve hex digits of the commit, and a binary built any other way reports its module build info
- **Pushing the tag is the whole release** — four targets, keyless Sigstore signatures on the checksums and the image, the image on GHCR as `v<version>`, and a GitHub Release whose notes are generated from the merged pull request titles. A pre-release gets neither "Latest" nor the `latest` image tag
- **`main` gets an image, not a release** — `.github/workflows/main-image.yml` rebuilds `ghcr.io/crewlet/crewlet:main` on every push to `main` without goreleaser: it runs `make image-context`, which copies `.goreleaser.yaml`'s build settings and uses the same `Dockerfile`, so a change to the release build must be made in both `.goreleaser.yaml` and the Makefile's `image-context`. Nothing builds it before a merge, so read its run on `main` after one (`RELEASING.md`, "Images from main")
- **The release surface is not asserted; read it when you touch it** — `goreleaser check`, the snapshot job and the binary's version comparison catch some of it; `.github/release.yml`, `.github/dependabot.yml`, `docs-publish.yml`'s `workflow_run` name and the `v*` tag trigger fail silently (`RELEASING.md`, "What CI already guards")
