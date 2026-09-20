/**
 * The wire, typed.
 *
 * Every name here is a `json` tag on a Go struct the engine already ships, and
 * the pairing is deliberate: this file is the client half of a protocol whose
 * server half is `internal/api/livestate/types.go`, `internal/tokens` and
 * `internal/api/queries`. A field renamed on one side and not the other is a
 * broken dashboard rather than a refactor, which is why the e2e gate replays
 * real server frames through this package (internal/e2e/golden_test.go).
 *
 * Two rules the shapes encode, both learned the hard way:
 *
 *  - **Optional is not nullable is not absent.** An overlay is MERGED onto the
 *    row a client already holds, so an omitted key reads as "unchanged". That
 *    is why `afk_reason` is required-but-possibly-empty while `budget` is
 *    `| null`: one has to be able to say "no longer AFK", the other has to be
 *    able to say "nobody is measuring".
 *  - **A count that could not be taken is `null`, never `0`.** The integrations
 *    answer and the budgets answer both distinguish "zero" from "this process
 *    cannot say", and collapsing them is how a dashboard reports a healthy
 *    silence over a store it never reached.
 */

// ---------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------

/** One payload-free row of the activity feed. */
export interface FeedRow {
  id: string;
  type: string;
  timestamp: string;
  source: string;
  actor: string;
  summary: string;
  category: string;
  trace_id: string;
  span_id: string;
  parent_span_id: string;
  topic: string;
  failed: boolean;
}

/**
 * A feed row plus the payload, as the live `event` push carries it.
 *
 * `failed` is REQUIRED here, and that is the point: it is derived once on the
 * server and stamped onto this frame as well as onto the snapshot's own row, so
 * the live half of a list and its hydrated half agree about the same event. It
 * used to be on the row alone, and a turn that failed while somebody was
 * watching rendered exactly like one that succeeded — then grew its failure
 * mark on the next reload.
 */
export interface EventEnvelope extends FeedRow {
  payload?: Record<string, unknown>;
}

/** One stored event, as `GET /events/{id}` and the `event` query answer it. */
export interface EventRecord {
  id: string;
  type: string;
  timestamp: string;
  source: string;
  actor: string;
  summary: string;
  category: string;
  trace_id: string;
  span_id: string;
  parent_span_id: string;
  topic: string;
  payload?: Record<string, unknown>;
  tags?: Record<string, string>;
  /**
   * Whether the work this event reports failed, derived SERVER-SIDE from the
   * event type plus the stored `failed` tag (store.EventRecord.Failed). It has
   * always been on the wire and this interface did not declare it, so every
   * screen reading a stored record had to cast to FeedRow to see it — which is
   * how a turn could render red in the feed and clean on its own page.
   */
  failed?: boolean;
}

export interface EventsPage {
  events: FeedRow[];
  /** The cursor for the next page, or null at the end of the retained record. */
  next: { before_time: string; before_id: string } | null;
  /** True when the store itself has nothing older, as opposed to this page ending. */
  exhausted: boolean;
}

export interface TraceAnswer {
  trace_id: string;
  events: EventRecord[];
  /**
   * True when the store stopped at its per-trace cap rather than at the end of
   * the trace. A trace shown short with no note reads as a complete causal
   * chain that simply ends, which is the one thing a reader must not conclude
   * from it — and the store's own doc asks the caller to say so.
   */
  truncated: boolean;
}

// ---------------------------------------------------------------------------
// Seats
// ---------------------------------------------------------------------------

/** Why a seat stopped. One shape for every kind of stop. */
export interface ErrorInfo {
  kind: string;
  message: string;
  phase: string;
  turn_id: string;
  at: string;
  event_id: string;
}

/** One tool the model called, as the phase event records it. */
export interface ToolExecution {
  name?: string;
  tool?: string;
  /** The round this call belongs to. THE ordering key — see `roundsOf`. */
  round?: number;
  arguments?: unknown;
  args?: unknown;
  result?: unknown;
  output?: unknown;
  error?: string;
  failed?: boolean;
  duration_ms?: number;
  origin?: string;
  server?: string;
}

/** One message of the prompt a phase was given. */
export interface PromptMessage {
  role?: string;
  content?: string;
}

/**
 * A round in flight. Never merged into `round_narration` — arriving text and
 * committed text are different facts, and a reader has to be able to tell
 * them apart. `abandoned` holds attempts a provider gave up on partway
 * through, oldest first.
 */
export interface PartialRound {
  round?: number;
  reasoning?: string;
  content?: string;
  abandoned?: RoundNarration[] | null;
}

/** One round's model turn, as the engine recorded it. */
export interface RoundNarration {
  round?: number;
  reasoning?: string;
  content?: string;
}

/** The in-flight LLM call: the latest progress round, or a phase-start seed. */
export interface LiveCall {
  /**
   * ONE RUN of a turn. A trigger that fails without acting is redelivered, so
   * one unit of work legitimately runs several times, and each run is its own
   * id: `(turn_id, phase, iteration)` is the key every phase row is stored
   * under, and it has to be unique per execution. See `adr/0017`.
   */
  turn_id: string;
  /**
   * The unit of work behind that run — what groups a trigger's attempts.
   * Absent on a row an engine from before the split wrote, where `turn_id`
   * carries it instead.
   */
  work_key?: string;
  phase: string;
  iteration: number;
  model: string;
  trigger: Record<string, unknown> | null;
  prompt: string;
  /**
   * The conversation as the phase was given it: the system and user
   * messages only. Typed rather than `unknown[]` — it was untyped, so the
   * live view could not read the system prompt off it and hardcoded "".
   */
  prompt_messages: PromptMessage[] | null;
  response: string;
  input_tokens: number;
  output_tokens: number;
  total_tokens: number;
  tool_executions: ToolExecution[] | null;
  /**
   * Per-round model turns — `{round, reasoning, content}` — where `round`
   * matches `tool_executions[].round`, which is what lets the two lists
   * interleave into one ledger. Absent on a phase this engine recorded
   * before it sent them.
   */
  round_narration?: RoundNarration[] | null;
  /** The round being written right now; absent when no round is open. */
  partial_round?: PartialRound | null;
  round_num: number;
  rounds: number;
  in_progress: boolean;
  failed?: boolean;
  error?: ErrorInfo | null;
  updated_at: string;
  /** When the call began. Unlike `updated_at`, it never moves. */
  started_at?: string;
}

/** A live token meter: the fleet's SHARED counter, as the budget gate enforces
 *  it — every node's spend since the last deliberate reset, against the cap in
 *  the active revision. Never comparable to a spend rollup, which is a window
 *  over time rather than the life of a counter. */
export interface Meter {
  used: number;
  max: number;
  /** When this scope last turned a charge away, in UTC; empty while it is not
   *  refusing. The gate's own record, kept in the shared counter beside the
   *  spend, so every node reports the same one and it clears on the scope's
   *  next admitted charge. It is what "exhausted" means: a refused charge
   *  increments nothing, so `used >= max` is sufficient but never necessary —
   *  a scope charged in rounds stops short of its cap for ever. */
  refused_at?: string;
}

/** The live half of a seat row, merged onto its static config row. */
export interface Overlay {
  /**
   * Omitted until an event says whether the seat is running, so the state the
   * roster sent (or the one a client already holds) stands.
   */
  state?: string;
  runtime_id?: string;
  current_phase?: string | null;
  current_iteration?: number;
  live_call?: LiveCall | null;
  last_error?: ErrorInfo | null;
  budget?: Meter | null;
  /** Always present, even empty: an omitted key would read as "still AFK". */
  afk_reason?: string;
}

/** A seat row: static config identity, plus whatever overlay has been merged. */
export interface AgentRow extends Overlay {
  id: string;
  agent_id?: string;
  role: string;
  handle?: string;
  /** Set by `applyAgents` when an overlay names a role the roster lacks. */
  [key: string]: unknown;
}

/** One in-flight detached coding run. */
export interface SandboxEntry {
  turn_id: string;
  role: string;
  agent_handle: string;
  agent_id: string;
  coding_agent: string;
  sandbox_id: string;
  task: string;
  status: string;
  started_at: string;
  question?: string;
  audience?: string;
}

/** One durable coding run, as the `sandbox_runs` query answers it. */
/** The durable statuses `sandbox.PendingRun` actually carries.
 *
 *  Not a free string: the screen's own tone map named `succeeded`, `completed`,
 *  `cancelled` and `reclaimed`, none of which the engine can write, so four of
 *  its seven entries were unreachable and three real states fell through to
 *  the neutral default. */
/**
 * The five statuses a run record can hold.
 *
 * THERE IS NO `done` OR `failed` HERE, and that is the engine's shape rather
 * than an omission: a run's record is DELETED once the run settles and its box
 * is reclaimed, so `sandbox.Active` is every status a record can carry and the
 * terminal pair can never reach a client. How a run ended is on the event
 * stream — the resumed turn's own events, or a `sandbox_run_failed` naming the
 * reason — never on this row.
 */
export type SandboxStatus =
  "launching" | "running" | "awaiting_clarification" | "resumed" | "reseed";

export interface SandboxRun {
  /** The RUN this job belongs to — one execution of a turn, and this
   *  record's own key. See `adr/0017`. */
  turn_id: string;
  /** The unit of work behind that run. Absent on a row written before the
   *  identities were split. */
  work_key?: string;
  agent_handle: string;
  role: string;
  status: SandboxStatus;
  coding_agent: string;
  /** Which configured cell the run's box is in: direct, container or e2b. */
  placement: string;
  task_description: string;
  question: string;
  audience: string;
  branch: string;
  trace_id: string;
  owner: string;
  box_exists: boolean;
  paused_at: string;
  pause_ttl_seconds: number;
  started_at: string;
  updated_at: string;
  answerable_in_chat: boolean;

  /**
   * What this run called through the MCP bridge, in order.
   *
   * THE ONE TOOL LOG THAT HAS NOWHERE ELSE TO LIVE. A native tool loop keeps
   * its calls in memory and the turn writes them when it ends; a bridged run's
   * are made by a process outside the engine, minutes or hours apart and
   * possibly across a restart. Absent on every run that is not bridged, which
   * is every ordinary coding run.
   */
  bridge_calls?: BridgeCall[];

  /**
   * How many calls were dropped from the MIDDLE of that list.
   *
   * The engine bounds the log at 200 and drops the middle rather than the
   * start — how a run began and how it ended are what explain it. Reported
   * rather than hidden: a log that silently skips is a log that lies about
   * what the run did.
   */
  bridge_calls_elided?: number;
}

/** One call a bridged run made. */
export interface BridgeCall {
  name: string;
  /** What the caller passed, as JSON TEXT — never a decoded map. */
  args?: string;
  output?: string;
  failed?: boolean;
  at: string;
}

// ---------------------------------------------------------------------------
// Spend
// ---------------------------------------------------------------------------

export interface Bucket {
  input_tokens: number;
  output_tokens: number;
  total_tokens: number;
  calls: number;
  /**
   * What the calls in this bucket were billed, and how many of them quoted
   * anything at all.
   *
   * TWO NUMBERS, because only a subscription coding CLI reports a price. A
   * `cost_usd` of 0 over `priced_calls: 0` means nobody said what this cost;
   * over 2 it means two runs were billed nothing.
   *
   * NOT RENDERED. The dashboard measures spend in TOKENS — a currency shown
   * for the minority of calls that quote one, beside a token count covering
   * all of them, reads as the company's spend and is a fraction of it. The
   * field stays on the wire and in the store, so putting a price back on a
   * screen is a rendering change rather than a migration. `priced_calls` IS
   * read: it says how much of a window has accounting of its own.
   */
  cost_usd: number;
  priced_calls: number;
}

export interface PhaseRow extends Bucket {
  phase: string;
}
export interface ModelRow extends Bucket {
  model: string;
}
export interface WorkerRow extends Bucket {
  worker: string;
}
export interface AgentSpendRow extends Bucket {
  role: string;
  handle: string;
  agent_id: string;
  by_phase: Record<string, Bucket>;
}
export interface TurnSpendRow extends Bucket {
  /** ONE ROW PER RUN. Each attempt at a redelivered trigger really did spend
   *  what it spent, so summing them would charge one turn with another's
   *  tokens — `work_key` is what relates them. See `adr/0017`. */
  turn_id: string;
  /** The unit of work this run was an attempt at, absent when it had none. */
  work_key?: string;
  role: string;
  handle: string;
  agent_id: string;
  started_at: string;
  ended_at: string;
  by_phase: Record<string, Bucket>;
}

export interface Rollup {
  /** The window this covers, as two RFC3339 instants — `until` exclusive. */
  since: string;
  until: string;
  agent_role: string;
  totals: Bucket;
  by_phase: PhaseRow[];
  by_model: ModelRow[];
  by_worker: WorkerRow[];
  by_agent: AgentSpendRow[];
  by_turn: TurnSpendRow[];
  /** High-water mark: live completions past it are folded in, earlier ones skipped. */
  aggregated_through: string;
}

/**
 * THE SAME SPEND WITH A TIME AXIS — `token_series`.
 *
 * `Rollup` is a breakdown whose every row is a sum over the whole window, so
 * it cannot say WHEN. The engine buckets because it is the one place that can:
 * the browser holds at most the live window's records, so an axis folded here
 * would be correct for a day and absent for every other range.
 */
export interface SeriesBand extends Bucket {
  /** The band's key in each point's `groups`. Empty on, and only on, `other`. */
  group: string;
  handle?: string;
  /** The residual: every group past the chart's cap, and how many it stands for. */
  other: boolean;
  folded: number;
}

export interface SeriesPoint extends Bucket {
  /** The bucket's START, RFC 3339 in UTC — never its middle or its end. */
  at: string;
  groups: Record<string, Bucket>;
  other: Bucket;
}

export interface TokenSeries {
  group: "phase" | "model" | "seat" | "unit" | "worker" | "turn";
  bucket: "hour" | "day";
  /** The window COVERED, which the store may have floored below what was asked. */
  since: string;
  until: string;
  series: SeriesPoint[];
  by_group: SeriesBand[];
  /**
   * Every record in the window, INCLUDING the ones this grouping places in no
   * band — so `totals` is not the sum of `by_group`, and `grouped` is what the
   * bands do cover. Grouping by worker leaves out every phase that is not a
   * worker's, and the gap is the answer to "why is this less than the rollup".
   */
  totals: Bucket;
  grouped: Bucket;
}

