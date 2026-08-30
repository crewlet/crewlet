# Configuration Reference

Crewlet companies are defined across **two tiers** — see the [Configuration concept page](../concepts/configuration.md) for the full design.

- **Tier A** (`crewlet.yaml`, restart-only): this node's identity and roles, the store file, the stream and coordination slots, API host/port/auth, the secret keyring, logging.
- **Tier B** (`company.yaml` imported into the store, live-editable): everything else — identity, providers, integrations, MCP servers, roles, units, turn engine, learning, budgets, scheduling.

This page documents the **Tier B** fields below.  For Tier A see [Configuration concept page §"Tier A example"](../concepts/configuration.md#tier-a-example-crewletyaml).

> **Machine-readable version.** `crewlet schema company` emits the JSON
> Schema for everything on this page, generated from the models
> themselves. Point your editor at it for autocomplete and typo
> squiggles, or hand it to an AI assistant — see
> [Authoring with an AI assistant](ai-authoring.md). Check your file with
> `crewlet validate <file>` (add `-json` for machine-readable errors);
> it reads no environment, so it works before any secret is exported.
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
token_budget: 1000000                   # optional — org-wide token limit (0 = unlimited)
notification_rate_limit: 10             # optional — max notifications/sec per agent (0 = unlimited;
                                        #   drop-based safety valve against webhook storms — burst
                                        #   handling is inbox coalescing below, which batches instead
                                        #   of dropping)
notification_coalesce_window_seconds: 0 # optional — inbox linger window (seconds) absorbing bursts
                                        #   before an idle agent's turn starts. 0 (default) adds no
                                        #   latency: backlog that piled up while the agent was BUSY
                                        #   still coalesces into one digest turn per conversation.
                                        #   Max 60 — the window counts against the broker ack-timeout
                                        #   budget bounding a message's unacked lifetime.
                                        #   See concepts/event-system.md § Inbox batching.
notification_coalesce_max_batch: 20     # optional — max events merged into one digest trigger

policies:                               # optional — org-wide policies (full text renders into the Plan prompt)
  - "All code must be reviewed before merging"
  - "Communicate decisions in writing"

roles: [...]                            # optional — root-level org-wide agents (see below)
units: [...]                            # optional — org structure tree (see below)

turn_engine:                            # optional — Plan/Execute/Review turn config
  max_iterations: 3                     # hard cap on self_iterate loops per turn
  max_tool_rounds: 20                   # max tool-call rounds within a single Execute-phase run
  plan_max_tool_rounds: 16              # max tool-call rounds within a single Plan-phase run
  onboarding_max_tool_rounds: 10        # dedicated first-turn onboarding pass before Plan (0 = disabled)
  subagent_max_turns: 20                # maximum tool rounds per spawn_subagent call
  subagent_timeout_seconds: 120         # wall-clock timeout per sub-agent
  subagent_budget_fraction: 0.2         # fraction of parent's remaining tokens a sub-agent may consume
                                        #   (for a batched call, the TOTAL slice shared across children)
  subagent_max_parallel: 3              # max children a batched spawn_subagent runs concurrently
  subagent_batch_timeout_seconds: 120   # aggregate wall-clock timeout for one batched spawn_subagent call
  subagent_min_per_child_tokens: 500    # floor on the per-child token slice; batch rejected if undercut
  executor_always_on_tools: []          # extra tools always exposed in the Execute phase
                                        #   (load_tool_skill is always-on independently)
  delegation_depth_limit: 3             # max colleague-handoff chain depth before depth_cap guard breach
  extension_enabled: true               # round-cap extension judge (Plan + Execute + onboarding)
  plan_max_tool_rounds_ceiling: 32      # hard ceiling for Plan rounds across extensions (2x base 16)
  execute_max_tool_rounds_ceiling: 40   # hard ceiling for Execute rounds across extensions (2x base 20)
  onboarding_max_tool_rounds_ceiling: 20  # hard ceiling for onboarding rounds across extensions (2x base 10)
  extension_round_step: 8               # max rounds the judge may grant per extension call
  conversation_session:                 # what this seat already said in ONE thread / issue / PR,
                                        #   carried into that conversation's next turn
                                        #   (see concepts/conversation-sessions.md)
    enabled: true                       # feature gate — a live kill switch; off restores the
                                        #   pre-ledger prompt exactly
    max_entries: 20                     # entries KEPT per conversation, trimmed at write time
    injected_max_entries: 5             # how many reach the prompt: the newest N, rendered oldest-first
    injected_max_chars: 6000            # byte budget for the block; oldest entries drop first
    retention_days: 30                  # matches the event store's horizon; applied at next start

learning:                               # optional — agent-learning subsystem
  enabled: true                         # master switch (auto-disables without DB + embeddings)
  episodic:
    retrieval_limit: 5                  # default hits query_episodes returns when the
                                        # model names no limit (1-20)
  reflect:
    enabled: true                       # ReflectEngine + reflect_and_persist tool
    persist_decider: true               # run the post-turn PersistDecider on every turn
    budget_tokens: 5000                 # soft cap on the decider's LLM call (0 disables)
    summarize_episodes: true            # cheap-model summarisation of query_episodes hits
    summarize_max_tokens: 400           # soft cap on summariser response length
  counterparty:
    enabled: true                       # CounterpartyProfiler + auto-inject + lookup inline
    budget_tokens: 3000                 # soft cap on the profiler's LLM call per turn
  skill_synthesis:
    enabled: true                       # SkillSynthesizer (single-turn + clustered)
    min_tool_calls: 5                   # single-turn trigger threshold
    budget_tokens: 4000                 # soft cap on the synthesizer's LLM call
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
    budget_tokens: 3000                 # soft cap on the refiner's LLM call
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
    budget_tokens: 4000                 # soft cap on the promotion LLM call
  personal_memory:
    max_refreshes_per_turn: 3           # cap on distinct context_hint values per turn
                                        # (idempotent repeats of the same hint are free)
  episode_lifecycle:
    # Trigger: per-agent raw count + amortised count(*) check on write.
    max_raw_episodes_per_agent: 500     # threshold that fires CompactionRequested
    write_check_every_n: 10             # only run the count(*) on every Nth write
    # Action 1: drop non-terminal mid-state rows past this age.
    non_terminal_max_age_days: 14
    # Action 2: drop skill-consolidated rows past this grace.
    consolidated_grace_days: 30
    # Action 3: cluster + LLM-compact remaining raw rows past this age.
    compaction_min_age_days: 30
    compaction_min_cluster_size: 3      # singletons / pairs left raw
    compaction_jaccard_threshold: 0.6   # tool-sequence similarity to pool a cluster
    compaction_batch_size: 200          # max raw rows pulled per pass
    compaction_budget_tokens: 4000      # soft cap on the compactor LLM call
    exemplar_count: 2                   # raw rows kept per cluster for drill-down
    # Action 4 (optional): evict ancient compacted entries for hard storage caps.
    compacted_max_age_days: 0           # 0 = disabled
```

Per-role auxiliary model (for reflection + episode summarisation) and
extension-judge model:

```yaml
roles:
  - name: Engineer
    llm: claude-sonnet                  # main model for plan/execute/review
    llm_auxiliary: gpt-4o-mini          # cheap/fast model for reflection
    llm_judge: gpt-4o-mini              # cheap/fast model for the round-cap extension judge
```

`llm_judge` is invoked when Plan or Execute exhausts its tool-round cap;
it decides whether the agent is making progress (extend) or thrashing
(fall through to rescue).  Falls back to `llm` -> `"default"` if unset.
See the [Turn Engine extension judge](../concepts/turn-engine.md#round-cap-extension-judge)
section for details.

See the [Turn Engine](../concepts/turn-engine.md) and [Agent Learning](../concepts/agent-learning.md) docs for what each field controls.

### Scheduling

System-level knobs for the [Scheduler](../concepts/scheduling.md) — the
cron analogue that fires role/unit `schedules:`. The scheduler
auto-enables when `enabled` is true, a database is configured, and the
org declares at least one schedule.

```yaml
scheduling:                              # optional — role/unit scheduled work
  enabled: true                          # master switch
  tick_seconds: 10                       # scheduler poll interval
  default_timezone: UTC                  # used by any Schedule without its own timezone
  jitter_seconds: 0                      # max per-schedule spread to smooth a shared cron minute
  catchup_min_seconds: 120               # lower clamp on the missed-tick catchup window
  catchup_max_seconds: 7200              # upper clamp on the missed-tick catchup window
```

See the [Scheduling](../concepts/scheduling.md) concept doc for delivery
modes (`each` / `lead`), at-most-once semantics, catchup, and the
per-task wall-clock timeout.

---

## Providers

```yaml
providers:
  llm:
    default:                            # named provider (referenced by roles via `llm: default`)
      type: openai                      # openai | anthropic | openai-compatible | cli-agent
      model: gpt-4o
      api_keys:                         # one or more keys; multiple enables rate-limit rotation
        - "${LLM_API_KEY}"              # supports ${ENV_VAR} references
        # - "${LLM_API_KEY_BACKUP}"     # add more for rate-limit rotation
                                        # empty list is allowed ONLY if the conventional env var
                                        # (OPENAI_API_KEY / ANTHROPIC_API_KEY) is set — otherwise the
                                        # engine fails fast at startup instead of deep in the first turn
      cooldowns:                        # optional — TTL when a key is marked exhausted
        rate_limit_seconds: 3600        #   429 / 402 default cooldown (a Retry-After / x-ratelimit-reset
        auth_seconds: 300               #   401 / 403 default cooldown   header on the error overrides it;
                                        #   repeated auth failures on one key back off exponentially)
                                        #   a bench is SHARED across the fleet, so a peer's 429 benches the
                                        #   key here too — see concepts/coordination.md
      base_url: "${LLM_BASE_URL}"       # optional — custom endpoint; supports ${ENV_VAR} references.
                                        #   Required for openai-compatible (it has no vendor default);
                                        #   on `openai` / `anthropic` it points the vendor's own wire
                                        #   format at a gateway or proxy instead of the vendor host
      timeout_seconds: 120              # optional — per-call HTTP timeout (default: 120); raise for slow / large-output reasoning models
                                        #   (the cli-agent backend drives a subprocess and uses cli.timeout_seconds instead)
      reasoning: false                  # optional — enable reasoning/extended thinking (default: false)
      reasoning_effort: medium          # optional — OpenAI reasoning effort: low | medium | high (default: medium)
      reasoning_budget_tokens: 10000    # optional — Anthropic thinking budget in tokens (default: 10000)
    budget:                             # multiple providers supported
      type: openai
      model: gpt-4o-mini
      api_keys:
        - "${OPENAI_API_KEY}"

    subscription:                       # a coding CLI you already subscribe to,
                                        # driven headless — NO API key.
                                        # See concepts/subscription-llm-backends.md
      type: cli-agent
      model: sonnet                     # whatever the CLI's --model accepts
      cli:
        agent: claude-code              # claude-code | codex | gemini-cli | qwen-code
                                        #   | opencode | cursor-agent | copilot | grok
                                        #   | custom
        state_dir: ""                   # optional — credential dir + per-seat CLI homes.
                                        #   Empty: $CREWLET_LLM_CLI_HOME/<key>, else
                                        #   ~/.crewlet/llm-cli/<key>. Use a persistent
                                        #   volume in an ephemeral container.
                                        #   Entries naming the SAME dir share one login
                                        #   (how per-phase models run off one
                                        #   `crewlet llm login`); they must then also
                                        #   share the same `agent`.
        timeout_seconds: 300            # optional — one CLI invocation, wall clock.
                                        #   Separate from the entry's HTTP
                                        #   timeout_seconds: this covers process
                                        #   launch + the model call + the CLI's retries
        max_concurrent: 4               # optional — CLI processes at once. Each is a
                                        #   200-400 MB runtime, and subscription plans
                                        #   throttle concurrency hard
        env: {}                         # optional — extra child env, ${ENV_VAR}-resolved.
                                        #   The child gets an ALLOWLISTED environment,
                                        #   never the engine's, so declare anything else
                                        #   the CLI needs here
        auth:
          mode: subscription            # subscription | api-key | inherit-env
          token: ""                     # optional — ${VAR} holding a headless
                                        #   subscription token; empty falls back to the
                                        #   profile's own var (CLAUDE_CODE_OAUTH_TOKEN)
          credential_bundle: ""         # optional — ${VAR} holding a `crewlet llm export`
                                        #   blob; empty falls back to
                                        #   CREWLET_LLM_CLI_<KEY>_CREDENTIALS
        overrides: {}                   # optional — replace any profile field when a
                                        #   vendor renames a flag (validated here, so a
                                        #   typo fails `crewlet validate`)

  embeddings:                           # optional — similarity search for the
                                        #   agent-learning subsystem (agent_diary
                                        #   candidate selection AND episode recall).
                                        #   Omit it and both fall back to recency;
                                        #   nothing else changes.
    type: openai                        # openai | openai-compatible
    model: text-embedding-3-small       # required — no default, because a default
                                        #   here would be a width the store was not
                                        #   sized for
    api_key: "${OPENAI_API_KEY}"        # supports ${ENV_VAR} references; empty falls
                                        #   back to OPENAI_API_KEY, the same variable
                                        #   the chat backend reads
    base_url: "..."                     # optional — custom endpoint. This is the only
                                        #   difference between `openai` and
                                        #   `openai-compatible`, so a local embedding
                                        #   server needs nothing else
    dimensions: 1536                    # the vector width. Requested from the API
                                        #   (third-generation models truncate on
                                        #   request), and checked against what comes
                                        #   back on every call
```

**The width is a contract with the store, not with the model.** Vectors of
two different widths cannot be compared, so a row written at the wrong one is
not a degraded search — it is a row that can be written and never read back,
silently and permanently. Two checks follow from that, and both refuse rather
than adapt:

- **At the apply.** A revision whose `dimensions` differs from the width this
  store already holds is rejected, with an error naming both. Changing the
  width means re-embedding what is stored, which is a decision for an operator
  who is watching rather than a silent divergence discovered at the first
  recall weeks later.
- **On every call.** A vector that comes back at the wrong width is refused
  rather than stored — on every call and not just the first, because a
  gateway or aggregator can move models mid-deployment.

Everything else about an embedding failure is cheap: a timed-out or refused
call is *no vector*, which every caller reads as "no similarity search this
turn" and carries on with recency. Nothing here retries — the caller's
degradation costs less than a retry spent inside a Plan-phase prefetch
somebody is waiting on.

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
  store_dir: "./crewlet-data/stream"  # empty = in-memory: right for a test,
                                    #   and nothing published survives a restart
  # url: "nats://nats.internal:4222"  # required for `nats`, REFUSED for
                                    #   embedded — an embedded server has no
                                    #   address, so a url there is read by
                                    #   nobody
  # replicas: 3                     # 1 solo (the default); 3 across an EMBEDDED
                                    #   cluster, where it is what makes a publish
                                    #   quorum-durable before it returns. It is
                                    #   checked against `cluster.peers`, so >1
                                    #   without them is refused — this node has
                                    #   nothing to replicate to
  # cluster:                        # an EMBEDDED server joining its peers, which
  #   name: crewlet                 #   is the fleet topology: every node embeds
  #   port: 6222                    #   one member of one cluster. `node.id` is
  #   peers:                        #   the member name, so it must survive a
  #     - "nats://node-1:6222"      #   restart — a name minted at boot orphans
  #     - "nats://node-2:6222"      #   this member's replicas every time
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
  path: "./crewlet-data/company.db"   # ONE file, owned exclusively by this
                                    #   process. Not a shared database, and no
                                    #   DSN: two engines on one file corrupt it

coordination:
  type: local                       # one node holding its own seat leases;
                                    #   a fleet needs `embedded-kv`. There is no
                                    #   address to give it: the coordination KV
                                    #   rides the stream's OWN connection. A
                                    #   second dial to the same broker fails
                                    #   independently, so a node could hold live
                                    #   leases over a connection that still works
                                    #   while the one carrying its inbox has
                                    #   dropped — alive to its peers, deaf to
                                    #   its work
  # lease_ttl_seconds: 45           # 0 takes the seat layer's own measured
                                    #   default of 45s — three heartbeat
                                    #   intervals, so two consecutive missed
                                    #   renewals still leave a full interval to
                                    #   recover in. Shorter speeds failover and
                                    #   sheds healthy seats on ordinary jitter

api:
  host: "0.0.0.0"
  port: 8000
  auth:
    tokens:
      - id: founder
        token: "${CREWLET_API_TOKEN_FOUNDER}"

logging:
  level: info       # debug, info (default), warn, error
  format: console   # console (default: columns and colour for a person),
                    #   text (slog key=value), json (for a log shipper)
```

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
    manages: [VP Engineering, PM Lead]
    # ... same role fields as unit roles (see table below)
```

Root-level roles participate fully in the `manages[]` hierarchy and task
routing. Their knowledge is scoped to the org (visible to all agents).
They don't inherit `mcp_env` from any unit.

### Units

The `units` key defines your org as a recursive tree of `OrgUnit` nodes.
Each unit can contain roles directly and/or child units, supporting any
nesting depth — flat teams, departments with sub-teams, divisions, or custom types.

```yaml
units:
  - name: Engineering                   # required — unit name
    type: department                    # optional — unit type (default: "team")
    lead: CTO                           # optional — inherited from parent if omitted
    purpose: "Build and ship the product"  # optional
    children:                           # optional — nested child units
      - name: Backend                   # required
        type: team                      # optional
        lead: Tech Lead                 # optional — inherited from parent if omitted
        goals:                          # optional
          - "Ship features on 2-week cadence"
        channel: backend                # optional — the team's chat channel, inherited
        integrations:                   # optional — the unit's tracker + wiki "home"
          jira:                          #   identity (webhook routing + write home; NOT
            project: "BACK"              #   read scope, NOT an MCP credential)
          confluence:
            space: "BACK"
        mcp_env:                        # optional — per-agent MCP creds, inherited by roles
          atlassian:                     #   (real tool credentials only; the chat transport
            JIRA_API_TOKEN: "${BACK_JIRA_TOKEN}"  #   identity is per-agent)
        roles:
          - name: Tech Lead             # required — unique agent identity
            goal: "..."                 # optional — individual mission
            backstory: "..."            # optional — personality, background, expertise
            llm: default                # optional — named LLM provider
            token_budget: 200000        # optional — per-agent token limit
            handle: tl                   # optional — custom handle (default: auto-slugified)
            email: tl@company.com       # optional — agent email
            manages: [Engineer A, Engineer B]  # optional — hierarchy links
            responsibilities:           # optional
              - "Review all PRs"
            behavioral_guidelines:      # optional
              - "Be thorough in code reviews"
            integrations:               # optional — per-agent transport identity
              mattermost:
                bot_token: "${MATTERMOST_BOT_TOKEN_TL}"
                username: tl-bot        # optional — the bot's Mattermost username
                channel: backend        # optional — default channel
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
| `name` | string | yes | Unique seat identity |
| `kind` | `agent` \| `human` | no | Who holds the seat (default `agent`). `human` marks a [human seat](../concepts/humans-in-the-org.md) — addressable, never spawned; rejects every runtime-only field below and requires at least one `contact` identity |
| `contact` | dict | human seats | External identities: `slack_user_id`, `mattermost_user_id` (a username, not an ID), `atlassian_account_id` (Jira+Confluence), `github_login`, `gitlab_username`. Each accepts a literal ID or exactly one whole-value `${VAR}` env reference, resolved at use time; values are whitespace-stripped, and a `${VAR}` embedded inside a longer string is rejected at validation (see [Humans in the Org Chart](../concepts/humans-in-the-org.md)) |
| `availability` | string | no | Human seats only — free-text availability rendered into rosters and `lookup_colleague` results |
| `goal` | string | no | Individual mission statement |
| `backstory` | string | no | Personality, background, expertise |
| `llm` | string or dict | no | Named LLM provider key. Dict form takes `default`, `plan`, `execute`, `review`, `subagent` keys for per-phase [Turn Engine](../concepts/turn-engine.md#per-phase-llm-models) model split |
| `llm_plan` / `llm_execute` / `llm_review` / `llm_subagent` | string | no | Per-phase overrides (alternative to the dict-shaped `llm`) |
| `llm_auxiliary` | string | no | Cheap/fast model used by reflection workers (PersistDecider, episode summariser) |
| `llm_judge` | string | no | Cheap/fast model used by the [round-cap extension judge](../concepts/turn-engine.md#round-cap-extension-judge); falls back to `llm` |
| `token_budget` | int | no | Per-agent token limit (0 = unlimited) |
| `handle` | string | no | Custom identity slug (default: auto-derived) |
| `email` | string | no | Agent email address |
| `manages` | list[string] | no | Names of roles this agent manages |
| `responsibilities` | list[string] | no | Role responsibilities |
| `behavioral_guidelines` | list[string] | no | Behavioral rules |
| `mcp_env` | dict | no | Per-agent MCP server credentials, keyed by server name — env vars for `stdio` servers, HTTP headers for `http` servers (e.g. `atlassian.JIRA_USERNAME` / `atlassian.JIRA_API_TOKEN`, `confluence.CONFLUENCE_USERNAME` / `confluence.CONFLUENCE_API_TOKEN`, `slack.SLACK_MCP_XOXB_TOKEN`, `mattermost.MATTERMOST_TOKEN`, `github.Authorization: "Bearer …"`). The per-agent tool-credential surface only — scope a server via its own filter (`JIRA_PROJECTS_FILTER` / `CONFLUENCE_SPACES_FILTER`) if needed. The unit's Jira project / Confluence space identity lives under `integrations` (below), not here |
| `integrations.slack` | dict | no | This seat's own Slack app: `bot_token`, `signing_secret`, optional `channel`. **Both credentials are required together** — without the token the seat receives messages it cannot answer, without the secret its route answers 503 while the app's settings page reports a healthy request URL. `crewlet slack provision` mints both into the `${VAR}`s these fields point at |
| `integrations.mattermost` | dict | no | Per-agent Mattermost **transport** identity (`bot_token`, optional `username`, optional `channel`). One credential, three readers: the same token is named as `mcp_env.mattermost.MATTERMOST_TOKEN` for the MCP subprocess, and the inbound websocket for this seat authenticates with it too |
| `integrations.jira.project` | string | no | **Authored on a unit or root-level role** (→ `org.Unit.JiraProject` / `org.Role.JiraProject`). The team's Jira project as integration identity: an issue that names nobody in the org chart routes to the unit lead, and it is the project the team files work under. **Not** an MCP credential, and it does **not** scope knowledge reads |
| `integrations.confluence.space` | string | no | **Authored on a unit or root-level role** (→ `org.Unit.ConfluenceSpace` / `org.Role.ConfluenceSpace`). The team's Confluence space as integration identity: a page change that names nobody routes to the unit lead, and it is where the team writes. It does **not** scope reads — read scope is the org-wide `knowledge.confluence_spaces` only |
| `schedules` | list | no | Role-scoped recurring tasks — see [Schedules](#schedules) |

### Schedules

Roles **and** units can own recurring work via a `schedules:` list (a
cron analogue). Each entry fires a task on its cron expression; see the
[Scheduling](../concepts/scheduling.md) concept doc for the full design.

```yaml
units:
  - name: Backend
    type: team
    lead: Backend Lead
    schedules:
      # unit schedule, target defaults to `each` → every direct member runs it
      - name: daily-standup
        cron: "30 9 * * 1-5"            # 5-field cron, evaluated in `timezone`
        timezone: Europe/Amsterdam      # IANA tz; defaults to scheduling.default_timezone
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
| `timezone` | string | no | IANA timezone (default: `scheduling.default_timezone`) |
| `target` | string | no | **Unit schedules only**: `each` (default — every direct member) or `lead` (the effective unit lead). Ignored for role schedules; for a per-person task, use a role schedule |
| `enabled` | bool | no | `false` keeps the schedule in config without firing (default `true`) |
| `timeout_seconds` | int | no | Hard wall-clock cap on the scheduled turn (default `180`) |
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
    webhook_secret: "${JIRA_WEBHOOK_SECRET}"  # Data Center only — HMAC-SHA256 secret

  confluence:                            # the knowledge base
    url: "${CONFLUENCE_URL}"              # a Data Center instance, or a Cloud site
    # cloud_id: "${CONFLUENCE_CLOUD_ID}"  # an Atlassian Cloud id — give this OR url
    # site_url: "https://acme.atlassian.net"  # with cloud_id: the base for links people open
    token: "${CONFLUENCE_API_TOKEN}"      # API token (org read account)
    email: "${CONFLUENCE_EMAIL}"          # Cloud only — the account's email, for Basic auth
    webhook_secret: "${CONFLUENCE_WEBHOOK_SECRET}"  # Data Center only — HMAC-SHA256 secret
    skills_space: TS                      # tool-skill pages; excluded from routing and search

  slack:                                 # per-seat apps live on each role
    typing_status: addressed             # working indicator: addressed (default) | always | off
    status_phrases:                      # optional — replaces the built-in wording, per phase
      plan: ["is nimbusing...", "is thinking very hard..."]

  mattermost:                            # self-hosted chat — transport AND inbound fleet
    enabled: true
    url: "${MATTERMOST_URL}"             # instance base URL (required when enabled)
    team: nimbus                         # team slug (required when enabled)
    typing_status: off                   # working indicator: off (default) | addressed | always
    provisioning:                        # read only by `crewlet mattermost provision`
      channels: [town-square, engineering]   # channels every agent bot joins

  github:
    enabled: true
    # url:  "https://github.example.com"       # Enterprise Server; omit for github.com
    webhook_secret: "${GITHUB_WEBHOOK_SECRET}"   # HMAC-SHA256 secret (required when enabled)
    token: "${GITHUB_ENGINE_TOKEN}"              # read credential for participant fan-out
    provisioning:                                # read only by `crewlet github provision`
      org: nimbus                                #   (organization holding the repositories)
      repos: [nimbus/api, nimbus/web]            #   (extra owner/repo entries to hook)
      org_webhook: auto                          #   auto | true | false

  gitlab:
    enabled: true
    url: "https://gitlab.com"                    # instance base URL (required when enabled)
    signing_secret: "${GITLAB_SIGNING_SECRET}"   # 19.1+ whsec_ HMAC — the only verification mode, required
    token: "${GITLAB_ENGINE_TOKEN}"              # optional read PAT → participants-based routing
    provisioning:                                # read only by `crewlet gitlab provision`
      group: nimbus-hq                           # top-level group agent service accounts join
      access_level: developer                    # developer | maintainer

```

- **`forge_app_id`** — verifies the Forge Invocation Token (FIT) on Cloud webhooks against Atlassian's JWKS; the `aud` claim must match. Required when Jira Cloud delivers through the Forge app.
- **`jira`** — the Atlassian tracker, served end to end. Give `url` **or** `cloud_id`, never both — they are two ways to name one instance and `crewlet validate` refuses the ambiguity. `token` is the org read account (an issue's watchers are the one routing input a webhook never carries); `email` switches authentication to Cloud's Basic scheme; `site_url` is the human base for links when the instance is named by a cloud id. `webhook_secret` is **required for Data Center** and unused on Cloud, whose events arrive through the Forge app instead. Each seat's own credential lives in `mcp_env.atlassian` (or `mcp_env.jira`) and is what the engine resolves its account id from — see [Jira](../integrations/jira.md).
- **`confluence`** — the knowledge base, and the **query-time search** behind every Plan phase's "Relevant knowledge" block. Same address rule as `jira`: `url` **or** `cloud_id`, never both. `token` is the org read account a seat with no Confluence credential of its own searches under; a seat WITH one searches as itself and Confluence enforces its page permissions natively. `webhook_secret` is required for Data Center and unused on Cloud. The knowledge backend is **single-homed** — the engine wires exactly one `knowledge.Searcher`, because two would make an agent's answer to "what do we already know about this" depend on which one was asked. Scope reads with `knowledge.confluence_spaces`; publish with [`crewlet confluence import`](../reference/cli.md#crewlet-confluence-import). See [Confluence](../integrations/confluence.md).
- **`slack`** — the hosted chat backend. The org-level block carries **no credentials at all**: Slack gives each agent its OWN app, so the token and signing secret live on each role's `integrations.slack`, and this block holds only the working-indicator settings. **`typing_status`** takes `off` / `addressed` / `always` and defaults to `addressed` — the opposite of Mattermost's default, because Slack's indicator renders TEXT and a phase change is something the person waiting can actually read, which is also what makes `status_phrases` worth having here. Inbound events arrive per seat at `/webhooks/slack/{handle}`, verified against that seat's own signing secret. Provision with [`crewlet slack provision`](../reference/cli.md#crewlet-slack-provision). See [Slack Integration](../integrations/slack.md).
- **`mattermost`** — the self-hosted chat backend, and the one integration that is **both** inbound and outbound: enabling it starts the outbound transport *and* the websocket fleet that holds one connection per agent seat (Mattermost has no usable inbound webhook, so nothing has to reach the engine — no public URL, no tunnel). `url` and `team` are both **required** when enabled. Per-agent identity lives on each role's `integrations.mattermost.bot_token`, named again as `mcp_env.mattermost.MATTERMOST_TOKEN` for the MCP subprocess. **`typing_status`** takes `off` / `addressed` / `always` and defaults to `off`, and there is deliberately no `status_phrases`: Mattermost renders a fixed client-side indicator with no API for the text. The `provisioning:` sub-block is read only by [`crewlet mattermost provision`](../reference/cli.md#crewlet-mattermost-provision), not the engine. A company may run Mattermost and Slack together — they are different workspaces with different people in them, and an org migrating from one to the other runs both for a while. See [Mattermost Integration](../integrations/mattermost.md).
- **`github`** — the hosted code host, served end to end. `url` is **optional**: leave it unset for github.com, whose API lives on a different host rather than a path on the web UI, and name an Enterprise Server there — the REST base is derived either way. `webhook_secret` is **required** when enabled, and takes any string (GitHub signs with it verbatim, so unlike GitLab's there is no shape to get wrong). The optional `token` is a read credential for **participant fan-out** — a payload carries the author, assignees and requested reviewers but not who has commented or reviewed — and it is what `crewlet github provision` registers webhooks with. Each seat's own credential lives in `mcp_env.github` and is what the engine resolves its login from; a human seat is reached by `contact.github_login` instead. The `provisioning:` sub-block is read only by [`crewlet github provision`](../reference/cli.md#crewlet-github-provision): `org_webhook: auto` takes one organization-level hook where the credential may (covering repositories created later) and falls back to one per repository where it may not. A company may run GitHub and GitLab together — they are two hosts with different repositories on them. See [GitHub Integration](../integrations/github.md).
- **`gitlab`** — webhook config + boot-time identity resolution. `url` and `signing_secret` are both **required** when enabled — inbound webhooks are verified by the GitLab 19.1+ signing-token HMAC only (the plain `X-Gitlab-Token` scheme is unsupported; self-managed < 19.1 is not supported). The optional `token` (a read-only PAT; the provisioner mints a dedicated `crewlet-engine` account for it) enables **participants-based routing** — comments and state changes reach everyone participating in the issue/MR, not just assignees and mentioned users. The GitLab MCP server is a `shared: false` [`mcp_servers`](#mcp-servers) entry — by default the official `glab mcp serve` (stdio, spawned per-role by the engine, no separate server) — and each agent's service-account PAT goes in `role.mcp_env.gitlab.GITLAB_TOKEN`. The `provisioning:` sub-block is read only by `crewlet gitlab provision`, not the engine. See [GitLab Integration](../integrations/gitlab.md).
- **`transports`** — outbound delivery transports (e.g. `email`). The Mattermost and GitLab transports are auto-derived from the sections above; this list adds any others. Jira is inbound-only by design and has no transport: an agent's writes to an issue go through its own MCP tools under its own credential, never through the engine. Confluence is inbound-only for the same reason — a page an agent writes is written by its own MCP tools. Slack is the exception among the chat backends: its transport is per seat, derived from each role's own app credentials rather than from an org-level block, so it is not listed here either.

## Knowledge

```yaml
knowledge:
  confluence_spaces: ["ENG", "HANDBOOK"] # org-wide spaces every agent can search (optional)
```

`knowledge.confluence_spaces` is the org-wide read scope: [Confluence](../integrations/confluence.md) space keys, materialised onto `org.Organization.ConfluenceSpaces` and read by the Confluence searcher. It requires an `integrations.confluence` block — a read scope for a backend that is not there narrows nothing while reading as though it does. **Optional:** leave it unset and a seat holding its own Confluence credential searches *unscoped*, bounded by that account's own page permissions; a seat with no credential of its own falls back to the org token and is **only** searched under a scope, because an unscoped search on the shared credential is how one seat reads a page its own account never could.

---

## MCP Servers

**All** MCP tool servers are declared here — including the Jira/Confluence (`atlassian`), Slack, and GitHub servers. A `shared: true` server runs once for everyone; a `shared: false` server is a per-role template, and each agent supplies its own credentials via `role.mcp_env[name]` (env vars for `stdio`, HTTP headers for `http`).

```yaml
mcp_servers:
  # shared stdio server (one instance for all agents)
  - name: tavily
    command: npm
    args: ["exec", "--yes", "--", "tavily-mcp@latest"]
    env:
      TAVILY_API_KEY: "${TAVILY_API_KEY}"

  # per-role stdio server — Jira + Confluence share one mcp-atlassian
  - name: atlassian
    shared: false                       # per-role: token from role.mcp_env.atlassian
    command: uvx
    args: ["mcp-atlassian"]
    env:
      JIRA_URL: "${JIRA_URL}"
      CONFLUENCE_URL: "${CONFLUENCE_URL}"

  # per-role http server — remote GitHub MCP
  - name: github
    transport: http                     # stdio | http (default: stdio)
    shared: false                       # per-role: Authorization header from role.mcp_env.github
    url: "https://api.githubcopilot.com/mcp/"
    tool_prefix: ""                     # optional — prefix tool names
    tool_annotations: {}                # optional — behavioural-hint overrides (see Tool Capabilities)
    startup_timeout_seconds: 120        # optional — connect + handshake + discovery
    request_timeout_seconds: 300        # optional — one tool call
```

Full field reference: `name` (required), `transport` (`stdio`/`http`), `shared` (default `true`), `command`/`args`/`env` (stdio), `url`/`headers` (http), `tool_prefix`, `tool_annotations`, `startup_timeout_seconds`, `request_timeout_seconds`.

### Timeouts

An MCP server is another program, and the failure that matters is not an
error but a *silence* — a server that launches and never completes the
handshake, or answers discovery and then never returns from a tool call,
raises nothing at all. The engine starts MCP servers on the
seat-acquisition path, so a silent one does not merely lose its own
tools: it holds up every seat behind it, for the life of the process.
Both deadlines therefore always apply.

- **`startup_timeout_seconds`** (default `120`) bounds launching the
  process (or opening the HTTP session), the protocol handshake, and the
  first `tools/list`. The default suits a `uvx` / `npx` server whose
  package is not yet in the local cache — the slow case for a *healthy*
  server. Lower it for a server you launch from a local checkout.
- **`request_timeout_seconds`** (default `300`) bounds one tool call. It
  matches the MCP SDK's own SSE-friendly HTTP read default, so a tool
  behaves the same over stdio and over HTTP. Raise it for a server whose
  tools genuinely run long (a large code search, a slow report); lower it
  for one that should always answer quickly, so a wedged call reaches the
  agent as a failed tool result it can react to instead of a turn that
  never ends.

A server that exceeds either deadline is logged and skipped; the rest of
the company still starts.

---

## Environment Variable References

All string values in YAML support `${ENV_VAR}` syntax, keeping secrets out of config files. Variables are resolved at startup from the [secret store](../concepts/secret-store.md) first (an encrypted table the provisioning CLIs can write into directly; inert until you store something), then `os.environ`. An unanswered reference resolves to the empty string.

Only the braced identifier form is substituted — `${NAME}` where `NAME` matches `[A-Za-z_][A-Za-z0-9_]*`. Bare `$NAME` and shell parameter expansions (`${1:-x}`, `${line#host=}`) pass through untouched, so config-authored script content — a sandbox setup step's helper script, say — survives intact.

```yaml
providers:
  llm:
    default:
      api_keys:                        # resolved at startup (one or many)
        - "${LLM_API_KEY}"
```

See [Environment Variables](../reference/environment-variables.md) for a full list.

---

## Full Example

See the [quickstart](quickstart.md) for a complete working config.
