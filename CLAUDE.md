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
See `docs/index.md` for the full documentation index.

## Tech Stack
- **Go 1.27+** — the module pins its own toolchain; `GOTOOLCHAIN=auto` fetches it rather than failing on a mismatch. Everything is pure Go: `CGO_ENABLED=0` builds every release target, which is what makes the cross-compile matrix a plain `GOOS`/`GOARCH` loop
- **The standard library first.** `net/http` for servers and clients, `log/slog` for logging, `context` for cancellation, `encoding/json`. A dependency has to earn its place against what `std` already does
- **Turso** (`turso.tech/database/tursogo`) — the embedded store, with `modernc.org/sqlite` as the certified fallback driver. Both pure Go, both run the same schema, both certified by the same suite. Turso is pure Go in the sense that matters — no cgo, no C toolchain — but it is not self-contained: its engine ships as a ~20 MB native library embedded in the driver and extracted at runtime into a shared per-user cache, which `internal/store/turso.go` prepares under a lock because upstream writes it non-atomically and PANICS on what a concurrent reader sees
- **Embedded NATS JetStream** (`github.com/nats-io/nats-server`) — the default event stream, running *in the engine's own process*. Apache Pulsar is the external alternative for a fleet
- **`github.com/modelcontextprotocol/go-sdk`** — MCP client and server
- **Official vendor SDKs** where one exists; a thin typed client where one does not (Plane, Mattermost, GitLab)
- **YAML** (`gopkg.in/yaml.v3`) — config parsing
- **`testing`** — the standard library's, with no assertion framework. Table tests where the cases vary, named tests where the reasoning does

## This Is a Public Open-Source Repository
Crewlet is MIT-licensed, developed in the open at `github.com/crewlet/crewlet`, ships as signed binaries and a container image from a `v*` tag, and its `docs/` tree is served at docs.crewlet.ai. Everything committed is published, permanently: git history keeps what a later commit deletes, and a released binary keeps what the working tree drops. Write every change as something a stranger will read, run, and depend on.

Rules:
- **Never commit a secret, and never fix one by deleting it.** No real API keys, tokens, PATs, webhook secrets, private hostnames, internal URLs, or customer/employee names — in code, tests, docs, examples, fixtures, or commit messages. Config examples use `${VAR}` references and placeholder values (`example.com`, `U0FOUNDER`, `sk-ant-...`). If a real credential does land in a commit, it is compromised the moment it is pushed: it must be **rotated**, and removing it in a follow-up commit is not a fix.
- **Commit subjects are semantic** — `type(scope): summary`, imperative, lowercase, no trailing period, ≤72 chars. Type is one of `feat` / `fix` / `docs` / `refactor` / `perf` / `test` / `ci` / `build` / `chore` / `revert`; scope is the *component*, normally the package directory under `internal/` (`agent`, `api`, `coord`, `engine`, `queue`, `sandbox`, `seat`, `store`, …), plus `dashboard` for `static/dashboard/` and `cli` for `cmd/crewlet/`; outside `internal/` it is the area (`docs`, `deps`, `examples`, `schema`, `scripts`, or the workflow name for `ci`). Unrelated fixes and tuning go in their own commit with their own scope. A pull request squash-merges into one commit whose subject is its title, so the title takes the same form. Nothing enforces this and history is permanent — the full rules are in `CONTRIBUTING.md`.
- **The pull request title is the release note.** There is no `CHANGELOG.md`: GitHub generates each Release body from the titles of the pull requests merged since the previous tag (grouped by `.github/release.yml`), and that generated body is the *only* record of what a release contains. So a title for a user-visible change — a new feature, a config or CLI change, a behavior change, a notable fix — must read as release notes for someone running Crewlet, not as a commit log. Nothing gates this at release time; a vague title is permanent once the pull request merges.
- **The root-level files are canonical for process**, and this file must not contradict them: `CONTRIBUTING.md` (dev setup, contributor conventions, dependency surfaces), `RELEASING.md` (the release runbook), `SECURITY.md` (private vulnerability reporting, operator scope notes), `CODE_OF_CONDUCT.md`, `LICENSE`. When a rule here changes, change it there in the same commit — a contributor reading `CONTRIBUTING.md` and an agent reading this file must never get different answers.
- **Follow the repository's own templates.** PRs use `.github/pull_request_template.md` (its checklist is the actual gate); issues use the templates in `.github/ISSUE_TEMPLATE/`.
- **Third-party code must be MIT-compatible and attributed.** Do not paste in code whose license forbids it. Source files carry no license headers — do not start adding them.
- **Security issues never go in a public issue or a public PR description** — see `SECURITY.md`.