/** One bucket of the event log's time axis. */
export interface EventBar {
  /** The bucket's START, RFC3339 — never its middle and never its end. */
  at: string;
  count: number;
}

/**
 * The event log's own time axis.
 *
 * WHAT A PAGE OF ROWS CANNOT SAY: a listing answers what happened and has no
 * dimension for when the company was busy. Counted by the ENGINE through the
 * same predicate the listing filters with, so a bar can never claim rows the
 * list beside it would not show.
 */
export interface EventSeries {
  bucket: "minute" | "hour" | "day";
  /** The window COVERED, snapped outward to whole buckets. */
  since: string;
  until: string;
  /** Every bucket in the window, INCLUDING the empty ones. */
  bars: EventBar[];
  /** The window's whole count, stated rather than left as a sum of the bars. */
  total: number;
  /**
   * How many rows each category would give, over the window, with the CATEGORY
   * filter lifted and every other one applied — which is the only meaning a
   * facet count can have. A category with no rows is absent, so a screen
   * rendering the closed set reads a missing key as the zero it is.
   */
  by_category: Record<string, number>;
}

/** The org-wide live meter, plus the identity of the engine run reporting it. */
export interface OrgBudget {
  meter_id?: string;
  seq?: number;
  org?: Meter;
}

export interface BudgetsAnswer {
  org: {
    max_tokens: number;
    durable_used: number;
    durable_updated_at: string;
    /** When the company cap last refused a charge; empty while it is not refusing. */
    refused_at: string;
  };
  seats: {
    role: string;
    handle: string;
    agent_id: string;
    max_tokens: number;
    durable_used: number;
    durable_updated_at: string;
    /** When this seat's cap last refused a charge; empty while it is not refusing. */
    refused_at: string;
  }[];
  /** False means the durable counter could not be READ — never that it is zero. */
  durable: boolean;
}

// ---------------------------------------------------------------------------
// The company document (the guarded `config` query, `GET /config`)
// ---------------------------------------------------------------------------

/**
 * The company document as the configuration surface serves it, REDACTED: a
 * credential field holds a whole `${VAR}` reference or the mask
 * `__redacted__`, never a value.
 *
 * WHERE EVERYTHING THE PROJECTION NO LONGER CARRIES IS READ. `/org` is
 * anonymous, so it was narrowed to a charter and a tree ([OrgProjection]); a
 * seat's email, model chain, token budget, contact identities, tool
 * credentials, integration blocks and schedules live here instead, behind the
 * operator token.
 *
 * A SUPERSET, deliberately open. Only the fields a screen reads are named, and
 * the index signature keeps every other key the engine writes — including the
 * keys a NEWER engine writes that this build has never heard of — part of the
 * value rather than something a typed copy silently drops.
 */
export interface CompanyDocument {
  name?: string;
  mission?: string;
  vision?: string;
  policies?: string[];
  roles?: ConfigRole[];
  units?: ConfigUnit[];
  integrations?: Record<string, unknown>;
  providers?: Record<string, unknown>;
  [key: string]: unknown;
}

/** One seat in the company document, as `config.Role` writes it. */
export interface ConfigRole {
  name: string;
  kind?: string;
  handle?: string;
  email?: string;
  /** A root seat's home unit: the engine MOVES the seat into it. */
  unit?: string;
  goal?: string;
  backstory?: string;
  responsibilities?: string[];
  behavioral_guidelines?: string[];
  manages?: string[];
  workers?: string[];
  /** 0 or absent is unlimited. */
  token_budget?: number;
  llm?: PhaseLLM;
  llm_review?: ProviderKeys;
  llm_subagent?: ProviderKeys;
  llm_auxiliary?: ProviderKeys;
  llm_judge?: ProviderKeys;
  llm_sandbox?: ProviderKeys;
  /** Unset means the company default, so this is three-valued. */
  learning_enabled?: boolean | null;
  /** A human seat's identities: one per surface, keyed by [HumanContactKey]. */
  contact?: Record<string, string>;
  availability?: string;
  /** Server name to variable name to a `${VAR}` reference or the mask. */
  mcp_env?: Record<string, Record<string, string>>;
  sandbox?: Record<string, unknown>;
  placement?: Record<string, unknown>;
  integrations?: ConfigRoleIntegrations;
  schedules?: ScheduleSpec[];
  [key: string]: unknown;
}

/**
 * The identities a human seat's `contact` holds, as `org.HumanContact` names
 * them: a Slack member ID, a Mattermost USERNAME (the key says user id, the
 * value is the name a mention renders), an Atlassian account ID, a GitHub
 * login and a GitLab username. The engine refuses a human seat with none.
 */
export type HumanContactKey =
  | "slack_user_id"
  | "mattermost_user_id"
  | "atlassian_account_id"
  | "github_login"
  | "gitlab_username";

/** A seat's own integration blocks, as `config.RoleIntegrations` writes them. */
export interface ConfigRoleIntegrations {
  github?: {
    tier?: string;
    repos?: string[];
    app_id?: number;
    app_slug?: string;
    installation_id?: number;
    private_key?: string;
    webhook_secret?: string;
    [key: string]: unknown;
  };
  slack?: { bot_token?: string; signing_secret?: string; channel?: string; [key: string]: unknown };
  mattermost?: { bot_token?: string; username?: string; channel?: string; [key: string]: unknown };
  jira?: { project?: string; [key: string]: unknown };
  confluence?: { space?: string; [key: string]: unknown };
  [key: string]: unknown;
}

/** One unit in the company document, as `config.Unit` writes it. */
export interface ConfigUnit {
  name: string;
  type?: string;
  /**
   * The unit's STABLE IDENTITY, which a name is not — a rename moves
   * everything keyed on the name and nothing keyed on this. Absent means the
   * name IS the key, which is what `org.Unit.Key` falls back to. Guarded, so
   * it is here rather than on [OrgUnit].
   */
  id?: string;
  purpose?: string;
  lead?: string;
  goals?: string[];
  channel?: string;
  knowledge?: string[];
  /** Inherited by the unit's direct AGENT members; a seat's own values win. */
  mcp_env?: Record<string, Record<string, string>>;
  /** The tracker project this unit owns. Vendor-neutral, and guarded. */
  project?: string;
  /** The knowledge container this unit writes pages in. Guarded. */
  space?: string;
  integrations?: { jira?: { project?: string }; confluence?: { space?: string } };
  roles?: ConfigRole[];
  children?: ConfigUnit[];
  schedules?: ScheduleSpec[];
  [key: string]: unknown;
}

/**
 * What a validation problem IS, as a value to branch on. Widened with
 * `string & {}` for the reason every other open enum here is: a newer engine
 * may name a kind this build does not know, and it renders as its message.
 */
export type ConfigProblemKind =
  | "missing"
  | "unknown_value"
  | "out_of_range"
  | "conflict"
  | "unknown_field"
  | "shape"
  | "invalid"
  | (string & {});

/**
 * One located, classified validation failure, as `config.Problem` writes it.
 *
 * `segments` is the same place as `path`, split: a map key in a config path
 * can hold a dot or a hyphen, so a path string cannot be split back into
 * segments reliably, and nothing in this client parses `path` to find out
 * where a problem is.
 */
export interface ConfigProblem {
  /** The authored path in the validated document; "" for a parse failure. */
  path: string;
  /**
   * Strings for keys, numbers for indexes. `null` for a document-level parse
   * failure, whose path is "": Go marshals a nil slice that way, as it does
   * every list in [Derived].
   */
  segments: (string | number)[] | null;
  kind: ConfigProblemKind;
  /** The full line, exactly as the refusal's `detail` carries it. */
  message: string;
  /** The engine-derived handle of the seat it is about. */
  seat?: string;
  unit?: string;
  /** 1-based line in the submitted text, for parse failures only. */
  line?: number;
}

/**
 * Something the engine will run but a person should know: a reference naming
 * nothing, or a rule a stored revision breaks that a new write would be
 * refused for.
 */
export interface ConfigWarning {
  kind: "dangling_reference" | "admission" | (string & {});
  ref: "lead" | "unit" | "manages" | "gitlab_access_level" | "" | (string & {});
  path: string;
  /** As on [ConfigProblem]: `null` when it names no place in the document. */
  segments: (string | number)[] | null;
  seat: string;
  unit: string;
  /** Display text: who or what holds the reference. */
  from: string;
  /** Display text: what it names. */
  to: string;
  message: string;
}

/** `200` from `PUT` or `PATCH /config?dry_run=true`: nothing was stored. */
export interface DryRunResult {
  valid: true;
  /** The revision the draft was validated against. */
  base_revision_id: string;
  warnings: ConfigWarning[] | null;
  derived: Derived;
}

/** `201` from a configuration write that stored and activated a revision. */
export interface WriteResult {
  revision_id: string;
  epoch: number;
  warnings: ConfigWarning[] | null;
  /** The hierarchy the stored document derives to, as `configapi` writes it
   *  beside the revision id on every 201. It was missing from this shape,
   *  which is how a caller that wanted the new chart after a save came to
   *  read it back from a second request the answer already carried. */
  derived: Derived;
}

// ---------------------------------------------------------------------------
// Org, tools, schedules
// ---------------------------------------------------------------------------

/** The `llm:` field of a seat, in every shape `config.PhaseLLM` accepts:
 *
 *      llm: fast                        one provider for every phase
 *      llm: [fast, backup]              a fallback chain for every phase
 *      llm: {default: fast, judge: tiny} a chain per phase
 *
 *  A phase left unset in the mapping form falls back to `default`. */
export type ProviderKeys = string | string[];
export type PhaseLLM =
  | ProviderKeys
  | {
      default?: ProviderKeys;
      review?: ProviderKeys;
      subagent?: ProviderKeys;
      auxiliary?: ProviderKeys;
      judge?: ProviderKeys;
      sandbox?: ProviderKeys;
    };

/**
 * One seat on the ANONYMOUS org projection (`/org`, the snapshot's `org` key
 * and the `org` push), as `internal/api`'s `OrgSeat` writes it.
 *
 * AN EXPLICIT PUBLIC SHAPE, never a marshal of the config's own role. The
 * projection used to be `config.Role` verbatim, so every field a role gained
 * reached anonymous readers the day it landed: contact identities, per-seat
 * credentials, placement labels and sandbox setup commands among them. What
 * is NOT here — `email`, `contact`, `llm`, `token_budget`, `schedules`,
 * `integrations`, `mcp_env` — is read through the operator-gated `config`
 * query instead, as [CompanyDocument].
 *
 * `handle` is what the DOCUMENT declares, and empty where the engine derives
 * one from the name. The handle a seat actually runs under is
 * [DerivedSeat.handle]; this client never derives one of its own.
 */
export interface OrgSeat {
  name: string;
  kind?: string;
  handle?: string;
  goal?: string;
  backstory?: string;
  responsibilities?: string[];
  behavioral_guidelines?: string[];
  manages?: string[];
  availability?: string;
}

/**
 * One unit on the anonymous org projection, as `internal/api`'s `OrgUnit`
 * writes it.
 *
 * `id`, `project`, `space`, `mcp_env` and `schedules` are GUARDED and are not
 * here: `internal/api/orgprojection_test.go` classifies every field of
 * `config.Unit` and fails the build over an unclassified one. A screen that
 * needs one of them reads the company document through the `config` query.
 */
export interface OrgUnit {
  name: string;
  type?: string;
  purpose?: string;
  /** The seat NAME the document declares as lead. Empty when it inherits one. */
  lead?: string;
  goals?: string[];
  channel?: string;
  /** Free-text knowledge references. NOT a read scope. */
  knowledge?: string[];
  roles?: OrgSeat[];
  children?: OrgUnit[];
}

/**
 * The company as the anonymous projection carries it: its charter, its seats
 * and units in the positions the document wrote them, and the hierarchy the
 * ENGINE derived from that document.
 *
 * `derived` is optional because an engine older than the field serves the
 * projection without it. A screen reading one without `derived` has the
 * authored tree and nothing else: it may show what the document says, and it
 * must not reconstruct what the engine would conclude from it.
 */
export interface OrgProjection {
  name?: string;
  mission?: string;
  vision?: string;
  policies?: string[];
  roles?: OrgSeat[];
  units?: OrgUnit[];
  derived?: Derived;
}

/**
 * The hierarchy the engine derived from a company document: every rule this
 * client must not re-implement — handle derivation, root seats moved into a
 * unit by their `unit:` reference, lead and channel inheritance, unit names
 * expanded inside `manages`, automatic management by a unit lead, and which
 * of several managers is primary — resolved once, in Go, by `config.Derive`.
 *
 * Every list here may arrive as `null`: Go marshals a nil slice that way, and
 * a company with no units has a nil unit list. A reader treats `null` as
 * empty.
 */
export interface Derived {
  /** In the engine's own seat order (`Organization.AllRoles`). */
  seats: DerivedSeat[] | null;
  /** Depth first, parents before children: the authored unit tree's order. */
  units: DerivedUnit[] | null;
}

export interface DerivedSeat {
  /**
   * The seat's authored path in the document (`units[0].roles[1]`). Carried by
   * the GUARDED config answers and omitted from the anonymous projection,
   * which hands a reader no document to point into.
   */
  path?: string;
  handle: string;
  name: string;
  kind: string;
  /**
   * The authored path of the seat's effective home unit, "" at the root.
   * Omitted from the anonymous projection with `path`: there, membership is
   * [DerivedUnit.seats].
   */
  unit_path?: string;
  /** A root seat the engine moved into a unit because of its `unit:` reference. */
  placed_by_ref: boolean;
  /** Handle of the primary manager, by the engine's own rule. "" when none. */
  manager: string;
  /** Handles of every seat that manages this one, in engine order. */
  managers: string[] | null;
  /** Handles of this seat's reports after expansion, explicit and automatic. */
  reports: string[] | null;
  /** The subset of `reports` added by lead auto-management. */
  auto_reports: string[] | null;
  /** The unit names whose change makes this seat onboard again. */
  onboarding_chain: string[] | null;
}

