
# Configuration Reference

Crewlet companies are defined across **two tiers** (design: [Configuration concept page](../concepts/configuration.md)).

- **Tier A** (`crewlet.yaml`, restart-only): this node's identity and roles, the store's files, the stream and coordination slots, API host/port/auth, the secret keyring, logging (level, shape, optional rotating log file), and on a node that serves git its repository cache and git processes.
- **Tier B** (`company.yaml` imported into the store, live-editable): everything else: identity, providers, integrations, MCP servers, roles, units, turn engine, learning, budgets, scheduling.

This page documents **Tier B**. For Tier A see [Configuration concept page §"Tier A example"](../concepts/configuration.md#tier-a-example-crewletyaml).

> **Machine-readable version.** `crewlet schema company` emits the JSON
> Schema for everything on this page, generated from the models: the same
> files the repository commits under `schema/`, which `make schema`
> regenerates on every model change, so page and schema cannot drift. Use it
> for editor autocomplete and typo squiggles, or give it to an AI assistant
> ([Authoring with an AI assistant](ai-authoring.md)). Check a file with
> `crewlet validate <file>` (`-json` gives located, classified problems and
> warnings, see [the validation loop](ai-authoring.md#the-validation-loop));
> it reads no environment, so it works before any secret is exported.
>
> **Credential positions are marked.** Every field, list member or map
> value holding a credential carries `"x-crewlet-secret": true` in the
> schema, telling an editor or tool where a `${VAR}` reference belongs, not a value. See
> [Credential positions](../reference/cli.md#credential-positions-x-crewlet-secret).
>
> **Unknown keys are rejected** at every level, roles and units included: a mistyped
> `backstroy:` fails validation naming its exact path.

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
diagnostics:                            # optional: what this engine may say about itself outside
  issue_reports:                        #   the company. See Engine Issue Reports.
    enabled: false                      # off by default: an administrator's, and anonymous, typed
                                        #   reports of the engine's own defects, nothing of the company's.
notification_rate_limit: 10             # optional — inbound notifications one seat may be woken by
                                        #   per second. 0 (the DEFAULT) is unlimited, so the valve is
                                        #   off unless you set it. Drop-based safety valve against
                                        #   webhook storms and notification loops — burst handling is
                                        #   inbox coalescing below, which batches instead of dropping.
                                        #   Fails OPEN: a valve that cannot reach its counter passes
                                        #   the notification rather than swallowing it. A replay's
                                        #   backlog is paced, never dropped, in a count of its own.
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

code: {...}                             # optional — Gitlet, the engine's own git host (see "Code" below)

inbox:                                  # optional — a person's inbox, every native domain's notices
  retention_days: 365                   #   how long it keeps one (default 365, 30..3650). See
                                        #   "A person's inbox" under "Tracker" below.
access_history:                         # optional: what each agent was told about its access,
  retention_days: 400                   #   every SSH certificate and command, delegated verdict
                                        #   and growth step, kept this long
                                        #   (default 400, 90..3650); each agent's newest told
                                        #   record stays while the agent exists.
atlas:                                  # optional: the atlas, the map of what the company runs on
  retention_days: 400                   #   how long its history is kept: a value somebody replaced,
                                        #   and an entry a person retired that nothing has reported
                                        #   since (default 400, 90..3650). What the atlas holds now
                                        #   never expires.
  refresh_interval_seconds: 3600        #   how often each connected integration is read whole for
                                        #   the atlas (default 3600; the floor is 300). An unchanged
                                        #   read writes nothing; see concepts/atlas.md.
  agent_records: true                   #   whether agents record in the atlas what a tool call of
                                        #   their own turn showed them, and dispute what it holds
                                        #   (default true). false stops both; see "What agents
                                        #   record" in concepts/atlas.md.
  owner_wakes: true                     #   whether the agent seat that owns an entry is woken when
                                        #   the entry drifts: its health turns to alerting, or its
                                        #   sources come to disagree (default true). At most one
                                        #   wake an entry a day; false wakes nobody.
assistant: {...}                        # optional, Ask Crewlet; off until a model is assigned
                                        #   (see "Ask Crewlet" below)
advisor:                                # optional, the Advisor's model; its rules run either way
  model: assistant                      #   a providers.llm key on a harness a helper can run
                                        #   (see guides/advisor.md, "Found by the Advisor's model")

turn_engine:                            # optional — executor/reviewer turn config
  max_iterations: 3                     # hard cap on self_iterate loops per turn
                                        #   (an executor run's own rounds are capped only
                                        #   where role.sandbox.max_turns / providers.sandbox.
                                        #   default_max_turns sets a cap: the run is its
                                        #   harness's loop)
  onboarding_max_tool_rounds: 10        # dedicated first-turn onboarding pass (0 = disabled)
  delegation:                           # bounds on every `delegate` call — see Turn Engine § Workers
    max_parallel: 3                     # workers running at once within one call
    max_tasks_per_call: 8               # tasks one call may contain; over it the call is REFUSED
    max_turns: 20                       # tool rounds one worker may run (a request above it is clamped)
    max_turns_ceiling: 40               # highest max_turns a worker template may declare
    budget_fraction: 0.2                # ONE slice for the whole call, shared by every task in it
    min_tokens_per_task: 500            # call refused up front if the per-task share falls below this; 0 = no floor
    task_timeout_seconds: 300           # wall-clock cap on one worker
    call_timeout_seconds: 900           # wall-clock cap on the whole call, dependency waves included;
                                        #   also counts toward how long an executor's box waits
                                        #   for a call (Code Sandbox § The tool bridge)
  executor_min_budget_tokens: 65000     # refuse an executor launch, and park the seat, below
                                        #   this remaining budget with the launch's brief
                                        #   beside it (0 = no floor for a launch whose
                                        #   session's context is unknown). A seat's or the
                                        #   company's token_budget ceiling below it is
                                        #   refused: no launch could ever fit. A relaunch that
                                        #   continues its turn's session needs the larger of
                                        #   that and the session's context with its brief
  delegation_depth_limit: 3             # max colleague-handoff chain depth before depth_cap guard breach
  extension_enabled: true               # round-cap extension judge for the onboarding pass
                                        #   (an executor run is its harness's loop, which no
                                        #   judge extends)
  onboarding_max_tool_rounds_ceiling: 20  # hard ceiling for onboarding rounds across extensions (2x base 10)
  extension_round_step: 8               # max rounds the judge may grant per extension call
  conversation_session:                 # each executor launch's session, kept and continued
                                        #   by the turn's relaunches and by its conversation's
                                        #   next turn — ONE thread / issue / PR, or one
                                        #   schedule on one seat, its next fire — which is
                                        #   also told how the conversation's earlier turns
                                        #   ended (see concepts/conversation-sessions.md)
    enabled: true                       # a live kill switch; off keeps and continues no
                                        #   executor session, reads no conversation's head,
                                        #   tells no turn how its earlier turns ended and
                                        #   lets no model read a seat's sessions;
                                        #   how each turn ended is still recorded on the
                                        #   executor log, which is the turn's account, not
                                        #   its session (see concepts/code-sandbox.md)
    retention_days: 30                  # days a seat's sessions are kept past their turn's
                                        #   last record — the event store's horizon; read
                                        #   off the fleet's revision at every reaper pass,
                                        #   so a change is honoured at the next one. A
                                        #   schedule whose fires are further apart never
                                        #   continues: every fire starts fresh
    max_bytes_per_seat: 67108864        # object-store bytes one seat's captured executor
                                        #   sessions may hold (49 MiB..64 GiB); past it a
                                        #   capture releases the seat's oldest ENDED
                                        #   turns' sessions, keeping how each ended, never
                                        #   one a live turn was handed to continue; also
                                        #   the allowance of the seat's saves of running
                                        #   sessions, which the store keeps a day once
                                        #   replaced, so a seat holds up to twice it:
                                        #   twenty seats or more belong on S3
    save_interval_seconds: 600          # shortest time between saves of a running
                                        #   executor job's session (300..3600; 0 takes
                                        #   600, anything else refused): a run lost with
                                        #   its box redoes about this plus one save's
                                        #   take, longer where a seat's saves are paced
                                        #   by size, about 27 min per sealed MiB past
                                        #   0.4 MiB (see concepts/code-sandbox.md)

learning:                               # optional — agent-learning subsystem
                                        #   Every pass here calls the seat's auxiliary
                                        #   model (`llm_auxiliary`), a harness like
                                        #   every model, under a FIXED deadline — 60 s
                                        #   after a turn, 30 s for a turn-start
                                        #   briefing — so point that key at a fast,
                                        #   cheap model (see concepts/llm-backends.md
                                        #   § Auxiliary work and latency)
  enabled: true                         # master switch (auto-disables without DB + embeddings)
  max_share: 0.15                       # the most of an agent's day of spend its learning
                                        #   may take (0 < x <= 1; 1 leaves it uncapped)
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
    llm_triage: haiku                   # cheap/fast model for a busy seat's wake triage
```

Each names a `providers.llm` key (see [Providers](#providers)). The satellite
calls run under a fixed deadline and pay a harness launch each, which is why they
want a fast model rather than the seat's own — see
[Auxiliary work and latency](../concepts/llm-backends.md#auxiliary-work-and-latency).

`llm_judge` is invoked when the first-turn onboarding pass exhausts its tool-round cap;
it decides whether the pass is making progress (extend) or thrashing
(fall through to rescue).  Falls back to `llm` -> `"default"` if unset. An
executor run is its harness's own loop, uncapped unless `role.sandbox.max_turns`
(else `providers.sandbox.default_max_turns`) sets a cap, which no judge extends.
See the [Turn Engine extension judge](../concepts/turn-engine.md#round-cap-extension-judge)
section for details.

See the [Turn Engine](../concepts/turn-engine.md) and [Agent Learning](../concepts/agent-learning.md) docs for what each field controls.

### The company's clock

```yaml
timezone: America/Los_Angeles            # an IANA zone name; absent is UTC
```

`timezone` is the company's **one clock**; every calendar edge is cut on it:

- where **today**, **this week** and **this month** begin: `due=` filters, board due bands, the overdue mark on every row, a person's day (`my_work`), the workload's overdue counts;
- what a relative date (`tomorrow`, `eow`, `+7d`) or all-day due date resolves to, whoever wrote it (seat, assistant, dashboard);
- the wall clock a [schedule](#schedules) with no `timezone` of its own fires on: `cron: "0 9 * * 1-5"` is 09:00 in this zone.

It serves **authored instants and calendar boundaries only**. No duration (lease, timeout, retention horizon) is measured on it, since a wall-clock duration changes length twice a year. There is only one: the tracker and scheduler take no zone of their own, or each subsystem would have its own "today". A schedule's own `timezone` is that one piece of work's wall clock (the Tokyo team's 09:30 standup) and nothing else is cut on it.

The value must be a real IANA name (`Europe/Berlin`, `America/New_York`, `UTC`). `Local` and `localtime` are refused: they are whatever zone the reading host has, so two nodes would cut different days. The [`org` projection](../reference/api-endpoints.md) carries the resolved clock to every reader (`UTC` where unset), so the dashboard cuts days where the engine does. An apply that changes it moves the next answer, scheduler tick and tool call; a running turn keeps the clock of the epoch it started on. A write that moves it is answered with a warning naming both zones (the dry run's too), since every budget window and zoneless schedule moves with it; the dashboard changes it on Settings › General, under **Company clock**, and asks for **Save anyway** on that warning.

### Token budgets

```yaml
token_budget:                            # on the company, and on any agent seat
  day: 3000000                           # one local day, midnight to midnight
  week: 10000000                         # one ISO week, from Monday
  month: 40000000                        # one calendar month
```

A token budget is a **ceiling per calendar window**, each cut on the
[company's clock](#the-companys-clock): a day is local midnight to midnight, a week the ISO week from Monday, a month the calendar month. Each key is optional and windows are independent: a model round is admitted only while **every** capped window has room, and each window reopens when it turns over. A single number is refused (it was a lifetime ceiling only an operator could reset): `token_budget: 10000000` is refused with the mapping to write instead.

The company's `token_budget` caps every seat's spend together. A seat's own caps that seat
alone, **on top of** the company's: every round is charged to both, and the first ceiling reached stops the seat. A human seat spends nothing, so a `token_budget` on one is refused.

Both are part of the company document and change through `/config` (a seat's alone with `PUT /config/roles/{handle}`); each node enforces them once it has applied that revision. See
[Budgets and spend](../guides/budgets-and-spend.md#budget-windows).

An **absent key is the only way to leave a window uncapped.** A ceiling of `0` or less is
refused (`must be at least 1 token … remove token_budget.day for no daily ceiling`): `0` is ambiguous between unlimited and nothing, so it means neither. Stopping a seat on purpose is not a budget's job. A key that is not a window (`year`) is refused too, naming the key to remove. A company file and a `/config` write each refuse all of these, a bare number over JSON as well as YAML. A ceiling below `turn_engine.executor_min_budget_tokens` (65000 by
default) is refused too, on a seat that runs an executor and on the company, since no launch could fit under it. A seat whose window leaves less than that floor, with its launch's brief beside it, waits
[parked](../concepts/agent-runtime.md#the-budget-park) until the window turns over. A
launch whose brief takes its floor past the most room a window can leave it (its ceiling less what the launch's own turn already spent there) is held at the floor alone, or at that room where the turn's own spend left less than the floor, and its first request may take the window past its ceiling. A relaunch that continues its turn's session needs [that
session's own floor](../concepts/code-sandbox.md#what-a-continued-launch-costs);
below it the resumed segment is held on the window, never parked.

[`crewlet validate`](../reference/cli.md) **warns**, never refuses, about a ceiling that
can never refuse a turn because another is always reached first:

- a day ceiling at or above the week's or month's;
- a week ceiling that seven days at the daily ceiling already reach, or a month ceiling that 31 days at the daily one, or six weeks at the weekly one, already reach;
- a week ceiling at or above the month's, which can then bind only in a week straddling two months;
- a seat's ceiling at or above the company's for the same window, or for a longer window
  that holds it (a seat's day against the company's month), since a seat's spend is the company's too. Every `/config` write's answer carries the same warning.

Spend is counted per window in the fleet's [coordination store](../concepts/coordination.md#token-budgets-are-windows), so it survives restarts and is one number across all nodes. A window nothing caps is counted too, so a ceiling added mid-window judges the spend already in it. A seat whose capped window is refusing is not handed work: its
mail waits on its inbox until the window turns over or the ceiling is raised (see
[the budget park](../concepts/agent-runtime.md#the-budget-park)). There is **no reset**: allowance returns when the window turns over, and room before then comes from raising the ceiling. See [Deployment § Token Budgets](../guides/deployment.md#token-budgets)
for how a refusal is recorded and reported.

### Scheduling

System-level knobs for the [Scheduler](../concepts/scheduling.md), which fires role/unit `schedules:`. It
auto-enables when `enabled` is true, a database is configured, and the
org declares at least one schedule.

```yaml
scheduling:                              # optional — role/unit scheduled work
  enabled: true                          # master switch
  tick_seconds: 10                       # scheduler poll interval
  jitter_seconds: 0                      # max per-schedule spread to smooth a shared cron minute
  catchup_min_seconds: 120               # lower clamp on the missed-tick catchup window (at least 1)
  catchup_max_seconds: 7200              # upper clamp on the missed-tick catchup window
  work_pickup:                           # hand an idle agent seat its next ready work item
    enabled: true                        # default on, whether or not any schedule exists
    cooldown_seconds: 600                # least time between two pickups of one seat, 60..1800
```

`work_pickup` is not a schedule: it hands an agent seat that is idle while holding ready work on Worklet its next item, once per item version
([The work pickup](../concepts/scheduling.md#the-work-pickup)), and runs
whatever `enabled` says.

A schedule with no `timezone` of its own fires on the company's
[clock](#the-companys-clock); there is no scheduler-wide default zone.
See the [Scheduling](../concepts/scheduling.md) concept doc for delivery
modes (`each` / `lead`), at-most-once semantics, catchup, and the
per-task wall-clock timeout.

### Ask Crewlet

Ask Crewlet is a dashboard chat that answers questions about the company and prepares changes for approval. It has no
identity of its own: every read is a request made as the signed-in person, and nothing is written until that person approves the previewed change. It
is **off** until the company assigns it a model.

```yaml
providers:
  llm:
    sonnet:                             # any entry the company already has
      harness: claude-code
      model: sonnet

assistant:
  model: sonnet                         # a providers.llm key; absent: off
  token_budget: { day: 200000 }         # per person, optional
  instructions: { length: brief }       # how it writes for everybody, optional
```

| Field | Default | Meaning |
|---|---|---|
| `model` | none | The `providers.llm` entry Ask Crewlet runs on. Absent, it is off. |
| `token_budget` | none | Each person's ceiling per calendar window, the shape of [a token budget](#token-budgets), charged on top of the company's. |
| `instructions` | none | How Ask Crewlet writes for everybody: `length` (`brief`, `balanced`, `detailed`), `format` (`lead`, `lists`, `tldr`) and `text`, at most 1000 characters and never a credential. Each person's own win over them. They shape tone, length and layout only ([Instructions](../guides/assistant.md#instructions)). |

Nothing else about a turn is a setting. A turn takes at most 40 model rounds
and 20 minutes, a proposed change waits 15 minutes for its owner's approval,
and a conversation is kept 30 days after last use
([Running Ask Crewlet](../guides/assistant.md#budgets-and-limits)).

**What one person may spend.** Every round of a person's turn is charged to the
company's budget and, where `token_budget` is set, to that person's own, across
the fleet. A person whose own window is spent, or the company's, is refused before the next model round with `budget_exhausted` naming which
(`person` or `org`), and resumes when the window turns over or the ceiling
is raised (Company › Budgets, under **Ask Crewlet, per person**). A per-person
ceiling that can never refuse a question is a warning, as a seat's is: one at or
above another of its own windows that holds it, or at or above the company's on
a window that holds it. A person may have two turns running on a node and start twenty a minute
across the fleet.

**The model reaches nothing the person's requests did not return.** Ask Crewlet
runs the entry as a helper's run, not as a seat does: the engine turns the
harness's own web tools off whatever the entry says, and the argv leaves the harness no tool of its own, so its one tool source is the
engine's `crewlet` server serving the person's own requests. A harness shell or file reader on the engine host could read whatever the engine user
can (the engine's data and keys), and a page the model fetched itself
is text no authority decision saw and a way out for the person's data. A seat on the same entry keeps its web tools.

So the model must be an entry whose **harness** can run that way, judged on
the argv, not the profile's claim: its profile declares
`local_tools: denied`, a measured web denial and a measured `tools_off`, with
an `mcp` allow list naming the engine's tools alone. Today that is
`claude-code` and `pi`; `codex` keeps `apply_patch`, which has no switch,
`opencode`'s policy is not measured to remove its tools, and a harness
with no measured web denial cannot run without its web tools
([Tool calls on this host](../concepts/llm-backends.md#tool-calls-on-this-host)).
An entry whose own `overrides` bring a tool back on that argv is refused too.

A document whose model breaks this is refused by `crewlet validate` and by
every `/config` write, naming `assistant.model` and why. A stored revision
that carries one still applies, so every seat keeps running, and
Ask Crewlet stays unavailable with that reason until the model is changed.
`crewlet llm doctor <key>` probes the helper's run of an assigned entry beside
the entry's own: it asks the harness to run a shell command, read a file
the doctor just wrote and fetch a page, and reports any that
succeeded as a problem.

---
## Providers

Every model a seat runs on is reached through a **harness** the engine drives as a process (Claude Code for Anthropic's models, Codex for OpenAI's, OpenCode for open models on any provider it speaks to). A `providers.llm` entry is that harness's settings: which harness, which model in its grammar, how it signs in. [LLM Backends](../concepts/llm-backends.md) has the rest: why there is no other kind of entry, picking a harness per vendor, isolation, the tool channel and every sign-in route.

```yaml
providers:
  llm:
    default:                            # named provider (referenced by roles via `llm: default`;
                                        #   `default` is also the fallback for a seat naming none)
      harness: claude-code                # optional — which harness to drive (default claude-code):
                                        #   claude-code | codex | opencode | gemini-cli | qwen-code
                                        #   | cursor-agent | copilot | grok | muse-code | kimi-code
                                        #   | hermes | pi | custom
      model: sonnet                     # REQUIRED — in the harness's own grammar: an alias or id for
                                        #   claude-code (sonnet, opus, claude-opus-5-5), an OpenAI
                                        #   id for codex, <provider>/<model> for opencode. Supports
                                        #   ${ENV_VAR}; a reference that resolves to nothing is
                                        #   REFUSED naming the variable — unlike a missing
                                        #   credential, which still builds: every call then fails
                                        #   with the harness's own sign-in error, which names it, where
                                        #   a missing model has no such tell
      run_in: ""                        # optional — where a seat's executor runs when it launches
                                        #   on this entry: direct | container; empty takes
                                        #   providers.sandbox.default_run_in. Only the ten harnesses
                                        #   with an in-box runner (claude-code, codex, gemini-cli,
                                        #   qwen-code, opencode, copilot, grok, muse-code, kimi-code,
                                        #   pi) run an executor, so the field is REFUSED on any other
                                        #   harness's entry, where nothing would read it; any
                                        #   harness serves the reviewer, workers and side passes
      state_dir: ""                     # optional — the login + per-seat harness homes, each harness
                                        #   under <state_dir>/<harness>. Empty:
                                        #   $CREWLET_LLM_HARNESS_HOME/<key>, else ~/.crewlet/llm-harness/<key>.
                                        #   Use a persistent volume in an ephemeral container.
                                        #   Entries naming the SAME dir and driving the same harness
                                        #   share one login (how per-phase models run off one
                                        #   `crewlet llm login`); on different harnesses they share
                                        #   nothing, so moving an entry to another harness applies
                                        #   live and starts that harness signed out. Its bundle
                                        #   (auth.credential_bundle) stays the old harness's and is
                                        #   refused until re-exported from the new one
      timeout_seconds: 600              # optional — one stretch of a phase's harness work on this
                                        #   host, wall clock (default 600): from its launch, or the
                                        #   end of the tool call it last waited on, to its next
                                        #   call or its exit — the seat's tools are never on it. A
                                        #   breach is a retryable `timeout`. An executor run is
                                        #   not on this clock: its rounds are capped only where
                                        #   providers.sandbox.default_max_turns sets a cap
      effort: high                      # optional: reasoning effort, in the harness's own levels
                                        #   (claude-code: low | medium | high | xhigh | max). Absent
                                        #   passes none, so the harness runs at its default. Refused
                                        #   on a harness with no such setting or a level it does not
                                        #   take. See LLM Backends, "Reasoning effort"
      max_concurrent: 4                 # optional — live harness processes on this host at once
                                        #   (default 4). Each is a 200-400 MB runtime that holds
                                        #   its slot for its whole run, and subscription plans
                                        #   throttle concurrency hard; a model call made inside one
                                        #   of a run's tool calls on the same entry borrows the
                                        #   run's slot. Executor runs are not counted: each seat
                                        #   runs one at a time
      env: {}                           # optional — extra child env, ${ENV_VAR}-resolved. The
                                        #   child gets an ALLOWLISTED environment, never the
                                        #   engine's, so declare anything else the harness needs here —
                                        #   for opencode, the key of the provider its model names.
                                        #   An executor run's box gets it too, beneath the seat's
                                        #   role.sandbox.env
      auth:
        mode: subscription              # subscription (default) | api-key | inherit-env | env.
                                        #   subscription: the harness's own login, or a headless token.
                                        #   api-key: ONE key in the harness's key variable
                                        #   (ANTHROPIC_API_KEY for claude-code, CODEX_API_KEY for
                                        #   codex); refused for a harness with none (hermes, pi,
                                        #   opencode, kimi-code). qwen-code's api-key names its
                                        #   api_key (its OPENAI_API_KEY is shared with other
                                        #   vendors); its subscription is a Qwen login, not
                                        #   measured against Qwen's service. inherit-env: forward
                                        #   the engine's own value of either variable. env: the
                                        #   entry's env alone signs the harness in (a cloud provider's
                                        #   switch, or a gateway's ANTHROPIC_AUTH_TOKEN); neither
                                        #   variable reaches the harness and no login is seeded
        token: ""                       # optional — ${VAR} holding a headless subscription token;
                                        #   empty falls back to the profile's own variable
                                        #   (CLAUDE_CODE_OAUTH_TOKEN). subscription mode only, and
                                        #   only on a harness that mints one (claude-code)
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

    gpt:                                # OpenAI's models through Codex
      harness: codex
      model: gpt-5
      auth:
        mode: api-key                   # reads CODEX_API_KEY; leave it out to run on a
                                        #   ChatGPT login (`crewlet llm login gpt`)

    open:                               # open models through OpenCode
      harness: opencode
      model: openrouter/qwen/qwen3-coder  # <provider>/<model>, split at the first slash;
                                        #   a bare id is refused on a write
      env:
        OPENROUTER_API_KEY: "${OPENROUTER_API_KEY}"  # the key of the provider the model names

  sandbox:                              # REQUIRED once the company has an agent seat and an entry
                                        #   above: every seat's executor runs as its harness's own run,
                                        #   in a box from this catalogue (code-sandbox.md)
    local:
      state_dir: "./acme-data/sandboxes"  # the box root: every box this host makes for a
                                        #   seat, each removed at teardown
      # image: ""                       # required where a run is placed in a container; a
                                        #   company hosting code runs its checks in it
      # runtime: auto                   # auto | docker | podman
      # network: ""                     # the container network; "none" cuts the box off
      # run_args: []                    # how a container box is sized
      # runtime_env: {}                 # environment for the docker/podman CLI, never the box
      # chromium_sandbox: required      # or waived: Chromium's own sandbox in a browser seat's
                                        #   container box (code-sandbox.md)
    # fake: true                        # the in-process double, for demonstrations;
                                        #   never beside local:
    default_run_in: direct              # where a run on an entry with no run_in happens. direct
                                        #   runs the harness on this host as the engine's user.
                                        #   Required unless fake: true, or every entry an
                                        #   executor launches on names its own run_in
    # default_max_turns: 0              # agentic-round cap for one executor run on seats
                                        #   naming no role.sandbox.max_turns; unset or 0 =
                                        #   uncapped; at most 1000
    # host_grace_seconds: 900           # how long a launching node may be away or silent
                                        #   before its runs are recorded lost: not a run cap
    # default_pause_ttl_seconds: 1800   # how long a box blocked on a person stays paused;
                                        #   0 = never pause
    # commit_attribution: true          # credit Crewlet as co-author on the commits a run
                                        #   makes as itself; false = credit nothing
    # setup: []                         # provisioning steps applied to every box
```

**One entry is one harness on one model, signed in one way.** Per-phase models are one entry per model; entries that share a `state_dir` and drive one harness share one login. A seat's `llm` takes a list, which composes a plan and a key: `llm: [default, claude-key]` runs on the plan and falls through to the key while the plan is spent ([Falling back between
entries](../concepts/llm-backends.md#falling-back-between-entries)).

**What an entry does not set.** No temperature, effort, thinking switch, output cap, endpoint or key list, on an entry or a call: a harness driven headlessly reads its model's behaviour from its own configuration, and an entry holds one sign-in. See [What an entry does not
set](../concepts/llm-backends.md#what-an-entry-does-not-set).

Sign an entry in and check it end to end before the first turn:

```bash
crewlet llm login default -from-host        # adopt the login this machine already has
crewlet llm doctor                          # every entry: binary, version, sign-in, a real tool call
```

The harness itself must be installed on the engine host: see
[Installation](installation.md#a-harness).

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
                                        #   a harness computes no vectors
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

**The width belongs to the model.** `text-embedding-3-large` emits 3072, `text-embedding-3-small` 1536, `gemini-embedding-001` 3072, `embed-v4.0` 1536; leaving `dimensions` unset takes the named model's. There is no global default (a number right for one model is silently wrong for the next), and a model this build does not know is refused, naming both fixes.

Every request asks for the width in the OpenAI `dimensions` parameter where the endpoint takes it, and every answer is checked against it either way:

| Model | `dimensions` at its endpoint | What the engine does |
|---|---|---|
| `text-embedding-3-large`, `text-embedding-3-small` | Supported: the vector is shortened ([OpenAI's reference](https://developers.openai.com/api/reference/resources/embeddings/methods/create)) | Sends it; a width below the model's own is stored |
| `gemini-embedding-001` | Not mentioned by [Google's compatibility page](https://ai.google.dev/gemini-api/docs/openai) | Sends it; an endpoint that ignored it answers 3072, which the width check refuses at any other width |
| `embed-v4.0` | Unsupported per [Cohere's Compatibility API](https://docs.cohere.com/docs/compatibility-api), so it answers at the default, 1536 ([the model page](https://docs.cohere.com/docs/cohere-embed)) | Never sends it; refuses any other `dimensions`, naming the field |
| any other model | Unknown | Sends it |

**Limits belong to the model too.** The engine carries what each vendor documents for the endpoint it calls:

| Model | Tokens an input | Inputs a request | Tokens a request |
|---|---|---|---|
| `text-embedding-3-large`, `text-embedding-3-small` | 8 192 | 2 048 | 300 000 |
| `gemini-embedding-001` | 2 048 | *state it* | *state it* |
| `embed-v4.0` | 128 000 | *state it* | *state it* |

Google's and Cohere's OpenAI-compatible endpoints document no per-request limit (Cohere's native API takes 96 texts a call, a different endpoint), so those two are refused until `max_batch_inputs` and `max_batch_tokens` are stated; a model this build does not know states all three, as it states its width. A stated limit may only *lower* a documented one (a gateway or proxy that accepts less); raising one is refused, naming the field, because the provider would refuse what the engine sends.

**A company that needs a limit states it.** A stored company naming `gemini-embedding-001`, `embed-v4.0` or an unknown model without the limits it needs is refused at boot as well as at an apply, as is a model with no known width, and `embed-v4.0` at any `dimensions` but 1536. No node starts on that revision, so state the limits in the same edit that names the model.

The engine counts **bytes** against token limits instead of shipping a tokenizer per vendor: every tokenizer these models use emits at most one token per byte, plus the few special tokens a server wraps an input in (none for OpenAI; sixteen set aside for every other model). So 8 192 bytes is always inside OpenAI's 8 192-token window in any language, at the price that ordinary prose (three to four bytes a token) is held to about a quarter of the window. An input past the bound is refused *before* anything is sent; a batch is sent as many requests as the limits need. A text too long for one input is each caller's choice: the knowledge corpus embeds a document's opening; a turn's ask and an episode are split between words into pieces that each fit, embedded in one call and pooled into one vector, so a long ask is still searched by meaning.

**The width is a contract with the store, not the model.** Vectors of different widths cannot be compared, so a row written at the wrong one can be written and never read back, silently and permanently. Three checks, all refusing rather than adapting:

- **At the apply.** A revision whose `dimensions` differs from the width the store already holds is rejected, with an error naming both. Changing the width is a restart, for an operator who is watching: after it the knowledge corpus re-embeds itself and each seat's diary and episodes are re-filled by the node holding the seat.
- **Per model, at recall.** Two models of one width are two spaces (`text-embedding-3-small` and `embed-v4.0` both answer 1 536 floats), so every diary and episode vector is stored with its model and recall compares only rows of the query's model. Changing `model` at the same width is an ordinary apply: each seat's diary and episodes are re-filled in the new space by the node holding it (about a million bytes of text a minute across the company). Until a row is, it is reached by recency and by conversation rather than ranked, and `query_episodes` says how many of a seat's turns its search could not reach.
- **On every call.** A vector at the wrong width is refused rather than stored, on every call, because a gateway or aggregator can move models mid-deployment.

**Nothing in the provider retries, because a failure's cost depends on who asked.** For a turn starting (diary and episode recall, a search's query vector) a timed-out or refused call is *no vector*: personal memory keeps its recent half, `## Similar prior work` renders nothing, `query_episodes` says its search could not run, and a hybrid search serves its keyword half; a retry would be spent inside a prefetch somebody waits on. For the knowledge corpus a failure is a backlog, and the duty asks again on its next tick. Every caller gets *which* failure it was, in three classes:

- **refused**: HTTP 400, 413 or 422, or an input past the bound. Resent unchanged it is refused again, so a caller can set that input aside rather than resend its whole batch for ever.
- **transient**: 408, 409, 425, 429 and 5xx, a timeout or a network failure. Asking again later may succeed.
- **configuration**: 401, 402, 403, 404 and the remaining 4xx, or a vector of the wrong width. Nothing succeeds until `providers.embeddings` is fixed.

**Ceilings differ by caller.** A single embedding is held to 15 seconds where nothing tighter bounds it (an episode embedded after its turn). What a person or a turn waits on is held to two seconds: a search's query vector (a person's or a seat's `search_knowledge`); a turn's ask as the turn starts; the hint `query_episodes` or `refresh_memory` passes; and a note `reflect_and_persist` keeps, which the seat's model waits on mid-turn (a note the post-turn reflection keeps has the same two seconds). Past it each degrades rather than waits: a hybrid search serves its keyword half and a semantic one answers nothing, each saying why; personal memory is chosen from the recent half of its candidate pool alone; similar prior work renders nothing and `query_episodes` says its search could not run (a recent turn is not similar work, and the block would offer it as precedent); the note is kept without a vector until the node holding the seat fills it. So a slow embeddings server shows up as `embedding_failed` searches, personal memory from recent notes only and no similar prior work, not as slow searches, slow turn starts and stalled tool calls.

A batch request carries up to the model's request total (300 000 tokens on OpenAI) and is held to 60 seconds, a fifth of the five minutes a corpus tick may go without progress. Nothing has measured how long a server takes over a full request (OpenAI's, or a self-hosted one on CPU, the likeliest to need longer). If batches time out against a slow server, lower `max_batch_tokens`: a smaller request is a shorter one, and the ceiling a stuck call is held to does not move. Keep it at or above the model's per-input window (`max_input_tokens`, or the model's own): one input must fit one request, so a request total below the window is also the most one input may hold, and the knowledge corpus would embed a shorter opening of every long source, each under a new digest and so embedded again.

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
                                    #   none, and its items, pages and
                                    #   repositories once it has them — so
                                    #   `crewlet validate` and the
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
                                    #   node refuses to start naming it — and
                                    #   set no `jetstream { limits {
                                    #   duplicate_window } }` below 15 minutes,
                                    #   the dead letters' window, which fails
                                    #   the boot naming it too — see
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
                                    #   Unset binds EVERY interface. The port
                                    #   admits only a member presenting a key
                                    #   derived from `secrets.keys`, but that
                                    #   credential and every byte after it
                                    #   cross the network in clear — set it to
                                    #   the private address the peers reach.
                                    #   A peer URL carries no `user:pass@`: the
                                    #   engine presents the derived one itself
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
                                    #   node holds the whole estate as a member.
                                    #   The link presents a key derived from
                                    #   `secrets.keys`, so a URL carries no
                                    #   `user:pass@`
  #   port: 7422                    #   A MEMBER's listener, where leaves join.
                                    #   A member that opens one is in a fleet
                                    #   even with no peers, so it must persist
                                    #   (`store_dir`, as above: the leaves that
                                    #   join it keep nothing, so it keeps
                                    #   everything they do) and needs
                                    #   `coordination.type: embedded-kv`
  #   host: 10.0.0.11               #   the interface the listener binds. It
                                    #   admits only a leaf whose link presents
                                    #   a key derived from `secrets.keys` (no
                                    #   secret crosses the network), but what
                                    #   the link carries after crosses it in
                                    #   clear, as the route port's does: bind
                                    #   it to the private network the leaves
                                    #   are on
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
                                    #   every ceiling below it share five
                                    #   eighths of what the broker can grant
                                    #   them, less the ceiling any of them that
                                    #   already exists holds. The unset ones
                                    #   are scaled down together to fit, never
                                    #   below 1 GiB each but Ask Crewlet's,
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
                                    #   about 320 MB for three nodes of forty
                                    #   seats and twenty schedules, and about
                                    #   18 MB for one node of ten seats; set
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
                                    #   conversation is kept is fixed: thirty
                                    #   days after its last use
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
                                    #   about 1.3 GiB. It does NOT follow the
                                    #   mutation log's ceiling as the knowledge
                                    #   base's does: chat grows faster (~32 MB a
                                    #   day against ~22), so following it would
                                    #   make chat the largest ask in the one
                                    #   budget, and a first boot would need
                                    #   ~138 GiB free rather than ~100 before the
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
  # executor_log_max_bytes: 536870912
                                    #   the executor log, where every seat's
                                    #   executor sessions are NAMED: a row per
                                    #   turn naming the sealed object its
                                    #   harness's session is kept in, in the
                                    #   object store (64 MiB..64 GiB). UNSET
                                    #   TAKES 512 MiB, sized by the turns the
                                    #   seats run rather than by the disk, and
                                    #   scaled down as far as 64 MiB where the
                                    #   broker is short: about seventeen days of
                                    #   a completely blocked trim at the
                                    #   reference company's ~32 MB a day, and
                                    #   at 64 MiB about two, short of min_age —
                                    #   past ~30 seats on a small host, set it.
                                    #   Crossing it refuses the append, and the
                                    #   top of it is kept for gate records. How
                                    #   long a session is kept is the company
                                    #   document's turn_engine.
                                    #   conversation_session.retention_days
  # access_log_max_bytes: 134217728
                                    #   the access log, where what each seat
                                    #   was told about its access and every
                                    #   SSH certificate and command are
                                    #   recorded (64 MiB..64 GiB). UNSET TAKES
                                    #   128 MiB, sized by those records rather
                                    #   than by the disk, and scaled down as far
                                    #   as 64 MiB where the broker is short:
                                    #   about two months of a completely blocked
                                    #   trim at the reference company's ~2 MB a
                                    #   day. How long the records are kept is
                                    #   the company document's
                                    #   access_history.retention_days
  # atlas_log_max_bytes: 134217728  #   the atlas log, where what people
                                    #   record about what the company runs on
                                    #   is kept with its history (64 MiB..64
                                    #   GiB). UNSET TAKES 128 MiB, sized by
                                    #   what people record rather than by the
                                    #   disk, and scaled down as far as 64 MiB
                                    #   where the broker is short: about four
                                    #   months of a completely blocked trim at
                                    #   the reference company's rate. How long
                                    #   the history is kept is the company
                                    #   document's atlas.retention_days
  # advisor_log_max_bytes: 67108864 #   the advisor log, where the Advisor's
                                    #   findings, runs, weekly reports and
                                    #   alarm episodes are kept with their
                                    #   history (64 MiB..64 GiB). UNSET TAKES
                                    #   64 MiB, its floor, sized by what the
                                    #   Advisor records rather than by the
                                    #   disk: about ten days of a completely
                                    #   blocked trim at the reference company's
                                    #   ~6 MB a day. Closed findings are kept
                                    #   90 days, runs 30, reports 26 weeks
  # code_log_max_bytes: 11453246122 #   the code log's ceiling (1 GiB..1 TiB):
                                    #   the ordered stream a repository's refs,
                                    #   packs and settings move on. It runs on
                                    #   every node that runs a state log,
                                    #   whether or not the company hosts code.
                                    #   UNSET DERIVES two thirds of the
                                    #   mutation log's ceiling, set or derived
                                    #   (so 2⅔..42⅔ GiB from a volume alone):
                                    #   the least of the byte rate of a
                                    #   company whose seats only push, at
                                    #   which a blocked trim fills either log
                                    #   in the same time. Once they work
                                    #   through changes, its push
                                    #   records, the records that compact
                                    #   their packs, its git clients' read
                                    #   barriers and its changes' records —
                                    #   opens, reviews, comments, merges, each
                                    #   revision's facts and mergeability, and
                                    #   its queue's and checks' — take nearly
                                    #   twice to about eight times the
                                    #   bytes a day its tracker writes and its
                                    #   seats' reads do. Before the first
                                    #   boot neither log exists, so such a
                                    #   company sets this from that day:
                                    #   twice the mutation log's ceiling, its
                                    #   least, which comes out of the other
                                    #   logs' share whole (at 100 GiB free,
                                    #   13.3 GiB beside a mutation log of
                                    #   6.6 GiB). Once the streams exist, the
                                    #   ratio the two logs' bytes_per_day
                                    #   show in `crewlet retention status` is
                                    #   its own, and `crewlet retention
                                    #   set-capacity` moves this log to it
                                    #   (guides/replication.md, "What the code
                                    #   log's reservation costs"). A push
                                    #   record names its pack and never
                                    #   carries it — the bytes are the
                                    #   object store's — so how large the
                                    #   repositories grow does not reach this
                                    #   ceiling. Crossing it refuses the append,
                                    #   like the mutation log's. The RESERVATION
                                    #   IS PAID WHETHER OR NOT ANYTHING PUSHES:
                                    #   on a volume small enough that the
                                    #   logs' derived ceilings are scaled to
                                    #   fit their share of the broker, this
                                    #   one costs the others room. A company
                                    #   that hosts no code
                                    #   and is short of broker room sets the
                                    #   1 GiB floor (1073741824) BEFORE the
                                    #   first boot; a stream that exists keeps
                                    #   its ceiling, so after it `crewlet
                                    #   retention set-capacity` is what lowers
                                    #   this one and raises another
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
                                    #   life of the deployment. Otherwise at
                                    #   least 0.25: the dead letters are kept by
                                    #   this age, and a second keep of one thing
                                    #   is collapsed for fifteen minutes, which
                                    #   the broker refuses a stream's age to be
                                    #   shorter than
  # credentials: ""                 # path to a NATS credentials file. type: nats
                                    #   needs one of credentials, token, tls.cert
                                    #   or a credential in url, and refuses a
                                    #   server that authenticates nobody
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
                                    #   five eighths and a 1 GiB coordination
                                    #   floor leave. A full bucket refuses
                                    #   uploads, never the fleet's leases. A
                                    #   data node raises an existing bucket to
                                    #   a larger figure at boot where the
                                    #   broker has room, and never lowers it.
                                    #   REFUSED with backend s3
  #   s3:                           #   REFUSED unless backend is s3
  #     endpoint: ""                #   the S3 API's base URL; empty is Amazon
                                    #   S3's own for the region. R2, MinIO,
                                    #   Ceph's gateway and GCS's interoperability
                                    #   endpoint each name theirs here: http or
                                    #   https, a host, and a port of 1..65535 if
                                    #   it has one. One no request can reach is
                                    #   REFUSED, naming what is wrong with it.
                                    #   At most 2048 characters
  #     region: eu-west-1           #   REQUIRED — every request is signed for
                                    #   a region, even where the provider
                                    #   ignores it (R2: auto, MinIO: us-east-1)
  #     bucket: acme-files          #   REQUIRED. Probed at boot under the
                                    #   prefix: a node that cannot store and
                                    #   delete an object there does not start.
                                    #   At most 255 characters
  #     prefix: ""                  #   prepended to every key: an object is
                                    #   `<prefix>files/<key>`, e.g. `acme/`.
                                    #   Companies sharing one bucket are kept
                                    #   apart only by credentials scoped to
                                    #   their own prefix, never by the prefix.
                                    #   At most 1024 characters
  #     path_style: false           #   address the bucket in the path rather
                                    #   than the host name; MinIO and most
                                    #   self-hosted gateways need it
  #     access_key_id: ${S3_ACCESS_KEY_ID}         # both as ${VAR} references,
  #     secret_access_key: ${S3_SECRET_ACCESS_KEY} #   or NEITHER, which takes
                                    #   the SDK's own chain: the environment, a
                                    #   shared profile, a web identity or the
                                    #   instance's role. A private CA is read
                                    #   from AWS_CA_BUNDLE

# gitlet:                           # how THIS node serves the company's
                                    #   repositories — read only where the node
                                    #   has the `gitlet` role and the company sets
                                    #   code.backend: gitlet (concepts/gitlet.md). A
                                    #   block on a node without the role is
                                    #   warned about: nothing reads it
#   cache_dir: "git"                #   its disposable copies of the
                                    #   repositories. Absolute, or relative to
                                    #   the store's directory; unset is
                                    #   <store dir>/git. A volume of its own is
                                    #   the production shape. Never backed up:
                                    #   it is rebuilt from the code log and the
                                    #   object store, and carries a
                                    #   CACHEDIR.TAG. A directory holding the
                                    #   store, its snapshots or the replicated
                                    #   estate is REFUSED: the cache empties its
                                    #   own tmp/, scratch/ and trash/ at boot
#   cache_max_bytes: 0              #   the budget it evicts down to, least
                                    #   recently used first; at least 1 GiB when
                                    #   set. UNSET is derived at every boot from
                                    #   the ROOM the cache can use: the volume's
                                    #   free space PLUS what the cache already
                                    #   holds (so a warm restart derives what
                                    #   its first boot did), LESS what a broker
                                    #   storing its streams on that volume may
                                    #   still write under its limit, the
                                    #   request spools' allowance and the
                                    #   facts' patch spools. Half of that
                                    #   where the volume also holds the store,
                                    #   its snapshots or the broker's streams,
                                    #   seven eighths of a volume of its own;
                                    #   never more, no floor, no ceiling, and
                                    #   a volume leaving NO room refuses
                                    #   Gitlet, naming what to change.
                                    #   Logged as code_cache_sized
#   max_processes: 0                #   git processes run at once to answer
                                    #   reads, one core each, and half as many
                                    #   again for background maintenance; 0 is
                                    #   one a core
#   max_transfers: 0                #   clones, fetches and pushes served at
                                    #   once; 0 is four a process slot and no
                                    #   more than one per 512 MiB of the memory
                                    #   the executor's pools leave: a transfer
                                    #   holds memory for its whole life, so
                                    #   size it to memory
#   max_checks: 4                   #   the company's checks this node runs at
                                    #   once in container cells; unset is a
                                    #   quarter of its cores, at least one; 0
                                    #   is none
#   import_allow_private: false     #   let this node's imports and mirrors
                                    #   (pull and push alike) dial a host on a
                                    #   network that is not the public internet,
                                    #   such as a forge beside the node. Off, a
                                    #   host any of whose addresses is not public
                                    #   is refused before git runs; a proxy
                                    #   HTTPS_PROXY names is dialed either way

coordination:
  type: local                       # one node holding its own seat leases;
                                    #   a fleet needs `embedded-kv`. There is no
                                    #   address to give it: the coordination KV
                                    #   lives on the stream's OWN broker. On the
                                    #   embedded one it holds a second
                                    #   connection, and holds ride a lease
                                    #   connection of their own; each is
                                    #   watched: any closing for good stops the
                                    #   node, so it never holds live leases
                                    #   while the one carrying its inbox is gone
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
                                    #   bucket while the fleet is down.

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
                        #   listener, where vendors reach it too. At most
                        #   2048 characters
  trusted_proxies: ["10.0.0.0/8"]
                        # whose X-Forwarded-For is believed. A CIDR list,
                        #   never a bool; 0.0.0.0/0 is refused. Empty with
                        #   an https external_url is warned about: whatever
                        #   terminates the TLS is then every caller's
                        #   source — in every audit row, and in the
                        #   sign-in throttle's key for every login
  public:               # optional — a second listener for the routes outside
    port: 8443          #   parties call: /webhooks/*, /otlp/{token}, /mcp/{token},
                        #   and a Gitlet node's /box/git/* and /box/gitlet/* (a box's
                        #   git, and a check's box's reads of its commit).
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
                        #   same address. At most 2048 characters
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
    limits:             # THIS node's bounds on the company's sign-in
                        #   policy (Tier B `sign_in`). A company value past
                        #   one is refused by this node's writes and held to
                        #   the limit by its sign-ins
      second_factor: required    # required (default): the company may not
                                 #   drop the second factor | optional: it
                                 #   may. optional off loopback is refused
                                 #   without accept_insecure
      min_password_length: 12    # 12..256 (default 12): the lowest password
                                 #   floor the company may set
      max_session_lifetime: 720h # 1h..720h (default 720h)
      max_idle_timeout: 720h     # 15m..720h (default 720h)
      max_step_up: 24h           # 5m..24h (default 24h)
    audit:
      changes: 9600h    # 2160h..24000h (default 9600h = 400 days)
      sessions: 2160h   # 168h..9600h   (default 2160h = 90 days), and never
                        #   longer than `changes`
    accept_insecure: false   # acknowledge an insecure posture off loopback:
                             #   what lets limits.second_factor: optional
                             #   stand on a routable address
    tokens:             # the DEPLOYMENT's machine credentials. At least one
                        #   is required once port is set
      - id: founder     # acts under the login token:founder — lowercase
                        #   letters, digits and hyphens, optionally joined
                        #   by colons (ci-pipeline, ci:release), at most 58
                        #   characters so the login stays within 64
        token: "${CREWLET_API_TOKEN_FOUNDER}"   # 26+ chars, on the RESOLVED value
        grants: [admin]  # admin, worklet, doclet, artifacts, chatlet, gitlet, atlas:
                        #   what it may do, and the most an invitation or a
                        #   create it makes may confer — the first person's
                        #   included. admin includes every app; on a node
                        #   whose admin is refuse, listing it is refused. A
                        #   CI token that fetches the company's code needs
                        #   gitlet

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

dictation:                # optional: voice dictation, on with no setup. See
                          #   the Voice Dictation guide
  enabled: true           # unset = true; false serves no dictation here
  recognizer_dir: ""      # a prepared recognizer (`crewlet speech prepare
                          #   -dir`), read only; empty uses the image's or
                          #   downloads it once (about 230 MB)
  max_concurrent: 0       # live dictations at once; 0 = 4 per available core
  session_seconds: 300    # the longest dictation, by time and audio (1..3600)
  idle_seconds: 15        # ends one that sends no audio (1..300)
```

**The sign-in policy is the company's; `api.auth.limits` bounds it on this node.** Second factor, shortest password, session lifetime, idle limit and step-up window are Tier B (`sign_in`, see [Sign-in](#sign-in)), changed from Settings › Sign-in policy without a restart. An administrator reads them, this node's limits and what it enforces through the [`sign_in_policy`](../reference/api-endpoints.md#queries) question. Changing a limit is an edit to this file and a restart.

**`crewlet validate` prints warnings as well as refusals.** The exit code turns on refusals alone, so CI gates on what cannot run and still prints what an operator should read. A warning is a valid configuration with a consequence: a declined fsync's window, a trim that never advances until somebody acknowledges a backup, a `stream.cluster.peers` entry left out of the member count (this node's own route or a repeat), an `ingress` node that binds no port, a `gitlet` block on a node that serves no git, a unit keyed on a name somebody will rename, a company with no human seat (it can admit nobody). Given both files it also prints warnings neither raises alone (a company hosting code on a fleet's default object store; a Gitlet node whose cache budget cannot hold a repository at its size limit); a configuration write answers them too. See [Warnings across the two tiers](../concepts/configuration.md#warnings-across-the-two-tiers).
Most deployments never run `validate` (an image, chart or operator renders the document and starts the node), so `crewlet run` logs every Tier A warning at every start, one `config_advisory` line at `WARN` each, into the log file when there is one, judged with that run's flags folded in (see [`crewlet run`](../reference/cli.md#every-start-says-what-its-tier-a-is-worth-knowing)). Every node logs a company's advisories once per epoch it applies; a company seeded with `-company` gets no other report (see [What an applied company is worth knowing](../concepts/configuration.md#what-an-applied-company-is-worth-knowing)).

`api.port` is **one** listener per node; `node.roles` decides what it carries. A node without `ingress` serves only its two [probes](../reference/api-endpoints.md#probes-on-a-node-without-ingress) there, so one Tier A file serves both shapes and there is no separate probe port. `crewlet validate` warns (not refuses, since `crewlet run -api-port` sets the port at run time) about a node whose roles include `ingress` while its port is `0`; `crewlet run` judges it on the port and roles its flags leave, logging `config_advisory` at every start.

`api.host` and `api.port` are what this node **binds**, rarely where it **answers**: a fleet behind a load balancer binds `0.0.0.0:8000` and is reached at `https://crewlet.example.com`. That outside address is **`api.external_url`**, **required** once a port is set on a node serving the API, at most 2,048 characters like `api.public.external_url` (an integration's status records the vendors' address exactly, to notice it moving: [Integration reconcile](../concepts/integration-reconcile.md)). It is Tier A because it answers questions before any company document loads: the session cookie's `Secure` flag and `__Host-` prefix follow its scheme, and its host is the origin every write and socket handshake is checked against. Every link the engine composes for a person uses it (`${crewlet_base_url}` in tool-skill prose, a page change's `url`, an invitation or password link), as does every webhook URL and pasted app manifest on a deployment with no public listener. A TLS-terminating proxy hides all of this from the request.

**`api.public` splits the routes outside parties call onto a socket of their own.** With `api.public.port` set, the vendor webhooks and app landings (`/webhooks/*`) and the sandbox endpoints (`/otlp/{token}`, `/mcp/{token}`, and on a Gitlet node a box's git, `/box/git/*`, and a check's box's reads of its commit, `/box/gitlet/*`) are served on that listener only. Every other route (dashboard, REST API, sign-in routes under `/auth`, `/config`, `/secrets`, `/setup`, `/iam`, `/operator`, the live socket, the `/health` and `/ready` probes) is served only on `api.port`. `api.port` answers a public route with a `404 no_route` whose hint names `api.public`. The public listener answers every other route as it answers a path nothing serves, with no hint (it reveals nothing of the admin port), whatever credential is sent, since it requires none. Its outside address is **`api.public.external_url`**, required once the port is set on an ingress node: webhook URLs and vendor app manifests are built on it, links a person follows stay on `api.external_url`. The per-run sandbox endpoints do not move: a box is handed its bridge at `CREWLET_MCP_BRIDGE_URL` and its telemetry receiver at `CREWLET_SANDBOX_OTEL_RECEIVER_URL`. Publish `api.public.port`, keep `api.port` private, and point both variables at the public address (a `direct` box dials its own node on loopback with nothing to set: an ingress node's public listener, or `api.port` on a node without the role); see [Deployment → Exposing webhooks without the admin API](../guides/deployment.md#exposing-webhooks-without-the-admin-api). A node without `ingress` binds no public listener; its seats' executor runs reach their tools through an ingress node's bridge, which relays each call. `crewlet validate` refuses a public listener beside `api.port: 0` on an `ingress` node (no HTTP surface, no probe); without the role `api.port: 0` stays the hard off switch. It also refuses any two listeners the file opens on one port (`api.port`, `api.public.port`, `stream.cluster.port`, `stream.leaf.port`) when they bind the same address or either binds every interface; `-roles`, `-api-host` and `-api-port` are held to the same rules.

**`api.artifacts` is where artifacts are opened**, on a listener of its own: an artifact is a page somebody or some agent wrote, and nothing it runs may share an origin with the dashboard's session. It serves four routes: one file of a version under a view ticket (`/t/{ticket}/...`), one asset of the ticket's artifact (`/t/{ticket}/_assets/{asset}`), and the embedded runtime and kit (`/_crewlet/runtime/{contract}/...`, `/_crewlet/kit/{version}/...`), public and cached for a year under a never-reused name. Every other path, `/health` included, is an empty `404`; it never reads or sets a cookie. The dashboard mints a ticket for the signed-in viewer; it lives an hour (never past the credential it was minted through) and every document request is authorized again, so a withdrawn share, ended session or trashed artifact stops the next one. A node whose copy of the artifacts or identity log is out of service or more than the stall grace behind decides through a data node whose copy answers, and answers `503` with `Retry-After` when none does. **`api.artifacts.external_url`** decides the origin mode: a template naming `{artifact}` at the start of its leftmost label gives each artifact a real origin, only on a registrable domain other than the dashboard's (checked against the Public Suffix List). On the dashboard's own site, or with a plain origin, every document gets an opaque origin and no browser storage, and `crewlet validate` warns about the same-site template. Validation refuses a template past 2,048 characters, an artifact origin equal to `api.external_url`'s, `api.public.external_url`'s or an `api.auth.allowed_origins` entry, a parent domain of the API's host, plain `http` off loopback, and a port with no `external_url` on an ingress node. A proxy in front must pass the original `Host` (checked against the host the ticket's artifact is served on) and the `Sec-Fetch-*` headers, leave response headers alone, and never cache a `/t/` path in a shared cache. The dashboard on every ingress node names this origin in its `frame-src`, whether or not that node binds the listener. A page's policy keeps a viewer's row from running as script and limits its fetches, sockets, images and media to its own origin, but a script the page's own script creates may load from any site, so an author who means to can send what viewers may read elsewhere: share `editor` accordingly (see the artifact note in `SECURITY.md`).

**`api.auth.admin` says whether this node serves admin**, required once a port is set: `serve`, or `refuse`. A person's [permissions](../concepts/identity-and-access.md#access-admin-and-one-permission-per-app) (`admin`, `worklet`, `doclet`, `artifacts`, `chatlet`, `gitlet`, `atlas`) live in the replicated store, written by an administrator; this is each node's statement, in the tier that holds the keyring and never read from Tier B, of whether `admin` counts here. A `refuse` node clamps admin away from every principal composed on it and keeps every app (admin includes the apps, expanded before admin is removed), so a node published to the company's users cannot be used to administer it, however their row reads. A zero or any other value is refused, naming both values. It applies **per node, per request**: a restart that changes it takes effect on that node's next request, with no apply or migration, so a fleet whose nodes disagree is legal during a rollout. Each node publishes its posture on its presence lease, and the fleet view (`GET /query/fleet`) shows it as `admin` per node.

**At least one `api.auth.tokens` entry is required.** A fresh deployment's identity estate is empty, so a Tier A token puts the first person in it, invited onto or created on the human seat the company declares for them: from that seat's page or panel in **Company › Structure**, or with
`crewlet iam invite <address> -seat <handle>` (`crewlet iam create
-email <address> -seat <handle>` creates them instead and prints their first
password link). Every person holds a human seat, so a company must declare one before anybody can join. An invitation or create confers only what its issuer holds, so the token must hold `admin` — `grants:
[admin]` — on a node that serves it. On a running deployment it is the way back in when an administrator is locked out. Each entry states its own
`grants` — required and non-empty, because a credential's blast radius belongs where it is pinned, naming only `admin`, `worklet`, `doclet`, `artifacts`,
`chatlet` and `gitlet` (an unknown name is refused naming the set, and `admin`
on a `refuse` node is refused too; a CI runner that fetches the company's code
is `[gitlet]`) — and its value must be at least 26 characters,
checked on what the `${VAR}` resolves to. A value may not start with `cwl_ast_`, the prefix the
engine reserves for Ask Crewlet's in-process grants: the request
guard reads that shape as a grant, so such a token could never authenticate.

**`secrets.keys` is required on every node**, port or not. Every record on every state log is signed and verified under the keyring, the company document a node fetches from peers is authenticated by its seal, and once a port is set it signs every session cookie and derives the key that verifies each per-run token. `crewlet validate` refuses a Tier A file without one; see [Configuration § What Tier A must state on every node](../concepts/configuration.md#what-tier-a-must-state-on-every-node).

Binding a credential to a seat is in neither tier. A token acts under the
login `token:<id>`, and acts **as a seat** when the identity directory holds a row under that login bound to one: `crewlet iam create -kind machine -login
token:<id> -seat <handle> -grants none`, which takes `admin` — the row binds the token to a seat and carries nothing itself,
since the token's permissions are its Tier A entry's (`crewlet iam bind`
moves an existing row onto a seat, and `crewlet iam unbind` takes it back). The seat must be a human seat nothing else holds (no person, other row or open invitation; one row holds a seat). There is never a `seat:` field on the token, because Tier A holds the keys to the secret store and may never read Tier B, nor a field on the seat's `contact` block, which says how to reach a person, not which credential they hold. See [Humans in the Org
Chart](../concepts/humans-in-the-org.md#acting-as-your-seat-on-the-dashboard-and-the-api).

**Give the first person `admin`, on a node that serves it.** They carry what their invitation or create conferred and may confer only what they hold, so a company whose people hold no `admin`, or whose only `serve` node is one nobody signs in on, cannot invite, create, move, suspend or remove anybody; `crewlet iam check` names it `no_admin_holder`, and the Tier A token stays the way back in.

The event store (LLM observability) is a table in that same file, created by the engine's migrations on first start; configure nothing beyond `store.path`. See [Deployment → The event store](../guides/deployment.md#the-event-store) for the layout.

---

## Organization Structure

Roles live at the **org root** (org-wide agents like a CEO or advisor) or **inside units** (team-scoped agents). Both are optional: root only, units only, or both.

### Root-Level Roles

```yaml
roles:                                    # optional — org-wide agents
  - name: CEO
    goal: "Set company direction"
    manages: [vp-eng, pm-lead]            # seat HANDLES, or a unit's KEY
    # ... same role fields as unit roles (see table below)
```

A `manages:` entry is a **handle or a unit key**, never a display name. A seat's handle is `handle:` where declared, else the slug of its name; a unit's key is its `id`. Both survive a rename, which a display name does not: `manages: [VP Engineering]` silently pointed at nobody once that seat was retitled.

Root-level roles take full part in the `manages[]` hierarchy and task routing. Their knowledge is scoped to the org (visible to all agents). They inherit no `mcp_env` from any unit.

### Units

The `units` key defines the org as a recursive tree of `OrgUnit` nodes. Each unit holds roles directly and/or child units, to any depth.

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
| `name` | string | yes | The seat's display name, for people. Nothing refers to the seat by it: `manages:` and a unit's `lead:` take the `handle` below |
| `kind` | `agent` \| `human` | no | Who holds the seat (default `agent`). `human` marks a [human seat](../concepts/humans-in-the-org.md), addressable, never spawned; rejects every runtime-only field below. It is also where a person joins: every person holds exactly one human seat, invited onto it or created on it in **Company › Structure** (or `crewlet iam invite -seat`). A company with no `kind: human` seat can admit nobody (legitimate when its agents run alone through Tier A tokens) and gets an `advisory` warning at `roles` from `crewlet validate` and every `/config` write, logged by every node that applies the company. `contact` identities are optional: a seat with none is legitimate, and the same three name it in an `advisory` warning at the seat's `contact` |
| `contact` | dict | no | A human seat's external identities, optional; refused on an agent seat: `slack_user_id`, `mattermost_user_id` (a username, not an ID: the engine asks the Mattermost server which account holds it, and reports one nobody holds), `atlassian_account_id` (Jira+Confluence), `github_login`, `gitlab_username`. Each takes a literal ID or exactly one whole-value `${VAR}`, resolved at use time through the [secret store](../concepts/secret-store.md) then the environment, by every consumer alike. Values are whitespace-stripped; the names of platforms that ignore case (`github_login`, `gitlab_username`, `mattermost_user_id`) are lowercased, Slack and Atlassian ids keep theirs; a `${VAR}` inside a longer string is rejected at validation (see [Humans in the Org Chart](../concepts/humans-in-the-org.md)). **No two seats may declare the same identity**: an account belongs to one person, and a duplicate is refused rather than sending one person another's mail. Each is a place a message can be sent; who a person is on the engine's own surface (dashboard, API, operator tool server) lives in the identity directory, which binds each person to the seat they were invited or created on (`crewlet iam bind` moves them; see [Acting as your seat](../concepts/humans-in-the-org.md#acting-as-your-seat-on-the-dashboard-and-the-api)). **A seat known by its contacts alone has agents use no app for it:** a permission is a person's, so a message from a seat no person holds wakes an agent that uses no Worklet, Doclet, Chatlet or artifacts verb for it. Create the person on the seat with their apps (`crewlet iam create -email … -seat … -grants …`); they are active at once and need never sign in ([An agent uses only the apps of the person it works for](../concepts/identity-and-access.md#an-agent-uses-only-the-apps-of-the-person-it-works-for)) |
| `availability` | string | no | Human seats only: free-text availability rendered into rosters and `lookup_colleague` results |
| `avatar` | dict | no | **Agent seats only.** How the dashboard draws the seat: `character`, one of the thirty Crewlet characters (`crewlet`, `hexlet`, `peaklet`, `prismlet`, `towerlet`, `bricklet`, `gemlet`, `coglet`, `pluslet`, `shieldlet`, `stacklet`, `sparklet`, `cloudlet`, `foxlet`, `rocketlet`, `crownlet`, `cactlet`, `wisplet`, `heartlet`, `flasklet`, `chiplet`, `duolet`, `chatlet`, `pagelet`, `archlet`, `dashlet`, `octlet`, `conelet`, `hivelet`, `folderlet`), and `color`, one of `purple`, `cyan`, `green`, `amber`, `rose` or `blue`. Either may be omitted: the engine picks the absent part from the seat's handle (minted once, never changes), so a renamed seat keeps its look. Every read that serves a seat to the dashboard serves the resolved look. Presentation only: no prompt, route or placement reads it. An id outside those lists, or the block on a human seat, is refused by an [admission rule](../concepts/configuration.md#what-a-stored-revision-is-held-to). The org builder offers both as pickers in the seat's editor and Add dialog, which suggests a character no agent wears yet |
| `goal` | string | no | Individual mission statement |
| `backstory` | string | no | Personality, background, expertise |
| `llm` | string or dict | no | Named LLM provider key: **the executor's**, so the seat's own model. Dict form adds `default`, `review`, `subagent`, `auxiliary`, `judge`, `triage` keys for the [per-phase model split](../concepts/turn-engine.md#per-phase-llm-models); there is no `execute` key (`llm` is it) and no `sandbox` key (the executor's run is the seat's code work). Only `claude-code`, `codex`, `gemini-cli`, `qwen-code`, `opencode`, `cursor-agent`, `copilot`, `grok`, `muse-code`, `kimi-code`, `hermes` and `pi` entries can run the executor: a seat whose chain holds none of them is refused at its `llm` (a seat naming no `llm`, at the fallback entry's `agent`), naming both remedies. A chain mixing one with another harness validates with an advisory warning: a launch skips the other harness, which still serves the reviewer, workers and side passes |
| `llm_review` / `llm_subagent` | string | no | Per-phase overrides (alternative to the dict-shaped `llm`) |
| `llm_auxiliary` | string | no | Cheap/fast model for the turn-start prefetch, condensing large tool results, and the reflection workers (PersistDecider, episode summariser). Each call pays a harness launch under a fixed deadline, so name a fast model; see [Auxiliary work and latency](../concepts/llm-backends.md#auxiliary-work-and-latency) |
| `llm_judge` | string | no | Cheap/fast model for the [round-cap extension judge](../concepts/turn-engine.md#round-cap-extension-judge) of the onboarding pass; falls back to `llm` |
| `llm_triage` | string | no | Cheap/fast model for the [triage](../concepts/turn-engine.md#per-phase-llm-models) of a wake that reaches the seat while its run works, from somebody other than the run's requester ([ADR-0168](../../adr/0168-a-busy-seats-other-wakes-are-triaged-and-the-engine-gates-the-choice.md)); falls back to `llm` |
| `token_budget` | dict | no | This seat's own token ceilings per calendar window (`day`, `week`, `month`, each optional), on top of the company's. See [Token budgets](#token-budgets) |
| `learning_enabled` | bool | no | Overrides the company's [learning](../concepts/agent-learning.md) setting for this seat. Unset inherits; `false` skips reflection for its turns but still writes its episodes, which are cheap and useful to a person |
| `handle` | string | no | Custom identity slug (default: auto-derived) |
| `email` | string | no | Seat email address. **No two seats may declare the same one**: the party registry keys on it and the first seat wins, so the others stop being findable by address and mail resolves to the wrong person. Compared case-insensitively |
| `unit` | string | no | **Root-level seats only.** The **key** of the unit this seat belongs to; the seat is moved into that unit's members before anything reads the org chart, as if written under the unit. It is what `PUT /config/roles/{handle}` writes, so an API-created seat can land in a team without rewriting the unit's block; a hand-written file normally nests the seat instead. A name matching no unit leaves the seat at the root and `crewlet validate` reports a dangling reference. On a seat already nested in a unit it moves nothing, and one naming a *different* unit is refused |
| `manages` | list[string] | no | Seat **handles** or unit **keys** this agent manages; a unit key expands to that unit's seats. Never a display name: a name gets renamed, a handle does not |
| `responsibilities` | list[string] | no | Role responsibilities |
| `behavioral_guidelines` | list[string] | no | Behavioral rules |
| `mcp_env` | dict | no | Per-agent MCP server credentials, keyed by server name: env vars for `stdio` servers, HTTP headers for `http` servers (e.g. `atlassian.JIRA_USERNAME` / `atlassian.JIRA_API_TOKEN`, `confluence.CONFLUENCE_USERNAME` / `confluence.CONFLUENCE_API_TOKEN`, `slack.SLACK_MCP_XOXB_TOKEN`, `mattermost.MATTERMOST_TOKEN`, `github.Authorization: "Bearer …"`). Per-agent tool credentials only; scope a server via its own filter (`JIRA_PROJECTS_FILTER` / `CONFLUENCE_SPACES_FILTER`). The unit's project / space identity is `project` and `space` (below). **Every key must name a declared [`mcp_servers`](#mcp-servers) entry**: credentials are looked up per *server*, so a block keyed on an undeclared name is never read and looks configured while the server starts with none. Six names are the exception and need no server, because the engine reads those blocks itself: `github`, `gitlab`, `datadog`, and Atlassian's `atlassian`, `jira` and `confluence` |
| `code.access` | string | no | **Agent seats only.** How much of a repository this seat may do on [Gitlet](../concepts/gitlet.md): `read_only` (default when unset), `review` or `full_access`, spelled exactly (no hyphen, capital or space). It caps what the chart grants the seat ([who may read and push](../concepts/gitlet.md#who-may-read-and-push)). A human seat carrying it is refused by an [admission rule](../concepts/configuration.md#what-a-stored-revision-is-held-to): a person acts as their own identity on Gitlet, so a tier would cap nothing; what keeps a person off the company's code is the Gitlet permission (`gitlet`). On an agent seat of a company hosting no code of its own (`code.backend` is not `gitlet`) it caps nothing, and `crewlet validate` **warns**, never fails, at the seat's `code.access`. What a seat may do on GitHub is its own app's `permissions`, below |
| `health_tools` | bool | no | **Agent seats only.** Offer this seat the two [health tools](../guides/fleet.md#the-health-tools), `fleet_health` and `code_status` (the administrator's reads of the deployment's health), and wake it each time an alarm fires or clears on a node ([alarm wakes](../concepts/event-system.md#alarm-wakes)). `false` (default) offers neither and wakes it for none: the authority table refuses the seat both tools and leaves them off its surfaces. Setting or clearing it **takes `admin`**, even inside a team the writer leads, because a lead who could set it would read the fleet's health through that seat's trail. It never widens what the seat may read of a repository: `code_status` holds it to its own repository grant. A human seat carrying it is refused by an [admission rule](../concepts/configuration.md#what-a-stored-revision-is-held-to): a person reads health through their own permissions |
| `mcp_limits` | dict | no | **Agent seats, and units.** Narrows the MCP servers granted to this seat, or to every agent seat beneath the unit at any depth, keyed by server name: `enabled: false` turns a server off for them (grant and values stay; `true` is refused), and `tools` holds a tool, by the server's own name, at `approve` (each call waits for a person) or `blocked` (never offered). It never widens: a seat is held to the strictest of its own block, every enclosing unit's and the server's entry. A unit's lead writes it for their own teams; lifting or loosening a limit is an administrator's. See [Narrowing a server for a team](../guides/tools-and-mcp.md#narrowing-a-server-for-a-team) |
| `integrations.github` | dict | no | This seat's own [GitHub App](../integrations/github.md#one-github-app-per-agent), one per agent (an app has one bot identity). A person writes three fields: `enabled` (required; off by default for a new seat), `permissions` (GitHub's own: `contents`, `issues`, `pull_requests`, `checks`, `actions`, `deployments` and `workflows`, each `read` or `write`; one left out is No access, see [Access in GitHub's own terms](../integrations/github.md#access-in-githubs-own-terms)) and `repos` (`owner/name` repositories it works in; empty means every repository the installation covers). `app_id`, `app_slug`, `installation_id`, `private_key` and `webhook_secret` are **written by the engine** when the app is created and installed, the last two as `${VAR}`s pointing at the [secret store](../concepts/secret-store.md) (see [What lands where](../integrations/github.md#what-lands-where)) |
| `integrations.slack` | dict | no | This seat's own Slack app: `enabled`, and for an app created by hand its `bot_token` and `signing_secret` as whole `${VAR}`s. With an app configuration token on the company block the engine creates the app and seals both under names derived from the handle, so a block of only `enabled: true` is the ordinary one; the seat speaks in its unit's `channel` |
| `integrations.mattermost` | dict | no | This agent's Mattermost bot: `enabled` (required), `roles` (the Mattermost roles its bot holds: `system_user`, plus `system_post_all_public` or `system_post_all`, or `system_admin`; required while on where the engine provisions the bot) and `channels` (within `integrations.mattermost.provisioning.channels`; empty means none). Where the engine provisions Mattermost it creates the bot and seals its token under a name derived from the handle, so the seat names no credential. Where it does not (a bot made by hand), `bot_token` names the token as one whole `${VAR}` no other seat or company block names, and the engine binds it to the transport and the Mattermost tool without ever writing it. `username` names the bot, defaulting to the handle. See [Each agent's access](../integrations/mattermost.md) |
| `project` | string | no | **Authored on a unit or root-level role** (→ `org.Unit.Project` / `org.Role.Project`). The team's tracker project as identity: an item naming nobody in the org chart routes to the unit lead, it is the project the team files work under, and on Worklet it is what an item filed with no `unit` belongs to, whoever filed it (see [Worklet](../guides/worklet.md#which-team-an-item-belongs-to)). **Vendor-neutral**: names a Worklet or Jira project, whichever [`tracker.backend`](#tracker) the company runs, so switching backends does not rewrite the org chart. **Not** an MCP credential, and it does **not** scope knowledge reads. Keys are an upper-case letter plus 1–9 upper-case letters or digits (`ENG`, `PROD`), the shape every backend accepts |
| `space` | string | no | **Authored on a unit or root-level role** (→ `org.Unit.Space` / `org.Role.Space`). The team's knowledge container as identity: a page change naming nobody routes to the unit lead, and it is where the team writes. Vendor-neutral and shaped like `project`. It does **not** scope reads (read scope is the org-wide `knowledge.scope` only). The reserved containers (`knowledge.skills_container`, default `TS`, and `knowledge.root_space`, default `HOME`) are refused here |
| `workers` | list[string] | no | Which [worker templates](#worker-templates) this seat may delegate to. **Empty means every one**: requiring each seat to opt in turns a shared library into per-seat copy-paste. A name no template defines is refused at load |
| `sandbox` | dict | no | What this seat's [executor box](../concepts/code-sandbox.md#a-seats-own-box-settings--rolesandbox) reads beyond `providers.sandbox`. Every agent seat's executor is its harness's own run in a box, placed by the `run_in` of the `providers.llm` entry the launch picks (empty takes `providers.sandbox.default_run_in`), so the block places and gates nothing: **absent means the engine-wide settings alone**. Eight keys: `setup`, per-seat provisioning applied after `providers.sandbox.setup`; `env`, the environment injected into the run, where external-service tokens are declared (a `${VAR}` resolving to nothing is left out and the run still starts, logging `sandbox_env_unresolved` naming the key; see [credentials and the run environment](../concepts/code-sandbox.md#credentials-and-the-run-environment)); `optional_keys`, the keys of `env` whose absence is expected, left out without that line (a setup step declares its own `optional_keys`, over its `env` and the paths of its `files`); `mcp.servers`, which of the seat's MCP servers the harness gets beside its tool bridge; `pause_ttl_seconds`, how long a box blocked on a person's answer stays paused (unset inherits `providers.sandbox.default_pause_ttl_seconds`, `0` never holds a paused box, negative refused); `max_turns`, the agentic-round cap on one of the seat's runs (unset inherits `providers.sandbox.default_max_turns`, uncapped when that is unset too; `0` uncapped; negative refused, and so is one past 1,000, since a capped run keeps its harness's record of every request in its box, about 134 MB of Gemini CLI's telemetry at 1,000, on a box root with no per-box quota ([the round cap](../concepts/code-sandbox.md#engine-provider--providerssandbox)); work that needs more is split, or set `0`); `max_concurrent_runs`, how many of the seat's runs hold it at once, at most one per conversation (unset is 2; below 1 refused; see [how a coding task runs](../concepts/code-sandbox.md#how-a-coding-task-runs)); and `browser`, a headless Chromium in the seat's box that its harness drives, refused unless every run of the seat lands in a `container` box off its host's network, on an entry that keeps the web, and withheld at a launch whose box runs on another kernel than the engine's or may reach the host's loopback, or whose harness would then hide the seat's own tools behind its search (see [a seat's browser](../concepts/code-sandbox.md#a-seats-browser)). Any other key is refused as unknown. A setup step's `files` entry (the seat's or an engine-wide one) that a box the seat runs in would refuse is refused on the write, naming the step, path and seat. A `direct` box writes a file only inside its own home (minted per box), so it takes a relative path and refuses every absolute one; a `container` box writes only under its home mount at `/home/user`; either refuses another account's home (`~name/…`), a backslash after a leading `~` (`~\…`), the home itself and a directory above it. Name such a file relative to the box's home (`.npmrc`, or `~/.npmrc`), which every cell writes under its home, or have a step command write it under `"$HOME"` ([setup steps](../concepts/code-sandbox.md#setup-steps--provisioning-the-box)) |
| `placement` | dict | no | Which nodes may run this seat; absent means any node that runs seats (see [Placement](../guides/fleet.md#placement)). Agent seats only: a human seat carrying one is refused. `node` pins it to one node id and `labels` names pairs a node must **all** carry under [`node.labels`](../concepts/configuration.md#nodelabels); give both and both must hold. Compared exactly against what a node advertises, so each part takes the shape a node's own is held to. A **label key**: not empty, at most 63 bytes, no whitespace or unprintable character (`zone`, `topology.example.com/rack`). A **label value**: at most 64 bytes of printable characters; may hold inner spaces or be empty, but none around it (a node's values are trimmed). A selector names **at most 16 labels**, as many as a node can carry. **`node`** is a node id: starts with a letter or digit, holds only letters, digits, `.`, `_` or `-`, at most 64 characters, compared as written, so a `${VAR}` is not resolved here. A selector outside these shapes could never match a node, so it is refused at validation naming the field, not left for the fleet to report as `seats_unplaceable` |
| `schedules` | list | no | Role-scoped recurring tasks, see [Schedules](#schedules) |

### Worker templates

`workers:` holds the reusable delegates a seat's executor hands work to with the `delegate` tool, keyed by the name typed into that call. A template carries the part of a worker that does not change per task (persona, tool set, model, answer shape), so a seat writes only the task.

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
| `description` | string | yes | What this worker is for, written for the **executor choosing one**. The only part of the template that reaches the parent's prompt |
| `system_prompt` | string | yes | The worker's persona and standing instructions. The runtime preamble (no nesting, no colleague contact, how to answer) is appended, so a template cannot weaken the boundary by forgetting it |
| `tools` | list[string] | no | Tools this worker asks for. Empty means none (right for a summariser). **Naming a tool grants nothing**: every name still passes the worker filter (the caller's own live tools, minus the engine-control denylist, minus shared-surface writes), so `workers:` is never a privilege-escalation path |
| `model` | string | no | A `providers.llm` key. An explicit key gets **no fallback chain**: a cheap-model choice that quietly ran elsewhere is worse than a refusal. Omit to take the seat's `llm_subagent` chain |
| `max_turns` | int | no | Tool rounds this worker may run. Refused at load, not clamped, above `turn_engine.delegation.max_turns_ceiling`: a silent clamp would overrule the operator with a number nothing in the file mentions |
| `output` | dict | no | The JSON Schema the worker's `submit_result` publishes, so the answer returns as fields the parent can index. Must be an object schema with 1–12 named properties, at most 3 levels deep, whose `required` entries are properties it has; keywords the engine does not read pass through to the provider untouched |

See [Turn Engine — Workers](../concepts/turn-engine.md#workers) for what a `delegate` call looks like and how dependency waves work.

### Schedules

Roles **and** units can own recurring work via a `schedules:` list (a cron analogue). Each entry fires a task on its cron expression; see [Scheduling](../concepts/scheduling.md).

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
| `name` | string | yes | Unique within the role/unit; part of the idempotency key and of the [conversation](../concepts/scheduling.md#each-fire-continues-the-last-fires-session) every fire is on its runner seat, so a rename starts the schedule's sessions afresh. At most 400 bytes escaped as a URL path segment (a space or slash takes three bytes, a non-ASCII letter six or more) |
| `cron` | string | yes | Standard 5-field cron (`min hour dom month dow`), evaluated in `timezone` |
| `task` | string | yes | Task prompt handed to the runner agent |
| `timezone` | string | no | IANA timezone this schedule fires in (default: the company's [`timezone`](#the-companys-clock)). Not `Local` or `localtime` |
| `target` | string | no | **Unit schedules only**: `each` (default; every direct member) or `lead` (the effective unit lead). Ignored for role schedules; for a per-person task, use a role schedule |
| `enabled` | bool | no | `false` keeps the schedule in config without firing (default `true`) |
| `timeout_seconds` | int | no | Hard wall-clock cap on the scheduled turn, its executor's run included (default `1800`, half an hour: [why](../concepts/scheduling.md#hard-wall-clock-timeout)) |
| `catchup` | bool | no | Fire one recent missed tick on restart (default `true`) |

> The at-most-once claim on every fire lives in the fleet's [coordination store](../concepts/coordination.md); each node's own `scheduled_runs` table records what it dispatched, and the schedules' history is derived from it. Schedules are **not** inherited by child units.

---

## Integrations

Inbound and notification integrations live under one `integrations:` block: admin credentials, webhook secrets and outbound `transports`. It carries only **non-tool** config; MCP **tool** servers are declared in [`mcp_servers`](#mcp-servers), and each agent's per-server credentials in `role.mcp_env`.

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

> **There is no address in this block.** Where people reach this deployment is [`api.external_url`](#tier-a); where vendors and sandboxes reach it, when it publishes a listener for them alone, is `api.public.external_url`. Both are in the operator's Tier A file, because the session cookie and origin check are decided *before* a company document loads. `api.external_url` is also what [tool-skill](../concepts/tool-skills.md#skill-variables) prose reaches as the reserved variable `${crewlet_base_url}`, so a company may not declare a `skill_variables` entry of that name.

- **`forge_app_id`**: verifies the Forge Invocation Token (FIT) on Cloud webhooks against Atlassian's JWKS; the `aud` claim must match. Required when Jira or Confluence Cloud delivers through the Forge app. The app id is shared by every site that installs the app, so a token must also name the `cloud_id` that `jira` or `confluence` sets for the event's product: a token names its site by cloud id only, so a block on the relay gives `cloud_id`, not the site's `url`; setup records it from the Atlassian organization when no address is given. Until it is set, that product's relayed events are refused and lost: Forge retries each for about four minutes, then drops it (see [`/webhooks/forge`](../reference/api-endpoints.md#webhooksforge)).
- **`atlassian`**: the Atlassian organization one service account per agent seat is created in, and which Atlassian the company runs. `deployment` is `cloud` or `data_center` (the dashboard form opens on `cloud`); a value here also decides the deployment of a `jira` or `confluence` `url` whose host is not Atlassian's own, which is how an Atlassian Isolated Cloud site on its own base domain is configured. `org_id` and `api_key` (an **unscoped** organization API key) are Cloud only. See [Jira](../integrations/jira.md#configuration).
- **`jira`**: the Atlassian tracker. Give `url` **or** `cloud_id`, never both; `crewlet validate` refuses the ambiguity. `token` is the org read account (an issue's watchers are the one routing input a webhook never carries). `email` switches authentication to Cloud's Basic scheme. `site_url` is the human base for links when the instance is named by a cloud id, and names the Cloud whose gateway that id is read through (Government Cloud's for a `…atlassian-us-gov-mod.net` site). The deployment `url` names is derived from its host and, for a non-Atlassian host, from `atlassian.deployment`. `webhook_secret` is **required for Data Center**; on Cloud it signs the admin webhook the engine registers and is not required, since a company on the Forge relay has no HMAC in its path. Each seat's own credential lives in `mcp_env.atlassian` (or `mcp_env.jira`), and the engine resolves its account id from it. See [Jira](../integrations/jira.md).
- **`confluence`**: the knowledge base, and the **query-time search** behind every turn's "Relevant knowledge" block and the `search_knowledge` tool. Same address rule as `jira`: `url` **or** `cloud_id`. `token` is the org read account a seat with no Confluence credential searches under; a seat WITH one searches as itself and Confluence enforces its page permissions. `webhook_secret` is required for Data Center and unused on Cloud. The knowledge backend is **single-homed**: the engine wires exactly one `knowledge.Searcher`, so an answer never depends on which one was asked. Scope reads with `knowledge.scope`; publish with [`crewlet confluence import`](../reference/cli.md#crewlet-confluence-import). See [Confluence](../integrations/confluence.md).
- **`slack`**: the hosted chat backend. The org-level block carries **no credentials**: each agent has its OWN Slack app, so the token and signing secret live on each role's `integrations.slack`, and this block holds only working-indicator settings. **`typing_status`** is `always` (default) or `addressed`; there is no `off`. Slack's indicator renders text, so `always` costs least here and `status_phrases` is worth having. Inbound events arrive per seat at `/webhooks/slack/{handle}`, verified against that seat's own signing secret. With `app_config_token` (a whole `${VAR}`, entered on the connect form), the engine creates and keeps every agent's app itself. See [Slack Integration](../integrations/slack.md).
- **`mattermost`**: the self-hosted chat backend, and the one integration both inbound and outbound: enabling it starts the outbound transport *and* a websocket fleet holding one connection per agent seat (Mattermost has no usable inbound webhook, so no public URL or tunnel is needed). `url` and `team` are both **required** when enabled. Per-agent identity lives on each role's `integrations.mattermost.bot_token`, named again as `mcp_env.mattermost.MATTERMOST_TOKEN` for the MCP subprocess. **`typing_status`** is `always` (default) or `addressed`; here `always` is expensive, since the indicator is re-asserted every few seconds rather than every 45, costing one to two orders of magnitude more requests than Slack's for less information, so most deployments want `addressed`. There is no `status_phrases`: Mattermost's indicator is fixed client-side text. The `provisioning:` sub-block provisions each agent's bot: its administrator token, and the channels an agent may be given on the integration's [Agents tab](../integrations/mattermost.md#each-agents-access-roles-and-channels). A company may run Mattermost and Slack together (different workspaces, different people). See [Mattermost Integration](../integrations/mattermost.md).
- **`github`**: the hosted code host. `url` is **optional**: unset for github.com (whose API is on a different host), or name an Enterprise Server; the REST base is derived either way. `webhook_secret` is **required** when enabled and takes any string (GitHub signs with it verbatim). The optional `token` is a read credential for **participant fan-out** (a payload carries author, assignees and requested reviewers but not who commented or reviewed), and is what `crewlet github provision` registers webhooks with. Each seat's app token is issued by the engine at use, under a name only that seat resolves ([One GitHub App per agent](../integrations/github.md#one-github-app-per-agent)), and the engine resolves its login with it; a human seat is reached by `contact.github_login`. The `provisioning:` sub-block is read by [`crewlet github provision`](../reference/cli.md#crewlet-github-provision) **and** the engine's reconcile loop, which registers and heals the hooks it names: `org_webhook: auto` takes one organization-level hook where the credential may (covering later repositories) and falls back to one per repository where it may not. A company may run GitHub and GitLab together. See [GitHub Integration](../integrations/github.md).
- **`check_interval_seconds`**: how often a **converged** integration is read back, hence how long access revoked by hand at a vendor goes unnoticed; it is the only thing that finds a revoked credential. Unset takes **600**; the floor is **60**, and a shorter value is refused naming the field, not clamped. Zero means unset, never "off". A converged pass costs one vendor read per seat and per project, per surface, per interval, so a small company can afford `60` and a large one should lengthen it. See [Integration Reconcile](../concepts/integration-reconcile.md#the-cadence-follows-who-has-to-act).
- **`gitlab`**: webhook config and boot-time identity resolution. `url` and `signing_secret` are both **required** when enabled. Inbound webhooks are verified only by the GitLab 19.1+ signing-token HMAC; the plain `X-Gitlab-Token` scheme and self-managed < 19.1 are unsupported. The optional `token` (a PAT with the `read_api` scope, which you supply; nothing provisions it) enables **participants-based routing**: comments and state changes reach everyone participating in the issue/MR, not just assignees and mentioned users. `webhook_name` (default `crewlet`) names the hooks this deployment owns, so a change of public base re-points them instead of leaving orphans; give two deployments watching one instance two names. The GitLab MCP server is a `shared: false` [`mcp_servers`](#mcp-servers) entry, by default the official `glab mcp serve` (stdio, spawned per role, no separate server). Its own `env` sets `GLAB_CHECK_UPDATE`, `GLAB_SHOW_WHATS_NEW` and `GLAB_SEND_TELEMETRY` to `"false"` so glab's own output never lands in a tool result and no call sends the instance a usage event; a variable set only in the engine host's environment does not reach it, since a stdio server gets an allowlist of that environment plus what its configuration declares (see [MCP tool server](../integrations/gitlab.md#mcp-tool-server)). Each agent's service-account token is minted and sealed by the engine where it provisions GitLab, bound to the tool by the server's `seat_credentials`; elsewhere the seat's `mcp_env.gitlab` names the token as an exclusive `${VAR}`. The `provisioning:` sub-block is read by `crewlet gitlab provision` **and** the reconcile loop; `mode` (`group` by default, or `instance` on self-managed) says where a service account is owned, hence which route creates it, mints its tokens and deletes it. The CLI's `-mode` overrides it for one run. See [GitLab Integration](../integrations/gitlab.md).
- **`datadog`**: monitor alerts as inbound events, so a firing monitor wakes a seat. `webhook_token` is **required** when enabled and is compared constant-time against `X-Crewlet-Token`. Datadog attaches only fixed-value headers, so nothing signs the payload and this token is the whole authentication; it must be at least **26 characters** (what the dashboard's Generate button and `crewlet` mint). A route with nothing to compare against answers **503**. `route_to` is **required**: an **agent** seat this company declares, or the literal `none` to dismiss alerts nobody owns on purpose; an unknown handle or a human seat is refused, and no seat may itself be handled `none`. `provisioning` is **required** too: `site` (the region the keys were issued in, checked against Datadog's list), `api_key` and `app_key`, because the engine registers the webhook that makes alerts arrive, and an enabled block without it reports connected and receives nothing. `webhook_name` (default `crewlet`) names that definition and so the handle a monitor writes, `@webhook-crewlet`; give two deployments watching one organization two names. `handle_tag` (default `crewlet`) is the monitor tag key naming a seat: a monitor tagged `crewlet:sre-lead` reaches that seat. See [Datadog Integration](../integrations/datadog.md).
- **`cloudflare`**: one account API token per agent seat, which the engine mints, narrows to the seat's level, rolls before it lapses and deletes, through the provisioner token you create. `provisioning` is **required** when enabled (the integration receives nothing): `account_id` (32 lowercase hexadecimal characters) and `api_token` (the provisioner token, as a `${VAR}`; the dashboard seals it as `CLOUDFLARE_API_TOKEN`) are required; `zones` is optional and names the zones every agent's token reaches, empty meaning every zone in the account. Each seat's access is `roles[].integrations.cloudflare`, with `enabled` and `level` (`read`, the default, `operate`, `configure` or `secure`). See [Cloudflare Integration](../integrations/cloudflare.md).
- **`scaleway`**: one IAM application per agent seat, with one policy granting the seat's level on its projects and its own API key, which the engine makes, narrows, rotates before it lapses and deletes, through the provisioner key you create. `provisioning` is **required** when enabled (the integration receives nothing): `organization_id` (a UUID), `access_key` (`SCW` and 17 capital letters or digits) and `secret_key` (as a `${VAR}`; the dashboard seals it as `SCALEWAY_SECRET_KEY`) are all required. The provisioner key can mint keys for, and grant anything to, any application in the Organization, so give it a short expiry and rotate it. `atlas` (off by default) has the engine make one more IAM application, named for Atlas, with read-only permission sets on every project and a key sealed as `SCALEWAY_ATLAS_SECRET_KEY`, and read the projects' servers, clusters, databases, load balancers, bucket names, DNS zones, containers and functions for the [atlas](../concepts/atlas.md); off deletes the application, its policy and its key. Each seat's access is `roles[].integrations.scaleway`, with `enabled`, `level` (`read`, the default, `operate` or `administer`) and `projects` (the project IDs its policy grants the level on; required while the seat is on). The seat's `sandbox.env` hands its key to the scw CLI as `SCW_SECRET_KEY` and `SCW_ACCESS_KEY`. See [Scaleway Integration](../integrations/scaleway.md).
- **`expo`**: one Expo robot user per agent seat on the company's Expo account; a person creates it at Expo and pastes its token on the Agents tab. Crewlet checks the robot's role against the seat's level, seals the token as `EXPO_TOKEN_<HANDLE>` and reads everything after with it. `provisioning` is **required** when enabled (the integration receives nothing): its one field `account_name` (as in `expo.dev/accounts/<name>`) holds no credential. Each seat's access is `roles[].integrations.expo`, with `enabled` and `level` (`viewer`, the default, `runner`, `developer` or `release_manager`, each an Expo robot role); the seat's `sandbox.env` hands its token to the EAS CLI as `EXPO_TOKEN: "${EXPO_TOKEN_<HANDLE>}"`. See [Expo Integration](../integrations/expo.md). The optional `atlas.token` (a `${VAR}`; the dashboard seals it as `EXPO_ATLAS_TOKEN`) is the access token of a view-only robot made for the atlas alone, which reads the account's projects and their latest builds; a robot with any role but Viewer, or an agent's own, is refused ([let the atlas read the account](../integrations/expo.md#let-the-atlas-read-the-account)).
- **`digitalocean`**: one personal access token per agent seat in the company's DigitalOcean team; a person creates it in the control panel and pastes it on the Agents tab. Crewlet proves its scopes against the seat's level, seals it as `DIGITALOCEAN_TOKEN_<HANDLE>`, reads everything after with it, and revokes it when the seat is turned off or its token replaced. `provisioning` is **required** when enabled: its one field `team` (the name the control panel's team menu shows) holds no credential. Each seat's access is `roles[].integrations.digitalocean`, with `enabled` and `level` (`read`, the default, `operate` or `administer`, each a fixed set of DigitalOcean's custom scopes; `administer` asks for a fresh sign-in); the seat's `sandbox.env` hands its token to doctl as `DIGITALOCEAN_ACCESS_TOKEN: "${DIGITALOCEAN_TOKEN_<HANDLE>}"`. See [DigitalOcean Integration](../integrations/digitalocean.md). The optional `atlas.token` (a `${VAR}`; the dashboard seals it as `DIGITALOCEAN_ATLAS_TOKEN`) is a personal access token made for the atlas alone, with eight read scopes, which reads the team's Droplets, clusters, databases, load balancers, domains, apps and buckets; a token proven able to change anything, or an agent's own, is refused ([let the atlas read the team](../integrations/digitalocean.md#let-the-atlas-read-the-team)).
- **`aws`**: one IAM role per agent seat in the company's AWS account, which the engine creates, levels, turns off and deletes through the provisioner role of a CloudFormation stack it renders, signing in with IAM Roles Anywhere and a certificate authority it mints and seals as `AWS_CERTIFICATE_AUTHORITY`. `provisioning` is **required** when enabled: `slug` (the short name every Crewlet name in the account carries), `home_region`, `regions` (Regions agents may work in; empty is the home Region and us-east-1), `iam_read`, `atlas_read` (off by default; adds a read-only role for the [atlas](../concepts/atlas.md#what-integrations-read) to the stack), `certificate_authority` and `connection` (the stack's `CrewletConnection` output, which may follow the certificate authority, and holds two more ARNs once the stack holds the atlas's role). Each seat's access is `roles[].integrations.aws`, with `enabled` and `level` (`read`, the default, `operate` or `administer`); its session is issued at use for 15 minutes, never sealed. See [AWS Integration](../integrations/aws.md).
- **`google_cloud`**: one service account per agent seat, which the engine creates in an identity project kept for Crewlet's accounts, grants the seat's level on its projects, disables and deletes, through a provisioner service account it reaches by Workload Identity Federation with its own ES256 key; no service account key is made. `provisioning` holds `identity_project` (the identity project's ID; the provisioner is `crewlet-provisioner` there), `provider` (the workload identity provider's full name, as the dashboard's Cloud Shell block prints it), `federation_key` (the engine's key, as a `${VAR}`; the dashboard makes it and seals it as `GOOGLE_CLOUD_FEDERATION_KEY`) and `atlas_read` (off by default; lets the [atlas](../concepts/atlas.md#what-integrations-read) search what each project runs as a service account of its own, which the Cloud Shell block makes). There is no project list: the engine reads the prepared projects from the provisioner's grants. Each seat's access is `roles[].integrations.google_cloud`, with `enabled`, `level` (`viewer`, the default, `operator` or `administer`) and `projects` (at least one when enabled). The seat's token, `GOOGLE_CLOUD_ACCESS_TOKEN_<HANDLE>`, is issued at use for an hour and never sealed. See [Google Cloud Integration](../integrations/google-cloud.md).
- **`ssh`**: the company's servers, reached with short-lived OpenSSH user certificates the engine signs with its own certificate authority. `provisioning` holds `certificate_authority` (the unencrypted Ed25519 OpenSSH private key, as a `${VAR}`; the dashboard makes it and seals it as `SSH_CERTIFICATE_AUTHORITY`; servers trust only its public line), `next_certificate_authority` (a staged authority during rotation, never signed with until promoted) and `source_addresses` (up to 16 ranges written into every certificate). `approvers` names the people who may approve an operate command beside administrators. `always_escalate` adds the company's own patterns (up to 64 RE2 expressions) to the engine's list of commands no [delegate](../guides/delegated-decisions.md) may allow. `commands` bounds a command's run time and returned output. `hosts` lists up to 200 servers, each with an `id`, an `address`, an optional `port`, one to eight pinned `host_keys`, `users.read`, an optional `users.operate`, a `description`, and `inventory` (off by default), which lets the engine read the server's listening ports and containers for the atlas ([let the atlas read a server](../integrations/ssh.md#let-the-atlas-read-a-server)). Each seat's access is `roles[].integrations.ssh`, with `enabled`, `level` (`read`, the default, or `operate`) and `hosts` (at least one when enabled). The seat's certificate is issued at use for minutes and never sealed. See [SSH Integration](../integrations/ssh.md).
- **`google_analytics`**: Google Analytics 4, reached through engine tools as each agent seat's own Google growth identity, which the engine makes in the Google Cloud connection's identity project; it needs `google_cloud` on with its `provisioning` block, and holds no secret. `properties` (optional, numeric IDs) narrows the properties agents may be given; `approvers` (optional) routes an agent's held admin acts to those people first, though only an administrator answers one. Each seat's access is `roles[].integrations.google_analytics`, with `enabled`, `level` (`read`, the default, `operate` or `administer`) and `properties` (at least one when enabled). See [Google Analytics](../integrations/google-analytics.md).
- **`adsense`**: AdSense, reached through engine tools as the company's consent grant: a person who manages the AdSense account signs in with Google once, as the company's own OAuth client (`oauth.client_id`, at most 2,048 bytes as every Google consent client's is, and `oauth.client_secret`, the secret a `${VAR}`; AdMob may share both). `accounts` (optional, publisher IDs) narrows the accounts agents may be given; `approvers` (optional) routes an agent's held public and admin acts to those people first, though only an administrator answers one. Each seat's access is `roles[].integrations.adsense`, with `enabled`, `level` (`read`, the default, `operate` or `administer`) and `accounts` (at least one when enabled). See [AdSense](../integrations/adsense.md).
- **`google_ads`**: Google Ads, reached through engine tools as each agent seat's own Google growth identity, like `google_analytics`; it needs `google_cloud` on with its `provisioning` block, and holds no secret. `customers` (optional) lists the 10-digit accounts agents may be given, each with the `manager` account it is reached through where it is not added directly; `spend.accounts` caps each account's spend (`account`, `currency`, `per_change`, `daily`), and an account without both caps refuses every spend call; `approvers` (optional) routes held acts first. Each seat's access is `roles[].integrations.google_ads`, with `enabled`, `level` (`read`, the default, `operate` or `administer`) and `customers` (at least one when enabled). See [Google Ads](../integrations/google-ads.md).
- **`search_console`**: Google Search Console, with Site Verification and the Indexing API, reached through engine tools as each agent seat's own Google growth identity, which the engine makes in the Google Cloud connection's identity project; it needs `google_cloud` on with its `provisioning` block, and holds no secret. `sites` (optional, at most 1,000) narrows the properties agents may be given, each as Search Console writes it: a URL prefix with its scheme and trailing slash (`https://www.example.com/`) or a domain (`sc-domain:example.com`); `approvers` (optional) routes an agent's held admin acts to those people first, though only an administrator answers one. Each seat's access is `roles[].integrations.search_console`, with `enabled`, `level` (`read`, the default, `operate` or `administer`) and `sites` (at least one when enabled). See [Google Search Console](../integrations/google-search-console.md).
- **`google_play`**: Google Play, reached through engine tools as each agent seat's own Google growth identity, like `google_analytics`; it needs `google_cloud` on with its `provisioning` block, and holds no secret. `apps` (optional) lists the package names agents may be given; `developer_accounts` (optional) names the developer accounts whose users and permissions may be asked about; `reports_buckets` (optional) names each account's monthly reports bucket, read by the company reader; `approvers` (optional) routes held acts first. Each seat's access is `roles[].integrations.google_play`, with `enabled`, `level` (`read`, the default, `operate` or `administer`) and `apps` (at least one when enabled). See [Google Play](../integrations/google-play.md).
- **`transports`**: outbound delivery transports (e.g. `email`). The Mattermost and GitLab transports are derived from the sections above; this list adds others. Jira and Confluence are inbound-only: an agent's writes go through its own MCP tools under its own credential, never the engine. Slack's transport is per seat, derived from each role's own app credentials, so it is not listed here either.

## Knowledge

```yaml
knowledge:
  backend: confluence                    # doclet (default) | confluence | none
  scope: ["ENG", "HANDBOOK"]             # org-wide containers every agent can search (optional)
  skills_container: TS                   # tool-skill pages; excluded from routing and search
  root_space: HOME                       # the organisation's own pages, e.g. the root Onboarding page
  vectors: true                          # fuse semantic recall; unset derives from providers.embeddings
```

`knowledge.backend` is **which knowledge base this company runs**; there is exactly one. Unset **derives**: `confluence` when an `integrations.confluence` block is declared, `doclet` otherwise. Naming `doclet` **beside** an `integrations.confluence` block is refused (pages would live in two places). `none` leaves the `## Relevant knowledge` block empty and does not register `search_knowledge`.

`knowledge.scope` is the org-wide read scope, materialised onto `org.Organization.KnowledgeScope` and read by whichever searcher is wired. **Empty means unscoped, which differs by backend:** on Doclet it is the whole company (every reader is a seat of one company). On Confluence it is whatever the asking seat's own account can read, so a credential-less seat searching unscoped there gets nothing (an unscoped query on the shared org token would let one seat read a page its own account never could). Set it only to **narrow** to a curated floor.

`knowledge.skills_container` (default `TS`) holds [tool-skill](../concepts/tool-skills.md) pages; `knowledge.root_space` (default `HOME`) holds the organisation's own pages, starting with the root Onboarding page every seat reads first. Both are **reserved**: refused as a unit's own `space` and held back from agents' writes, but differently ([who may write where](../concepts/knowledge-system.md#who-may-write-where)). The skills container is machinery: excluded from knowledge search and routing, closed to every agent write, written by a person only with `admin`. The root is the company's canon: searched and routed like any container, closed only to an agent adding a new page. `skills_container` is three-valued: absent takes the default, a name takes that container, and an explicit `""` turns tool skills **off**.

`knowledge.vectors` fuses semantic recall into the search. **Unset derives** from whether `providers.embeddings` is configured; an explicit `true` with no provider is refused.

---

## Tracker

```yaml
tracker:
  backend: worklet                       # worklet (default) | jira | none
```

Which **work tracker** this company runs, on the same terms as `knowledge.backend`. Unset derives `jira` when an `integrations.jira` block is declared and `worklet` otherwise; `worklet` beside `integrations.jira` is refused (work would be filed in two places and a unit's `project` key would name two trackers). `none` means no work tracker: nothing is filed or routed as a work item, and a seat is woken by everything else (chat, schedules, an `a2a_ask`, and connected integrations' webhooks, a GitHub or GitLab issue among them).

The two axes are **separate**: Worklet against a Confluence wiki, or Jira against Doclet, is an ordinary arrangement, with separate routing and lead maps.

**Moving either one to or from the engine's own backend (`worklet`, `doclet`) takes effect when each node restarts**, as moving [`chat.backend`](#chat) to or from `chatlet` does. A node builds Worklet and Doclet once, from the company it started with, so a revision switching one activates normally while the node keeps serving what it was built with, warning `native_backend_needs_restart` at every activation until it restarts (see [A native backend changes at a restart](../concepts/control-plane.md#a-native-backend-changes-at-a-restart)). Moving between two vendors needs no restart, nor does a node's first company; but once a node has run a company that kept every half on a vendor, turning one native waits for its restart too.

The resolved backend of each axis, and nothing else of either block, is published to every signed-in reader as [`services` on the org answer](../reference/api-endpoints.md#get-org), since it decides which dashboard surfaces exist: no Worklet without Worklet, a My work holding only Gitlet's Reviews (none where it hosts no code either), no Doclet without Doclet.

### A person's inbox

```yaml
inbox:
  retention_days: 365                    # how long a person's inbox keeps a notice
                                         #   (default 365, 30..3650). THE HISTORY IT
                                         #   POINTS AT IS UNTOUCHED — this is a
                                         #   mailbox horizon, not an archive one, and
                                         #   "what was I told about last year" is
                                         #   answered by the history either way
```

Inbox retention is a setting of the **whole company**, at the top of its document, not of the tracker: a person's inbox holds every native domain's notices (Worklet's, and Gitlet's about a change where the company sets [`code.backend: gitlet`](#code); [Who a change wakes](../concepts/gitlet.md#who-a-change-wakes)) under one horizon, as the company's one [`timezone`](#the-companys-clock) is one clock. Below a month an inbox stops being one, and past ten years it duplicates the history, so both are refused at `inbox.retention_days`.

Everything else a tracker could be told is a fact about the **operator** (backups, replay window), which lives in Tier A under [`stream.tracker_retention`](#tier-a); or about the whole **company** (the clock, inbox retention); or a decision the engine makes once for everybody.

**Every broker member needs a stream that survives a restart.** A state-log domain's write-ahead log lives on the stream, and an embedded member with no `stream.store_dir` (a single node included) keeps its streams in memory, so a restart recreates them empty, and a node whose durable tables are ahead of a stream that restarted from nothing refuses to serve permanently, with no snapshot that helps. `crewlet validate` refuses it from the Tier A file alone, and so does the engine at boot. A node without `data` is no member: it joins the members' broker as a leaf, keeps no stream, and is refused a `store_dir`; a node dialling an external cluster keeps nothing either (that cluster's storage is its operator's).

It applies whatever backends a company names, and whether or not the node has a company: Worklet, Doclet and Chatlet keep their items, pages and messages there, and the **identity estate** (the people who can sign in) is a domain too, run by every node that holds it from boot.

### Sign-in

```yaml
sign_in:
  require_second_factor: true            # default true: a password sign-in also
                                         #   gives a code from an authenticator app.
                                         #   false needs the node's
                                         #   api.auth.limits.second_factor: optional
  min_password_length: 12                # 12..256 (default 12)
  session_lifetime_seconds: 604800       # 3600..2592000 (default 7 days): from sign-in
  idle_timeout_seconds: 43200            # 900..2592000 (default 12 hours): unused
  step_up_seconds: 3600                  # 300..86400 (default 1 hour): how long a
                                         #   proof of identity authorises a sensitive
                                         #   change before it is asked again
```

The company's sign-in policy is the **company's**, the same on every node and changed from Settings › Sign-in policy without a restart. Each node holds it within its operator's [`api.auth.limits`](#tier-a): a write past a limit is refused, and a value set before the operator lowered the limit is enforced at the limit. A change asks for a recent proof, is recorded in the revision history and audit log, and if it weakens the policy is checked with a warning saying what it allows. A lower lifetime or idle limit ends a live session at its next request; a higher one reaches only later sign-ins. A second factor applies at each person's next sign-in, and a higher password floor at the next password they set. See [Identity and access § The company sets its sign-in policy](../concepts/identity-and-access.md#the-company-sets-its-sign-in-policy-within-each-nodes-limits).

### Delegated decisions

```yaml
delegated_decisions:
  enabled: true                          # off (the default) puts every request to people
  rules:
    - kinds: [ssh_operate]               # what the rule delegates; growth acts and other are never delegated
      delegates: [security-engineer]     # one agent seat, or two on different first llm entries
      escalate_to: [ops-lead]            # at least one person, beside administrators
      sample_one_in: 10                  # optional: one decision in N is spot-checked (1..1000)
    - kinds: [pr_merge, deploy]          # ask_to_merge and ask_to_deploy
      delegates: [release-reviewer]
      escalate_to: [eng-lead]
      always_escalate_paths: [billing/**] # optional, merges and deploys only: at most 64 path globs
      required_checks:                   # optional, merges and deploys only: at most 64 entries
        - repo: acme/api                 # a GitHub repository, owner/name
          branch: main
          checks: [test, lint]           # check run names, 1 to 32
  deadline_seconds: 900                  # optional: a request no delegate answered goes to people (300..1800)
  allows_per_hour: 30                    # optional: allows per kind and delegate each clock hour (1..60)
  server_allows_per_hour: 10             # optional: commands allowed per server each clock hour (1..60)
```

Which requests waiting on a person an **agent seat decides in that person's place**, within rules the engine enforces ([Delegated Decisions](../guides/delegated-decisions.md), [ADR-0125](https://github.com/crewlet/crewlet/blob/main/adr/0125-a-delegate-decides-a-request-only-within-rules-the-engine-enforces.md)). A setting of the whole company, at the top of its document.
- **Rule**: names the kinds it delegates (each kind in one rule at most), one delegate seat or a panel of two that must both allow, and the people an escalation is put to.
- **Delegate**: an agent seat. For `ssh_operate`, a seat holding any `roles[].integrations.ssh` block is refused. Since any agent may ask for a merge or deploy, a `pr_merge` or `deploy` request a delegate made itself goes to people the moment it is held. A panel's two seats must run on different first `llm` entries.
- **`escalate_to`**: at least one person, because who administers the company changes outside the document. Those people may also approve the rule's requests (for `ssh_operate` as `integrations.ssh.approvers` do; for `pr_merge` and `deploy` beside the requesting agent's leads).
- **`always_escalate_paths`**: repository path globs, in Gitlet's path glob syntax, added to the [paths no delegate allows](../guides/delegated-decisions.md#paths-no-delegate-allows). Refused on a rule delegating neither `pr_merge` nor `deploy`.
- **`required_checks`**: per GitHub repository and branch, the checks a merge into it or a deploy from it needs beside its branch protection or rulesets, for a private repository on a plan with neither ([Checks a rule requires](../guides/delegated-decisions.md#checks-a-rule-requires)). Each passes only as a check run completed with `success` on the commit decided, reported by an app that is not one of the company's agents. Refused on a rule delegating neither kind, for a `repo` that is not `owner/name`, a branch that is not a branch name, an entry without checks, a check name that is empty, padded, holding a control character or past 128 bytes, a check or a repository's branch listed twice, and past 64 entries or 32 checks in one. An entry for a repository the company's GitHub does not cover is an advisory.
- **`deadline_seconds`, `allows_per_hour`, `server_allows_per_hour`**: limits held on every delegate whatever it answers; a request past the deadline, or an allow past either hourly cap, goes to people. Each is optional (900 seconds, 30 and 10 when unset), with ranges that cannot switch it off: 300 to 1800 seconds, 1 to 60 an hour; a written 0 is refused.
- **`enabled: false`** keeps the rules and puts every waiting request in front of people on the next read.

---

## Chat

```yaml
chat:
  backend: chatlet                       # chatlet (default) | vendor | none
```

Which **chat surface** this company talks on, the third axis, same shape as the two above. Unset derives `vendor` when the company declares a vendor chat surface and `chatlet` otherwise; "declares" means `integrations.mattermost`, `integrations.slack`, **or any seat's own `integrations.slack` app** (Slack's credentials are per seat, so a company may never have written the org-level block). Naming `chatlet` beside any of them is refused (conversations would live in two places). Naming `vendor` with none declared is refused (no surface to talk on). `none` means seats are woken by the tracker, the code host and schedules only.

The value is `vendor` rather than a product name because a company may run **both** Mattermost and Slack (different workspaces, different people; a migration runs the pair for months).

The resolved backend is published to every signed-in reader beside the other two, as `chat` in [`services` on the org answer](../reference/api-endpoints.md#get-org): no Chatlet in the dashboard unless the company runs it. Which vendors it talks on is told only to a reader holding Chatlet.

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

`message_retention_days` is **three-valued**: left out is a year, `0` keeps messages for ever, 30 to 3650 is that horizon. `inbox.retention_days` reads `0` as "take the default", because an inbox row is derived from history while a message is the only copy of what was said.

Log size (Tier A's `stream.chat_log_max_bytes`) and replay window are the operator's. How much of a thread a woken seat sees is the engine's: one byte budget for every chat surface, whole messages kept, the middle of a long thread condensed (see [the thread a turn was woken in](../concepts/agent-runtime.md#the-thread-a-turn-was-woken-in)).

**A channel two units both declare is refused on Chatlet**: a unit's room belongs to the one unit that names it (its subtree are the members, its lead answers what nobody else did). `crewlet validate` reports it beside each unit: give each its own channel, or leave a child unit's empty to inherit its parent's room. It is an [admission rule](../concepts/configuration.md#what-a-stored-revision-is-held-to): a stored revision with the shape still runs, the refusal reported as a warning. Moving a channel between units is an ordinary edit: the room follows at the next apply, with the new unit's people and the history kept (see [unit rooms](../concepts/chatlet.md#what-a-company-gets)). On Mattermost or Slack it is only a warning, since a company writes it on purpose: everything scoped to one unit lands in the other's room.

**A Chatlet company where no unit declares a `channel`** is warned about at the first unit: no team has a room until somebody opens one and adds its seats. A child unit that inherits its parent's room is neither a warning nor a refusal.

---

## Code

```yaml
code:
  backend: gitlet                       # gitlet, or none (unset): Gitlet hosts the repositories
  limits:                               # every field optional
    max_push_bytes: 1073741824          # 1 MiB..1 GiB; the largest pack one push sends (default 1 GiB)
    max_blob_bytes: 52428800            # 1 KiB..1 GiB, at most max_push_bytes (default 50 MiB)
    repo_max_bytes: 21474836480         # 1 MiB..1 TiB, at least max_push_bytes (default 20 GiB)
    pushes_per_seat_day: 1000           # 1..100000; one seat's pushes a company day, every repository (default 1000)
  deleted_retention_days: 7             # 1..365; how long a removed repository is restorable (default 7)
  scratch:                              # seats' own repositories, made by their tools; every field optional
    idle_days: 7                        # 1..365; idle this long, a scratch repository is deleted (default 7)
    max_per_seat: 50                    # 0..1000; scratch repositories one seat may hold, 0 for none (default 50)
  checks:                               # how the checks repositories define run; every field optional
    env:                                # the secrets a check may name in its env; a check is given only those
      NPM_TOKEN: "${NPM_TOKEN}"
    max_timeout_seconds: 3600           # 60..21600; the longest any check runs (default 3600)
  identity_domain: example.com          # lowercase DNS name; the addresses engine-built commits name (default seats.invalid)
  remote_credentials:                   # the variables a mirror or an import may name, each for the hosts it may go to
    NIMBUS_WEB_MIRROR_TOKEN: { hosts: [forge.example.com] }
    LEGACY_API_TOKEN: { hosts: [forge.example.com] }
  merge_by_floor: writers               # writers (default), maintainers or persons: the least any protected merge needs
  repos:                                # at most 10000
    - name: nimbus-web                  # one host-name label: a-z, 0-9 and -, at most 63
      owner: { unit: engineering }      # exactly one of unit (a key) and seat (a handle)
      visibility: company               # company (default) or unit: which seats read it
      search: indexed                   # indexed (default) or grep: how code search reads it
      default_branch: main              # default main
      description: The customer-facing web app.   # at most 1024 bytes
      archived: false                   # true: read-only, takes no push
      protect:                          # at most 64 rules
        - branch: main                  # exactly one of branch and tag
          merge_by: maintainers         # the merge gate, branch rules only: writers (default), maintainers or persons
          approvals: one                # none (default) or one counted approval
          approvals_unless: [comment_only]  # path classes exempt from approvals: whitespace_only, comment_only
          human_paths:                  # at most 32; a change touching one waits for a person's approval
            - glob: "deploy/**"
              unless: [whitespace_only] # unless every path it matched is of these classes
          checks: [unit, lint]          # at most 32; the checks a merge waits for, by name
          landing: {depth: 4, batch: 2} # the queue's candidates at once and changes a candidate: 1..8 each (default 4 and 2)
          owners_approve: true          # one approval eligible for every owned glob a change touches
          delete_source: seats          # seats (default): a merge deletes a seat's source branch; none
          agents_edit_checks: false     # let a seat approve a change to .crewlet/** on a rule requiring checks
        - branch: release/**
          allow_force: false            # a push not descending from the branch (never on a tag)
          allow_delete: true            # deleting a matching ref
          direct_push: persons          # persons (default): a person, never a seat; none: nothing pushes to it
        - tag: "v*"
      path_owners:                      # at most 64; whom a change touching their paths asks for a review
        - paths: ["web/**", "**/*.css"] # 1..32 globs over the tree, from its root
          unit: engineering             # exactly one of unit (a key) and seat (a handle)
        - paths: ["Makefile", ".github/**"]
          seat: release-bot
      mirror_to:                        # a push mirror: Gitlet pushes these refs to another host
        url: https://forge.example.com/acme/nimbus-web.git   # https; no user, query or fragment; at most 1024 bytes
        credential: ${NIMBUS_WEB_MIRROR_TOKEN}   # one whole ${VAR} code.remote_credentials declares for this host
        as: release-bot                 # the seat whose own identity the push carries; it must read the repository
        refs: [refs/heads/main, "refs/tags/v*"]  # at most 64; default: the default branch and refs/tags/*
    - name: upstream-lib
      owner: { unit: engineering }
      mirror_from: { url: "https://forge.example.com/upstream/lib.git" }   # a pull mirror: no push of its own
    - name: legacy-api
      owner: { unit: engineering }
      import: { url: "https://forge.example.com/acme/api.git", credential: "${LEGACY_API_TOKEN}" }   # read at its create
```

`code.backend` is `gitlet` or `none`; unset is `none` (no vendor block names a host the engine runs in Gitlet's place). Any other value is refused. `gitlet` turns on **Gitlet**, the engine's own git host. Only that setting is published to every signed-in reader, as [`services.code` on the org answer](../reference/api-endpoints.md#get-org), because it decides whether the dashboard draws its Gitlet workspace. Access is the Gitlet permission (`gitlet`), given to a person, a token or a service account like every [app](../concepts/identity-and-access.md#access-admin-and-one-permission-per-app), and held by every agent seat. Refs and packs are records on the code log; every data node runs a **committer** that decides the pushes asked of it. A push goes to its repository's committer, the first data node in a rendezvous order of the repository's id; if it stops or answers nothing, the runner-up takes over with the same push id. A node in a [maintenance mode](../guides/retention.md#the-three-modes) decides none and refuses a push as a node that ran nothing; while the whole fleet is in one, no push is committed. Like `tracker.backend`, it is part of a node's **native runtime**, built once per process from the first company the node runs: a node that has run no company yet starts Gitlet at the activation, no restart; on every other node a revision turning it on or off takes effect at each node's restart, said by a `native_backend_needs_restart` warning naming `code.backend` (see [A native backend changes at a restart](../concepts/control-plane.md#a-native-backend-changes-at-a-restart)). Its log, like every state log, runs on every data node from boot whether or not the company hosts code, on the stream that survives a restart (above).

**Where git is served is Tier A's.** A node with the `gitlet` [role](../concepts/configuration.md#noderoles) (every node declaring no roles) builds Gitlet when its runtime is built for a company setting `code.backend: gitlet`: a disposable cache of the repositories on its own disk, sized and placed by the node's [`gitlet` block](#tier-a), and git 2.55 or newer on its `PATH` (a client needs 2.40), checked at boot; an older git refuses to start, naming version and path. The role needs `ingress`, since git is served on the API listener.

**Changes and their reviews take no block of their own.** [Changes](../concepts/gitlet.md#changes) ([Review a change](../guides/gitlet.md#review-a-change)) come with `code.backend: gitlet` and are shaped by `path_owners` (whom a change asks for review), a `protect` rule's merge gate (what a merge needs), and the top-level [`inbox.retention_days`](#a-persons-inbox) ([Who a change wakes](../concepts/gitlet.md#who-a-change-wakes)). Every person may review, and a seat whose grant on the repository reaches `review` (the chart's, capped by its [`code.access`](#role-fields-summary); [Who may read and push](../concepts/gitlet.md#who-may-read-and-push)).

`code.limits` is read off the **current** revision at every decision, so a change applies at activation. The sizes nest: a file inside a push, a push inside a repository. A revision that turns Gitlet off must drop `code.limits` with it (below); a node still serving Gitlet until its restart holds pushes to the last hosting revision's limits.

- **`max_push_bytes`**: the largest pack one push may send. Unset is 1 GiB, also the maximum; a larger history is imported in parts.
- **`max_blob_bytes`**: the largest single file a push may carry. Unset is 50 MiB. It sizes git's memory on a serving node, so a larger binary belongs in [Worklet's project files](../concepts/object-store.md).
- **`repo_max_bytes`**: the most packed bytes one repository may hold. Unset is 20 GiB. It is per **node** as much as per repository, since every serving node keeps a whole copy. It counts packs as pushes stored them, so a history [imported](../reference/cli.md#crewlet-gitlet-import) in steps counts for more (about twice as much in eleven steps; [Limits](../concepts/gitlet.md#limits)). A seat outside the owning unit is held to half of it and half of the refs, so the owners keep the other half ([A seat's push](../concepts/gitlet.md#a-seats-push)).
- **`pushes_per_seat_day`**: pushes one **seat** may make in a day on the company's [calendar](#the-companys-clock), all repositories together, a push refused whole by a ref rule included: 1 to 100 000; unset is 1 000. It bounds a seat pushing in a loop. A push past it is refused `push_quota` and recorded nowhere (an attempt past it is refused likewise, unrecorded), its sentence saying when the next company day begins and the count with it. A person's pushes are never counted. A seat pushing to several repositories at one instant may land past it by its pushes in flight, less one (each repository's committer counts its own node's), and never again that day ([A seat's push](../concepts/gitlet.md#a-seats-push)).

The order is checked on what a push is actually held to, defaults included: `max_push_bytes: 10485760` alone is refused because the default `max_blob_bytes` (50 MiB) is larger; set both. `code.limits` on a company not setting `code.backend: gitlet` is refused.

`code.repos` declares the company's repositories. **Every data node reconciles it at every boot and activation**; an unfinished reconcile is rerun on a backoff from five seconds to five minutes until it applies. A name added is created, a changed entry's settings are settled, and a repository the configuration created that the document no longer declares is **deleted**: it takes no push, its refs and packs are kept, and declaring it again restores it. A repository somebody else created is never deleted for being absent here, and nothing is deleted because Gitlet was turned off. See [Gitlet](../concepts/gitlet.md#declaring-repositories) for each change and the activation fence.

- **`name`**: the clone address, so renaming deletes and creates another. One host-name label: a name with a dot (`<handle>.<slug>`) is a seat's scratch repository, made only by that seat's tool, and is refused here. A second declaration is refused at the later entry.
- **`owner`**: exactly one of `unit` (its key, or a name only one unit carries) and `seat` (a handle), resolved against the chart; one the chart lacks refuses the revision, naming those it has.
- **`visibility`**: which **seats** may read it. `company` (unset): every seat. `unit`: the owning unit's members and the leads above them, or the owning seat and the leads above its unit; other seats see it absent and their reads are refused. A person with the [Gitlet permission](../concepts/identity-and-access.md#access-admin-and-one-permission-per-app) (`gitlet`) reads every repository whatever this says, one without reads none. Any other value refuses the revision. `crewlet validate` **warns** about a `unit` repository whose owning unit has no lead, own or inherited, at its `visibility` (see [Visibility](../concepts/gitlet.md#visibility)).
- **`search`**: `indexed` (unset): from an index of its default branch, read by a company-wide search or one naming it. `grep`: git grep over a copy, only by a search naming it. An index costs each Gitlet node keeping one disk and a build at every new default-branch tip, so `grep` suits repositories nobody searches by content (generated, vendored). A tool-made repository starts at `grep`, a seat's scratch repository is always `grep` ([Repositories made by tools](../concepts/gitlet.md#repositories-made-by-tools)). Any other value refuses the revision. The first two Gitlet nodes of an `indexed` repository's order each keep its index ([Code search](../concepts/gitlet.md#code-search)).
- **`default_branch`**: any branch name git takes except `seats` and `seats/<one name>` (the seats' namespaces, which no push may create); unset is `main`.
- **`protect`**: each rule names exactly one of a `branch` and a `tag` pattern, each pattern once. A pattern is a name in git's grammar: a component `*` matches one component, `*` inside a component any run but `/`, a component `**` any number of components (one or more as the last); no `*` matches **only itself**; at most eight components counting `refs/` and its namespace. Every rule matching a ref holds, so `allow_force` and `allow_delete` are allowed only where every matching rule allows them, and `direct_push: none` on any freezes the ref. `direct_push: persons` (the default) lets a person push and **never a seat**, a unit's lead included: a seat's work reaches a protected ref when a person pushes it ([A seat's push](../concepts/gitlet.md#a-seats-push)). `allow_force` on a tag rule is refused. A ref no rule matches may be force-pushed or deleted, so `crewlet validate` **warns** (never fails) about each repository whose default branch no rule protects, at its `protect`, naming the rule to add, except an archived one.
  A `branch` rule may carry a **merge gate**; setting any of these forms one, the rest defaulting:
  - `merge_by`: `writers` (unset: anybody who may write), `maintainers`, or `persons` (never a seat).
  - `approvals`: `none` (unset) or `one` counted approval.
  - `approvals_unless`: the [path classes](../concepts/gitlet.md#change-classes) `whitespace_only` and `comment_only` that exempt a change from approvals when every path it touches is of one.
  - `human_paths`: at most 32 `{glob, unless}`; a change touching a matching path waits for a **person's** approval, unless every path it matched is of a class `unless` lists. Globs are written as a path owner's paths are.
  - `checks`: at most 32 names the merge waits for: a lowercase letter or digit, then up to 63 more of them, `.`, `_` and `-`.
  - `landing`: the branch's merge queue (which tests each change before it lands): `depth` 1-8 candidates at once, `batch` 1-8 changes a candidate; 4 and 2 unset.
  - `owners_approve`: one counted approval from somebody eligible for every [path owner](../concepts/gitlet.md#path-owners)'s glob a change touches.
  - `delete_source`: `seats` (unset): a merge deletes a source branch in a seat's own namespace; or `none`.
  - `agents_edit_checks`: a seat's approval stands for a change to `.crewlet/**`, which on a rule requiring checks otherwise waits for a person.

  Each check, glob and class is listed once; `no_code_change` is refused in an exempting list (approval already carries across a revision that changed nothing). A gate on a `tag` rule is refused at its first gate field, since no merge reaches a tag, and so is one weighing more than 8 KiB as the code log records it. `crewlet validate` **warns** (never fails) about a gate field that does nothing: `owners_approve` on a repository naming no `path_owners`, `landing` on a rule requiring no `checks` (only a branch whose merges wait for checks has a queue), `approvals_unless` beside `approvals: none`; and, at a `landing`, a queue that cannot land one change a day for every seat that may merge: its `depth` x `batch` changes a round, a round as long as `code.checks.max_timeout_seconds`, against every human seat and every `full_access` agent seat writing the repository under the stricter of `merge_by` and `code.merge_by_floor` ([How fast a queue lands](../concepts/gitlet.md#how-fast-a-queue-lands)). A gate is shown on a detailed read (`GET /gitlet/repos/{name}`); a change into a branch whose gate requires a check is queued, and the repository's home Gitlet node lands the queue (see [Merge gates](../concepts/gitlet.md#merge-gates)).
- **`path_owners`**: at most 64 entries, each naming exactly one of `unit` (its key, or a name only one unit carries; its [effective lead](../concepts/organization-model.md#lead-inheritance) is asked) and `seat` (a handle), resolved as `owner` is, and 1 to 32 `paths`. A path is a glob over the repository's tree from its root, at most 128 bytes: `*` component matches one component, `*` inside a component any run but `/`, `**` any number of components (one or more as the last); `docs/**` is everything inside `docs`, `*.go` only root Go files, `**/*.go` every one; a glob with `*` has at most eight components, one without matches **only** that path. `*` is the one metacharacter: `?`, `[`, `\` and a leading `!` are refused. Each glob is listed once per entry and each owner in one entry (a unit by name and by key is one owner); the whole weighs at most 256 KiB as the code log records it (only maximal counts with long globs, or globs with `&`, `<` and `>`, which JSON spells as six bytes each, reach that). A path owner is **whom a change touching its paths asks for a review, and nothing more**; it reads and pushes by its grant. See [Path owners](../concepts/gitlet.md#path-owners).
- **`mirror_from`** `{url, credential}`: a **pull mirror** of a repository at another host. It is imported first (at create, when an existing repository is given a `mirror_from`, or when its `url` changes), then moves only by Gitlet's syncs; every push and change of its own is refused `repo_mirrored`. A `protect` list beside it is warned about; `archived` beside it is refused.
- **`mirror_to`** `{url, credential, as, refs}`: a **push mirror**: Gitlet pushes the refs to a repository at another host as the seat `as` names, with that seat's own identity, never a bot. `credential` is required, and `as` must be a seat of the chart that reads this repository (an agent's seat what its grant reads, a person's every one while the person uses Gitlet), or the revision is refused. A configuration reads no identity directory, so a person's seat is judged on the chart and its holder's Gitlet use is judged before every push: a push refused because they do not (nobody holds the seat, the person may not act, or they lack the `gitlet` permission) fails the mirror until an administrator gives them Gitlet or `as` names another seat; a mirror not level for half an hour raises `code_mirror_failing` ([Mirrors and imports](../concepts/gitlet.md#mirrors-and-imports)). `refs`: at most 64 patterns of `refs/heads/<glob>` and `refs/tags/<glob>`, each once, none reaching `refs/heads/seats/` (`refs/heads/**` among them); unset is the default branch and `refs/tags/*`.
- **`import`** `{url, credential}`: fills the repository at create from another host. It is `importing` until done and takes no push, change, archive or purge meanwhile, each refused `repo_importing`. Only the create reads it: one declared for an existing repository fetches nothing, which the reconcile logs once a run (`code_import_ignored`). Beside `mirror_from` or `archived` it is refused.

  Each `url` is `https` with no user, query or fragment, at most 1 024 bytes. Each `credential` is **one whole `${VAR}` reference** of at most 128 bytes (a literal token, or a reference with anything around it, refuses the revision, since the code log records it as written on every node and backup) naming a variable [`code.remote_credentials`](#coderemote_credentials) declares for the `url`'s host, or the revision is refused. Gitlet resolves it, from the [secret store](../concepts/secret-store.md) then the environment, only when it dials; an unset `credential` fetches anonymously. Only a person or this document sets a mirror or an import: a seat's tool asking is refused `not_permitted`, whatever its grant. A scratch repository takes none. The home Gitlet node runs the import, a pull mirror's sync every 15 minutes, and a push mirror's push whenever the repository moves (at most once a minute) and every 15 minutes while it does not, each protected branch pushed at its green commit and held back while it has none, as the seat `as` names with its own token at the other host. The credential is sent as HTTP Basic: `user:password` where it holds a colon, otherwise a token beside the user `x-access-token`. See [Declaring repositories](../concepts/gitlet.md#declaring-repositories).

`code.deleted_retention_days`: how long a deleted repository stays restorable, 1 to 365 days (an [admission rule](../concepts/configuration.md#what-a-stored-revision-is-held-to), like the 10 000 repositories `code.repos` may list, so a revision already stored past it still applies); unset or `0` is 7, the trim's default `min_age`. Past it, the `code-retention` duty purges the repository in bounded steps and its name is free again. A longer retention set before then saves it (a purge begins only on a node that has applied the fleet's revision), and a begun purge is finished whatever the retention says (see [Deletion and purge](../concepts/gitlet.md#deletion-and-purge)). Like `code.limits`, `code.repos` and `code.deleted_retention_days` are refused on a company not setting `code.backend: gitlet`, and a node still serving Gitlet after a revision turned it off keeps the last hosting retention until restart.

`code.scratch` is the policy for repositories **seats make for themselves**. A person, a unit's lead and a seat can create, rename, archive, delete and restore repositories through their tools ([Repositories made by tools](../concepts/gitlet.md#repositories-made-by-tools)), and a seat at `full_access` can make a **scratch** repository named `<handle>.<slug>`, which nobody else's tool creates and which takes no protection. Both fields are read off the **current** revision:

- **`idle_days`**: days a scratch repository may go with no push, rename, settings change or restore before the `code-retention` duty deletes it, restorable for `deleted_retention_days`: 1 to 365; unset is 7. Reading is no activity. One a **live turn** has named (through its tools, workers or executor's bridge; running or parked on its executor) is never deleted while the turn lasts, and keeping it moves no clock: after the turn ends it is deleted at the duty's next pass if still idle. A day at least, because a turn records its hold on a repository idle for 22 hours, and two nodes' clocks may differ by two hours.
- **`max_per_seat`**: scratch repositories one seat may hold: 0 to 1 000; unset is 50. **Every** one the seat owns counts until purged, a deleted one included; 0 lets no seat create one. A seat at the limit is refused `scratch_limit` naming it.

The 10 000 repositories `code.repos` may list is also what a tool's create of a **project** repository is held to: all project repositories Gitlet holds, declared and tool-made, short of purged; a create at it is refused `repo_limit`. Scratch repositories never count there. Like the rest of the block, `code.scratch` is refused on a company not setting `code.backend: gitlet`.

`code.checks` is how the company runs the [checks](../concepts/gitlet.md#checks) its repositories define in `.crewlet/checks.yaml`: the file names what to run, this block with which secrets and for how long at most. A check runs in a [box](../concepts/code-sandbox.md#check-boxes) with no harness: a container made from `providers.sandbox.local.image` (one holding git 2.40 or newer and what the checks build with) on the Gitlet node that claims it, so a company that sets no image runs no check, each ending `errored` `defs_invalid`. Every field is read off the **current** revision when a box is made, and the block is refused on a company not setting `code.backend: gitlet`:

- **`env`**: the secrets a check may be given, variable names to values: a check gets exactly the entries its file names in its `env`, since each is a credential the change's own code reads. A key is a variable name (a letter or underscore, then letters, digits and underscores, at most 128 bytes) and none of the names the engine sets in a box (`CREWLET_GIT_*`, `CREWLET_CHECK_*`), refused naming the variable. A value takes `${VAR}`, resolved when the box is made; a config read shows it as `__redacted__` ([Configuration](../concepts/configuration.md)).
- **`max_timeout_seconds`**: the longest any check runs, whatever its file's `timeout_seconds` says: 60 to 21 600 (six hours); unset is 3 600, an hour: GitLab CI's and Azure Pipelines' default job timeout, so a pipeline moved here keeps its default, and the longest one hung check holds one of its Gitlet node's `gitlet.max_checks` slots. A company whose checks take longer raises it. It is also the round `crewlet validate` prices a merge queue's capacity at, so a value far above what checks take reads as a slower queue than there is.

`code.identity_domain` is the domain of the addresses on commits the engine builds through a tool ([Engine-built commits](../concepts/gitlet.md#engine-built-commits)): the committer's `Crewlet <engine@<domain>>`, which marks host-built commits in every clone's history and which a [coding box](../concepts/gitlet.md#coding-boxes)'s push may carry only on a commit the repository already holds; and the author's `<handle>@<domain>`, for a seat whose role states no `email` (one `@` between local part and domain, at most 254 bytes). A `${VAR}` reference is never resolved for a commit: it would be read from whichever node built it, and an address kept out of the document would be written into history for good. Unset is `seats.invalid`, under the `.invalid` domain RFC 2606 reserves, so no address the engine writes belongs to anybody who could receive mail at it. It is a lowercase DNS name (labels of letters, digits and hyphens, none beginning or ending with a hyphen, joined by dots) of at most 189 bytes, so every address in it, a 64-byte handle's included, fits the 254 an address may be. It is read at every commit, so a change applies at activation, and a commit keeps the domain it was built with. Refused on a company not setting `code.backend: gitlet`.

`code.merge_by_floor` is the least authority a merge into **any** protected branch needs: `writers` (unset; anybody who may write), `maintainers`, or `persons` (never a seat). A merge is held to the stricter of the floor and the `merge_by` of every rule matching its branch (no merge gate reads as `writers`), so a company wanting a person behind every merge says so once here. Read off the **current** revision at every decision, refused on a company not setting `code.backend: gitlet`, and any other value is refused naming the three (see [Merge gates](../concepts/gitlet.md#merge-gates)).

### `code.remote_credentials`

`code.remote_credentials` names the variables a repository's mirror or import may be dialed with, each with the hosts its value may be sent to, keyed by the variable's name as a `credential` writes it inside `${…}`:

```yaml
code:
  remote_credentials:
    GITHUB_TOKEN: { hosts: [github.com] }
    GITLAB_SELF_TOKEN: { hosts: ["gitlab.example.com", "gitlab.example.com:8443"] }
```

**Every remote's credential must be one this table declares for the remote's host**:

- a mirror or import in `code.repos` is refused when the revision is written;
- one a person asks for with [`crewlet gitlet import`](../reference/cli.md#crewlet-gitlet-import), `code_import_repo` or a repository's settings is refused `not_permitted` by the deciding data node;
- every dial resolves no other.

The value is resolved where the repository's home Gitlet node dials, from the [secret store](../concepts/secret-store.md) and then that node's environment, and sent to the remote's host. Without this table, anyone who may create a repository could name any variable the node resolves (`CREWLET_SECRET_KEY_*`, a Tier A token, the object store's key) and have its value sent to a host of their choosing. With it, an administrator decides which token goes to which forge. The table holds names and hosts, never a value, so rotating a token changes nothing here.

- A key is a variable's name: a letter or underscore, then letters, digits and underscores, at most 125 bytes, so `${NAME}` fits a credential's 128.
- **`hosts`**: at least one, each a host name or an IP address in its one written form, with an optional `:port` and nothing else (no scheme, user or path). A host is matched **whole**, port included (443 where none is written): a variable declared for `github.com` is never sent to `api.github.com`, `github.com.example.net` or another port. Each host is listed once.
- At most 64 variables of at most 16 hosts each. These are [admission rules](../concepts/configuration.md#what-a-stored-revision-is-held-to), so a revision already stored past either still applies.

It is read off the **current** revision at every decision and dial: a declaration withdrawn or narrowed to other hosts stops the value leaving at the mirror's next attempt, which fails at its dial, and the [`code_mirror_failing`](../reference/alarms.md) alarm says so once overdue. A repository whose mirror names a credential declared nowhere stays as it is; declare the variable for its host, or change the mirror. Like the rest of the block, it is refused on a company that does not set `code.backend: gitlet`.

---

## MCP Servers

**All** MCP tool servers are declared here, including the Jira/Confluence (`atlassian`), Slack and GitHub servers. A `shared: true` server runs once for the agents it is granted to; a `shared: false` server is a per-role template, started for each agent it is granted to with that agent's own values from `role.mcp_env[name]` (env vars for `stdio`, HTTP headers for `http`). Who gets a server is its `access` (absent, every agent seat) or, for one that reads each agent's own vendor credential, the integration's lane; an `mcp_env` block grants nothing (see [Tools & MCP](../guides/tools-and-mcp.md#choosing-which-agents-get-a-server)).

```yaml
mcp_servers:
  # shared stdio server (one instance for all agents)
  - name: tavily
    description: Searches and reads the public web   # optional, one line beside its name in each agent's prompt
    command: npm
    args: ["exec", "--yes", "--", "tavily-mcp@0.2.22"]
    env:
      TAVILY_API_KEY: "${TAVILY_API_KEY}"
    access:                             # optional, absent = every agent seat
      units: [research]                 # every agent seat in this unit and the units beneath it
      seats: [pm]                       # and this agent seat
      except: [research-intern]         # but not this one, though its unit is named
    tool_annotations:                   # it advertises none; every tool only reads the web
      tavily_search:   { read_only: true, open_world: true }
      tavily_extract:  { read_only: true, open_world: true }
      tavily_crawl:    { read_only: true, open_world: true }
      tavily_map:      { read_only: true, open_world: true }
      tavily_research: { read_only: true, open_world: true }

  # per-role stdio server — Jira + Confluence share one mcp-atlassian
  - name: atlassian
    shared: false                       # per-role, for each agent on at Jira or Confluence; token from role.mcp_env.atlassian
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
    enabled: true                       # optional, default true; false keeps the entry and starts nothing
    tool_prefix: ""                     # optional — prefix tool names
    tool_annotations: {}                # optional — behavioural-hint overrides (see Tool Capabilities)
    tools:                              # optional: the level each tool runs at
      default: allow                    #   allow (the default) or blocked, for every tool levels does not name
      levels: {}                        #   one tool by the server's own name: allow or blocked
    startup_timeout_seconds: 120        # optional — connect + handshake + discovery
    request_timeout_seconds: 300        # optional — one tool call

  # shared http server the company signs in to with OAuth, once, from its page
  - name: linear
    transport: http
    url: "https://mcp.linear.app/mcp"
    oauth:
      client: cimd                      # cimd | dcr | own (own takes client_id and client_secret)
```

Fields:

- `name`: required; lowercase letters, digits and hyphens, at most 32 characters (see [Tools & MCP](../guides/tools-and-mcp.md#naming-a-server-and-its-tools)). `crewlet` is reserved: it is the name every harness sees the seat's own tools under, over the run's session on this host and over the [tool bridge](../concepts/llm-backends.md#the-tool-bridge) in an executor's box, and a server of that name would be replaced there. So is `container`, Claude Code's name for its own container's tools, whose results it clears from a long session, so a continuing run would lose them. Both are refused at apply too. So is `browser`, the name a browser seat's box sees [its own browser](../concepts/code-sandbox.md#a-seats-browser) under, which takes the place of a server of that name there; it is an [admission rule](../concepts/configuration.md#what-a-stored-revision-is-held-to), so a revision stored before it still applies.
- `enabled`: default `true`; `false` turns the server off and keeps the entry and every `mcp_env` block and box listing that names it (see [Tools & MCP](../guides/tools-and-mcp.md#turning-a-server-off)).
- `description`: one line of at most 200 characters, no line breaks, listed beside the name in each agent's prompt (see [Tools & MCP](../guides/tools-and-mcp.md#saying-what-a-server-is-for)).
- `transport`: `stdio`/`http`.
- `shared`: default `true`.
- `access`: which agent seats get the server: `units` (each reaching every agent seat beneath it), `seats` and `except`, by unit key and seat handle. Absent, every agent seat. Refused on a server that reads each agent's own vendor credential, which the integration's lane grants (see [Tools & MCP](../guides/tools-and-mcp.md#choosing-which-agents-get-a-server)).
- `command`/`args`/`env`: stdio; each key an environment variable name.
- `url`/`headers`: http; `https`, or plain `http` to this host or a private network, never with a user name or password in it; each key a header name.
- `optional_keys`: the keys of `env`, `headers` or a seat's `mcp_env` block whose value may resolve to nothing; such a key is left out rather than holding the server back. Every other `${VAR}` must resolve before the server starts (see [Tools & MCP](../guides/tools-and-mcp.md#a-key-that-may-go-without-its-value)).
- `tool_prefix`: letters, digits, `_` and `-`, at most 49 characters, put in front of every tool the server serves; a tool whose prefixed name exceeds 50 characters is not offered (see [Tools & MCP](../guides/tools-and-mcp.md#a-tools-name)).
- `tool_annotations`.
- `tools`: the level each tool runs at. `default` covers every tool `levels` does not name; `levels` names one tool by the server's own name; each is `allow`, `approve` or `blocked`. With neither, a tool the server marks read-only runs at `allow` and every other at `approve`. A call at `approve` waits for a person's answer; a blocked tool is never offered to an agent; a server whose every tool is not at `allow` by `default: allow` is given to no executor box (see [Tools & MCP](../guides/tools-and-mcp.md#setting-each-tools-level)).
- `startup_timeout_seconds`, `request_timeout_seconds`: see [Timeouts](#timeouts).
- `seat_credentials`: which agent credential a per-seat server reads, under which variable or header: `{kind, field, key, as, prefix}`. A server the engine recognises needs none (see [Tools & MCP](../guides/tools-and-mcp.md)).
- `oauth`: signs the company in to a shared remote server (see [Tools & MCP](../guides/tools-and-mcp.md#signing-in-with-oauth)).
  - `grant`: `authorization_code` (default; a person signs in once, from its page) or `client_credentials` (a service identity the engine signs in as, with no person; see [Tools & MCP](../guides/tools-and-mcp.md#signing-in-as-a-service-identity)).
  - `client`: `cimd`, `dcr` or `own` for a person's sign-in; refused for a service identity.
  - `client_id` and `client_secret` (a `${VAR}`): the company's own client's; both required for `client_credentials`. `client_id` is at most 2,048 bytes, what the grant a sign-in makes keeps.
  - `scopes`: what to ask for: at most 50, each one scope token of at most 256 bytes, what the grant keeps. A sign-in whose authorization server gives a longer client id, issuer, resource or scope to ask for is refused before anybody is sent to consent; a scope it grants unasked past 256 bytes is left out of the grant's record.
  - Refused on a stdio or per-agent server, beside an `Authorization` header and in a box's `sandbox.mcp.servers`.

### Timeouts

A silent MCP server (one that never completes the handshake, or answers discovery and never returns from a call) raises no error. The engine starts servers on the seat-acquisition path, so a silent one holds up every seat behind it for the life of the process. Both deadlines therefore always apply.

- **`startup_timeout_seconds`** (default `120`) bounds launching the process (or opening the HTTP session) plus the handshake, and separately the first `tools/list`, so a start can take up to twice it. The default suits a `uvx`/`npx` server whose package is not yet cached, the slow case for a *healthy* server. Lower it for a server launched from a local checkout.
- **`request_timeout_seconds`** (default `300`) bounds one tool call, one ceiling whatever the transport. Raise it for tools that genuinely run long; lower it for one that should answer quickly, so a wedged call reaches the agent as a failed result instead of a turn that never ends. It also sets how long an executor's box waits: the harness in the box waits as long as the bridge can take to answer, which counts the longest `request_timeout_seconds` among the seat's servers ([The tool
  bridge](../concepts/code-sandbox.md#the-tool-bridge--a-seats-own-tools-from-inside-a-box)).

A server that exceeds either deadline is logged and skipped; the rest of the company still starts.

### MCP registry

`mcp_registry` names the community registry Settings › MCP & tools searches beside the [curated catalogue](../guides/tools-and-mcp.md#the-curated-catalogue-and-a-community-registry) when somebody adds a server. Absent, there is no community search.

```yaml
mcp_registry:
  url: https://registry.modelcontextprotocol.io   # /v0.1/servers is appended
```

| Field | Type | Required | Description |
|---|---|---|---|
| `url` | string | no | Address of a registry speaking the MCP Registry v0.1 API; `/v0.1/servers` is appended. Must be `https` with no credentials, query or fragment (a private host included), because its answers are drafted into launch lines. A result is only a draft for the add flow, marked not reviewed; the engine dials nothing a listing names |

---

## Environment Variable References

String values support `${ENV_VAR}` syntax, keeping secrets out of config files. An unanswered reference resolves to the empty string. The tiers resolve references differently:

- **Tier B (the company)** keeps every reference verbatim (in the store, a backup, `GET /config`) and resolves it where the provider, transport or integration using it is constructed: from the [secret store](../concepts/secret-store.md) first (an encrypted table the provisioning CLIs can write into directly; inert until you store something), then the environment. It validates what is **written**, not what a reference resolves to, so a field with a closed set or pattern (an enum, a seat handle, a unit id) takes a reference only where its own entry says so. Three fields resolve only when the **whole** value is one reference: a seat's Slack `bot_token`, its Mattermost `bot_token` and its Mattermost `username`. Their transports take anything else literally, so write one `${VAR}` or the value itself, never a reference inside other text (`bot-${SUFFIX}`), which validation refuses. A Mattermost `username` written as a whole reference is the one patterned field that takes one.
- **Tier A (`crewlet.yaml`)** resolves from the **environment only** (it holds the keys to the secret store, so it can never read from it), at startup, **before** the file is decoded. The result depends on the field:
  - **Text field**: the reference is substituted whole or embedded (`edge-${ZONE}`) and the result is that text exactly, so it works in any text field, one with a pattern or closed set included (`node.id: "${HOSTNAME}"`, `logging.level: "${LOG_LEVEL}"`), which is then judged on the resolved value. A reference nothing answers leaves the text empty, which reads as unset: the default where there is one (`stream.type` and `coordination.type` fall back to `embedded` and `local`), a refusal where a value is required. The exception is `logging.file.path`, where empty means "write no file", so an unanswered path variable stops the boot instead of silently running with no durable log.
  - **Number or boolean field** (a port, replica count, anything `*_seconds`, `*_bytes` or `*_hours`, a count, a switch): the reference must be the **whole** value, read exactly as the same characters written there, trimmed of surrounding space as every Tier A string is. `api.port: "${API_PORT}"` with `API_PORT=8080` is `api.port: 8080` (also when `API_PORT` is read from a file ending in a newline), and `stream.debug: "${DEBUG}"` with `DEBUG=true` is `stream.debug: true` (a switch also takes `yes`, `no`, `on` and `off`). Two shapes are refused by name: a reference with other text around it (`"80${N}"`), and one resolving to nothing, since a number silently falling back to its default would run with a value nobody chose. `crewlet schema bootstrap` encodes exactly that.

**A number is the number it spells, in both tiers, however it arrives.** A fraction in a field that counts whole units (a port, a replica count, a round cap) is refused naming the field rather than truncated (`api.port: 8080.9`), and so is a number that is not finite: YAML's `.nan` and `.inf`, written out or resolved from a Tier A reference.

Only the braced identifier form is substituted: `${NAME}` where `NAME` matches `[A-Za-z_][A-Za-z0-9_]*`. Bare `$NAME` and shell parameter expansions (`${1:-x}`, `${line#host=}`) pass through untouched. A sandbox setup step's commands and file bodies are not substituted: the engine resolves only a file whose whole body is one `${VAR}`, and the step's `env`, and hands everything else to the box as written, where its own shell expands from the run environment ([setup steps](../concepts/code-sandbox.md#setup-steps--provisioning-the-box)).

```yaml
providers:
  llm:
    default:
      harness: opencode
      model: "${LLM_MODEL}"                          # resolved where the entry is built
      env:
        OPENROUTER_API_KEY: "${OPENROUTER_API_KEY}"  # the secret store first, then the environment
```

See [Environment Variables](../reference/environment-variables.md) for a full list.

---

## Full Example

See the [quickstart](quickstart.md) for a complete working config.
