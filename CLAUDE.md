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
Crewlet is a Python engine for orchestrating hierarchically organized AI agent companies.
See `docs/index.md` for the full documentation index.

## Tech Stack
- **Python 3.12+** — use modern syntax (type unions with `|`, match statements where appropriate). 3.12 and 3.13 are both tested in CI and declared in the classifiers; anything you write must run on both
- **Pydantic v2** — all data models must be Pydantic BaseModel or use Protocol classes for interfaces
- **asyncio** — all I/O is async. Use `async def` for any function that does I/O
- **openai + anthropic SDKs** — official SDKs for LLM providers
- **mcp** — official MCP Python SDK for MCP client/server protocol
- **PyYAML** — config parsing
- **structlog** — structured logging (see `src/crewlet/_logging.py`)
- **pytest + pytest-asyncio** — testing (asyncio_mode = "auto")
- **ruff** — linting and formatting

## This Is a Public Open-Source Repository
Crewlet is MIT-licensed, developed in the open at `github.com/crewlet/crewlet`, published to PyPI as `crewlet`, and its `docs/` tree is served at docs.crewlet.ai. Everything committed is published, permanently: git history keeps what a later commit deletes, and the sdist keeps what the working tree drops. Write every change as something a stranger will read, run, and depend on.

Rules:
- **Never commit a secret, and never fix one by deleting it.** No real API keys, tokens, PATs, webhook secrets, private hostnames, internal URLs, or customer/employee names — in code, tests, docs, examples, fixtures, or commit messages. Config examples use `${VAR}` references and placeholder values (`example.com`, `U0FOUNDER`, `sk-ant-...`). If a real credential does land in a commit, it is compromised the moment it is pushed: it must be **rotated**, and removing it in a follow-up commit is not a fix.
- **Commit subjects are semantic** — `type(scope): summary`, imperative, lowercase, no trailing period, ≤72 chars. Type is one of `feat` / `fix` / `docs` / `refactor` / `perf` / `test` / `ci` / `build` / `chore` / `revert`; scope is the *component*, normally the package directory under `src/crewlet/` (`agent`, `sandbox`, `plane`, `api`, `db`, `dashboard`, …) or the area for everything else (`docs`, `deps`, `packaging`, `examples`, `schema`, `scripts`, or the workflow name for `ci`). Unrelated fixes and tuning go in their own commit with their own scope. A pull request squash-merges into one commit whose subject is its title, so the title takes the same form. Nothing enforces this and history is permanent — the full rules are in `CONTRIBUTING.md`.
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
- Use `Protocol` classes (from `typing`) for all provider interfaces
- Use `UUID` for all entity IDs (auto-generated via `uuid4`)
- Use `datetime` with UTC timezone for all timestamps
- Use `enum.Enum` or `StrEnum` for status types
- Prefer composition over inheritance
- Every public class/function needs type annotations
- Use official protocol SDKs (e.g. `mcp`, `openai`, `anthropic`) where available

## Logging
All logging MUST use structured logging via `structlog`. Never use stdlib `logging` directly.

```python
from crewlet._logging import get_logger

logger = get_logger("module.name")  # e.g. get_logger("task.engine")

# Use snake_case event names with keyword arguments — no printf-style formatting
logger.info("task_created", task_id=task.id_str, creator=creator)
logger.debug("resolving_hierarchy", agent_id=agent.id_str, role=role)
logger.warning("budget_exhausted", agent_id=agent_id, used=used, limit=limit)
logger.error("mcp_server_failed", server=name, error=str(exc))
logger.exception("handler_failed", handler=name, event_type=event.type)
```

Rules:
- Get loggers via `get_logger("component.name")` — this binds `component=` automatically
- Event names are short, machine-parsable snake_case strings (not sentences)
- All dynamic data goes in keyword arguments, never in the event string
- Never use `logging.getLogger()` or `logging.basicConfig()` — use `configure_logging()` from `_logging.py`
- App startup logging is configured via `configure_logging(level, fmt)` in `cli.py`

