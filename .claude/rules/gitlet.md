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
- **git ≥ 2.55 on a Gitlet node, ≥ 2.40 for a client** (ADR-0087) — Gitlet, the engine's own git host, runs real upstream git as a subprocess (`internal/gitlet/gitproc`), never a reimplementation, and the engine image BUILDS the git it runs from the tarball `scripts/git-pin.env` pins by version and SHA-256, through `scripts/git.sh`, the ONE recipe the image, CI and a contributor share (it links no OpenSSL of its own; `RELEASING.md` has the licence reading). The SUITE runs that pin, never the machine's git: the make targets that run the suite (`test`, `test-solo`, `test-norace`, `test-gitproc`, `bench`) depend on `make git` and put the node pin first on PATH (`GIT_ENV`); a one-package loop runs it as two statements, `bin=$(scripts/git.sh node) && PATH="$bin:$PATH" go test ./internal/gitlet/…`, never one substitution, which is empty when the recipe fails. A git-driven test on a git below its floor FAILS rather than skips (`gittest.RequireNode`, `gittest.Require`), and `TestTheGitFloorsPassTheSuite` runs the two floors against each other under every make target that runs it. A diff to the pin or the recipe changes every Gitlet node's git, so it is release surface (`CONTRIBUTING.md`, Development setup)
