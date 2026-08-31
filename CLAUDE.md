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
- **Go 1.27+** — the module pins its own toolchain; `GOTOOLCHAIN=auto` fetches it rather than failing on a mismatch. Everything is pure Go: `CGO_ENABLED=0` builds every release target, which is what makes the cross-compile matrix a plain `GOOS`/`GOARCH` loop — over the four platforms the store driver has a database engine for. It does NOT make the linux artifact static; see the Turso row below
- **The standard library first.** `net/http` for servers and clients, `log/slog` for logging, `context` for cancellation, `encoding/json`. A dependency has to earn its place against what `std` already does
- **Turso** (`turso.tech/database/tursogo`) — the embedded store, and the ONLY driver. The `modernc.org/sqlite` fallback is retired, along with the `store.driver` field and `CREWLET_STORE_DRIVER`, so there is no dialect intersection to write inside — use what Turso has. It is pure Go in the sense that matters — no cgo, no C toolchain — but it is not self-contained: its engine ships as a ~20 MB native library embedded in the driver and extracted at runtime into a shared per-user cache, which `internal/store/turso.go` prepares under a lock because upstream writes it non-atomically and PANICS on what a concurrent reader sees. That library is also what bounds the release matrix — upstream embeds it for linux and darwin on amd64/arm64 and windows/amd64 only, which is why there is no Windows target and why `internal/store/platform.go` turns an unsupported build into a compile error. And it is why the linux binary is NOT static despite `CGO_ENABLED=0`: purego declares its `dlopen` imports with `//go:cgo_import_dynamic`, so the artifact is dynamically linked against glibc and does not run on musl or `scratch`. Do NOT "fix" that with `-extldflags -static` — a static program cannot `dlopen`, and that build segfaults on its first query (measured; ci.yml's `cross` job asserts the artifact stays dynamic). `-tags musl` picks the embedded `.so`, not the binary's own linkage, so it does not produce an Alpine build either — there is no musl archive
- **NATS JetStream** (`github.com/nats-io/nats-server`) — the event stream, and the ONLY broker. Embedded by default, running *in the engine's own process* with no listener and no service to operate; a fleet either clusters those embedded members (`stream.cluster`) or dials an external NATS cluster (`stream.type: nats`). The Apache Pulsar backend is retired, along with `stream.type: pulsar`, its `tenant`/`namespace`/`admin_url`/`tls_trust_certs` fields and the whole `coordination.nats` estate block — the coordination KV rides the stream's own connection on every topology now, which is precisely what Pulsar could never do (it has no compare-and-set, so every Pulsar deployment ran a second NATS estate anyway)
- **`github.com/modelcontextprotocol/go-sdk`** — MCP client and server
- **OpenTelemetry** (`go.opentelemetry.io/otel` + the OTLP trace exporters) — tracing, and the one dependency here that `std` genuinely cannot supply: W3C trace context, a sampler, and a wire format three collectors already speak. Configured from the standard `OTEL_*` environment rather than Tier A, so an operator pastes their collector's own snippet. The provider is always installed and only the exporter is conditional, because a trace id is a column in the event store before it is ever an exporter's concern
- **Official vendor SDKs** where one exists; a thin typed client where one does not (Mattermost, GitLab, Confluence)
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
- **No workarounds** — do not add special cases, flags, or conditional logic to work around a broken abstraction. Fix the abstraction. A compatibility shim — an alias, a both-spellings fallback, an adapter — is that same move under a respectable name, and for a surface no release ever shipped there is nothing on the other side of it to be compatible with (see "Breaking Changes Are Free Until a Tag Ships Them" below).
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

## Package Layout

```
cmd/crewlet/          # The one binary. run / validate / schema / migrate /
                      #   budgets / backup / secrets / config / llm, and the six vendor CLIs
                      #   (gitlab, github, jira, slack, confluence,
                      #   mattermost).
                      #   Every command the switch dispatches must appear in
                      #   usage() — nothing connects them, and a test asserts it
internal/             # Everything else. No package here is importable from
                      #   outside the module, which is deliberate: the engine
                      #   has no stable Go API to keep, only a CLI, a config
                      #   format and a wire protocol
dashboard/            # The dashboard's SOURCE: React 19 + TypeScript, built by
                      #   Vite. `make dashboard` builds it into static/dashboard
                      #   and THAT IS COMMITTED — `go build ./...` and
                      #   `go install …@latest` must work with no Node on the
                      #   machine, and an embed directive cannot run a bundler.
                      #   CI rebuilds and diffs the tree (`make dashboard-check`),
                      #   the same idiom as `go mod tidy -diff`
static/               # Embedded assets — static/dashboard/ is that build
                      #   output, served by internal/api and compiled into the
                      #   binary via embed. NEVER hand-edited
tests/dashboard/js/   # The e2e client replay only: internal/e2e captures a real
                      #   company's socket frames and pushes them through the
                      #   dashboard's own protocol module (the second build
                      #   target, static/dashboard/protocol.js) under plain
                      #   `node`. The dashboard's OWN suites are Vitest, in
                      #   dashboard/src, run by `make dashboard-test`
examples/             # The Nimbus example company — a working two-tier config
docs/                 # Product documentation, published to docs.crewlet.ai
schema/               # GENERATED JSON Schema for both config tiers. Emitted by
                      #   `crewlet schema <tier>`; a test regenerates and
                      #   compares, so never hand-edit — change the Go models
skills/               # FOUNDER-facing authoring skills for an AI assistant
scripts/              # The two vendor dev-loop bootstraps (bash)
```

### The packages, and the one thing each is for

**Every package states its own rationale in its package doc.** `go doc ./internal/coord` is the authority on coordination, not this file — what follows is a map, so you know which doc to read. Where a decision would be tempting to "fix" back to the obvious shape, the package doc is where that is written down, together with what the obvious alternative cost when it was tried. None of it binds: change the code and rewrite the comment in the same commit.

*The spine — what everything else is built on:*

| Package | What it is for |
|---|---|
| `queue` | THE EventQueue contract. One interface, two backends (`memory`, `jetstream`), both certified by ONE suite in `queuetest`. Nothing above this package may branch on which backend is running. A durable subscription IS a seat's mailbox: it exists without a consumer and retains what is published while nothing is attached. Handlers have three outcomes — ack, nak, and *defer*. Attachment has four verbs of differing destructiveness, because `unsubscribe` never said which one it was |
| `coord` | Cross-process ownership: TTL leases with a monotonic fencing epoch, plus everything else a fleet has to agree on (`fleet.go`) — the activation pointer (a COMPARE-AND-SET: there is no leader, so the flip is what stops two operators overwriting each other) and per-node apply status, the completion ledger, the delivery dedupe, the notification valve, the credential cooldowns, the token counter and the company's SECRETS. Every "do I hold this?" is three-valued — held / definitively not / **unknown** — and treating unknown as loss tears a healthy company down over a two-second store blip. Retention here is a BUCKET's age, never a per-write TTL |
| `store` | The node's LOCAL database: the audit event log, learning memory, and the bootstrap half of the secret store that `fleetsecrets` migrates off at boot. **One file, one process** — exclusively owned, so nothing here has to be safe against a peer, and the migrator's advisory-lock idiom simply disappears. Anything the COMPANY has to agree on belongs in `coord` instead, and migrations 0010–0013 are what that rule cost when it was broken. One driver: `Open` and `Pending` reach a connection through ONE helper, because the second path was the one that skipped the native-library preparation and panicked |
| `events` | The envelope and the typed-payload registry. Two load-bearing properties: evolution is additive-only, and an event type this build does not know decodes, round-trips and re-publishes losslessly — a rolling upgrade puts unknown types on the wire, and dropping them would make every upgrade an outage |
| `org` | The organization model: the hierarchy, the seats that hold it, and the identity derivation every node shares. A seat's id is DERIVED — a UUIDv5 over (org name, handle) — so every node computes the same id with no database and no running instance, which is what lets the node that wins a delivery route to a seat it is not running. A pool miss means "not on this node", never "does not exist" |
| `config` | The two tiers. Tier A (`Bootstrap`) is ops-owned, on disk, restart to change. Tier B (`Company`) is founder-owned, versioned in the store, edited live. Tier A is the root of trust and therefore resolves with `EnvOnly` — it holds the keys to the secret store, so it can never read a value out of it. Tier B's secrets are `${VAR}` POINTERS, stored verbatim, resolved only where a provider or transport is constructed |
| `configplane` | The control plane: how a fleet agrees which company configuration it is running, and what a node does when it does not have the current one. An append-only ACTIVATION POINTER names the epoch and every node writes its own apply status; a node reads both and picks a posture. Append-only because re-activating an UNCHANGED revision is the credential-rotation gesture, and a pointer keyed on revision id would rebuild nothing on exactly that operation |
| `seat` | Which seats this node runs. The placement policy is deliberately dumb — greedy claim to `ceil(seats / live nodes)`, converging in BOTH directions, `preferred` orders the attempt and never gates it. `watchdog` is the other half: a wedged event loop cannot be signalled, so the thread's only honest move is to exit |
| `node` | Where the fleet layer becomes runnable — a broker client, a coordination backend and the seat host, composed. Deliberately thin: the one behaviour none of its parts can express alone is that WHEN A SEAT IS ACQUIRED ITS MAILBOX IS ATTACHED, and released in an order that never leaves a seat consuming work it no longer owns |
| `engine` | The ENTANGLEMENT POINT — which concrete thing satisfies which seam. Deliberately many small files rather than one large one -- this package is flat, and the hardest passage of all, the inbox handler, is a package of its own (`agent/inbox`); the config apply is `reconcile.go` and `epoch.go` here. `mcp.go` is where `mcp_servers:` becomes running children: shared servers on the epoch, per-role children on a seat's LEASE, each claimed seat with its own bridge and its own registry — two children of one template publish the same tool names, so anything shared between seats hands one seat's identity to another |

*The agent runtime:*

| Package | What it is for |
|---|---|
| `agent/turn` | The turn loop — Plan, Execute, Review — and the guards between phases |
| `agent/runner` | Drives the three phases against real models and real tools — the wiring between the prompt builder, the tool registry, the provider chain and the turn loop's contract, none of which knows the others. Plumbing, plus the two rules plumbing cannot express: how a phase's structured answer is extracted, and what happens when the model never gives one |
| `agent/phase` | The per-phase vocabulary: which phase is running, which model it runs on, and the one question a phase's outcome turns on — did it actually DELIVER? |
| `agent/toolloop` | The model↔tool round-trip, and the `Suspend` primitive a detached coding run returns through |
| `agent/execstate` | The wire format for a suspended Execute loop. A detached sandbox run stops the phase mid-loop with its tool call UNANSWERED and is re-entered later — after a restart, possibly on another node — so the conversation is serialized into the pending-run row rather than parked in a goroutine. The sandbox coordinator carries it back without ever decoding it. |
| `agent/extension` | The round-cap extension judge: when a phase runs out of tool rounds, a cheap model says whether it is progressing or thrashing, and the engine grants more or falls through to the rescue path. THE ARITHMETIC IS SEPARATED FROM THE JUDGE — how many rounds a decision is worth is pure policy, impossible to exercise through a live model, so it is a value type tested directly |
| `agent/subagent` | The ephemeral workers an Execute phase spawns: one tool loop with a parent-written prompt, a parent-chosen slice of the parent's own tools, and hard caps on rounds, wall-clock and tokens. A sub-agent is a LEAF — it spawns nothing further and contacts no colleague — which is the only thing bounding the depth of the construct, so there is no depth counter anywhere. The grant is a security boundary, enforced in code because a prompt is only a request |
| `agent/inbox` | What wakes a seat, and what it must not be woken by twice. `ledgered` is the completion side |
| `agent/ledger` | The prior-work ledgers: `iteration` (within a turn), `conversation` (across turns in one thread), `budgets`. Structured, never a transcript replay — the thread has moved by the next turn |
| `agent/prefetch` | The Plan-phase context assembly: relevant knowledge, memory, the blocks a prompt gets |
| `agent/prompts` | The prompt text. Tuned against observed model behaviour; a reworded section is a behaviour change |
| `agent/skills` | Prompt fragments admitted from the knowledge base and injected per phase, plus the load-before-use guard |
| `agent/builtin` | The tools the engine itself ships |
| `agent/colleague` | Resolving a free-text query to a seat — the lookup before an A2A ask, a chat mention or a page mention. A model types what it remembers, so the answer is either one seat or an honest list of who it might be, NEVER a guess. Four tiers, earlier ones short-circuiting later, so a query that is exactly somebody's handle is never fuzzy-matched against everybody else |
| `agent/turnctx` | What a turn IS, carried as an explicit argument rather than ambient state. A turn has at least five values that want to travel ambiently, and a goroutine SHARES whatever context it captured — forever, including after the turn that created it finished — so what would elsewhere merely obscure a dependency is a live data race here. |
| `tools` | The registry, holding four kinds of tool under one contract and recording WHICH at registration — the only frame that knows, and the last that can say |
| `mcp` | MCP client and child supervision. A stdio server is a process TREE, not a process. |
| `a2a` | Agent-to-agent asking: one ask, one answer, then closed. THE CHANNEL IS AN AUTHORIZATION RECORD, NOT A TRANSPORT — nothing queues here, and both the brief and the reply travel over the durable seat inbox, so a colleague owned by another node is an ordinary target. The in-process bus this replaced woke the target on whichever node owned ITS seat, where the queue was empty |
| `knowledge` | The backend-neutral seam the Plan-phase knowledge prefetch reads through. Exactly ONE backend per company, because two would make "what do we already know about this" depend on which searcher was asked. A caller passes a seat, plain text and ancestor exclusions — never a CQL fragment, a space key or a project list — and search is BEST EFFORT: every failure path is an empty result, so a turn never dies because a wiki was slow |
| `providers/llm` | The model contract. It does NOT retry (rotation belongs to the credential pool, fallback to the chain) and does NOT decide what a failure means beyond a coarse kind |
| `providers/credential` | The key pool a provider rotates through: each call leases a key, a rate-limit or auth failure benches it for a TTL, and an exhausted pool hands the seat's fallback chain on to another model. Cooldowns are measured on a MONOTONIC clock, so an NTP correction or a VM migration neither revives a benched key early nor strands a live one; selection is least-in-flight, declaration order breaking the tie |
| `providers/embeddings` | The vector backend behind diary and episode recall |
| `sandbox` | Code work as a suspended Execute phase. A coding run is DETACHED: the tool starts it, the loop suspends, and the engine resumes that same loop — minutes later, possibly after a restart, possibly on another node. Nothing parks a goroutine on a running job. Two real backends behind one contract — a remote VM per run (E2B, cloud or self-hosted) and the engine host itself — and the closed set `providers.sandbox.type` accepts is asserted against the switch that builds them |
| `hostbox` | The primitives for running somebody else's process in a directory on the engine host. Two subsystems must do it IDENTICALLY — the subscription CLI backend, which drives a coding CLI per seat with its own HOME, and the local sandbox provider — and each primitive is a guard, so a second implementation is not drift but a hole |
| `procgroup` | Addressing a child process TREE with one signal. Every long-running child starts behind something that forks — `uvx`, `npx`, a Node runtime, a shell — so signalling the process Go started reaches the launcher and nothing beneath it. Its own process group makes the tree one negative pid, and it lives here rather than beside either caller because two copies of a platform-conditional signal helper is how one stops matching the other |
| `learning` | What a seat remembers. Everything here is BEST EFFORT by design: a failed write is logged, a failed read answers empty. `memsync` is the exception that makes the rest true across a fleet: a seat's memory is written to the NODE's store, and placement moves seats — so every memory row rides a COMPACTED CHANGELOG (one subject per row, one message retained per subject) and a node hydrates a seat's rows before its mailbox attaches. Deletes deliberately do not travel: the lifecycle re-converges, and a tombstone protocol would be a second thing to keep correct forever |
| `schedule` | Role- and unit-scoped cron work, with at-most-once delivery, missed-tick catchup and a wall-clock cap |

*The edges:*

| Package | What it is for |
|---|---|
| `api` | REST + the dashboard. ONE wiring for embedded and standalone; what differs is only what the node can SEE, and that is one seam (`NodeRuntime`). Auth guards every route bar probes, webhooks (HMAC), OTLP (signed token) and the dashboard shell |
| `api/webhooks` | The six inbound vendor routes, plus Slack's OAuth landing. A route whose secret is unset has nothing to verify with and answers 503 — it does not accept the delivery |
| `whsec` | The Standard Webhooks signing-secret format, and the one place that decides what a valid one is: `whsec_` plus padded base64 over a 32-byte key, with the DECODED bytes as the HMAC key. Three places need the rule and none may disagree — written twice it drifted, and a `${VAR}` holding a 16-byte key was refused as a literal and silently accepted as a reference |
| `api/secretsapi` | `/secrets`: the company's credentials, written through a running node because the coordination broker is inside its process and listens on no socket. Always guarded, reads included; the ONE route that returns a value needs an explicit `?reveal=true` and logs the access |
| `api/stream` | The `/ws/stream` socket: pushes, plus a request/response query channel that is a thin adapter over the SAME function each REST route calls |
| `observe` | The observability edge, and the two routes are deliberately different: the STORE is written by a publish listener inline on the publishing node (no consumer group, so no two nodes can write one row); the PROJECTION is fed by an ephemeral per-caller broadcast |
| `tokens` | Rolls per-phase LLM spend into the breakdown a dashboard renders — by phase, model, worker, agent and turn. ONE IMPLEMENTATION is the whole point: this aggregation had three copies once, and a refresh routinely disagreed with the page it replaced. A leaf package, because both the live projection and the event store need it and either importing the other would be a cycle |
| `tracing` | The OpenTelemetry wiring: ONE TracerProvider, built from the standard `OTEL_*` environment (never a Tier A block), and the conversion between OTel's `context.Context` carrier and the `trace_id`/`span_id`/`parent_span_id` the event envelope has always had. The provider is installed UNCONDITIONALLY and only the exporter is optional, so ids reach the event store whether or not a collector exists — and its own `init` installs a working one, because OTel's built-in default is a no-op that passes the parent's span context through and would silently reinstate the bug this package removed |
| `notify` | The backend-neutral notification spine — conversation keys, digest coalescing, party resolution, the rate valve. Built before any vendor sat on it, because a spine built after its first vendor has that vendor welded into it |
| `backup` | A restorable copy of everything a node holds, taken from INSIDE the engine because that is the only place both estates are reachable: the store file is locked to this process and the driver refuses a second opener, and the embedded broker binds no socket for any outside tool to dial. The store copy is `VACUUM INTO` (the backup API is stubbed and plain `VACUUM` is behind an experimental flag) and is VERIFIED before it counts; the streams — mailboxes and every coordination bucket alike, since a bucket IS a stream — go over the JetStream wire API, because the vendored client has no snapshot call at all. The MANIFEST IS THE CLAIM: written last, and a directory without one is the debris of a run that did not finish rather than a partial backup |
| `maintenance` | The retention sweep for the tables that answer "recently" rather than "ever" — the delivery dedupe, the notification valve, fire claims, channel membership, per-node config status. Every one of their migrations says they are swept and ships the index a range delete needs — a table with a documented retention and no sweep grows for the life of the deployment. A fleet singleton, because N nodes scanning the same rows is waste rather than corruption |
| `mattermost`, `slack`, `gitlab`, `github`, `jira`, `confluence` | The vendors. Each contributes only what is genuinely its own: a client, a parser, a transport, a prompt, a provisioning reconcile — and no more, which is why Jira has no transport (an agent writes through its own MCP tools) and why its reconcile and GitHub's report rather than mint (neither vendor issues a credential on a provisioner's behalf) |
| `fleetsecrets` | The company's credential store: this package owns the KEY, coordination owns the BYTES. Also the one-way migration off `store`'s own table, which copies before it deletes and never overwrites a name the fleet already holds |
| `logging`, `secrets`, `provision`, `envref`, `envfile`, `redact`, `workkey` | The small shared grammars. Each imports nothing from the rest of the engine, which is what lets `config` itself depend on them |
| `version` | What the binary calls itself: the tag goreleaser stamps at link time, falling back to the module's own build info and then to `dev`, so a binary built outside the release path names itself honestly rather than claiming to be a release it is not. Its tests cover the package and nothing else now: four cases over the stamp/build-info/`dev` fallback. They used to be ten times its code and to assert the RELEASE SURFACE instead — the ldflags stamp, the pure-Go build, the image's `${TARGETPLATFORM}` copy, the catch-all notes category, the pre-release flags, the single tag trigger and every Dependabot entry — because each of those fails silently and has no other symptom |

