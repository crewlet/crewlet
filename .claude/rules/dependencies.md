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
Dependabot watches every manifest — the workflows, `go.mod`, `dashboard/package.json`, the `Dockerfile` and `docker-compose.yml` — from `.github/dependabot.yml`: one entry per surface, weekly, each pinning the prefix `build(deps)`, because unset Dependabot infers one and can write a bare `Bump …` into `main` and the release notes. `CONTRIBUTING.md` ("Dependency updates") has the detail. The rules:
- **Group only where a split bump is broken** — the React family (`react`, `react-dom`, `@types/react`, `@types/react-dom`: react-dom asserts at runtime that it and `react` carry the exact same version), the vite family (`vite`, `vitest`, `@vitejs/plugin-react`: `vite` is a required peer of the other two, so a lone major `ERESOLVE`s), and `@crewlethq/*` (the design system pins its own packages exactly). Nothing else: a group titles every bump vaguely and puts one member's bad release in front of the rest
- **Three workflows handle the bumps, and every diff to one is security review**, since nothing asserts their guards: `dependabot-dashboard.yml` rebuilds and pushes the bundle of a dashboard bump that changes the emitted bytes (with `DASHBOARD_BUNDLE_TOKEN`, a fine-grained Dependabot secret for this repository with Contents read and write; its commit carries `[dependabot skip]`); `dependabot-recreate.yml` asks a conflicted bump Dependabot will not rebase to `@dependabot recreate` (with `DEPENDABOT_RECREATE_TOKEN`, Pull requests read and write, in the `dependabot-recreate` environment); `dependabot-merge.yml` approves and queues `gh pr merge --auto --squash` only when Dependabot is both the author and the actor, which relies on `main`'s protection rule requiring the `ci` checks and on "Allow auto-merge". Read every `if:`, `permissions:` and token scope on each edit
- **Tool servers the docs launch are pins Dependabot cannot see** — `internal/config/toolservers_test.go` holds every package a page, a skill or the curated MCP catalogue fetches by name to an exact release, the one its catalogue entry (`internal/mcp/catalogue`) records as `Release`; moving one is reading and running the new release. The shipped examples float instead (`@latest` at most), except a server a bundled tool skill triggers on by tool name
- **Pick the newest release, then pin it exactly** — established from the registry rather than memory, written as a literal (`4.2.4`), never `latest` or `@main`, which nothing moves. A version held back on purpose carries its reason at the pin
- **The git and Chromium pins are watched by `.github/workflows/pins.yml`, not Dependabot**, one leg of its matrix each — weekly, `scripts/git-pin-check.sh` verifies each newer git release's signature against `scripts/git-signing-key.asc` and opens one issue, `git pin behind upstream`, naming the pin's new version and SHA-256, and it fails 60 days before the key's signing subkeys expire. A bump moves a pin's version and SHA-256 together, from that report, and is titled `build(deps): bump git from <a> to <b>`; nothing asserts the workflow, so read its `permissions:`, both issue titles and its `key` step on every edit (`CONTRIBUTING.md`, "The git pin is checked every week")
- **The Chromium pin follows Chrome for Testing** — `scripts/chromium-pin-check.sh` holds the newest pin to Stable's major and the floor pin to the newest patch of `internal/browser/cdp.MinMajor`'s major, downloads every platform's zip of a release it proposes and hashes it (no signature exists for these builds), and keeps one issue, `chromium pin behind upstream`; a bump is titled `build(deps): bump chromium from <a> to <b>` or `build(deps): bump chromium's floor from <a> to <b>`, and raising the floor is a change to `MinMajor` that moves the floor pin with it (`CONTRIBUTING.md`, "The Chromium pin is checked every week")
- **A new dependency surface ships with its `updates:` entry**, and actions are pinned to a version tag or a full SHA (a branch ref gets no update pull requests)

## What `scripts/` holds
```
scripts/              # The two vendor dev-loop bootstraps, the DCO gate
                      #   (check-signoff.sh) with its suite (check-signoff_test.sh),
                      #   and the git the engine runs: git-pin.env (the pins),
                      #   git.sh (the one recipe that builds them), git-pin-check.sh
                      #   (the weekly check against upstream, verifying each release
                      #   against git-signing-key.asc) and their suite git-pin_test.sh;
                      #   and the Chromiums the suite drives: chromium-pin.env (the
                      #   Chrome for Testing pins), chromium.sh (the one recipe that
                      #   fetches them), chromium-pin-check.sh (the weekly check
                      #   against Chrome for Testing) and their suite
                      #   chromium-pin_test.sh
```
