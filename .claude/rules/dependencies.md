---
paths:
  - "{go.mod,go.sum,Dockerfile,docker-compose.yml}"
  - "dashboard/{package.json,package-lock.json}"
  - "internal/browser/boxhelper/tree/{package.json,package-lock.json,installed.sha256}"
  - ".github/dependabot.yml"
  - ".github/workflows/*.yml"
  - "scripts/*-pin*"
  - "scripts/{git,chromium,node}.sh"
  - "scripts/playwright-tree*.sh"
  - "internal/config/toolservers_test.go"
  - "internal/mcp/catalogue/**"
  - ".claude/worktrees/*/{go.mod,go.sum,Dockerfile,docker-compose.yml}"
  - ".claude/worktrees/*/dashboard/{package.json,package-lock.json}"
  - ".claude/worktrees/*/internal/browser/boxhelper/tree/{package.json,package-lock.json,installed.sha256}"
  - ".claude/worktrees/*/.github/dependabot.yml"
  - ".claude/worktrees/*/.github/workflows/*.yml"
  - ".claude/worktrees/*/scripts/*-pin*"
  - ".claude/worktrees/*/scripts/{git,chromium,node}.sh"
  - ".claude/worktrees/*/scripts/playwright-tree*.sh"
  - ".claude/worktrees/*/internal/config/toolservers_test.go"
  - ".claude/worktrees/*/internal/mcp/catalogue/**"
---