## Pre-commit Checks
Before committing, ALWAYS run and fix any issues from **`make check`**, which is every gate CI runs on a pull request, in one target:
- `gofmt -l .` — prints the files that need formatting; the output must be empty
- `go mod tidy -diff` — go.mod / go.sum must already be what `go mod tidy` would write. It prints the patch and exits non-zero rather than writing it; `make tidy` applies it. This catches the half the build cannot: an UNDER-tidy module already fails `go build ./...`, but a leftover `require`, a stale `go.sum` line or a wrong `// indirect` marker compiles green and lands as churn in someone else's pull request
- `go vet ./...`
- `golangci-lint run` — what CI's lint job runs
- `go build ./...`
- `go test ./... -race -count=1` — the full suite, under the detector, as CI runs it
- a cross-compile of every release target (linux and darwin × amd64 and arm64)

The race detector is not a special case for concurrency work: CI runs the WHOLE suite under it, because the engine's concurrency model is real parallelism and every "atomic because it is single-threaded" assumption is a data race until proven otherwise. `-count=1` is the other half — without it a cached PASS from before the change answers for the change. `make test-norace` is the faster loop and is not a gate.

**The Makefile is the same command CI runs, or it is a lie — and nothing checks that any more.** Every target mirrors `.github/workflows/ci.yml` flag for flag. `internal/version/makefile_test.go` used to assert it; it was dropped with the rest of the file-content assertions, so a target that quietly drops `-race`, stops cross-compiling a release target, stops setting a `CREWLET_*` variable a suite selects on, or starts a compose profile `docker-compose.yml` does not define would report a pass CI never gave and NOTHING would notice. Change a target and its `ci.yml` step in the same commit, and read both. A new target still ships with its `## ` help line — `make help` is to this file what `usage()` is to the CLI — but only `usage()` has a test behind it now.