export interface DerivedUnit {
  /** Authored path; omitted from the anonymous projection, as on a seat. */
  path?: string;
  name: string;
  /** The EFFECTIVE type, after the engine's default. */
  type: string;
  /** Handle of the effective lead, declared or inherited. "" when it has none. */
  lead: string;
  lead_inherited: boolean;
  channel: string;
  channel_inherited: boolean;
  /** Handles of the direct members, after root seats were attached. */
  seats: string[] | null;
}

/**
 * One entry of a seat's or a unit's `schedules:`, as `org.Schedule` writes it.
 *
 * GUARDED, like everything else a seat's own configuration holds: it reaches
 * this client through the `config` query or through `/schedules`, never
 * through the anonymous org projection.
 */
export interface ScheduleSpec {
  name: string;
  cron: string;
  task: string;
  timezone?: string;
  /** A unit schedule's runner: the lead, or every member. */
  target?: string;
  /** Unset means ENABLED, so this is three-valued rather than a bool. */
  enabled?: boolean | null;
  timeout_seconds?: number;
  catchup?: boolean | null;
}

/**
 * One tool's behavioural hints, as the engine recorded them AT REGISTRATION.
 *
 * EVERY HINT IS A WORD, never a bool, and the third word is the point: "the
 * server did not advertise this" and "the server said no" are different facts,
 * and an absent hint arriving as `false` would read as a positive denial — so
 * a fresh MCP server's unannotated tools would look like proven reads on the
 * one screen an operator audits them on. All three values are truthy, so a
 * careless `if (!ann.read_only)` cannot mean "not read-only" either.
 *
 * A hint this build does not know renders as ITSELF, for the reason the event
 * registry gives: a rolling upgrade puts a newer node's value on an older
 * node's screen.
 */
export type ToolHint = "yes" | "no" | "unknown";

export interface ToolAnnotations {
  read_only: ToolHint;
  destructive: ToolHint;
  idempotent: ToolHint;
  open_world: ToolHint;
}

export interface ToolRow {
  name: string;
  description: string;
  /** `builtin` / `mcp:<server>` / `a2a` — the two-value origin grammar. */
  source: string;
  /** A human-readable name the server advertised, when it advertised one. */
  title?: string;
  annotations: ToolAnnotations;
  /**
   * WHERE calling this puts something in front of somebody outside the turn,
   * empty for a tool that reaches nobody. The registry's own predicate — not
   * "was this served by MCP": a proven read-only MCP tool delivers nowhere,
   * and the native tracker's comment tool delivers although it is a builtin.
   */
  delivers: string;
  /**
   * The JSON Schema the model is offered, ABSENT when the tool takes no
   * arguments. Absent and `{}` are different: only the first means this build
   * did not send one.
   */
  input_schema?: Record<string, unknown>;
}

/** One declared schedule, as `schedule.Row` serialises it.
 *
 *  THE NAMES ARE THE SERVER'S. This type used to declare `scope`, `scope_name`,
 *  `last_run` and `last_outcome`; the server sends `scope_type` and `scope_id`
 *  and has no last-run field at all — the ledger is a separate answer — so
 *  four of its nine fields rendered as `undefined` on every row. */
export interface ScheduleRow {
  /** `role` or `unit`. */
  scope_type: string;
  /** The seat handle or the unit name this schedule is scoped to. */
  scope_id: string;
  name: string;
  cron: string;
  timezone: string;
  task: string;
  /** Empty for a role schedule, where a target is meaningless rather than
   *  defaulted — see `schedule.Row`. */
  target: string;
  enabled: boolean;
  timeout_seconds: number;
  catchup: boolean;
  /** The seats a fire actually reaches, resolved from the scope and target. */
  runners: string[];
  /** Zero-valued when there is no next fire: disabled, an expression that
   *  cannot be parsed, or a date the calendar never reaches. */
  next_run: string;
  /** Why `next_run` is empty when the reason is a DEFECT rather than a choice
   *  — an unparseable cron, an unknown timezone. Empty for a healthy row and
   *  for a merely disabled one, so a blank cell is never the only symptom. */
  problem?: string;
}

/** One fire, from the at-most-once dispatch ledger.
 *
 *  `outcome` has exactly two values — `fired` and `skipped_catchup` — because
 *  this is a DISPATCH ledger and not a turn-outcome one. Nothing here can say
 *  a turn failed; the turn says that. */
export interface ScheduleRunRow {
  scope_type: string;
  scope_id: string;
  schedule_name: string;
  /** The tick this fire stands for, as the ledger's own at-most-once key. */
  fire_label: string;
  target_handle: string;
  scheduled_at: string;
  fired_at: string;
  outcome: "fired" | "skipped_catchup" | "";
  trace_id: string;
}

export interface SchedulesAnswer {
  schedules: ScheduleRow[];
  /** The COMPANY's newest fires, capped across every schedule — so a company
   *  with twenty hourly ones fills this in a couple of hours. One schedule's
   *  own history is `schedule_runs`. */
  recent_runs?: ScheduleRunRow[];
}

/** One schedule's own dispatch history, newest first. */
export interface ScheduleRunsAnswer {
  scope_type: string;
  scope_id: string;
  schedule_name: string;
  runs: ScheduleRunRow[];
  /** The page filled, so older fires are past it. */
  truncated: boolean;
}

// ---------------------------------------------------------------------------
// Health and fleet
// ---------------------------------------------------------------------------

/**
 * What the 5-second `health` push carries — and ONLY that.
 *
 * The richer `api.Health` (node identity, applied epoch, posture, queue,
 * version, whether a company config is even active) is answered by the
 * `stream` query. The two are deliberately different names so a query can
 * never collide with a push kind; they are also deliberately different
 * shapes, and reading a `stream` field off a `health` push is how an engine
 * with NO ACTIVE CONFIG — refusing every inbound webhook — came to render
 * identically to a healthy idle one.
 */
export interface HealthPush {
  status: string;
  /**
   * Both are always on the wire. They stay optional here because the client
   * holds one health value that is NOT a push: the `{status: "unknown"}` it
   * falls back to while the socket is down, which asserts no count at all.
   */
  in_flight?: number;
  shutting_down?: boolean;
}

export interface EngineHealth {
  status: string;
  node?: string;
  configured?: boolean;
  version?: string;
  /**
   * When this node's ENGINE started, which is when the node started: the API
   * is served inside the engine's process, and the fleet view reports the same
   * instant for this node.
   */
  started_at?: string;
  queue?: string;
  clients?: number;
  /**
   * How far back the event log can be read, in SECONDS: the hard bottom of
   * paging, past which every page is empty for ever.
   *
   * `store.EventHistory` is the only thing that decides it, and three screens
   * restated it as literal copy ("the store keeps 30 days") while nothing on
   * the wire carried it — so a change to the retention would have left all
   * three lying with nothing to catch it, and a reader told the wrong floor
   * stops paging early. Seconds rather than days, because a client that
   * re-derives the unit is a second place it can be wrong; the sentence is
   * `eventHistoryLabel` in `lib/format.ts`.
   */
  event_history_seconds?: number;
  in_flight?: number;
  shutting_down?: boolean;
  posture?: string;
  applied_epoch?: number;
  /** The handles THIS node currently holds. */
  seats?: string[];
}

export interface FleetNode {
  id: string;
  roles: string[];
  labels?: Record<string, string> | null;
  /** The lease's fencing token — node id plus a per-process suffix. */
  owner?: string;
  /** The seat-ownership protocol version this node speaks. */
  protocol?: number;
  /** A COUNT, not a list: which seats is `FleetAnswer.seats`. */
  seats: number;
  /** Seconds until this node's own lease expires. */
  expires_in?: number;
  config_epoch?: number;
  config_status?: string;
  config_error?: string;
  /**
   * WHICH revision this node is on, which the epoch does not say.
   *
   * Carried on the apply record for exactly this reason: a fleet view is read
   * while nodes are mid-transition, and a node still on the previous revision
   * is what an operator is looking for. `internal/api/queries/fleet.go` has
   * written it on every row since it was added; this type simply never
   * declared it, so the one screen that compares them read it through a cast.
   */
  config_revision_id?: string;
  config_reported_at?: string;
  /**
   * How far this node's own derived copies have come up — `ready` of `total`.
   *
   * A DIFFERENT QUESTION FROM THE EPOCH: a node can hold the current revision
   * and still be replaying the log the state is derived from, and only one of
   * those makes its seats servable. `internal/coord/nodestatus.go` says where
   * the fact belongs — "where an operator asking why is the new node holding
   * nothing can see it, which is the fleet view" — and it writes TWO integers
   * rather than a bool because "3 of 5" and "0 of 2" are the readings that
   * have to be told apart.
   *
   * ABSENT RATHER THAN ZERO. The node publishes them only once the total is
   * non-zero, so absent is "this node is not saying", never "nothing is
   * ready" — the same rule `in_flight` follows.
   */
  projections_ready?: number;
  projections_total?: number;
  in_flight?: number;
  draining?: boolean;
  started_at?: string;
  posture?: string;
}

/**
 * The state log's retention document, as `crewlet retention status --json` and
 * `GET /work/retention` both serve it.
 *
 * MIRRORS `statelog.Report` FIELD FOR FIELD, deliberately. It is one wire
 * contract with three readers — a CLI, a script and this screen — and a shape
 * restated loosely here would be a fourth idea of what the answer is.
 */
export interface RetentionReport {
  v: number;
  node_id: string;
  at: string;
  backup_owner?: string;
  domains: RetentionDomain[];
  nodes: RetentionNode[];
  /**
   * False means the node block is what could be READ of the fleet rather than
   * the fleet. An empty list is otherwise two different answers — "this fleet
   * has no nodes", which cannot happen, and "coordination could not be
   * listed", which happens during exactly the outage somebody is reading this
   * during.
   */
  register_readable: boolean;
  /**
   * The level the node SERVED this document at, which is what keeps a
   * replication answer inside the read-level contract rather than exempt from
   * it. Nobody chooses it, on any surface: `stale` while the node could
   * measure its own distance from the log, `consistent_prefix` when it could
   * not — because `stale` is a claim about AGE and an unmeasured lag cannot
   * make one. Anything but `stale` is rendered, since a document that cannot
   * claim an age otherwise looks exactly like one that can.
   */
  read_level?: ReadLevel;
  snapshots: RetentionSnapshot[];
  replica: RetentionReplica;
  alarms: RetentionAlarm[];
  /**
   * The capacity operation currently holding the fleet, ABSENT when there is
   * none. Maintenance stops every publisher on every node — no seats, no
   * duties, no scheduler, no write routes — and it was visible on no screen
   * at all: an operator watching a company go quiet had nothing to look at
   * that said why.
   */
  maintenance?: RetentionMaintenance;
}

/** One open capacity operation. */
export interface RetentionMaintenance {
  stream: string;
  operation_id: string;
  /** Where it stands, and which write-then-seal cycle it is on. */
  phase: string;
  attempt: number;
  target_max_bytes: number;
  original_max_bytes: number;
  since: string;
  by?: string;
  /**
   * The nodes whose acknowledgement the seal is still waiting for. EMPTY IS
   * NOT UNKNOWN: an operation with nobody outstanding is one waiting on its
   * operator, which is the state somebody finding this most needs to see.
   */
  participants_missing?: string[];
  /** Why it cannot proceed without a person, empty while it can. */
  blocked?: string;
}

/** One registered domain's row. */
export interface RetentionDomain {
  domain: string;
  stream: string;
  generation: number;
  replay: string;
  first_seq: number;
  last_seq: number;
  bytes: number;
  max_bytes?: number;
  /** ABSENT when the broker could not be asked — which is not zero headroom. */
  headroom_fraction?: number;
  /** What has actually been removed, and what THIS tick concluded may be. */
  trim_floor: number;
  trim_to: number;
  terms: RetentionTerm[];
  blocked_by?: string;
  blocked_since?: string;
  /** The sentence a blocked trim leads with. */
  prose?: string;
  /** The snapshot loop's OWN skip reason — a different problem from a blocked
   *  trim, with a different remedy, which is why it is its own field. */
  snapshot_blocked_by?: string;
}

/**
 * A term's value made explicit: read, unreadable, not applicable, or read and
 * binding nothing.
 *
 * `unbounded` is the one that is not about readability. A term permitting
 * everything — no hold pins the log, a solo fleet takes no snapshots — has a
 * sequence of 2^64-1 inside the engine, which is the identity for the minimum
 * the trim takes and not a position at all. It used to arrive here as `ok`
 * with that number on it, and the screen printed `18446744073709552000` beside
 * the term's own prose saying nothing was pinning anything — not even the
 * right digits, since a JSON number is a float64.
 */
export type RetentionTermState = "ok" | "unknown" | "n/a" | "unbounded";

export interface RetentionTerm {
  name: string;
  state: RetentionTermState;
  /** Meaningless unless `state` is "ok", and zero in every other state. */
  seq?: number;
  detail?: string;
  /** On EVERY term rather than the blocking one: an operator watching a term
   *  approach is the case this surface exists for. */
  remedy: string;
}

export interface RetentionNode {
  node_id: string;
  /** Counted means the trim waits for it; live means it holds a lease right
   *  now. INDEPENDENT: counted-and-not-live is the node pinning the log, and
   *  live-and-not-counted is one inside its eviction fence window. */
  counted: boolean;
  live: boolean;
  /** ABSENT for a node that is live and has never reported, which renders as
   *  "counted, no position yet" rather than as a node at position zero. */
  at?: string;
  domains?: Record<string, RetentionNodeDomain>;
  evicted?: RetentionEviction;
}

export interface RetentionNodeDomain {
  generation: number;
  seq: number;
  /** BESIDE seq, never instead of it: a node applying nothing while its
   *  position advances looks identical to a caught-up one from either alone. */
  applied_through: number;
  /** ABSENT rather than zero when the stream could not be read. */
  lag?: number;
  deferred?: number;
}