## Package Layout
```
src/crewlet/          # Main package
  engine.py           # Engine class — central entry point
  config.py           # YAML config → Pydantic models
  cli.py              # CLI commands (run, validate, api)
  cli_llm.py          # `crewlet llm` — operate the subscription CLI LLM
                      #   backends: list / login (broker the vendor OAuth,
                      #   --capture-token, --username/--password-stdin) /
                      #   logout / status / doctor (binary + version + login
                      #   + a REAL smoke completion, since a profile can look
                      #   right and still not emit a parseable tool call) /
                      #   export / import
  _env.py             # Shared .env loader for CLI entry points (run, api,
                      #   confluence/plane import + resync, gitlab/plane
                      #   provision) — load_env_file
  config_resolution.py # Resolution FINGERPRINT — a keyed, per-process
                      #   digest of what a payload's ${VAR} references
                      #   currently resolve to. Half of apply_config's
                      #   no-op check: re-activating an unchanged
                      #   revision is the documented rotation gesture, so
                      #   a payload-only comparison made it rebuild
                      #   nothing. Never persisted or logged
  env_refs.py         # THE ${VAR} reference grammar — one compiled pattern
                      #   shared by config.py (substitution), secrets/
                      #   registry.py (skip-pointers-when-masking), org/
                      #   models.py (contact literal-vs-reference) and
                      #   provisioning.py (mint-into-this-var). Imports
                      #   nothing from crewlet, so even config.py's own
                      #   dependencies can use it. Four divergent copies
                      #   previously caused a redaction leak and a
                      #   mint-into-unreadable-${1} hole; tests/test_env_refs
                      #   fails the build on a new re.compile of the grammar
  provisioning.py     # Integration-agnostic ${VAR}-minting contract shared
                      #   by the provisioning CLIs: TokenSink protocol
                      #   (record/discard/flush are ASYNC — a sink may
                      #   persist remotely, and write-through closes the
                      #   minted-but-unpersisted window; discard is the
                      #   other half — a credential revoked because it
                      #   could not be persisted everywhere must not be
                      #   left standing in the vars that WERE written,
                      #   since a dead token reads exactly like a live
                      #   one) + SecretStoreSink
                      #   (encrypted secret_values — the engine reads it back
                      #   directly, no file to source) / EnvFileSink
                      #   (understands `export VAR=value` lines; created 0600
                      #   at open(), write-through on record; flush is a no-op
                      #   when nothing was recorded and the file never
                      #   existed) / PrintSink + add_sink_arguments +
                      #   open_token_sink (shared --secret-store/--env-file/
                      #   --print flag set + factory) + referenced_env_vars +
                      #   sole_env_var (exactly-one-whole-reference check for
                      #   capture contracts);
                      #   per-integration seat scans live in each
                      #   integration's module
  org/                # Organization model (hierarchy, roles; Role.kind
                      #   agent|human — human seats are addressable,
                      #   never spawned; see docs/concepts/humans-in-the-org.md.
                      #   models.py also owns SEAT IDENTITY —
                      #   agent_id_for / agent_seat_by_handle /
                      #   agent_seat_by_id: derive_agent_id is a uuid5 over
                      #   (org name, handle), so any node recovers any seat
                      #   from an id with no DB and no live instance. That
                      #   derivation + its inverse is what lets routing
                      #   address a seat this process is not running)
  agent/              # Agent runtime (definition, instance, pool, turn engine)
                      #   definition.py, instance.py, pool.py, memory.py,
                      #   turn.py (TurnEngine; resume_state re-enters Execute
                      #     mid-loop on sandbox completion), plan.py,
                      #   iteration_log.py (prior-work ledger —
                      #     IterationRecord + render_iteration_ledger. Each
                      #     phase rebuilds its LLM conversation per
                      #     iteration and plan_tool_executions resets, so a
                      #     self_iterate round would otherwise re-plan a
                      #     delivery that already fired. The engine-recorded
                      #     tool calls carry the guarantee — notably on the
                      #     done→self_iterate override, where Review wrote
                      #     no completed_work at all — and
                      #     ReviewOutcome.completed_work adds the prose
                      #     gloss on top. Args elide PER VALUE, never per
                      #     blob: json.dumps preserves key order and the
                      #     discriminator (channel/key/page_id) is usually
                      #     the shortest value),
                      #   onboarding_phase.py (run_onboarding_phase — dedicated
                      #     first-turn onboarding pass BEFORE Plan, own round
                      #     budget so it never starves submit_plan; strictly
                      #     run-once per org chain: tri-state marker read
                      #     (None=lookup failed → skip, retry next turn) +
                      #     process-local latch + single-flight lock on
                      #     AgentInstance + cross-process DB pass-lease
                      #     (marker-store try_claim_pass, TTL-bounded)),
                      #   execute.py (run_execute_phase — allow_suspend loop;
                      #     ExecuteResumeState resume of a suspended loop),
                      #   execute_sandbox.py (launch/collect/teardown plumbing
                      #     for the run_sandbox tool), review.py,
                      #   subagent.py, guards.py, prompts.py, turn_context.py,
                      #   phase_model.py, llm_loop.py (run_tool_loop suspend
                      #     primitive — ToolResult.suspend; publishes
                      #     AgentTurnProgress TWICE per round — once the
                      #     model has spoken, before its tools run, and
                      #     again once they return — and builds that
                      #     response with the SAME
                      #     assistant_text_with_reasoning the phase record
                      #     uses, so the dashboard's live row and the turn
                      #     you expand later are one text, reasoning
                      #     included, not two assemblies of it),
                      #   extension.py (round-cap extension judge for Plan/Execute),
                      #   tool_discovery.py (activate_tool +
                      #     list_mcp_server_tools meta-tools shared by Plan/Execute),
                      #   skills/ (PromptSkill registry + per-backend sync
                      #     workers: sync.py ToolSkillSyncWorker
                      #     (Confluence, space+marker-label admission),
                      #     plane_sync.py PlaneSkillSyncWorker (Plane —
                      #     strict single-enumeration boot walk, webhook
                      #     fetch+re-admit with evict on 404/archived/
                      #     decode-failure); PromptSkill.source_page_id/
                      #     source_page_version are the backend-neutral
                      #     provenance fields (Plane stamps version 0);
                      #     confluence_codec.py (Confluence wire format —
                      #     shared markdown/html helpers live in
                      #     knowledge/markdown_docs.py);
                      #     guard.py — required-skill load-before-use guard;
                      #     publish CLIs live in confluence/ + plane/)
  queue/              # EventQueue protocol (Pulsar + memory; subscribe_batch +
                      #   BatchOptions — batched key-partitioned inbox delivery.
                      #   ATTACHMENT LIFECYCLE is four verbs, not one:
                      #   quiesce (stop taking new work, stay attached) /
                      #   unquiesce (its inverse — a store blip keeps the
                      #   seat, so a quiesce that is never followed by a
                      #   detach must be reversible or the node is owned,
                      #   attached and deaf forever; on Pulsar it restarts
                      #   the consume loop AND asks the prefetch back, else
                      #   the fetched-unacked messages wait out the 30-min
                      #   ack timeout) / detach (non-destructive, keeps the
                      #   subscription + its mail) / delete_subscription
                      #   (destructive, needs NO local consumer — role
                      #   decommission must not depend on which node ran
                      #   the seat). `unsubscribe` is gone: its name never
                      #   said which one it was. DeferDelivery is the third
                      #   handler outcome beside ack-on-return and
                      #   NAK-on-raise: leave it unacked, stop consuming;
                      #   admin.py (BrokerAdmin + PulsarBrokerAdmin —
                      #   subscription lifecycle over the admin v2 REST API,
                      #   because creating one by SUBSCRIBING joins a Shared
                      #   subscription a peer owns and steals its traffic,
                      #   measured);
                      #   memory.py is a BROKER + N CLIENTS, not one object:
                      #   _Broker holds subs/mail/DLQ, each MemoryEventQueue
                      #   is a node (client() mints a peer). Conflating them
                      #   meant one node's detach dropped its peer's consumer
                      #   and one node's pause gated the whole subscription;
                      #   topics.py — THE subject
                      #   grammar: agent_inbox_topic(handle) is the one
                      #   definition of crewlet.agent.{handle}.inbox,
                      #   which nine call sites used to f-string by hand.
                      #   A producer and a consumer that disagree about a
                      #   topic name never raise — the publish lands in a
                      #   topic nobody reads. Imports nothing from crewlet;
                      #   tests/test_queue/test_topics fails the build on a
                      #   new hand-built f-string, in src/ AND in tests/)
  a2a/                # Agent-to-agent channels over the DURABLE queue.
                      #   The A2ABus (an asyncio.Queue per channel) is
                      #   GONE: it was a second delivery path beside the
                      #   seat inbox that carried the wake, and a path
                      #   that only works in one process cannot be a
                      #   fast path for a fleet — the target woke on the
                      #   node owning ITS seat and found the queue
                      #   empty. channels.py = A2AChannelStore (Postgres
                      #   + memory twin) — the participants and open/
                      #   closed state every authorization decision
                      #   reads; service.py carries the brief ON the
                      #   wake event and REPLIES by publishing the
                      #   answering turn's final text back (there was no
                      #   reply path at all: the prompt named
                      #   send_a2a_message, a tool that is not
                      #   registered and that tests assert is absent).
                      #   One ask, one answer, then closed — which is
                      #   what stops a volley; a2a_ask charges the
                      #   delegation cap, the reply does not
  db/                 # Database layer (asyncpg, migrations, token_usage,
                      #   deterministic agent-id derivation in agents.py;
                      #   client.py — Database.acquire()/transaction()/
                      #   advisory_lock() hold ONE connection, which is what
                      #   makes session-scoped state work (asyncpg's pool
                      #   reset runs pg_advisory_unlock_all on release, so a
                      #   pool-path advisory lock is a silent no-op);
                      #   budgets.py / deliveries.py / credentials.py /
                      #   rate_limits.py — the SHARED COUNTERS: token
                      #   budget usage (caps stay config-derived in
                      #   memory, only usage is shared), inbound webhook
                      #   dedupe (GitHub/GitLab had none at all),
                      #   fleet-wide credential cooldowns, and the
                      #   notification valve. Each ships a Postgres store
                      #   + a memory twin under one contract suite;
                      #   turn_completions.py — the COMPLETION LEDGER:
                      #   "has this trigger already been worked?", read
                      #   before a turn and written after one. NOT a
                      #   claim — no in_progress, no expiry, no
                      #   supersede rule: the seat lease is already the
                      #   mutual exclusion, so a claim's only honest
                      #   disposition for a stale row is "re-run", which
                      #   is what you do with no row. Keyed on
                      #   CONSTITUENT event ids because a coalesced
                      #   digest is minted fresh every time. BOTH
                      #   directions fail open — not knowing whether
                      #   work was done has one safe answer, and it is
                      #   the pre-ledger one;
                      #   maintenance.py — MaintenanceWorker, the
                      #   retention sweep for the five tables that
                      #   answer "recently" and are written on every
                      #   event that asks (webhook_deliveries,
                      #   rate_limits, scheduled_runs, turn_completions,
                      #   a2a_channels — plus the idle-close of A2A
                      #   channels no turn ever answered). Both migrations
                      #   always SAID rows were swept on a TTL and both
                      #   ship the index for it; `purge` existed on the
                      #   stores and on their protocols and NOTHING
                      #   called it, so all three grew for the life of
                      #   the deployment. One retention per table, tied
                      #   to what that table is for — the ledger's floor
                      #   is catchup_max_seconds, because deleting a row
                      #   a tick could still evaluate lets that fire run
                      #   twice;
                      #   leases.py — LeaseStore/MemoryLeaseStore, the
                      #   cross-process ownership primitive: TTL lease +
                      #   monotonic `epoch` fencing token, owner = a process
                      #   INCARNATION (config.resolve_node_incarnation),
                      #   release expires in place so the epoch never resets;
                      #   config_plane.py — THE CONTROL PLANE (replaces the
                      #   competing-consumer `engine-config`/`api-config`
                      #   groups, under which exactly ONE process applied a
                      #   revision and the rest ran the old company forever):
                      #   config_activations = append-only epoch pointer
                      #   (appended INSIDE CompanyConfigStore's own
                      #   activation transaction via ACTIVATION_INSERT_SQL,
                      #   and append-only because re-activating an UNCHANGED
                      #   revision is the documented rotation gesture) +
                      #   config_apply_status = per-node outcome, three-valued
                      #   (ok | error | degraded — degraded = failed AFTER a
                      #   restart-required subsystem was mutated, so rollback
                      #   could not restore it; never counted as converged) +
                      #   decide_posture (serve|wait|shed|isolated|stuck).
                      #   The rule: lag alone NEVER sheds — every successful
                      #   rollout produces lag, so shedding on it makes the
                      #   fastest node the cause of a fleet-wide outage.
                      #   See docs/concepts/control-plane.md;
                      #   secret_values.py — SecretValueStore over the
                      #   encrypted secret store (per-row AAD binds the var
                      #   name; keyring REQUIRED, no plaintext mode) +
                      #   DatabaseSecretSource/load_secret_source, the boot
                      #   snapshot installed as the process secret source)
  secrets/            # Company-config whole-config encryption at rest:
                      #   the ENTIRE company_config payload is encrypted as
                      #   one opaque blob ({"__encrypted__": "enc:v1:…"}).
                      #   cipher.py (SecretCipher protocol + KeyringCipher
                      #   AES-256-GCM + enc:v1: envelope),
                      #   document.py (encrypt/decrypt_document wrapper +
                      #   store_config/load_config/redact_config/rekey_config
                      #   read/write helpers every payload consumer funnels
                      #   through), registry.py (structural secret_pointers over
                      #   the decrypted payload + redact_payload/restore_redacted
                      #   for display masking + safe write-back), keygen.py,
                      #   fake.py, resolver.py (SecretSource protocol +
                      #   install_secret_source/lookup_secret/
                      #   refresh_secret_snapshot — the process-wide source
                      #   config._resolve_env_value consults BEFORE
                      #   os.environ; store wins so a stale .env can't shadow
                      #   a rotated secret; see docs/concepts/secret-store.md);
                      #   keyring sourced from Tier A
                      #   BootstrapConfig.secrets (root of trust, never in DB —
                      #   no keyring = plaintext company_config storage,
                      #   opt-in; the secret_values store has no plaintext
                      #   mode and Tier A itself resolves with
                      #   use_store=False, since it carries the DSN + keyring
                      #   that open the store). See
                      #   docs/concepts/configuration.md#secrets and
                      #   docs/concepts/secret-store.md
  task/               # Task engine (models, tracker, escalation)
  seat/               # SEAT OWNERSHIP — which node runs which agent.
                      #   See docs/concepts/seat-ownership.md and
                      #   docs/guides/fleet.md.
                      #   placement.py — THE vocabulary node.roles /
                      #   node.labels / role.placement share with the
                      #   host; imports nothing from crewlet so config.py
                      #   and host.py can both depend on it. Owns the
                      #   capacity math, which is the part that is easy
                      #   to get wrong: placement is NOT a filter over a
                      #   fleet-wide fair share — 9 seats pinned to one
                      #   node and 1 free over 3 nodes gives ceil(10/3)=4
                      #   and strands 5 forever while every sweep reads
                      #   healthy. Share is per placement GROUP, over the
                      #   nodes eligible for it, summed; a node that does
                      #   not run seats is not in the denominator.
                      #   host.py (SeatHost: converge on
                      #   ceil(seats/live nodes) in BOTH directions —
                      #   claiming alone only converges for a fleet that
                      #   SHRINKS, so a node that booted alone would hold
                      #   every seat and scaling out would do nothing until
                      #   something died; the share is a ceiling, which is
                      #   what makes the give-back settle instead of
                      #   oscillating. `node:{id}` presence
                      #   leases as THE membership read — inferring the
                      #   count from seat ownership reads an unclaimed
                      #   fleet as zero nodes and every node then takes
                      #   every seat; claim/release rate limits because a
                      #   move costs an MCP spawn, not a lease (attach is
                      #   5 ms, measured); `preferred` ORDERS the claim and
                      #   never gates it — the hint outlives the node that
                      #   set it, so gating strands a dead node's seats
                      #   forever, and it cannot rank a node's OWN seats
                      #   (it names the last claimer) so the shed order is
                      #   plain sorted; may_start() is FRESHNESS not
                      #   membership — a renew at t proves exclusivity
                      #   through t+ttl, and a membership snapshot can be a
                      #   full TTL stale, i.e. exactly the window the check
                      #   exists to close; on_release carries a REASON
                      #   (voluntary quiesce-then-detach vs fenced
                      #   detach-first-abandon) and an unproven teardown
                      #   KEEPS the lease; on_admission fires on the renew
                      #   EDGE so the owner's consumer stops and restarts
                      #   with the store blip; renew()==False drops the seat
                      #   NOW, LeaseError keeps it — conflating them tears a
                      #   healthy company down over a DB blip);
                      #   watchdog.py (EventLoopWatchdog — a stalled loop
                      #   can't be signalled, so the thread's only real
                      #   move is os._exit: a wedged-but-alive node holds
                      #   its broker prefetch for the full 30-min ack
                      #   timeout while its leases lapse and peers take
                      #   over; exiting collapses that to 9 ms. Beat/poll
                      #   are SCALED to the threshold — a beat slower than
                      #   it makes a healthy loop shoot itself. A GONE
                      #   loop is not a WEDGED one — indistinguishable
                      #   from the thread (the beat just stops), opposite
                      #   situations: only a live loop still holds a
                      #   peer's mail. It records its loop and stands
                      #   down when that loop closed, else every engine
                      #   abandoned rather than stopped arms a TTL-long
                      #   suicide timer (it killed this repo's own suite
                      #   at 63%, exit 75, with zero test failures).
                      #   Armed by
                      #   the engine alongside the seat host and DISARMED
                      #   for the whole of shutdown, which is the one part
                      #   of the process that legitimately blocks the loop
                      #   — exiting through it abandons the seat release
                      #   that makes a drain graceful).
                      #   Constants are MEASURED:
                      #   docs/concepts/scaling.md § Where the constants
                      #   come from; the harness that re-measures them is
                      #   tests/test_queue/test_broker_behavior.py
  schedule/           # Scheduler — role/unit cron-style recurring work:
                      #   cron.py (5-field evaluator), scheduler.py (tick loop
                      #   + describe_schedules projection for the dashboard
                      #   /schedules view), store.py (scheduled_runs ledger)
  events/             # Event types, routing (subscriptions via EventQueue).
                      #   subscriptions.py resolves every recipient from
                      #   the ORG — role name or derived agent id → seat →
                      #   inbox — never from the local agent pool. Each
                      #   crewlet.events.* topic has ONE fleet-wide
                      #   consumer group, so the node that wins a delivery
                      #   is rarely the node running the recipient, and a
                      #   pool miss was a terminal drop (warn + ack), not
                      #   a retry
  knowledge/          # Shared-knowledge read — protocol.py (KnowledgeSearcher
                      #   Protocol + KnowledgeHit + AUTO_DRAFTED_PARENT/
                      #   AUTO_DRAFT_TITLE_PREFIX, the one seam the Plan
                      #   prefetch talks through; engine wires exactly ONE
                      #   backend, selected by integration presence),
                      #   accessibility.py (accessible_spaces +
                      #   accessible_projects org-wide scope),
                      #   confluence_search.py (ConfluenceSearcher — CQL),
                      #   plane_search.py (PlaneSearcher — workspace page
                      #   search: recency-ordered, skills-project + auto-
                      #   draft-parent exclusions, per-agent PLANE_API_KEY →
                      #   engine-client fallback; the server ANDs query
                      #   tokens, so on zero hits the searcher relaxes the
                      #   query full→4→2-token prefixes and pre-trims to the
                      #   server's 16-distinct-token cap),
                      #   markdown_docs.py (backend-neutral .md doc parsing +
                      #   render_markdown/html_to_text/frontmatter dump
                      #   shared by both import CLIs and page codecs)
  confluence/         # Confluence page WRITE side — pages.py (generic page
                      #   ops + ConfluencePublishError), knowledge.py
                      #   (knowledge-doc encode + crewlet-doc labels;
                      #   directory convention: space=parent dir, title=first
                      #   H1, optional frontmatter overrides only),
                      #   import_cli.py (unified `crewlet confluence
                      #   import`/`resync`: routes each .md — trigger:=skill,
                      #   otherwise knowledge doc in its parent-dir space;
                      #   --prune deletes orphaned import-managed skill pages
                      #   via pages.find_all_by_label/delete_page),
                      #   promotion.py (ConfluencePromotionWriter — the
                      #   Confluence PromotionPageWriter backend)
  learning/           # Agent-learning subsystem — ReflectEngine,
                      #   PersistDecider, AgentDiary, SkillSynthesizer/
                      #   Refiner, EpisodeStore + lifecycle,
                      #   CounterpartyProfiler, OnboardingMarkerStore,
                      #   relevant_knowledge.py (Plan-phase ## Relevant
                      #   knowledge prefetch over the KnowledgeSearcher
                      #   seam), skill_synthesizer.py (PromotionSynthesizer
                      #   + the consumer-owned PromotionPageWriter Protocol
                      #   — backends in confluence/promotion.py +
                      #   plane/promotion.py; SkillPromoted.container_key)
  providers/          # LLM + Embeddings protocols and implementations.
                      #   llm/cli_agent.py — the `cli-agent` provider type:
                      #   a locally installed coding CLI (claude / codex /
                      #   gemini / opencode / cursor-agent / copilot / grok)
                      #   driven headless on the operator's SUBSCRIPTION, no
                      #   API key. Pieces: cli_profiles.py (declarative
                      #   per-CLI data — argv, output paths, which files are
                      #   credentials vs memory, login commands; every field
                      #   overridable from YAML so flag drift is a config
                      #   edit, and `custom` needs no engine change),
                      #   cli_workspace.py (THE isolation boundary —
                      #   per-seat HOME/XDG/vendor dirs, allowlisted child
                      #   env (never os.environ), volatile-path prune per
                      #   generation, shared-credential seed + refresh
                      #   write-back), cli_protocol.py (messages+tools → one
                      #   prompt; JSON envelope → tool_calls — the CLI's own
                      #   tools are never used), cli_login.py (broker the
                      #   vendor's OAuth, capture a headless token, drive a
                      #   stdin credential login where one exists, export/
                      #   import the credential bundle), scope.py
                      #   (bind_llm_scope — the turn-scoped contextvar the
                      #   shared provider instance reads to pick a seat's
                      #   workspace). Operator surface: `crewlet llm`
                      #   (cli_llm.py). See
                      #   docs/concepts/subscription-llm-backends.md
  sandbox/            # Sandbox-as-a-tool code runtime: code work is the
                      #   run_sandbox Execute tool (tools/run_sandbox_tool.py)
                      #   — the executor calls it, the Execute loop SUSPENDS,
                      #   and the engine RESUMES the same loop with the result
                      #   spliced in when the detached run completes (no
                      #   separate completion turn). Pieces here:
                      #   protocol.py (Sandbox/SandboxProvider/
                      #   CodingAgentRunner protocols + RunHandle/specs/
                      #   results; pause/close + start/poll/collect),
                      #   manager.py (SandboxManager — acquire (install +
                      #   apply_setup)/teardown + reconnect for box reuse),
                      #   setup.py (declarative provisioning framework —
                      #   SandboxSetupStep (files/commands/env/brief),
                      #   apply_setup at acquire, environment_brief
                      #   env-context block for the coding-agent brief;
                      #   steps come ONLY from providers.sandbox.setup +
                      #   role.sandbox.setup — the engine ships none; the
                      #   git-auth recipe (scoped credential helper reading
                      #   the seat's code-host PAT + SSH→HTTPS insteadOf +
                      #   identity mapping from $CREWLET_AGENT_* + a brief
                      #   telling the agent to use the token) is config,
                      #   shipped in examples/nimbus.company.yaml (GitLab
                      #   form; the GitHub form is in
                      #   docs/integrations/github.md); setup
                      #   commands run WITH the run env so recipes read
                      #   engine facts at provisioning time),
                      #   credentials.py
                      #   (build_sandbox_env — tool-agnostic run env: LLM
                      #   creds from providers.llm + generic agent identity
                      #   as CREWLET_AGENT_HANDLE/_EMAIL + setup-step env +
                      #   role.sandbox.env (external tokens are DECLARED
                      #   there, e.g. GITHUB_TOKEN — the engine never names
                      #   a tool-specific var); ${VAR} refs resolving empty
                      #   log sandbox_env_unresolved),
                      #   pending_store.py (durable pending_sandbox_run rows
                      #   + at-most-once resume flip (running |
                      #   awaiting_clarification | reseed → resumed) +
                      #   execute_state JSONB = the suspended Execute
                      #   conversation; the row is also the box record —
                      #   sandbox_id non-empty ⇔ a box exists, paused_at set
                      #   ⇔ it is paused), coordinator.py
                      #   (SandboxCoordinator — completion → resume the
                      #   suspended Execute loop; pause-on-collect / reuse /
                      #   teardown-at-phase-end lifecycle; inbox-pause busy
                      #   gate + restart recovery, incl. reaping a `resumed`
                      #   tail abandoned by a dead engine),
                      #   waiter.py (SandboxWaiter — THE completion signal:
                      #   poll tick that detects a finished/dead job + doubles
                      #   as the running box's keepalive; reaps a vanished
                      #   sandbox after repeated connect failures so a lost
                      #   box can't orphan a run; PAUSE REAPER — E2B holds a
                      #   paused box forever and bills for the snapshot, so
                      #   each tick kills clarification pauses older than
                      #   pause_ttl (by id, never connect — that resumes) and
                      #   flips the run to `reseed`, which re-seeds from the
                      #   pushed branch on the eventual answer),
                      #   e2b.py (E2BSandboxProvider — real
                      #   sandboxes, cloud or self-hosted via domain; box
                      #   resources come from the TEMPLATE (build-time
                      #   cpu_count/memory_mb) — create takes none, so there
                      #   is no engine-side limits knob),
                      #   local.py (LocalSandboxProvider — the ENGINE HOST as
                      #     a backend, so code work can use the subscription
                      #     CLI login `crewlet llm login` established, with no
                      #     E2B account or API key. Two containments:
                      #     DirectSandbox (process tree; per-box HOME/XDG +
                      #     allowlisted env isolate STATE, not the host —
                      #     write_file refuses paths outside the box, and the
                      #     detached job is spawned with start_new_session so
                      #     one killpg reaches the tree AND asyncio reaps it,
                      #     since a zombie reads as alive to `kill -0`) and
                      #     ContainerSandbox (docker/podman, box bind-mounted
                      #     at /home/user so in-box paths match E2B; --init
                      #     reaps). SandboxSpec.credential_files carries the
                      #     CLI login in; a refreshed one is written back),
                      #   coding_agents/ (_detached.py DetachedFileRunner
                      #   base — start() runs the agent UNCAPPED (no timeout
                      #   kill) + closes stdin; poll() = done-marker OR
                      #   _result_done(streamed output) for an agent that
                      #   finishes but never exits (opencode#17516); sandbox
                      #   teardown reaps the husk; TTL = budget + buffer is the
                      #   only ceiling; collect() reconstructs transcript +
                      #   surfaces the exit code; runners self-configure the
                      #   LLM via CodingAgentLLM not env model;
                      #   claude_code.py ClaudeCodeRunner — headless
                      #   `claude -p` JSON, marker-driven; opencode.py
                      #   OpenCodeRunner — `opencode run --format json`
                      #   (streams line-flushed events: text .part.text +
                      #   terminal step_finish reason:stop (or session
                      #   .status:idle) → _result_done, since `run` hangs
                      #   without exiting); writes a custom
                      #   provider into opencode.json for a custom base_url
                      #   (addresses crewlet/<model>; key via {env:…};
                      #   share:disabled); ask.py — crewlet-ask clarification
                      #   shim + MCP scoping; a detached completion publishes
                      #   the redacted transcript as the Execute phase),
                      #   mcp_render.py (resolve_sandbox_mcp — role's scoped
                      #   MCP servers + creds → per-agent launch specs),
                      #   otel.py (SandboxOtelReceiver + per-run token store —
                      #   engine-fronted OTLP receiver; route
                      #   POST /otlp/{token}/v1/{signal}; wired via
                      #   CREWLET_SANDBOX_OTEL_RECEIVER_URL),
                      #   fake.py (in-process test fakes); the
                      #   per-role gate is role.sandbox, engine provider is
                      #   providers.sandbox (see docs/concepts/code-sandbox.md)
  notifications/      # External notification system (outbound transports;
                      #   transports/chat_threads.py — BACKEND-NEUTRAL thread
                      #   follow model (ChatThreadTracker + MentionGrammar);
                      #   each backend supplies only its mention grammar
                      #   (slack_threads.py `<@U123>` markup vs
                      #   mattermost_threads.py literal @username) and its
                      #   thread-id shape. Rows live in chat_thread_follows,
                      #   keyed by `backend` — thread ids are unique only
                      #   WITHIN a backend;
                      #   typing_status.py — backend-neutral WorkingStatusDriver
                      #   over a StatusPoster protocol; a poster declares its
                      #   backend, its refresh cadence (sized to that backend's
                      #   expiry) and supports_status_text: Slack renders free
                      #   text, a plain typing indicator does not, and where it
                      #   does not the phrase pools go inert;
                      #   coalesce.py — conversation keys + digest merging
                      #   for inbox batching; handle.py — party-level
                      #   HandleRegistry over agents ∪ human seats
                      #   (resolution + sender attribution; the engine
                      #   never sends to humans as itself);
                      #   transports/plane.py — PlaneTransport (inbound-only:
                      #   webhook dedupe + two-layer routing — directed
                      #   payload targets, subscriber fan-out via the engine
                      #   read client — project id→identifier cache, tool-
                      #   skills index-callback hook, excluded skills
                      #   project, project-lead fallback; send() no-op;
                      #   public client seam for page consumers:
                      #   new_user_client/engine_client/workspace/base_url +
                      #   resolve_project_ids, the shared identifier→UUID
                      #   primitive);
                      #   notification_prompts/plane.py —
                      #   PlaneNotificationPrompt (routed_via-keyed);
                      #   typing_status.py — Slack working indicator via
                      #   assistant.threads.setStatus (chat:write; Slack has
                      #   no public bot typing API — bolt-js#885):
                      #   TurnEngine-driven sessions keyed by
                      #   (handle, channel, thread_ts), refcounted by turn_id,
                      #   45 s heartbeat under Slack's 120 s status TTL, held
                      #   across a detached-sandbox suspend, cleared on reply /
                      #   skip / failure; gated by integrations.slack.typing_status
                      #   (addressed | always | off); wording = per-phase
                      #   pools (PHASE_PHRASES / StatusPhrases), one line
                      #   drawn per phase and held for it, overridden by
                      #   integrations.slack.status_phrases)
  mattermost/         # Mattermost integration (self-hosted OSS chat) —
                      #   client.py (async REST: bots, tokens, teams,
                      #   channels, posts, typing, the since= backfill read,
                      #     server_time_ms (the Date header — reconnect
                      #     windows compare SERVER-stamped post timestamps,
                      #     so "now" cannot come from the engine's clock)
                      #     + THE url helpers: normalize_base_url /
                      #     websocket_url / site_urls_match, one derivation
                      #     shared by config.py, the transport and doctor),
                      #   doctor.py (`crewlet mattermost doctor` — checks
                      #     what fails SILENTLY: ServiceSettings.SiteURL vs
                      #     the configured url (Mattermost accepts a
                      #     websocket only from a browser whose Origin
                      #     matches SiteURL exactly, and the engine sends no
                      #     Origin — so a mismatch blinds every human while
                      #     agents keep working), a browser-shaped upgrade,
                      #     and a REAL authenticated socket per seat),
                      #   events.py (MattermostEventFleet — ONE WEBSOCKET PER
                      #     AGENT SEAT, because Mattermost has no usable
                      #     inbound webhook: outgoing webhooks fire only in
                      #     public channels and carry no root_id / channel
                      #     type / mentions. Republishes each post onto the
                      #     standard raw_webhook envelope so everything
                      #     downstream stays webhook-shaped. Mattermost
                      #     replays nothing on reconnect, so each seat keeps a
                      #     cursor and re-reads its channels over the gap,
                      #     bounded to 15 min — a blip, not an outage),
                      #   provision.py + provision_cli.py (`crewlet mattermost
                      #     provision` — Plane/GitLab shape, NOT Slack's: no
                      #     manifest, no ledger, no OAuth click, because an
                      #     admin token mints a bot's PAT directly; the CLI
                      #     also hosts `doctor`).
                      #   Engine-side (MattermostConfig, MattermostTransport,
                      #   its prompt, identity registration) lives in
                      #   config/notifications like GitLab; the transport OWNS
                      #   the fleet so a live config swap rebuilds both.
                      #   See docs/integrations/mattermost.md
  slack/              # Slack app provisioning — `crewlet slack provision`
                      #   (one-app-per-agent automation via Slack's App
                      #   Manifest APIs): manifest.py (canonical BOT_SCOPES/
                      #   BOT_EVENTS + per-agent app manifest — the single
                      #   source of truth for the app shape), api.py
                      #   (SlackManifestClient — apps.manifest.* +
                      #   tooling.tokens.rotate + oauth.v2.access; 429s
                      #   waited out within a wall-clock budget), state.py
                      #   (slack-apps.json ledger + manifest fingerprints,
                      #   atomic 0600 writes), envfile.py (EnvStore — the
                      #   ONE .env read+written; file wins over shell
                      #   exports for managed keys; dotenv-round-trip-safe
                      #   quoting), provision.py (plans from
                      #   role.integrations.slack placeholders via
                      #   config.env_var_reference + run_provision
                      #   orchestration — per-agent failure isolation,
                      #   expiry-aware token rotation, created-app forced
                      #   install), provision_cli.py (CLI handler; the
                      #   OAuth code is pasted from the API's
                      #   GET /webhooks/slack-oauth landing page; see
                      #   docs/integrations/slack.md)
  tools/              # Agent tool system (builtins + A2A tools);
                      #   capabilities.py — tool classification from MCP
                      #   annotations (ToolAnnotations +
                      #   writes_to_shared_surface; keeps the engine
                      #   tool-stack agnostic, see
                      #   docs/concepts/tool-capabilities.md);
                      #   spawn_subagent_tool.py (ephemeral sub-agents);
                      #   run_sandbox_tool.py (the run_sandbox Execute tool —
                      #     gated on CheckContext.sandbox_enabled; launches a
                      #     detached coding run + suspends the loop)
  github/             # GitHub integration (per-role remote MCP)
  gitlab/             # GitLab integration — per-agent provisioning side:
                      #   client.py (async REST client), provision.py
                      #   (idempotent reconcile: service account + membership
                      #   + PAT + webhooks per agent seat; sinks/${VAR} scan
                      #   come from top-level provisioning.py, only the
                      #   GitLab seat scan seat_token_vars lives here),
                      #   provision_cli.py
                      #   (`crewlet gitlab provision`). Engine-side GitLab
                      #   (webhook route, parse_gitlab_webhook, identity
                      #   registration, GitLabConfig) lives in api/config/
                      #   notifications like GitHub; see
                      #   docs/integrations/gitlab.md
  plane/              # Plane integration (self-hosted fork) — REST-client
                      #   half: client.py (thin async X-API-Key client,
                      #   cursor pagination w/ strict completeness mode;
                      #   users/me, projects, work-item subscribers,
                      #   pages CRUD incl. archive_page +
                      #   external_id/external_source identity + fields=
                      #   projection, workspace page search,
                      #   service accounts + token lifecycle, webhook
                      #   CRUD, members/project-members, method-probe
                      #   capability checks),
                      #   provision.py (`provision()` — idempotent
                      #   reconcile: seats + memberships + tokens minted
                      #   into the config's own ${VAR} refs + crewlet-engine
                      #   account + webhook-secret capture + decommission;
                      #   pre-mutation capability preflight — stock-CE/
                      #   not-admin abort, degraded mode without the token-
                      #   lifecycle capability, page-echo detection;
                      #   drift = notes, report ends with the member table),
                      #   provision_cli.py (`crewlet plane provision` on the
                      #   existing plane CLI group; per-field url/workspace
                      #   resolution — never a full-model re-validation,
                      #   token/webhook_secret stay RAW as the minting
                      #   contract),
                      #   plane_codec.py (skill-page wire format over
                      #   description_html — leading YAML code block),
                      #   import_cli.py (`crewlet plane import`/`resync` +
                      #   PlanePublishError: external_id-keyed idempotency,
                      #   archive→delete prune, project pre-flight),
                      #   promotion.py (PlanePromotionWriter — ensure-exists
                      #   Auto-Drafted-Skills parent, access=public).
                      #   Engine-side Plane (PlaneConfig, /webhooks/plane
                      #   route, PlaneTransport + prompt, identity
                      #   registration) lives in config/api/notifications
                      #   like GitLab; see docs/integrations/plane.md
  mcp/                # MCP integration (stdio + HTTP/SSE)
  timescaledb/        # TimescaleDB event store (observability, in main PG)
  api/                # REST API + dashboard (Starlette). ONE wiring for
                      #   embedded and standalone: state comes from the
                      #   active config revision (config_refresh) and
                      #   events from subscribe_stream — never a
                      #   boot-time snapshot or a publish listener.
                      #   runtime.py — NodeRuntime, the single seam for
                      #   facts only a co-located engine can answer
                      #   (in-flight turns, drain state, live MCP tools,
                      #   config posture + applied epoch);
                      #   config_refresh.py — ConfigStateRefresher, the
                      #   cached projection's reconciler over the
                      #   activation pointer (refresh_if_changed = one tick,
                      #   run() = the loop). A MERGED node passes poll=False
                      #   and the engine drives the tick from its own
                      #   reconcile loop, so one process polls once and its
                      #   two halves can't disagree about the epoch;
                      #   auth guards EVERY route bar probes, webhooks
                      #   (HMAC), /otlp (signed token) and the dashboard
                      #   shell —
                      #   routes/ (per-domain handlers: agents, events,
                      #     tokens, org, stream, webhooks (Jira/Slack/GitHub/
                      #     GitLab/Plane/Confluence/Forge inbound, incl.
                      #     POST /webhooks/plane — X-Plane-Signature HMAC),
                      #     dashboard, health),
                      #   app.py, tokens.py (aggregation),
                      #   live_state.py (in-memory agent-state projection +
                      #     in-flight live_call, so state survives a browser
                      #     refresh without per-read DB scans; + active-
                      #     sandboxes set from the SandboxRun* events →
                      #     dashboard Running-sandboxes panel; + last_error
                      #     and a frozen failed live_call so a stopped seat
                      #     says WHY; + the live token rollup (records kept
                      #     in the window, aggregated through api/tokens.py —
                      #     one implementation, never a second one in JS);
                      #     apply_event returns a Change naming what moved),
                      #   streaming.py (StreamService: ingest → projection +
                      #     /ws/stream fan-out + one shared health tick +
                      #     DERIVED PUSHES — agents/sandboxes/tokens
                      #     envelopes carry the RESULT of applying an event,
                      #     so a dashboard mirrors the projection instead of
                      #     re-deriving it),
                      #   queries.py (the /ws/stream request/response channel:
                      #     agent / agent_memory / event / events / trace /
                      #     tokens / schedules / config* — each a thin adapter
                      #     over the SAME function the REST route calls, so
                      #     the two surfaces cannot answer differently;
                      #     config* gated by auth.resolve_operator);
                      #   serves the dashboard from the top-level static/
                      #     (see static/ below)
  static/             # Web assets served by the API. static/dashboard/ is
                      #   the zero-build modular ES-module dashboard.
                      #   THE WEBSOCKET IS THE ONLY DATA CHANNEL: socket.js
                      #   (pushes + a query(what, params) Promise channel;
                      #   the REST snapshot is degraded-mode only), store.js
                      #   (a MIRROR of the server projection — it derives
                      #   nothing, and wakes subscribers per SLICE so a
                      #   health tick does not redraw the page), patch.js
                      #   (keyed in-place DOM patching — rendering with
                      #   innerHTML on every envelope is what made the page
                      #   strobe; every repeated row needs a data-k),
                      #   scheduler.js (rAF coalescing), records.js (ONE
                      #   normalizer for an LLM record, pass-through rather
                      #   than a field whitelist — four hand-maintained
                      #   whitelists are how a phase failure got deleted on
                      #   its way to the screen), pulse.js (THE COMPANY
                      #   PULSE — the overview's lead panel: one row per
                      #   seat, one cell per minute of the last hour, lit
                      #   by real feed events, red where the server's
                      #   `failed` flag says so. buildPulse is pure and
                      #   runs ONCE per render, threaded through to the
                      #   hero grid AND every seat card's strip so the two
                      #   cannot disagree about a seat), health.js (THE
                      #   ENGINE HEALTH SURFACE — a popover on the live
                      #   dot, plus the two conditions that escalate into
                      #   always-on chrome because they must never wait
                      #   for a click: a dead socket, and an engine with
                      #   NO ACTIVE COMPANY CONFIG, which discards every
                      #   inbound webhook and used to render exactly like
                      #   a healthy idle one. Booleans from the engine
                      #   are read THREE-VALUED — losing the socket
                      #   clears the health slice, so `=== false ? bad :
                      #   good` renders "Configuration: active" on a page
                      #   that cannot see the engine; the dot's
                      #   status→class map is a table for the same
                      #   reason. emptyOrPending in ui.js is where
                      #   socket-down / nothing-configured / genuinely-
                      #   empty are told apart once, for every list view).
                      #   Views are pure
                      #   render(state) -> markup + a `slices` list;
                      #   hash router, per-view modules; turnRail() in ui.js
                      #   draws an in-flight turn as an object (phases spent
                      #   / live / pending, packet on the live segment);
                      #   staleness() in state.js is why MOTION STOPS WHEN
                      #   WORK STOPS — pips that keep pulsing on a hung turn
                      #   claim progress that is not happening; llm.js renders each
                      #   LLM invocation with
                      #   collapsible, height-capped prompt messages so a long
                      #   system prompt cannot bury the response, plus a
                      #   Source chip/block naming the event that triggered the
                      #   turn — notification triggers show a branded
                      #   integration badge (Slack/Jira/…) + sender via
                      #   describe_trigger. llm.js also OWNS the
                      #   `<think>` grammar the engine writes
                      #   (events.types.format_reasoning_and_content):
                      #   responseBody turns it into a Reasoning block
                      #   inline with the tool badges, and anything
                      #   wanting the plain text (row previews, the
                      #   overview live card) calls stripThink instead of
                      #   re-writing the regex. It renders a LIVE row and a
                      #   finished one identically because the engine
                      #   builds both responses with ONE function — a live
                      #   record's toggle identity is records.js `_key`
                      #   (timestamp-free), since updated_at moves every
                      #   round and would re-open what the reader
                      #   collapsed; eventDetail.js renders inbound
                      #   notifications as a readable integration-branded view
                      #   (state.js integrationMeta/integrationBadge) — see
                      #   describe_trigger / turn-engine.md).
                      #   Visual system = the crewlet.io panel language:
                      #   PURE BLACK ground, and every division on it is a
                      #   different ALPHA OF ONE WARM CREAM (panel fill,
                      #   hairline, inset) — that single material is what
                      #   makes a dense surface read as one object. The
                      #   brand gradient is used as LIGHT (a hairline on the
                      #   hero's top edge, the rail packet), never as a fill.
                      #   styles/tokens.css is the ONLY place a colour is
                      #   defined (surface/border/text ramps, glass, the
                      #   type stack + the three tracking tokens, the brand
                      #   gradient/ramp, the panel/card/lift shadows, and
                      #   8 categorical hue
                      #   families each shipping a --<hue> MARK step and a
                      #   --<hue>-ink TEXT step; --red is a reserved status
                      #   hue). One shared panel recipe in components.css
                      #   backs .card/.list/.widget/.stat/.tool-card/.turn/
                      #   .mem-card — views never re-declare a surface.
                      #   Phase + event-category hue assignments (state.js
                      #   PHASE_HUE / EVENT_CATEGORIES) are validated for
                      #   CVD + normal-vision separation in BOTH themes —
                      #   re-verify before changing one.
                      #   Screens: Dashboard, Company (Overview / People
                      #   Directory / Org Chart / Audit log), Agents,
                      #   Activity, Tokens, Tools, Schedules, Fleet,
                      #   Configuration.
                      #   A seat's raw event list belongs to Activity ALONE
                      #   (#/events?actor=<role> seeds its actor filter);
                      #   the agent page links there rather than rendering
                      #   a second copy below its turns.
                      #   Fleet reads the LEASE TABLE, not a fan-out of
                      #   /health probes: /health answers about the node
                      #   that served it, so behind a load balancer a
                      #   refresh tells a different story.
                      #   EVERY nav entry is backed by a real endpoint — no
                      #   placeholder screens. org.js flattens the /org tree
                      #   into SEATS (unit chain + effective lead + inherited
                      #   mcp_env) — views consume seats, never the raw
                      #   payload; cards.js renders the shared seat card
                      #   (agents + human seats), with state.js statusLine
                      #   deriving "what it is doing" from live state only.
                      #   See docs/reference/dashboard-design.md
  extensions/         # Extension system