**A skip is not a pass.** Two suites need something the machine may not have and skip silently without it: the dashboard's suites need `node`, and `internal/e2e`'s client-half replay needs `node` too. A green local run has not necessarily exercised either. CI fails rather than skips where it can, and so do the make targets: they refuse to run without node. No queue backend is in that category any more — both certify themselves in-process, the JetStream suite starting an embedded broker per test, so there is no environment variable whose absence silently drops a backend. What `make check` does NOT cover, it prints when it passes — `make snapshot`. `internal/store`'s capability cases skip DELIBERATELY, and that is the mechanism rather than a gap: a feature Turso announces but does not reach Go yet turns into a passing test the day it lands.

## Dependency Updates
Dependabot watches every dependency surface in the repo — `.github/workflows/*.yml` (`github-actions`), `go.mod` (`gomod`), `Dockerfile` (`docker`), and `docker-compose.yml` (`docker-compose`). The config is `.github/dependabot.yml`: one entry per surface on a weekly schedule, plus the commit prefix that surface's bumps carry, and nothing else; keep it that way unless a knob earns its place. `docker` and `docker-compose` are separate ecosystems reading separate manifests — a repository with both needs both entries.

- **Pick the newest release, then pin it exactly.** Every time a version gets chosen — a Go module, a tool you reach for, a base image, an action, a compose service, a toolchain — take the latest released one, and establish what that IS from the registry or index rather than from memory. A version that was current when something was written is a version that is now behind, and quietly picking it forfeits whatever was fixed since, with nothing to say so. "Latest" here means the newest release written as a LITERAL pin (`4.2.4`), never a floating tag: `latest` and `@main` give Dependabot no version to move, so the surface is watched by nothing while looking exactly like a surface with nothing to update — and a floating tag makes a green run a claim about a build nobody can name afterwards, which is the whole reason every compose service is pinned to the one CI runs against.
- **A version held back on purpose carries its reason AT the pin.** That is the one exception, and it is a comment on the line, not a memory: `mattermost-db` holds its Postgres major so an existing volume stays readable. Without the comment the next reader — or the next agent — cannot tell a considered hold from a bump nobody got to, and "upgrade everything" silently breaks the thing the hold was protecting.
- **A new dependency surface ships with its `updates:` entry.** Adding the first `Dockerfile`, a `package.json`, a `go.mod` — the Dependabot entry is part of that change, not a follow-up. Nothing fails when it is missing; the surface is just never watched, which looks exactly like a surface with nothing to update.
- **Actions pinned to a non-version ref are invisible to Dependabot.** A branch pointer (`@release/v1`) or `@main` yields no update PRs at all, so pin actions to a version tag or a full SHA. Maintainer-facing detail is in `CONTRIBUTING.md`.
- **A bump merges itself.** `.github/workflows/dependabot-merge.yml` approves a Dependabot PR and queues it with `gh pr merge --auto --squash`, so it lands on green CI with no maintainer in the loop. Two things outside the tree hold that up, and neither is visible from a checkout: `main`'s protection rule must REQUIRE the `ci` checks — `--auto` waits for the checks a rule names and for nothing else, so with no rule the bump merges the instant it is mergeable — and "Allow auto-merge" must be on in the repository settings, or the step fails and paints the PR red. The job runs only when Dependabot is BOTH the PR's author and the actor that triggered the run; the actor half is what stops a commit a PERSON pushed onto a Dependabot branch from collecting the repository's own approval.
- **Nobody retitles a bump before it lands**, so each entry pins its `commit-message.prefix`, and every surface carries the same one: `build(deps)`. A bump moves a pinned version, which is a change to what the project builds against whichever file records it, and it is what the bumps already in `main` carry. Unset, Dependabot INFERS the prefix from recent history, and an inference that changes its mind writes a bare `Bump x from 1 to 2` straight into `main` and into the release notes. Dependabot capitalises the "Bump" and offers no way not to, so a bump is the one subject here that does not start lowercase.
- **The auto-merge guard is remembered, not asserted — so read it on every edit.** `internal/version` used to check both halves of the author/actor condition and the `--auto --squash` flags; that test was dropped. Nothing checks them now, and each fails silently: weaken the `if:` and the workflow keeps running, it just starts approving and merging pull requests Dependabot did not write; drop `--auto` and the bump merges before a check has reported. This file has `contents: write` and `pull-requests: write` and runs `gh pr review --approve`, so its `if:` is the only thing between that and an unreviewed push to `main`. Treat any diff to `.github/workflows/dependabot-merge.yml` as security review.


