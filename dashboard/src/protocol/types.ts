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
   * They are read together with `inbound` and are misleading apart — "128
   * arrived" alone cannot tell a working integration from one whose every
   * delivery reaches nobody, and a seat draining a thread's backlog as one
   * turn looks like a seat that ignored twelve messages.
   */
  skipped?: number | null;
  coalesced?: number | null;
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

export type WorkStatus = "todo" | "in_progress" | "blocked" | "in_review" | "done" | (string & {});
export type WorkType = "task" | "bug" | "story" | "epic" | "spike" | (string & {});
export type WorkPriority = "low" | "normal" | "high" | "urgent" | (string & {});

/** One item as a board row draws it. The BODY IS ABSENT — fifty items at
 *  64 KiB each is three megabytes to draw a list of titles. */
export interface WorkSummary {
  id: string;
  key: string;
  project: string;
  type: WorkType;
  title: string;
  status: WorkStatus;
  priority: WorkPriority;
  assignee?: string;
  reporter?: string;
  parent_id?: string;
  labels?: string[];
  due?: string;
  updated_at: string;
  revision: number;
}

export interface WorkItemsAnswer {
  items: WorkSummary[];
  limit: number;
  offset: number;
  /** The last key number minted per project — the board header's "ENG-42 was
   *  the last one". A different fact from how many are open, and the one
   *  nothing else can supply. */
  minted: Record<string, number>;
}

export interface WorkComment {
  id: string;
  item_id: string;
  author: string;
  author_kind?: string;
  body: string;
  reply_to?: string;
  mentions?: string[];
  created_at: string;
  edited_at?: string;
}

export interface WorkChange {
  id: string;
  item_id: string;
  kind: string;
  actor?: string;
  actor_kind?: string;
  fields?: Record<string, { from: string; to: string }>;
  comment_id?: string;
  excerpt?: string;
  created_at: string;
}

export interface WorkLink {
  kind: string;
  other_id: string;
  key?: string;
  title?: string;
  status?: WorkStatus;
  /** The half nobody authored. A UI renders it differently, and an editor
   *  knows which end to change. */
  derived?: boolean;
}

export interface WorkItem extends WorkSummary {
  body?: string;
  watchers?: string[];
  reassignments?: number;
  close_reason?: string;
  created_at?: string;
}

export interface WorkItemDetail {
  item: WorkItem;
  revision: number;
  comments?: WorkComment[];
  history?: WorkChange[];
  links?: WorkLink[];
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

export interface RevisionMeta {
  id: string;
  epoch?: string;
  summary: string;
  author: string;
  created_at: string;
  active?: boolean;
}

export interface ConfigDiff {
  from: string;
  to: string;
  changes: { op: string; path: string; from?: unknown; to?: unknown }[];
}

export interface SecretRow {
  name: string;
  key_id: string;
  updated_at: string;
  updated_by: string;
  source: string;
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
  work_items: WorkItemsAnswer;
  work_item: WorkItemDetail;
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