export interface RetentionEviction {
  by: string;
  at: string;
  /** When the trim stops counting the node — one fence window after the
   *  gesture. Printed because the gesture is NOT immediate, and an operator
   *  who does not know that reads the unchanged watermark as a failure. */
  effective_at: string;
  effective: boolean;
}

export interface RetentionSnapshot {
  node_id: string;
  /** Absent when the node holds none, in which case `skip` says why. */
  domains?: Record<string, number>;
  at?: string;
  bytes?: number;
  /** The loop's own reason for holding none, which is what turns "node-4
   *  none" into an answer. */
  skip?: string;
}

export interface RetentionReplica {
  store_bytes: number;
  projected_join_seconds: number;
  rejoin_window_seconds: number;
}

export interface RetentionAlarm {
  kind: string;
  detail: string;
  remedy: string;
}

/** What a retention gate answered. The outcome is three-valued (D134). */
export interface RetentionGateResult {
  node: string;
  evicted: boolean;
  /**
   * `applied` is durable AND in this node's rows; `pending` is durable at the
   * position and unapplied HERE, so what it produced is unresolved rather
   * than failed; `unknown` is the only one where retrying is correct.
   */
  outcome: "applied" | "pending" | "unknown";
  position?: { stream?: string; generation?: number; seq?: number };
  op_id?: string;
}

export interface FleetSeatLease {
  handle: string;
  /** The node holding it. `owner` beside it is the fencing token, not an id. */
  node: string;
  owner?: string;
  epoch?: number;
  expires_in?: number;
}

export interface FleetDutyLease {
  duty: string;
  node: string;
  expires_in?: number;
}

export interface FleetAnswer {
  nodes: FleetNode[];
  seats: FleetSeatLease[];
  duties: FleetDutyLease[];
  unplaceable: { handle: string; placement?: string; reason?: string }[];
  unmanned_roles: string[];
  this_node: string;
  /** The epoch every node is meant to converge on. A number, not an id. */
  target_epoch: number;
}

/** One observation a reconcile pass made that is not "fine". */
export interface ReconcileFinding {
  kind: string;
  subject?: string;
  /**
   * What is wrong, in one sentence — and only that.
   *
   * What to do about it is `remedy` and which things it is about is
   * `subjects`. Three fields because a reader wants them in three different
   * moments and one string cannot be laid out: glued, they arrived as one
   * unbroken paragraph carrying a problem, a name and an instruction, with
   * the disclosure summary running straight into its last word.
   */
  detail?: string;
  /** What to do about it, rendered as its own line at a quieter weight. */
  remedy?: string;
  action_url?: string;
  /**
   * What this finding MEANS, from the engine's own per-kind verdict table
   * (integration.FindingKind.Verdict) rather than from a second copy of the
   * closed set kept here.
   *
   * Two kinds are advisory — their phase is `ready`: something the engine did
   * not do and cannot undo, on an integration that is working. A reader that
   * treats every finding as a fault reports those as broken; one that keeps
   * its own list of which kinds are advisory reports a kind it has not heard
   * of as fine, which is worse. Optional because a node older than the field
   * sends neither — treat an absent phase as "cannot say", never as ready.
   */
  phase?: string;
  actor?: string;
  /**
   * Everything this finding is about, when there are many and `subject`
   * cannot name them all.
   *
   * `detail` is the card's status line and the engine caps it, so a finding
   * that listed its subjects inline arrived as a wall cut off mid-item —
   * thirty-six Datadog service accounts ending `…@agents.cr…`. The engine
   * now puts the COUNT in `detail` and the whole list here, so a reader with
   * room lays them out and one without is still correct.
   *
   * It named three of them in the sentence too, which put every short list
   * on screen twice: once as prose and once as the list, a centimetre apart.
   *
   * Absent on the ordinary finding about one thing.
   */
  subjects?: string[];
}

/**
 * What the reconcile loop last found for one surface.
 *
 * The row's `reconcile` is THREE-VALUED like every count beside it: an object
 * is a real finding, and `null` is either a node that could not read the
 * fleet's rows or a surface the loop has not reached yet. Neither is a claim
 * that the surface is healthy.
 */
export interface ReconcileStatus {
  /** unconfigured | awaiting_admin | provisioning | activating | degraded | ready */
  phase: string;
  /**
   * The phase in a reader's words, from [integration.Phase.Label] in Go.
   *
   * Optional because a node older than the field sends none, not because a
   * screen may skip it: derive nothing from `phase` that this can answer.
   */
  phase_label?: string;
  /**
   * Whether a disconnect has been ASKED FOR, which is a fact the phase
   * cannot carry on its own: between the request and the first teardown pass
   * the stored phase is still whatever the last reconcile concluded.
   */
  disconnecting?: boolean;
  /** "" | engine | provider | admin | operator — who has to act. */
  actor?: string;
  detail?: string;
  action_url?: string;
  /** settled | waiting | blocked */
  outcome?: string;
  attempts?: number;
  last_error?: string;
  last_attempt_at?: string | null;
  settled_at?: string | null;
  next_attempt_at?: string | null;
  /** Everything the pass saw, not only what the phase was derived from. */
  findings?: ReconcileFinding[];
}

export interface IntegrationRow {
  key: string;
  label?: string;
  configured: boolean;
  detail?: string;
  /** Deliveries the edge accepted. */
  inbound?: number | null;
  /**
   * The two OUTCOME counts, three-valued: a number, or null when this process
   * could not read its event log. Reporting that as 0 would claim every
   * delivery woke a seat on a node that cannot tell.
   *
   * They are read together with `inbound` and are misleading apart: "128
   * arrived" alone cannot tell a working integration from one whose every
   * delivery reaches nobody, and a seat draining a thread's backlog as one
   * turn looks like a seat that ignored twelve messages.
   */
  skipped?: number | null;
  coalesced?: number | null;
  /** Null means "nothing here can say", never "this surface is fine". */
  reconcile?: ReconcileStatus | null;
  /**
   * The public base URL this surface's registration at the third-party app
   * points at, and whether it is still the one in force.
   *
   * `endpoint_current` is three-valued: null is "nothing has recorded an
   * address for this surface", which is not the same claim as "the address
   * moved". False is a delivery route pointing somewhere that no longer
   * answers, which for a surface no pass converges only a person can fix.
   */
  endpoint?: string | null;
  endpoint_current?: boolean | null;
  [key: string]: unknown;
}

export interface IntegrationsAnswer {
  integrations: IntegrationRow[];
  traffic_known: boolean;
  traffic_since: string;
}

// ---------------------------------------------------------------------------
// Learning, conversations, knowledge
// ---------------------------------------------------------------------------

export interface DiaryEntry {
  id?: string;
  scope?: string;
  retention?: string;
  content: string;
  created_at: string;
  ttl_until?: string;
  tags?: string[];
}

export interface Episode {
  id?: string;
  turn_id?: string;
  agent_handle?: string;
  conversation_key?: string;
  task_summary?: string;
  outcome?: string;
  review_outcome?: string;
  duration_ms?: number;
  created_at: string;
  content?: string;
  compacted?: boolean;
}

export interface SynthesizedSkill {
  id?: string;
  key?: string;
  title: string;
  summary?: string;
  body?: string;
  version?: number;
  updated_at?: string;
  uses?: number;
}

/** Who a counterparty IS, which is two identities and not one string.
 *
 *  A profile's subject is either a seat in this company or an unmapped person
 *  on some surface — a chat account, an issue reporter — and the name is
 *  DISPLAY ONLY: somebody renaming themselves on Slack must not orphan what
 *  this seat learned about them. */
export interface CounterpartySubject {
  handle?: string;
  external_id?: string;
  platform?: string;
  name: string;
}

/** What one seat has learned about one colleague.
 *
 *  THE TWO INSTANTS MEASURE DIFFERENT CADENCES and both are carried, because
 *  the difference is the interesting one: `last_updated_at` moves on every
 *  interaction and `last_corroborated_at` only when the traits actually
 *  changed. A colleague seen daily whose profile has not moved in months is
 *  one this seat has stopped learning about — which is the state the Plan
 *  phase's own prefetch demotes on, so a screen showing one number would
 *  disagree with the prompt. */
export interface CounterpartyProfile {
  subject: CounterpartySubject;
  /** Whether the subject resolved to a seat in this company. */
  resolved: boolean;
  /** A bag whose keys the model invents — never a fixed schema. */
  traits: Record<string, unknown>;
  interactions: number;
  first_seen_at: string;
  last_updated_at: string;
  last_corroborated_at: string;
}

export interface AgentMemoryAnswer {
  id: string;
  diary: DiaryEntry[];
  episodes: Episode[];
  skills: SynthesizedSkill[];
  /** How many the seat HAS, which is not how many `skills` carries: the
   *  listing is cut at the page limit, and this is what says so. ALWAYS
   *  PRESENT — `queries.Sources.agentMemory` seeds it in the answer's
   *  default map, so it is `0` for a seat that has learned nothing rather
   *  than absent. Optional here, a reader had to fall back to
   *  `skills.length`, which is the page size and therefore silently
   *  under-reports exactly the seat this count exists for. */
  skills_total: number;
  counterparties: CounterpartyProfile[];
  onboarded_at: string;
}

/** One recorded turn in one conversation, from the conversation ledger. */
export interface ConversationEntry {
  turn_id?: string;
  at?: string;
  trigger?: string;
  /** What the turn set out to do, in the seat's own words. */
  intent?: string;
  /** PRE-RENDERED tool-call lines, fixed at write time so a later reader
   *  cannot restate history against a surface that has since changed. */
  tool_calls?: string;
  /** `reply` is set ONLY when something actually reached the waiting party,
   *  and `unsent` carries the same artifact when nothing did. Which of the
   *  two holds it is the whole record of whether anybody received it — a
   *  screen that rendered them alike would show work announced to nobody as
   *  announced. */
  reply?: string;
  unsent?: string;
  decision?: string;
  completed_work?: string;
}

export interface ConversationRow {
  /** The surface-scoped conversation identity — a thread, an issue, a
   *  channel. OPAQUE: the notification layer owns its grammar. */
  key: string;
  turns: number;
  last_at: string;
}

export interface ConversationsAnswer {
  handle: string;
  conversations: ConversationRow[];
  entries: ConversationEntry[];
  /** False when this node holds no conversation ledger at all. */
  available: boolean;
}

/** One open or closed agent-to-agent channel. */
export interface A2AChannel {
  id: string;
  requester: string;
  target: string;
  messages: number;
  opened_at: string;
  last_at: string;
  closed_at: string;
}

export interface A2AAnswer {
  channels: A2AChannel[];
  available: boolean;
}

export interface KnowledgeHit {
  id: string;
  title: string;
  url: string;
  container: string;
  snippet: string;
  updated_at: string;
}

/**
 * Why a knowledge search did not run. `""` means it ran — a `note` alongside
 * it describes a search that STARTED and degraded, which is a different fact
 * from one that never started. Widened to `string` because the engine may
 * learn a reason this build does not know, and an unknown one must render as
 * its prose `note` rather than crash the screen.
 */
export type KnowledgeReason =
  | ""
  | "no_company"
  | "no_backend"
  | "no_scope"
  /** The backend keeps a local index and it is still catching up. NOT the
   *  same fact as an empty result: on a freshly joined node this is true for
   *  the whole first index build, and rendering it as "nothing found" tells a
   *  reader the company has written nothing down. */
  | "building"
  | (string & {});

export interface KnowledgeAnswer {
  backend: string;
  query: string;
  hits: KnowledgeHit[];
  /** False when the search did not run at all; `reason` says which state. */
  available: boolean;
  /**
   * The state, as a value to branch on. Never branch on `note` — it is prose
   * for a person and free to be reworded — and never infer the state from
   * `backend`, which is empty for "no backend" and "no company" alike.
   */
  reason: KnowledgeReason;
  /** Search is best effort: a failure is an empty result plus this note. */
  note: string;
}

// ---------------------------------------------------------------------------
// The native tracker and knowledge base
// ---------------------------------------------------------------------------
//
// These are the engine's OWN backends — a company on Jira or Confluence has
// none of these questions registered at all, and the screens say so rather
// than drawing an empty board. Every field here is the Go type's own JSON
// tag; the wire shape is [work.Summary], [work.Detail], [pages.Summary] and
// [pages.Detail], serialised as they stand.

/** The tracker's SIX statuses. `blocked` is deliberately not among them: a
 *  blocker is DATA a badge and a filter read, carried beside the status on
 *  `WorkSummary.blocked`, because a task can be both in progress and blocked
 *  and a single field cannot say so. There is no `close_reason` either —
 *  `cancelled` IS "finished without being delivered", which is what keeps it
 *  invisible to a delivery count with no second value to keep in step. */
export type WorkStatus =
  "todo" | "in_progress" | "in_review" | "done" | "cancelled" | "closed" | (string & {});

/** The four groups every rule is written at. */
export type WorkStatusGroup = "not_started" | "active" | "done" | "closed" | (string & {});

/** OPEN, not an enum: the type catalogue is the workspace's, and a company
 *  filing "incident" is filing something this build has never heard of. */
export type WorkType = string;
export type WorkPriority = "none" | "low" | "normal" | "high" | "urgent" | (string & {});

/** How stale an answer may be, as the engine ACTUALLY served it — never the
 *  level asked for. A level never silently downgrades, so the two can differ
 *  only by a refusal.
 *
 *  THE FOUR THE ENGINE HAS. It listed `monotonic`, which is not one of them
 *  and never arrives, and omitted `consistent_prefix`, which does — so the one
 *  value a screen has to treat specially was the one the type did not name.
 *  `session` is here because the framework has it; no read surface offers it,
 *  since it waits for a position no client can supply.
 *
 *  The open `string` arm stays: a newer build may serve a level this bundle
 *  does not know, and a badge that renders it is better than a type error. */
export type ReadLevel = "linearizable" | "session" | "stale" | "consistent_prefix" | (string & {});

/** One task as a board row draws it. The BODY IS ABSENT — fifty tasks at
 *  64 KiB each is three megabytes to draw a list of titles. */