## Releases
The engine ships as signed binaries and a container image, built by goreleaser from a `v*` tag. See `RELEASING.md`.

- **The tag is the version.** Nothing in the tree records one, so there is nothing for a tag to disagree with and no version bump to make. goreleaser stamps the tag into `internal/version.value` at link time; a binary built any other way reports its module build info instead, so it names itself honestly rather than claiming to be a release. **Never add a literal version constant back.**
- **Version numbers follow semver** — the minor number moves for features, the patch number for fixes.
- **Pushing the tag is the whole release** — the workflow builds four targets, signs the checksums with keyless Sigstore, pushes the image to GHCR and creates the GitHub Release with notes GitHub generates from the merged pull requests. Those generated notes are the only description a release gets, so pull request titles carry it. A pre-release takes neither GitHub's "Latest" nor the `latest` image tag.
- **The release surface is NOT asserted — read it yourself when you touch it.** `internal/version` used to check the ldflags stamp, the pure-Go build, the `${TARGETPLATFORM}` copy, the catch-all notes category, the pre-release flags and the single tag trigger against the files that carry them; those tests were dropped. What is left is what the pipeline does on its own: `goreleaser check` validates `.goreleaser.yaml`, the snapshot job builds all four targets and the image on any pull request touching the release paths, and both release jobs run the built binary and compare its version against `dist/metadata.json` by equality. That covers the stamp, cgo and the image COPY loudly. It covers **nothing** in `.github/release.yml`, `.github/dependabot.yml`, `docs-publish.yml`'s `workflow_run` name or the `v*` tag trigger — each of those fails silently in production, with no symptom until someone reads a release body, notices a bump that never arrived, or pushes the tag that races.