## No Band-Aids — Always Do the Proper Fix
Never apply band-aid fixes, workarounds, or partial solutions. Always implement the correct, complete fix — even if it is large, touches many files, or requires significant refactoring. A quick hack that "works for now" is not acceptable; it creates tech debt that compounds and makes the codebase harder to reason about.

**When suggesting a fix, the cost of the fix is never a factor.** We do not care how long the fix takes, how many lines of code it touches, how many files it spans, or how big the change ends up being. Always propose the proper fix — the one that actually solves the problem at the root — regardless of size or effort. Do not downgrade your suggestion to a smaller, cheaper, or faster alternative because the proper fix "feels too big". If the proper fix is a 2000-line refactor across 50 files, suggest that fix. If it requires rewriting a subsystem, suggest that. Never pre-filter your suggestions based on effort, time, or scope.

Rules:
- **Root-cause fixes only** — diagnose the actual problem and fix it at the source. Do not patch symptoms.
- **No workarounds** — do not add special cases, flags, or conditional logic to work around a broken abstraction. Fix the abstraction.
- **Size is not an excuse, ever** — if the proper fix requires changing 20 files, change 20 files. If it requires changing 200 files, change 200 files. If it requires redesigning a subsystem, redesign it. The scope of the fix should match the scope of the problem — nothing less.
- **Time is not a factor** — do not consider how long a fix will take when deciding what to suggest or implement. A fix that takes a week is not worse than one that takes an hour; only the correctness and completeness of the fix matter.
- **Lines of code are not a factor** — do not prefer a smaller diff over a correct one. A one-line hack that papers over the issue is strictly worse than a thousand-line change that solves it properly.
- **Always suggest the proper fix first** — when proposing fixes, lead with the correct, complete one. Do not hide it behind cheaper alternatives or present "quick" options as the default. The user explicitly wants the proper fix no matter the size.
- **No "temporary" solutions** — there is no such thing as a temporary fix in this codebase. Every change should be one you're willing to maintain indefinitely.
- **Refactor when needed** — if implementing a feature correctly requires refactoring existing code first, do the refactoring. Do not bolt new functionality onto a broken foundation.
- **Update everything** — a proper fix includes updating all tests, docs, and dependent code. A fix that breaks tests or leaves stale docs is not done.

When in doubt, ask: "Is this the fix I would make if I had unlimited time and unlimited diff size?" If not, find the fix that is.

## Fix Every Bug You Find — Even Unrelated Ones
While working on a task you WILL sometimes notice bugs that have nothing to do with what you were asked to do. Fix them anyway. A discovered bug is never "out of scope" — relatedness to the current task has no bearing on whether it gets fixed. If it is broken and you found it, you fix it, in the same change. A bug you walked past is a bug you implicitly endorsed.

Rules:
- **See a bug, fix a bug** — whenever you discover a bug — while reading code, building a feature, debugging something else, or reviewing — fix it as part of your current change. Do not leave it for "later", a "follow-up", or someone else.
- **Unrelated is not an excuse** — the bug does not need to relate to your task, the file you are in, or what the user asked for. "That's not what I'm working on" is not a reason to leave a bug in place.
- **Never silently ignore a bug** — you may not knowingly leave a discovered bug unfixed. There is no "I noticed it but skipped it". Silence about a known bug is not allowed.
- **Fix it properly** — apply the same bar as everything else: root-cause fix, no band-aids, no deferred work, with tests and docs updated (see "No Band-Aids" and "No Deferred Work" above). Size, time, and diff length are not factors here either.
- **Surface what you fixed** — call out incidental bug fixes in your summary and commit message so they are visible and reviewable. Where it keeps history clean, put unrelated fixes in their own commit.
- **If a fix genuinely can't fit in this change** — e.g. it needs its own coordinated effort or a risky redesign — do not quietly drop it. Fix everything you safely can now and explicitly raise the remainder with the user; never let a known bug pass unmentioned (same rule as "No silent narrowing of scope" below).

When in doubt, ask: "Did I notice anything broken that I'm about to leave broken?" If yes, fix it.