export interface WorkSummary {
  id: string;
  key: string;
  project: string;
  title: string;
  /** The slug a card is drawn under. On the ROW rather than only on the
   *  document, because a board draws an icon per card and a column per type,
   *  and a row that could be GROUPED BY a value it did not CARRY rendered
   *  every card as the same kind of thing. */
  type: WorkType;
  status: WorkStatus;
  status_group?: WorkStatusGroup;
  priority?: WorkPriority;
  assignee?: string;
  parent?: string;
  depth?: number;
  start?: string;
  due?: string;
  /** The row's OWN copy of the overdue predicate, so a renderer never
   *  re-derives it differently — which is how one screen shows a task as
   *  overdue and another does not. */
  overdue?: boolean;
  estimate_min?: number;
  points?: number;
  /** Data a badge and a filter read. It gates nothing: closing a task with
   *  open blockers is allowed. */
  blocked?: boolean;
  /** What `blocked` is the one-bit answer to: the same edges, carrying WHICH
   *  task holds this one up rather than only that something does.
   *
   *  It is on the ROW because a renderer that draws the RELATION between two
   *  rows cannot derive it from either of them — the alternative is a
   *  single-task read per bar. `blocked` is exactly "some entry here is
   *  `open`", computed from the same rows in the same statement, so a badge
   *  can never appear beside no edges.
   *
   *  A blocker the caller's own filter excluded is an id this page holds no
   *  row for. That is the honest answer, not an omission: the edge exists and
   *  this page cannot draw it. */
  waiting_on?: WorkBlocker[];
  archived?: boolean;
  rank?: string;
  updated: string;
  /** The composed log position this row was last written at. */
  version: number;
}

/** One person's load, as `work_workload` answers it.
 *
 *  It counts OPEN work — every task assigned to them, whatever its dates —
 *  because "who is carrying the most" is a question about a whole queue. */
export interface WorkloadRow {
  handle: string;
  open: number;
  /** Both measures a company may size in, both carried rather than one
   *  chosen: which one is used differs by team, so a company-wide answer
   *  that picked one would be wrong for everybody sizing in the other. */
  points: number;
  estimate_min: number;
  /** The three shapes of "this is not simply work in progress". A person
   *  whose whole queue is blocked has a different problem from one who is
   *  simply busy. */
  blocked: number;
  overdue: number;
  unscheduled: number;
}

export interface WorkloadAnswer {
  rows: WorkloadRow[];
  /** The answer stopped at the handle cap. */
  truncated?: boolean;
  read_level?: ReadLevel;
  log_seq?: number;
  applied_through?: number;
  log_lag?: number;
  complete: boolean;
  incomplete?: WorkIncomplete;
}

/** One dependency edge as the task that waits on it sees it.
 *
 *  THE STATE TRAVELS WITH THE EDGE rather than being looked up per end,
 *  because a renderer holds one page of rows and a blocker is routinely not on
 *  it: without `open` here, drawing a cleared edge differently from a live one
 *  would need a read per blocker — and with the blocker off the page there is
 *  nothing to read it from. */
export interface WorkBlocker {
  /** The blocking task's ID, never its key: an edge is drawn between two rows
   *  on one page and `WorkSummary.id` is what they are matched on. */
  id: string;
  /** Whether the blocker is still holding this task up. `false` is a settled
   *  fact rather than a missing one — the blocker has finished. */
  open?: boolean;
  /** The blocker not listing this task back: the residue of a dependency
   *  gesture whose mirror did not land, which the repair duty is working on. */
  one_sided?: boolean;
  /** The repair duty's DECISION that this edge will never be mirrored — a
   *  different fact from "not mirrored yet", and the one that stops a reader
   *  waiting for it to settle. */
  one_sided_final?: boolean;
}

/** What an answer could NOT account for: records this node holds and cannot
 *  decode, whose scope could intersect the question. */
export interface WorkIncomplete {
  records: number;
  from: { stream: string; generation: number; seq: number };
  /** What is affected. It does NOT say in which direction — that is what
   *  "cannot decode" means. */
  scope: string[];
  /** The record version this node could not read, which is the one number an
   *  operator needs to pick a build. */
  version: number;
}

/** One aggregate over an answer's whole matched set. */
export interface WorkTotal {
  key: string;
  column: string;
  op: string;
  /** ABSENT when no row contributed — which is not zero: "nothing is
   *  estimated" and "everything is estimated at nothing" are different
   *  facts. */
  value?: number;
  /** The instant, for a min or a max over a date column. */
  at?: string;
}

/** One column of a grouped answer.
 *
 *  `count` is over the WHOLE set, never over `rows`: a column of four hundred
 *  tasks says four hundred and carries twenty. */
export interface WorkGroup {
  key: string;
  label?: string;
  count: number;
  rows: WorkSummary[];
  subgroups?: WorkGroup[];
  /** Lanes this column has beyond the cap, said rather than silently cut —
   *  the same rule `groups_dropped` follows for the columns themselves. A
   *  swimlane board is bounded by its CELLS: the statement count is the
   *  product of the two axes, so the column cap drops when lanes are asked
   *  for. */
  subgroups_dropped?: number;
}

export interface WorkItemsAnswer {
  items: WorkSummary[];
  /** The board's columns. `items` is EMPTY whenever this is set — returning
   *  both would be the same rows twice. */
  groups?: WorkGroup[];
  /** Columns that did not fit the cap. A board that drew sixty-four of two
   *  hundred and said nothing would look like a company with sixty-four
   *  assignees. */
  groups_dropped?: number;
  /** True on an axis where one task is on several columns — a label board —
   *  so a reader knows the counts do not sum to `total_hint`. */
  groups_overlap?: boolean;
  totals?: WorkTotal[];
  /** What this answer was expanded from, echoed so a payload that arrives
   *  detached from its request can still say which saved view it is. */
  view?: string;
  preset?: string;
  /** Capped by construction: an exact total over an unbounded set is the one
   *  query in this grammar that turns a poll into a scan. */
  total_hint: number;
  /** True when the count STOPPED at the ceiling rather than reaching the end.
   *  Read this rather than comparing `total_hint` against a threshold of your
   *  own: that is a second copy of the engine's ceiling, and it is wrong at
   *  exactly one value — a set of exactly ten thousand is exact. */
  total_capped?: boolean;
  next_cursor?: string;
  read_level?: ReadLevel;
  log_seq?: number;
  applied_through?: number;
  /** ABSENT rather than zero when the broker could not be reached: a read
   *  asks how far behind an answer may be, and an unreachable broker answers
   *  "not at all". */
  log_lag?: number;
  /** False means rows may be missing, rows that should have left may still be
   *  present, and the totals were computed over the incomplete set. A screen
   *  that swallows this is worse than a stale tile: staleness and coverage
   *  are different facts, and `read_level` speaks only to the first. */
  complete: boolean;
  incomplete?: WorkIncomplete;
}

/** One entry in a container's view strip.
 *
 *  A BUILTIN ROW HAS NO ID. Every container has a list, a board, a calendar and
 *  a timeline without anybody saving one — they are not objects, so there is
 *  nothing to rename, protect, rank or pin — and a screen renders them from
 *  `key`. */
/**
 * The renderings a view may be drawn in, and there are no others.
 *
 * HELD AGAINST THE ENGINE'S OWN CLOSED SET by a Go gate —
 * `internal/tracker/viewshape_client_test.go` — because this is a copy the
 * dashboard has to keep: it is a separate build in a separate language and
 * cannot import `tracker.ViewTypes`. A shape the engine mints that this union
 * does not name is a tab that renders a blank body, and one named here the
 * engine refuses is a branch nothing can reach. Both are silent.
 *
 * `timeline` draws the rows against a DATE AXIS, which is the one arrangement
 * the others cannot express: a list orders by a column, a board groups by one,
 * and a calendar puts a task on the day it is due — none of them can show that
 * a task spans three weeks, or that it cannot start until another finishes.
 * `table` puts one field per column, which is what a question about a FIELD
 * rather than about a task needs.
 *
 * THE TRASH IS NOT ONE OF THESE. It is a table carrying `removed=true`,
 * because what makes a listing the trash is the query rather than the drawing
 * — so every view saved with that parameter is one.
 */
export type WorkViewShape = "list" | "board" | "calendar" | "timeline" | "table";

export interface WorkView {
  id?: string;
  key: string;
  name: string;
  type: WorkViewShape;
  container: { kind: string; id: string };
  builtin: boolean;
  /** Empty is a SHARED view; a handle makes it personal to that person. */
  owner?: string;
  protected?: boolean;
  /** The container's landing tab, and at most one row carries it. */
  default?: boolean;
  /** THIS VIEWER's, never the row's: the same view is pinned for one reader
   *  and not for another. */
  pinned?: boolean;
  rank?: string;
  icon?: string;
  /** The saved query, in `work_items`' own parameter names. */
  params?: Record<string, string>;
}

export interface WorkViewsAnswer {
  views: WorkView[];
  read_level?: ReadLevel;
  log_seq?: number;
  applied_through?: number;
  log_lag?: number;
  complete: boolean;
  incomplete?: WorkIncomplete;
}

/** A project's chart-owned unit, as a reader renders it.
 *
 *  `resolved` IS A FIELD rather than an absence, because "this project names a
 *  unit the chart no longer has" is a finding — and an absent unit would be
 *  indistinguishable from a project that names none. */
export interface WorkUnitRef {
  key?: string;
  name?: string;
  resolved: boolean;
}

export interface WorkLeadRef {
  handle?: string;
  kind?: "agent" | "human" | "operator" | "system";
}

/** A project's task census.
 *
 *  MAINTAINED by the applier on every status-group change and every arrival or
 *  departure, never aggregated per poll — which is what makes a sixty-second
 *  refresh three column reads rather than a scan of every task in the
 *  company. */
export interface WorkTaskCounts {
  open: number;
  done: number;
  closed: number;
}

export interface WorkProjectRow {
  key: string;
  name: string;
  purpose?: string;
  unit: WorkUnitRef;
  lead: WorkLeadRef;
  default_assignee?: string;
  task_counts: WorkTaskCounts;
  archived?: boolean;
  version: number;
}

export interface WorkProjectsAnswer {
  projects: WorkProjectRow[];
  total: number;
  truncated?: boolean;
  read_level?: ReadLevel;
  log_seq?: number;
  applied_through?: number;
  log_lag?: number;
  complete: boolean;
  incomplete?: WorkIncomplete;
}

/** One label in a project's tag set. */
export interface WorkProjectTag {
  slug: string;
  label: string;
  color?: string;
  description?: string;
  archived?: boolean;
}

export interface WorkFieldGroup {
  applies_to?: string;
  fields: WorkFieldDef[];
}

export interface WorkStatusDef {
  status: WorkStatus;
  label: string;
  group: string;
  description: string;
}

export interface WorkProjectDetail extends WorkProjectRow {
  statuses: WorkStatusDef[];
  types: WorkTypeDef[];
  fields: WorkFieldGroup[];
  /** The workspace field ids this project redeclares — the state a field in
   *  the middle of a move between scopes is in. */
  shadowed?: string[];
  tags?: WorkProjectTag[];
  policy_stamp: number;
  read_level?: ReadLevel;
  log_seq?: number;
  applied_through?: number;
  log_lag?: number;
  complete: boolean;
  incomplete?: WorkIncomplete;
}

/** One measurable outcome under a goal. */
export interface WorkGoalTarget {
  id: string;
  name: string;
  type: "tasks" | "number" | "percent" | "binary";
  start?: number;
  goal?: number;
  current?: number;
  unit?: string;
  tasks?: string[];
  projects?: string[];
  done?: boolean;
  /** 0..1, ABSENT when the target measures nothing — a `tasks` target whose
   *  tasks were all purged, or a numeric one that starts where it ends.
   *  Rendering that as 0% is a goal somebody escalates. */
  progress?: number;
  finished_tasks?: number;
  total_tasks?: number;
}

/** A goal, with what its targets say.
 *
 *  THE PROGRESS IS COMPUTED ON EVERY READ and stored nowhere. A goal is at
 *  what its targets are at; a stored number would be a second answer that
 *  drifts the moment a task closes without anybody editing the goal. */
export interface WorkGoal {
  id: string;
  name: string;
  description?: string;
  owners: string[];
  members?: string[];
  group?: string;
  health?: "on_track" | "at_risk" | "off_track" | "done" | "";
  start_at?: string;
  due_at?: string;
  archived?: boolean;
  targets?: WorkGoalTarget[];
  /** ABSENT when the goal has no targets — "nothing has happened" and "there
   *  is nothing to measure" are different facts. */
  progress?: number;
  version: number;
  created_by?: string;
  created_at: string;
  updated_at: string;
  /** The health check-in history, newest LAST as it was written. The only part
   *  of a goal a person writes in prose, served by `work_goals` since it
   *  existed and declared by nothing until now. */
  updates?: WorkGoalUpdate[];
}

/** One check-in on a goal: who, when, the health they declared, and why. */
export interface WorkGoalUpdate {
  at: string;
  author: string;
  health?: "on_track" | "at_risk" | "off_track" | "done" | "";
  text?: string;
}

export interface WorkGoalsAnswer {
  goals: WorkGoal[];
  read_level?: ReadLevel;
  log_seq?: number;
  applied_through?: number;
  log_lag?: number;
  complete: boolean;
  incomplete?: WorkIncomplete;
}

/** One task type a company may file under — the DECLARATION, where
 *  [WorkType] is the slug a task carries. */
export interface WorkTypeDef {
  slug: string;
  name: string;
  plural?: string;
  icon?: string;
  description?: string;
  /** True for a slug this build ships. It survives a company renaming the
   *  type, because what it says is that the ENGINE knows the slug. */
  builtin?: boolean;
  archived?: boolean;
}

/** One custom-field declaration. */
/** One choice on a `dropdown`, `labels` or `relationship` field.
 *
 *  A VALUE STORES THE ID, so rendering one means looking its name up here —
 *  which is the whole reason the declaration travels beside the value. */
export interface WorkFieldOption {
  id: string;
  slug: string;
  name: string;
  color?: string;
  order?: number;
  archived?: boolean;
}

