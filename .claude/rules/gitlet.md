---
paths:
  - "internal/{gitlet,e2e,engine,gitref,jsapi,sandbox}/**"
  - "scripts/git*"
  - "{Makefile,Dockerfile}"
  - ".claude/worktrees/*/internal/{gitlet,e2e,engine,gitref,jsapi,sandbox}/**"
  - ".claude/worktrees/*/scripts/git*"
  - ".claude/worktrees/*/{Makefile,Dockerfile}"
---

# The Git Gitlet Runs

- Gitlet runs git as a subprocess (`internal/gitlet/gitproc`), never a reimplementation, at the floors `CLAUDE.md` gives (ADR-0087).
- The engine image BUILDS its git from the tarball `scripts/git-pin.env` pins by version and SHA-256, via `scripts/git.sh`: the ONE recipe the image, CI and contributors share. The git it builds links no OpenSSL of its own (`RELEASING.md` has the licence reading).
- Every make target that runs the suite (`test`, `test-solo`, `test-norace`, `test-gitproc`, `bench`) depends on `make git` and puts the node pin first on PATH (`GIT_ENV`). A one-package loop runs the pin too, as two statements: `bin=$(scripts/git.sh node) && PATH="$bin:$PATH" go test ./internal/gitlet/…`, never one substitution, which is empty when the recipe fails (`CONTRIBUTING.md`, "Development setup").
- A git-driven test on a git below its floor FAILS, never skips (`gittest.RequireNode`, `gittest.Require`). `TestTheGitFloorsPassTheSuite` runs the two floors against each other under every make target that runs it.
- A diff to the pin, the recipe or the signing key changes every Gitlet node's git, so it is release surface (`CONTRIBUTING.md`, "The git pin is checked every week").