## Tuning Knobs Are Findings Too — Don't Defer Them Either
This extends "Fix Every Bug You Find — Even Unrelated Ones" to the findings developers love to wave away. While working you WILL notice values that aren't crashes or wrong output but are plainly suboptimal: a hardcoded magic number, an arbitrary timeout, retry count, backoff constant, batch size, poll interval, cache TTL, concurrency cap, page size, buffer size, rate limit, log level, feature-flag default, temperature or model choice, a value that should be configurable but is baked in, or a knob exposed to callers that should just be a sane constant. The reflex is to label it "just a tuning knob, not a bug" and move on. That label is not a free pass — it is the exact moment the work gets dropped. A wrong knob is a slow-motion bug; it just waits for production load to detonate — a 1-second timeout that flaps under load, a retry count that hammers a dying dependency, a temperature that makes a judge non-deterministic. The only honest difference from a crash is that the right value is often a judgment call, and a judgment call is something you make, not something you skip. A knob you shrugged at is a default you shipped.

Rules:
- **A wrong knob is a finding, not a footnote** — "it's a tuning knob, not a bug" does not downgrade it, defer it, or excuse leaving it. Relatedness has no bearing either (see "Fix Every Bug You Find" above): if you read past a `batch_size=1` that should be batched, you found it, so you address it in the same change, to the same bar as a real fix — root-cause, no band-aids (see "No Band-Aids" above).
- **Tune to the justified value now** — pick the value you can defend from the code, the data, and the surrounding behavior, and set it. Do not leave a placeholder, a vague comment, or a stubbed-out "configurable later" hook (see "No Deferred Work" below).
- **Expose or collapse, deliberately** — a hardcoded value callers genuinely need to vary becomes real config; a knob with no honest reason to differ becomes a plain constant with the chosen value. Decide which direction is correct and do it — do not leave a half-configurable mess.
- **Tie the value to something real** — record why this value, not just what, where the next reader will see it (the config doc, a comment at the definition, or the commit message). Anchor it to the dependency's p99, the page-size limit, the SLA, the memory budget — so the next reader inherits the rationale, not a fresh mystery.
- **Surface what you tuned** — call out every knob you changed, exposed, or collapsed in your summary and commit message, with the old and new value and the reason, exactly as you would an incidental bug fix (see "Surface what you fixed" under "Fix Every Bug You Find"). Where it keeps history clean, put unrelated tuning in its own commit.
- **If the right value is genuinely a judgment call you can't settle here** — when the correct setting truly needs production data, a benchmark you can't run, or a product decision (a concurrency cap trading cost against latency, a cache TTL trading freshness against load), do not silently leave the bad value. Make your best-justified call, mark it clearly, and explicitly raise it with the user with a concrete recommendation and the tradeoff — never let a known-suspect knob pass unmentioned (same rule as "If a fix genuinely can't fit in this change" under "Fix Every Bug You Find").

When in doubt, ask: "Am I calling this 'just a knob' so I don't have to decide its value?" If yes, decide it — or escalate it with a concrete recommendation — but never walk past it.

## No Deferred Work — Always Deliver the Clean Implementation
Never defer work. Do not push problems forward with TODOs, "follow-up" tickets, stub functions, or "we'll fix this later" comments. When you start a task, you finish it — completely, cleanly, and in the same change. Long, hard, or sprawling tasks are not an excuse to ship something partial; they are a reason to commit to the full implementation.

Rules:
- **Decide and deliver** — when faced with a choice, make the call and implement it end-to-end. Do not stop halfway, hedge with multiple half-built paths, or leave the decision for "later".
- **No TODOs, no stubs, no placeholders** — do not commit `TODO`, `FIXME`, `XXX`, `pass  # implement later`, `raise NotImplementedError`, or empty function bodies pretending to be done. If it's in the diff, it's finished.
- **No "follow-up" tickets for the current scope** — if it is part of the task you accepted, it ships in this change. Do not split a coherent piece of work into a "first pass" plus a backlog item.
- **Long tasks still get finished** — if the task is large, work through it. Do not stop at "good enough for now" and call it done. The size of the task is not a license to defer parts of it.
- **No "phase 1 / phase 2" hand-waving** — do not invent artificial phases to justify shipping less than the task requires. If phasing is genuinely necessary (e.g. a real migration with data dependencies), confirm it explicitly with the user before splitting.
- **No silent narrowing of scope** — if you decide the task is bigger than expected, do not quietly shrink what you deliver. Either deliver the full scope or raise it with the user before cutting.
- **Tests, docs, migrations all included** — "delivered" means the implementation, its tests, its docs, and any required migrations or config updates are all in the change. A feature without tests or docs is not delivered.

