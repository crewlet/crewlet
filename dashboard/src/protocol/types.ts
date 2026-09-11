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
  turn_id: string;
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

/** A live token meter. Process-lifetime — never comparable to a spend rollup. */
export interface Meter {
  used: number;
  max: number;
  refused_at: string;
}

/** The live half of a seat row, merged onto its static config row. */
export interface Overlay {
  state?: string;
  runtime_id?: string;
  current_task?: string | null;
  current_phase?: string | null;
  current_iteration?: number;
  input_tokens?: number;
  output_tokens?: number;
  total_tokens?: number;
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
export interface SandboxRun {
  turn_id: string;
  agent_handle: string;
  role: string;
  status: string;
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
}

// ---------------------------------------------------------------------------
// Spend
// ---------------------------------------------------------------------------

export interface Bucket {
  input_tokens: number;
  output_tokens: number;
  total_tokens: number;
  calls: number;
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
  turn_id: string;
  role: string;
  handle: string;
  agent_id: string;
  started_at: string;
  ended_at: string;
  by_phase: Record<string, Bucket>;
}

export interface Rollup {
  since_days: number;
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
    live_used: number;
  };
  seats: {
    role: string;
    handle: string;
    agent_id: string;
    max_tokens: number;
    durable_used: number;
    durable_updated_at: string;
    live_used: number;
  }[];
  /** False means the durable counter could not be READ — never that it is zero. */
  durable: boolean;
}

// ---------------------------------------------------------------------------
// Org, tools, schedules
// ---------------------------------------------------------------------------

/** A role as `config.Company` serialises it. Verbatim: the config's own names. */
export interface OrgRole {
  name: string;
  kind?: string;
  handle?: string;
  email?: string;
  goal?: string;
  backstory?: string;
  responsibilities?: string[];
  behavioral_guidelines?: string[];
  manages?: string[];
  token_budget?: number;
  llm?: string;
  llm_auxiliary?: string;
  learning_enabled?: boolean;
  availability?: string;
  contact?: Record<string, string>;
  mcp_env?: Record<string, Record<string, string>>;
  integrations?: Record<string, unknown>;
  schedules?: ScheduleSpec[];
}

export interface OrgUnit {
  name: string;
  type?: string;
  purpose?: string;
  lead?: string;
  goals?: string[];
  channel?: string;
  knowledge_refs?: string[];
  mcp_env?: Record<string, Record<string, string>>;
  integrations?: Record<string, unknown>;
  roles?: OrgRole[];
  children?: OrgUnit[];
  schedules?: ScheduleSpec[];
}

export interface OrgTree {
  name?: string;
  mission?: string;
  vision?: string;
  policies?: string[];
  roles?: OrgRole[];
  units?: OrgUnit[];
}

export interface ScheduleSpec {
  name: string;
  cron: string;
  task: string;
  timezone?: string;
}

export interface ToolRow {
  name: string;
  description: string;
  /** `builtin` / `mcp:<server>` / `a2a` — the two-value origin grammar. */
  source: string;
}

export interface ScheduleRow {
  name: string;
  cron: string;
  task: string;
  scope: string;
  scope_name: string;
  timezone: string;
  next_run: string;
  last_run: string;
  last_outcome: string;
}

export interface ScheduleRunRow {
  name: string;
  scope: string;
  scope_name: string;
  fired_at: string;
  outcome: string;
  detail: string;
}