/** What a field's type lets it be configured with. */
export interface WorkFieldConfig {
  options?: WorkFieldOption[];
  unit?: string;
  precision?: number;
  min?: number;
  max?: number;
  /** A `date` field that holds a time of day as well as a day. */
  time?: boolean;
  progress?: string;
  multi?: boolean;
}

export interface WorkFieldDef {
  id: string;
  slug: string;
  name: string;
  description?: string;
  type: string;
  config?: WorkFieldConfig;
  applies_to?: string[];
  required?: boolean;
  required_in_subtasks?: boolean;
  /** ONE-WAY: the values left the value table, so restoring means a new
   *  field with a new id. */
  archived?: boolean;
  pinned?: boolean;
  hide_from_agents?: boolean;
}

export interface WorkCatalogueAnswer {
  /** The EFFECTIVE set: what this build ships plus what the company declared,
   *  a declaration replacing a builtin of the same slug. */
  types: WorkTypeDef[];
  /** The WORKSPACE's declarations. A project declares its own beside them. */
  fields: WorkFieldDef[];
  policy_version: number;
  types_version: number;
  fields_version: number;
  read_level?: ReadLevel;
  log_seq?: number;
  applied_through?: number;
  log_lag?: number;
  complete: boolean;
  incomplete?: WorkIncomplete;
}

/** One entry in a person's inbox, with the log position it was at. */
export interface WorkInboxEntry {
  record_id: string;
  position: number;
  /** On a snooze, when it comes back. */
  until?: string;
}

/** One human's own state.
 *
 *  `held` is false for somebody nobody has written yet, which is an EMPTY
 *  state rather than a missing one: every human starts with no inbox, no pins
 *  and no priorities, and the first write is what creates the record. */
export interface WorkPersonState {
  handle: string;
  unread?: WorkInboxEntry[];
  read?: WorkInboxEntry[];
  snoozed?: WorkInboxEntry[];
  /** Snoozes whose time has come. REPORTED, never promoted — putting one back
   *  is a write, and a read that performed one would change fleet state. */
  due?: WorkInboxEntry[];
  primary_reasons?: string[];
  priorities?: string[];
  pinned_views?: string[];
  favorites?: { kind: string; id: string }[];
  /** Who last set the queue when it was not this person, and empty when it
   *  was theirs. Their own next change clears it. */
  priorities_set_by?: string;
  priorities_set_at?: string;
  seen_through?: { stream: string; generation: number; seq: number };
  version: number;
  held: boolean;
  read_level?: ReadLevel;
  log_seq?: number;
  applied_through?: number;
  log_lag?: number;
  complete: boolean;
  incomplete?: WorkIncomplete;
}

export interface WorkComment {
  id: string;
  task: string;
  author: string;
  author_kind?: string;
  body: string;
  reply_to?: string;
  /** A colleague this comment is a QUESTION to, who owes it an answer — set
   *  only at creation, because turning an old remark into a question would
   *  wake somebody for a conversation that has moved on. */
  ask?: string;
  /** The comment id this one answers, which is what closes that question. */
  answers?: string;
  mentions?: string[];
  resolved?: boolean;
  resolved_by?: string;
  resolved_at?: string;
  removed?: boolean;
  created_at: string;
  updated_at?: string;
}

export interface WorkChange {
  id: string;
  kind: string;
  actor?: string;
  actor_kind?: string;
  operator_id?: string;
  comment_id?: string;
  excerpt?: string;
  turn_id?: string;
  fields?: Record<string, unknown>;
  /** A commit that woke nobody — a fact about the change rather than about
   *  its importance, since a bulk edit is quiet by construction. */
  quiet?: boolean;
  /** The EFFECTIVE instant: the fleet-agreed one rather than the writer's own
   *  clock, so two nodes render one feed in one order. */
  at: string;
  /** Where the change sits on the log, which is the cursor a feed pages on.
   *  A position on one stream, so it is never compared with one from
   *  another. */
  log_seq: number;
}

export interface WorkLink {
  kind: string;
  other: string;
  key?: string;
  title?: string;
  status?: WorkStatus;
  note?: string;
  /** The half nobody authored. A UI renders it differently, and an editor
   *  knows which end to change. */
  derived?: boolean;
  /** An edge whose mirror was never written, and one whose mirror was
   *  refused permanently. The first is a repair a duty retries; the second is
   *  an attention flag a person resolves. */
  one_sided?: boolean;
  one_sided_final?: boolean;
}

/** One task WHOLE, as the item screen draws it.
 *
 *  NOT an extension of WorkSummary, deliberately. A board row and a task are
 *  two different shapes and six of the row's fields do not exist on the wire
 *  here: `blocked` and `overdue` are DERIVED per row and live on the answer
 *  (see WorkItemDetail.blocked), and the row's `updated`, `start`, `due` and
 *  `estimate_min` are spelled `updated_at`, `start_at`, `due_at` and
 *  `estimate_minutes` on a task. Inheriting them made the compiler promise
 *  fields the server never sends — which is how a Blocked badge that renders
 *  on the board silently never renders on the item it links to. */
/** Who removed a task, when, and what removed it alongside.
 *
 *  A REMOVAL IS REVERSIBLE AT ANY AGE — `restore_work_item` takes it back and
 *  there is no window — so this is a state the task is in rather than the end
 *  of its record. `removed_with` names the parent whose removal took this one
 *  with it, which is what tells a task somebody deleted from one that went
 *  with its container. */
export interface WorkTombstone {
  by: string;
  /** `agent`, `human` or `operator`, as every other authored row on this
   *  wire spells it — a plain string, because an unknown value off a newer
   *  build must decode rather than throw. */
  kind?: string;
  at: string;
  removed_with?: string;
}

export interface WorkItem {
  id: string;
  key: string;
  project: string;
  /** The unit this was FILED into, immutable and a record of what was true;
   *  routing_unit is the mutable half — whose lead hears about it now. */
  filed_unit?: string;
  routing_unit?: string;
  parent?: string;
  depth?: number;
  type?: WorkType;
  title: string;
  body?: string;
  body_version?: number;
  status: WorkStatus;
  status_group?: WorkStatusGroup;
  priority?: WorkPriority;
  rank?: string;
  reporter?: string;
  assignee?: string;
  collaborators?: string[];
  /** The set, and `muted` the subtraction: "not a watcher" and "watching but
   *  muted" are different facts and both travel. */
  watchers?: string[];
  muted?: string[];
  tags?: string[];
  start_at?: string;
  due_at?: string;
  due_all_day?: boolean;
  estimate_minutes?: number;
  points?: number;
  archived?: boolean;
  /** PRESENT ONLY WHEN THE TASK IS IN THE TRASH. The detail read does not
   *  filter removed tasks, so a removed task's page resolves and answers
   *  exactly like a live one's — this field is what tells the screen which it
   *  is looking at. NOT on the row: the listing's SELECT does not read the
   *  tombstone, so a summary never carries one. */
  removed?: WorkTombstone;
  /** The item's own hand-off budget, spent by an agent reassigning it and
   *  reset by any human touch. Past its cap the engine refuses the next
   *  hand-off rather than letting the item circle. */
  reassignments?: number;
  /** Keys this task used to answer to — a project rename or a merge leaves
   *  them, and every one still resolves, which is why they are worth
   *  showing beside the current one. */
  former_keys?: string[];
  /** What this task blocks: the MIRRORED half of a dependency, carried so a
   *  close can say who it unblocks without scanning the company. */
  dependents?: string[];
  checklists?: WorkChecklist[];
  spend?: WorkSpend;
  body_author?: string;
  body_at?: string;
  /** When the task entered the status it is in — the EFFECTIVE instant, so
   *  two nodes compute one duration. */
  status_entered_at?: string;
  done_at?: string;
  closed_at?: string;
  created_at?: string;
  updated_at?: string;
  /** The composed log position this task was last written at. */
  version: number;
}

/** One sub-item of a checklist. It mints no object and appears on no board. */
export interface WorkChecklistItem {
  id: string;
  name: string;
  done?: boolean;
  assignee?: string;
  parent?: string;
  order?: number;
  /** The subtask this item BECAME, which renders it struck through with the
   *  new key rather than deleted. */
  promoted_to?: string;
}

export interface WorkChecklist {
  id: string;
  name: string;
  items?: WorkChecklistItem[];
}

/** What a task has cost, in turns rather than in a seat's month.
 *
 *  A FUNCTION of the applied records rather than a separately transmitted
 *  number, which is what makes it impossible for it to disagree with the
 *  turns it summarises. */
export interface WorkSpend {
  turns?: number;
  rounds?: number;
  input?: number;
  output?: number;
  cache_read?: number;
  cache_write?: number;
  wall_ms?: number;
  tokens?: number;
}

/** One custom-field value with the declaration that explains it.
 *
 *  The three marks are states a filter already excludes, and each is a
 *  different fact: `hidden` is a value whose declaration was ARCHIVED,
 *  `foreign` is one mirrored in from another tracker, and `undeclared` is one
 *  this company explains nowhere. A panel that rendered all three as ordinary
 *  fields would invite somebody to filter on a field that cannot be filtered. */
export interface WorkFieldValue {
  slug?: string;
  name?: string;
  id: string;
  type?: string;
  value: unknown;
  hidden?: boolean;
  foreign?: boolean;
  undeclared?: boolean;
}

export interface WorkItemDetail {
  task: WorkItem;
  comments?: WorkComment[];
  history?: WorkChange[];
  links?: WorkLink[];
  /** The task's custom-field values, ANNOTATED — see [WorkFieldValue]. The
   *  raw map stays on the task; this is the reader's view of it. */
  fields?: WorkFieldValue[];
  /** Pages the thread backwards, and is empty when this page is all of it. */
  comments_cursor?: string;
  /** The SAME predicate WorkSummary.blocked carries — an open dependency edge
   *  — computed by the server in the same transaction as the task, so the
   *  badge here and the badge on the board row cannot disagree. It is on the
   *  ANSWER rather than on the task because it is derived rather than stored:
   *  `links` say what the relations are, not whether any blocker is open. */
  blocked?: boolean;
  read_level?: ReadLevel;
  log_seq?: number;
  applied_through?: number;
  complete: boolean;
  incomplete?: WorkIncomplete;
}

export type PageStatus = "published" | "draft" | "trashed" | (string & {});

export interface PageSummary {
  id: string;
  container: string;
  parent_id?: string;
  title: string;
  status: PageStatus;
  author?: string;
  version: number;
  /** A tool-skill page: machinery the sync publishes, not prose somebody
   *  wrote to be read. */
  skill?: boolean;
  onboarding?: boolean;
  labels?: string[];
  updated_at: string;
  revision: number;
}

export interface PagesAnswer {
  pages: PageSummary[];
  limit: number;
  offset: number;
  /** THE COVERAGE HALF, which `Sources.pageList` returns and this type dropped
   *  — so a page list served far behind the log was pixel-identical to a
   *  complete one. Same envelope as every other state-log answer. */
  read_level?: ReadLevel;
  position?: number;
  log_lag?: number;
  complete?: boolean;
}

export interface PageContainer {
  key: string;
  name?: string;
  purpose?: string;
  created_at?: string;
  /** How many pages this container holds, TRASHED ONES EXCLUDED — so the
   *  number on the rail and the list behind it agree. DERIVED beside the
   *  container rather than stored on it: the document is what a writer wrote,
   *  and a count on it would have every page create rewrite its container. */
  pages: number;
}

export interface PageComment {
  id: string;
  page_id: string;
  author: string;
  author_kind?: string;
  body: string;
  mentions?: string[];
  created_at: string;
  edited_at?: string;
}

/** One past version, METADATA ONLY: the projection keeps no bodies, and
 *  reading one is a coordination read on demand. */
export interface PageRevision {
  version: number;
  author?: string;
  message?: string;
  created_at: string;
}

export interface Page extends PageSummary {
  body?: string;
  watchers?: string[];
  created_at?: string;
}

/** One change to a page, from the wiki's own history table. */
export interface PageChange {
  id: string;
  page_id: string;
  /** The page's CURRENT title and container, resolved by the read: a feed of
   *  uuids is a feed nobody reads. Empty for a page that has been purged, whose
   *  entries are the record it ever existed. */
  title?: string;
  container?: string;
  kind: string;
  actor?: string;
  actor_kind?: string;
  operator_id?: string;
  comment_id?: string;
  excerpt?: string;
  /** The turn that made this change — what a wiki cannot have, and what makes
   *  "why did this page change" one click. */
  turn_id?: string;
  /** A change that announced nothing. A fact about the change rather than its
   *  importance: a label edit is quiet by construction. */
  quiet?: boolean;
  at: string;
  log_seq: number;
}

export interface PageActivityAnswer {
  changes: PageChange[];
  next_cursor?: string;
  read_level?: ReadLevel;
  complete?: boolean;
}

/** One saved version of a page, BODY INCLUDED — which is what the revision
 *  summaries on the detail could say existed and never show. */
export interface PageRevisionBody {
  page_id: string;
  version: number;
  title: string;
  body: string;
  message?: string;
  author?: string;
  created_at: string;
}

export interface PageDetail {
  page: Page;
  revision: number;
  comments?: PageComment[];
  history?: PageRevision[];
  children?: PageSummary[];
  /** The parent chain, outermost first — the breadcrumb. */
  ancestors?: PageSummary[];
}

// ---------------------------------------------------------------------------
// Turn
// ---------------------------------------------------------------------------

/** A page of the company's phase records, payloads included. */
export interface PhasesPage {
  phases: EventRecord[];
  /** Pass back verbatim to page further. Empty at the end of the record. */
  next: { before_time?: string; before_id?: string };
  exhausted: boolean;
}