When in doubt, ask: "If I stopped right now, would a reader of this diff consider the task done?" If not, keep going until the answer is yes.


## Code Conventions
- **Interfaces are defined by the consumer**, in the package that calls them, and kept to what that caller needs. A provider package exports a concrete type; the package that uses it declares the two-method interface it actually calls. There is no `interfaces.go`
- **`context.Context` first, always**, and threaded rather than stored in a struct. The one exception is a rollback or a teardown, which takes `context.WithoutCancel`: the failure it is undoing is often the cancellation itself, and a cleanup that inherits a dead context does nothing at all
- **Accept interfaces, return structs**
- **Zero values must be meaningful, or the type must refuse them.** A `pause_ttl_seconds` of 0 read as "never pause" and tore down the checkout of every seat that said nothing about the knob — a config field whose zero value is a valid *setting* needs a pointer or an explicit `Unset` sentinel, not a comment
- **Errors wrap with `%w`** and say what to do about it. An error a person will read names the field, the file or the variable they have to change — not just what failed. Sentinels are `errors.Is`-comparable; typed errors carry the identifier the caller needs
- **A three-valued answer is `(value, error)`**, never a bool. "Held", "definitively not held" and "the store could not be reached" are three different facts, and collapsing the last two into `false` is the single most incident-hardened lesson carried into this engine — see `internal/coord`
- **IDs are `uuid.UUID`**, timestamps are `time.Time` in UTC, durations are `time.Duration`. A config field that is a number of seconds is named `…Seconds` and converted once, at the edge
- **Enums are a named string type** with typed constants and a `Valid()` method, so an unknown value off the wire is a value rather than a panic
- **Concurrency**: a goroutine's lifetime belongs to whoever started it, and every one has a way to stop. Prefer channels for handoff and a mutex for state; `sync.OnceValue` over a `sync.Once` plus a variable. Anything shared runs under `-race` in CI
- **Comments explain WHY**, and especially why the obvious alternative is wrong. The diff shows what the code does. A package's rationale goes in its package doc, where `go doc` will find it

## Logging
All logging goes through `internal/logging`, which builds a `*slog.Logger` bound to its component. Never the bare `slog` default, and never `fmt.Sprintf` into a message.

```go
import "github.com/crewlet/crewlet/internal/logging"

log := logging.Get("task.engine")

log.Info("task_created", "task_id", task.ID, "creator", creator)
log.Debug("resolving_hierarchy", "agent_id", agent.ID, "role", role)
log.Warn("budget_exhausted", "agent_id", id, "used", used, "limit", limit)
log.Error("mcp_server_failed", "server", name, "error", err)
```

Rules:
- Get loggers via `logging.Get("component.name")` — this binds `component=` automatically
- Event names are short, machine-parsable snake_case strings (not sentences)
- All dynamic data goes in key/value pairs, never in the message
- Never `slog.Info(...)` on the package-level default, and never `log.Printf`
- The process configures itself once via `logging.Configure(level, format, w)` in `cmd/crewlet` — and `Configure` is the ONLY way to set the destination. A command changes how loud it is with `logging.SetVerbosity(level, format)`, which keeps the sink already installed. Installing a writer a function was HANDED makes the global depend on its caller: `run` takes `stderr` so it can be tested, and installing that argument gave 29 parallel tests a global pointing at whichever buffer was configured last — a data race, and one test's log lines in another's output

## Package Layout