# Dependency Updates
Dependabot watches every manifest (the workflows, `go.mod`, `dashboard/package.json`, `internal/browser/boxhelper/tree/package.json`, the `Dockerfile`, `docker-compose.yml`) from `.github/dependabot.yml`: one entry per surface, weekly, each pinning the prefix `build(deps)`, since unset Dependabot infers one and can write a bare `Bump …` into `main` and the release notes. Detail: `CONTRIBUTING.md` ("Dependency updates"). How to pick and pin a version, and which tool servers are pinned, is in `CLAUDE.md`.
- **Group only where a split bump is broken**: the React family (`react`, `react-dom`, `@types/react`, `@types/react-dom`; react-dom asserts at runtime that it and `react` are the exact same version), the vite family (`vite`, `vitest`, `@vitest/*`, `@vitejs/plugin-react`; `vite` is a required peer of the plugin and vitest, so a lone major `ERESOLVE`s, and `vitest` and `@vitest/browser-playwright` peer each other at the exact version), and `@crewlethq/*` (`@crewlethq/ui` pins the tokens and icons exactly, and the scope's four packages release together). Nothing else: a group titles every bump vaguely and puts one member's bad release in front of the rest.
- **Four workflows handle the bumps; every diff to one is security review**, since nothing asserts their guards. Read every `if:`, `permissions:` and token scope on each edit.
  - `dependabot-dashboard.yml` rebuilds and pushes the bundle when a dashboard bump changes the emitted bytes, with `DASHBOARD_BUNDLE_TOKEN` (a fine-grained Dependabot secret for this repository, Contents read and write); its commit carries `[dependabot skip]`.
  - `dependabot-notehelper.yml` is its twin for a gomod bump that moves the note helper's committed builds (`make notehelper`), with the same guards, split and token; its `push` job writes only the two builds, by name.
  - `dependabot-recreate.yml` asks a conflicted bump Dependabot will not rebase to `@dependabot recreate`, with `DEPENDABOT_RECREATE_TOKEN` (Pull requests read and write, in the `dependabot-recreate` environment).
  - `dependabot-merge.yml` approves and queues `gh pr merge --auto --squash` only when Dependabot is both author and actor, and never for a pull request that changes only `internal/browser/boxhelper/tree/` (its `paths-ignore`); it relies on `main`'s protection rule requiring the `ci` checks and on "Allow auto-merge".
- **Tool servers are pins Dependabot cannot see**: each one's `internal/mcp/catalogue` entry records its release as `Release`, and moving one means reading and running the new release.
- **The Playwright tree a seat's box installs is a manifest Dependabot watches**: `internal/browser/boxhelper/tree/`'s `package.json` (`@playwright/mcp` alone, exact, no `engines`) and `package-lock.json` have an npm entry, no group, and `dependabot-merge.yml` never queues a bump of them. `TestTheLockIsTheReleaseSomebodyRead` keeps each bump red until `internal/browser`'s release constants name it, `TestTheLockedCoreExpectsTheChromiumSomebodyRead` until its expected Chromium does, and `scripts/playwright-tree.sh` and `TestTheInstalledDigestNamesTheFilesTheLockInstalls` until `tree/installed.sha256` names the files the new lock installs, so it lands only once read, from a branch of your own, as `build(deps): bump playwright mcp from <a> to <b>` (`CONTRIBUTING.md`, "A Playwright MCP bump is read before it lands"); Dependabot closes its own.
- **`.github/workflows/pins.yml`, not Dependabot, watches the git, Chromium, floor Node and sherpa-onnx pins**, one matrix leg each. Nothing asserts the workflow, so read its `permissions:`, each leg's issue title and the key step of the git and floor Node legs on every edit.
  - git (`CONTRIBUTING.md`, "The git pin is checked every week"): weekly, `scripts/git-pin-check.sh` verifies each newer git release's signature against `scripts/git-signing-key.asc` and opens one issue, `git pin behind upstream`, naming the pin's new version and SHA-256; it fails 60 days before the key's signing subkeys expire. A bump moves a pin's version and SHA-256 together, from that report, and is titled as the report says: `build(deps): bump git from <a> to <b>` for the node pin, `build(deps): bump git's node floor from <a> to <b>` or `… client floor …` for a floor.
  - Chromium follows Chrome for Testing (`CONTRIBUTING.md`, "The Chromium pin is checked every week"): `scripts/chromium-pin-check.sh` holds the newest pin to Stable's major and the floor pin to the newest patch of `internal/browser.MinChromiumMajor`'s major, downloads and hashes every platform's zip of a release it proposes (these builds have no signature), and keeps one issue, `chromium pin behind upstream`. A bump is titled `build(deps): bump chromium from <a> to <b>` or `build(deps): bump chromium's floor from <a> to <b>`; raising the floor changes `MinChromiumMajor` and moves the floor pin with it.
  - floor Node (`CONTRIBUTING.md`, "The floor Node is checked every week"): `scripts/node-pin-check.sh` holds the pin to the newest release of Node `internal/browser/boxhelper.MinNodeMajor`, verifying each release it proposes against the releasers' keys in `scripts/node-release-keys.asc` (held to the fingerprints it pins) and each tarball against that signed list; it fails from 60 days before the major's end of life, naming the oldest major above it in long-term support whose own end is more than 60 days away, and its `key` step from 60 days before any releaser's key stops signing, and keeps one issue, `node pin behind upstream`. A bump is titled `build(deps): bump node's floor from <a> to <b>`; a raise, `build(deps): raise node's floor from <a> to <b>`, changes `MinNodeMajor`, and `MinNPMMajor` to the npm the new floor bundles with `scripts/playwright-tree.sh`'s `min_npm`, and moves the pin with them. It is the one Node version the tree names (`CONTRIBUTING.md`, "Which node").
- **Actions are pinned to a version tag or a full SHA**: a branch ref gets no update pull requests.

## What `scripts/` holds
- The two vendor dev-loop bootstraps.
- The DCO gate, `check-signoff.sh`, and its suite `check-signoff_test.sh`.
- The git the engine runs: `git-pin.env` (the pins), `git.sh` (the one recipe that builds them), `git-pin-check.sh` (the weekly check above, with `git-signing-key.asc`), and their suite `git-pin_test.sh`.
- The Chromiums the suite drives: `chromium-pin.env` (the Chrome for Testing pins), `chromium.sh` (the one recipe that fetches them), `chromium-pin-check.sh` (the weekly check above), and their suite `chromium-pin_test.sh`.
- The floor Node, the oldest the box helper supports: `node-pin.env` (the release's pins), `node.sh` (the one recipe that fetches it, keeping its npm), `node-pin-check.sh` (the weekly check above, with `node-release-keys.asc`), and their suite `node-pin_test.sh`.
- The Playwright tree the suite installs: `playwright-tree.sh` (the one recipe that installs `internal/browser/boxhelper/tree`'s lock with npm, for `make playwright-tree`), and its suite `playwright-tree_test.sh`.