## Breaking Changes Are Free Until a Tag Ships Them
A `v*` tag is the only compatibility boundary this project has — it is the one thing an operator can pin, pull and run. A surface no tag has ever shipped therefore has nobody behind it: nothing pinned it, no deployment holds its old shape, and there is nobody to migrate. **Change it outright** — rename the config field, reshape the struct, drop the CLI flag, move the route, redraw the interface — rather than carrying a compatibility path for a version that never existed. That path is a band-aid with no wound (see "No Band-Aids" above), and it is permanent: the alias, the fallback branch and the adapter all outlive the reason nobody can reconstruct for them.

Rules:
- **Establish what has shipped; never remember it.** `git tag --merged` and the repository's Releases page say which tags exist, and `git log <newest tag>..HEAD -- <path>` says whether what you are about to change is inside one. Today that answer is *nothing*: the only release is `v0.1.0`, and it sits on the INITIAL COMMIT, which does not contain one `.go` file. Not a package, config field, CLI command, event type, API route, schema file or dashboard module in this tree has ever been in a release. Re-run the check rather than trusting this paragraph; the next tag changes the answer for everything it carries.
- **An unreleased surface gets no compatibility path.** No deprecated alias beside the new name, no "accept both spellings", no `if the old field is set` fallback, no adapter, no `V2` type because the old name has to keep meaning what it meant, no vestigial flag or subcommand kept so that nothing breaks. Every one of them is code with no caller, indistinguishable to the next reader from code whose caller nobody found.
- **And no breaking-change ceremony.** The `!` in `feat(config)!:` and a `BREAKING CHANGE:` footer exist to tell an operator what they have to change. With no release behind the surface there is no operator and no migration, and the generated notes are the ONLY record a release gets — so the ceremony spends the one place a real break has to be visible on a break that never happened.
- **A free break is still a complete break.** Freedom from compatibility is not freedom from the sweep: the rename lands everywhere in the same change — `docs/`, `examples/`, `skills/`, the dashboard, the tests, the package docs — and `schema/` is REGENERATED, never hand-edited. Grep the old name and finish at zero hits.
- **What a missing tag does not free you from is state and peers.** Two constraints hold at any tag count, because neither one is about releases:
  - **An applied migration is history, not source.** `schema_migrations` keys on the FILENAME, so editing a file that already ran silently never re-runs it: every database that applied it keeps the old shape while the code assumes the new one. Reshape with a new numbered migration under `internal/store/schema/`, released or not.
  - **A rolling upgrade puts two builds on one stream and one coordination store.** The event envelope evolves additive-only (`internal/events`) because an unknown type must decode, round-trip and re-publish losslessly in both directions — a contract between PEERS, which no tag count makes optional. The same holds for any key, lease or payload two builds share.