```
cmd/crewlet/          # The one binary. run / validate / schema / migrate /
                      #   budgets / secrets / config, and the seven vendor CLIs
                      #   (gitlab, github, plane, jira, slack, confluence,
                      #   mattermost).
                      #   Every command the switch dispatches must appear in
                      #   usage() — nothing connects them, and a test asserts it
internal/             # Everything else. No package here is importable from
                      #   outside the module, which is deliberate: the engine
                      #   has no stable Go API to keep, only a CLI, a config
                      #   format and a wire protocol
static/               # Embedded assets — static/dashboard/ is the zero-build
                      #   ES-module dashboard, served by internal/api and
                      #   compiled into the binary via embed
tests/dashboard/js/   # The dashboard's own suites. JavaScript, so they live
                      #   outside the Go tree and run under plain `node`,
                      #   driven from internal/api. No package.json, no runner
examples/             # The Nimbus example company — a working two-tier config
docs/                 # Product documentation, published to docs.crewlet.ai
schema/               # GENERATED JSON Schema for both config tiers. Emitted by
                      #   `crewlet schema <tier>`; a test regenerates and
                      #   compares, so never hand-edit — change the Go models
decisions/            # The design record. Why the engine is shaped the way
                      #   it is, and what the obvious alternative cost. Cited
                      #   from the code it governs. See decisions/README.md
skills/               # FOUNDER-facing authoring skills for an AI assistant
scripts/              # The three vendor dev-loop bootstraps (bash)
```

### The packages, and the one thing each is for

**Every package states its own rationale in its package doc.** `go doc ./internal/coord` is the authority on coordination, not this file — what follows is a map, so you know which doc to read. Where a decision would be tempting to "fix" back to the obvious shape, it is written up under `decisions/`.

*The spine — what everything else is built on:*

| Package | What it is for |
|---|---|
| `queue` | THE EventQueue contract. One interface, three backends (`memory`, `jetstream`, `pulsar`), all certified by ONE suite in `queuetest`. Nothing above this package may branch on which backend is running. A durable subscription IS a seat's mailbox: it exists without a consumer and retains what is published while nothing is attached. Handlers have three outcomes — ack, nak, and *defer*. Attachment has four verbs of differing destructiveness, because `unsubscribe` never said which one it was |
| `coord` | Cross-process ownership: TTL leases with a monotonic fencing epoch, plus everything else a fleet has to agree on (`fleet.go`) — the activation pointer (a COMPARE-AND-SET: there is no leader, so the flip is what stops two operators overwriting each other) and per-node apply status, the completion ledger, the delivery dedupe, the notification valve, the credential cooldowns, the token counter and the company's SECRETS. Every "do I hold this?" is three-valued — held / definitively not / **unknown** — and treating unknown as loss tears a healthy company down over a two-second store blip. Retention here is a BUCKET's age, never a per-write TTL |
| `store` | The node's LOCAL database: the audit event log, learning memory, durable turn state, and the bootstrap half of the secret store that `fleetsecrets` migrates off at boot. **One file, one process** — exclusively owned, so nothing here has to be safe against a peer, and the migrator's advisory-lock idiom simply disappears. Anything the COMPANY has to agree on belongs in `coord` instead, and migrations 0010–0011 are what that rule cost when it was broken. Two certified drivers, one dialect: every statement must parse on both |
| `events` | The envelope and the typed-payload registry. Two load-bearing properties: evolution is additive-only, and an event type this build does not know decodes, round-trips and re-publishes losslessly — a rolling upgrade puts unknown types on the wire, and dropping them would make every upgrade an outage |
| `config` | The two tiers. Tier A (`Bootstrap`) is ops-owned, on disk, restart to change. Tier B (`Company`) is founder-owned, versioned in the store, edited live. Tier A is the root of trust and therefore resolves with `EnvOnly` — it holds the keys to the secret store, so it can never read a value out of it. Tier B's secrets are `${VAR}` POINTERS, stored verbatim, resolved only where a provider or transport is constructed |
| `seat` | Which seats this node runs. The placement policy is deliberately dumb — greedy claim to `ceil(seats / live nodes)`, converging in BOTH directions, `preferred` orders the attempt and never gates it. `watchdog` is the other half: a wedged event loop cannot be signalled, so the thread's only honest move is to exit |
| `engine` | The ENTANGLEMENT POINT — which concrete thing satisfies which seam. Deliberately many small files rather than one large one; the two hardest passages (the inbox handler, the config apply) are packages of their own. `mcp.go` is where `mcp_servers:` becomes running children: shared servers on the epoch, per-role children on a seat's LEASE, each claimed seat with its own bridge and its own registry — two children of one template publish the same tool names, so anything shared between seats hands one seat's identity to another |

*The agent runtime:*

