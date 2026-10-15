# Crewlet — Project Conventions

## IMPORTANT — Read Docs First
Before starting ANY task, you MUST read the following project docs to understand the project context:
- `docs/concepts/overview.md` — project vision, design principles, and architecture
- `docs/concepts/organization-model.md` — hierarchy, roles, and identity
- `docs/concepts/agent-runtime.md` — agent lifecycle and execution
- `docs/getting-started/quickstart.md` — getting started guide

See `docs/index.md` for the full documentation index.

Do this at the start of every conversation, before writing or modifying any code. These docs are the source of truth for how the system should work.

## Overview
Crewlet is a Go engine for orchestrating hierarchically organized AI agent companies.

## Tech Stack
- **Go 1.27+** — the module pins its own toolchain; `GOTOOLCHAIN=auto` fetches it rather than failing on a mismatch. Everything is pure Go: `CGO_ENABLED=0` builds every release target, which makes the cross-compile matrix a plain `GOOS`/`GOARCH` loop over the four platforms the store driver has a database engine for. It does NOT make the linux artifact static; see Turso
- **The standard library first** — `net/http`, `log/slog`, `context`, `encoding/json`. A dependency has to earn its place against what `std` already does
- **Turso** (`turso.tech/database/tursogo`) — the embedded store and the ONLY driver: there is no second driver and no setting to select one, so use what Turso has. It needs no cgo, but its engine is a ~20 MB native library embedded in the driver and extracted at runtime into a shared per-user cache, which `internal/store/turso.go` prepares under a lock because upstream writes it non-atomically and panics on what a concurrent reader sees. That library bounds the release matrix — linux and darwin on amd64 and arm64, no Windows; `internal/store/platform.go` makes any other build a compile error — and makes the linux binary dynamically linked against glibc, so it does not run on musl or `scratch`. Do NOT "fix" that with `-extldflags -static`: a static program cannot `dlopen`, and that build segfaults on its first query (ci.yml's `cross` job asserts the artifact stays dynamic). `-tags musl` picks the embedded `.so`, not the binary's linkage, so it gives no Alpine build either
- **NATS JetStream** (`github.com/nats-io/nats-server`) — the event stream and the ONLY broker; there is no Pulsar backend. Embedded by default, in the engine's own process with no listener; a fleet clusters the embedded members (`stream.cluster`) or dials an external NATS cluster (`stream.type: nats`), and a node without the `data` role joins the members' broker as a LEAF with JetStream off. The coordination KV lives on the stream's own broker on every topology: over the queue's connection on an external broker, over a second watched connection on the embedded one (ADR-0001). Either connection NATS closes for good stops the node
- **`github.com/modelcontextprotocol/go-sdk`** — MCP client and server
- **OpenTelemetry** (`go.opentelemetry.io/otel` and the OTLP trace exporters) — tracing, configured from the standard `OTEL_*` environment rather than Tier A. The provider is always installed and only the exporter is conditional, because a trace id is an event-store column before it is an exporter's concern
- **Coding CLIs are the only LLM backend** — every model call runs a vendor's own CLI as a subprocess (Claude Code for Anthropic's models, on a plan or an API key; Codex for OpenAI's; OpenCode for open models), driven through `internal/providers/llm/cliagent`. There is no HTTP client to a model vendor and none is to be added: the vendors move their request shapes, caching and sign-in with every model, and their CLIs carry that for us (`docs/concepts/llm-backends.md`). The CLI runs its OWN loop: a phase's tools reach it as ONE MCP server, `crewlet`, which it calls natively. A seat's executor is always its CLI's own fully permissioned run in a box (Claude Code or OpenCode, the two CLIs with an in-box runner; ADR-0066), reaching the seat's tools over the bridge (`internal/api/mcpbridge`), which holds every call to the seat's surface; every other phase (the reviewer, delegated workers, the onboarding pass, the side passes) runs on the engine host with the CLI's local tools denied, through `crewlet mcp-relay` into a per-run unix socket (`internal/toolsession`), every call held to the phase's gates by the tool loop. No tool rides a prompt and no call is read out of a model's prose (ADR-0049)
- **`github.com/openai/openai-go`** — the embeddings client (`providers.embeddings`) and only that
- **Official vendor SDKs** for integrations where one exists; a thin typed client where one does not (Mattermost, GitLab, Confluence)
- **`github.com/aws/aws-sdk-go-v2`** — the object store's `s3` backend and only that: request signing, the regional endpoint rules and the credential chain. Its HTTP goes through `httpx`. The default backend, `nats`, needs nothing beyond the broker
- **YAML** (`gopkg.in/yaml.v3`) — config parsing
- **`testing`** — the standard library's, with no assertion framework. Table tests where the cases vary, named tests where the reasoning does

## This Is a Public Open-Source Repository
Crewlet is MIT-licensed, developed in the open at `github.com/crewlet/crewlet`, ships signed binaries and a container image from a `v*` tag (plus `ghcr.io/crewlet/crewlet:main`, rebuilt on every push to `main`, which is not a release), and its `docs/` tree is served at docs.crewlet.ai. Everything committed is published permanently — git history keeps what a later commit deletes — so write every change as something a stranger will read, run, and depend on.

Rules:
- **Never commit a secret, and never fix one by deleting it.** No real API keys, tokens, PATs, webhook secrets, private hostnames, internal URLs, or customer/employee names — in code, tests, docs, examples, fixtures, or commit messages. Examples use `${VAR}` references and placeholders (`example.com`, `U0FOUNDER`, `sk-ant-...`). A credential that lands in a commit is compromised the moment it is pushed: it must be **rotated**, and removing it in a later commit is not a fix.
- **Commit subjects are semantic** — `type(scope): summary`, imperative, lowercase, no trailing period, ≤72 chars. Type is one of `feat` / `fix` / `docs` / `refactor` / `perf` / `test` / `ci` / `build` / `chore` / `revert`. Scope is the component: the package directory under `internal/`, `dashboard` for `dashboard/` and `static/dashboard/`, `cli` for `cmd/crewlet/`, and outside `internal/` the area (`docs`, `deps`, `examples`, `schema`, `scripts`, or the workflow name for `ci`). An incidental fix or tuning the change needed goes in its own commit with its own scope; an unrelated one goes in an issue (see "Fix the Bugs That Affect Your Work" below). A pull request lands as a squash or a merge commit, so its title takes the same form. Nothing enforces this; the full rules are in `CONTRIBUTING.md`.
- **Every commit is signed off** — `git commit -s`, certifying the `DCO` file at the root. Enforced: `scripts/check-signoff.sh` (with its own suite, `scripts/check-signoff_test.sh`) runs in `make check` and in ci.yml's `sign-off` job over a pull request's own commits. Presence is checked, never a match against the author, and merge commits are exempt. Nothing checks the commit the merge button writes; the repository's merge settings keep the trailers. A missing trailer is repaired by rewriting the commit (`git commit --amend -s --no-edit`, or `git rebase --signoff origin/main`).
- **The pull request title is the release note.** There is no `CHANGELOG.md`: each Release body is generated from the titles of the pull requests merged since the previous tag (grouped by `.github/release.yml`), so a user-visible change's title must read as a release note for someone running Crewlet.
- **The root-level files are canonical for process**, and this file must not contradict them: `CONTRIBUTING.md`, `RELEASING.md`, `SECURITY.md`, `CODE_OF_CONDUCT.md`, `DCO` (verbatim upstream text, never edited), `LICENSE`. A rule changed here is changed there in the same commit.
- **Follow the repository's own templates** — `.github/pull_request_template.md` for pull requests, `.github/ISSUE_TEMPLATE/` for issues.
- **Third-party code must be MIT-compatible and attributed.** Source files carry no license headers — do not start adding them.
- **Security issues never go in a public issue or a public PR description** — see `SECURITY.md`.

## No Band-Aids — Always Do the Proper Fix
Never apply band-aid fixes, workarounds, or partial solutions. Always implement the correct, complete fix — even if it is large, touches many files, or requires significant refactoring. A quick hack that "works for now" creates tech debt that compounds.

**When suggesting a fix, the cost of the fix is never a factor.** We do not care how long it takes, how many lines or files it touches, or how big the change ends up. Always propose the proper fix — the one that solves the problem at the root — regardless of size or effort: if it is a 2000-line refactor across 50 files or a subsystem rewrite, suggest that. Never pre-filter suggestions on effort, time, or scope.

Rules:
- **Root-cause fixes only** — diagnose the actual problem and fix it at the source. Do not patch symptoms.
- **No workarounds** — no special cases, flags, or conditional logic around a broken abstraction: fix the abstraction. A compatibility shim — an alias, a both-spellings fallback, an adapter — is the same move under a respectable name, and for a surface no release ever shipped there is nothing on the other side of it to be compatible with (see "Breaking Changes Are Free Until a Tag Ships Them" below).
- **Size, time and lines of code are not factors** — 20 files or 200, an hour or a week, one line or a thousand: the scope of the fix matches the scope of the problem, and a one-line hack that papers over an issue is strictly worse than a thousand-line change that solves it.
- **Always suggest the proper fix first** — lead with the correct, complete fix; never hide it behind cheaper alternatives or present a "quick" option as the default.
- **No "temporary" solutions** — every change is one you are willing to maintain indefinitely.
- **Refactor when needed** — do not bolt new functionality onto a broken foundation.
- **Update everything** — a proper fix includes all tests, docs, and dependent code.

When in doubt, ask: "Is this the fix I would make if I had unlimited time and unlimited diff size?" If not, find the fix that is.

## Fix the Bugs That Affect Your Work — Open an Issue for Unrelated Ones
While working you WILL notice bugs, and what happens to one turns on a single question: **does it affect what you are implementing?** If it does, it is part of the task: fix it in this change. If it is totally unrelated, do not fix it here: open a GitHub issue for it, so it is on record and gets a change of its own — an unrelated fix folded into a change is a second change that nobody reviewed as one and that is reverted with the first. Doing neither is never an option: every bug you find leaves this change fixed or filed.

Rules:
- **A bug that affects your work is part of the task** — it does when your change depends on it (calls, reads or builds on the broken behaviour), when it is in the code or text you are changing, when your change would make it worse or newly reachable, when it is another copy of the defect you are fixing, or when it keeps your change from passing a test or gate. That it was broken before you arrived does not make it unrelated.
- **A totally unrelated bug gets an issue, not a fix** — one none of that describes, noticed while reading, debugging or reviewing something else. Leave its code alone.
- **"Totally unrelated" is the bar, not the default** — if you cannot show that none of those holds, fix it. An issue is never a way to skip a fix your change needs.
- **Write the issue as a bug report** — search the open issues first and add to one that already describes it. Otherwise open one per bug on `.github/ISSUE_TEMPLATE/bug_report.md`, labelled `bug` and typed `Bug`: what is wrong and how it shows; where (a permalink, or `path:line` at a commit SHA); under Environment, that commit in place of a release when you found it in the source; and the proper fix you would make. An issue is as public and permanent as a commit.
- **A security bug never goes public** — anything you believe is exploitable, whether or not it affects your work, goes through `SECURITY.md`'s private reporting and into no issue, commit message or pull request description. If you cannot file that report yourself, tell the user in the session and let them decide how its fix lands.
- **Fix it properly** — root-cause, with tests and docs (see "No Band-Aids" above and "No Deferred Work" below).
- **Surface what you fixed and what you filed** — call out every incidental fix in your summary and commit message, in a commit of its own where that keeps history clean, and link every issue you opened or added to in your summary and the pull request description. If you could not open one, say so and give the user its title and body.
- **If a fix your change needs can't fit in it** — it needs its own coordinated effort or a risky redesign — do not drop it, and do not file it as though it were unrelated: fix what you safely can and raise the rest with the user (same rule as "No silent narrowing of scope" below).

When in doubt, ask: "Did I notice anything broken that I'm about to leave broken?" If yes: fixed here if it affects this change, an issue if it is totally unrelated — never neither.

## Tuning Knobs Are Findings Too — Don't Defer Them Either
A value that is not a crash but is plainly wrong is a finding like a bug: a magic number, an arbitrary timeout, retry count, backoff, batch size, poll interval, cache TTL, concurrency cap, page or buffer size, rate limit, log level, flag default, temperature or model choice, a baked-in value that should be config, or config that should be a constant. "Just a tuning knob" is the moment the work gets dropped, and a wrong knob is a slow-motion bug waiting for production load — a 1-second timeout that flaps, a retry count that hammers a dying dependency, a temperature that makes a judge non-deterministic. The right value is often a judgment call, and a judgment call is something you make, not something you skip.

Rules:
- **It goes where a bug would** (see above) — a knob your change depends on, touches or makes worse is tuned in this change, to the same bar as a real fix; one in code your change does not touch gets an issue naming the value you would set and why.
- **Tune to the justified value now** — the value you can defend from the code, the data, and the surrounding behaviour. No placeholder, vague comment, or "configurable later" hook.
- **Expose or collapse, deliberately** — a value callers genuinely need to vary becomes real config; a knob with no honest reason to differ becomes a plain constant.
- **Tie the value to something real** — record why this value where the next reader will see it (the config doc, a comment at the definition, the commit message), anchored to the dependency's p99, the page-size limit, the SLA, the memory budget.
- **Surface what you tuned** — every knob you changed, exposed, or collapsed, with the old and new value and the reason, in your summary and commit message; link the issue for every knob you filed.
- **If the right value needs production data, a benchmark you can't run, or a product decision** — make your best-justified call, mark it clearly, and raise it with the user with a concrete recommendation and the tradeoff. An issue for a knob you did not tune carries the same.

When in doubt, ask: "Am I calling this 'just a knob' so I don't have to decide its value?" If yes, decide it — or escalate it with a concrete recommendation — but never walk past it.

## No Deferred Work — Always Deliver the Clean Implementation
Never defer work: no TODOs, "follow-up" tickets, stub functions, or "we'll fix this later" comments. When you start a task you finish it — completely, cleanly, and in the same change. A long or sprawling task is a reason to commit to the full implementation, not to ship part of it.

Rules:
- **Decide and deliver** — make the call and implement it end-to-end; no half-built parallel paths, no decision left for "later".
- **No TODOs, no stubs, no placeholders** — no `TODO`, `FIXME`, `XXX`, `raise NotImplementedError`, or empty bodies pretending to be done. If it's in the diff, it's finished.
- **No "follow-up" tickets for the current scope** — what is part of the task ships in this change. An issue is for what lies outside it (see "Fix the Bugs That Affect Your Work" above), never for a part of it.
- **No "phase 1 / phase 2" hand-waving** — phase only when it is genuinely necessary (a real migration with data dependencies), and confirm it with the user first.
- **No silent narrowing of scope** — deliver the full scope, or raise it with the user before cutting.
- **Tests, docs, migrations all included** — a feature without tests or docs is not delivered.

When in doubt, ask: "If I stopped right now, would a reader of this diff consider the task done?" If not, keep going.

## Code Conventions
- **Interfaces are defined by the consumer**, in the package that calls them, and kept to what that caller needs. A provider exports a concrete type. There is no `interfaces.go`
- **`context.Context` first, always**, threaded rather than stored in a struct. A rollback or a teardown takes `context.WithoutCancel`: the failure it is undoing is often the cancellation itself, and a cleanup that inherits a dead context does nothing at all
- **Accept interfaces, return structs**
- **Zero values must be meaningful, or the type must refuse them** — a config field whose zero value is a valid setting needs a pointer or an explicit `Unset` sentinel, not a comment
- **Errors wrap with `%w`** and say what to do about it: an error a person reads names the field, the file or the variable to change. Sentinels are `errors.Is`-comparable; typed errors carry the identifier the caller needs
- **Every piece of state answers "who has to agree on it?"** before it is written anywhere. *This node alone*: the node's own database file. *Every data node, identically, derived from an ordered log*: the replicated estate, which ONLY a state log's applier writes. *The whole company, now*: the coordination store. *Whichever node holds this seat, current value only*: a compacted changelog (ADR-0014). *A replicated row names it and one store the fleet shares keeps its bytes*: the object store (ADR-0026) — and a table that names an object is declared in `internal/objstore/references`, or the collector deletes its files. "It has always been in the store" is not an answer. The stream is the write-ahead log and the SQL estate is derived, never the reverse (ADR-0002, 0003, 0004); `internal/store`'s estate gates refuse a table that skips the question
- **A three-valued answer is `(value, error)`**, never a bool: "held", "definitively not held" and "the store could not be reached" are three facts (see `internal/coord`)
- **IDs are `uuid.UUID`**, timestamps `time.Time` in UTC, durations `time.Duration`; a config field holding seconds is named `…Seconds` and converted once, at the edge
- **Enums are a named string type** with typed constants and a `Valid()` method, so an unknown value off the wire is a value rather than a panic
- **Concurrency**: a goroutine's lifetime belongs to whoever started it, and every one has a way to stop. Channels for handoff, a mutex for state; `sync.OnceValue` over a `sync.Once` plus a variable. Everything runs under `-race`
- **Comments explain WHY**, and especially why the obvious alternative is wrong. A package's rationale goes in its package doc, where `go doc` will find it

## Package Layout

```
cmd/crewlet/          # The one binary: run / validate / schema / migrate / budgets /
                      #   backup / retention / worklet / objects / fleet / seats /
                      #   artifacts / secrets / config / iam / llm / search, and the
                      #   six vendor CLIs (gitlab, github, jira, slack, confluence,
                      #   mattermost). Every command the switch dispatches must
                      #   appear in usage(): nothing connects them, a test asserts it
internal/             # Everything else, mapped package by package in
                      #   internal/README.md. Nothing here is importable from outside
                      #   the module: the engine has no stable Go API, only a CLI, a
                      #   config format and a wire protocol
dashboard/            # The dashboard's SOURCE: React 19 + TypeScript, built by Vite.
                      #   src/contract/ is the ONE home of every declaration an engine
                      #   test reads (clientsource), and its own suites are Vitest,
                      #   beside the modules they cover (`make dashboard-test`).
                      #   `make dashboard` builds it into static/dashboard and THAT IS
                      #   COMMITTED, so `go build ./...` needs no Node; CI rebuilds and
                      #   diffs it (`make dashboard-check`)
static/               # Embedded assets; static/dashboard/ is that build output,
                      #   compiled in via embed. NEVER hand-edited. Its protocol.js is
                      #   the wire protocol alone, which internal/e2e/replay.mjs runs
                      #   under `node`
examples/             # The Nimbus example company — a working two-tier config
docs/                 # Product documentation, published to docs.crewlet.ai
schema/               # GENERATED JSON Schema for both config tiers (`crewlet schema
                      #   <tier>`; a test regenerates and compares). Never hand-edit:
                      #   change the Go models
adr/                  # Decision records, for a decision that binds more than one
                      #   package (one inside a package stays in its doc). Each names
                      #   an Authority it may not restate and an Enforced-by (a test,
                      #   a compile error or `nothing`); internal/adr checks both ways
skills/               # FOUNDER-facing authoring skills for an AI assistant
scripts/              # The two vendor dev-loop bootstraps, and the DCO gate
                      #   (check-signoff.sh) with its suite (check-signoff_test.sh)
```

**`internal/README.md` maps every package** — what it is for, and the decisions it holds that a reader would be tempted to "fix" back to the obvious shape. Before you change a package, read its row there and its package doc (`go doc ./internal/<pkg>`), which is the authority. Change the code, the comment and the row together, and give a new package a row.

**A CLI command that talks to a running node authenticates with `CREWLET_API_TOKEN` and nothing else** — never a flag (a token typed as an argument lands in shell history and `ps`, so `-token` is an unknown flag), and never Tier A's token list, which is what a node accepts rather than a wallet for the CLI. The one exception is `iam token -login`, which mints the caller's own token by signing in as them for that one request (the password and any second-factor code read from the terminal or piped lines, never a flag) and signs out after. `crewlet mcp-relay` is no exception, because it is not a client of a node's API: it is the stdio end of a seat's tool session, which a coding CLI starts and which carries no credential at all — the engine's own `0700` socket directory is what admits it. A cookie write states `api.external_url`'s origin; a bearer write states none.

## Pre-commit Checks
Before committing, ALWAYS run **`make check`** and fix what it reports. It is every gate CI runs on a pull request:
- `gofmt -l` over `go run ./internal/sourcetree/gofiles` — never `gofmt -l .`, which enters every nested checkout under `.claude/worktrees/`. It is one of TWO formatting gates, at two standards: `golangci-lint run` enables the `gofmt` formatter with `simplify` on, and `goimports` beside it, so a redundant composite-literal type passes this one and fails that one. Repair with `make fmt` (`gofmt -s -w` over the same list, which clears both gofmt standards) or `golangci-lint fmt` handed the captured list (which also fixes the import grouping) — never plain `gofmt -w`, which cannot fix what the linter reports. `CONTRIBUTING.md` says the same.
- `go mod tidy -diff` (`make tidy` applies the patch)
- `scripts/check-signoff.sh` and its suite `scripts/check-signoff_test.sh`
- `go vet ./...` — the only vet: both test targets run `go test -vet=off`
- `golangci-lint run` — use a prebuilt release; one built with a Go older than `go.mod`'s refuses the module
- `go build ./...`, and a cross-compile of every release target
- the three dashboard gates: `dashboard-lint` (`npm run format:check`), `dashboard-check` (rebuild and diff the committed bundle) and `dashboard-test` (`npm run typecheck && npm test`)
- `make test`, then `make test-solo`

**Every package runs under the race detector with `-count=1`**, so a cached pass never answers for a change; `make test-norace` is a faster loop and not a gate. **The packages that stand up a multi-member broker run alone**, in `make test-solo` at `-p 1`, because N embedded NATS servers in one CPU-starved process leave replicated creates unanswered. Which packages those are is COMPUTED, never listed: a package declares it by importing `internal/solo` from its `TestMain`, and `internal/solo`'s roster guard fails the build in both directions. Never reach for a build tag, `-short`, `-skip` or a custom flag instead — each fails silently (`go doc ./internal/solo` has the measurements). `make check` runs both halves, so nothing is uncovered, but a run of `make test` by itself has NOT exercised the fleet. In CI each half is one job on its own runner: `test (race)` runs `make test` with `TEST_WEIGHTS`/`TEST_TIMINGS` (longest packages first, never fewer packages), and `end-to-end gates` runs `make test-solo`.

**A skip is not a pass.** Both test targets pipe `go test -json` through `internal/skipgate`, which fails the run on a skip not declared in `internal/skipgate/allowed.go`, each entry with a reason and a `When` (`Always`, also reported once it stops firing, or `Environment`). `internal/store`'s capability cases skip DELIBERATELY, and that is the mechanism rather than a gap: a feature Turso announces but does not reach Go yet turns into a passing test the day it lands. `node` runs three suites: the dashboard's own (through `npm`, which ships with it), `internal/e2e`'s client replay (in `make test-solo`) and `internal/sandbox/codingagent`'s probe of the OpenCode notes plugin the engine embeds (in `make test`), and every make target that runs one refuses to start without it. What `make check` does not cover (`make snapshot`) it prints when it passes.

**The Makefile is the command CI runs.** The two test jobs call `make test` and `make test-solo` themselves, so editing `GOTEST`, `GOTESTBUILD` or `GOTESTRUN` changes CI; every other job inlines its own command, mirroring its target by hand, and nothing checks that they agree. Change a target and its `ci.yml` step in the same commit, and give a new target its `## ` help line.

## Dependency Updates
Dependabot watches every manifest — the workflows, `go.mod`, `dashboard/package.json`, the `Dockerfile` and `docker-compose.yml` — from `.github/dependabot.yml`: one entry per surface, weekly, each pinning the prefix `build(deps)`, because unset Dependabot infers one and can write a bare `Bump …` into `main` and the release notes. `CONTRIBUTING.md` ("Dependency updates") has the detail. The rules:
- **Group only where a split bump is broken** — the React family (`react`, `react-dom`, `@types/react`, `@types/react-dom`: react-dom asserts at runtime that it and `react` carry the exact same version), the vite family (`vite`, `vitest`, `@vitejs/plugin-react`: `vite` is a required peer of the other two, so a lone major `ERESOLVE`s), and `@crewlethq/*` (the design system pins its own packages exactly). Nothing else: a group titles every bump vaguely and puts one member's bad release in front of the rest
- **Three workflows handle the bumps, and every diff to one is security review**, since nothing asserts their guards: `dependabot-dashboard.yml` rebuilds and pushes the bundle of a dashboard bump that changes the emitted bytes (with `DASHBOARD_BUNDLE_TOKEN`, a fine-grained Dependabot secret for this repository with Contents read and write; its commit carries `[dependabot skip]`); `dependabot-recreate.yml` asks a conflicted bump Dependabot will not rebase to `@dependabot recreate` (with `DEPENDABOT_RECREATE_TOKEN`, Pull requests read and write, in the `dependabot-recreate` environment); `dependabot-merge.yml` approves and queues `gh pr merge --auto --squash` only when Dependabot is both the author and the actor, which relies on `main`'s protection rule requiring the `ci` checks and on "Allow auto-merge". Read every `if:`, `permissions:` and token scope on each edit
- **Tool servers the docs launch are pins Dependabot cannot see** — `internal/config/toolservers_test.go` holds every package a page or a skill fetches by name to an exact release, the one `pinnedToolServers` records as `readAt`; moving one is reading and running the new release. The shipped examples float instead (`@latest` at most), except a server a bundled tool skill triggers on by tool name
- **Pick the newest release, then pin it exactly** — established from the registry rather than memory, written as a literal (`4.2.4`), never `latest` or `@main`, which nothing moves. A version held back on purpose carries its reason at the pin
- **A new dependency surface ships with its `updates:` entry**, and actions are pinned to a version tag or a full SHA (a branch ref gets no update pull requests)

## Releases
Signed binaries and a container image, built by goreleaser from a `v*` tag; `RELEASING.md` is the runbook.
- **The tag is the version** — nothing in the tree records one. goreleaser stamps it into `internal/version.value` at link time (the image of `main` is stamped `main-` and twelve hex digits of the commit), and a binary built any other way reports its module build info. Never add a literal version constant back
- **Semver** — the minor number moves for features, the patch number for fixes
- **Pushing the tag is the whole release** — four targets, keyless Sigstore signatures on the checksums and the image, the image on GHCR as `v<version>`, and a GitHub Release whose notes are generated from the merged pull request titles. A pre-release takes neither "Latest" nor the `latest` image tag
- **`main` gets an image, and it is not a release** — `.github/workflows/main-image.yml` rebuilds `ghcr.io/crewlet/crewlet:main` on every push to `main` without goreleaser: `make image-context` reuses `.goreleaser.yaml`'s build settings and the same `Dockerfile`, so a change to the release build is a change to both. Nothing builds it before a merge, so read its run on `main` after one (`RELEASING.md`, "Images from main")
- **The release surface is not asserted — read it when you touch it** — `goreleaser check`, the snapshot job and the binary's version comparison catch some of it; `.github/release.yml`, `.github/dependabot.yml`, `docs-publish.yml`'s `workflow_run` name and the `v*` tag trigger fail silently (`RELEASING.md`, "What CI already guards")

## Breaking Changes Are Free Until a Tag Ships Them
A `v*` tag is the only compatibility boundary: a surface no tag has shipped has nobody behind it. **Change it outright** — rename the field, reshape the struct, drop the flag, move the route — rather than carry a compatibility path for a version that never existed.

- **Establish what has shipped; never remember it** — `git tag --merged` and `git log <newest tag>..HEAD -- <path>`. Today the only release, `v0.1.0`, sits on the initial commit, which holds no `.go` file, so nothing in this tree has shipped. Re-run the check rather than trusting this line.
- **No compatibility path for an unreleased surface** — no alias, both-spellings reader, fallback, adapter, `V2` type or vestigial flag — **and no breaking-change ceremony**: no `!` and no `BREAKING CHANGE:` footer for a break nobody can be running.
- **A free break is still a complete break** — the rename lands everywhere in the same change (`docs/`, `examples/`, `skills/`, the dashboard, tests, package docs) and `schema/` is regenerated. Grep the old name to zero.
- **No compatibility with an older UNRELEASED build** — nothing it wrote is read, migrated, swept or tolerated, and code that exists only for one is deleted. Two constraints hold anyway:
  - **An applied migration is history, not source** — `schema_migrations` keys on the filename, so reshape with a new numbered migration under `internal/store/schema/`.
  - **This build must be a good OLDER peer to the next one** — a rolling upgrade mixes builds, so the forward mechanisms stay: the event envelope evolves additive-only and an unknown type round-trips losslessly; a state-log record this build cannot read is deferred, never dropped, and every record is stamped with the lowest version that reads it (a gate record above that version stops the applier, so its writer waits on `statelog.Readers`); a parked run's unknown keys ride `PendingRun.Extra`; an apply refuses only a revision that cannot run; and a `coord.ProtocolVersion` bump makes the newer node refuse to claim beside a live lower lease.
- **Once a tag ships it, the bar changes** — the break carries `!` and a `BREAKING CHANGE:` footer naming what an operator must change, the pull request title says so, and while the major is 0 the minor number moves for it. `CONTRIBUTING.md` and `RELEASING.md` carry the whole rule; change the three together.

## Testing
- Run **`make test`** and **`make test-solo`**, never a bare `go test ./...`, which runs the cluster-forming packages beside everything else and bypasses `internal/skipgate`. One package with `go test ./internal/foo/` is a fine fast loop
- Test files sit beside what they cover, a test file that is not Go included: the dashboard's Vitest suites beside their modules in `dashboard/src`, `internal/e2e/replay.mjs` beside the `golden_test.go` that runs it, and `internal/sandbox/codingagent/opencode_steer_probe.mjs` beside the `opencode_steer_test.go` that runs it. There is no top-level `tests/` tree
- **A contract with more than one implementation gets ONE suite** (`queuetest`, `coordtest`, `storetest`, `scheduletest`, `sandboxtest`), which every backend runs, the memory twins included
- **A store on a fresh path starts from the migrated image** — `storetest.OpenNode`, `storetest.OpenEstate`, or `storetest.Seed` before an engine boots on it. Only a test whose subject is migration opens a fresh file with `store.OpenNode`
- **A test broker that reserves a fixed amount declares its size** — `brokertest.StoreIn` for a node's stream, `StoreMaxBytes: brokertest.StoreMaxBytes` for a broker opened directly, `externaltest.Start` for an external one. Left unset, the limit is three quarters of the runner's free disk, so the case passes or fails on the machine. Only a case whose subject is the derived limit states its own
- Every subsystem needs tests, and a test names the invariant it protects, not the function it calls
- Use the fakes in `internal/providers`; tests never call real LLM APIs. A phase that offers tools plays its model as a scripted CLI run over the REAL tool session (`internal/providers/llm/llmtest`), never by calling the dispatcher around it
- **A test that cannot fail is worse than no test** — break what you wrote on purpose and confirm the suite goes red

## Documentation — Keep Docs Updated
Every change to code, config, or behaviour includes its documentation; docs are part of the deliverable.
- A change to a public API, config format, CLI command, data model or behaviour updates the relevant pages in `docs/`; a new feature gets a page or a section; a renamed or removed name is fixed everywhere it appears (grep for it)
- An architecture change updates `docs/concepts/`, and a quickstart-affecting change `docs/getting-started/quickstart.md`. A new page goes in `docs/index.md`, which is also the docs site's navigation
- **Links and anchors are gated** — `internal/docsgate` fails on a relative link to a missing file, an anchor no heading renders, and a `docs/` page the index does not list; `dashboard/src/app/links.test.ts` fails on a printed dashboard address that resolves to Not Found. Renaming a heading renames every anchor to it
- **`docs/` is product documentation**, for people running Crewlet. Process for people with commit rights on this repository is a root-level file beside `CONTRIBUTING.md`
- **Diagrams are Mermaid**, never ASCII box art; directory trees and literal command output stay plain code fences
- If in doubt, update them: stale docs are worse than none, and a one-line config rename still needs its docs

## crewlet-operator — Update It in the Same Change
`crewlet/crewlet-operator` (public, MIT) is the Kubernetes operator that runs Crewlet companies from `crewlet.ai` custom resources. It CONSUMES this engine's contract — it renders both config tiers, calls the API, copies a few run-time decisions out of the source and runs the image — and its suites run this repository's `main` as `ghcr.io/crewlet/crewlet:main` publishes it, never a pin (its ADR-0002). An engine change it has not followed turns its next run red, so when a change here needs support there, that support is part of the change. (`crewlet-operator` always names that repository — never a person, and never `internal/api/operator`.)

What needs one — anything it renders, calls, copies, runs or links:
- **A document it renders** — a field, enum value, default, rule or refusal of Tier B or Tier A, and so of `schema/`
- **A credential position** — a field tagged `secret:"true"`, each mapped to a reference field on one of its kinds (a test holds the two lists to each other)
- **A route or an answer it calls** — `/health`, `/ready`, `/fleet`, `/agents`, `/config`, `/secrets` and the pause tools of `/operator/mcp`: the path, method, status, error envelope, the fields it reads, the paths a validation error names, who may call it
- **A decision it copies** — the engine functions its `internal/render/mirror_test.go` fingerprints by file and name, and the CLI profiles (`cliprofile`)
- **How the engine runs in a pod** — `crewlet run -config`, the `CREWLET_*` variables, the ports, the probes, the drain and its grace, and where the engine writes: it runs non-root on a read-only root filesystem with only a data volume, `/tmp` and the home directory writable
- **The image and the tag** — the `Dockerfile`, `.goreleaser.yaml`, `make image-context` and `main-image.yml` (its path and name, which its release verifies the signature against), and any `v*` tag
- **A page or heading it links** — `https://github.com/crewlet/crewlet/blob/main/docs/<page>.md#<heading>`; its docs gate fails when one goes

Rules:
- **Look before you commit, in its tree rather than your memory** — grep it for what you are changing and for the engine file you are editing. No hit is the answer
- **Make the change there; a note is not the change** — on a branch of it, by its own conventions (its `CLAUDE.md`, `make check`, `git commit -s`). If it cannot be reached, say exactly what it needs in your summary and the pull request
- **Prove it against this change, not against `main`** — `CREWLET_VERSION` for a commit, `CREWLET_IMG` for an image built with `make image-context` (its `CONTRIBUTING.md`, "The engine suite"). Say what you could not run
- **Deliver on this change's terms** — push and open a pull request there only when you do here, each naming the other, and land the pair back to back, this one first
- **Follow `main` plainly** — its API is `v1alpha1`: the companion change is a rename or a reshape, with no alias, both-spellings reader or engine-version switch, unless it has shipped a tag carrying the surface
- **Say what you did, or why nothing was needed** — the summary and the pull request description link the companion change, or say in one line why there is none (`crewlet-operator: none — internal to the turn loop`). Most changes need none and must not get one

`CONTRIBUTING.md` ("Changes `crewlet-operator` has to follow") states the same rule for contributors; change both together.

## Architecture Reference
The implementation follows `docs/concepts/`. Its subsystems:
1. **Event Queue** — durable pub/sub behind one contract: an embedded NATS JetStream by default, clustered members or an external cluster for a fleet, an in-memory twin for tests
2. **Organization Model** — the hierarchy is the execution graph: `roles:` and `units:` in the company document, applied with every revision; a seat's handle and a unit's key are its identity, minted on every write and never changed in place
3. **Agent Runtime** — queue-driven seats; a two-stage turn whose executor is the seat's coding CLI's own run in a box, detached (the turn parks at launch and the same turn resumes on the run's collection, for the engine's delivery check and the reviewer); and a tool loop that runs every other phase as CLI runs of the coding CLI's own loop and decides every call the CLI makes over MCP
4. **Worklet**: the engine's own work tracker, `statelog`'s first domain; a company on an external PM tool sets `tracker.backend` to it and the engine mirrors nothing. Hand-offs are bounded on the item (`reassignments`), not by the delegation depth cap
5. **Decision Framework** — DACI stays behavioural guidance; what the engine records is a structured ask on a work item (`adr/0023`), enforcing only its shape, that a choice names one of its options, and a promised `inform`
6. **Knowledge System**: Doclet, the company's pages (`statelog`'s third domain), searched by keyword (BM25 over the engine's own index; Turso has no fts5), semantically (two-stage 1-bit retrieval: a replicated inverted file over sign codes, then an exact rerank) or `hybrid` by default (the two fused by reciprocal rank fusion at k = 60). Embeddings are a domain computed once by a fleet duty. Confluence is the other backend, one per company. Plus each agent's diary
7. **Communication** — external chat (Mattermost, Slack) and ephemeral A2A channels; the eight surfaces are `integration.Kinds`
8. **Notification Service** — a queue-based spine, the vendors on top
9. **Provider Layer** — every model a coding CLI (one login or key per entry, a spent plan sitting its entry out), handed a phase's tools as one MCP server and read back through its own record of each request, behind a fallback chain: per call on the engine host, continuing from what the phase already did, and per launch for a seat's executor, which takes the chain's first Claude Code or OpenCode member not sitting out; embeddings on the OpenAI SDK
10. **Store** — each node's own embedded Turso files: its NODE file and, on a data node, the whole REPLICATED estate beside it (two files, because a snapshot copies one); a node without `data` keeps a scratch file. Spend and turn history is the replicated `usage` domain, kept 181 days (ADR-0020); turn detail stays 30 days in each data node's event log and is read from every node at query time (ADR-0021)
11. **Coordination** — TTL leases with a fencing epoch, plus the fleet's shared counters, ledgers and sealed credentials; a token budget is counted per calendar window and never reset by hand (ADR-0019)
12. **Seat Ownership** — which node runs which seat, converging without a coordinator
13. **Tool Registry** — builtins, MCP tools and A2A tools, each recording its origin
14. **Tool Skills** — knowledge-base-sourced prompt fragments injected per phase
15. **Code Sandbox** — where every agent seat's executor runs: a box in the cell its launching `providers.llm` entry's `run_in` names (`direct` on the engine host, `container`, or `e2b`), else `providers.sandbox.default_run_in`, so `providers.sandbox` is required once a company has an agent seat and a model; the seat takes no other work while its run works, a run can park on a person's question, and a `direct` or `container` box is driven by the node that launched it (ADR-0065)
16. **API + Dashboard** — one wiring, always in the engine's process; the socket carries the live state and its questions, REST carries every write, and the dashboard writes as the principal its session resolves to (`/operator/act`, ADR-0024)
17. **Scheduler** — role- and unit-scoped cron work with at-most-once delivery, catchup and a wall-clock cap
18. **Control Plane** — the config activation pointer and per-node apply status; lag alone never sheds
19. **State Log** — one ordered stream per domain is the write-ahead log, N identical SQL copies are the state, and the checkpoint commits in the same transaction as the rows; one write authority (take one snapshot, decide inside it, publish, let the broker arbitrate), three write outcomes, four read levels
20. **Identity Estate** — people, credentials, invitations and sessions as `statelog`'s fourth domain; uniqueness is arbitrated on one directory subject, never indexed, and a person holds one human seat for as long as they exist (ADR-0032)
21. **Estate Topology** — a node holds the estate (`data`, which `ingress` and `workers` require) or reaches it through one that does, over the estate router, every request signed under the fleet keyring (ADR-0025, ADR-0031); a file is a replicated row naming an object in the fleet's one object store (ADR-0026)
