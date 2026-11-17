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
- **Go 1.27+** — the module pins its own toolchain; `GOTOOLCHAIN=auto` fetches it rather than failing on a mismatch. Everything is pure Go: `CGO_ENABLED=0` builds every release target, which makes the cross-compile matrix a plain `GOOS`/`GOARCH` loop over the four platforms the store driver has a database engine for. It does NOT make the linux artifact static; see Turso
- **Turso** (`turso.tech/database/tursogo`) — the embedded store and the ONLY driver: there is no second driver and no setting to select one, so use what Turso has. It needs no cgo, but its engine is a ~20 MB native library embedded in the driver and extracted at runtime into a shared per-user cache, which `internal/store/turso.go` prepares under a lock because upstream writes it non-atomically and panics on what a concurrent reader sees. That library bounds the release matrix — linux and darwin on amd64 and arm64, no Windows; `internal/store/platform.go` makes any other build a compile error — and makes the linux binary dynamically linked against glibc, so it does not run on musl or `scratch`. Do NOT "fix" that with `-extldflags -static`: a static program cannot `dlopen`, and that build segfaults on its first query (ci.yml's `cross` job asserts the artifact stays dynamic). `-tags musl` picks the embedded `.so`, not the binary's linkage, so it gives no Alpine build either

## Releases
Signed binaries and a container image, built by goreleaser from a `v*` tag; `RELEASING.md` is the runbook.
- **The tag is the version** — nothing in the tree records one. goreleaser stamps it into `internal/version.value` at link time (the image of `main` is stamped `main-` and twelve hex digits of the commit), and a binary built any other way reports its module build info. Never add a literal version constant back
- **Semver** — the minor number moves for features, the patch number for fixes
- **Pushing the tag is the whole release** — four targets, keyless Sigstore signatures on the checksums and the image, the image on GHCR as `v<version>`, and a GitHub Release whose notes are generated from the merged pull request titles. A pre-release takes neither "Latest" nor the `latest` image tag
- **`main` gets an image, and it is not a release** — `.github/workflows/main-image.yml` rebuilds `ghcr.io/crewlet/crewlet:main` on every push to `main` without goreleaser: `make image-context` reuses `.goreleaser.yaml`'s build settings and the same `Dockerfile`, so a change to the release build is a change to both. Nothing builds it before a merge, so read its run on `main` after one (`RELEASING.md`, "Images from main")
- **The release surface is not asserted — read it when you touch it** — `goreleaser check`, the snapshot job and the binary's version comparison catch some of it; `.github/release.yml`, `.github/dependabot.yml`, `docs-publish.yml`'s `workflow_run` name and the `v*` tag trigger fail silently (`RELEASING.md`, "What CI already guards")