| Package | What it is for |
|---|---|
| `agent/turn` | The turn loop — Onboarding, Plan, Execute, Review — and the guards between phases |
| `agent/toolloop` | The model↔tool round-trip, and the `Suspend` primitive a detached coding run returns through |
| `agent/inbox` | What wakes a seat, and what it must not be woken by twice. `ledgered` is the completion side |
| `agent/ledger` | The prior-work ledgers: `iteration` (within a turn), `conversation` (across turns in one thread), `budgets`. Structured, never a transcript replay — the thread has moved by the next turn |
| `agent/prefetch` | The Plan-phase context assembly: relevant knowledge, memory, the blocks a prompt gets |
| `agent/prompts` | The prompt text. Tuned against observed model behaviour; a reworded section is a behaviour change |
| `agent/skills` | Prompt fragments admitted from the knowledge base and injected per phase, plus the load-before-use guard |
| `agent/builtin` | The tools the engine itself ships |
| `tools` | The registry, holding four kinds of tool under one contract and recording WHICH at registration — the only frame that knows, and the last that can say |
| `mcp` | MCP client and child supervision. A stdio server is a process TREE, not a process. See `decisions/602` |
| `providers/llm` | The model contract. It does NOT retry (rotation belongs to the credential pool, fallback to the chain) and does NOT decide what a failure means beyond a coarse kind |
| `sandbox` | Code work as a suspended Execute phase. A coding run is DETACHED: the tool starts it, the loop suspends, and the engine resumes that same loop — minutes later, possibly after a restart, possibly on another node. Nothing parks a goroutine on a running job. Two real backends behind one contract — a remote VM per run (E2B, cloud or self-hosted) and the engine host itself — and the closed set `providers.sandbox.type` accepts is asserted against the switch that builds them |
| `learning` | What a seat remembers. Everything here is BEST EFFORT by design: a failed write is logged, a failed read answers empty |
| `schedule` | Role- and unit-scoped cron work, with at-most-once delivery, missed-tick catchup and a wall-clock cap |

*The edges:*

| Package | What it is for |
|---|---|
| `api` | REST + the dashboard. ONE wiring for embedded and standalone; what differs is only what the node can SEE, and that is one seam (`NodeRuntime`). Auth guards every route bar probes, webhooks (HMAC), OTLP (signed token) and the dashboard shell |
| `api/webhooks` | The seven inbound vendor routes. A route whose secret is unset has nothing to verify with and answers 503 — it does not accept the delivery |
| `api/secretsapi` | `/secrets`: the company's credentials, written through a running node because the coordination broker is inside its process and listens on no socket. Always guarded, reads included; the ONE route that returns a value needs an explicit `?reveal=true` and logs the access |
| `api/stream` | The `/ws/stream` socket: pushes, plus a request/response query channel that is a thin adapter over the SAME function each REST route calls |
| `observe` | The observability edge, and the two routes are deliberately different: the STORE is written by a publish listener inline on the publishing node (no consumer group, so no two nodes can write one row); the PROJECTION is fed by an ephemeral per-caller broadcast |
| `notify` | The backend-neutral notification spine — conversation keys, digest coalescing, party resolution, the rate valve. Built before any vendor sat on it, because a spine built after its first vendor has that vendor welded into it |
| `mattermost`, `slack`, `plane`, `gitlab`, `github`, `jira`, `confluence` | The vendors. Each contributes only what is genuinely its own: a client, a parser, a transport, a prompt, a provisioning reconcile — and no more, which is why Jira has no transport (an agent writes through its own MCP tools) and why its reconcile and GitHub's report rather than mint (neither vendor issues a credential on a provisioner's behalf) |
| `fleetsecrets` | The company's credential store: this package owns the KEY, coordination owns the BYTES. Also the one-way migration off `store`'s own table, which copies before it deletes and never overwrites a name the fleet already holds |
| `secrets`, `provision`, `envref`, `envfile`, `redact`, `workkey` | The small shared grammars. Each imports nothing from the rest of the engine, which is what lets `config` itself depend on them |

## Pre-commit Checks
Before committing, ALWAYS run and fix any issues from:
- `gofmt -l .` — prints the files that need formatting; the output must be empty
- `go vet ./...`
- `golangci-lint run` — what CI's lint job runs
- `go test ./...`

These are the same checks CI runs. Anything touching concurrency — the seat host, the queue, the turn engine — also gets `go test ./... -race`, which CI runs on everything: the engine's concurrency model is real parallelism, so every "atomic because it is single-threaded" assumption is a data race until proven otherwise.