tests/                # Mirror structure of src/crewlet/, plus three that
                      #   are exceptions. test_dashboard/ mirrors the
                      #   dashboard, which is JavaScript, so its suites
                      #   are ES modules under test_dashboard/js/ (a
                      #   three-function harness + a vendored DOM, no npm
                      #   and no build step) run under whatever `node` is
                      #   on PATH by a pytest wrapper that SKIPS when
                      #   there is none. test_scripts/ mirrors scripts/,
                      #   which is shell, so its suites EXTRACT the pure
                      #   helper functions and source them into a real
                      #   bash, and assert the rest (what may never reach
                      #   argv, what mode a file is created with) against
                      #   the source — the same static shape
                      #   tests/test_examples/test_local_stack.py uses
                      #   for the Plane bootstrap. tests/test_fleet/
                      #   mirrors nothing: it runs TWO Engines against
                      #   one broker and one lease table and gates the
                      #   seat-ownership exit criteria (handoff preserves
                      #   order; a node that lost its lease starts no
                      #   turn while still attached; a completion reaches
                      #   only the owner; an unclaimed seat's mail
                      #   survives). Parametrized over the memory twin
                      #   AND a real Pulsar — "the same suite passes on
                      #   the twin" is itself a criterion, so a twin that
                      #   models the broker wrongly fails there instead
                      #   of certifying the bug in CI
