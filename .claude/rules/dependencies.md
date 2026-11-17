---
paths:
  - "{go.mod,go.sum,Dockerfile,docker-compose.yml}"
  - "dashboard/{package.json,package-lock.json}"
  - ".github/dependabot.yml"
  - ".github/workflows/*.yml"
  - "scripts/*-pin*"
  - "scripts/{git,chromium}.sh"
  - "internal/config/toolservers_test.go"
  - "internal/mcp/catalogue/**"
  - ".claude/worktrees/*/{go.mod,go.sum,Dockerfile,docker-compose.yml}"
  - ".claude/worktrees/*/dashboard/{package.json,package-lock.json}"
  - ".claude/worktrees/*/.github/dependabot.yml"
  - ".claude/worktrees/*/.github/workflows/*.yml"
  - ".claude/worktrees/*/scripts/*-pin*"
  - ".claude/worktrees/*/scripts/{git,chromium}.sh"
  - ".claude/worktrees/*/internal/config/toolservers_test.go"
  - ".claude/worktrees/*/internal/mcp/catalogue/**"
---

# Dependency Updates
Dependabot watches every manifest (the workflows, `go.mod`, `dashboard/package.json`, the `Dockerfile`, `docker-compose.yml`) from `.github/dependabot.yml`: one entry per surface, weekly, each pinning the prefix `build(deps)`, since unset Dependabot infers one and can write a bare `Bump …` into `main` and the release notes. Detail: `CONTRIBUTING.md` ("Dependency updates"). How to pick and pin a version, and which tool servers are pinned, is in `CLAUDE.md`.
- **Group only where a split bump is broken**: the React family (`react`, `react-dom`, `@types/react`, `@types/react-dom`; react-dom asserts at runtime that it and `react` are the exact same version), the vite family (`vite`, `vitest`, `@vitejs/plugin-react`; `vite` is a required peer of the other two, so a lone major `ERESOLVE`s), and `@crewlethq/*` (the design system pins its own packages exactly). Nothing else: a group titles every bump vaguely and puts one member's bad release in front of the rest.
- **Three workflows handle the bumps; every diff to one is security review**, since nothing asserts their guards. Read every `if:`, `permissions:` and token scope on each edit.
  - `dependabot-dashboard.yml` rebuilds and pushes the bundle when a dashboard bump changes the emitted bytes, with `DASHBOARD_BUNDLE_TOKEN` (a fine-grained Dependabot secret for this repository, Contents read and write); its commit carries `[dependabot skip]`.
  - `dependabot-recreate.yml` asks a conflicted bump Dependabot will not rebase to `@dependabot recreate`, with `DEPENDABOT_RECREATE_TOKEN` (Pull requests read and write, in the `dependabot-recreate` environment).
  - `dependabot-merge.yml` approves and queues `gh pr merge --auto --squash` only when Dependabot is both author and actor; it relies on `main`'s protection rule requiring the `ci` checks and on "Allow auto-merge".
- **Tool servers are pins Dependabot cannot see**: each one's `internal/mcp/catalogue` entry records its release as `Release`, and moving one means reading and running the new release.
- **`.github/workflows/pins.yml`, not Dependabot, watches the git and Chromium pins**, one matrix leg each. Nothing asserts the workflow, so read its `permissions:`, both issue titles and its `key` step on every edit.
  - git (`CONTRIBUTING.md`, "The git pin is checked every week"): weekly, `scripts/git-pin-check.sh` verifies each newer git release's signature against `scripts/git-signing-key.asc` and opens one issue, `git pin behind upstream`, naming the pin's new version and SHA-256; it fails 60 days before the key's signing subkeys expire. A bump moves a pin's version and SHA-256 together, from that report, and is titled as the report says: `build(deps): bump git from <a> to <b>` for the node pin, `build(deps): bump git's node floor from <a> to <b>` or `… client floor …` for a floor.
  - Chromium follows Chrome for Testing (`CONTRIBUTING.md`, "The Chromium pin is checked every week"): `scripts/chromium-pin-check.sh` holds the newest pin to Stable's major and the floor pin to the newest patch of `internal/browser/cdp.MinMajor`'s major, downloads and hashes every platform's zip of a release it proposes (these builds have no signature), and keeps one issue, `chromium pin behind upstream`. A bump is titled `build(deps): bump chromium from <a> to <b>` or `build(deps): bump chromium's floor from <a> to <b>`; raising the floor changes `MinMajor` and moves the floor pin with it.
- **Actions are pinned to a version tag or a full SHA**: a branch ref gets no update pull requests.

## What `scripts/` holds
- The two vendor dev-loop bootstraps.
- The DCO gate, `check-signoff.sh`, and its suite `check-signoff_test.sh`.
- The git the engine runs: `git-pin.env` (the pins), `git.sh` (the one recipe that builds them), `git-pin-check.sh` (the weekly check above, with `git-signing-key.asc`), and their suite `git-pin_test.sh`.
- The Chromiums the suite drives: `chromium-pin.env` (the Chrome for Testing pins), `chromium.sh` (the one recipe that fetches them), `chromium-pin-check.sh` (the weekly check above), and their suite `chromium-pin_test.sh`.