**A skip is not a pass.** Three suites need something the machine may not have and skip silently without it: the dashboard's suites need `node`, `internal/store` runs twice over `CREWLET_STORE_DRIVER=turso|sqlite`, and the Pulsar conformance suite needs `CREWLET_TEST_PULSAR_URL`. A green local run has not necessarily exercised any of them. CI fails rather than skips where it can.

## Dependency Updates
Dependabot watches every dependency surface in the repo — `.github/workflows/*.yml` (`github-actions`), `go.mod` (`gomod`), `Dockerfile` (`docker`), and `docker-compose.yml` (`docker-compose`). The config is `.github/dependabot.yml`: one entry per surface on a weekly schedule, plus the commit prefix that surface's bumps carry, and nothing else; keep it that way unless a knob earns its place. `docker` and `docker-compose` are separate ecosystems reading separate manifests — a repository with both needs both entries.

- **A new dependency surface ships with its `updates:` entry.** Adding the first `Dockerfile`, a `package.json`, a `go.mod` — the Dependabot entry is part of that change, not a follow-up. Nothing fails when it is missing; the surface is just never watched, which looks exactly like a surface with nothing to update.
- **Actions pinned to a non-version ref are invisible to Dependabot.** A branch pointer (`@release/v1`) or `@main` yields no update PRs at all, so pin actions to a version tag or a full SHA. Maintainer-facing detail is in `CONTRIBUTING.md`.
- **A bump merges itself.** `.github/workflows/dependabot-merge.yml` approves a Dependabot PR and queues it with `gh pr merge --auto --squash`, so it lands on green CI with no maintainer in the loop. Two things outside the tree hold that up, and neither is visible from a checkout: `main`'s protection rule must REQUIRE the `ci` checks — `--auto` waits for the checks a rule names and for nothing else, so with no rule the bump merges the instant it is mergeable — and "Allow auto-merge" must be on in the repository settings, or the step fails and paints the PR red. The job runs only when Dependabot is BOTH the PR's author and the actor that triggered the run; the actor half is what stops a commit a PERSON pushed onto a Dependabot branch from collecting the repository's own approval.
- **Nobody retitles a bump before it lands**, so each entry pins its `commit-message.prefix`, and every surface carries the same one: `build(deps)`. A bump moves a pinned version, which is a change to what the project builds against whichever file records it, and it is what the bumps already in `main` carry. Unset, Dependabot INFERS the prefix from recent history, and an inference that changes its mind writes a bare `Bump x from 1 to 2` straight into `main` and into the release notes. Dependabot capitalises the "Bump" and offers no way not to, so a bump is the one subject here that does not start lowercase.
- **The auto-merge guard is asserted, not remembered** — `internal/version` checks both halves of the author/actor condition and the `--auto --squash` flags. Each of them fails silently: the workflow keeps running, it just runs on the wrong PRs or merges before a check has reported.


## Releases
The engine ships as signed binaries and a container image, built by goreleaser from a `v*` tag. See `RELEASING.md`.

- **The tag is the version.** Nothing in the tree records one, so there is nothing for a tag to disagree with and no version bump to make. goreleaser stamps the tag into `internal/version.value` at link time; a binary built any other way reports its module build info instead, so it names itself honestly rather than claiming to be a release. **Never add a literal version constant back.**
- **Version numbers follow semver** — the minor number moves for features, the patch number for fixes.
- **Pushing the tag is the whole release** — the workflow builds six targets, signs the checksums with keyless Sigstore, pushes the image to GHCR and creates the GitHub Release with notes GitHub generates from the merged pull requests. Those generated notes are the only description a release gets, so pull request titles carry it. A pre-release takes neither GitHub's "Latest" nor the `latest` image tag.
- **The release surface is asserted, not remembered** — `internal/version` guards the ldflags stamp, the pure-Go build, the `${TARGETPLATFORM}` copy, the catch-all notes category, the pre-release flags and the single tag trigger. Each of those fails silently in production and has no other symptom.