export interface SchedulesAnswer {
  schedules: ScheduleRow[];
  recent_runs?: ScheduleRunRow[];
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
 * with NO ACTIVE CONFIG — dropping every inbound webhook — came to render
 * identically to a healthy idle one.
 */
export interface HealthPush {
  status: string;
  in_flight?: number;
  shutting_down?: boolean;
}

export interface EngineHealth {
  status: string;
  node?: string;
  configured?: boolean;
  engine?: boolean;
  version?: string;
  started_at?: string;
  engine_started_at?: string;
  queue?: string;
  clients?: number;
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
  config_reported_at?: string;
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

/** A term's third value made explicit: read, unreadable, or not applicable. */
export type RetentionTermState = "ok" | "unknown" | "n/a";

export interface RetentionTerm {
  name: string;
  state: RetentionTermState;
  /** Meaningless unless `state` is "ok". */
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
  detail?: string;
  action_url?: string;
}

/**
 * What the reconcile loop last found for one surface.
 *
 * The row's `reconcile` is THREE-VALUED like every count beside it: an object
 * is a real finding, and `null` is either a process with no loop to ask (a
 * standalone API) or a surface the loop has not reached yet. Neither is a
 * claim that the surface is healthy.
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

export interface CounterpartyProfile {
  observer_handle: string;
  subject: string;
  summary: string;
  updated_at: string;
  observations?: number;
}

export interface AgentMemoryAnswer {
  id: string;
  diary: DiaryEntry[];
  episodes: Episode[];
  skills: SynthesizedSkill[];
  counterparties: CounterpartyProfile[];
  onboarded_at: string;
}

/** One recorded turn in one conversation, from the conversation ledger. */
export interface ConversationEntry {
  turn_id?: string;
  conversation_key: string;
  work_key?: string;
  summary?: string;
  outcome?: string;
  created_at: string;
  [key: string]: unknown;
}

export interface ConversationRow {
  key: string;
  source?: string;
  local?: string;
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
 *  invisible to velocity with no second value to keep in step. */
export type WorkStatus =
  | "todo"
  | "in_progress"
  | "in_review"
  | "done"
  | "cancelled"
  | "closed"
  | (string & {});

/** The four groups every rule is written at. */
export type WorkStatusGroup = "not_started" | "active" | "done" | "closed" | (string & {});

/** OPEN, not an enum: the type catalogue is the workspace's, and a company
 *  filing "incident" is filing something this build has never heard of. */
export type WorkType = string;
export type WorkPriority = "none" | "low" | "normal" | "high" | "urgent" | (string & {});

/** How stale an answer may be, as the engine ACTUALLY served it — never the
 *  level asked for. A level never silently downgrades, so the two can differ
 *  only by a refusal. */
export type ReadLevel = "stale" | "session" | "monotonic" | "linearizable" | (string & {});

/** One task as a board row draws it. The BODY IS ABSENT — fifty tasks at
 *  64 KiB each is three megabytes to draw a list of titles. */
export interface WorkSummary {
  id: string;
  key: string;
  project: string;
  title: string;
  status: WorkStatus;
  status_group?: WorkStatusGroup;
  priority?: WorkPriority;
  assignee?: string;
  sprint?: number;
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
  archived?: boolean;
  rank?: string;
  updated: string;
  /** The composed log position this row was last written at. */
  version: number;
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

export interface WorkItemsAnswer {
  items: WorkSummary[];
  /** Capped by construction: an exact total over an unbounded set is the one
   *  query in this grammar that turns a poll into a scan. */
  total_hint: number;
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
 *  A BUILTIN ROW HAS NO ID. Every container has a list, a board and a calendar
 *  without anybody saving one — they are not objects, so there is nothing to
 *  rename, protect, rank or pin — and a screen renders them from `key`. */
export interface WorkView {
  id?: string;
  key: string;
  name: string;
  type: "list" | "board" | "calendar";
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

export interface WorkComment {
  id: string;
  task: string;
  author: string;
  author_kind?: string;
  body: string;
  reply_to?: string;
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

export interface WorkItem extends WorkSummary {
  type?: WorkType;
  body?: string;
  reporter?: string;
  collaborators?: string[];
  watchers?: string[];
  tags?: string[];
  /** The item's own hand-off budget, spent by an agent reassigning it and
   *  reset by any human touch. Past its cap the engine refuses the next
   *  hand-off rather than letting the item circle. */
  reassignments?: number;
  created_at?: string;
  updated_at?: string;
}

export interface WorkItemDetail {
  task: WorkItem;
  comments?: WorkComment[];
  history?: WorkChange[];
  links?: WorkLink[];
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
}

export interface PageContainer {
  key: string;
  name?: string;
  purpose?: string;
  created_at?: string;
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

/** Every stored event of one turn, ordered oldest first. */
export interface TurnAnswer {
  turn_id: string;
  events: EventRecord[];
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
  /** The reconcile finding this input being absent produces. */
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
  /** Three-valued, as everywhere: null is "this process cannot say". */
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
    value: string;
    present: boolean;
    resolved?: boolean | null;
    config_path: string;
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
  org?: OrgTree;
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
export interface QueryMap {
  agent: AgentAnswer;
  agent_memory: AgentMemoryAnswer;
  events: EventsPage;
  event: EventRecord;
  trace: TraceAnswer;
  turn: TurnAnswer;
  phases: PhasesPage;
  tokens: Rollup;
  stream: EngineHealth;
  fleet: FleetAnswer;
  budgets: BudgetsAnswer;
  schedules: SchedulesAnswer;
  integrations: IntegrationsAnswer;
  sandbox_runs: { runs: SandboxRun[] };
  retention: RetentionReport;
  work_items: WorkItemsAnswer;
  work_item: WorkItemDetail;
  work_views: WorkViewsAnswer;
  pages: PagesAnswer;
  page: PageDetail;
  containers: { containers: PageContainer[] };
  conversations: ConversationsAnswer;
  a2a_channels: A2AAnswer;
  knowledge: KnowledgeAnswer;
  config: OrgTree | null;
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
  | "not_found"
  /** This node understood the question and cannot answer it YET — a
   *  projection still catching up after a restart or a fresh join. A screen
   *  says "ask again in a moment", never "there is nothing": the second is an
   *  answer a person acts on. */
  | "unavailable"
  | "no_event_store"
  | "timeout"
  | "closed";