/** Every stored event of one RUN of a turn, ordered oldest first. */
export interface TurnAnswer {
  turn_id: string;
  /**
   * The unit of work this run was an attempt at, and every run of it the
   * store holds, OLDEST FIRST.
   *
   * A turn id names one execution (see `adr/0017`), so a trigger that failed
   * without reaching outside the engine and was redelivered is several turns
   * — and this screen is where every deep link in the product lands. One
   * element (this turn) is the ordinary case; an empty `work_key` means the
   * trigger had no key to collapse on, and `attempts` is then empty too.
   */
  work_key?: string;
  attempts?: TurnRow[];
  events: EventRecord[];
  /**
   * True when the store stopped at its per-turn cap rather than at the end of
   * the turn.
   *
   * A turn is read OLDEST FIRST, so the rows a cut loses are its ENDING — the
   * two records the screen reads its outcome, its wall clock and its plan
   * summary off. Without this, a truncated turn is indistinguishable from one
   * that never finished: the header said "no turn record" directly above the
   * rows it did get, and captioned an event span as the turn's own
   * measurement.
   */
  truncated: boolean;
  /**
   * EVERY TRACE THIS TURN TOUCHED, as the store itself indexed them.
   *
   * The screen can derive a list from the rows it holds, and does — but only
   * from the rows it HOLDS. A turn read to the per-turn cap is missing its
   * middle, and a turn whose events fell out of the retention window is
   * missing most of itself, so a derived list silently loses whichever traces
   * lived in the gap. `internal/api/queries/insight.go` seeks these
   * separately for that reason and DEGRADES to an empty list rather than
   * failing the read, so an absent or empty value means "the seek did not
   * answer", never "this turn touched no trace".
   */
  trace_ids?: string[];
}

/** A seat's phase history, newest first, with a cursor. */
export interface AgentAnswer {
  role: string;
  live: Overlay | null;
  llm_history: EventRecord[];
  /** Pass back as `before` to page further into the record. */
  next: string;
}

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

/**
 * One revision, as `configapi.meta` writes it.
 *
 * The field names are the SERVER's. They were `id`, `author` and `active`
 * here, which no answer has ever carried: the screen read `undefined` for
 * every one of them and threw on the first `.slice(10)`. A shape declared
 * from memory rather than from the emitter is a screen that renders once and
 * then never again.
 */
export interface RevisionMeta {
  revision_id: string;
  parent_revision_id?: string;
  summary: string;
  source: string;
  created_by: string;
  created_at: string;
  activated_at?: string;
  is_active?: boolean;
}

/**
 * One revision with its document, as `GET /config/revisions/{id}` answers:
 * the metadata of [RevisionMeta] beside a REDACTED `payload`.
 *
 * The builder reads it to settle a write whose answer never arrived: the
 * write landed exactly when the revision now active names the draft's base
 * as its parent and its summary carries the write id the save sent.
 */
export interface ConfigRevision extends RevisionMeta {
  payload: CompanyDocument;
}

/** One difference, as `configapi.Change` writes it: `kind`, not `op`. */
export interface ConfigChange {
  path: string;
  kind: "added" | "removed" | "changed";
  from?: unknown;
  to?: unknown;
}

export interface ConfigDiff {
  from: string;
  to: string;
  changes: ConfigChange[];
  /** How many differences there ARE, which is not how many `changes`
   *  carries: the answer is cut at the server's response budget, and this is
   *  what says so. `crewlet config diff` has no such budget and prints every
   *  one. Render this as the count and say what the listing left out.
   *  ALWAYS PRESENT — `configapi.Service.Diff` writes it on every answer, so
   *  identical revisions report `0` rather than omitting it. Optional here,
   *  a reader had to fall back to `changes.length`, which is the response
   *  budget and therefore reads a cut diff as the whole comparison. */
  changes_total: number;
}

// ---------------------------------------------------------------------------
// Setup
// ---------------------------------------------------------------------------

/**
 * One input an integration cannot work without, as `/setup` answers it.
 *
 * The screen renders a form from these and knows nothing about any
 * third-party app: every word a person reads is carried here, so adding one
 * adds no branch to a component. See internal/setup for what each field
 * means.
 */
export interface SetupRequirement {
  field: string;
  label: string;
  kind: "secret" | "url" | "id" | "choice" | "text" | "handle" | "toggle";
  config_path: string;
  secret_name?: string;
  required: boolean;
  /**
   * Shown and required only when another field holds a given value.
   *
   * A FIELD REQUIRED BY AN ANSWER rather than in general. GitHub's
   * organization token is the pair it exists for: it is the one thing
   * standing between "every repository in the organization" and that being
   * true, and a credential the other answer never uses. `required` alone
   * blocked a connect that needed nothing; optional let a choice be stored
   * that could not be carried out.
   *
   * Evaluated against what the FORM holds, not what is stored: the gating
   * field is being answered in the same dialog, so the stored value is the
   * one being replaced. The API checks the same condition against the
   * submission, so a caller that skips this form is refused too.
   */
  required_when?: { field: string; equals: string };
  /**
   * One of the few fields that ESTABLISH the connection.
   *
   * An ORDER, not a filter: these lead the form, with a rule under them and
   * everything that configures what happens OVER the connection below. Both
   * halves are one form, so an app's settings are the screen its operator
   * already saw when they connected it.
   */
  connect?: boolean;
  /**
   * One value across every surface of this tool, asked once and written to
   * all of them.
   *
   * Atlassian's account email, API token, cloud id and link address: two
   * config blocks, one Atlassian account. Not "same field name" — Jira's url
   * and Confluence's url are both `url` and are different addresses.
   */
  shared?: boolean;
  /** The words in `help` that become the link to `vendor_url`. */
  link_text?: string;
  /**
   * What the form offers when this company has no value.
   *
   * A SUGGESTION, not a stored value: nothing is written until submit, so a
   * default that is wrong is one somebody changes rather than one they have
   * to discover.
   */
  default?: string;
  /**
   * What a `${VAR}` in this field currently reads as, for a field that is
   * NOT a credential.
   *
   * A reference is a name, and the links a form draws are built out of
   * VALUES: the Atlassian API keys page is per-organization, so once that id
   * lived in the sealed store the link was built out of the literal text
   * `${ATLASSIAN_ORG_ID}`. Absent on a credential, on a literal (which is
   * already the value) and on a reference naming nothing.
   */
  resolved_value?: string;
  mintable?: boolean;
  /**
   * Written without being asked for.
   *
   * The `enabled` toggle on every inbound app: connecting an integration and
   * leaving it switched off is not a thing anybody means, so the question
   * was a control whose only sensible answer was the one it already had. It
   * is still submitted, from its default.
   */
  hidden?: boolean;
  help?: string;
  where?: string;
  vendor_url?: string;
  choices?: { value: string; label: string; hint?: string }[];
  format?: string;
  /**
   * The reconcile finding this input being absent produces.
   *
   * ON THE WIRE AND NOT READ BY THIS SCREEN, deliberately. It is the server's
   * join — setupapi's own suite checks that every vendor has a credential
   * field claiming `credential_missing`, so a row asking for a credential
   * cannot offer every field except the credential — and the dialog used to
   * narrow to it when a Fix control existed. It does not any more: a card that
   * needs attention says so in its tag, and the gear opens the same settings
   * rather than a narrowed copy (see `actionFor` in Integrations.tsx).
   *
   * Declared because it is part of the shape the API sends, not because
   * anything here consumes it.
   */
  blocks?: string;
  /**
   * What the document holds here right now, for everything that is NOT a
   * credential: the region, the URL, the handle, the group.
   *
   * It is what the form opens on, so a settings dialog is an edit of a
   * configuration rather than a blank form over one. Absent on a secret, and
   * deliberately: a credential's value has no path onto this wire at all
   * (internal/setup: Requirement.MarshalJSON).
   */
  value?: string;
  /** The document names something here: a literal or a ${VAR}. */
  present: boolean;
  /** Null for a field the document leaves empty: there is nothing to resolve. */
  resolved?: boolean | null;
  seat?: string;
}

export interface SetupToolState {
  key: string;
  configured: boolean;
  enabled: boolean;
  requirements: SetupRequirement[];
  /**
   * One sentence on what connecting this app DOES, from the engine.
   *
   * The connect form opens with it. It lives in Go with the app whose
   * requirements it introduces, for the reason every other word on that form
   * does: this screen knows nothing about any app.
   */
  summary?: string;
  /** Nothing REQUIRED is outstanding. Not a health claim. */
  satisfied: boolean;
  /**
   * Whether every box this app's dialog draws has been answered: the company
   * block's requirements, and every seat's.
   *
   * SEPARATE FROM `satisfied`, and the gap is the whole of what a button can
   * usefully offer. A GitHub company whose agent has no app yet is not
   * satisfied and has nothing left to type: both acts that produce an app
   * happen at GitHub, from that agent's own row.
   */
  form_complete?: boolean;
  inbound_path?: string;
  public_url?: string;
  /** This build runs a provisioning pass for this vendor. */
  can_provision?: boolean;
  /**
   * Whether a seat without its own credential makes this app unfinished.
   *
   * Slack and GitHub: an agent with no Slack app cannot post and an agent
   * with no GitHub App acts as nobody, so the roster IS the integration on
   * both. Everywhere else the roster is informational, and reading it as work
   * outstanding puts a Continue button on a working card.
   */
  seats_required?: boolean;
  /**
   * What a person clicks at the third-party app to delete ONE agent's app,
   * once a seat's `manage_url` has opened it. The same for every seat, so it
   * is stated once. Empty where the engine removes what it made on its own.
   */
  manage_path?: string;
  /** The transient third-party app credential its pass asks for, never stored. */
  needs_operator?: SetupRequirement | null;
  /**
   * Per-seat setup, for a third-party app whose credentials live on the
   * seat rather than on the company. Slack is the one: each agent has its
   * own app.
   */
  seats?: SetupSeatState[];
}

/**
 * What one agent still needs a person to do before it can act as itself.
 *
 * A CLOSED SET, so a screen renders the control from the value rather than
 * from the sentence in `detail`: an app this agent has none of, and an app it
 * has that nothing has installed, are two acts by a person minutes or days
 * apart, and an operator who has done the first must be shown the second
 * rather than the same button again.
 *
 * Widened with `string & {}` for the same reason [KnowledgeReason] is: a newer
 * node may name a step this build has no control for, and drawing nothing is
 * the only honest answer to a step it cannot perform.
 */
export type SetupSeatStep = "create_app" | "install_app" | (string & {});

export interface SetupSeatState {
  handle: string;
  name?: string;
  requirements: SetupRequirement[];
  /** Whether anything is written down for this seat, working or not. */
  present?: boolean;
  /**
   * Whether this seat is MEANT to be covered, whether or not it holds
   * anything yet.
   *
   * Separate from `present`, and the gap between them is a real state: a
   * GitHub seat that names the tier it will run at and has no app yet has
   * opted in with both of its two acts still to do.
   */
  enrolled?: boolean;
  satisfied: boolean;
  inbound_path?: string;
  public_url?: string;
  /**
   * The third-party app definition this agent's own app is created from,
   * ready to paste, for an app whose seats a person has to build by hand.
   *
   * The engine's own, byte for byte the one its provisioning command pushes:
   * the scopes, the events and this seat's request URL are what make an app
   * the engine can use. Absent where the company has no public address yet,
   * or where the build has no manifest for the app.
   */
  manifest?: string;
  /**
   * Why there is no manifest, when there could have been one: a company with
   * no public address, or a role name past the app's own name cap. A silent
   * absence is the worst answer, because the field help tells an operator to
   * paste one.
   */
  manifest_note?: string;
  /**
   * The one line under this agent's name: where its own credential for this
   * app is kept, or what is missing. Only Slack has a route per seat, so
   * every other app's roster has this and no path.
   */
  detail?: string;
  /**
   * How much this agent may do at the app, where the app has tiers. Empty
   * where it has none, which is every app but the two code hosts.
   */
  tier?: string;
  /**
   * That tier written for a person: its name, and the one line saying what
   * it grants.
   *
   * SENT RATHER THAN DERIVED HERE. The vocabulary belongs to the package
   * that builds the manifest and mints the tokens, and a screen prettifying
   * the raw id would be a second, silent statement of what `read_only`
   * means, free to drift from the permissions actually asked for.
   */
  tier_label?: string;
  tier_hint?: string;
  /** What is outstanding for this seat. Empty means nothing is. */
  step?: SetupSeatStep;
  /**
   * Where a person goes for that step, when the engine can address it.
   *
   * EMPTY FOR "create_app", and that is the contract rather than an omission:
   * an app is created by POSTing a manifest from a page carrying the
   * operator's own session at the app, so there is no address to link to. The
   * screen asks the engine for a manifest and submits a form with it.
   */
  action_url?: string;
  /**
   * Where a person goes to DELETE what this seat holds at the third-party
   * app, when deleting it is not something the engine can do.
   *
   * GitHub is why: a teardown can uninstall an agent's app, which revokes its
   * access, but there is no endpoint at any permission for deleting the app
   * registration. That is done from its settings page by its owner, so a
   * disconnect hands over a link rather than claiming to have finished.
   */
  manage_url?: string;
}

/** One provisioning pass, live or finished. */
export interface SetupRun {
  run_id: string;
  key: string;
  state: "running" | "done" | "failed";
  started_at: string;
  ended_at?: string;
  findings?: ReconcileFinding[];
  error?: string;
  report?: ReconcileStatus;
}

export interface SetupListing {
  tools: SetupToolState[];
  public_base_url: {
    /** The RESOLVED address, empty when the reference resolves to nothing. */
    value: string;
    present: boolean;
    /**
     * Whether the configured value resolves to something. THREE-VALUED:
     * `null` means this node cannot say, which is not the same as `false`.
     * `present` alone is what a banner read once, so a `${VAR}` nobody
     * exported rendered as "reach this engine at" and then nothing.
     */
    resolved?: boolean | null;
    config_path: string;
    /**
     * The variable the setting points at, empty for a literal. What makes
     * an unresolved address actionable: "set it" is advice, "export
     * PUBLIC_BASE_URL" is an instruction.
     */
    reference?: string;
  };
}

export interface SecretRow {
  name: string;
  key_id: string;
  updated_at: string;
  updated_by: string;
  source: string;
}

/**
 * One `${VAR}` the active company document names, and the field that names it.
 *
 * `GET /config/references` answers a list of these. `path` is the operator's
 * own spelling of the field — `roles[0].integrations.slack.bot_token` — so it
 * is something they can find, and a credential with several readers appears
 * once per reader.
 */