## Testing
- Run tests: `go test ./...`
- Test files sit beside what they cover, as Go expects: `internal/queue/queue.go` → `internal/queue/queue_test.go`
- **A contract with more than one implementation gets ONE suite.** `queuetest`, `coordtest`, `storetest`, `scheduletest` and `sandboxtest` export the suite; each backend runs it. A twin that agrees only with itself proves nothing, which is why the memory twins are certified by the same cases as the real backends
- Every subsystem needs tests, and a test names the invariant it protects rather than the function it calls
- Use the fakes in `internal/providers` — tests never call real LLM APIs
- **A test that cannot fail is worse than no test.** Mutate what you wrote: break the thing on purpose and confirm the suite goes red. A guard nothing catches is a claim, not coverage

## Documentation — Keep Docs Updated
Every change to code, config, or behavior MUST include corresponding documentation updates. Docs are NOT optional — they are part of the deliverable.

Rules:
- **Any code change that affects public APIs, config formats, CLI commands, data models, or system behavior** must update the relevant docs in `docs/`
- **New features** require a new or updated doc page explaining the feature, its usage, and any config options
- **Renamed or removed APIs/config fields** must be updated everywhere in docs — grep for old names and fix all references
- **New modules or packages** must be added to the Package Layout section above and to `docs/index.md`
- **Architecture changes** must update the relevant `docs/concepts/` page
- **Quickstart-affecting changes** must update `docs/getting-started/quickstart.md`
- **Always check** `docs/index.md` to see if new pages need to be added or existing entries need updating — it is also the docs site's navigation source, so an unlinked page fails the site build
- **`docs/` is the product documentation, published to docs.crewlet.ai** — it is written for people running Crewlet: how the engine works, how to configure, deploy, and integrate it. Project process aimed at people with commit rights on *this* repository is NOT product documentation and does not go there; it lives as a root-level markdown file beside `CONTRIBUTING.md` / `SECURITY.md` / `RELEASING.md`. Ask who the reader is: a founder running an agent company, or a Crewlet maintainer?
- **Diagrams are Mermaid** — architecture, flow, sequence, and state diagrams go in ` ```mermaid ` fences, never ASCII box art. GitHub renders them inline and the docs site themes them with the Crewlet design system. Directory trees and literal command output stay plain code fences.
- If in doubt about whether docs need updating, update them — stale docs are worse than no docs

This applies to every commit, not just "big" changes. A one-line config rename still needs a docs update.


## Architecture Reference
The implementation must follow the architecture docs in `docs/concepts/`. Key subsystems:
1. **Event Queue** — durable pub/sub behind one contract: an embedded NATS JetStream by default, an external NATS or Apache Pulsar for a fleet, an in-memory twin for tests
2. **Organization Model** — hierarchy is the execution graph
3. **Agent Runtime** — queue-driven seats, a four-phase turn, an LLM tool loop that can suspend
4. **Task Engine** — there is none: task state lives in the PM tool, and the engine mirrors nothing
5. **Decision Framework** — DACI behavioral guidance (via chat channels, no dedicated engine)
6. **Knowledge System** — query-time knowledge-base search for shared docs (Plane page search, or Confluence CQL — single-homed, one per company) + per-agent diary
7. **Communication** — external chat (Mattermost, Slack) + ephemeral A2A channels. The seven vendors this build serves are Mattermost, Slack, Plane, GitLab, GitHub, Jira and Confluence (`decisions/701`, `decisions/703`); every one routes end to end, and no integration block is refused any more
8. **Notification Service** — queue-based spine, vendors on top
9. **Provider Layer** — pluggable LLM and embeddings, with a credential pool and a fallback chain around them
10. **Store** — the node's own embedded database (Turso, or mainline SQLite); coordination lives in the KV layer instead, never here
11. **Coordination** — TTL leases with a fencing epoch, plus the fleet's shared counters, ledgers and the company's sealed credentials
12. **Seat Ownership** — which node runs which seat, and how a fleet converges without a coordinator
13. **Tool Registry** — builtins + MCP tools + A2A tools, each recording its origin
14. **Tool Skills** — knowledge-base-sourced prompt fragments injected per phase
15. **Code Sandbox** — a per-role coding-agent Execute backend; a run is detached and resumes a suspended loop. E2B for a remote VM, the engine host for a local one
16. **API + Dashboard** — one wiring, embedded or standalone; the websocket is the dashboard's only data channel
17. **Scheduler** — role/unit-scoped cron work with at-most-once delivery, catchup and a wall-clock cap
18. **Control Plane** — the config activation pointer and per-node apply status; lag alone never sheds
