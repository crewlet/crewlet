# Configuration Reference

Crewlet companies are defined across **two tiers** — see the [Configuration concept page](../concepts/configuration.md) for the full design.

- **Tier A** (`crewlet.yaml`, restart-only): this node's identity and roles, the store's files, the stream and coordination slots, API host/port/auth, the secret keyring, logging (level, shape and an optional rotating log file).
- **Tier B** (`company.yaml` imported into the store, live-editable): everything else — identity, providers, integrations, MCP servers, roles, units, turn engine, learning, budgets, scheduling.

This page documents the **Tier B** fields below.  For Tier A see [Configuration concept page §"Tier A example"](../concepts/configuration.md#tier-a-example-crewletyaml).

> **Machine-readable version.** `crewlet schema company` emits the JSON
> Schema for everything on this page, generated from the models
> themselves — the same files the repository commits under `schema/`,
> which `make schema` regenerates whenever a model changes, so a field
> named here and a field the schema accepts cannot drift. Point your
> editor at it for autocomplete and typo
> squiggles, or hand it to an AI assistant — see
> [Authoring with an AI assistant](ai-authoring.md). Check your file with
> `crewlet validate <file>` (add `-json` for located, classified problems
> and warnings, see [the validation loop](ai-authoring.md#the-validation-loop));
> it reads no environment, so it works before any secret is exported.
>
> **Credential positions are marked.** Every field, list member or map
> value that holds a credential carries `"x-crewlet-secret": true` in the
> schema, so an editor or a tool writing this file knows where a `${VAR}`
> reference belongs instead of a value — see
> [Credential positions](../reference/cli.md#credential-positions-x-crewlet-secret).
>
> **Unknown keys are rejected.** Every config model forbids extra
> fields, at every level including roles and units — a mistyped
> `backstroy:` fails validation naming the exact path rather than being
> silently ignored.

---

## Top-Level Fields (Tier B)

```yaml
name: "Acme AI Corp"                    # required — company name
mission: "Build intelligent products"   # optional — company mission
vision: "Lead the AI industry"          # optional — company vision
timezone: Europe/Berlin                 # optional — the company's ONE clock, an IANA zone
                                        #   (default UTC). See "The company's clock" below.
token_budget:                           # optional — org-wide token ceilings, one per calendar
  day: 3000000                          #   window on the company clock, each optional; an
  month: 40000000                       #   absent window is uncapped. See "Token budgets" below.
notification_rate_limit: 10             # optional — inbound notifications one seat may be woken by
                                        #   per second. 0 (the DEFAULT) is unlimited, so the valve is
                                        #   off unless you set it. Drop-based safety valve against
                                        #   webhook storms and notification loops — burst handling is
                                        #   inbox coalescing below, which batches instead of dropping.
                                        #   Fails OPEN: a valve that cannot reach its counter passes
                                        #   the notification rather than swallowing it.
notification_coalesce_window_seconds: 0 # optional — inbox linger window (seconds) absorbing bursts
                                        #   before an idle agent's turn starts. 0 (default) adds no
                                        #   latency: backlog that piled up while the agent was BUSY
                                        #   still coalesces into one digest turn per conversation.
                                        #   Max 60 — the window counts against the broker ack-timeout
                                        #   budget bounding a message's unacked lifetime.
                                        #   See concepts/event-system.md § Inbox batching.
notification_coalesce_max_batch: 20     # optional — events collected into one DRAIN before it is
                                        #   partitioned by conversation, 1..100 (default 20). It
                                        #   bounds a digest as a consequence — a digest cannot exceed
                                        #   a drain — and it bounds the drain, which has to fit the
                                        #   ack budget alongside a whole turn. A drain spanning
                                        #   several conversations shares the cap between them, so
                                        #   raise it for seats that routinely serve several busy
                                        #   threads at once.

policies:                               # optional — org-wide policies (full text renders into the executor's prompt)
  - "All code must be reviewed before merging"
  - "Communicate decisions in writing"

workers: {...}                          # optional — reusable delegate templates (see below)

roles: [...]                            # optional — root-level org-wide agents (see below)
units: [...]                            # optional — org structure tree (see below)

assistant: {...}                        # optional, Ask Crewlet; off unless enabled
                                        #   (see "Ask Crewlet" below)

turn_engine:                            # optional — executor/reviewer turn config
  max_iterations: 3                     # hard cap on self_iterate loops per turn
                                        #   (an executor run's own rounds are capped by
                                        #   role.sandbox.max_turns / providers.sandbox.
                                        #   default_max_turns: the run is its CLI's loop)
  onboarding_max_tool_rounds: 10        # dedicated first-turn onboarding pass (0 = disabled)
  delegation:                           # bounds on every `delegate` call — see Turn Engine § Workers
    max_parallel: 3                     # workers running at once within one call
    max_tasks_per_call: 8               # tasks one call may contain; over it the call is REFUSED
    max_turns: 20                       # tool rounds one worker may run (a request above it is clamped)
    max_turns_ceiling: 40               # highest max_turns a worker template may declare
    budget_fraction: 0.2                # ONE slice for the whole call, shared by every task in it
    min_tokens_per_task: 500            # call refused up front if the per-task share falls below this; 0 = no floor
    task_timeout_seconds: 300           # wall-clock cap on one worker
    call_timeout_seconds: 900           # wall-clock cap on the whole call, dependency waves included
  executor_min_budget_tokens: 25000     # refuse an executor launch, and park the seat, below
                                        #   this remaining budget (0 = no floor). A seat's or
                                        #   the company's token_budget ceiling below it is
                                        #   refused: no launch could ever fit
  delegation_depth_limit: 3             # max colleague-handoff chain depth before depth_cap guard breach
  extension_enabled: true               # round-cap extension judge for the onboarding pass
                                        #   (an executor run is its CLI's loop, which no
                                        #   judge extends)
  onboarding_max_tool_rounds_ceiling: 20  # hard ceiling for onboarding rounds across extensions (2x base 10)
  extension_round_step: 8               # max rounds the judge may grant per extension call
  conversation_session:                 # what this seat already said in ONE thread / issue / PR,
                                        #   carried into that conversation's next turn
                                        #   (see concepts/conversation-sessions.md)
    enabled: true                       # feature gate — a live kill switch; off restores the
                                        #   pre-ledger prompt exactly
    max_entries: 20                     # entries KEPT per conversation, trimmed at write
                                        #   time. A second bound applies at RENDER time:
                                        #   the injected block drops whole oldest entries,
                                        #   saying how many, once it would exceed 24k
                                        #   characters
    retention_days: 30                  # matches the event store's horizon; applied at next start

learning:                               # optional — agent-learning subsystem
                                        #   Every pass here calls the seat's auxiliary
                                        #   model (`llm_auxiliary`), a coding CLI like
                                        #   every model, under a FIXED deadline — 60 s
                                        #   after a turn, 30 s for a turn-start
                                        #   briefing — so point that key at a fast,
                                        #   cheap model (see concepts/llm-backends.md
                                        #   § Auxiliary work and latency)
  enabled: true                         # master switch (auto-disables without DB + embeddings)
  episodic:
    retrieval_limit: 5                  # default hits query_episodes returns when the
                                        # model names no limit (1-20)
  reflect:
    enabled: true                       # ReflectEngine + reflect_and_persist tool
    persist_decider: true               # run the post-turn PersistDecider on every turn
    summarize_episodes: true            # cheap-model briefing of the turn-start
                                        # Similar prior work hits (query_episodes is
                                        # never summarised)
  counterparty:
    enabled: true                       # CounterpartyProfiler + auto-inject + lookup inline
  skill_synthesis:
    enabled: true                       # SkillSynthesizer (single-turn + clustered)
    min_tool_calls: 5                   # single-turn trigger threshold
    max_skills_per_agent: 50            # hard cap; once reached the synthesizer no-ops
    duplicate_jaccard_threshold: 0.7    # reject near-duplicates of existing skills
    # Clustered synthesis (opt-in). A fleet singleton: one node runs the tick.
    scheduler_enabled: false            # set true to enable the background tick
    scheduler_interval_seconds: 3600    # seconds between ticks. Hourly is cheap:
                                        #   a tick is one indexed scan per seat
                                        #   and only calls the model when a NEW
                                        #   cluster qualified
    cluster_window_hours: 168           # look-back window (7d). Bounds the QUIET
                                        #   seat, whose last 200 turns can span a
                                        #   year; episode_fetch_limit bounds the
                                        #   busy one
    cluster_min_size: 3                 # turns that must converge before a
                                        #   cluster earns a skill. Two is a
                                        #   coincidence
    cluster_jaccard_threshold: 0.6      # similarity that pools two turns. Lower
                                        #   than duplicate_jaccard_threshold on
                                        #   purpose: pooling asks "same kind of
                                        #   work", rejecting asks "same skill"
    episode_fetch_limit: 200            # max episodes pulled per agent per tick
  skill_refinement:
    enabled: true                       # both halves: the post-turn refiner AND
                                        # the refine_skill tool. false withdraws
                                        # the tool too — they write the same rows
    auto_refine_on_success: true        # append "Observed in practice" on done
    auto_refine_on_failure: true        # append "Counter-example" on failed.
                                        # self_iterate is never refined: the turn
                                        # is not settled yet. Both false leaves
                                        # the refiner unbuilt
    max_body_chars: 20000               # a refinement whose result exceeds this
                                        # is refused, not truncated
    max_versions_kept: 10               # history retention per skill (older pruned)
  skill_promotion:
    enabled: true                       # daily cross-agent promotion pass. Needs a
                                        #   knowledge base (integrations.confluence)
                                        #   — a promoted skill is a draft
                                        #   page a unit lead reviews
    min_sibling_count: 3                # distinct SEATS that must converge. One seat
                                        #   with four similar skills is a catalogue
                                        #   to curate, not a team practice
    jaccard_threshold: 0.6              # similarity that pools two seats' skills
  skill_curator:
    enabled: true                       # age unused skills out: active → stale → archived.
                                        #   Pinned skills are exempt; an archived skill stays in
                                        #   the table, and restoring one is a state change
    interval_hours: 24                  # how often the curator walks the skill table
    stale_after_days: 30                # unused days before a skill goes stale
    archive_after_days: 90              # unused days before it is archived (≥ stale_after_days)
  personal_memory:
    max_refreshes_per_turn: 3           # cap on distinct context_hint values per turn
                                        # (idempotent repeats of the same hint are free)
  episode_lifecycle:
    # Trigger: the background pass counts each seat's raw rows on its own
    # tick — one indexed query per seat — and compacts whoever is over.
    max_raw_episodes_per_agent: 500     # threshold that fires CompactionRequested
    # Action 1: drop non-terminal mid-state rows past this age.
    non_terminal_max_age_days: 14
    # Action 1b: drop raw turns that called no tools past this age — the one
    # bound on a chat-only seat's rows, which compaction cannot cluster.
    tool_free_max_age_days: 90
    # Action 2: drop skill-consolidated rows past this grace.
    consolidated_grace_days: 30
    # Action 3: cluster + LLM-compact remaining raw rows past this age.
    compaction_min_age_days: 30
    compaction_min_cluster_size: 3      # singletons / pairs left raw
    compaction_jaccard_threshold: 0.6   # tool-sequence similarity to pool a cluster
    compaction_batch_size: 200          # max raw rows pulled per pass
    exemplar_count: 2                   # raw rows kept per cluster for drill-down (at least 1)
    # Action 4 (optional): evict ancient compacted entries for hard storage caps.
    compacted_max_age_days: 0           # 0 = disabled
```

Per-role auxiliary model (for reflection + episode summarisation) and
extension-judge model:

```yaml
roles:
  - name: Engineer
    llm: sonnet                         # the seat's own model — what the executor runs on
    llm_auxiliary: haiku                # cheap/fast model for reflection
    llm_judge: haiku                    # cheap/fast model for the round-cap extension judge
```

Each names a `providers.llm` key (see [Providers](#providers)). Both satellite
calls run under a fixed deadline and pay a CLI launch each, which is why they
want a fast model rather than the seat's own — see
[Auxiliary work and latency](../concepts/llm-backends.md#auxiliary-work-and-latency).

`llm_judge` is invoked when the first-turn onboarding pass exhausts its tool-round cap;
it decides whether the pass is making progress (extend) or thrashing
(fall through to rescue).  Falls back to `llm` -> `"default"` if unset. An
executor run is its coding CLI's own loop, bounded by `role.sandbox.max_turns`
(else `providers.sandbox.default_max_turns`), which no judge extends.
See the [Turn Engine extension judge](../concepts/turn-engine.md#round-cap-extension-judge)
section for details.

See the [Turn Engine](../concepts/turn-engine.md) and [Agent Learning](../concepts/agent-learning.md) docs for what each field controls.

### The company's clock

```yaml
timezone: America/Los_Angeles            # an IANA zone name; absent is UTC
```

`timezone` is the company's **one clock**, and every calendar edge the engine cuts is cut on it:

- where **today**, **this week** and **this month** begin — for a `due=` filter, the due bands on a board, the overdue mark on every row, a person's own day (`my_work`) and the workload's overdue counts;
- what a relative date (`tomorrow`, `eow`, `+7d`) or an all-day due date resolves to, whether a seat, an operator's assistant or the dashboard wrote it;
- the wall clock a [schedule](#schedules) that names no `timezone` of its own fires on — so `cron: "0 9 * * 1-5"` is 09:00 in this zone.

It is a clock for **authored instants and calendar boundaries only**. No duration — a lease, a timeout, a retention horizon — is measured against it, because a duration measured on a wall clock changes length twice a year. And there is only one: the tracker and the scheduler take no zone of their own, because a company whose board, whose people's days and whose standups each kept a clock had one "today" per subsystem. A schedule's own `timezone` is that one piece of work's wall clock — the Tokyo team's 09:30 standup — and nothing else is cut on it.

The value must be a real IANA name (`Europe/Berlin`, `America/New_York`, `UTC`), and `Local` and `localtime` are refused: each is whatever zone the host reading it is set to, so two nodes would cut two different days from it. The [`org` projection](../reference/api-endpoints.md) carries the resolved clock to every reader — `UTC` where none is written — so the dashboard cuts its days where the engine does. An apply that changes it moves the next answer, the next scheduler tick and the next tool call; a turn already running keeps the clock of the epoch it started on.

### Token budgets

```yaml
token_budget:                            # on the company, and on any agent seat
  day: 3000000                           # one local day, midnight to midnight
  week: 10000000                         # one ISO week, from Monday
  month: 40000000                        # one calendar month
```

A token budget is a **ceiling per calendar window**, and every window is cut on the
[company's clock](#the-companys-clock): a day runs from local midnight to the next, a week
is the ISO week from Monday, a month is the calendar month. Each key is optional and the
windows are independent — a model round is admitted only while **every** capped window has
room for it, and each window opens again on its own when it turns over. There is no single
number any more: one number was a ceiling for the life of the deployment, which nothing
reset but an operator zeroing a counter, and `token_budget: 10000000` is refused with the
mapping to write instead.

The company's `token_budget` caps every seat's spend together. A seat's own caps that seat
alone, **on top of** the company's: every round is charged to both, so a seat is stopped by
whichever ceiling it reaches first. A human seat spends nothing, so a `token_budget` on one
is refused.

On a running company both are part of the company document and change through `/config`
— a seat's on its own with `PUT /config/roles/{handle}` — and each node enforces them once
it has applied that revision. See
[Budgets and spend](../guides/budgets-and-spend.md#budget-windows).

An **absent key is the only way to leave a window uncapped.** A ceiling of `0` or less is
refused (`must be at least 1 token … remove token_budget.day for no daily ceiling`): `0`
used to mean "unlimited", and it is also what a ceiling of nothing would be, so it means
neither. Stopping a seat on purpose is not a budget's job. A key that is not a window —
`year` — is refused too, naming the key to remove. A company file and a `/config` write each refuse
both, and a bare number is refused over JSON as well
as YAML. A ceiling below `turn_engine.executor_min_budget_tokens` (25000 by
default) is refused too, on a seat that runs an executor and on the company,
since no launch could ever fit under it; and a seat whose window leaves less
than that floor waits [parked](../concepts/agent-runtime.md#the-budget-park)
until it turns over.

[`crewlet validate`](../reference/cli.md) **warns** — it never refuses — about a ceiling that
can never refuse a turn, because another ceiling is always reached first:

- a day ceiling at or above the week's or the month's (a day lies inside both);
- a week ceiling that seven days at the daily ceiling already reach, or a month ceiling that
  31 days at the daily one, or six weeks at the weekly one, already reach;
- a week ceiling at or above the month's, which can then bind only in a week that straddles
  two months;
- a seat's ceiling at or above the company's for the same window, or for a longer window
  that holds it (a seat's day against the company's month) — everything a seat spends is
  the company's spend too. Every `/config` write's answer carries the same warning.

Spend is counted in the fleet's [coordination store](../concepts/coordination.md#token-budgets-are-windows),
one figure per window, so it survives restarts and is one number for the whole company however
many nodes run it — and a window nothing caps is counted too, so a ceiling added mid-window
judges the spend already in it. A seat whose capped window is refusing is not handed work: its
mail waits on its inbox until the window turns over or its ceiling is raised (see
[the budget park](../concepts/agent-runtime.md#the-budget-park)). There is **no reset**: a
window's allowance comes back when the window turns over, and room before then is made by
raising its ceiling. See [Deployment § Token Budgets](../guides/deployment.md#token-budgets)
for how a refusal is recorded and reported.

### Scheduling

System-level knobs for the [Scheduler](../concepts/scheduling.md) — the
cron analogue that fires role/unit `schedules:`. The scheduler
auto-enables when `enabled` is true, a database is configured, and the
org declares at least one schedule.

```yaml
scheduling:                              # optional — role/unit scheduled work
  enabled: true                          # master switch
  tick_seconds: 10                       # scheduler poll interval
  jitter_seconds: 0                      # max per-schedule spread to smooth a shared cron minute
  catchup_min_seconds: 120               # lower clamp on the missed-tick catchup window (at least 1)
  catchup_max_seconds: 7200              # upper clamp on the missed-tick catchup window
```

A schedule that names no `timezone` of its own fires on the company's
[clock](#the-companys-clock); there is no scheduler-wide default zone.
See the [Scheduling](../concepts/scheduling.md) concept doc for delivery
modes (`each` / `lead`), at-most-once semantics, catchup, and the
per-task wall-clock timeout.

### Ask Crewlet

Ask Crewlet is a drawer in the dashboard that answers a person's questions
about the company and prepares changes for them to approve. It has no
identity of its own: every read is a request made as the signed-in person,
and nothing is written until that person approves the change it previews. It
is **off** unless the company turns it on.

```yaml
providers:
  llm:
    assistant:                          # a dedicated entry, recommended
      agent: claude-code
      model: sonnet
      timeout_seconds: 90               # one interactive round
      max_concurrent: 4                 # its own slots, apart from the seats'
      overrides: { web_tools: denied }  # required for every Ask Crewlet entry

assistant:
  enabled: true                         # absent or false: off
  llm: [assistant]                      # providers.llm keys, a fallback chain
  max_steps: 30                         # tool steps per turn, 1..40
  turn_timeout_seconds: 900             # longest turn, 30..1200
  proposal_ttl_seconds: 900             # how long a proposal waits, 60..3600
  retention_days: 30                    # conversation history, 1..365
  token_budget: { day: 200000 }         # per person, optional
  deep_work:                            # optional: absent is quick mode only
    llm: assistant-deep                 # a Claude Code entry (agent: claude-code)
    timeout_seconds: 1200               # one run's wall clock, 60..1200
    max_steps: 150                      # rounds and Crewlet calls a task makes, 10..400
    max_tokens: 3000000                 # tokens a task may spend, 10000..20000000
```

| Field | Default | Meaning |
|---|---|---|
| `enabled` | off | Runs Ask Crewlet. A pointer, so an absent key and `false` both mean off. |
| `llm` | none | The model chain, first key first, in the shape a seat's `llm_auxiliary` takes. Required when `enabled` is true. |
| `max_steps` | 30 | The most tool steps one turn may take: enough to look a route up, read what it touches and propose the change. |
| `turn_timeout_seconds` | 900 | The longest one turn may run. Every round is a CLI launch of fifteen to twenty-five seconds. |
| `proposal_ttl_seconds` | 900 | How long a proposed change waits for its owner's approval before it expires. |
| `retention_days` | 30 | How long a person's conversations are kept. |
| `token_budget` | none | Each person's ceiling per calendar window, the shape of [a token budget](#token-budgets), charged on top of the company's own. |
| `deep_work.llm` | none | The entry deep work runs on: a Claude Code entry (`agent: claude-code`), run as its CLI's own run in a box, whose own shell, files and web tools a deep-work run uses. Placed in its own `run_in`, else the first configured `e2b` or image-backed `container` cell, else the engine host under OS confinement ([Running Ask Crewlet](../guides/assistant.md#deep-work)). |
| `deep_work.timeout_seconds` | 1200 | The longest one deep-work run takes. Never past the turn's read grant, which is why it is also the ceiling. |
| `deep_work.max_steps` | 150 | The most agentic rounds and Crewlet calls one deep-work task makes, across the runs an approval continues. |
| `deep_work.max_tokens` | 3000000 | The most tokens one deep-work task spends before it is stopped; the person's and the company's budgets still apply. |

A numeric field left out, or `0`, takes its default; a value outside its range
is refused naming the field.

**What one person may spend.** Every round of a person's turn is charged to the
company's budget and, where `token_budget` is set, to that person's own, across
every node of the fleet. A person whose own window is spent, or whose company's
is, is refused before the next model round with `budget_exhausted` naming which
(`person` or `org`), and starts again when the window turns over or the ceiling
is raised (on Company › Budgets, under **Ask Crewlet, per person**). A per-person
ceiling that can never refuse a question is a warning, as a seat's is: one at or
above another of its own windows that holds it, or at or above the company's on
a window that holds it, since a person's Ask Crewlet spend is the company's too
and the company's ceiling is reached first. Beside the
tokens, a person may have two turns running on a node and start twenty a minute
across the whole fleet.

**Every model in the chain must reach nothing the person's requests did not
return.** Each `llm` key must name a `providers.llm` entry that is:

- on a profile whose run has **no tool of its own, by construction**, so the
  CLI's one tool source is the engine's `crewlet` server serving the person's
  own requests: its profile declares `local_tools: denied` and a measured
  `tools_off`, and the argv every Ask Crewlet call runs carries it, with an `mcp`
  allow list naming the engine's tools alone. A CLI with its own shell or file
  reader on the engine host could read whatever the engine user can, the
  engine's own data and keys included. Today that is `claude-code` and `pi`;
  `codex` keeps `apply_patch`, which has no switch, and `opencode`'s policy has
  not been measured to remove its tools, so both are refused.
- **`web_tools: denied`**, set on the entry with
  `overrides: { web_tools: denied }`. A page the model fetched itself is text
  no authority decision saw, and a fetch is a way out for the person's data.
  Seats keep their web tools; only the entry that sets this loses them. The
  CLIs that can deny web are `claude-code`, `codex`, `opencode` and `pi`
  (see [Tool calls on this host](../concepts/llm-backends.md#tool-calls-on-this-host));
  any other is refused.

A document that breaks one of these is refused by `crewlet validate` and by
every `/config` write, each naming the chain member and the entry field to
change. A stored revision that somehow carries one still applies, so every
seat keeps running, and Ask Crewlet stays unavailable with that reason
until the chain is fixed. Run `crewlet llm doctor assistant` before turning
it on: on the exact argv Ask Crewlet runs it asks the CLI to run a shell
command, to read a file the doctor just wrote and to fetch a page, and reports
any of the three that succeeded as a problem. Nothing records that run, so the
chain cannot require it: it is the operator's check to make.

---

## Providers

Every model a seat runs on is reached through a **coding CLI** the engine
drives as a process — Claude Code for Anthropic's models, Codex for OpenAI's,
OpenCode for open models on any provider it speaks to — so a `providers.llm`
entry is that CLI's settings: which CLI, which model in its grammar, and how it
signs in. [LLM Backends](../concepts/llm-backends.md) is the whole story: why
there is no other kind of entry, picking a CLI per vendor, isolation, the tool
channel and every sign-in route.

```yaml
providers:
  llm:
    default:                            # named provider (referenced by roles via `llm: default`;
                                        #   `default` is also the fallback for a seat naming none)
      agent: claude-code                # optional — which CLI to drive (default claude-code):
                                        #   claude-code | codex | opencode | gemini-cli | qwen-code
                                        #   | cursor-agent | copilot | grok | muse-code | kimi-code
                                        #   | hermes | pi | custom
      model: sonnet                     # REQUIRED — in the CLI's own grammar: an alias or id for
                                        #   claude-code (sonnet, opus, claude-opus-5-5), an OpenAI
                                        #   id for codex, <provider>/<model> for opencode. Supports
                                        #   ${ENV_VAR}; a reference that resolves to nothing is
                                        #   REFUSED naming the variable — unlike a missing
                                        #   credential, which still builds: every call then fails
                                        #   with the CLI's own sign-in error, which names it, where
                                        #   a missing model has no such tell
      run_in: ""                        # optional — where a seat's executor runs when it launches
                                        #   on this entry: direct | container | e2b; empty takes
                                        #   providers.sandbox.default_run_in. Only claude-code and
                                        #   opencode can run an executor, so the field is REFUSED on
                                        #   any other CLI's entry, where nothing would read it; any
                                        #   CLI serves the reviewer, workers and side passes
      state_dir: ""                     # optional — the login + per-seat CLI homes, each CLI
                                        #   under <state_dir>/<agent>. Empty:
                                        #   $CREWLET_LLM_CLI_HOME/<key>, else ~/.crewlet/llm-cli/<key>.
                                        #   Use a persistent volume in an ephemeral container.
                                        #   Entries naming the SAME dir and driving the same CLI
                                        #   share one login (how per-phase models run off one
                                        #   `crewlet llm login`); on different CLIs they share
                                        #   nothing, so moving an entry to another CLI applies
                                        #   live and starts that CLI signed out. Its bundle
                                        #   (auth.credential_bundle) stays the old CLI's and is
                                        #   refused until re-exported from the new one
      timeout_seconds: 600              # optional — one stretch of a phase's CLI work on this
                                        #   host, wall clock (default 600): from its launch, or the
                                        #   end of the tool call it last waited on, to its next
                                        #   call or its exit — the seat's tools are never on it. A
                                        #   breach is a retryable `timeout`. An executor run is
                                        #   bounded by its round cap instead
                                        #   (providers.sandbox.default_max_turns)
      max_concurrent: 4                 # optional — live CLI processes on this host at once
                                        #   (default 4). Each is a 200-400 MB runtime that holds
                                        #   its slot for its whole run, and subscription plans
                                        #   throttle concurrency hard; a model call made inside one
                                        #   of a run's tool calls on the same entry borrows the
                                        #   run's slot. Executor runs are not counted: each seat
                                        #   runs one at a time
      env: {}                           # optional — extra child env, ${ENV_VAR}-resolved. The
                                        #   child gets an ALLOWLISTED environment, never the
                                        #   engine's, so declare anything else the CLI needs here —
                                        #   for opencode, the key of the provider its model names.
                                        #   An executor run's box gets it too, beneath the seat's
                                        #   role.sandbox.env
      auth:
        mode: subscription              # subscription (default) | api-key | inherit-env | env.
                                        #   subscription: the CLI's own login, or a headless token.
                                        #   api-key: ONE key in the CLI's key variable
                                        #   (ANTHROPIC_API_KEY for claude-code, CODEX_API_KEY for
                                        #   codex); refused for a CLI with none (hermes, pi,
                                        #   opencode, kimi-code). qwen-code keeps no login, so it
                                        #   is api-key only, with api_key named (its OPENAI_API_KEY
                                        #   is shared with other vendors). inherit-env: forward
                                        #   the engine's own value of either variable. env: the
                                        #   entry's env alone signs the CLI in (a cloud provider's
                                        #   switch, or a gateway's ANTHROPIC_AUTH_TOKEN); neither
                                        #   variable reaches the CLI and no login is seeded
        token: ""                       # optional — ${VAR} holding a headless subscription token;
                                        #   empty falls back to the profile's own variable
                                        #   (CLAUDE_CODE_OAUTH_TOKEN). subscription mode only, and
                                        #   only on a CLI that mints one (claude-code)
        api_key: ""                     # optional — ${VAR} holding the API key; empty falls back
                                        #   to the profile's own key variable, from the secret
                                        #   store, then the environment — except a shared one
                                        #   (qwen-code's OPENAI_API_KEY), where it is required.
                                        #   api-key mode only — a write is refused elsewhere,
                                        #   where it would reach nothing
        credential_bundle: ""           # optional — ${VAR} holding a `crewlet llm export` blob;
                                        #   empty falls back to CREWLET_LLM_CLI_<KEY>_CREDENTIALS
      overrides: {}                     # optional — replace any profile field when a vendor
                                        #   renames a flag OR moves the field the answer lives in
                                        #   (`text_paths`); validated here, so a typo is refused by
                                        #   `crewlet validate` and by every API write. A moved
                                        #   answer field is the drift that passes validation and
                                        #   fails at a seat's first turn — `crewlet llm doctor`
                                        #   names it

    claude-key:                         # Anthropic's models on an API key rather than a plan
      model: sonnet
      auth:
        mode: api-key                   # reads ANTHROPIC_API_KEY

    gpt:                                # OpenAI's models through Codex — for a seat's reviewer,
                                        #   workers and side passes (llm_review, llm_subagent, …):
                                        #   Codex runs no executor
      agent: codex
      model: gpt-5
      auth:
        mode: api-key                   # reads CODEX_API_KEY; leave it out to run on a
                                        #   ChatGPT login (`crewlet llm login gpt`)

    open:                               # open models through OpenCode
      agent: opencode
      model: openrouter/qwen/qwen3-coder  # <provider>/<model>, split at the first slash;
                                        #   a bare id is refused on a write
      env:
        OPENROUTER_API_KEY: "${OPENROUTER_API_KEY}"  # the key of the provider the model names

  sandbox:                              # REQUIRED once the company has an agent seat and an entry
                                        #   above: every seat's executor runs as its CLI's own run,
                                        #   in a box from this catalogue (code-sandbox.md)
    local:
      state_dir: "./acme-data/sandboxes"  # the box root: every box this host makes, a seat's
                                        #   and deep work's, each removed at teardown
      # image: ""                       # required only where a run is placed in a container
      # runtime: auto                   # auto | docker | podman
      # network: ""                     # the container network; "none" cuts the box off
      # run_args: []                    # how a container box is sized
      # runtime_env: {}                 # environment for the docker/podman CLI, never the box
    # e2b:                              # the remote backend, beside or instead of local:
    #   api_key: "${E2B_API_KEY}"       #   required whenever e2b: is present
    #   domain: ""                      #   set for a self-hosted cluster
    #   template: ""                    #   empty = the prebuilt template; also how a box is sized
    # fake: true                        # the in-process double, for demonstrations;
                                        #   never beside e2b: or local:
    default_run_in: direct              # where a run on an entry with no run_in happens. direct
                                        #   runs the CLI on this host as the engine's user.
                                        #   Required unless the catalogue names exactly one
                                        #   cell, or every entry an executor launches on
                                        #   names its own run_in
    # default_max_turns: 96             # agentic-round cap for one executor run on seats
                                        #   naming no role.sandbox.max_turns; 0 = uncapped
    # default_timeout_seconds: 900      # box TTL / keepalive: not a run cap
    # default_pause_ttl_seconds: 1800   # how long a box blocked on a person stays paused;
                                        #   0 = never pause
    # commit_attribution: true          # credit Crewlet as co-author on the commits a run
                                        #   makes as itself; false = credit nothing
    # setup: []                         # provisioning steps applied to every box
```

**One entry is one CLI on one model, signed in one way.** Per-phase models
are one entry per model, and entries that share a `state_dir` and drive one
CLI share one login. A seat's `llm` takes a list, which is how a plan and a key compose:
`llm: [default, claude-key]` runs on the plan and falls through to the key while
the plan is spent — see [Falling back between
entries](../concepts/llm-backends.md#falling-back-between-entries).

**What an entry does not set.** There is no temperature, effort, thinking
switch, output cap, endpoint or key list on an entry, and none on a call: a
coding CLI driven headlessly reads its model's behaviour from its own
configuration, and an entry holds one sign-in. See [What an entry does not
set](../concepts/llm-backends.md#what-an-entry-does-not-set).

Sign an entry in and check it end to end before the first turn:

```bash
crewlet llm login default -from-host        # adopt the login this machine already has
crewlet llm doctor                          # every entry: binary, version, sign-in, a real tool call
```

The CLI itself must be installed on the engine host — see
[Installation](installation.md#a-coding-cli).

### Embeddings

```yaml
providers:
  embeddings:                           # optional: the semantic half of Doclet's
                                        #   and Worklet's search (the company's pages and
                                        #   work items, embedded once for the fleet)
                                        #   and similarity search for the
                                        #   agent-learning subsystem (agent_diary
                                        #   candidate selection AND episode recall).
                                        #   Omit it and search is keyword only,
                                        #   the diary's candidates are its recent
                                        #   notes alone and episode recall renders
                                        #   nothing (recent turns are not similar
                                        #   work); nothing else changes.
    type: openai                        # openai | openai-compatible
    model: text-embedding-3-large       # required, and it DECIDES THE WIDTH: this
                                        #   one emits 3072, text-embedding-3-small
                                        #   1536. A new company gets the large one,
                                        #   because the width is what search quality
                                        #   rests on and it is not a knob worth
                                        #   guessing
    api_key: "${OPENAI_API_KEY}"        # supports ${ENV_VAR} references; empty falls
                                        #   back to OPENAI_API_KEY, the variable
                                        #   OpenAI's own SDK documents. Embeddings are
                                        #   the one model call the engine makes itself:
                                        #   a coding CLI computes no vectors
    base_url: "..."                     # optional — custom endpoint. This is the only
                                        #   difference between `openai` and
                                        #   `openai-compatible`, so a local embedding
                                        #   server needs nothing else
  # dimensions: 1024                  # OPTIONAL OVERRIDE, 64..4096. Unset takes the
                                        #   named model's OWN width, which is what
                                        #   you want unless you are deliberately
                                        #   shortening it: third-generation models
                                        #   truncate on request, and a shorter vector
                                        #   is a smaller index and a worse answer.
                                        #   A model this build does not know is
                                        #   REFUSED with `dimensions` unset rather
                                        #   than given a guess — name a known model
                                        #   or state the width yourself. embed-v4.0
                                        #   takes no override: Cohere's endpoint
                                        #   does not support the parameter
  # max_input_tokens: 512               # THE MODEL'S LIMITS, in tokens, and unset
  # max_batch_inputs: 32                #   takes the ones its vendor documents.
  # max_batch_tokens: 16384             #   Required where none is documented — a
                                        #   model this build does not know, and the
                                        #   request limits of gemini-embedding-001
                                        #   and embed-v4.0, whose OpenAI-compatible
                                        #   endpoints document none. A stated value
                                        #   may only LOWER a documented one (a
                                        #   gateway that accepts less); raising one
                                        #   is refused naming the field
```

**The width belongs to the model.** `text-embedding-3-large` emits 3072,
`text-embedding-3-small` 1536, `gemini-embedding-001` 3072 and `embed-v4.0`
1536, and leaving `dimensions` unset takes whichever the model named here
emits. There is no global default, because a number that was right for one
model is silently wrong for the next — and a model this build does not know is
refused rather than guessed at, naming both ways to fix it.

Every request asks for the width, in the OpenAI `dimensions` parameter, where
the endpoint takes it — so a shortened width is the width that comes back —
and every answer is checked against it either way:

| Model | `dimensions` at its endpoint | What the engine does |
|---|---|---|
| `text-embedding-3-large`, `text-embedding-3-small` | Supported: the vector is shortened to the width asked ([OpenAI's reference](https://developers.openai.com/api/reference/resources/embeddings/methods/create)) | Sends it, so a width below the model's own is the width stored |
| `gemini-embedding-001` | Not mentioned by [Google's compatibility page](https://ai.google.dev/gemini-api/docs/openai), whose examples send none | Sends it, as it always has; an endpoint that ignored it would answer 3072, which the width check refuses at any other width |
| `embed-v4.0` | Listed as unsupported by [Cohere's Compatibility API](https://docs.cohere.com/docs/compatibility-api), which takes no other way to choose a width either, so it answers at the model's default, 1536 ([the model page](https://docs.cohere.com/docs/cohere-embed)) | Never sends it, and refuses any other `dimensions`, naming the field |
| any other model | Unknown | Sends it |

**So do its limits.** How many tokens one input may hold, how many inputs one
request may carry and how many tokens one request may carry in all are facts
about the model, and the engine carries what each vendor documents for the
endpoint it calls:

| Model | Tokens an input | Inputs a request | Tokens a request |
|---|---|---|---|
| `text-embedding-3-large`, `text-embedding-3-small` | 8 192 | 2 048 | 300 000 |
| `gemini-embedding-001` | 2 048 | *state it* | *state it* |
| `embed-v4.0` | 128 000 | *state it* | *state it* |

Google's and Cohere's OpenAI-compatible endpoints document no limit per
request — Cohere's native API takes 96 texts a call, but that is a different
endpoint's — so those two are refused until `max_batch_inputs` and
`max_batch_tokens` are stated, and a model this build does not know states all
three, exactly as it states its width. A stated limit may only *lower* a
documented one, for a gateway or proxy in front of the model that accepts less;
raising one is refused, naming the field, because the provider would refuse
what the engine then sends.

**A company that needs a limit states it.** A stored company naming
`gemini-embedding-001`, `embed-v4.0` or a model this build does not know,
without the limits it needs, is refused at boot as well as at an apply,
exactly as a model with no known width is — and, the same way, `embed-v4.0`
at any `dimensions` but 1536, which its endpoint cannot produce. No node
starts on that revision, so state the limits in the same edit that names the
model.

The engine counts **bytes** against those token limits rather than shipping a
tokenizer per vendor: every tokenizer these models use emits at most one token
per byte of the text it is given, plus the few special tokens a server wraps an
input in (none for OpenAI, whose count is the input's own; sixteen are set
aside for every other model). So an input of 8 192 bytes is always inside
OpenAI's 8 192-token window, whatever language it is in — at the price that
ordinary prose, three to four bytes a token, is held to about a quarter of the
window. An input past that bound is refused *before* anything is sent, and a
batch is sent in as many requests as the model's limits need. What a text too
long for one input becomes is each caller's choice: the knowledge corpus embeds
a document's opening, while a turn's ask and an episode are split between words
into pieces that each fit, embedded in one call, and pooled into one vector —
so a long ask is still searched by meaning, all of it, rather than refused.

**The width is a contract with the store, not with the model.** Vectors of
two different widths cannot be compared, so a row written at the wrong one is
not a degraded search — it is a row that can be written and never read back,
silently and permanently. Two checks follow from that, and both refuse rather
than adapt:

- **At the apply.** A revision whose `dimensions` differs from the width this
  store already holds is rejected, with an error naming both. Changing the
  width is a restart, which is a decision for an operator who is watching
  rather than a silent divergence discovered at the first recall weeks later:
  after it the knowledge corpus re-embeds itself and each seat's diary and
  episodes are re-filled by the node holding the seat.
- **Per model, at recall.** Two models of one width are two spaces —
  `text-embedding-3-small` and `embed-v4.0` both answer 1 536 floats — so
  every diary and episode vector is stored with the model it came from and
  recall compares only rows of the query's model. Changing `model` at the
  same width is therefore an ordinary apply: each seat's diary and episodes
  are re-filled in the new space by the node holding it — about a million
  bytes of text a minute across the company — and until a row is, it is
  reached by recency and by conversation rather than ranked against a space
  it is not in, and `query_episodes` says how many of a seat's turns its
  search could not reach.
- **On every call.** A vector that comes back at the wrong width is refused
  rather than stored — on every call and not just the first, because a
  gateway or aggregator can move models mid-deployment.

**What a failure costs depends on who asked, so nothing in the provider
retries.** For a turn starting — diary and episode recall, a search's query
vector — a timed-out or refused call is *no vector*: personal memory keeps its
recent half, `## Similar prior work` renders nothing and `query_episodes` says
its search could not run (a recent turn is not similar work), and a hybrid
search serves its keyword half — and a retry would be spent inside a prefetch
somebody is waiting on. For the knowledge corpus, a failure is
a backlog, and the duty asks again on its next tick. What the provider gives
every caller instead is *which* failure it was, in three classes: **refused**
(HTTP 400, 413 or 422, or an input past the bound — sent again unchanged it
will be refused again, so a caller can set that one input aside rather than
resend its whole batch for ever), **transient** (408, 409, 425, 429 and 5xx, a
timeout or a network failure — asking again later may succeed) and
**configuration** (401, 402, 403, 404 and the remaining 4xx, or a vector of the
wrong width — nothing will succeed until `providers.embeddings` is fixed).

**One call's ceiling is not another's.** A single embedding is held to 15
seconds where nothing tighter bounds it — an episode embedded after its turn,
for one. What a person or a turn waits on is held to two seconds instead: a
search's query vector, whether a person or a seat's `search_knowledge` asked; a
turn's ask as the turn starts; the hint `query_episodes` or `refresh_memory`
passes; and a note `reflect_and_persist` keeps, which the seat's model is
waiting on mid-turn (a note the reflection after the turn keeps is held to the
same two seconds). Past it each degrades rather than waits. A hybrid search
serves its keyword half and a semantic one answers nothing, each saying why.
Personal memory is chosen from the recent half of its candidate pool alone. Similar prior work renders nothing, and
`query_episodes` says its search could not run — by design, since a recent
turn is not similar work and the block would offer it as precedent. The note
is kept without a vector until the node holding the seat fills it. So a slow
embeddings server shows up as `embedding_failed` searches, personal memory drawn
from recent notes only and no similar prior work, not as slow searches, slow
turn starts and stalled tool calls. A batch request carries up to
the model's request total (300 000 tokens on OpenAI) and is held to 60 seconds,
a fifth of the five minutes a corpus tick may go without progress. Nothing has
measured how long a server takes over a full request — OpenAI's or a self-hosted
one on CPU, the deployment most likely to need longer. If batches time out
against a slow server, lower `max_batch_tokens`: a smaller request is a shorter
one, and the ceiling a stuck call is held to does not move. Keep it at or above
the model's per-input window (`max_input_tokens`, or the model's own) — one
input must fit one request, so a request total below the window is also the most
one input may hold, and the knowledge corpus would then embed a shorter opening
of every long source, each under a new digest and so embedded again.

## Tier A

Tier A (`crewlet.yaml`, restart-only) says where this node's stream, store and
API are.  Example:

```yaml
# crewlet.yaml — Tier A bootstrap
stream:
  type: embedded                    # a JetStream server inside this process:
                                    #   no listener, no port, no service to
                                    #   operate. `nats` points the same slot at
                                    #   an external server — the same client
                                    #   code either way, so it is a connection
                                    #   choice rather than a second backend
  store_dir: "./crewlet-data/stream"  # REQUIRED on a broker MEMBER — every
                                    #   data node on an embedded stream, a
                                    #   single node included. Empty would be
                                    #   in-memory, and every data node keeps
                                    #   its state logs here from boot — the
                                    #   people who can sign in, company or
                                    #   none — so `crewlet validate` and the
                                    #   engine both refuse it rather than
                                    #   lose them at the first restart. A
                                    #   member of a fleet's broker also holds
                                    #   the fleet's streams for every node
                                    #   that reaches it. A leaf (a node
                                    #   without `data`) is refused one: it
                                    #   keeps no stream of its own. ONE
                                    #   broker per directory: a member holds
                                    #   `crewlet-broker.lock` inside it while
                                    #   its broker runs, and a second — an
                                    #   engine, or a `crewlet` command
                                    #   reaching the fleet's store with the
                                    #   engine stopped — is refused, naming
                                    #   the holder's pid, host and start.
                                    #   NATS itself takes no lock and would
                                    #   accept it, and two writers corrupt
                                    #   the store
  # store_max_bytes: 68719476736    # how much of that directory's volume the
                                    #   EMBEDDED broker may hold — the ONE number
                                    #   every stream ceiling on it is compared
                                    #   against, because a ceiling is a
                                    #   RESERVATION the broker refuses if it
                                    #   cannot back it. UNSET, the broker sizes
                                    #   itself: three quarters of that volume's
                                    #   free space when its JetStream came up,
                                    #   plus what it already occupies there,
                                    #   which is what a single-engine host should
                                    #   have. MEASURED ONCE, AT BOOT, on either
                                    #   path — a disk that later grows or shrinks
                                    #   does not move this limit, and a node that
                                    #   should see a resized volume is restarted.
                                    #   SET IT WHEN MORE THAN ONE ENGINE SHARES A
                                    #   FILESYSTEM and divide it between them:
                                    #   free space bounds their SUM, so two
                                    #   engines each sizing themselves from what
                                    #   they can see over-commit it, and the
                                    #   failure is `insufficient storage
                                    #   resources available` — or, on a fleet,
                                    #   `no suitable peers for placement,
                                    #   insufficient storage` — naming whichever
                                    #   stream was provisioned last. Bounds:
                                    #   6 GiB..64 TiB, and it must not be
                                    #   smaller than the ceilings declared
                                    #   inside it. What a fresh node reserves
                                    #   (every state log at its floor, and the
                                    #   object bucket) can exceed the bounds'
                                    #   own floor, so a value they admit can
                                    #   be short of what a node boots in: `crewlet
                                    #   validate` warns below the least a
                                    #   fresh node of the configuration
                                    #   reserves (see the deployment guide).
                                    #   REFUSED for `type: nats` — an external
                                    #   cluster's account limits are its own
                                    #   operator's, and this node reads them back
                                    #   rather than declaring them
  # url: "nats://nats.internal:4222"  # required for `nats`, REFUSED for
                                    #   embedded — an embedded server has no
                                    #   address, so a url there is read by
                                    #   nobody. The server it reaches must
                                    #   hold the connection to a max_payload
                                    #   of at least 8 MiB (`max_payload: 8MB`
                                    #   on every server, and on the account
                                    #   the engine signs in to where that
                                    #   states one; nats-server's default is
                                    #   1 MiB): an event, or a node's answer
                                    #   to another, may be that large, so the
                                    #   node refuses to start naming it — see
                                    #   Deployment. A file's bytes cross in
                                    #   messages of 128 KiB, which a server's
                                    #   default already carries
  # replicas: 3                     # 1 solo (the default); 3 across an EMBEDDED
                                    #   cluster, where it is what makes a publish
                                    #   quorum-durable before it returns. At most
                                    #   5, JetStream's own ceiling, and on an
                                    #   embedded cluster at most its members —
                                    #   this node and `cluster.peers` — so >1
                                    #   without peers is refused: this node has
                                    #   nothing to replicate to. It is the
                                    #   BROKER's copies of its streams and
                                    #   buckets — the company's files among
                                    #   them under the default `nats` object
                                    #   store (`store.objects`)
  # cluster:                        # an EMBEDDED server joining its peers, which
  #   name: crewlet                 #   is the fleet topology: every node embeds
                                    #   one member of one cluster. REQUIRED once
                                    #   anything else here is set — the server
                                    #   reads none of these fields from an
                                    #   unnamed cluster, so a block without it
                                    #   starts a solo node that forms no cluster
                                    #   at all, and Tier A refuses that. EMBEDDED
                                    #   ONLY: an external cluster is formed by
                                    #   its own operator's config, so this whole
                                    #   block is REFUSED against `type: nats`
                                    #   rather than accepted and read by nobody
  #   port: 6222                    #   `node.id` is
  #   peers:                        #   the member name, so it must survive a
  #     - "nats://node-1:6222"      #   restart — a name minted at boot orphans
  #     - "nats://node-2:6222"      #   this member's replicas every time.
                                    #   `peers` lists the OTHER members, each
                                    #   scheme://host:port: an entry that is
                                    #   recognisably this node's own route
                                    #   (host+port, or advertise) or a repeat
                                    #   is not counted as a member, with a
                                    #   warning, and one that is not dialable
                                    #   is refused
  #   host: 10.0.0.11               #   the interface the route port binds.
                                    #   Unset binds EVERY interface, and a route
                                    #   port is unauthenticated cluster access —
                                    #   set it to the private address the peers
                                    #   reach
  #   advertise: "node-1.internal:6222"
                                    #   what peers should DIAL for this member,
                                    #   when that differs from what it binds — a
                                    #   mapped container port, a NAT, a load
                                    #   balancer. Members learn each other from
                                    #   the members they already have, so unset
                                    #   this address is derived from the
                                    #   connection's remote address, which
                                    #   behind a NAT is unreachable or somebody
                                    #   else's. A bare host keeps this member's
                                    #   own route port
  # leaf:                           # the LEAF LINK, and a node is on exactly
                                    #   one side of it. WHICH SIDE is its
                                    #   BROKER KIND, derived from this block and
                                    #   nothing else: `urls` makes it a leaf,
                                    #   and any other embedded broker is a
                                    #   member. `node.roles` is a separate
                                    #   question: what the node's DISK keeps.
  #   urls:                         #   A LEAF dials the members' leaf
  #     - "nats-leaf://node-1:7422" #   listeners here — any one that answers
  #     - "nats-leaf://node-2:7422" #   will do — and its embedded broker runs
                                    #   NO JetStream: no replica, no vote, no
                                    #   stream store (`store_dir`, `cluster` and
                                    #   a leaf `port` are refused beside it).
                                    #   Every stream and bucket its clients use
                                    #   is a member's, reached across this link.
                                    #   A node without `data` must be a leaf
                                    #   and a node with it must not: every data
                                    #   node holds the whole estate as a member
  #   port: 7422                    #   A MEMBER's listener, where leaves join.
                                    #   A member that opens one is in a fleet
                                    #   even with no peers, so it must persist
                                    #   (`store_dir`, as above: the leaves that
                                    #   join it keep nothing, so it keeps
                                    #   everything they do) and needs
                                    #   `coordination.type: embedded-kv`
  #   host: 10.0.0.11               #   the interface the listener binds. Like
                                    #   the route port it accepts any connection
                                    #   that reaches it: the fleet is one trust
                                    #   domain on a network its operator
                                    #   controls, so bind it there
  #   advertise: "node-1.internal:7422"
                                    #   what leaves should dial for this member,
                                    #   when it differs from what it binds
  # sync: always                    # what an acknowledged publish has actually
                                    #   reached. `always` (the default, at every
                                    #   replica count) fsyncs every write before
                                    #   acknowledging it; a duration — `30s` —
                                    #   declines the fsync and names the window
                                    #   an acked write may be behind the disk.
                                    #   It is NOT inferred from `replicas`: "a
                                    #   replicated member has a quorum instead
                                    #   of a disk" holds when one host loses
                                    #   power and fails when a RACK does, and a
                                    #   three-node fleet in one rack is three
                                    #   copies of one unflushed page cache. A
                                    #   window is refused where it would be
                                    #   recorded and not honoured: against an
                                    #   external cluster (which stores its own
                                    #   data), on a leaf (which runs no
                                    #   JetStream, so the members' own `sync`
                                    #   decides), below 3 replicas (no quorum
                                    #   to trade the disk for), and on a
                                    #   cluster whose peers are all on this
                                    #   host (one failure domain). Declining
                                    #   it on a real three-host fleet is a
                                    #   legitimate trade and costs 1–3 ms per
                                    #   write on NVMe
  # debug: true                     # the EMBEDDED broker's OWN debug logging,
                                    #   which is a SEPARATE question from
                                    #   `logging.level` — that one is how loud
                                    #   the ENGINE is. Off by default because
                                    #   nats-server's debug output is per
                                    #   internal-client rather than per event,
                                    #   and the engine's coordination reads
                                    #   manufacture those: every KV key listing
                                    #   is an ordered consumer created and
                                    #   deleted, and each deletion writes two
                                    #   `JetStream connection closed: Client
                                    #   Closed` lines. Two 15-second duty loops
                                    #   list keys every tick, so this is a
                                    #   constant background stream on an idle
                                    #   node — turn it on when the BROKER is
                                    #   what you are diagnosing. Its warnings and
                                    #   errors are never gated by it and always
                                    #   reach the log. What it unlocks are debug
                                    #   lines, so `logging.level: debug` (or
                                    #   `-debug`, or a `debug` log file) still has
                                    #   to be recording them — `crewlet validate`
                                    #   warns when nothing is. REFUSED for `type:
                                    #   nats`: an external cluster logs wherever
                                    #   its own operator configured it to
  # tracker_log_max_bytes: 17179869184
                                    #   the byte ceiling on the mutation log —
                                    #   the ordered stream the engine's own
                                    #   tracker writes through. UNSET DERIVES a
                                    #   quarter of the stream volume's free
                                    #   space, clamped to 4..64 GiB, because one
                                    #   fixed number is five years of history on
                                    #   the modelled rate and one boot on a
                                    #   small disk. CROSSING IT REFUSES, it does
                                    #   not shed: there is no age bound on this
                                    #   stream, so a full log drops no history —
                                    #   the append is refused, loudly, naming
                                    #   whatever is blocking the trim. ONE
                                    #   BUDGET FOR EVERY STATE LOG: the broker
                                    #   reserves each ceiling in full when it
                                    #   creates the stream, so this field and
                                    #   every ceiling below it share half of
                                    #   what the broker can grant them, less
                                    #   the ceiling any of them that already
                                    #   exists holds. The unset ones are scaled
                                    #   down together to fit, never below 1 GiB
                                    #   each but Ask Crewlet's,
                                    #   which gives way down to its own 64 MiB
                                    #   floor; any other unset ask already below
                                    #   1 GiB (the identity log's) is kept as
                                    #   asked. A value you set is never scaled,
                                    #   and a boot that cannot reserve it fails
                                    #   naming the field, the bytes it needed
                                    #   and the bytes the broker had. Every one
                                    #   of them is the value a stream is
                                    #   CREATED with: editing it later changes
                                    #   nothing until `crewlet retention
                                    #   set-capacity` does.
                                    #   WHAT THE BROKER CAN GRANT is
                                    #   `store_max_bytes` wherever you set one,
                                    #   and Tier A refuses a limit smaller than
                                    #   the ceilings declared inside it
  # tracker_vectors_max_bytes: 68719476736
                                    #   the vector changelog's ceiling
                                    #   (1 GiB..2 TiB). SIZED FOR THE PEAK: the
                                    #   stream keeps one message per source, so
                                    #   a week's minting is ~91 MB — but
                                    #   changing the embedding model rewrites
                                    #   EVERY source in a few hours, and for the
                                    #   following week all of them are in the
                                    #   window: the whole corpus, ~17 MB per
                                    #   agent seat per YEAR OF HISTORY. UNSET
                                    #   DERIVES what the mutation log derives
                                    #   from the volume (a quarter of its free
                                    #   space, 4..64 GiB), whatever
                                    #   tracker_log_max_bytes is set to: that
                                    #   field bounds a trailing window of
                                    #   records, and this log's peak is the
                                    #   company's whole history. At 64 GiB it
                                    #   holds a model change twice over for
                                    #   ~2 000 seat-years (400 seats in their
                                    #   fifth year); past that, set ~34 MB per
                                    #   agent seat per year of history. A
                                    #   ceiling sized from the steady state
                                    #   would refuse the one operation it exists
                                    #   to survive
  # pages_log_max_bytes: 4294967296 #   the knowledge base's log, the ordered
                                    #   stream every Doclet page write goes
                                    #   through (1..256 GiB). UNSET DERIVES a
                                    #   quarter of the mutation log's ceiling,
                                    #   set or derived (so 1..16 GiB from a
                                    #   volume alone): a knowledge base is
                                    #   a few thousand pages against a tracker's
                                    #   hundreds of thousands of items, so its
                                    #   log grows at about a quarter of the rate
                                    #   and a blocked trim fills either one in
                                    #   the same time. Crossing it refuses the
                                    #   append, like the mutation log's
  # iam_log_max_bytes: 536870912    #   the identity estate's log, the ordered
                                    #   stream every person, credential,
                                    #   invitation and session goes through
                                    #   (64 MiB..16 GiB). NOT DERIVED FROM THE
                                    #   DISK either: what it grows with is your
                                    #   HEADCOUNT and how often people sign in.
                                    #   Unset takes a flat 512 MiB. SESSIONS SIZE
                                    #   IT — a session writes one record when it
                                    #   opens and one when it closes and nothing
                                    #   in between, because a re-issue moves a
                                    #   deadline inside the cookie — so this is
                                    #   about eighteen months of a completely
                                    #   blocked trim for a few hundred people at
                                    #   a pessimistic sign-in rate. Set it toward
                                    #   the 64 MiB floor for a small company and
                                    #   up for a large one
  # usage_log_max_bytes: 1073741824
                                    #   the usage log's ceiling, the compacted
                                    #   stream every data node publishes its own
                                    #   company days to — spend, turns, page
                                    #   reads, schedule fires — so history is
                                    #   answered fleet-wide and outlives the
                                    #   node that spent it (64 MiB..64 GiB). A
                                    #   node without `data` publishes none: its
                                    #   events are kept by a data node in
                                    #   custody and counted in that node's day.
                                    #   NOT DERIVED FROM THE DISK either: unset
                                    #   takes a flat 1 GiB. SIZED BY A CENSUS,
                                    #   not a rate: one message per (node, day,
                                    #   seat, schedule or person) for 181 days,
                                    #   about 217 MB for three nodes of forty
                                    #   seats and twenty schedules, and about
                                    #   12 MB for one node of ten seats — set
                                    #   it toward the 64 MiB floor for a small
                                    #   company. Crossing it refuses the append
                                    #   and the log_headroom alarm names
                                    #   `usage`
  # artifacts_log_max_bytes: 4294967296
                                    #   the artifacts log, the ordered stream
                                    #   every artifact's create, publish,
                                    #   share, trash and purge goes through
                                    #   (1..256 GiB). UNSET DERIVES a quarter of
                                    #   the mutation log's ceiling, set or
                                    #   derived (so 1..16 GiB from a volume
                                    #   alone): a record names a version's
                                    #   files by object, and the object store
                                    #   keeps their bytes, so the log grows with
                                    #   how often people and agents publish
                                    #   rather than with what they publish.
                                    #   Crossing it refuses the append
  # artifact_data_log_max_bytes: 4294967296
                                    #   the artifact datastore's log, the
                                    #   ordered stream every write, undo and
                                    #   schema change of an artifact's
                                    #   datastore goes through (1..512 GiB).
                                    #   UNSET DERIVES a quarter of the
                                    #   mutation log's ceiling, set or
                                    #   derived (so 1..16 GiB from a volume
                                    #   alone). Its records carry the rows
                                    #   themselves, so the log grows with how
                                    #   much pages and seats write: a company
                                    #   whose pages write heavily sets it
                                    #   higher, which is why it accepts twice
                                    #   the artifacts log's bound. Crossing it
                                    #   refuses the append, and the top of it
                                    #   is kept for gate records as on the
                                    #   tracker's log
  # assistant_log_max_bytes: 536870912
                                    #   Ask Crewlet's log, where
                                    #   every person's conversations, turns
                                    #   and proposals are recorded (64 MiB..
                                    #   64 GiB). UNSET TAKES 512 MiB, sized
                                    #   by how many people use
                                    #   Ask Crewlet rather than by the disk,
                                    #   and scaled down as far as 64 MiB
                                    #   where the broker is short:
                                    #   about two weeks of a completely
                                    #   blocked trim at the reference
                                    #   company's rate. Crossing it refuses
                                    #   the append, and the top of it is kept
                                    #   for gate records. How long a
                                    #   conversation is kept is the company
                                    #   document's `assistant.retention_days`
  # chat_log_max_bytes: 8589934592  #   the chat log, the ordered stream every
                                    #   Chatlet message, reaction and membership
                                    #   change goes through (1..256 GiB). UNSET
                                    #   ASKS FOR 8 GiB, capped at a sixteenth of
                                    #   the volume's free space (a quarter of
                                    #   what an unset mutation log derives, so
                                    #   4 GiB on 64 GiB free and the whole 8 GiB
                                    #   from 128): a larger ask on a small
                                    #   volume comes out of the vector
                                    #   changelog's room for a model change.
                                    #   What is asked is scaled with the others,
                                    #   so a 64 GiB first boot creates it at
                                    #   about 1.4 GiB. It does NOT follow the
                                    #   mutation log's ceiling as the knowledge
                                    #   base's does: chat grows faster (~32 MB a
                                    #   day against ~22), so following it would
                                    #   make chat the largest ask in the one
                                    #   budget, and a first boot would need
                                    #   ~136 GiB free rather than ~100 before the
                                    #   vector changelog holds a fifth-year
                                    #   model change clear of the headroom
                                    #   alarm. So if you size
                                    #   tracker_log_max_bytes for a long blocked
                                    #   trim, size this for the same horizon, or
                                    #   chat refuses first. WHY 8 GiB: at the
                                    #   declared census of 20 000 messages a day
                                    #   and a mean record near 1.5 KiB, plus its
                                    #   read barriers, the log grows ~32 MB a
                                    #   day, so a healthy fleet holds ~225 MB —
                                    #   one min_age window, not a year. This
                                    #   ceiling is what a COMPLETELY BLOCKED
                                    #   trim may spend first: ~8 months at that
                                    #   census, ~3½ at the supported 50 000/day
                                    #   (on 64 GiB free, capped and scaled:
                                    #   ~6 weeks and ~2½). Crossing it refuses
                                    #   the append, which for a transcript is
                                    #   the only acceptable direction — old
                                    #   messages go by a prune RECORD all data
                                    #   nodes apply
                                    #   (chat.chatlet.message_retention_days),
                                    #   never by a stream bound
  # tracker_retention:              # when the log may be trimmed. Every term
                                    #   here is a statement about the OPERATOR's
                                    #   estate rather than the company's policy,
                                    #   which is why it is Tier A. Read only on
                                    #   a node with the `data` role, which is
                                    #   where the trim and the snapshots run
  #   min_age: 7d                   #   the age floor NO trim may cross, whatever
                                    #   the other terms say (24h..90d). It can
                                    #   only make a trim more conservative, so it
                                    #   is a lower bound on how long the log
                                    #   keeps a record and never a ceiling — and
                                    #   it says nothing about any node's own
                                    #   store file
  #   backup_max_age: 24h           #   how stale the newest backup may be before
                                    #   the trim STOPS ENTIRELY (1h..30d). A
                                    #   company that never backs up never trims:
                                    #   the log is the only copy of what no node
                                    #   has applied yet
  #   backup_floor: engine          #   whose word the trim takes for what is
                                    #   backed up, PER LOG: each log follows the
                                    #   newest counted backup covering it.
                                    #   `engine` counts the backups the engine
                                    #   wrote and verified; `operator` an
                                    #   explicit acknowledgement of each log
                                    #   (`crewlet retention ack -stream`), for a
                                    #   company that trims only what has left
                                    #   the host — and then trims NOTHING of a
                                    #   log until somebody says so for that log
  #   snapshot_interval: 24h        #   how stale a node's newest snapshot may be
                                    #   before it takes another (1h..7d). A
                                    #   snapshot is a full copy of the replicated
                                    #   estate, so four a day is a day's worth of
                                    #   I/O to save a joining node a replay it
                                    #   can do in under a minute. CROSS-FIELD:
                                    #   `snapshot_interval × 2 < min_age`, or
                                    #   every snapshot is older than the trim
                                    #   floor and a node that lost its store has
                                    #   nothing to resume from
  #   rejoin_window: 30m            #   the budget for a node to become a
                                    #   complete replica (5m..24h) — what a join
                                    #   is measured against and reported on, and
                                    #   what a fleet's data nodes size their
                                    #   startup probe by, since a join at boot
                                    #   runs inside the recovery. A setting
                                    #   rather than a constant, because the
                                    #   answer is a property of the operator's
                                    #   disks and network
  # event_retention_hours: 720      # 0 takes the queue's own default (30 days).
                                    #   Unbounded is deliberately not expressible:
                                    #   an event log nothing sweeps grows for the
                                    #   life of the deployment
  # credentials: ""                 # path to a NATS credentials file
  # token: "${CREWLET_NATS_TOKEN}"  # bearer token for an external server
  # tls:                            # the TCP layer under that auth: which CA to
  #   ca: /etc/crewlet/ca.pem       #   trust; empty = the host roots
  #   cert: /etc/crewlet/client.pem #   the CLIENT certificate, for a broker
  #   key: /etc/crewlet/client.key  #   configured `tls { verify: true }`.
                                    #   Both or neither — half a keypair is
                                    #   refused at validation. There is no
                                    #   skip-verify switch, deliberately: it is
                                    #   set once during a bring-up and never
                                    #   unset, and a private CA is one file

store:
  path: "./crewlet-data/company.db"   # the NODE estate — the audit log, memory,
                                    #   config revisions. Never a secret: the
                                    #   company's live in the fleet's store,
                                    #   and a `crewlet` command run with the
                                    #   engine stopped writes them there,
                                    #   asking this file's lock whether the
                                    #   engine runs, without opening it.
                                    #   Owned exclusively by this process. Not a
                                    #   shared database, and no DSN: two engines
                                    #   on one file corrupt it
  # snapshot_dir: "./crewlet-data/snapshots"
                                    #   where this node keeps its own snapshots
                                    #   of the replicated estate — the file a
                                    #   peer joining the fleet copies instead of
                                    #   replaying the whole log. Absolute, or
                                    #   relative to the store's directory. THE
                                    #   DEFAULT PUTS A FULL COPY ON THE SAME
                                    #   VOLUME as the live database, which is why
                                    #   the snapshot loop refuses below 1.1× the
                                    #   store's size free rather than filling the
                                    #   disk the applier is committing to. A
                                    #   separate volume is the production shape
  # replicated_path: "./crewlet-data/crewlet-replicated.db"
                                    #   the REPLICATED estate — everything a
                                    #   state log's applier writes, held whole
                                    #   by every data node from boot, and by no
                                    #   node without `data`. Empty puts it
                                    #   beside `path`, which is what makes "back
                                    #   up the data directory" true. It is a
                                    #   separate FILE rather than more tables
                                    #   because a snapshot for a joining node is
                                    #   a copy of the estate's file alone;
                                    #   separate it only to put it on a
                                    #   different disk, and never onto the same
                                    #   file as `path`
  # scratch: false                  #   DELETE this node's store at every boot,
                                    #   under the store's own lock, and open it
                                    #   with no replicated estate at all.
                                    #   REQUIRED on a node without the `data`
                                    #   role and REFUSED on one with it, so the
                                    #   deletion is never a surprise either way.
                                    #   Explicit rather than derived from the
                                    #   roles because it is the one setting here
                                    #   that deletes something. The offline
                                    #   `migrate`, `config` and `search eval`
                                    #   commands refuse a scratch store: what
                                    #   they wrote would be gone at the next
                                    #   boot. `secrets`, `llm`, the
                                    #   provisioners and every other command
                                    #   resolving the company's ${VAR}s refuse
                                    #   it unless -api names a running node
                                    #   that holds data: the company's secrets
                                    #   are the fleet's, and a node that holds
                                    #   none has no store of them to reach
  # max_open_conns: 0               #   the ORDINARY READERS of this node's
                                    #   own database and of the replicated
                                    #   estate's (that one never fewer than
                                    #   two). 0 takes the store's own default,
                                    #   max(8, GOMAXPROCS): two dashboards'
                                    #   worth of socket queries. ONE
                                    #   connection reserved for identity work
                                    #   on each file, and one pinned
                                    #   connection per state log the
                                    #   replicated estate carries, are ADDED
                                    #   on top rather than taken out of the
                                    #   number, so it is always the readers.
                                    #   Raise it if the `pool_starved` alarm
                                    #   fires — see reference/alarms.md —
                                    #   which means reads are queuing before
                                    #   they start
  # busy_timeout_seconds: 0         #   how long a WRITE waits for the
                                    #   database's write lock before giving up
                                    #   and retrying once; 0 takes the store's
                                    #   default of 5s, half the dashboard's own
                                    #   query timeout. It bounds the wait
                                    #   wherever it happens — in the driver, or
                                    #   in the engine's own FIFO queue for that
                                    #   lock. Raise it on a node applying a
                                    #   burst, where `store_tx_retry` in the
                                    #   log names it — to 7 for chat near the
                                    #   supported 50 000 messages a day, whose
                                    #   queue reaches the ten seconds two 5s
                                    #   waits allow. Keep it under 10, the
                                    #   dashboard's query timeout; see
                                    #   guides/replication.md
  # objects:                        # where the company's FILES are kept — see
                                    #   concepts/object-store.md. EVERY node
                                    #   carries the same block, a node without
                                    #   `data` included: an upload or a download
                                    #   goes to the store from the node serving
                                    #   it. The first node to boot records the
                                    #   store in the coordination store, and a
                                    #   node configured with another refuses to
                                    #   boot, naming both
  #   backend: nats                 #   nats (the default, and what an absent
                                    #   block means): a JetStream object store
                                    #   bucket, `crewlet_files`, on the fleet's
                                    #   own broker, kept at `stream.replicas`
                                    #   copies on its members like every stream
                                    #   and backed up with them. s3: an
                                    #   S3-compatible bucket, named below
  #   nats_max_bytes: 0             #   the nats bucket's byte RESERVATION on
                                    #   the broker, 1 GiB..64 TiB. Unset
                                    #   derives the smaller of a quarter of the
                                    #   broker's limit and what its state logs'
                                    #   half and a 1 GiB coordination floor
                                    #   leave. A full bucket refuses uploads,
                                    #   never the fleet's leases. An existing
                                    #   bucket keeps the reservation it was
                                    #   created with. REFUSED with backend s3
  #   s3:                           #   REFUSED unless backend is s3
  #     endpoint: ""                #   the S3 API's base URL; empty is Amazon
                                    #   S3's own for the region. R2, MinIO,
                                    #   Ceph's gateway and GCS's interoperability
                                    #   endpoint each name theirs here
  #     region: eu-west-1           #   REQUIRED — every request is signed for
                                    #   a region, even where the provider
                                    #   ignores it (R2: auto, MinIO: us-east-1)
  #     bucket: acme-files          #   REQUIRED. Probed at boot under the
                                    #   prefix: a node that cannot store and
                                    #   delete an object there does not start
  #     prefix: ""                  #   prepended to every key: an object is
                                    #   `<prefix>files/<key>`, e.g. `acme/`.
                                    #   Companies sharing one bucket are kept
                                    #   apart only by credentials scoped to
                                    #   their own prefix, never by the prefix
  #     path_style: false           #   address the bucket in the path rather
                                    #   than the host name; MinIO and most
                                    #   self-hosted gateways need it
  #     access_key_id: ${S3_ACCESS_KEY_ID}         # both as ${VAR} references,
  #     secret_access_key: ${S3_SECRET_ACCESS_KEY} #   or NEITHER, which takes
                                    #   the SDK's own chain: the environment, a
                                    #   shared profile, a web identity or the
                                    #   instance's role. A private CA is read
                                    #   from AWS_CA_BUNDLE

coordination:
  type: local                       # one node holding its own seat leases;
                                    #   a fleet needs `embedded-kv`. There is no
                                    #   address to give it: the coordination KV
                                    #   lives on the stream's OWN broker. On the
                                    #   embedded one it holds a second
                                    #   connection, and both are watched: either
                                    #   closing for good stops the node, so it
                                    #   never holds live leases over one while
                                    #   the one carrying its inbox is gone
  # lease_ttl_seconds: 45           # 0 takes the seat layer's own measured
                                    #   default of 45s — three heartbeat
                                    #   intervals, so two consecutive missed
                                    #   renewals still leave a full interval to
                                    #   recover in. Shorter speeds failover and
                                    #   sheds healthy seats on ordinary jitter.
                                    #   AT LEAST 15 when set: it is also how
                                    #   long the heartbeat may go without a
                                    #   beat before the node's watchdog ends it
                                    #   (exit 75), and a busy node goes seconds.
                                    #   A fraction above that is fine.
                                    #   Seats and presence only: a fleet duty
                                    #   sizes its lease from its own tick and
                                    #   a tracker move or merge from its own
                                    #   work.
                                    #   IT IS THE BUCKET'S TTL, set by whichever
                                    #   node created the lease bucket first and
                                    #   ADOPTED by every node after it — a peer
                                    #   booting with a different value does not
                                    #   rewrite it, and logs
                                    #   `coord_kv_lease_ttl_differs` naming the
                                    #   one in force — AND RUNS AT IT, because
                                    #   the bucket's age is what expires a
                                    #   lease and a node claiming longer than
                                    #   it would have every acquire refused.
                                    #   Its heartbeat and a stop's allowance are
                                    #   fractions of the live value too. So
                                    #   make it agree across the fleet:
                                    #   changing it requires deleting the
                                    #   bucket while the fleet is down

api:
  host: "0.0.0.0"       # what this node BINDS
  port: 8000            # node.roles decides what it carries. With `ingress`:
                        #   the dashboard, the REST API, the sign-in routes,
                        #   the webhooks, the executors' tool bridge and the
                        #   probes. Without it: /health and /ready only.
                        #   0 (the default) binds nothing at all
  external_url: "https://crewlet.example.com"
                        # REQUIRED once port is set on an ingress node — where
                        #   a PERSON reaches this deployment, which is rarely
                        #   what the two lines above bind; with no public
                        #   listener, where vendors reach it too
  trusted_proxies: ["10.0.0.0/8"]
                        # whose X-Forwarded-For is believed. A CIDR list,
                        #   never a bool; 0.0.0.0/0 is refused. Empty with
                        #   an https external_url is warned about: whatever
                        #   terminates the TLS is then every caller's
                        #   source — in every audit row, and in the
                        #   sign-in throttle's key for every login
  public:               # optional — a second listener for the routes outside
    port: 8443          #   parties call: /webhooks/*, /otlp/{token}, /mcp/{token}.
                        #   Once set they are served ONLY here (404 on api.port),
                        #   and everything else — the dashboard, the REST API,
                        #   the sign-in routes, /config, /secrets, the probes —
                        #   ONLY on api.port. host defaults to every interface.
                        #   0 / unset keeps every route on api.port
    external_url: "https://hooks.example.com"
                        # REQUIRED once public.port is set on an ingress node —
                        #   where VENDORS and sandboxes reach this listener:
                        #   every webhook and app manifest is built on it. A
                        #   box's bridge and OTLP endpoints are built on
                        #   CREWLET_MCP_BRIDGE_URL and
                        #   CREWLET_SANDBOX_OTEL_RECEIVER_URL, which name the
                        #   same address
  artifacts:            # optional: the artifact origin, a THIRD listener that
    port: 8001          #   serves the files of artifact versions, their assets
                        #   and the embedded page runtime, and nothing else: no
                        #   API route, no probe, no cookie. Ingress nodes only;
                        #   0 / unset serves no artifacts on this node.
                        #   host defaults to every interface
    external_url: "https://{artifact}.example-usercontent.net"
                        # REQUIRED once artifacts.port is set on an ingress
                        #   node. {artifact} at the start of the leftmost
                        #   label gives every artifact an origin of its own
                        #   (wildcard DNS and TLS, on a registrable domain
                        #   that is not the dashboard's); a plain origin
                        #   (https://artifacts.example.com, or a second port
                        #   on the dashboard's host) serves every document
                        #   with an opaque origin
  auth:
    admin: serve        # serve | refuse. Required once port is set: whether
                        #   THIS node grants admin to those who hold it.
                        #   refuse clamps admin away here and keeps every app
    session:
      absolute: 168h          # 1h..720h (default 168h)
      step_up: 1h             # 5m..24h  (default 1h): how recent a proof of
                              #   identity every step-up gesture asks for — a
                              #   config, setup, credential, deployment or
                              #   /iam write, revealing a secret, changing
                              #   how somebody proves who they are. ONE window,
                              #   as GitHub's sudo mode keeps one. A Tier A
                              #   token or a machine token is fresh by
                              #   construction
    audit:
      changes: 9600h    # 2160h..24000h (default 9600h = 400 days)
      sessions: 2160h   # 168h..9600h   (default 2160h = 90 days), and never
                        #   longer than `changes`
    # People always sign in with a login and a password; these three say
    #   what that asks of them.
    totp: required      # required (the default when unset) | optional.
                        #   required: a password sign-in by somebody who
                        #   holds no second factor opens a session that may
                        #   only enrol one (POST /auth/totp). optional off
                        #   loopback is refused without accept_insecure
    accept_insecure: false   # acknowledge an insecure posture off loopback
    min_password_length: 12  # 12..256; 0 takes the engine's floor of 12.
                             #   Enforced wherever a person sets a password —
                             #   an invitation's redemption, a change, a reset
                             #   or first password link — and what
                             #   /auth/config tells a form to refuse
    tokens:             # the DEPLOYMENT's machine credentials. At least one
                        #   is required once port is set
      - id: founder     # acts under the login token:founder — lowercase
                        #   letters, digits and hyphens, optionally joined
                        #   by colons (ci-pipeline, ci:release), at most 58
                        #   characters so the login stays within 64
        token: "${CREWLET_API_TOKEN_FOUNDER}"   # 26+ chars, on the RESOLVED value
        grants: [admin]  # admin, worklet, doclet, artifacts, chatlet: what it
                        #   may do, and the most an invitation or a create it
                        #   makes may confer — the first person's included.
                        #   admin includes every app; on a node whose admin
                        #   is refuse, listing it is refused

logging:
  level: info       # debug, info (default), warn, error
  format: console   # console (default: columns and colour for a person),
                    #   text (slog key=value), json (for a log shipper)
  stderr: true      # default. `false` hands the stream to the file below and
                    #   needs one: a node logging nowhere is refused. It never
                    #   silences a boot failure or the watchdog's exit notice,
                    #   which is what it has over `2>/dev/null`
  file:             # optional — a durable copy, IN ADDITION to stderr
    path: "/var/log/crewlet/${CREWLET_NODE_ID}.log"
                    #   missing directories are created; empty writes no file
    format: json    # the file's OWN shape — empty follows logging.format
    level: debug    # the file's OWN level — empty follows logging.level.
                    #   A debug file behind a warn console, or the reverse
    max_size_mb: 100    # rotate here. There is no "never": an uncapped log
                        #   file fills the disk the store is on (default 100)
    max_backups: 5      # kept as <path>.1 (newest) … <path>.5.
                        #   0 keeps none — one file, and no history (default 5)

retention:
  backup_owner: platform-oncall   # who owns this deployment's backups: a
                                  #   person, a team, a scheduler's name. Free
                                  #   text, read by a human at the moment an
                                  #   alarm names it. `crewlet validate` warns
                                  #   when it is unset, and every start logs
                                  #   it — a company that never backs up never
                                  #   trims, so "who is responsible for this"
                                  #   has a real answer on every deployment
                                  #   that intends to keep working
```

**The sign-in settings are readable from the dashboard, not editable there.** `api.auth.totp`, `min_password_length` and `session` are this node's own file, so an administrator reads what THIS node enforces through the [`sign_in_policy`](../reference/api-endpoints.md#queries) question, every value as the engine applies it (an unset setting arrives as its default) and the session's idle lifetime beside them, which is a constant of the engine. Nodes of one fleet may disagree, and the answer names the node it describes. Changing any of them is an edit to this file and a restart.

**`crewlet validate` prints warnings as well as refusals**, and they are
separate on purpose: the exit code turns on refusals alone, so a CI step gates
on what cannot run and still prints what its operator should read. A warning is
a configuration that is valid and carries a consequence worth knowing before it
is applied — a declined fsync's window, a trim that will never advance until
somebody acknowledges a backup, a `stream.cluster.peers` entry left out of the
member count because it is this node's own route or a repeat, a node that
declares the `ingress` role and binds no port, a unit keyed on a name somebody
will rename, a company that declares no human seat and so can admit nobody.

**`validate` is not the only place a warning is said.** Most deployments
never run it — an image, a chart or an operator renders the document and
starts the node — so `crewlet run` logs every Tier A warning at every start
too, one `config_advisory` line at `WARN` each, into the log file when there is
one, and judged on the document with that run's flags folded in (see
[`crewlet run`](../reference/cli.md#every-start-says-what-its-tier-a-is-worth-knowing)).
A company's advisories are logged the same way by every node, once for each
epoch it applies, which is the only report a company seeded from a file with
`-company` gets (see
[What an applied company is worth knowing](../concepts/configuration.md#what-an-applied-company-is-worth-knowing)).

`api.port` is **one** listener per node, on every node, and `node.roles`
decides what it carries rather than whether it exists. A node without the
`ingress` role serves its two
[probes](../reference/api-endpoints.md#probes-on-a-node-without-ingress) there
and nothing else, so an orchestrator can probe a satellite exactly as it
probes an ingress node, and one Tier A file serves both shapes. There is
deliberately no separate probe port: on an ingress node the probes are already
on this listener, and a second address for the same two routes would be one
more thing to keep in step. `crewlet validate` warns about a node whose
declared roles include `ingress` while its port is `0`: the node every
integration, browser and probe looks for would bind nothing. It warns rather
than refuses, because `crewlet run -api-port` sets the port at run time — and
`crewlet run` judges the same warning on the port and roles its flags leave
the node with, logging it as `config_advisory` at every start.

`api.host` and `api.port` are what this node **binds**, which is rarely where it
**answers**: a fleet behind a load balancer binds `0.0.0.0:8000` and is reached
at `https://crewlet.example.com`. That outside address is **`api.external_url`**,
and it is **required** once a port is set on a node serving the API. It is in
Tier A rather than in the company document because it answers questions that
come before a company document has been loaded at all: the session cookie's
`Secure` flag and `__Host-` prefix follow its scheme, and its host is the
origin every write and every socket handshake is checked against. It is also
where every link the engine composes for a person points — what tool-skill
prose reaches as `${crewlet_base_url}`, a page change's `url`, an invitation
or a password link — and, on a deployment with no public listener, the base
every webhook URL and pasted app manifest is built on. The engine sits behind a
TLS-terminating proxy and cannot read any of that off the request it receives.

**`api.public` splits the routes outside parties call onto a socket of their
own.** With `api.public.port` set, the vendor webhooks and app landings
(`/webhooks/*`) and the sandbox endpoints (`/otlp/{token}`, `/mcp/{token}`) are
served on that listener and nowhere else, and every other route — the
dashboard, the REST API, the sign-in routes under `/auth`, `/config`,
`/secrets`, `/setup`, `/iam`, `/operator`, the live socket and the `/health` and
`/ready` probes — only on `api.port`. `api.port` answers a public route with a
`404 no_route` whose hint names `api.public`; the public listener answers every
other route exactly as it answers a path nothing serves, with no hint, so the
published socket says nothing about an admin port behind it, and it does so
whatever credential is sent, since it requires none. Its own outside address is
**`api.public.external_url`**, required once the port is set on an ingress
node: every webhook URL and vendor app manifest is built on it, while every
link a person follows stays on `api.external_url`. The per-run sandbox
endpoints are not: a box is handed its bridge at `CREWLET_MCP_BRIDGE_URL` and
its telemetry receiver at `CREWLET_SANDBOX_OTEL_RECEIVER_URL`. Publish
`api.public.port`, keep `api.port` private, and point both variables at the
public address (a `direct` box dials its own node on loopback with nothing to
set: an ingress node's public listener, or `api.port` on a node without the
role) — see
[Deployment → Exposing webhooks without the admin API](../guides/deployment.md#exposing-webhooks-without-the-admin-api).
A node without the `ingress` role binds no public listener at all, so one file
still serves every role: its seats' executor runs reach their tools through an
ingress node's bridge, which relays each call to them. `crewlet validate`
refuses a public listener beside `api.port: 0` on a node with the `ingress`
role (that is no HTTP surface at all, and no probe would answer); on a node
without it `api.port: 0` stays the hard off switch and binds nothing. It also refuses any two listeners the
file opens on one port — `api.port`, `api.public.port`, `stream.cluster.port`
and `stream.leaf.port` — when they bind the same address or either binds every
interface; `-roles`, `-api-host` and `-api-port` are held to the same rules.

**`api.artifacts` is where artifacts are opened**, on a listener of its own,
because an artifact is a page somebody (or some agent) wrote, and nothing it
runs may share an origin with the dashboard's session. It serves four routes:
one file of a version under a view ticket (`/t/{ticket}/...`), one asset of
the ticket's artifact (`/t/{ticket}/_assets/{asset}`), and the embedded
runtime and kit (`/_crewlet/runtime/{contract}/...`, `/_crewlet/kit/{version}/...`),
public, and cached for a year under a name that is never reused (a kit
release, or the runtime named by its bytes). Every other path, `/health` included, is an empty
`404`; it never reads a cookie and never sets one. A ticket is minted by the
dashboard for the signed-in viewer, lives an hour (never past the credential it
was minted through), and every document request is authorized again: a share
withdrawn, a session ended or an artifact trashed stops the next one. A node
whose own copy of the artifacts or identity log is out of service or more than
the stall grace behind decides through a data node whose copy answers, and
answers `503` with `Retry-After` when none does. **`api.artifacts.external_url`**
decides the origin mode: a template naming `{artifact}` at the start of its
leftmost label gives each artifact a real origin, but only on a registrable
domain other than the dashboard's (checked against the Public Suffix List); on
the dashboard's own site, or with a plain origin, every document is served with
an opaque origin, its browser storage unavailable, and `crewlet validate` warns
about the same-site template. Validation refuses an artifact origin equal to
`api.external_url`'s, `api.public.external_url`'s or an `api.auth.allowed_origins`
entry, a parent domain of the API's host, plain `http` off loopback, and a port
with no `external_url` on an ingress node. Whatever proxy stands in front of
the listener must pass the original `Host` (every request is checked against
the host its ticket's artifact is served on) and the `Sec-Fetch-*` headers
through, must leave the response headers alone, and must never cache a `/t/`
path in a shared cache. The dashboard on every ingress node names this origin
in its `frame-src`, whether or not that node binds the listener itself. A
page's policy keeps a viewer's row from running as script and its fetches,
sockets, images and media on its own origin, but a script the page's own
script creates may load from any site, so a page whose author means to can
send what its viewers may read elsewhere: share `editor` accordingly (see
the artifact note in `SECURITY.md`).

**`api.auth.admin` says whether this node serves admin**, and it is required
once a port is set: `serve`, or `refuse`. A person's
[permissions](../concepts/identity-and-access.md#access-admin-and-one-permission-per-app)
(`admin`, `worklet`, `doclet`, `artifacts`, `chatlet`) live in the replicated
store and are written by an administrator; this is each node's statement, in
the tier that holds the keyring and never read from Tier B, of whether `admin`
counts here at all. A `refuse` node clamps admin away from every principal composed on it and
keeps every app — admin includes the apps, and they are expanded before admin
is taken away — which is how a node published to the people who use the company
is kept from being one anybody administers it through, however their row reads.
A zero is refused naming both values, and any other value naming them too. It
is applied **per node, per request**, so a restart that changes it takes effect
on the next request that node serves — no apply, no migration — which also
makes a fleet whose nodes disagree a legal state during a rollout. Each node
therefore publishes its posture on its presence lease, and the fleet view
(`GET /query/fleet`) shows it as `admin` per node.

**At least one `api.auth.tokens` entry is required.** A fresh
deployment's identity estate is empty, so a Tier A token is what puts the
first person in it — invited onto, or created on, the human seat the company
declares for them: from that seat's card in **Company › Org chart**, or with
`crewlet iam invite <address> -seat <handle>` (`crewlet iam create -email
<address> -seat <handle>` creates them instead and prints their first password
link). Every person holds a human seat, so a company has to declare one before
anybody can join it. An invitation or a create confers only what its issuer
holds, so the token that brings them in has to hold `admin` — `grants:
[admin]` — on a node that serves it. On a running deployment it is the way back
in when an administrator has locked themselves out. Each entry states its own
`grants` — required and non-empty, because a credential's blast radius belongs
where it is pinned, naming only `admin`, `worklet`, `doclet`, `artifacts`
and `chatlet` (an unknown name is refused naming the set, and `admin` on a
`refuse` node is refused too) — and its value must be at least 26 characters,
checked on what the `${VAR}` resolves to. A value may not start with `cwl_ast_`, the prefix the
engine reserves for Ask Crewlet's in-process grants: the request
guard reads that shape as a grant, so such a token could never authenticate.

**`secrets.keys` is required on every node**, whether or not a port is set.
Every record on every state log is signed and verified under the keyring, the
company document a node fetches from its peers is authenticated by its seal,
and — once a port is set — it signs every session cookie and derives the key
that verifies each per-run token. `crewlet validate` refuses a Tier A file
without one; see [Configuration § What Tier A must state on every
node](../concepts/configuration.md#what-tier-a-must-state-on-every-node).

Binding a credential to a seat is in neither tier. A token acts under the
login `token:<id>`, and it acts **as a seat** when the identity directory holds
a row under that login bound to one — `crewlet iam create -kind machine -login
token:<id> -seat <handle> -grants none`, which takes `admin` — the row binds
the token to a seat and carries nothing itself, since the token's permissions
are its Tier A entry's (`crewlet iam bind`
moves an existing row onto a seat, and `crewlet iam unbind` takes it back). The
seat must be a human seat nothing else holds: no person, no other row and no
open invitation, since a seat is held by one row of the directory. Never
a `seat:` field on the token, because Tier A holds the keys to the secret store
and may never read Tier B; and never a field on the seat's `contact` block,
which says how to reach a person rather than which credential they hold. See
[Humans in the Org
Chart](../concepts/humans-in-the-org.md#acting-as-your-seat-on-the-dashboard-and-the-api).

**Give the first person `admin`, on a node that serves it.** The first person
carries what the invitation or the create that enrolled them conferred, and may
confer only what they hold, so a company whose people hold no `admin` — or
whose only `serve` node is one nobody signs in on — is one in which no person
can invite, create, move, suspend or remove anybody; `crewlet iam check` names
it `no_admin_holder`, and the Tier A token stays the way back in.

The event store (LLM observability) is a table in that same file, created by
the engine's own migrations on first start — there is nothing to configure
beyond `store.path`.  See
[Deployment → The event store](../guides/deployment.md#the-event-store)
for the full layout.

---

## Organization Structure

Roles can live in two places: at the **org root** (for org-wide agents like a CEO
or cross-cutting advisor) or **inside units** (for team-scoped agents). Both are
optional — you can have root-level roles only, units only, or both.

### Root-Level Roles

```yaml
roles:                                    # optional — org-wide agents
  - name: CEO
    goal: "Set company direction"
    manages: [vp-eng, pm-lead]            # seat HANDLES, or a unit's KEY
    # ... same role fields as unit roles (see table below)
```

A `manages:` entry is a **handle or a unit key**, never a display name. A
seat's handle is `handle:` where it declares one and the slug of its name
where it does not; a unit's key is its `id`. Both survive a rename, which a
display name is exactly what does not — so `manages: [VP Engineering]` broke
the moment somebody retitled that seat, silently, leaving a hierarchy link
pointing at nobody.

Root-level roles participate fully in the `manages[]` hierarchy and task
routing. Their knowledge is scoped to the org (visible to all agents).
They don't inherit `mcp_env` from any unit.

### Units

The `units` key defines your org as a recursive tree of `OrgUnit` nodes.
Each unit can contain roles directly and/or child units, supporting any
nesting depth — flat teams, departments with sub-teams, divisions, or custom types.

```yaml
units:
  - name: Engineering                   # required — unit name, and what people
                                        #   read: in a prompt, on a board, in a
                                        #   channel topic
    id: engineering                     # the unit's KEY and STABLE IDENTITY.
                                        #   Lowercase letters, digits, `-` and
                                        #   `_`, starting with a letter. A name
                                        #   is prose and gets renamed; an id is
                                        #   read by nobody and survives, so
                                        #   everything durable keys on the id
                                        #   when there is one. Unique across the
                                        #   chart, and it may not equal another
                                        #   unit's NAME. Adding one to a unit
                                        #   that already has work filed against
                                        #   it rewrites nothing: a filter on a
                                        #   unit matches its id and its name.
                                        #   IT DOES NOT STOP A RENAME
                                        #   RE-ONBOARDING the seats beneath it —
                                        #   onboarding turns on the NAME, which
                                        #   is what an agent reads as its team,
                                        #   so changing the name changes the
                                        #   context those seats were introduced
                                        #   with, id or no id.
                                        #   WRITE ONE. Omitted, the engine mints
                                        #   it from the name when the document
                                        #   is read — deterministically, so a
                                        #   re-import mints the same key rather
                                        #   than a second identity for one team
                                        #   — and warns on a stored revision
                                        #   that predates the field
    type: department                    # optional — unit type (default: "team")
    lead: cto                           # optional — the lead seat's HANDLE;
                                        #   inherited from the parent if omitted
    purpose: "Build and ship the product"  # optional
    children:                           # optional — nested child units
      - name: Backend                   # required
        id: backend                     # the child unit's key — same rules
        type: team                      # optional
        lead: tl                        # optional — the lead seat's HANDLE;
                                        #   inherited from the parent if omitted
        goals:                          # optional
          - "Ship features on 2-week cadence"
        channel: backend                # optional — the team's chat channel, inherited
                                        #   by children that name none. It is an
                                        #   ADDRESS: lowercase letters, digits, `-`
                                        #   and `_`, starting with a letter or a
                                        #   digit, up to 64 characters, and a name
                                        #   rather than a rendering or an id —
                                        #   `backend`, not `#backend` and not
                                        #   `C_BACKEND`. On Chatlet the engine
                                        #   opens this room and hands the name to
                                        #   agents to type into their own tools; on
                                        #   Slack and Mattermost the same grammar is
                                        #   what both accept as a channel name
        project: "BACK"                 # optional — the unit's tracker "home": routing +
        space: "BACK"                   #   write target. NOT read scope, NOT a credential,
                                        #   and NOT vendor-specific — the same keys name a
                                        #   Worklet/Doclet project/space or a Jira/Confluence one
        mcp_env:                        # optional: MCP creds shared by the unit's direct agent roles
          atlassian:                     #   (real tool credentials only; the chat transport
            JIRA_USERNAME: "${BACK_JIRA_USER}"    #   identity is per-agent; mcp-atlassian takes a
            JIRA_API_TOKEN: "${BACK_JIRA_TOKEN}"  #   token only beside its account's address)
        roles:
          - name: Tech Lead             # required — unique agent identity
            goal: "..."                 # optional — individual mission
            avatar:                     # optional (agent seats only): how the
              character: hexlet         #   dashboard draws this seat. One of the
              color: cyan               #   30 Crewlet characters and 6 colours
            backstory: "..."            # optional — personality, background, expertise
            llm: default                # optional — named LLM provider
            token_budget: {day: 200000} # optional — this seat's own ceilings per window
            handle: tl                   # optional — custom handle (default: auto-slugified)
            email: tl@company.com       # optional — agent email
            manages: [eng-a, eng-b]     # optional — hierarchy links, by seat
                                        #   HANDLE or unit key
            responsibilities:           # optional
              - "Review all PRs"
            behavioral_guidelines:      # optional
              - "Be thorough in code reviews"
            integrations:               # optional — per-agent transport identity
              mattermost:
                bot_token: "${MATTERMOST_BOT_TOKEN_TL}"
                username: tl-bot        # optional — the bot's Mattermost username
                channel: backend        # optional — a channel this bot is added to
            mcp_env:                    # optional — per-agent MCP server credentials
              atlassian:
                JIRA_USERNAME: "${TL_JIRA_USER}"
                JIRA_API_TOKEN: "${TL_JIRA_TOKEN}"
              mattermost:
                MATTERMOST_TOKEN: "${MATTERMOST_BOT_TOKEN_TL}"  # same token as role.integrations.mattermost
              github:
                Authorization: "Bearer ${GITHUB_TOKEN_TL}"
```

### Role Fields Summary

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | yes | The seat's display name — what people read. It is NOT how anything refers to this seat: `manages:` and a unit's `lead:` both take the `handle` below |
| `kind` | `agent` \| `human` | no | Who holds the seat (default `agent`). `human` marks a [human seat](../concepts/humans-in-the-org.md), addressable, never spawned; rejects every runtime-only field below. It is also where a person joins: every person holds exactly one human seat, invited onto it or created on it from that seat in **Company › Org chart** (or `crewlet iam invite -seat`), so a company that declares no `kind: human` seat at all can admit nobody, legitimate for a company its agents run alone through Tier A tokens, and named in an `advisory` warning at `roles` by `crewlet validate` and every `/config` write, and logged by every node that applies the company. Its `contact` identities are optional: a seat with none is legitimate, and the same three name it in an `advisory` warning at the seat's `contact` |
| `contact` | dict | no | A human seat's external identities — optional, see `kind` above; refused on an agent seat: `slack_user_id`, `mattermost_user_id` (a username, not an ID — the engine asks the Mattermost server which account holds it, and reports one nobody holds), `atlassian_account_id` (Jira+Confluence), `github_login`, `gitlab_username`. Each accepts a literal ID or exactly one whole-value `${VAR}` reference, resolved at use time through the [secret store](../concepts/secret-store.md) and then the environment, by every consumer alike; values are whitespace-stripped, the names of platforms that ignore case (`github_login`, `gitlab_username`, `mattermost_user_id`) are lowercased while Slack and Atlassian ids keep theirs, and a `${VAR}` embedded inside a longer string is rejected at validation (see [Humans in the Org Chart](../concepts/humans-in-the-org.md)). **No two seats may declare the same identity** — an external account belongs to one person, and a duplicate is refused rather than silently sending one of them somebody else's mail. Every one is a place a message can be sent; who a person is on the engine's own surface — the dashboard, the API, the operator tool server — is not here but in the identity directory, which binds each person to the seat they were invited or created on (`crewlet iam bind` moves them to another) (see [Acting as your seat](../concepts/humans-in-the-org.md#acting-as-your-seat-on-the-dashboard-and-the-api)) |
| `availability` | string | no | Human seats only — free-text availability rendered into rosters and `lookup_colleague` results |
| `avatar` | dict | no | **Agent seats only.** How the dashboard draws this seat wherever it shows who the seat is: `character`, one of the thirty Crewlet characters (`crewlet`, `hexlet`, `peaklet`, `prismlet`, `towerlet`, `bricklet`, `gemlet`, `coglet`, `pluslet`, `shieldlet`, `stacklet`, `sparklet`, `cloudlet`, `foxlet`, `rocketlet`, `crownlet`, `cactlet`, `wisplet`, `heartlet`, `flasklet`, `chiplet`, `duolet`, `chatlet`, `pagelet`, `archlet`, `dashlet`, `octlet`, `conelet`, `hivelet`, `folderlet`), and `color`, one of `purple`, `cyan`, `green`, `amber`, `rose` or `blue`. Either may be left out: an absent character is `crewlet`, the original, and an absent colour is `purple`, so a seat that says nothing is drawn as every agent was before a seat could choose. Presentation only: no prompt, route or placement reads it, and it survives a rename because nothing derives it from the name. An id outside those lists, or the block on a human seat (a person is drawn as themself), is refused by an [admission rule](../concepts/configuration.md#what-a-stored-revision-is-held-to). The org builder offers both as pickers in the seat's editor and in the Add dialog, which suggests a character no agent wears yet |
| `goal` | string | no | Individual mission statement |
| `backstory` | string | no | Personality, background, expertise |
| `llm` | string or dict | no | Named LLM provider key — **the executor's**, and so the seat's own model. Dict form adds `default`, `review`, `subagent`, `auxiliary`, `judge` keys for the [per-phase model split](../concepts/turn-engine.md#per-phase-llm-models); there is no `execute` key, because `llm` is it, and no `sandbox` key, because the executor's run is the seat's code work. Only `claude-code` and `opencode` entries can run the executor: a seat whose chain holds neither is refused at its `llm` (a seat naming no `llm` at the fallback entry's `agent`), naming both remedies, and a chain mixing one with another CLI validates with an advisory warning, since a launch skips the other CLI, which still serves the reviewer, workers and side passes |
| `llm_review` / `llm_subagent` | string | no | Per-phase overrides (alternative to the dict-shaped `llm`) |
| `llm_auxiliary` | string | no | Cheap/fast model used by the turn-start prefetch, the condensing of large tool results, and the reflection workers (PersistDecider, episode summariser). Each call pays a CLI launch under a fixed deadline, so name a fast model — see [Auxiliary work and latency](../concepts/llm-backends.md#auxiliary-work-and-latency) |
| `llm_judge` | string | no | Cheap/fast model used by the [round-cap extension judge](../concepts/turn-engine.md#round-cap-extension-judge) of the onboarding pass; falls back to `llm` |
| `token_budget` | dict | no | This seat's own token ceilings per calendar window — `day`, `week`, `month`, each optional — on top of the company's. See [Token budgets](#token-budgets) |
| `learning_enabled` | bool | no | Overrides the company's [learning](../concepts/agent-learning.md) setting for this seat alone. Unset inherits it; `false` skips reflection for this seat's turns while still writing its episodes, which are cheap and useful to a person regardless |
| `handle` | string | no | Custom identity slug (default: auto-derived) |
| `email` | string | no | Seat email address. **No two seats may declare the same one** — the party registry keys on it and the first seat wins, so all but one silently stop being findable by address and mail meant for one person resolves to another. Compared case-insensitively, the way the registry folds it |
| `unit` | string | no | **Root-level seats only.** The **key** of the unit this seat belongs to; the seat is moved into that unit's members before anything else reads the org chart, exactly as if it had been written under the unit. It is what `PUT /config/roles/{handle}` writes, so an API-created seat can land in a team without rewriting the unit's block; a hand-written file normally nests the seat under its unit instead. A name that matches no unit leaves the seat at the root and is reported by `crewlet validate` as a dangling reference. On a seat already nested in a unit it moves nothing, and one naming a *different* unit is refused |
| `manages` | list[string] | no | Seat **handles** or unit **keys** this agent manages; a unit key expands to that unit's seats. Never a display name — a name is prose that gets renamed and a handle is not |
| `responsibilities` | list[string] | no | Role responsibilities |
| `behavioral_guidelines` | list[string] | no | Behavioral rules |
| `mcp_env` | dict | no | Per-agent MCP server credentials, keyed by server name — env vars for `stdio` servers, HTTP headers for `http` servers (e.g. `atlassian.JIRA_USERNAME` / `atlassian.JIRA_API_TOKEN`, `confluence.CONFLUENCE_USERNAME` / `confluence.CONFLUENCE_API_TOKEN`, `slack.SLACK_MCP_XOXB_TOKEN`, `mattermost.MATTERMOST_TOKEN`, `github.Authorization: "Bearer …"`). The per-agent tool-credential surface only — scope a server via its own filter (`JIRA_PROJECTS_FILTER` / `CONFLUENCE_SPACES_FILTER`) if needed. The unit's project / space identity is `project` and `space` (below), not here. **Every key must name a declared [`mcp_servers`](#mcp-servers) entry**, because the engine looks credentials up per *server*: a block keyed on a name nothing declares is never read by anything, so it sits in the document looking configured while the server it was meant for starts with none. Six names are the exception and need no server, because the engine reads those blocks itself — `github`, `gitlab`, `datadog`, and Atlassian's `atlassian`, `jira` and `confluence` |
| `integrations.github` | dict | no | This seat's own [GitHub App](../integrations/github.md#one-github-app-per-agent), one per agent, because an app has exactly one bot identity. A person writes three fields: `enabled` (required; off by default for a new seat), `permissions` (GitHub's own permissions, `contents`, `issues`, `pull_requests`, `checks`, `actions`, `deployments` and `workflows`, each `read` or `write`; a permission left out is No access, see [Access in GitHub's own terms](../integrations/github.md#access-in-githubs-own-terms)) and `repos` (the repositories it works in as `owner/name`; empty means every repository the installation covers). `app_id`, `app_slug`, `installation_id`, `private_key` and `webhook_secret` are **written by the engine** when the app is created and installed, the last two as `${VAR}`s pointing at the [secret store](../concepts/secret-store.md) (see [What lands where](../integrations/github.md#what-lands-where)) |
| `integrations.slack` | dict | no | This seat's own Slack app: `enabled`, and, for an app created by hand, its `bot_token` and `signing_secret` as whole `${VAR}`s. With an app configuration token on the company block the engine creates the app and seals both under names it derives from the handle, so a block that is only `enabled: true` is the ordinary one; the seat speaks in its unit's `channel` |
| `integrations.mattermost` | dict | no | This agent's Mattermost bot: `enabled` (required), `roles` (the Mattermost roles its bot holds: `system_user`, plus `system_post_all_public` or `system_post_all`, or `system_admin`; required while on where the engine provisions the bot) and `channels` (within `integrations.mattermost.provisioning.channels`; empty means no channels). Where the engine provisions Mattermost it creates the bot and seals its token under a name derived from the handle, so the seat names no credential. Where it does not (a bot made by hand), `bot_token` names the bot's token as one whole `${VAR}` no other seat or company block names, and the engine binds it to the transport and the Mattermost tool without ever writing it. `username` names the bot, defaulting to the handle. See [Each agent's access](../integrations/mattermost.md) |
| `project` | string | no | **Authored on a unit or root-level role** (→ `org.Unit.Project` / `org.Role.Project`). The team's tracker project as identity: an item that names nobody in the org chart routes to the unit lead, it is the project the team files work under, and on Worklet it is what an item filed with no `unit` belongs to, whoever filed it (see [Worklet](../guides/worklet.md#which-team-an-item-belongs-to)). **Vendor-neutral**: it names a Worklet project or a Jira one, whichever [`tracker.backend`](#tracker) the company runs, so switching backends does not rewrite the org chart. **Not** an MCP credential, and it does **not** scope knowledge reads. Keys are an upper-case letter plus 1–9 upper-case letters or digits (`ENG`, `PROD`), which is the shape every backend accepts |
| `space` | string | no | **Authored on a unit or root-level role** (→ `org.Unit.Space` / `org.Role.Space`). The team's knowledge container as identity: a page change that names nobody routes to the unit lead, and it is where the team writes. Vendor-neutral and shaped like `project`, above. It does **not** scope reads — read scope is the org-wide `knowledge.scope` only. The reserved containers (`knowledge.skills_container`, default `TS`, and `knowledge.root_space`, default `HOME`) are refused here |
| `workers` | list[string] | no | Which [worker templates](#worker-templates) this seat may delegate to. **Empty means every one** — a company that publishes three workers wants its seats using them, and requiring each seat to opt in turns a shared library into per-seat copy-paste. A name no template defines is refused at load |
| `sandbox` | dict | no | What this seat's [executor box](../concepts/code-sandbox.md#a-seats-own-box-settings--rolesandbox) reads beyond `providers.sandbox`. Every agent seat's executor is its coding CLI's own run in a box, placed by the `run_in` of the `providers.llm` entry the launch picks (empty takes `providers.sandbox.default_run_in`), so the block places nothing and gates nothing: **absent means the engine-wide settings alone**. Six keys: `setup`, per-seat provisioning applied after `providers.sandbox.setup`; `env`, the environment injected into the run and where external-service tokens are declared — a `${VAR}` in it that resolves to nothing is left out and the run still starts, its start logging `sandbox_env_unresolved` naming the key ([credentials and the run environment](../concepts/code-sandbox.md#credentials-and-the-run-environment)); `optional_keys`, the keys of `env` whose absence is expected and so left out without that line — a setup step declares its own `optional_keys`, over its `env` and the paths of its `files`; `mcp.servers`, which of the seat's MCP servers the coding agent gets beside its tool bridge; `pause_ttl_seconds`, how long a box blocked on a person's answer stays paused (unset inherits `providers.sandbox.default_pause_ttl_seconds`, `0` never holds a paused box, negative refused); and `max_turns`, the agentic-round cap on one of the seat's runs (unset inherits `providers.sandbox.default_max_turns`, 96 when that is unset too; `0` uncapped; negative refused). Any other key is refused as unknown. A setup step's `files` entry — the seat's own or an engine-wide one — that a box the seat runs in would refuse is refused on the write, naming the step, the path and the seat: a `direct` box writes a file only inside its own home, which it mints per box, so it takes a relative path and refuses every absolute one, and a `container` box writes one only under its home mount at `/home/user`. An `e2b` box writes a file at any other path, but refuses another account's home (`~name/…`) and a path naming a directory, so the home itself and a directory above it are refused on the write as well. Name such a file relative to the box's home instead (`.npmrc`, or `~/.npmrc`), which every cell writes under its home, or have a command of the step write it under `"$HOME"` ([setup steps](../concepts/code-sandbox.md#setup-steps--provisioning-the-box)) |
| `placement` | dict | no | Which nodes may run this seat; absent means any node that runs seats (see [Placement](../guides/fleet.md#placement)). Agent seats only: a human seat is never claimed, so one carrying a placement is refused. `node` pins it to one node id and `labels` names pairs a node must **all** carry under [`node.labels`](../concepts/configuration.md#nodelabels); give both and both must hold. Everything is compared exactly against what a node advertises, so each part takes the shape a node's own is held to. A **label key** follows the node-label grammar: not empty, at most 63 bytes, and no whitespace or unprintable character anywhere (`zone`, `topology.example.com/rack`). A **label value** may hold spaces inside it or be empty, but none around it, since a node's values are trimmed. **`node`** is a node id: it starts with a letter or digit and holds only letters, digits, `.`, `_` or `-`, at most 64 characters — and it is compared as written, so a `${VAR}` is not resolved here. A selector outside these shapes could never match a node, so it is refused at validation, naming the field, rather than left for the running fleet to report as `seats_unplaceable` |
| `schedules` | list | no | Role-scoped recurring tasks — see [Schedules](#schedules) |

### Worker templates

`workers:` holds the reusable delegates a seat's executor hands work to with the `delegate` tool, keyed by the name it types into that call. A template carries the half of a worker that does not change per task — the persona, the tool set, the model, and the shape of the answer — so a seat writes only the task.

```yaml
workers:
  researcher:
    description: reads sources and reports findings with citations
    system_prompt: |
      You research things carefully and report only what you can point at.
    tools: [confluence_search, confluence_get_page]   # a REQUEST, not a grant — see below
    model: fast                                       # a providers.llm key; omit for llm_subagent
    max_turns: 12                                     # omit for turn_engine.delegation.max_turns
    output:                                           # omit for the default {result, notes}
      type: object
      properties:
        findings:  {type: string}
        citations: {type: array, items: {type: string}}
      required: [findings]
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `description` | string | yes | What this worker is for, written for the **executor choosing one** rather than for the operator. It is the only part of the template that reaches the parent's prompt |
| `system_prompt` | string | yes | The worker's persona and standing instructions. The runtime preamble (no nesting, no colleague contact, how to answer) is appended, so a template never restates the boundary and cannot weaken it by forgetting to |
| `tools` | list[string] | no | The tools this worker asks for. Empty means none at all, which is the right shape for a summariser. **Naming a tool grants nothing**: every name still passes the worker filter — the caller's own live tools, minus the engine-control denylist, minus shared-surface writes — so `workers:` is never a privilege-escalation path |
| `model` | string | no | A `providers.llm` key. An explicit key gets **no fallback chain**: an operator's cheap-model choice that quietly ran somewhere else is worse than a refusal. Omit to take the seat's `llm_subagent` chain |
| `max_turns` | int | no | Tool rounds this worker may run. Refused at load — not clamped — above `turn_engine.delegation.max_turns_ceiling`: a template is an edit, and an operator overruled by a number nothing in the file mentions writes it again next time |
| `output` | dict | no | The JSON Schema the worker's `submit_result` publishes, so the answer comes back as fields the parent can index rather than prose it re-parses. Must be an object schema with 1–12 named properties, at most 3 levels deep, whose `required` entries are properties it actually has; keywords the engine does not read are passed through to the provider untouched |

See [Turn Engine — Workers](../concepts/turn-engine.md#workers) for what a `delegate` call looks like and how dependency waves work.

### Schedules

Roles **and** units can own recurring work via a `schedules:` list (a
cron analogue). Each entry fires a task on its cron expression; see the
[Scheduling](../concepts/scheduling.md) concept doc for the full design.

```yaml
units:
  - name: Backend
    id: backend
    type: team
    lead: backend-lead
    schedules:
      # unit schedule, target defaults to `each` → every direct member runs it
      - name: daily-standup
        cron: "30 9 * * 1-5"            # 5-field cron, evaluated in `timezone`
        timezone: Europe/Amsterdam      # IANA tz; defaults to the company's `timezone`
        task: "Post your standup: shipped yesterday / on today / blockers."
      - name: weekly-report
        cron: "0 16 * * 5"
        target: lead                    # unit schedules: each | lead
        task: "Collect the week's progress and post a summary to the team channel."
    roles:
      - name: Backend Dev
        schedules:                      # role schedule → runs as this role
          - name: morning-smoke
            cron: "0 9 * * 1-5"
            task: "Run the smoke-test pipeline and triage failures"
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | yes | Unique within the role/unit; part of the idempotency key |
| `cron` | string | yes | Standard 5-field cron (`min hour dom month dow`), evaluated in `timezone` |
| `task` | string | yes | Task prompt handed to the runner agent |
| `timezone` | string | no | IANA timezone this one schedule fires in (default: the company's [`timezone`](#the-companys-clock)). Not `Local` or `localtime` |
| `target` | string | no | **Unit schedules only**: `each` (default — every direct member) or `lead` (the effective unit lead). Ignored for role schedules; for a per-person task, use a role schedule |
| `enabled` | bool | no | `false` keeps the schedule in config without firing (default `true`) |
| `timeout_seconds` | int | no | Hard wall-clock cap on the scheduled turn, its executor's run included (default `1800`, half an hour: [why](../concepts/scheduling.md#hard-wall-clock-timeout)) |
| `catchup` | bool | no | Fire one recent missed tick on restart (default `true`) |

> Scheduling requires a database (for the at-most-once `scheduled_runs`
> ledger). Schedules are **not** inherited by child units.

---

## Integrations

Inbound / notification integrations live under a single `integrations:` block — admin credentials, webhook secrets, and outbound `transports`. These carry only **non-tool** config; the MCP **tool** servers are declared in [`mcp_servers`](#mcp-servers), and each agent's per-server credentials live in `role.mcp_env`. It's the inbound mirror of `mcp_servers` (outbound tool actions).

```yaml
integrations:
  forge_app_id: "ari:cloud:ecosystem::app/your-forge-app-id"   # Jira Cloud's delivery path

  jira:
    url: "${JIRA_URL}"                    # a Data Center instance, or a Cloud site
    # cloud_id: "${JIRA_CLOUD_ID}"        # an Atlassian Cloud id — give this OR url
    # site_url: "https://acme.atlassian.net"  # with cloud_id: the base for links people open
    token: "${JIRA_API_TOKEN}"            # API token (org read account)
    email: "${JIRA_EMAIL}"                # Cloud only — the account's email, for Basic auth
    webhook_secret: "${JIRA_WEBHOOK_SECRET}"  # HMAC-SHA256 secret — required on Data Center

  confluence:                            # the knowledge base
    url: "${CONFLUENCE_URL}"              # a Data Center instance, or a Cloud site
    # cloud_id: "${CONFLUENCE_CLOUD_ID}"  # an Atlassian Cloud id — give this OR url
    # site_url: "https://acme.atlassian.net"  # with cloud_id: the base for links people open
    token: "${CONFLUENCE_API_TOKEN}"      # API token (org read account)
    email: "${CONFLUENCE_EMAIL}"          # Cloud only — the account's email, for Basic auth
    webhook_secret: "${CONFLUENCE_WEBHOOK_SECRET}"  # Data Center only — HMAC-SHA256 secret

  slack:                                 # per-seat apps live on each role
    typing_status: always                # working indicator: always (default) | addressed
    status_phrases:                      # optional — replaces the built-in wording, per phase
      execute: ["is nimbusing...", "is thinking very hard..."]
      review:  ["is re-nimbusing...", "is double-checking..."]

  mattermost:                            # self-hosted chat — transport AND inbound fleet
    enabled: true
    url: "${MATTERMOST_URL}"             # instance base URL (required when enabled)
    team: nimbus                         # team slug (required when enabled)
    typing_status: always                # working indicator: always (default) | addressed
    provisioning:                        # how the engine provisions each agent's bot
      admin_token: "${MATTERMOST_ADMIN_TOKEN}"  # a system admin's token
      channels: [town-square, engineering]   # the channels agents may be given

  github:
    enabled: true
    # url:  "https://github.example.com"       # Enterprise Server; omit for github.com
    webhook_secret: "${GITHUB_WEBHOOK_SECRET}"   # HMAC-SHA256 secret (required when enabled)
    token: "${GITHUB_ENGINE_TOKEN}"              # read credential for participant fan-out
    provisioning:                                # read by the reconcile loop AND `crewlet github provision`
      org: nimbus                                #   (organization holding the repositories)
      repos: [nimbus/api, nimbus/web]            #   (extra owner/repo entries to hook)
      org_webhook: auto                          #   auto | true | false

  gitlab:
    enabled: true
    url: "https://gitlab.com"                    # instance base URL (required when enabled)
    signing_secret: "${GITLAB_SIGNING_SECRET}"   # 19.1+ whsec_ HMAC — the only verification mode, required
    token: "${GITLAB_ROUTING_TOKEN}"              # optional read PAT → participants-based routing
    provisioning:                                # read by the reconcile loop AND `crewlet gitlab provision`
      group: nimbus-hq                           # top-level group agent service accounts join
      access_level: developer                    # developer | maintainer

```

> **There is no address in this block.** Where people reach this deployment is [`api.external_url`](#tier-a), and where vendors and sandboxes reach it, when it publishes a listener for them alone, is `api.public.external_url` — both in the operator's own Tier A file, because the session cookie and the origin check are decided *before* a company document is loaded, and are part of what authenticates the request that would go on to load one. `api.external_url` is also what [tool-skill](../concepts/tool-skills.md#skill-variables) prose reaches as the reserved variable `${crewlet_base_url}`, which is why a company may not declare a `skill_variables` entry of that name.

- **`forge_app_id`** — verifies the Forge Invocation Token (FIT) on Cloud webhooks against Atlassian's JWKS; the `aud` claim must match. Required when Jira Cloud delivers through the Forge app.
- **`atlassian`** — the Atlassian organization one service account per agent seat is created in, plus which Atlassian the company runs. `deployment` is `cloud` or `data_center`; the dashboard's form opens on `cloud`, and a value written here also decides the deployment of a `jira` or `confluence` `url` whose host is not one of Atlassian's own — which is how an Atlassian Isolated Cloud site, at a base domain of its own, is configured. `org_id` and `api_key` (an **unscoped** organization API key) are Cloud only. See [Jira](../integrations/jira.md#configuration).
- **`jira`** — the Atlassian tracker, served end to end. Give `url` **or** `cloud_id`, never both — they are two ways to name one instance and `crewlet validate` refuses the ambiguity. `token` is the org read account (an issue's watchers are the one routing input a webhook never carries); `email` switches authentication to Cloud's Basic scheme; `site_url` is the human base for links when the instance is named by a cloud id, and the Cloud whose gateway that id is read through (Government Cloud's for a `…atlassian-us-gov-mod.net` site). Which deployment `url` names is derived from its host — and, for a host that is not Atlassian's own, from `atlassian.deployment`. `webhook_secret` is **required for Data Center**; on Cloud it signs the admin webhook the engine registers, and is not required because a company on the Forge relay has no HMAC in its path. Each seat's own credential lives in `mcp_env.atlassian` (or `mcp_env.jira`) and is what the engine resolves its account id from — see [Jira](../integrations/jira.md).
- **`confluence`** — the knowledge base, and the **query-time search** behind every turn's "Relevant knowledge" block and the `search_knowledge` tool. Same address rule as `jira`: `url` **or** `cloud_id`, never both. `token` is the org read account a seat with no Confluence credential of its own searches under; a seat WITH one searches as itself and Confluence enforces its page permissions natively. `webhook_secret` is required for Data Center and unused on Cloud. The knowledge backend is **single-homed** — the engine wires exactly one `knowledge.Searcher`, because two would make an agent's answer to "what do we already know about this" depend on which one was asked. Scope reads with `knowledge.scope`; publish with [`crewlet confluence import`](../reference/cli.md#crewlet-confluence-import). See [Confluence](../integrations/confluence.md).
- **`slack`**: the hosted chat backend. The org-level block carries **no credentials at all**: Slack gives each agent its OWN app, so the token and signing secret live on each role's `integrations.slack`, and this block holds only the working-indicator settings. **`typing_status`** takes `always` / `addressed` and defaults to `always`, and Slack is where that default costs least: its indicator renders TEXT, so a phase change is something the person waiting can actually read, which is also what makes `status_phrases` worth having here. There is no `off`. Inbound events arrive per seat at `/webhooks/slack/{handle}`, verified against that seat's own signing secret. With `app_config_token` (a whole `${VAR}`, entered on the connect form), the engine creates and keeps every agent's app itself. See [Slack Integration](../integrations/slack.md).
- **`mattermost`** — the self-hosted chat backend, and the one integration that is **both** inbound and outbound: enabling it starts the outbound transport *and* the websocket fleet that holds one connection per agent seat (Mattermost has no usable inbound webhook, so nothing has to reach the engine — no public URL, no tunnel). `url` and `team` are both **required** when enabled. Per-agent identity lives on each role's `integrations.mattermost.bot_token`, named again as `mcp_env.mattermost.MATTERMOST_TOKEN` for the MCP subprocess. **`typing_status`** takes `always` / `addressed` and defaults to `always`, which on this backend is the expensive one: the indicator has to be re-asserted every few seconds rather than every 45, so a multi-minute turn costs one to two orders of magnitude more requests than Slack's for strictly less information. Most Mattermost deployments want `addressed`. There is deliberately no `status_phrases`: Mattermost renders a fixed client-side indicator with no API for the text. The `provisioning:` sub-block is how the engine provisions each agent's bot: its administrator token, and the channels an agent may be given on the integration's [Agents tab](../integrations/mattermost.md#each-agents-access-roles-and-channels). A company may run Mattermost and Slack together — they are different workspaces with different people in them, and an org migrating from one to the other runs both for a while. See [Mattermost Integration](../integrations/mattermost.md).
- **`github`** — the hosted code host, served end to end. `url` is **optional**: leave it unset for github.com, whose API lives on a different host rather than a path on the web UI, and name an Enterprise Server there — the REST base is derived either way. `webhook_secret` is **required** when enabled, and takes any string (GitHub signs with it verbatim, so unlike GitLab's there is no shape to get wrong). The optional `token` is a read credential for **participant fan-out** — a payload carries the author, assignees and requested reviewers but not who has commented or reviewed — and it is what `crewlet github provision` registers webhooks with. Each seat's own app token is issued by the engine at use, under a name only that seat resolves ([One GitHub App per agent](../integrations/github.md#one-github-app-per-agent)), and is what the engine resolves its login with; a human seat is reached by `contact.github_login` instead. The `provisioning:` sub-block is read by [`crewlet github provision`](../reference/cli.md#crewlet-github-provision) **and** by the engine's own reconcile loop, which registers and heals the hooks it names: `org_webhook: auto` takes one organization-level hook where the credential may (covering repositories created later) and falls back to one per repository where it may not. A company may run GitHub and GitLab together — they are two hosts with different repositories on them. See [GitHub Integration](../integrations/github.md).
- **`check_interval_seconds`** — how often a **converged** integration is read back, and therefore how long access somebody revoked by hand at a vendor goes unnoticed. Unset takes **600** (ten minutes); the floor is **60**, and a shorter value is refused naming the field rather than clamped. It is the only thing that ever finds a revoked credential, weighed against what a converged pass costs at the vendor — one read per seat and per project, per surface, per interval — so a small company can afford `60` and a large one should lengthen it. Zero means unset, never "off". See [Integration Reconcile](../concepts/integration-reconcile.md#the-cadence-follows-who-has-to-act).
- **`gitlab`** — webhook config + boot-time identity resolution. `url` and `signing_secret` are both **required** when enabled — inbound webhooks are verified by the GitLab 19.1+ signing-token HMAC only (the plain `X-Gitlab-Token` scheme is unsupported; self-managed < 19.1 is not supported). The optional `token` (a PAT with the `read_api` scope, which you supply: nothing provisions it) enables **participants-based routing** — comments and state changes reach everyone participating in the issue/MR, not just assignees and mentioned users. `webhook_name` (default `crewlet`) names the hooks this deployment owns, so a change of public base re-points them instead of leaving a live orphan at every level; give two deployments watching one instance two names. The GitLab MCP server is a `shared: false` [`mcp_servers`](#mcp-servers) entry — by default the official `glab mcp serve` (stdio, spawned per-role by the engine, no separate server), whose own `env` sets `GLAB_CHECK_UPDATE`, `GLAB_SHOW_WHATS_NEW` and `GLAB_SEND_TELEMETRY` to `"false"` so that what glab prints on its own never lands in a tool result and no call sends the instance a usage event (a variable set only in the engine host's environment does not reach it: a stdio server is handed an allowlist of that environment plus what its configuration declares; see [MCP tool server](../integrations/gitlab.md#mcp-tool-server)) — and each agent's service-account token is the engine's to mint and seal where it provisions GitLab, bound to the tool by the server's `seat_credentials`; where it does not, the seat's `mcp_env.gitlab` names the token as an exclusive `${VAR}`. The `provisioning:` sub-block is read by `crewlet gitlab provision` **and** by the engine's own reconcile loop; `mode` (`group` by default, or `instance` on a self-managed instance) says where a service account is owned and therefore which route creates it, mints its tokens and deletes it — the CLI's `-mode` overrides it for one run. See [GitLab Integration](../integrations/gitlab.md).
- **`datadog`** — monitor alerts as inbound events, so a firing monitor wakes a seat the way a comment on a merge request does. `webhook_token` is **required** when enabled and is compared constant-time against `X-Crewlet-Token`: Datadog attaches headers with fixed values only, so nothing varies with the payload to sign and this token is the whole authentication — which is why it must be at least **26 characters**, the length the dashboard's Generate button and `crewlet` itself mint. A route with nothing to compare against answers **503** rather than accepting a delivery. `route_to` is **required** and names an **agent** seat this company declares, or the literal `none` to dismiss alerts nobody owns on purpose; a handle no seat has, or one naming a human seat, is refused, and no seat may itself be handled `none`. `provisioning` is **required** too — `site` (the region the keys were issued in, checked against Datadog's own list), `api_key` and `app_key` — because the engine registers the webhook that makes an alert arrive at all, and an enabled block without it reports itself connected and receives nothing. `webhook_name` (default `crewlet`) is the name of that definition and therefore the handle a monitor writes, `@webhook-crewlet`; give two deployments watching one organization two names. `handle_tag` (default `crewlet`) is the monitor tag key that names a seat, so a monitor tagged `crewlet:sre-lead` reaches that seat. See [Datadog Integration](../integrations/datadog.md).
- **`transports`** — outbound delivery transports (e.g. `email`). The Mattermost and GitLab transports are auto-derived from the sections above; this list adds any others. Jira is inbound-only by design and has no transport: an agent's writes to an issue go through its own MCP tools under its own credential, never through the engine. Confluence is inbound-only for the same reason — a page an agent writes is written by its own MCP tools. Slack is the exception among the chat backends: its transport is per seat, derived from each role's own app credentials rather than from an org-level block, so it is not listed here either.

## Knowledge

```yaml
knowledge:
  backend: confluence                    # doclet (default) | confluence | none
  scope: ["ENG", "HANDBOOK"]             # org-wide containers every agent can search (optional)
  skills_container: TS                   # tool-skill pages; excluded from routing and search
  root_space: HOME                       # the organisation's own pages, e.g. the root Onboarding page
  vectors: true                          # fuse semantic recall; unset derives from providers.embeddings
```

`knowledge.backend` is **which knowledge base this company runs**, and there is exactly one: two would make an agent's answer to "what do we already know about this" depend on which was asked. Leaving it unset **derives**: `confluence` when an `integrations.confluence` block is declared, `doclet` otherwise, so a company that has configured nothing gets a wiki, and an Atlassian company that has not read this note still searches Confluence. Naming `doclet` **beside** an `integrations.confluence` block is refused: pages would live in two places with nothing keeping them in step. `none` is a real posture: the `## Relevant knowledge` block stays empty and `search_knowledge` is not registered.

`knowledge.scope` is the org-wide read scope, materialised onto `org.Organization.KnowledgeScope` and read by whichever searcher is wired. **Empty means unscoped, and what unscoped MEANS differs by backend:** on Doclet it is the whole company, because the engine is the boundary: every reader is a seat of one company and there is no second account to launder a read through. On Confluence it is whatever the asking seat's own account can read, which is why a credential-less seat searching unscoped there gets nothing: an unscoped query on the shared org token is how one seat reads a page its own account never could. Set it only to **narrow** to a curated floor.

`knowledge.skills_container` (default `TS`) holds [tool-skill](../concepts/tool-skills.md) pages and `knowledge.root_space` (default `HOME`) holds the organisation's own pages, starting with the root Onboarding page every seat reads first. Both are **reserved** — refused as a unit's own `space`, and held back from agents' writes — but not for the same reason, so not in the same way ([who may write where](../concepts/knowledge-system.md#who-may-write-where)). The skills container is machinery: excluded from knowledge search and from routing, closed to every agent write, and written by a person only with `admin`. The root is the company's own canon: searched and routed like any other container, and closed only to an agent adding a new page. `skills_container` is three-valued — absent takes the default, a name takes that container, and an explicit `""` turns tool skills **off** entirely.

`knowledge.vectors` fuses semantic recall into the search. **Unset derives** from whether `providers.embeddings` is configured — a company already paying for embeddings for its diary gets the better search — and an explicit `true` with no provider is refused, because there would be nothing to compute an embedding with.

---

## Tracker

```yaml
tracker:
  backend: worklet                       # worklet (default) | jira | none
```

Which **work tracker** this company runs, on exactly the terms `knowledge.backend` runs on. Unset derives `jira` when an `integrations.jira` block is declared and `worklet` otherwise; `worklet` beside `integrations.jira` is refused, because work would be filed in two places and a unit's `project` key would name two trackers. `none` is a company with no work tracker at all: nothing is filed or routed as a work item, and a seat is woken by everything else: chat, schedules, an `a2a_ask`, and the webhooks of the integrations the company has connected, a GitHub or GitLab issue among them.

The two axes are **separate** on purpose. A company running Worklet against a Confluence wiki, or Jira against Doclet, is an ordinary arrangement rather than a mixture to refuse: they are two products with separate routing and separate lead maps.

The backend each axis resolves to, and nothing else of either block, is published to every signed-in reader as [`services` on the org answer](../reference/api-endpoints.md#get-org), because it decides which of the dashboard's own surfaces exist: a company that does not run Worklet has no Worklet or My work there, and one that does not run Doclet has no Doclet.

### Worklet's own policy

```yaml
tracker:
  backend: worklet
  worklet:                               # ONLY on a Worklet company: an inbox
                                         #   horizon on a company running Jira
                                         #   describes nothing, and is refused
    inbox_retention_days: 365            # how long a person's inbox keeps a row
                                         #   (default 365, 30..3650). THE HISTORY IT
                                         #   POINTS AT IS UNTOUCHED — this is a
                                         #   mailbox horizon, not an archive one, and
                                         #   "what was I told about last year" is
                                         #   answered by the history either way
```

Everything else a tracker could be told is either a fact about the **operator** — how they back up, how long their disk holds a replay window — which lives in Tier A under [`stream.tracker_retention`](#tier-a), a fact about the whole **company** — the clock its dates mean is the top-level [`timezone`](#the-companys-clock) — or a decision the engine makes once for everybody.

**Every broker member needs a stream that survives a restart.** A state-log domain's write-ahead log lives on the stream, and an embedded member with no `stream.store_dir` — a single node included — keeps its streams in memory, so a restart recreates them empty, and a node whose durable tables are ahead of a stream that restarted from nothing refuses to serve permanently, with no snapshot that helps. `crewlet validate` refuses it from the Tier A file alone, and so does the engine at boot. A node without `data` is no member: it joins the members' broker as a leaf and keeps no stream of its own, so it is refused a `store_dir` instead, and a node dialling an external cluster keeps nothing either — that cluster's storage is its operator's.

It applies whatever backends a company names, and whether the node has a company at all. Worklet, Doclet or Chatlet puts its items, its pages or its messages there, but the **identity estate** (the people who can sign in) is a domain too, and every node that holds the estate runs it from boot. This rule used to be judged on the two files together and ask about the backends: it let a company on Jira and Confluence through, and then a node started with no company, both of which started logs in memory and lost them on the first restart with nothing having warned.

---

## Chat

```yaml
chat:
  backend: chatlet                       # chatlet (default) | vendor | none
```

Which **chat surface** this company talks on, the third axis and the same shape as the two above. Unset derives `vendor` when the company declares a vendor chat surface and `chatlet` otherwise — and for chat "declares" means `integrations.mattermost`, `integrations.slack`, **or any seat's own `integrations.slack` app**, because Slack's credentials are per seat and a company with seven working Slack apps may never have written the org-level block at all. Naming `chatlet` beside any of them is refused: conversations would live in two places, a person would reply in one of them, and nothing anywhere could say which thread was the real one. Naming `vendor` with none of them declared is refused too, since there is no surface to talk on. `none` is a company whose seats are woken by the tracker, the code host and schedules only.

The value is `vendor` rather than a product name — the other two axes name theirs — because a company legitimately runs **both** Mattermost and Slack: they are different workspaces with different people in them, and an org migrating between them runs the pair for months.

The backend this axis resolves to is published to every signed-in reader beside the other two, as `chat` in [`services` on the org answer](../reference/api-endpoints.md#get-org): a company that does not run Chatlet has no Chatlet in the dashboard. Which vendors it talks on is told only to a reader holding Chatlet.

See [Chatlet](../concepts/chatlet.md) for what Chatlet is, who a message wakes, and what it deliberately does not do.

### Chatlet's own policy

```yaml
chat:
  backend: chatlet
  chatlet:                               # ONLY on a Chatlet company — a retention
                                         #   horizon on a company whose messages
                                         #   live in Slack describes nothing, and
                                         #   is refused
    message_retention_days: 365          # how long a message lives (default 365,
                                         #   30..3650). 0 KEEPS MESSAGES FOR EVER,
                                         #   which is a different setting from
                                         #   leaving the field out — chat is the
                                         #   highest-volume thing the engine
                                         #   stores, and every message sits in
                                         #   every snapshot and every node join
    default_channel_private: false       # create a channel private when the caller
                                         #   names no visibility. Default false:
                                         #   a company's rooms are readable by its
                                         #   own seats
    typing_status: always                # always (default) | addressed — the same
                                         #   closed set, and the same meaning, as
                                         #   integrations.slack.typing_status
```

`message_retention_days` is **three-valued**, and the difference matters: leaving it out takes a year, `0` keeps messages for ever, and a number between 30 and 3650 is that horizon. Note that `tracker.worklet.inbox_retention_days` reads `0` the other way — as "take the default" — because an inbox row is derived from history that answers for ever, while a message is the only copy of what somebody said.

Everything else Chatlet could be told is either the operator's — how big the log may grow (Tier A's `stream.chat_log_max_bytes`) and how long its replay window is kept — or a decision the engine makes once for everybody. How much of a thread a woken seat is shown is one of those: the engine reads the thread at turn start for every chat surface alike and bounds it at one byte budget, keeping whole messages and condensing the middle of a long thread (see [the thread a turn was woken in](../concepts/agent-runtime.md#the-thread-a-turn-was-woken-in)).

**A channel two units both declare is refused on Chatlet.** A unit's room belongs to the one unit that names it — its members are that unit's subtree, and its lead answers what nobody else did — so a second unit naming the same channel would be left out of a room its seats are told is theirs. `crewlet validate` reports it beside each of the units: give each its own channel, or leave a child unit's empty so it inherits its parent's room. It is an [admission rule](../concepts/configuration.md#what-a-stored-revision-is-held-to), so a stored revision that carries the shape still runs, with the refusal reported as a warning instead. Moving a channel from one unit to another, by contrast, is an ordinary edit: the room follows the channel at the next apply, with the new unit's people in it and what was said in it kept (see [unit rooms](../concepts/chatlet.md#what-a-company-gets)). On Mattermost or Slack the same shape is only a warning, because there it is one a real company writes on purpose: everything scoped to one unit lands in the other's room, and every seat in both is told that channel is its team's own.

**A Chatlet company where no unit declares a `channel` at all** is warned about at the first unit: a unit's room is the one kind whose members the org chart keeps, so without one no team has a room of its own until somebody opens one by hand and adds its seats. A child unit that names none and inherits its parent's is neither a warning nor a refusal — that is the field working as intended.

---

## MCP Servers

**All** MCP tool servers are declared here — including the Jira/Confluence (`atlassian`), Slack, and GitHub servers. A `shared: true` server runs once for everyone; a `shared: false` server is a per-role template, and each agent supplies its own credentials via `role.mcp_env[name]` (env vars for `stdio`, HTTP headers for `http`).

```yaml
mcp_servers:
  # shared stdio server (one instance for all agents)
  - name: tavily
    command: npm
    args: ["exec", "--yes", "--", "tavily-mcp@0.2.22"]
    env:
      TAVILY_API_KEY: "${TAVILY_API_KEY}"
    tool_annotations:                   # it advertises none; every tool only reads the web
      tavily_search:   { read_only: true, open_world: true }
      tavily_extract:  { read_only: true, open_world: true }
      tavily_crawl:    { read_only: true, open_world: true }
      tavily_map:      { read_only: true, open_world: true }
      tavily_research: { read_only: true, open_world: true }

  # per-role stdio server — Jira + Confluence share one mcp-atlassian
  - name: atlassian
    shared: false                       # per-role: token from role.mcp_env.atlassian
    command: uvx
    args: ["mcp-atlassian==0.23.1"]
    env:                                # each product the company runs; every ${VAR} must resolve
      JIRA_URL: "${JIRA_URL}"
      CONFLUENCE_URL: "${CONFLUENCE_URL}"

  # per-role http server — remote GitHub MCP
  - name: github
    transport: http                     # stdio | http (default: stdio)
    shared: false                       # per-role: each seat's own app token, bound by the engine
    url: "https://api.githubcopilot.com/mcp/"
    tool_prefix: ""                     # optional — prefix tool names
    tool_annotations: {}                # optional — behavioural-hint overrides (see Tool Capabilities)
    startup_timeout_seconds: 120        # optional — connect + handshake + discovery
    request_timeout_seconds: 300        # optional — one tool call
```

Full field reference: `name` (required; `crewlet` is reserved — it is the name every coding CLI sees the seat's own tools under — over its run's session on this host, and over the [tool bridge](../concepts/llm-backends.md#the-tool-bridge) in an executor's box — and a server called the same would be replaced by it there), `transport` (`stdio`/`http`), `shared` (default `true`), `command`/`args`/`env` (stdio), `url`/`headers` (http), `optional_keys` (the keys of `env`, `headers` or a seat's `mcp_env` block for the server whose value may resolve to nothing: such a key is left out rather than holding the server back, and every other `${VAR}` must resolve before the server starts — see [Tools & MCP](../guides/tools-and-mcp.md#a-key-that-may-go-without-its-value)), `tool_prefix`, `tool_annotations`, `startup_timeout_seconds`, `request_timeout_seconds`, and `seat_credentials` (which agent credential a per-seat server reads, under which variable or header: `{kind, field, key, as, prefix}`; a server the engine recognises needs none, see [Tools & MCP](../guides/tools-and-mcp.md)).

### Timeouts

An MCP server is another program, and the failure that matters is not an
error but a *silence* — a server that launches and never completes the
handshake, or answers discovery and then never returns from a tool call,
raises nothing at all. The engine starts MCP servers on the
seat-acquisition path, so a silent one does not merely lose its own
tools: it holds up every seat behind it, for the life of the process.
Both deadlines therefore always apply.

- **`startup_timeout_seconds`** (default `120`) bounds launching the
  process (or opening the HTTP session) and the protocol handshake, and
  separately the first `tools/list`, so a start can take up to twice it
  before the server is given up on. The default suits a `uvx` / `npx`
  server whose package is not yet in the local cache — the slow case for a
  *healthy* server. Lower it for a server you launch from a local checkout.
- **`request_timeout_seconds`** (default `300`) bounds one tool call. It is
  one ceiling whatever the transport, so a tool behaves the same over stdio
  and over HTTP. Raise it for a server whose tools genuinely run long (a
  large code search, a slow report); lower it for one that should always
  answer quickly, so a wedged call reaches the agent as a failed tool result
  it can react to instead of a turn that never ends.

A server that exceeds either deadline is logged and skipped; the rest of
the company still starts.

### MCP registry

`mcp_registry` names the community registry Settings › MCP & tools searches
beside the [curated catalogue](../guides/tools-and-mcp.md#the-curated-catalogue-and-a-community-registry)
when somebody adds a server. Absent, there is no community search.

```yaml
mcp_registry:
  url: https://registry.modelcontextprotocol.io   # /v0.1/servers is appended
```

| Field | Type | Required | Description |
|---|---|---|---|
| `url` | string | no | The address of a registry that speaks the MCP Registry v0.1 API; the API's `/v0.1/servers` path is appended to it. It must be an `https` address with no credentials, query or fragment, a private host included, because what it answers is drafted into launch lines. A result is only ever a draft for the add flow, marked as not reviewed, and the engine dials nothing a listing names |

---

## Environment Variable References

String values support `${ENV_VAR}` syntax, keeping secrets out of config files. An unanswered reference resolves to the empty string. The two tiers resolve references differently, and it decides where one works:

- **Tier B (the company)** keeps every reference verbatim — in the store, in a backup, on `GET /config` — and resolves it where the provider, transport or integration that uses it is constructed: from the [secret store](../concepts/secret-store.md) first (an encrypted table the provisioning CLIs can write into directly; inert until you store something), then the environment. It validates what is **written**, not what a reference resolves to, so a field with a closed set of values or a pattern — an enum, a seat handle, a unit id — takes a reference only where that field's own entry says so. Three fields are resolved only when the **whole** value is one reference — a seat's Slack `bot_token`, its Mattermost `bot_token` and its Mattermost `username` — because their transports take anything else as the literal it looks like: write one `${VAR}`, or the value itself, and never a reference inside other text (`bot-${SUFFIX}`), which validation refuses rather than letting it reach the server braces and all. A Mattermost `username` written as a whole reference is the one patterned field that takes one.
- **Tier A (`crewlet.yaml`)** resolves from the **environment only** — it holds the keys to the secret store, so it can never read a value out of it — and does so at startup, **before** the file is decoded. What a reference becomes depends on the field it lands in. In a **text** field it is substituted whole or embedded (`edge-${ZONE}`) and the result is that text, character for character — so it works in any text field, one with a pattern or a closed set included (`node.id: "${HOSTNAME}"`, `logging.level: "${LOG_LEVEL}"`), which is then judged on the value it resolved to. One nothing answers leaves the text empty, which a text field reads exactly as unset — its default where it has one (`stream.type` and `coordination.type` fall back to `embedded` and `local`), and a refusal where a value is required — with one exception: `logging.file.path`, where empty is the setting "write no file", so a path whose variable nothing answered stops the boot rather than silently running with no durable log. In a **number** or **boolean** field — a port, a replica count, anything `*_seconds`, `*_bytes` or `*_hours`, a count, a switch — a reference must be the **whole** value, and what it resolves to is read exactly as the same characters written there would be, trimmed of the space around it as every Tier A string is: `api.port: "${API_PORT}"` with `API_PORT=8080` is `api.port: 8080`, with `API_PORT` read from a file ending in a newline too, and `stream.debug: "${DEBUG}"` with `DEBUG=true` is `stream.debug: true` (a switch also takes `yes`, `no`, `on` and `off`, as it does written out). Two shapes are refused there by name rather than guessed at: a reference with other text around it (`"80${N}"`), and one that resolves to nothing, since a number that quietly fell back to its default because a variable was missing would run with a value nobody chose. `crewlet schema bootstrap` encodes exactly that.

**A number is the number it spells, in both tiers, however it arrives.** A fraction in a field that counts whole units — a port, a replica count, a round cap — is refused naming the field rather than truncated (`api.port: 8080.9` used to run as 8080), and so is a number that is not finite — YAML's `.nan` and `.inf`, written out or what a Tier A reference resolves to — which no setting has a meaning for and which used to pass every bound a value is checked against.

Only the braced identifier form is substituted — `${NAME}` where `NAME` matches `[A-Za-z_][A-Za-z0-9_]*`. Bare `$NAME` and shell parameter expansions (`${1:-x}`, `${line#host=}`) pass through untouched. A sandbox setup step's commands and file bodies are not substituted at all: the engine resolves only a file whose whole body is one `${VAR}`, and the step's `env`, and hands everything else to the box as written, where its own shell expands what it expands from the run environment ([setup steps](../concepts/code-sandbox.md#setup-steps--provisioning-the-box)).

```yaml
providers:
  llm:
    default:
      agent: opencode
      model: "${LLM_MODEL}"                          # resolved where the entry is built
      env:
        OPENROUTER_API_KEY: "${OPENROUTER_API_KEY}"  # the secret store first, then the environment
```

See [Environment Variables](../reference/environment-variables.md) for a full list.

---

## Full Example

See the [quickstart](quickstart.md) for a complete working config.