examples/             # Working examples (the Nimbus example org:
                      #   nimbus.config.yaml + nimbus.company.yaml +
                      #   nimbus-docs/ + tool-skills/). The MINIMAL
                      #   example is the quickstart's inline config, not
                      #   a file here — one canonical minimal company,
                      #   guarded by tests/test_examples/test_docs_configs.py
schema/               # GENERATED JSON Schema for both config tiers
                      #   (company.schema.json + bootstrap.schema.json).
                      #   Emitted by `crewlet schema <tier> -o <path>`
                      #   from the Pydantic models; a test regenerates
                      #   and compares, so never hand-edit — change the
                      #   models and re-run. Consumed by editors (the
                      #   `# yaml-language-server: $schema=` modeline),
                      #   CI, and AI-assisted authoring.
skills/               # FOUNDER-facing authoring skills for an AI
                      #   assistant (company-architect/SKILL.md —
                      #   interview script, config invariants, the
                      #   write→validate→fix loop). NOT the same thing
                      #   as examples/tool-skills/, which are the
                      #   knowledge-base-sourced fragments the engine's
                      #   own agents load at runtime. See
                      #   docs/getting-started/ai-authoring.md
scripts/              # Repository tooling — NOT shipped in the wheel
                      #   (the sdist carries it). gitlab-dev-bootstrap.sh,
                      #   plane-dev-bootstrap.sh + mattermost-dev-bootstrap.sh
                      #   stand up the local integration loops (each pairs
                      #   with a profile-gated service in docker-compose.yml); release_metadata.py is the
                      #   version/tag logic the release
                      #   workflow runs on the tag, kept here rather than
                      #   inline in the YAML so tests/test_packaging can
                      #   exercise it on every PR. Stdlib-only: it runs
                      #   on a bare checkout, before any install