- **Once a tag ships it, the bar changes.** Then a break carries the `!` and a `BREAKING CHANGE:` footer naming what an operator must change, the pull request title says so plainly because that title *is* the release note, and while the major is 0 the minor number moves for it. `CONTRIBUTING.md` and `RELEASING.md` carry the same rule — change all three together.

When in doubt, ask: "Could anyone be running the shape I am about to change?" If no tag ever shipped it, nobody is — so change it cleanly and completely rather than carrying its ghost forward.

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
1. **Event Queue** — durable pub/sub behind one contract: an embedded NATS JetStream by default, clustered embedded members or an external NATS cluster for a fleet, an in-memory twin for tests
2. **Organization Model** — hierarchy is the execution graph
3. **Agent Runtime** — queue-driven seats, a three-phase turn, an LLM tool loop that can suspend
4. **Task Engine** — there is none: task state lives in the PM tool, and the engine mirrors nothing
5. **Decision Framework** — DACI behavioral guidance (via chat channels, no dedicated engine)
6. **Knowledge System** — query-time knowledge-base search for shared docs (Confluence CQL — single-homed, one per company, behind a seam that keeps it swappable) + per-agent diary
7. **Communication** — external chat (Mattermost, Slack) + ephemeral A2A channels. The six vendors this build serves are Mattermost, Slack, GitLab, GitHub, Jira and Confluence; every one routes end to end, and no integration block is refused any more
8. **Notification Service** — queue-based spine, vendors on top
9. **Provider Layer** — pluggable LLM and embeddings, with a credential pool and a fallback chain around them
10. **Store** — the node's own embedded database (Turso, the only driver); coordination lives in the KV layer instead, never here
11. **Coordination** — TTL leases with a fencing epoch, plus the fleet's shared counters, ledgers and the company's sealed credentials
12. **Seat Ownership** — which node runs which seat, and how a fleet converges without a coordinator
13. **Tool Registry** — builtins + MCP tools + A2A tools, each recording its origin
14. **Tool Skills** — knowledge-base-sourced prompt fragments injected per phase
15. **Code Sandbox** — a per-role coding-agent Execute backend; a run is detached and resumes a suspended loop. E2B for a remote VM, the engine host for a local one
16. **API + Dashboard** — one wiring, embedded or standalone; the websocket is the dashboard's only data channel
17. **Scheduler** — role/unit-scoped cron work with at-most-once delivery, catchup and a wall-clock cap
18. **Control Plane** — the config activation pointer and per-node apply status; lag alone never sheds