export interface ConfigReference {
  path: string;
  name: string;
}

// ---------------------------------------------------------------------------
// Frames
// ---------------------------------------------------------------------------

/** The connect handshake. Built entirely from memory — no store round trip. */
export interface Snapshot {
  agents?: AgentRow[];
  events?: FeedRow[];
  sandboxes?: SandboxEntry[];
  org?: OrgProjection;
  tools?: ToolRow[];
  health?: HealthPush;
  tokens?: Rollup;
  budget?: OrgBudget;
  schedules?: ScheduleRow[];
}

export type PushKind =
  | "snapshot"
  | "event"
  | "agents"
  | "seats"
  | "sandboxes"
  | "tokens"
  | "budget"
  | "schedules"
  | "org"
  | "tools"
  | "health"
  | "result"
  | "error"
  | "pong";

export interface Frame {
  kind: PushKind;
  data?: unknown;
  id?: number;
  what?: string;
  error?: string;
  ts?: string;
}

/** The named answers the socket's request/response channel serves. */
/** One field's before and after, AS TEXT — every renderer of a delta wants
 *  "todo → in_progress", and the typed value is on the row for anything that
 *  needs it. */
export interface WorkDelta {
  from: string;
  to: string;
}

/** One commit, as the activity feed renders it.
 *
 *  `at` is the AUTHORED instant — what the writer's own clock said, and what a
 *  card renders — and `effective_at` is the fleet-agreed one every duration is
 *  measured on. Both, because they are different facts and a surface carrying
 *  one of them silently answers a different question than it looks like. */
export interface WorkActivityRecord {
  id: string;
  log_seq: number;
  log_stream: string;
  log_generation: number;
  at: string;
  effective_at: string;
  kind: string;
  actor?: string;
  actor_kind?: string;
  operator_id?: string;
  subject_kind: string;
  subject_id: string;
  subject_key?: string;
  project?: string;
  excerpt?: string;
  fields?: Record<string, WorkDelta>;
  comment_id?: string;
  batch_id?: string;
  turn_id?: string;
  /** How a reader tells "nothing was announced" from "nothing happened" —
   *  which is the whole reason a quiet commit still writes a row. */
  notified: boolean;
  late?: boolean;
}

export interface WorkActivityAnswer {
  records: WorkActivityRecord[];
  /** Resumes exactly after the last row, as a log POSITION — a bare sequence
   *  names no stream and no generation, so a cursor built from one cannot
   *  survive a reanchor. */
  next_cursor?: string;
  read_level?: ReadLevel;
  log_seq?: number;
  applied_through?: number;
  log_lag?: number;
  complete: boolean;
  incomplete?: WorkIncomplete;
}

/** One question waiting on somebody, with what to do about it. */
export interface WorkAskRow extends WorkSummary {
  comment: string;
  asked_by: string;
  asked_at: string;
  body: string;
  /** The literal call that answers it. A model handed a comment id still has
   *  to compose the call, and every one it composes differently is a round
   *  spent being refused. */
  answer_with: string;
}

/** One sub-item claimed by somebody. It lives on ANOTHER person's task, which
 *  is why it is its own block: no assignee filter over tasks reaches it. */
export interface WorkChecklistRow {
  task: string;
  task_key: string;
  task_title: string;
  checklist: string;
  item: string;
  name: string;
  done: boolean;
}

/** Everything one person is expected to look at — seven different CLAIMS on
 *  their attention, each bounded the same so no block crowds out another. */
export interface WorkMyWork {
  handle: string;
  /** In the STORED order, never re-sorted: the order is the content — it is
   *  what somebody decided — and sorting it discards the decision. */
  priorities: WorkSummary[];
  assigned: WorkSummary[];
  asked_of_me: WorkAskRow[];
  checklist_items: WorkChecklistRow[];
  collaborating: WorkSummary[];
  watching_recent: WorkSummary[];
  unblocked_recent: WorkSummary[];
  read_level?: ReadLevel;
  log_seq?: number;
  applied_through?: number;
  log_lag?: number;
  complete: boolean;
  incomplete?: WorkIncomplete;
}

/** One notice in a person's inbox, as `tracker.InboxNotice` serialises it.
 *
 *  THE ONE FACT NO COMMERCIAL TRACKER RECORDS is `reason`: the applier writes
 *  WHY this change found this person, as one of nineteen, in the precedence
 *  order that decided it. A notice also says whether it ASKS something of them
 *  (`addressed`) or merely informs, and whether it arrived only because nobody
 *  better was found (`fallback`). */
export interface WorkInboxNotice {
  /** The history row this came from, and what a mark names. */
  record_id: string;
  log_seq: number;
  log_stream: string;
  log_generation: number;
  /** The AUTHORED instant — a card saying "yesterday" must not move because a
   *  record was redelivered. */
  at: string;
  reason: string;
  /** Which half of the person's own split this fell in. */
  primary: boolean;
  /** It asks something rather than informing: a turn that must answer. */
  addressed: boolean;
  /** Delivered only because nobody better was found — a lead hearing about a
   *  report's task because the report has left. */
  fallback?: boolean;
  kind: string;
  subject_id: string;
  /** The human-readable key, resolved by the applier so the read is an index
   *  range rather than a join. */
  subject_key?: string;
  excerpt?: string;
  actor?: string;
  actor_kind?: string;
  read: boolean;
  snoozed?: boolean;
  snoozed_until?: string;
}

/** A page of one person's inbox. */
export interface WorkInboxAnswer {
  handle: string;
  notices: WorkInboxNotice[];
  /** The split that was APPLIED, defaulted — so a screen can say "you are
   *  seeing these because" without repeating the defaulting rule. */
  primary_reasons: string[];
  /** Where this person's own record says they have read to. */
  seen_through?: { stream?: string; generation?: number; seq?: number };
  next_cursor?: string;
  /** Counts over THIS PAGE, and the answer says so: a total over the table
   *  would be a second scan of rows this answer did not return. A badge built
   *  on them therefore saturates at the page size rather than claiming a
   *  total. */
  unread: number;
  primary: number;
  read_level?: ReadLevel;
  log_seq?: number;
  applied_through?: number;
  log_lag?: number;
  complete?: boolean;
  incomplete?: WorkIncomplete;
}

/** One ranked hit from the company's own lexical item search. */
export interface WorkRanked {
  id: string;
  key: string;
  title: string;
  project: string;
  type: string;
  status: string;
  assignee?: string;
  /** A PLACE, 1-based, and deliberately NOT a score. The arithmetic that
   *  ordered these — score within a method, reciprocal rank fusion across
   *  methods, per slice of the corpus — is finished before a coordinator sees
   *  them, so a score field here could only ever have carried zero. */
  rank: number;
  /** The index's own excerpt, which is what makes a ranked answer readable
   *  without opening every hit. */
  snippet?: string;
}

/** The ranked search, and the one refusal that is not a failure.
 *
 *  `available: false` with `reason: "building"` means this node joined
 *  recently and is still indexing — items that exist are simply not findable
 *  from here yet. It is reported rather than returned as an error because
 *  nothing is wrong, and a screen that drew "nothing matches" would have a
 *  reader file the duplicate. */
export interface WorkSearchAnswer {
  hits: WorkRanked[];
  available: boolean;
  reason?: string;
  note?: string;
}

/** One person a change reached, and why.
 *
 *  `reason` is the ONE of nineteen that named them — the resolution is ordered
 *  and a handle appears once — and `addressed` is whether the notice ASKS
 *  something of them rather than informing them. */
export interface RoutingRecipient {
  handle: string;
  reason: string;
  addressed: boolean;
  fallback?: boolean;
  fallback_rank?: number;
  excerpt?: string;
}

/** What this node can say about who one change reached.
 *
 *  THE THREE EMPTY STATES ARE THREE ANSWERS and each sends a reader somewhere
 *  different: `nobody` at who has left the company, `swept` at the retention
 *  horizon, `unknown` at neither. `quiet` is the commit that announced nothing
 *  at all, which is most of them. */
export type Delivery = "quiet" | "reached" | "nobody" | "swept" | "unknown";

/** Who one change woke, and under which reason. */
export interface WorkRoutingAnswer {
  record_id: string;
  /** False when no change with that id exists here — a dead link, or a
   *  reanchor that has not replayed this far. NOT the same as a change that
   *  woke nobody. */
  held: boolean;
  kind?: string;
  subject_id?: string;
  subject_key?: string;
  actor?: string;
  actor_kind?: string;
  at?: string;
  /** Whether the commit CARRIED a notification. It does not mean somebody was
   *  woken: the applier deliberately does not hold the roster that would
   *  need, so an announced change can still reach nobody. */
  notified: boolean;
  recipients: RoutingRecipient[];
  delivery: Delivery;
  /** The instant the stated retention horizon falls on — what `delivery` was
   *  decided against. */
  retained_from?: string;
  /** The list was cut. It cannot happen for a change this engine wrote, so
   *  it means a peer wrote a larger recipient set. */
  truncated?: boolean;
  addressed: number;
  fallback: number;
  read_level?: ReadLevel;
  log_seq?: number;
  applied_through?: number;
  log_lag?: number;
  complete?: boolean;
  incomplete?: WorkIncomplete;
}

/** One unit of agent work, as a list row. */
export interface TurnRow {
  /**
   * ONE RUN of a turn. A trigger that fails without acting is redelivered, so
   * one unit of work legitimately runs several times, and each run is its own
   * id: `(turn_id, phase, iteration)` is the key every phase row is stored
   * under, and it has to be unique per execution. See `adr/0017`.
   */
  turn_id: string;
  /**
   * The unit of work behind that run — what groups a trigger's attempts.
   * Absent on a row an engine from before the split wrote, where `turn_id`
   * carries it instead.
   */
  work_key?: string;
  agent_id?: string;
  role?: string;
  /** The span of the turn's own EVENTS, which is not its duration: the span
   *  covers the reflection pass that publishes after the turn ends. */
  started_at: string;
  ended_at: string;
  /** The turn's OWN measurement, and zero for one that has not finished —
   *  which `complete` is what tells apart. */
  duration_ms: number;
  /** Whether a completion record exists. A turn with none is either running
   *  or died mid-flight, and those look identical from a list. */
  complete: boolean;
  phases: number;
  /** SELF-ITERATE rounds — the highest iteration any phase reached. A phase's
   *  TOOL rounds are `rounds_used` on its own record; the two are different
   *  quantities and were both called "rounds", so a one-iteration turn listed
   *  "Rounds 1" directly above phase rows reading "3r" and "1r". */
  iterations: number;
  /** Whether ANY event of the turn was a failure, which is a different
   *  question from its outcome: a turn can recover from a failed provider
   *  call and still end well. */
  failed: boolean;
  input_tokens: number;
  output_tokens: number;
  total_tokens: number;
  /** Every distinct model the turn used, comma-joined — a turn routinely uses
   *  two, a cheap one for the extension judge and the seat's own. */
  models?: string;
  summary?: string;
  trigger?: string;
  task_id?: string;
}

export interface TurnsAnswer {
  turns: TurnRow[];
  /** The cursor to resume from, on the turn's START. */
  next: string | null;
}

/** Who the presented credential belongs to — see `lib/viewer.ts`. */
export interface Viewer {
  /** The operator id the token resolves to, or "" for an anonymous caller. */
  operator_id: string;
  /** Whether this caller may ask the operator-gated questions. */
  operator: boolean;
  /** The seat whose `contact.crewlet_operator_id` names that id, or "".
   *  UNBOUND IS AN ORDINARY STATE, not a misconfiguration. */
  handle: string;
  name: string;
  kind: string;
}

export interface QueryMap {
  viewer: Viewer;
  work_inbox: WorkInboxAnswer;
  agent: AgentAnswer;
  agent_memory: AgentMemoryAnswer;
  events: EventsPage;
  event_series: EventSeries;
  event: EventRecord;
  trace: TraceAnswer;
  turn: TurnAnswer;
  turns: TurnsAnswer;
  phases: PhasesPage;
  tokens: Rollup;
  token_series: TokenSeries;
  stream: EngineHealth;
  fleet: FleetAnswer;
  budgets: BudgetsAnswer;
  schedules: SchedulesAnswer;
  schedule_runs: ScheduleRunsAnswer;
  integrations: IntegrationsAnswer;
  sandbox_runs: { runs: SandboxRun[] };
  retention: RetentionReport;
  work_items: WorkItemsAnswer;
  work_item: WorkItemDetail;
  work_views: WorkViewsAnswer;
  work_projects: WorkProjectsAnswer;
  work_project: WorkProjectDetail;
  work_workload: WorkloadAnswer;
  work_activity: WorkActivityAnswer;
  work_my_work: WorkMyWork;
  work_goals: WorkGoalsAnswer;
  work_catalogue: WorkCatalogueAnswer;
  work_person: WorkPersonState;
  work_search: WorkSearchAnswer;
  work_routing: WorkRoutingAnswer;
  pages: PagesAnswer;
  page: PageDetail;
  containers: { containers: PageContainer[] };
  page_activity: PageActivityAnswer;
  page_revision: PageRevisionBody;
  conversations: ConversationsAnswer;
  a2a_channels: A2AAnswer;
  knowledge: KnowledgeAnswer;
  config: CompanyDocument | null;
  config_audit: RevisionMeta[];
  config_diff: ConfigDiff;
  config_entities: { kind: string; ids?: string[]; id?: string; entity?: unknown };
}

export type QueryName = keyof QueryMap;

/** The machine-readable codes a rejected query carries. */
export type QueryErrorCode =
  | "unknown_query"
  | "unauthorized"
  | "query_failed"
  /** This node understood the question and REFUSED it: a parameter missing,
   *  malformed, or outside the set the field accepts. The caller's fault, not
   *  the engine's — retrying sends the same bad request again. */
  | "bad_params"
  | "not_found"
  /** This node understood the question and cannot answer it YET — a
   *  projection still catching up after a restart or a fresh join. A screen
   *  says "ask again in a moment", never "there is nothing": the second is an
   *  answer a person acts on. */
  | "unavailable"
  | "timeout"
  | "closed";