```

## Pre-commit Checks
Before committing, ALWAYS run and fix any issues from:
- `ruff check src/ tests/` — lint check
- `ruff format --check src/ tests/` — format check

If either fails, fix the issues (use `ruff check --fix` and `ruff format`) before committing. These are the same checks CI runs.

If the change touches `pyproject.toml`, `README.md`, or any non-`.py` file under `src/crewlet/`, also run the packaging build CI runs on every PR — a break there is otherwise invisible until the tag that publishes it:
- `python -m build && twine check --strict dist/*`

## Dependency Updates
Dependabot watches every dependency surface in the repo — `.github/workflows/*.yml` (`github-actions`), `pyproject.toml` (`pip`), and `docker-compose.yml` (`docker-compose`). The config is `.github/dependabot.yml`, and it is deliberately three entries on a weekly schedule with no further knobs; keep it that way unless a knob earns its place.

- **A new dependency surface ships with its `updates:` entry.** Adding the first `Dockerfile`, a `package.json`, a `go.mod` — the Dependabot entry is part of that change, not a follow-up. Nothing fails when it is missing; the surface is just never watched, which looks exactly like a surface with nothing to update.
- **Actions pinned to a non-version ref are invisible to Dependabot.** A branch pointer (`@release/v1`) or `@main` yields no update PRs at all, so pin actions to a version tag or a full SHA. Maintainer-facing detail is in `CONTRIBUTING.md`.

## Packaging & Releases
The package is published to PyPI as `crewlet` from a `v*` tag via `.github/workflows/release.yml` (Trusted Publishing over OIDC — no API token in the repo). See `RELEASING.md`.

- **The version lives in exactly one place**: `__version__` in `src/crewlet/__init__.py`. `pyproject.toml` reads it via `[tool.hatch.version]`. Never add a literal `version =` back.
- **Version numbers follow semver** — the minor number moves for features, the patch number for fixes. The tag must name the version `__init__.py` reports; the workflow refuses to build when they disagree.
- **Pushing the tag is the whole release** — the workflow publishes to PyPI and then creates the GitHub Release itself, with notes GitHub generates from the merged pull requests (grouped by `.github/release.yml`) and the sdist + wheel attached. Those generated notes are the only description a release gets, so pull request titles carry it. Pre-release and dev versions are flagged so they never become GitHub's "Latest". The version and tag checks live in `scripts/release_metadata.py`, not inline in the workflow, so `tests/test_packaging` runs them on every PR.
- **`README.md` keeps repo-relative links** (GitHub and docs.crewlet.ai resolve them); `hatch-fancy-pypi-readme` absolutises them for PyPI. A new link form the patterns miss fails `tests/test_packaging`; fix the pattern in `pyproject.toml`, not the README.
- **No `License ::` classifier** — `license = "MIT"` is an SPDX expression, and PEP 639 requires PyPI to reject a distribution carrying both.
- **Python classifiers must match the CI matrices** exactly — and every CI job that fans out over interpreters (`test`, `package-install`) must name the same list. `crewlet[all]` must stay the union of the runtime extras. All asserted by `tests/test_packaging`.

## Testing
- Run tests: `pytest`
- Run linter: `ruff check src/ tests/`
- Every subsystem needs unit tests
- Use mock LLM providers in tests (never call real APIs in tests)
- Test files mirror source: `src/crewlet/queue/protocol.py` → `tests/test_queue/test_protocol.py`

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
1. **Event Queue** — persistent pub/sub via Apache Pulsar (EventQueue protocol)
2. **Organization Model** — hierarchy is the execution graph
3. **Agent Runtime** — queue-driven callback handlers, LLM execution loop
4. **Task Engine** — ExecutionTracker, external PM tool integration
5. **Decision Framework** — DACI behavioral guidance (via Slack channels, no dedicated engine)
6. **Knowledge System** — query-time knowledge-base search for shared docs (Confluence CQL or Plane page search, one backend per org) + per-agent `agent_diary` (pgvector)
7. **Communication** — external chat (Slack or Mattermost, or both) + ephemeral A2A channels
8. **Notification Service** — EventQueue-based, outbound-only transports
9. **Provider Layer** — pluggable LLM, embeddings. Includes the `cli-agent` LLM type: a locally installed coding CLI driven on the operator's subscription instead of an API key, with per-seat filesystem isolation and an in-prompt tool-call envelope (see `docs/concepts/subscription-llm-backends.md`)
10. **Database** — PostgreSQL (token_usage, agent_diary + episodes via pgvector)
11. **Tool Registry** — builtins + MCP tools + A2A tools
12. **Tool Skills** — knowledge-base-sourced prompt fragments injected per-phase based on the active tool / MCP surface (`agent.skills`; see `docs/concepts/tool-skills.md`)
13. **Code Sandbox** — per-role sandboxed coding-agent Execute backend (`crewlet.sandbox`; E2B, or the engine host via `type: local` — `direct`/`container`; see `docs/concepts/code-sandbox.md`). Artefact paths are per-sandbox (`Sandbox.home` → `RunPaths`), never module constants — many local boxes share one filesystem
14. **API** — standalone Starlette process (EventQueue)
15. **Extension System** — hooks and middleware
16. **Scheduler** — role/unit-scoped cron-style recurring work; fires `TaskAssigned` on a schedule with at-most-once delivery (`scheduled_runs`), missed-tick catchup, and a per-task wall-clock cap (`crewlet.schedule`; see `docs/concepts/scheduling.md`)
