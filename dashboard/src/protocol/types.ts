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
 *    is why `stopped_reason` is required-but-possibly-null while `budget` is
 *    `| null`: one has to be able to say "no longer stopped", the other has to
 *    be able to say "nobody is measuring".
 *  - **A count that could not be taken is `null`, never `0`.** The integrations
 *    answer and the budgets answer both distinguish "zero" from "this process
 *    cannot say", and collapsing them is how a dashboard reports a healthy
 *    silence over a store it never reached.
 */

import type { BrokerKind, FleetBrokerAnswer } from "./broker.ts";
import type { LogPosition } from "./session.ts";
// THE SHAPES AN ENGINE GATE HOLDS ARE NOT DECLARED HERE. They live in
// `../contract/`, the one home of every declaration a Go test reads, and this
// file composes them into the answers and frames it declares. RELATIVE, like
// every contract import in this directory: it is also built alone as
// `protocol.js`, where the `~` alias does not exist.
import type { BUDGET_WINDOWS } from "../contract/config.ts";
import type { OBJECTS_STATES } from "../contract/fleet.ts";
import type { BudgetState, GROUPS } from "../contract/spend.ts";
import type { SANDBOX_TAIL_OUTCOMES } from "../contract/sandbox.ts";
import type { LIVE_CALL_DETAIL } from "../contract/wire.ts";
import type { McpServersStatusAnswer } from "../contract/mcp.ts";
import type { Coverage } from "../contract/coverage.ts";
import type { OrgServices } from "../contract/services.ts";
import type { LimitsAnswer } from "../contract/limits.ts";
import type { AssistantViewerAssistant } from "../contract/assistant.ts";
import type { SignInPolicyAnswer } from "../contract/signin.ts";
import type { EngineHealth } from "../contract/health.ts";
import type { StopReason } from "../contract/stops.ts";
import type {
  IntegrationsAnswer,
  LinkHostsAnswer,
  ReconcileFinding,
  ReconcileStatus,
} from "../contract/integrations.ts";
import type { AgentEpisode, AgentMemory, MemoryOverview } from "../contract/memory.ts";
import type { BackupsAnswer } from "../contract/backups.ts";
import type {
  LinkedFromStatus,
  PageBacklinks,
  PageReadsAnswer,
  SkillLoad,
} from "../contract/pages.ts";
import type { PushKind, SeatActivity, StoppedReason } from "../contract/wire.ts";
import type {
  ARTIFACT_CAPABILITIES,
  ARTIFACT_FOLDER_VISIBILITIES,
  ARTIFACT_HISTORY_OPS,
  ARTIFACT_KINDS,
  ARTIFACT_LIST_SCOPES,
  ARTIFACT_LIST_SORTS,
  ARTIFACT_ROLES,
  AUDIENCE_KINDS,
  COLLECTION_PRESETS,
  COLUMN_TYPES,
  DASHBOARD_AGGS,
  DASHBOARD_COLUMN_TYPES,
  DASHBOARD_COLUMN_WORDS,
  DASHBOARD_DATASET_STATUSES,
  DASHBOARD_NUMBER_FORMATS,
  DASHBOARD_REFRESH_MODES,
  DASHBOARD_SOURCE_KINDS,
  DASHBOARD_WIDGET_TYPES,
  DATA_OP_KINDS,
  DATA_VERDICTS,
  FIELD_CONDITIONS,
  ORIGIN_MODES,
  PRESENCE_ACTIVITIES,
  READ_RULES,
  SHAREABLE_ROLES,
} from "../contract/artifacts.ts";
import type { InformSurface, WorkViewShape } from "../contract/work.ts";
import type {
  RetentionGenerationState,
  RetentionIdentityCause,
  RetentionTermName,
  RetentionTrimFloorState,
} from "../contract/retention.ts";

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
  /** The seat the event concerns, by the id every node derives for it — the
   *  store's own `agent_id`, on the live row and the seeded one alike, so the
   *  event log narrows its live rows to one seat (`seat=`) exactly as the
   *  engine narrows the rows it pages in. Absent for an event about no seat. */
  agent_id?: string;
  /** The agent-to-agent channel the event belongs to — the store's own
   *  `channel_id`, on the live row and the seeded one alike, so the event log
   *  narrows its live rows to one channel (`channel=`) exactly as the engine
   *  narrows the rows it pages in. Absent on every event that is not A2A. */
  channel_id?: string;
  /**
   * The filterable dimensions the store promoted out of the payload — present
   * on a row read from history (`events`), absent on a live one. A listing
   * never carries the payload, so this is where a history row says what only
   * its payload holds: the node that published it (`node`), and the runtime
   * audit's tool and direction (`tool`, `dir`) and its author's kind and the
   * credential it acted through (`actor_kind`, `operator_id`) — the author
   * itself is the row's own `actor`.
   */
  tags?: Record<string, string>;
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
  /**
   * The unit of work this row's run was an attempt at — see `adr/0017`.
   * Off the promoted column server-side. Absent on an event no run published,
   * and on one from a run with no ledgerable trigger (a scheduled fire, a
   * sub-agent).
   */
  work_key?: string;
  /**
   * The node that PUBLISHED this event — where the work it describes ran —
   * read off the row server-side (store.EventRecord.Node). Not the node that
   * answered with it: a data node keeps the events of a node without `data`
   * in custody and holds rows it never published. Absent only on a row no node
   * published.
   */
  node?: string;
}

export interface EventsPage {
  events: FeedRow[];
  /** The cursor for the next page, or null at the end of the retained record. */
  next: { before_time: string; before_id: string } | null;
  /** True when the store itself has nothing older, as opposed to this page ending. */
  exhausted: boolean;
  /** Which nodes the page was merged from; a node that did not answer is named. */
  coverage?: Coverage;
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
  /** Which nodes the trace was assembled from. */
  coverage?: Coverage;
}

// ---------------------------------------------------------------------------
// Seats
// ---------------------------------------------------------------------------

/** Why a seat stopped. One shape for every kind of stop. */
export interface ErrorInfo {
  /** The classifier's kind, which a screen words for a person
   *  (`failureWords`). */
  kind: string;
  /** The failure in its own words: sent only to an admin, since it names
   *  the company's model entries and carries what a vendor or a tool wrote
   *  (`livestate.Reach.Config`). */
  detail?: string;
  phase: string;
  turn_id: string;
  at: string;
  event_id: string;
}

/** One tool the model called, as the phase event records it. */
export interface ToolExecution {
  name: string;
  /** The round this call belongs to. THE ordering key — see `rounds()`. */
  round: number;
  arguments: unknown;
  /** What the tool returned — or, on a failed call, what it failed with. */
  result: string;
  /** False on a call that failed. */
  success: boolean;
  /** Present only on a failed call, repeating its `result`. */
  error?: string;
  /**
   * The call's own id — what tells two calls of one response apart, and what
   * a running call (`running_calls[].id`) is matched to once it answers.
   * Absent where nothing named the call.
   */
  call_id?: string;
  /** When the call was handed to the tool (RFC 3339, UTC), and how long it
   *  took. Both absent on a row nothing timed. */
  started_at?: string;
  duration_ms?: number;
  /** `builtin` or `mcp:<server>` for the tool that answered, and the bare
   *  server name for an `mcp:` one. Absent on a call no tool answered. */
  origin?: string;
  server?: string;
}

/**
 * One part of a prompt as the builder that wrote it marked it out
 * (`types.PromptSection`): a stable snake_case key, the title a reader sees,
 * and how many UTF-8 BYTES of the prompt it spans.
 *
 * A prompt's sections TILE it exactly, in order: their bytes add up to the
 * prompt's length in UTF-8, every boundary falls between two characters, and a
 * headed span starts with its own heading line. A reader that finds a map
 * breaking any of that derives the outline from the headings instead
 * (`lib/promptmap.ts`), because a map that does not tile would mis-slice.
 */
export interface PromptSection {
  key: string;
  title: string;
  bytes: number;
  /**
   * The span begins with its own heading line, whose text is `title` — what
   * the builder's `Heading` writes. False or absent for a part that opens
   * with no heading of its own (a `Lead`: a persona, a task, a trigger's
   * text), whose first line may still be a heading somebody else wrote and
   * the span QUOTES. The engine writes it only when true.
   */
  headed?: boolean;
}

/** One message of the prompt a phase was given. */
export interface PromptMessage {
  role?: string;
  content?: string;
  /** This message's own section map; absent from an engine that wrote none. */
  sections?: PromptSection[] | null;
}

/**
 * A round in flight. Never merged into `round_narration` — arriving text and
 * committed text are different facts, and a reader has to be able to tell
 * them apart. `abandoned` holds attempts a provider gave up on partway
 * through, oldest first.
 */
export interface PartialRound {
  round?: number;
  content?: string;
  abandoned?: RoundNarration[] | null;
}

/** One round's model turn, as the engine recorded it. */
export interface RoundNarration {
  round?: number;
  content?: string;
  /**
   * The model answered this round in prose and called no tool, in a phase
   * that had to end in a tool call (a submission tool, or a required call).
   * Absent means false. A later round means the engine asked again; a
   * declined last round of a settled phase means it ended without its
   * submission.
   */
  declined?: boolean;
}

/** One round of a phase's tool loop, as the engine timed it (`types.PhaseRound`). */
export interface PhaseRound {
  /** One-based, on the scale `tool_executions[].round` shares. */
  round: number;
  started_at: string;
  duration_ms: number;
  model?: string;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  tool_calls: number;
  /**
   * Why the model stopped writing this round's response: `end`, `tool_use`
   * or `cap` (see `contract/stops.ts`). Absent where its backend reported
   * none — "not reported", never "ended normally".
   */
  stop_reason?: StopReason;
}

/**
 * One CLI run of a phase (`types.PhaseInvocation`): one process driving its
 * own loop over the phase's tools, from launch to exit — the unit the engine
 * observes for every CLI, where a round is one model request that a CLI's
 * record shows for most CLIs and not for all.
 */
export interface PhaseInvocation {
  started_at: string;
  duration_ms: number;
  /** The `providers.llm` entry and the model that served the run. */
  provider_key?: string;
  model?: string;
  /**
   * How the run ended: `end` (its model stopped), `cap` (its CLI's own turn
   * cap), `stopped` (the engine ended it) or `failed`. A string rather than a
   * union, so an outcome a newer engine records arrives as itself.
   */
  outcome: string;
  /**
   * The rounds the run made, both inclusive, on the scale `rounds[].round`,
   * `tool_executions[].round` and `round_narration[].round` share — what a
   * round is filed under its run by. Both 0 on a run that made no request.
   */
  first_round: number;
  last_round: number;
  /**
   * False for a run whose CLI's record showed no model requests: the whole
   * run is then ONE round, which nothing timed on its own — its span is the
   * run's.
   */
  requests_observed: boolean;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  /** How many of the phase's tools the run called. */
  tool_calls: number;
}

/**
 * One tool call a phase is running right now (`types.RunningCall`) — one entry
 * of `running_calls`, since a coding CLI may run a response's calls at once.
 */
export interface RunningCall {
  /** The call's own id: what its `tool_executions` row carries as `call_id`
   *  once it answers. Absent where nothing named the call. */
  id?: string;
  round: number;
  name: string;
  /** What the call was handed — part of the seat's BODY, so absent on a call
   *  whose body is withheld from this reader ([LiveCall.body_withheld]); its
   *  name and timing are status and always sent. */
  arguments?: string;
  started_at: string;
}

/** A person's note a phase read mid-turn, and the round that first read it
 *  (`types.PhaseSteer`). */
export interface PhaseSteer {
  round: number;
  note_id: string;
  /** The kind of step that read it (`types.SteerBoundary`, one of
   *  `STEER_BOUNDARIES`): a round of the engine's loop, or a step of the
   *  phase's coding run. */
  boundary: string;
}

/** The one work item a turn is charged to (`types.WorkItem`). */
export interface WorkItemRef {
  backend: string;
  id: string;
  key: string;
  project: string;
}

/** Where in a turn a seat is (`livestate.Stage`). */
export type TurnStage = "context" | "phase" | "parked";

/** The turn a seat is on — running, or parked on a detached coding run. */
export interface LiveTurn {
  turn_id: string;
  work_item?: WorkItemRef | null;
  work_item_basis?: string;
  /** When the turn began; a resumed segment keeps the turn's first start. */
  started_at: string;
  stage: TurnStage;
  node?: string;
}

/** Who paused a seat, when and why (`livestate.Paused`). An INPUT to the
 *  seat's state, not the state: the engine says whether a paused seat reads as
 *  stopped. */
export interface Paused {
  /** The seat a bound person or token writes as, or the credential's own login. */
  by: string;
  /** What `by` names, in `iam.ActorKind`'s vocabulary (`agent`, `human`,
   *  `operator`, `system`) — a string, so a kind a newer engine adds arrives
   *  as itself. */
  by_kind: string;
  /** The credential the pause was taken through (`pat:<id>`,
   *  `session:<lineage>`, a token's login); absent when none made it. */
  operator_id?: string;
  at: string;
  reason?: string;
  /** Whether the pause also ended the seat's running work: the turn it was
   *  on, at its next round, and any coding run it had out, at once. */
  stop_running: boolean;
}

/**
 * A seat's models at their usage limit (`livestate.ProviderLimit`): ONE steady
 * condition for as long as the limit lasts, moved only by what says it began
 * or ended, never by the turns and probes in between.
 *
 * It reaches every reader, so it names no `providers.llm` entry: which are
 * spent and which carries a seat is the company's configuration, an admin's to
 * read on Settings, Models.
 */
export interface ProviderLimit {
  /** Whether the seat waits for the limit (every model refused) rather than
   *  running on a fallback, which is not named. */
  parked: boolean;
  /** When the vendor said the limit lifts, in UTC; null where unknown. */
  resets_at: string | null;
  /** When the condition began, in UTC. */
  since: string;
  /** The CLI the limited entry drives (`claude-code`), a key of
   *  `CLI_PRODUCTS`; empty where the report did not say. */
  cli: string;
  /** Whether that CLI signs in on a plan rather than a metered key. */
  subscription: boolean;
}

/** The newest turn a seat ended (`livestate.LastTurn`). */
export interface LastTurn {
  turn_id: string;
  /** The turn's newest event: its completion, or the reflection after it. */
  ended_at: string;
  /** `stopped` when a person ended it with a pause — never a failure. */
  outcome: "completed" | "failed" | "stopped";
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
   * Absent for a run with no ledgerable trigger (a scheduled fire, a
   * sub-agent).
   */
  work_key?: string;
  phase: string;
  iteration: number;
  model: string;
  /**
   * THE BODY — `trigger`, `prompt`, `prompt_messages`, `response`,
   * `tool_executions`, `round_narration`, `partial_round` and every running
   * call's `arguments` — is the seat's TRAIL, and the engine sends it only to
   * a reader who may read that seat's trail: an admin, or the seat's lead.
   * Everybody else is sent the call's STATUS with every one of these keys
   * absent, every body version zero and [body_withheld] set — so each is
   * optional here, and a screen draws a withheld one as "not shown to you",
   * never as an empty prompt.
   */
  trigger?: Record<string, unknown> | null;
  prompt?: string;
  /**
   * The conversation as the phase was given it: the system and user
   * messages only. Typed rather than `unknown[]` — it was untyped, so the
   * live view could not read the system prompt off it and hardcoded "".
   */
  prompt_messages?: PromptMessage[] | null;
  response?: string;
  input_tokens: number;
  output_tokens: number;
  total_tokens: number;
  tool_executions?: ToolExecution[] | null;
  /**
   * Per-round model turns — `{round, content}` — where `round`
   * matches `tool_executions[].round`, which is what lets the two lists
   * interleave into one ledger. Absent on a phase this engine recorded
   * before it sent them.
   */
  round_narration?: RoundNarration[] | null;
  /** The round being written right now; absent when no round is open. */
  partial_round?: PartialRound | null;
  round_num: number;
  /**
   * The round the phase is on, ONE-BASED: the rounds that have come back, and
   * the one in flight from the frame the loop publishes as its provider call
   * is made — the same name and quantity a finished phase record carries as
   * `rounds_used`, so a running phase and a settled one are read from one
   * field.
   */
  rounds_used: number;
  /** Each round's own timing and tokens, as the loop recorded it. */
  rounds?: PhaseRound[] | null;
  /**
   * The phase's CLI runs that have ENDED, in order. The run in flight is not
   * on it until it ends: its rounds are the ones past every entry's
   * `last_round`. Status, like `rounds`: a reader withheld the body is sent
   * it.
   */
  invocations?: PhaseInvocation[] | null;
  /** The round cap currently GRANTED, which an extension raises mid-phase. */
  max_rounds?: number;
  /** The most any extension may raise that cap to. */
  round_ceiling?: number;
  /**
   * When the LATEST round began its provider call: later than every entry in
   * `rounds` while that call is out, equal to the last one's start while the
   * round's tools run, absent before the first round opens. Only the first
   * case is a model call in flight — see `lib/waterfall.ts`.
   */
  round_started_at?: string;
  /**
   * When the CLI run in flight was launched — the run whose rounds are past
   * every entry in `invocations`, which lists only the runs that have ended.
   * Absent between runs.
   */
  run_started_at?: string;
  /**
   * Every tool call running RIGHT NOW, in the order they arrived — a coding
   * CLI may run a response's calls at once. Absent between calls.
   */
  running_calls?: RunningCall[] | null;
  /**
   * Every person's note the phase has read so far, with the round whose
   * provider call first saw it. What the note said and who sent it are on the
   * turn's `agent_turn_steered` rows.
   */
  steers?: PhaseSteer[] | null;
  /** The share of `input_tokens` the prompt cache served and stored — a
   *  breakdown of it, never an addition to it. */
  cache_read_tokens?: number;
  cache_write_tokens?: number;
  /** The item the turn is charged to, when it is on one. */
  work_item?: WorkItemRef | null;
  /** The node running the call. */
  node?: string;
  in_progress: boolean;
  failed?: boolean;
  error?: ErrorInfo | null;
  updated_at: string;
  /** When the call began. Unlike `updated_at`, it never moves. */
  started_at?: string;
  /**
   * The version of the copy this call holds of each heavy field
   * (`LIVE_CALL_DETAIL`). A push names every one and carries a field only
   * when its version moved; the store fills in the rest from what it holds.
   * Always present, on every surface that carries a call. A call whose body
   * is withheld from this reader names every body version ZERO, so the store
   * never asks for a body it will not be sent (`mergeLiveCall`).
   */
  versions: CallVersions;
  /**
   * The engine withheld this call's BODY from this reader — see [prompt].
   * Absent (or false) on a call sent whole.
   */
  body_withheld?: boolean;
}

/** A live call's heavy-field versions, keyed as `LIVE_CALL_DETAIL` is. */
export type CallVersions = Record<keyof typeof LIVE_CALL_DETAIL, number>;

/** The `live_call{agent_id}` answer: one seat's call in flight, whole, or
 *  null — asked and answered by the seat's AGENT ID, the key every overlay is
 *  merged by, never by its name, which two seats may share. */
export interface LiveCallAnswer {
  agent_id: string;
  /** The handle the seat answers to — a label, never what the answer is
   *  paired by. */
  handle: string;
  live_call: LiveCall | null;
  /** The overlay sequence this call is at — see `Overlay.live_call_seq`, and
   *  present beside a null call too. The answer can reach the tab after a push
   *  that was generated later, so one behind what the store has applied is
   *  dropped when the slot has since been cleared or taken by another call,
   *  and otherwise brings only the heavy fields it holds at a newer version
   *  (`Store.applyLiveCall`). */
  live_call_seq: number;
}

/** One calendar window of one scope's token counter — the day, the ISO week or
 *  the month on the company's clock — as the engine states it
 *  (`types.BudgetWindow`). Never comparable to a spend rollup, which is a
 *  window over time a reader chose rather than the window a ceiling is written
 *  for. */
export interface BudgetWindow {
  period: (typeof BUDGET_WINDOWS)[number]["period"];
  /** The window's label on the company clock: `2026-09-23`, `2026-W39`,
   *  `2026-09`. */
  window: string;
  /** The window's half-open span, in UTC. `resets_at` is when its allowance
   *  comes back without a ceiling being raised. */
  starts_at: string;
  resets_at: string;
  used: number;
  /** The ceiling, ABSENT where nothing caps the window — never 0. The live
   *  push lists capped windows only, so there it is always present. */
  limit?: number;
  /** When the window last turned a call away, in UTC; absent while it has
   *  not: a charge it refused, or work turned away unsent because the window
   *  was already full (a turn's next call, a parked delivery, a person's
   *  question, a reflection pass). The gate's own record, kept in the shared
   *  counter beside the spend, so every node reports the same one; it clears
   *  on the scope's next admitted charge or when the window turns over. */
  refused_at?: string;
  /** The engine's judgement of the window. The client computes none. */
  state: BudgetState;
}

/** A live token meter: the fleet's SHARED counters for one scope, one entry per
 *  CAPPED window. */
export interface BudgetMeter {
  windows: BudgetWindow[];
}

/** The live half of a seat row, merged onto its static config row. */
export interface Overlay {
  /**
   * What the seat is doing — the ONE seat-state vocabulary, computed by the
   * engine and on every row. A screen maps it; it never derives it.
   */
  activity?: SeatActivity;
  /** Why a `stopped` seat cannot take work; null on a seat that is not
   *  stopped. Always present, so a seat that started again sheds its reason. */
  stopped_reason?: StoppedReason | null;
  current_phase?: string | null;
  current_iteration?: number;
  live_call?: LiveCall | null;
  /** Orders the `live_call` slot across calls and across a clear: the engine's
   *  own monotonic sequence read when the seat's call last changed — set,
   *  folded, frozen or cleared to null — carried on every surface the call is,
   *  a null call included. A tab records the newest it has applied per seat,
   *  and a push or an answer whose sequence is behind it changes the slot only
   *  where the slot is still that same call — and then only by a heavy field
   *  at a newer version — so a `live_call` answer that a clearing or newer-call
   *  push overtook on the wire cannot put an older call back on screen. Reset
   *  from each snapshot; comparable only within one node's projection, the
   *  only one a socket reads between snapshots. */
  live_call_seq?: number;
  last_error?: ErrorInfo | null;
  budget?: BudgetMeter | null;
  /** The turn the seat is on, or null when it is on none. Always present, for
   *  `stopped_reason`'s reason. */
  turn?: LiveTurn | null;
  /** The newest turn the seat ended, or null while none is known. */
  last_turn?: LastTurn | null;
  /** Who paused the seat, or null while nobody has. Always present, for
   *  `stopped_reason`'s reason: an omitted key would leave a resumed seat paused. */
  paused?: Paused | null;
  /** The seat's models at their usage limit, or null while they are not.
   *  Always present, so a limit that lifted is shed. */
  provider_limit?: ProviderLimit | null;
}

/**
 * One row of an `agents` push: a seat's overlay, and the AGENT ID that names
 * the roster row it is merged onto — nothing else that names the seat, since
 * the roster carries its name and handle as they are now and an event carries
 * them as they were.
 */
export interface OverlayRow extends Overlay {
  agent_id: string;
}

/** A seat row: static config identity, plus whatever overlay has been merged. */
export interface AgentRow extends Overlay {
  /** The seat's HANDLE — what every screen addresses it by. */
  id: string;
  /**
   * The seat's AGENT ID, which the engine derives from its handle (ADR-0013):
   * a handle never changes, so neither does this, and no two seats share it.
   * THE key a live overlay, a sandbox run, a spend row and a phase record
   * carry and pair with this row by — never `role`, which is prose two seats
   * may share.
   */
  agent_id: string;
  /** The seat's name. Display only: nothing pairs a row by it. */
  role: string;
  handle?: string;
  [key: string]: unknown;
}

/** One in-flight detached coding run. */
export interface SandboxEntry {
  turn_id: string;
  /** What the seat was called when the run started — display only; the run
   *  pairs with its seat's row by `agent_id`. */
  role: string;
  agent_handle: string;
  agent_id: string;
  coding_agent: string;
  /** The executor-chain member the run was launched on, and its model — which
   *  a fallback at launch makes other than the chain's head. The member is the
   *  company's configuration and is sent to an admin alone; the model goes to
   *  every reader. */
  provider_key?: string;
  model?: string;
  sandbox_id: string;
  /**
   * What the run was asked to do — the seat's TRAIL, sent only to an admin
   * and the seat's lead ([body_withheld] says when it was withheld).
   */
  task?: string;
  /** The run record's own word: `launching`, `running`,
   *  `awaiting_clarification`, `reseed` or `answered`. */
  status: string;
  started_at: string;
  /**
   * The question a parked run asks — sent to whoever may read the seat's
   * trail AND to the people it is put to, who answer it.
   */
  question?: string;
  audience?: string;
  /** The item the launching turn is charged to, when it is on one. */
  work_item?: WorkItemRef | null;
  /** The node driving the run. */
  owner?: string;
  /** When the run's box began being held paused while it waits on a person. */
  paused_at?: string;
  /** The engine withheld the run's body (`task`) from this reader. */
  body_withheld?: boolean;
}

/**
 * The six statuses a run record can hold.
 *
 * THERE IS NO `done` OR `failed` HERE, and that is the engine's shape rather
 * than an omission: a run's record is DELETED once the run settles and its box
 * is reclaimed, so `sandbox.Active` is every status a record can carry and the
 * terminal pair can never reach a client. How a run ended is on the event
 * stream — the resumed turn's own events, a `sandbox_run_failed` naming the
 * reason, or a `sandbox_run_stopped` naming who stopped it — never on this row.
 */
export type SandboxStatus =
  "launching" | "running" | "awaiting_clarification" | "reseed" | "answered" | "resumed";

/** One durable coding run, as the `sandbox_runs` query answers it. */
export interface SandboxRun {
  /** The RUN this job belongs to — one execution of a turn, and this
   *  record's own key. See `adr/0017`. */
  turn_id: string;
  /** The job the row holds NOW — a turn can launch more than one — and what
   *  `sandbox_tail` is asked by. Empty on a run the live projection knows of
   *  before its durable row is read. */
  launch_id?: string;
  /** The unit of work behind that run. Absent on a turn with no ledgerable
   *  trigger, and on a live entry whose durable row has not been read yet. */
  work_key?: string;
  agent_handle: string;
  role: string;
  status: SandboxStatus;
  coding_agent: string;
  /** The executor-chain member the job was launched on, and its model. Absent
   *  on a live entry that named none; the member is also absent for every
   *  reader but an admin, since it is the company's configuration. */
  provider_key?: string;
  model?: string;
  /** Which configured cell the run's box is in: direct, container or e2b. */
  placement: string;
  /**
   * THE RUN'S BODY (`task_description`, `question`, `question_context`,
   * `decision`, `reply` and `bridge_calls`) is the launching seat's trail:
   * sent to an admin and the seat's lead, and the question with its context
   * and decision also to the people it is put to. Absent for everybody else,
   * who are told so by [body_withheld].
   */
  task_description?: string;
  /** The question, as one sentence: the title of what the run asks. A row from
   *  an older build may carry the whole free-text question here instead. */
  question?: string;
  /** What the question says beside its title, as Markdown (body). Absent when
   *  the run gave none. */
  question_context?: string;
  /** The options the run asks a person to choose between, in a work item's
   *  ask's shape, never with an `inform` (body). Absent on an open question,
   *  which is answered in words. */
  decision?: WorkDecision;
  /** What reached the waiting party, where something did — body. */
  reply?: string;
  audience: string;
  /** Who the question is put to, resolved against the org chart when the run
   *  parked. Empty on a run that is not parked. */
  audience_handles?: string[];
  /** True when `audience` named nobody the chart has and the question was put
   *  to the seat's lead chain instead. */
  audience_fallback?: boolean;
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
   * The answers the run's question let go of, by the id of the delivery each
   * arrived in (`answer_run` answers it as `answer_id`): an answer here was
   * not used, and the run waits on its question again. Absent on a row from an
   * engine that does not say.
   */
  declined_answers?: string[];
  /**
   * The answer recorded on the run, by the deliveries it arrived in, who gave
   * it and when. The first recorded answer wins: an answer whose id is not
   * here was not used. Empty on a run with no recorded answer.
   */
  answer_ids?: string[];
  answered_by?: string;
  answered_at?: string;
  /** The item the launching turn was charged to; null when it was on none. */
  work_item?: WorkItemRef | null;

  /**
   * What this run called through the MCP bridge, in order.
   *
   * THE ONE TOOL LOG THAT HAS NOWHERE ELSE TO LIVE. Every run's calls are
   * made by its coding CLI, a process outside the engine, over the MCP bridge
   * — minutes or hours apart and possibly across a restart. ALWAYS AN ARRAY on
   * a row the store answered, empty for a run that has called nothing yet;
   * absent where it is not known — a live entry whose row is not read yet, or
   * a body withheld from this reader.
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
  /** The engine withheld this run's body from this reader — see [task_description]. */
  body_withheld?: boolean;
}

/** One call a bridged run made. */
export interface BridgeCall {
  name: string;
  /** What the caller passed, as JSON TEXT — never a decoded map. */
  args?: string;
  output?: string;
  failed?: boolean;
  /** When the row recorded the call, on the store's clock. Always present. */
  at: string;
  /**
   * The bridge's own measurement of the call: when it was handed to the
   * seat's tools (RFC 3339, UTC) and how long they took to answer. Both
   * absent on a call nobody measured — "not recorded", never "instant".
   */
  started_at?: string;
  duration_ms?: number;
  /** `builtin` or `mcp:<server>` for the tool that answered, and the bare
   *  server name for an `mcp:` one. Absent on a call no tool answered. */
  origin?: string;
  server?: string;
}

// ---------------------------------------------------------------------------
// Spend
// ---------------------------------------------------------------------------

/**
 * One slice of spend, in TOKENS — the one unit every call is measured in.
 *
 * THE ENGINE'S BUCKET CARRIES A PRICE BESIDE THESE, AND THIS DECLARES NONE OF
 * IT. Only a subscription coding CLI quotes what a run cost, so a currency
 * figure covers the minority of calls that quote one while the token count
 * beside it covers all of them — and on a screen the first reads as the
 * company's spend. The dashboard renders tokens only, never money (rule 19 in
 * `docs/reference/dashboard-design.md`); a field nothing declares is a field
 * no screen can reach for, and `money.test.tsx` and the bundle scan in
 * `internal/api/dashboardjs_test.go` hold that from both ends.
 */
export interface Bucket {
  input_tokens: number;
  output_tokens: number;
  total_tokens: number;
  calls: number;
  /**
   * The share of `input_tokens` the provider's prompt cache served and
   * stored. A BREAKDOWN of the input, never an addition to it: every backend
   * counts the cached prefix inside its input total, so the cache's share is
   * `cache_read_tokens / input_tokens` and adding the two double-counts the
   * prefix (`tokens.Bucket`). Both are zero where no backend reported a
   * cache, which is not the same as "nothing was cached" — see
   * `routes/spend/model.ts`' `cacheShare`.
   */
  cache_read_tokens?: number;
  cache_write_tokens?: number;
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
/** One SEAT's spend: one row per agent id, so two seats sharing a name are two
 *  rows. `role` and `handle` are what the chart calls the seat now. */
export interface AgentSpendRow extends Bucket {
  role: string;
  handle: string;
  agent_id: string;
  /** A PERSON's row rather than a seat's: what the auxiliary model spent for
   *  the human seat `handle` names — a question answered on the operator
   *  surface, or a background pass of a unit they lead. No `agent_id`, and on
   *  a named window no `turns`. */
  person?: boolean;
  by_phase: Record<string, Bucket>;
  /** How many of the seat's turns ENDED in the window, and how many failed —
   *  on a named window only. The live window holds phase records and cannot
   *  count endings, so there both are absent, never zero. */
  turns?: number;
  failed?: number;
}
/** One configured provider entry (`providers.llm.<key>`): what it served, the
 *  models it answered with, and the seats that leaned on it most. */
export interface ProviderSpendRow extends Bucket {
  provider_key: string;
  /** Every model the entry answered with, biggest first. */
  models: string[];
  /** The three seats that spent the most through it, by handle; `seats_total`
   *  is how many did at all. */
  seats: string[];
  seats_total: number;
}
/** How far back a named spend window can reach: the usage domain's history in
 *  days, and the oldest company day still answerable. */
export interface SpendHorizon {
  days: number;
  floor: string;
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
  /** A NAMED window's company days — first, last (inclusive) and how many.
   *  Absent on the live window, which is a rolling span. */
  from?: string;
  to?: string;
  days?: number;
  /** The one seat this answer was narrowed to, by agent id — the identity
   *  the usage domain records a seat under, and so the one it was filtered
   *  by. Absent for the whole company. */
  agent_id?: string;
  /** That seat's handle NOW, read off the chart for a link to it — never the
   *  handle the request typed. Absent for the whole company and for a seat
   *  the chart no longer holds. */
  seat?: string;
  /** How far back a named window can reach. Absent on the live window. */
  horizon?: SpendHorizon;
  totals: Bucket;
  by_phase: PhaseRow[];
  by_model: ModelRow[];
  by_provider?: ProviderSpendRow[];
  /** True where `by_provider` was left out because the reader is not an
   *  admin: the company's model entries are its configuration. */
  entries_withheld?: boolean;
  by_worker: WorkerRow[];
  by_agent: AgentSpendRow[];
  /** The tail of recent turns — the LIVE window's only. A company day holds no
   *  turn, so a named window has none; per-turn spend is `turns` sorted by
   *  tokens. */
  by_turn?: TurnSpendRow[];
  /** How far the live window has counted; absent on a named window, whose
   *  company days carry no per-call instant. */
  aggregated_through?: string;
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
  /**
   * The band's key in each point's `groups`. Empty on, and only on, `other`.
   * By seat it is the seat's agent id and by unit the unit's key — an
   * IDENTITY, since two seats or two units may share a name — which is why
   * `label` exists.
   */
  group: string;
  /** What a seat or unit band is called. Absent where `group` is already the
   *  words: a phase, a model, a worker, a turn id. */
  label?: string;
  handle?: string;
  /** How many seats spent in a UNIT band over the window. */
  seats?: number;
  /**
   * A UNIT band split by the seats that spent in it, biggest first, summing to
   * the band. Absent on every other grouping and on the residual row.
   */
  parts?: SeriesPart[];
  /** The residual: every group past the chart's cap, and how many it stands for. */
  other: boolean;
  folded: number;
}

/** One seat's share of a unit band (`tokens.GroupPart`). */
export interface SeriesPart extends Bucket {
  /** The seat as `group: seat` keys its band: its handle, else its role. */
  group: string;
  handle?: string;
}

export interface SeriesPoint extends Bucket {
  /** The bucket's START — the first instant of its company day or ISO week,
   *  RFC 3339 in UTC. A week's start can be before the window's. */
  at: string;
  /** The bucket's label on the company calendar: `2026-09-23` or `2026-W39`. */
  window: string;
  /** How many of the window's days fall in the bucket — 7 for a whole week,
   *  fewer for the first and last a window cuts short. */
  days: number;
  groups: Record<string, Bucket>;
  other: Bucket;
}

export interface TokenSeries {
  group: (typeof GROUPS)[number]["value"];
  bucket: "day" | "week";
  /** The window, as instants (`until` exclusive) and as its company days. */
  since: string;
  until: string;
  from: string;
  to: string;
  days: number;
  seat?: string;
  horizon: SpendHorizon;
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
  /** How many of `count` reported a failure — a SPLIT of the bar, never an
   *  addition to it, counted by the engine with the rule a turn's own
   *  `failed` mark uses. */
  failed?: number;
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
  /** The window COVERED, snapped outward to whole buckets — and never below
   *  the first whole bucket inside the engine's 30-day history, so a window
   *  the history clips starts later than it was asked to, and one lying wholly
   *  below that bucket comes back empty (`since` equal to `until`, no bars). */
  since: string;
  until: string;
  /** Every bucket in the window, INCLUDING the empty ones. */
  bars: EventBar[];
  /** The window's whole count, stated rather than left as a sum of the bars. */
  total: number;
  /** The window's failed count — the sum of the bars' `failed`. */
  failed?: number;
  /**
   * How many rows each category would give, over the window, with the CATEGORY
   * filter lifted and every other one applied — which is the only meaning a
   * facet count can have. A category with no rows is absent, so a screen
   * rendering the closed set reads a missing key as the zero it is.
   */
  by_category: Record<string, number>;
  /** Which nodes the bars were summed over. */
  coverage?: Coverage;
}

/** The org-wide live meter, plus the identity of the engine run reporting it
 *  and the company clock its windows were cut on.
 *
 *  A REPORT, never the absence of one: before any node has reported the
 *  snapshot and the push carry `null`, which the store holds as `null` — "no
 *  reading yet". `org.windows` empty is the other fact, "nothing is capped",
 *  and the two send a reader to opposite places ("Set one" is offered only on
 *  the second). */
export interface OrgBudget {
  meter_id: string;
  seq: number;
  timezone: string;
  org: BudgetMeter;
}

/** The `budgets` answer: every scope's day, week and month, capped or not. */
export interface BudgetsAnswer {
  /** The company clock every window is cut on (IANA, or `UTC`). */
  timezone: string;
  /** False means the durable counters could not be READ — never that they are
   *  zero — and every window list is then empty. */
  durable: boolean;
  /** The share of a ceiling at which the engine calls a window `near`: the
   *  one threshold a screen may draw as a mark. */
  near_fraction: number;
  org: { windows: BudgetWindow[] };
  seats: {
    agent_id: string;
    role: string;
    handle: string;
    windows: BudgetWindow[];
  }[];
  /**
   * The ceiling every person's Dashboard Assistant spend is held to
   * (`assistant.token_budget`): all three windows, `limit` where capped.
   * `used` is the VIEWER's own spend only where `counted` is true (a person,
   * under a company that caps a person's spend); elsewhere it is a zero
   * nobody measured, and a screen draws no figure.
   */
  assistant: { counted: boolean; windows: BudgetWindow[] };
}

// ---------------------------------------------------------------------------
// The company document (the guarded `config` query, `GET /config`)
// ---------------------------------------------------------------------------

/**
 * The company document as the configuration surface serves it, REDACTED: a
 * credential field holds a whole `${VAR}` reference or the mask
 * `__redacted__`, never a value.
 *
 * WHERE EVERYTHING THE PROJECTION DOES NOT CARRY IS READ. `/org` is
 * anonymous, so it carries a charter, a tree, the budgets and each seat's
 * RESOLVED chain and tool sources ([OrgProjection]); a seat's email, authored
 * `llm` fields, contact identities, tool credentials, integration blocks and
 * schedules live here instead, behind Admin.
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

/**
 * A `token_budget:` mapping, as `config.TokenBudget` writes it: the most one
 * scope may spend in each calendar window on the company clock. A window with
 * no key has no ceiling; the engine refuses a 0 rather than reading it as
 * unlimited.
 */
export type TokenBudget = {
  readonly [P in (typeof BUDGET_WINDOWS)[number]["period"]]?: number;
};

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
  /** This seat's own ceilings, per calendar window; absent is uncapped. */
  token_budget?: TokenBudget;
  llm?: PhaseLLM;
  llm_review?: ProviderKeys;
  llm_subagent?: ProviderKeys;
  llm_auxiliary?: ProviderKeys;
  llm_judge?: ProviderKeys;
  /** Unset means the company default, so this is three-valued. */
  learning_enabled?: boolean | null;
  /** A human seat's identities: one per surface, keyed by [HumanContactKey]. */
  contact?: Record<string, string>;
  availability?: string;
  /**
   * How an agent seat is drawn: a Crewlet character and a colour, each
   * optional, an absent one the first of its list in `contract/avatar.ts`.
   * Agent seats only; the engine refuses it on a human seat.
   */
  avatar?: { character?: string; color?: string; [key: string]: unknown };
  /** Server name to variable name to a `${VAR}` reference or the mask. */
  mcp_env?: Record<string, Record<string, string>>;
  sandbox?: Record<string, unknown>;
  placement?: Record<string, unknown>;
  integrations?: ConfigRoleIntegrations;
  /**
   * The tracker project a root-level seat owns. Vendor-neutral, so on the seat
   * itself and never under `integrations:`; a unit member takes its unit's.
   * Guarded.
   */
  project?: string;
  /**
   * The knowledge container a root-level seat writes in, beside `project`.
   * Guarded.
   */
  space?: string;
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

/**
 * A seat's own integration blocks, as `config.RoleIntegrations` writes them:
 * the chat apps it speaks as and the GitHub App it acts as. Its project and
 * space are not here — they are [ConfigRole]'s own `project` and `space`.
 */
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
  slack?: { bot_token?: string; signing_secret?: string; [key: string]: unknown };
  mattermost?: { bot_token?: string; username?: string; channel?: string; [key: string]: unknown };
  [key: string]: unknown;
}

/** One unit in the company document, as `config.Unit` writes it. */
export interface ConfigUnit {
  name: string;
  type?: string;
  /**
   * The unit's KEY and STABLE IDENTITY, which a name is not — a rename moves
   * nothing keyed on this. The engine mints it from the name on every write
   * that stores a unit without one, so every stored unit carries it; [OrgUnit]
   * carries the same value as `id`.
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
  /** The handle of the seat it is about. */
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
    };

/**
 * One seat on the ANONYMOUS org projection (`/org`, the snapshot's `org` key
 * and the `org` push), as `internal/api`'s `OrgSeat` writes it.
 *
 * AN EXPLICIT PUBLIC SHAPE, never a marshal of the config's own role. The
 * projection used to be `config.Role` verbatim, so every field a role gained
 * reached anonymous readers the day it landed: contact identities, per-seat
 * credentials, placement labels and sandbox setup commands among them. What
 * is NOT here — `email`, `contact`, the authored `llm_*` fields,
 * `schedules`, `integrations`, `mcp_env` — is read through the `config`
 * query instead, as [CompanyDocument], which takes Admin.
 *
 * `llm` and `tool_sources` are RESOLVED by the engine, never the authored
 * fields of the same name: a client re-running the precedence between a flat
 * `llm_<phase>`, the `llm` mapping and the company's default provider — or
 * the grant rule between a shared server, a template and a unit's `mcp_env` —
 * would be a second implementation of a rule the engine already applies.
 *
 * `handle` is what the DOCUMENT declares — the engine mints one from the name
 * on every write that stores a seat without one, and never moves it after. The
 * handle a seat actually runs under is [DerivedSeat.handle]; this client never
 * derives one of its own.
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
  /** This seat's own ceilings per window, as written; absent = only the company's. */
  token_budget?: TokenBudget;
  /**
   * Every phase's provider chain as a turn resolves it, keyed by phase wire
   * name (`execute`, `review`, …): the first key is the model the phase runs
   * on, every later one a fallback in the order tried. Provider KEYS only.
   * Absent on a human seat, on a company configuring no provider, and for
   * every reader but an admin — so an absent one says the seat has none only
   * to an admin (`lib/seats.ts`'s `resolvedAbsence`).
   */
  llm?: Record<string, string[]>;
  /**
   * Where this seat's tools come from, in the registry's origin grammar:
   * `builtin`, then `mcp:<server>` per server it is GRANTED, in declaration
   * order. The grant, not what is running — the node heartbeat's MCP report
   * says whether a server started. Absent on a human seat and for every
   * reader but an admin.
   */
  tool_sources?: string[];
  /**
   * The Crewlet character and the colour this agent seat is drawn as, as
   * the engine resolved them: its defaults are already filled in. Absent on
   * a human seat, which is drawn as the person it is.
   */
  avatar?: { character: string; color: string };
}

/**
 * One unit on the org projection, as `internal/api`'s `OrgUnit` writes it.
 *
 * `project`, `space`, `mcp_env` and `schedules` are GUARDED and are not here:
 * `internal/api/orgprojection_test.go` classifies every field of `config.Unit`
 * and fails the build over an unclassified one. A screen that needs one of
 * them reads the company document through the `config` query.
 */
export interface OrgUnit {
  /**
   * The unit's KEY — its declared `id`, which the engine mints from the name
   * where the document declares none — which is what a `manages:` entry and a
   * seat's `unit:` resolve, what `/config/units/{id}` addresses, and what every
   * route to a unit carries. `name` is prose and two units may share one, so
   * nothing addresses a unit by it.
   */
  id: string;
  name: string;
  type?: string;
  purpose?: string;
  /** The seat HANDLE the document declares as lead. Empty when it inherits one. */
  lead?: string;
  goals?: string[];
  channel?: string;
  /** Free-text knowledge references. NOT a read scope. */
  knowledge?: string[];
  roles?: OrgSeat[];
  children?: OrgUnit[];
}

/**
 * The company as the overview's projection carries it: its charter, its seats
 * and units in the positions the document wrote them, and the hierarchy the
 * ENGINE derived from that document.
 *
 * `derived` is absent only on `{}`, a node running no company, and comes from
 * the same document as the tree beside it in the same call, so the two pair
 * by construction. A screen never reconstructs what it says from the
 * authored tree: `lib/seats.ts` lays it over the tree as the authority.
 */
export interface OrgProjection {
  name?: string;
  mission?: string;
  vision?: string;
  policies?: string[];
  /**
   * The company's ONE clock (ADR-0018), as the ENGINE resolved it: the IANA
   * zone the document writes, or `UTC` where it writes none — never empty on
   * a running company, so a reader has nothing to default. Every day the
   * engine cuts ("today", a due band, an overdue mark) is cut on it, so a
   * screen deciding which DAY something falls on cuts on this rather than on
   * the browser's own zone. Absent only on `{}`, a node running no company.
   */
  timezone?: string;
  /** The company's own ceilings per window, as written; absent = uncapped. */
  token_budget?: TokenBudget;
  /**
   * Which tracker and knowledge base the company runs, as the engine
   * resolved them (see `contract/services.ts`). Every reader is handed it.
   * Absent only on `{}`, a node running no company: not known yet, never
   * "none".
   */
  services?: OrgServices;
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
   * the GUARDED config answers and omitted from the overview's projection,
   * which hands a reader no document to point into.
   */
  path?: string;
  handle: string;
  name: string;
  kind: string;
  /**
   * The authored path of the seat's effective home unit, "" at the root.
   * Omitted from the overview's projection with `path`: there, membership is
   * [DerivedUnit.seats].
   */
  unit_path?: string;
  /**
   * A root seat the engine moved into a unit because of its `unit:` reference.
   *
   * Absent wherever `path` is — the projection hands a reader no document to
   * have been moved in, so "not moved" would be an answer rather than an
   * absence. Read it as false.
   */
  placed_by_ref?: boolean;
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
  /** Authored path; omitted from the overview's projection, as on a seat. */
  path?: string;
  /**
   * The unit's KEY — what a `manages:` entry and a seat's placement resolve.
   * `name` is prose and two units may share one, so a derived unit is matched
   * to its authored unit by this.
   */
  id: string;
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
   * and Worklet's comment tool delivers although it is a builtin.
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
 *  THE NAMES ARE THE SERVER'S. This type used to declare `scope`,
 *  `last_run` and `last_outcome`; the server sends `scope_type`, `scope_id`
 *  and `scope_name` and has no last-run field at all — the ledger is a
 *  separate answer — so three of its fields rendered as `undefined` on every
 *  row. */
export interface ScheduleRow {
  /** `role` or `unit`. */
  scope_type: string;
  /** The scope's IDENTITY, which is what the at-most-once ledger keys a fire
   *  on: a seat's agent id, or a unit's key. Opaque; read `scope_name` to
   *  show or link it. */
  scope_id: string;
  /** The same scope as a person reads it: the seat's handle, or the unit's
   *  key. Display and links only; nothing is filed under it. A seat the
   *  company no longer holds has no handle, and the engine sends its id. */
  scope_name: string;
  name: string;
  cron: string;
  /** The zone this schedule FIRES in, resolved by the engine: its own
   *  `timezone`, or the company's clock (ADR-0018) where it names none — so
   *  it is never empty, and a reader defaults nothing. */
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
  /** ABSENT when there is no next fire: disabled, an expression that cannot
   *  be parsed, or a date the calendar never reaches. The engine used to send
   *  Go's zero instant here, which parses as a time two thousand years ago —
   *  so every "when next" read it as overdue. */
  next_run?: string;
  /** Why `next_run` is empty when the reason is a DEFECT rather than a choice
   *  — an unparseable cron, an unknown timezone. Empty for a healthy row and
   *  for a merely disabled one, so a blank cell is never the only symptom. */
  problem?: string;
}

/** One fire, from the at-most-once dispatch ledger.
 *
 *  `outcome` has exactly three values — `fired`, `skipped_catchup` and
 *  `skipped_paused` (the runner seat was paused when the fire came due) —
 *  because this is a DISPATCH ledger and not a turn-outcome one. Nothing here
 *  can say a turn failed; the turn says that. */
export interface ScheduleRunRow {
  scope_type: string;
  /** See `ScheduleRow.scope_id`: the identity this fire is keyed on. */
  scope_id: string;
  /** See `ScheduleRow.scope_name`, resolved at read time from the company the
   *  node is running. */
  scope_name: string;
  schedule_name: string;
  /** The tick this fire stands for, as the ledger's own at-most-once key. */
  fire_label: string;
  target_handle: string;
  scheduled_at: string;
  fired_at: string;
  outcome: "fired" | "skipped_catchup" | "skipped_paused" | "";
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
  scope_name: string;
  schedule_name: string;
  runs: ScheduleRunRow[];
  /** The page filled, so older fires are past it. */
  truncated: boolean;
}

// ---------------------------------------------------------------------------
// Health and fleet
// ---------------------------------------------------------------------------

export interface FleetNode {
  id: string;
  roles: string[];
  /**
   * How the node's broker takes part in the fleet's — derived from its own
   * `stream` block, never from its roles. `unknown` for a node advertising a
   * kind this build does not know (a newer build's).
   */
  broker: BrokerKind | string;
  labels?: Record<string, string> | null;
  /** The lease's fencing token — node id plus a per-process suffix. */
  owner?: string;
  /** The seat-ownership protocol version this node speaks. */
  protocol?: number;
  /** A COUNT, not a list: which seats is `FleetAnswer.seats`. */
  seats: number;
  /** Seconds until this node's own lease expires. */
  expires_in?: number;
  /**
   * The epoch this node last ATTEMPTED, with `config_status` saying how that
   * went and `config_error` why it failed — never, by itself, the epoch it
   * runs: a node that refused the fleet's revision reports that revision's
   * epoch beside `error`, and goes on serving the one before it. Which revision
   * it runs is `config_serving_revision_id`; whether that is the fleet's is
   * `configStanding`'s question (`lib/configStanding.ts`).
   */
  config_epoch?: number;
  config_status?: string;
  config_error?: string;
  /**
   * WHICH revision this node last attempted, at `config_epoch` — which the
   * epoch does not say.
   *
   * Carried on the apply record for exactly this reason: a fleet view is read
   * while nodes are mid-transition, and a node still on the previous revision
   * is what an operator is looking for. Whether the attempt took is
   * `config_status`; what the node runs is `config_serving_revision_id`.
   */
  config_revision_id?: string;
  /**
   * The revision this node's engine RUNS: the one its last successful apply
   * installed, or the one it booted on; empty while it runs none. A node that
   * refused the fleet's revision goes on serving the one before, and this is
   * the field an operator reads to see which.
   *
   * NOT "IT DIFFERS FROM `config_revision_id`" AS THE TEST FOR A REFUSAL: a
   * refused re-activation of the revision a node already runs — the
   * credential-rotation gesture — names one revision in both, and read that
   * way a refused rotation is a node that converged. `config_status` (`error`
   * or `degraded`) is what says the attempt was refused; this says what runs
   * regardless (`coord.NodeApply.Serving`).
   */
  config_serving_revision_id?: string;
  /**
   * The revision this node would COME BACK ON if it restarted now: its boot
   * row's, as its reconciler last read or wrote it. Absent from a node that
   * boots on nothing — one whose own database is discarded at every start, one
   * that has never applied a revision and holds no offline choice, and a build
   * older than the field — and never absent because the row could not be read.
   *
   * A THIRD FACT, not a reading of `config_serving_revision_id`: what a node
   * boots on moves only once an apply has SUCCEEDED, and that write is local
   * and best effort, so while a failed one is retried the node runs a revision
   * it would not start on and the two names differ (`coord.NodeApply.Boots`).
   */
  config_boot_revision_id?: string;
  /**
   * The epoch this node last APPLIED — its reconciler's own progress, the
   * fleet view's copy of the `applied_epoch` its health push carries — beside
   * `config_epoch`, the one it last ATTEMPTED. The two differ exactly when
   * `config_status` says that attempt failed: a node that applied 5 and
   * refused 6 reports `config_epoch` 6 and this 5, and only this says the
   * epoch it runs. A save is judged on a node by comparing the saved epoch
   * with this, as the node's own push is judged (`coord.NodeApply.Applied`).
   *
   * ABSENT, never 0, from a node that has applied nothing since it started —
   * it runs what it booted on, or nothing — and from a build older than the
   * field: a 0 is an epoch a comparison would read.
   */
  config_applied_epoch?: number;
  config_reported_at?: string;
  /**
   * How far this node's state-log appliers have come up — `ready` of `total`,
   * one per registered domain.
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
  appliers_ready?: number;
  appliers_total?: number;
  /**
   * How many turns are running on this node, off its presence heartbeat.
   * ABSENT, never 0, when its last heartbeat carried no status, and present on
   * every heartbeat that carried one: a published status always states it,
   * beside `draining`. So it also says whether a status was published at all,
   * which is how an absent `posture` is read.
   */
  in_flight?: number;
  draining?: boolean;
  started_at?: string;
  /**
   * The node's config posture, its control plane's own word: `serve`, `wait`,
   * `shed`, `isolated` or `stuck`.
   *
   * ABSENT FOR ONE OF TWO REASONS, which `in_flight` tells apart. With it
   * present, the node could not read its control plane on its last heartbeat
   * and published the rest of its status without a posture, rather than
   * broadcast the `serve` its readiness probe falls back to
   * (`coord.NodeStatus.Posture`). With it absent too, that heartbeat carried
   * no status at all.
   */
  posture?: string;
}

/**
 * The fleet answer's object-store block — `queries.FleetObjects`.
 *
 * THE ENGINE'S OWN RENDERING IS THIS SCREEN'S FIXTURE: the card's suite reads
 * `internal/api/testdata/objects_answer.json`, which the Go renderer writes,
 * so a field renamed on either side fails a test until the other follows it.
 */

/** One of [OBJECTS_STATES]. */
export type ObjectsState = (typeof OBJECTS_STATES)[number];

/** One collection pass: what the store held, and what no row named any more. */
export interface ObjectCollect {
  at: string;
  /** Listed the whole store and judged every object old enough to judge. */
  completed: boolean;
  listed: number;
  /** Past the day's grace, and so judged. */
  aged: number;
  deleted: number;
  referenced: number;
  /** Uploads begun more than a day ago and never finished, abandoned. */
  abandoned: number;
  /** Why the pass stopped judging; absent when it ran in full. Its counts stand either way. */
  skipped?: string;
  /** What kept the pass from abandoning unfinished uploads; the collection stands. */
  sweep_error?: string;
  error?: string;
}

/** One file whose bytes the store cannot give back. */
export interface ObjectMissingFile {
  /** The object the file's row names. */
  object: string;
  /** The file, as `PROJECT/path`. */
  named_by: string;
  /** The store holds it with the wrong bytes, rather than not at all. */
  damaged?: boolean;
}

/** What an audit that ran to its end found. */
export interface ObjectFindings {
  at: string;
  /** Over an estate that was complete; when false the counts are floors. */
  completed: boolean;
  referenced: number;
  missing: number;
  damaged: number;
  /** The first hundred files that cannot be read; `missing` + `damaged` is the whole count. */
  missing_files?: ObjectMissingFile[];
}

/** The last audit attempt: every object a row names, asked of the store. */
export interface ObjectAudit {
  at: string;
  /** Asked about every named object, over an estate that was complete. */
  completed: boolean;
  referenced: number;
  missing: number;
  damaged: number;
  error?: string;
  /**
   * What the last audit to run to its end found — this attempt, or the one
   * before it when this one failed. Absent before any has.
   */
  found?: ObjectFindings;
}

/** The collector's last report, every field but the passes present. */
export interface ReportedObjects {
  state: "reported";
  /** `nats`, or `s3:<endpoint>/<bucket>/<prefix>` — the store the fleet agreed on. */
  backend: string;
  /** The data node that ran the passes. */
  node: string;
  /** Absent until a pass of that kind has ended. */
  collect?: ObjectCollect;
  audit?: ObjectAudit;
}

/**
 * Where the company's files are kept, and what the collector last found.
 *
 * THREE STATES NAMED APART rather than folded into an empty report: a store
 * that would not give the record up, a fleet whose collector has not reported
 * yet, and a report.
 */
export type FleetObjects = { state: Exclude<ObjectsState, "reported"> } | ReportedObjects;

/**
 * The state log's retention document, as `crewlet retention status --json` and
 * `GET /worklet/retention` both serve it.
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
  /** On a log that claims identity, the top of `max_bytes` kept for the
   *  records that install or lift a gate, so an eviction still lands on a log
   *  full for everything else. ABSENT on a log that keeps none. */
  reserve_bytes?: number;
  /** Of the ceiling ORDINARY writes are held to — `max_bytes` less
   *  `reserve_bytes`. ABSENT when the broker could not be asked — which is not
   *  zero headroom. */
  headroom_fraction?: number;
  /** What the log took in over the trailing day — the rate `log_ceiling_short`
   *  holds the ceiling against `min_age` of. ABSENT where nothing was measured
   *  (a compacted log, one in its first two days, a node whose trim has not
   *  ticked), and `0` only for a log that took in nothing: the two are
   *  opposite facts and must never share a rendering. */
  bytes_per_day?: number;
  /** Everything below it may already be gone; it never moves down within a
   *  generation. Meaningful only where `trim_floor_state` is `published`. */
  trim_floor: number;
  /** What the last tick concluded may be removed — zero while blocked.
   *  Meaningful only where `trim_floor_state` is `published`. */
  trim_to: number;
  /**
   * Whether the floor, the conclusion, the terms and the blocking term are a
   * conclusion at all. After every reanchor the trim has concluded nothing
   * about the adopted stream until its first tick on it, and that row read
   * as "floor 0 · advancing" — about a trim that had looked at nothing.
   */
  trim_floor_state: RetentionTrimFloorState;
  /** An EMPTY LIST where nothing is concluded — never null. */
  terms: RetentionTerm[];
  blocked_by?: RetentionTermName;
  blocked_since?: string;
  /** Why the answering node refuses every read of this domain right now, and
   *  which finding is behind a `wrong_stream`. ABSENT while it serves them. */
  not_ready?: RetentionRefusal;
  /** Why it refuses this domain's WRITES while serving its reads — a peer's
   *  rows hold records the log lost (`log_truncated`). */
  writes_refused?: RetentionRefusal;
  /** The sentence a blocked trim leads with. */
  prose?: string;
  /** The snapshot loop's OWN skip reason — a different problem from a blocked
   *  trim, with a different remedy, which is why it is its own field. */
  snapshot_blocked_by?: string;
  /**
   * The answering node could not read this log's evictions when it assembled
   * the report — a failed store read, or the replicated estate closed for an
   * adoption's rename or a shutdown. An unread log contributes no tombstone,
   * so every node reads as NOT evicted there: a guess, not a fact, and never
   * proof that a node was readmitted. ABSENT when they were read.
   */
  evictions_unreadable?: boolean;
  /**
   * When the backup this log's trim counts was taken: the newest verified
   * point the `backup_floor` policy counts that covers this log's stream — a
   * node's copy, or the operator's acknowledgement of this log. ABSENT where
   * none covers it, which is the state the backup term blocks on and
   * `backup_age` names, per log.
   */
  backup_at?: string;
}

/** Why the answering node refuses a domain. */
export interface RetentionRefusal {
  /** A read refusal (`wrong_stream`, `stalled`, …) or a write refusal (`log_truncated`). */
  code: string;
  /** Kept open, so a cause a newer node names is shown rather than dropped. */
  causes?: (RetentionIdentityCause | (string & {}))[];
  /** The sentence the refusal carries everywhere else it is met. */
  detail: string;
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
  name: RetentionTermName;
  state: RetentionTermState;
  /** Meaningless unless `state` is "ok", and zero in every other state. */
  seq?: number;
  detail?: string;
  /** On every term. The screens print it for the term holding the trim, or
   *  setting its floor, which is the one an operator acts on. */
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
  /** Whether `seq` compares with the log's at all. */
  generation_state: RetentionGenerationState;
  /** ABSENT rather than zero when the stream could not be read — and for a
   *  position from another generation, whose sequence is in another space. */
  lag?: number;
  deferred?: number;
  /** The node's own report that the log holds another record at its
   *  checkpoint than the one it consumed there. */
  log_diverged?: boolean;
  /** The stream its rows are keyed to, and the record its checkpoint stands
   *  on — what a reanchor weighs. ABSENT where the node did not publish them. */
  stream_created_at?: string;
  checkpoint_stored_at?: string;
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

/**
 * One standing alarm: `statelog.Alarm`. ONE PER KIND, whatever number of logs
 * it stands on, which is why its length is the health envelope's count.
 */
export interface RetentionAlarm {
  kind: string;
  /** The measurement, or the most pressing log's where it stands on several. */
  detail: string;
  remedy: string;
  /**
   * Every measurement the alarm stands on, each naming the logs it was taken
   * on. Under `backup_age` these are the logs whose own counted backup is
   * missing or older than the policy — each log is acknowledged and aged on
   * its own — and one of them the backup term has held past its replay window
   * carries the term there, raising no `trim_blocked` of its own. Absent for a
   * node-wide condition.
   */
  domains?: RetentionAlarmDomain[];
}

/**
 * One measurement an alarm stands on, and the logs it was taken on:
 * `statelog.AlarmDomain`. ONE LOG where a term is set, since a held trim's
 * instant and count are its own; EVERY LOG MEASURED IN THE SAME WORDS
 * otherwise, so a young fleet's missing backup arrives once, naming every log,
 * and is drawn once.
 */
export interface RetentionAlarmDomain {
  /** The logs, in the report's own order: never empty. */
  domains: string[];
  detail: string;
  /** The trim term holding this log, where the alarm is about its trim: under
   *  `trim_blocked`, and under `backup_age` for a hold folded there. */
  term?: RetentionTermName;
  /** When this log's trim stopped advancing. */
  since?: string;
}

/**
 * What a retention gate answered once the gesture was past its judgement: ONE
 * GESTURE over every identity-claiming log, answered per log.
 *
 * The shape is `api.GateAnswer`, and the fixtures this dashboard's suite
 * renders it from are the engine's own rendering
 * (`internal/api/testdata/gate_answer.json`) — the dialog read a top-level
 * `outcome` for as long as the route had stopped writing one, on fixtures it
 * had typed itself, and rendered every gesture as "no acknowledgement".
 */
export interface RetentionGateResult {
  node: string;
  evicted: boolean;
  /** The GESTURE's operation id — what finishes it, sent back unchanged. */
  op_id: string;
  /** Whether every log holds the record durably (`applied` or `pending`). */
  complete: boolean;
  domains: RetentionGateDomain[];
}

/** One log's answer to a gate gesture. */
export interface RetentionGateDomain {
  domain: string;
  stream: string;
  /** This log's own operation, derived from the gesture's. */
  op_id: string;
  /**
   * The write's three-valued outcome (D134), ABSENT when `error` is set: a
   * refusal is not one of the three. `applied` is durable AND in this node's
   * rows; `pending` is durable at the position and unapplied HERE; `unknown`
   * may or may not be on the log.
   */
  outcome?: "applied" | "pending" | "unknown";
  /**
   * Set on an `unknown` THIS node cannot settle: its operation ledger may
   * have lost the row the operation needs, so it published nothing and
   * answers the same gesture the same way every time. Its remedy is another
   * node (`other_node`), never Finish here.
   */
  unvouched?: boolean;
  /** Where the record is durable — ABSENT for `unknown`, which has none: a
   *  zero position would read as a record at the log's origin. */
  position?: { stream: string; generation: number; seq: number };
  /** Why the log gave no outcome, and the refusal's name (`log_full`,
   *  `evicted`, …) where it was a refusal. */
  error?: string;
  reason?: string;
  /**
   * What to do about a log the gesture did not finish — values of
   * [GATE_ACTIONS], kept as strings so an action a newer node sends is shown
   * rather than dropped — and the sentence saying why, naming no surface's
   * controls. Both absent on a finished log.
   */
  actions?: string[];
  hint?: string;
}

export interface FleetSeatLease {
  handle: string;
  /** The node holding it. `owner` beside it is the fencing token, not an id. */
  node: string;
  owner?: string;
  epoch?: number;
  expires_in?: number;
  /**
   * Since when `node` has held it: the tenure's start (RFC 3339, UTC, the
   * coordination store's clock), which a renewal does not move.
   */
  acquired_at: string;
}

export interface FleetDutyLease {
  duty: string;
  node: string;
  expires_in?: number;
}

/** The fleet's activation pointer as the fleet answer renders it. */
export interface FleetActivation {
  epoch: number;
  revision_id: string;
  /** RFC 3339, or empty where the pointer carried no instant. */
  at: string;
  summary: string;
}

export interface FleetAnswer {
  nodes: FleetNode[];
  seats: FleetSeatLease[];
  duties: FleetDutyLease[];
  unplaceable: { handle: string; placement?: string; reason?: string }[];
  unmanned_roles: string[];
  this_node: string;
  /**
   * The epoch every node is meant to converge on. A number, not an id: which
   * revision it stands for is `activation`. 0 when there is no activation or
   * it could not be read.
   */
  target_epoch: number;
  /**
   * The activation `target_epoch` is — which revision, activated when, with
   * the summary whoever activated it wrote. Read in the same read as the
   * epoch, so the two never describe different activations. NULL when nothing
   * has been activated or the pointer could not be read: there is then no
   * target, and no node is behind one.
   */
  activation?: FleetActivation | null;
  /** Where the company's files are kept; absent where the node reads no record. */
  objects?: FleetObjects;
}

// ---------------------------------------------------------------------------
// Learning, conversations, knowledge
// ---------------------------------------------------------------------------

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
  /** A page of the threads the seat holds entries in, newest activity
   *  first — of `conversations_total`. */
  conversations: ConversationRow[];
  conversations_total: number;
  entries: ConversationEntry[];
  /** The node holding the seat, which answered, or `none`. */
  held_by: string;
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
  /** False when this node could not read the channel record at all — which
   *  is not the same fact as a record with no channels in it. */
  available: boolean;
  /** Which channels were asked for (`open`, `closed` or `all`). Absent on an
   *  answer that is not available. */
  state?: string;
  /** True when the record held more channels than one answer carries, so
   *  `channels` is the most recently active of them rather than all of them.
   *  Absent on an answer that is not available. */
  truncated?: boolean;
  /** Why the record could not be read, on an answer that is not available. */
  note?: string;
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
  /** The caller holds Worklet and not Doclet: the knowledge base is not
   *  theirs to search, so nothing was searched, which is never "nothing
   *  matched". The dashboard asks the question only with Doclet access
   *  (`lib/useSearchQuery.ts`), and draws this for an answer that raced an
   *  access change. */
  | "no_access"
  | (string & {});

/**
 * How a search ranks — ONE vocabulary for `knowledge` and `work_search`
 * (`knowledge.Mode` in the engine). The label for `semantic` is "Meaning"; the
 * wire value is never that word, and the engine refuses it.
 */
export type SearchMode = "hybrid" | "keyword" | "semantic";

/**
 * Why a ranked search served less than it was asked for; `""` when it served
 * what was asked. `no_embeddings` is configuration (no provider, or
 * `knowledge.vectors` off); `embedding_failed` is transient (the query's own
 * vector could not be computed this time); `semantic_partial` is part of the
 * fleet losing its meaning half; `unsupported` is a backend with no such
 * ranker (Confluence). Widened for a reason a newer engine adds.
 */
export type SearchDegradation =
  "" | "no_embeddings" | "embedding_failed" | "semantic_partial" | "unsupported" | (string & {});

/**
 * What a ranked search actually did, carried whole on both `knowledge` and
 * `work_search` so one mode control reads either answer.
 */
export interface SearchOutcome {
  /** The mode asked for, resolved (an absent one is `hybrid`). */
  mode: SearchMode;
  /** The ranking the hits came from; `""` when nothing ran — a semantic
   *  search with nothing to rank by meaning answers no hits rather than a
   *  keyword ranking passed off as one. */
  served_mode: SearchMode | "";
  /** The modes this backend can serve AS ASKED right now. A mode missing
   *  from it would be answered degraded. */
  modes: SearchMode[];
  degraded: SearchDegradation;
  /** The fleet the search was divided across, plus how many of the corpus's
   *  buckets no answer covered. Not complete before a search has run. */
  coverage: Coverage & { buckets_missing: number };
}

export interface KnowledgeAnswer extends SearchOutcome {
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
// Worklet and Doclet
// ---------------------------------------------------------------------------
//
// These are the engine's OWN backends. A company on Jira or Confluence has
// none of these questions registered at all, and the screens say so rather
// than drawing an empty board. Every field here is the Go type's own JSON
// tag; the wire shape is the `worklet` and `doclet` packages' own types (for
// example [worklet.TaskDetail], [doclet.Summary] and [doclet.Detail]),
// serialised as they stand.

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
  /** The key is one ANOTHER task claimed first, so it opens that task and this
   *  one is reached by its id. Never open a row by `key` directly: hand it to
   *  `itemAddress`, which is the one place that rule is written. */
  key_collision?: boolean;
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
  /** THE OPT-IN CARD FACTS, present only on an answer that named them in
   *  `fields=` — and then present at zero too (`[]`, `0`), so "asked, and
   *  none" is never an absent key. The same row is what an agent reads
   *  through `list_work_items`, which never asks. */
  tags?: string[];
  /** Live tasks waiting on this one, through the authored `waiting_on`
   *  edges `blocking=` reads — the card's "blocks N". */
  dependents_count?: number;
  /** Questions on the task still waiting for their answer. */
  open_asks?: number;
  spend?: WorkRowSpend;
}

/** What one task has cost, as a card and the most expensive tasks draw it:
 *  tokens only, never a price. */
export interface WorkRowSpend {
  tokens: number;
  turns: number;
  workers: number;
  sent_back: number;
  reopens: number;
}

/** Where `around=` placed one task in an answer's DRAWING order — the list's
 *  own sort, or a board read column by column and lane by lane — over the
 *  whole answer rather than the page. `prev`/`next` are ADDRESSES
 *  (`worklet.ItemAddress`), null at the ends: a neighbour's key, or its id
 *  where another task claimed that key first — stepping to the key would open
 *  the claimant, which is not on this board. `position` is null past the
 *  count's ceiling. On a label board the
 *  order counts cards, so `total_hint` here is the number drawn. */
export interface WorkAround {
  position: number | null;
  prev: string | null;
  next: string | null;
  total_hint: number;
  total_capped?: boolean;
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
  /** Present only when the request named `around=`, and NULL when that task
   *  is not in this answer — it moved off the board — rather than absent. */
  around?: WorkAround | null;
}

/** One entry in a container's view strip.
 *
 *  A BUILTIN ROW HAS NO ID. Every container has a list, a board, a calendar and
 *  a timeline without anybody saving one — they are not objects, so there is
 *  nothing to rename, protect, rank or pin — and a screen renders them from
 *  `key`. */
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
  /** How many tasks the view selects when RUN — the board's own `total_hint`
   *  for it — carried only on a row pinned for the viewer, and only when the
   *  strip was asked with `counts=true`. Absent is "not counted", never zero. */
  count?: number;
  /** The count stopped at the engine's ceiling, so it is a floor. */
  count_capped?: boolean;
  /** Why a pinned row asked for a count carries none: the view no longer
   *  compiles against this company (a filtered field was archived, say). */
  count_refused?: string;
}

/** One file kept in a project, as a listing draws it — everything but the
 *  key of the object holding its bytes, which only a download reads. */
export interface WorkFile {
  project: string;
  path: string;
  content_type?: string;
  hash?: string;
  size: number;
  /** What `If-Match` takes back, as the download's ETag carries it. */
  version: number;
  created_by?: string;
  created_at: string;
  updated_by?: string;
  updated_at: string;
  /** Set on a removed file, which only a listing asking for them returns. */
  removed_by?: string;
  removed_at?: string;
}

export interface WorkFilesAnswer {
  project: string;
  files: WorkFile[];
  /** The path to pass as `after` for the next page; absent on the last. */
  next?: string;
  read_level?: ReadLevel;
  log_seq?: number;
  applied_through?: number;
  log_lag?: number;
  complete: boolean;
  incomplete?: WorkIncomplete;
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

/** A task's two unit references as a reader renders them — see
 *  [WorkItemDetail.units].
 *
 *  BOTH HALVES, ALWAYS, because they answer different questions and a screen
 *  draws them side by side: `filed` is the team the work belongs to and never
 *  moves, `routing` is whose lead hears about it now. */
export interface WorkItemUnits {
  filed: WorkUnitRef;
  routing: WorkUnitRef;
}

export interface WorkLeadRef {
  handle?: string;
  kind?: "agent" | "human" | "operator" | "system";
}

/** A project's task census, one number per status group.
 *
 *  MAINTAINED by the applier on every status-group change and every arrival or
 *  departure, never aggregated per poll — which is what makes a sixty-second
 *  refresh four column reads rather than a scan of every task in the
 *  company.
 *
 *  There is no `open`: it folded the work nobody has started into the work
 *  somebody is doing, which are the two states a reader acts on oppositely.
 *  Every unfinished item is `todo + active` — see `unfinished` in
 *  `lib/work.ts`. */
export interface WorkTaskCounts {
  /** Not started. */
  todo: number;
  /** Started — the `active` status group. */
  active: number;
  done: number;
  closed: number;
}

/** When a project's work last changed, and who changed it.
 *
 *  MAINTAINED beside the counts, by the apply that writes the project's own
 *  history row — so this is the head of that project's activity feed and never
 *  disagrees with it. A turn's spend, a board re-order and an edit to the
 *  project's own settings do not move it: none of them is the work changing.
 *
 *  `actor_kind` is which of the four kinds the handle belongs to, because
 *  `ana` the person and `ana` the seat are different answers; an `operator` is
 *  a token acting for the company and belongs to no seat. Either field may be
 *  empty — a commit can name nobody — and the instant is what says the answer
 *  exists at all. */
export interface WorkLastChange {
  at: string;
  actor?: string;
  actor_kind?: "agent" | "human" | "operator" | "system";
}

export interface WorkProjectRow {
  key: string;
  name: string;
  purpose?: string;
  unit: WorkUnitRef;
  lead: WorkLeadRef;
  default_assignee?: string;
  /** The day (`YYYY-MM-DD`, on the company's clock) the lead means the
   *  project to be finished. ABSENT when none is set — not the earliest
   *  date, so `sort=target` puts those rows last in both directions. */
  target_date?: string;
  task_counts: WorkTaskCounts;
  /** ABSENT for a project no work has ever been filed into, which is a
   *  different fact from a project whose work is old — see
   *  {@link WorkLastChange}. */
  last_change?: WorkLastChange;
  archived?: boolean;
  version: number;
}

/**
 * How many projects each archival set holds, under the listing's own `q` and
 * `unit` and nothing else.
 *
 * `archived=` SELECTS one set, which is what makes the listing honest and also
 * what leaves an empty answer ambiguous: a screen asking for the live projects
 * and getting none cannot tell a company with no projects from one that has
 * archived every one of them, and a reader acts on those two oppositely. The
 * census is the same question minus the archival term, so the screen never
 * guesses and never asks twice.
 */
export interface WorkProjectCensus {
  active: number;
  archived: number;
}

export interface WorkProjectsAnswer {
  projects: WorkProjectRow[];
  /** The count of the set that was ASKED for — `census` of that mode. */
  total: number;
  truncated?: boolean;
  census: WorkProjectCensus;
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
  /** The notice's PACKED log position, `(generation << 40) | seq`, read by the
   *  engine from the notice's own history row. */
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
  /** EXCEPTIONS to `seen_through`: `unread` holds notices at or below it
   *  marked unread again, `read` notices above it marked read out of order. */
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
  /** How far this person has read. The generation is OMITTED when it is
   *  zero — a record carrying one is stamped for the build that stores it. */
  seen_through?: { stream: string; generation?: number; seq: number };
  /** The record's version: what `set_priorities`' `if_match` states. */
  version: number;
  /** How far ahead a snooze may be set, in SECONDS — the engine's bound, so a
   *  screen offers only the presets the write accepts. */
  max_snooze_ahead: number;
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
  /** The structure an ask carries when it asks somebody to choose. Set only
   *  on the comment that asks, and never changed after. */
  decision?: WorkDecision;
  /** The option an answer chose, by id — one of the answered ask's own. */
  choice?: string;
  mentions?: string[];
  resolved?: boolean;
  resolved_by?: string;
  resolved_at?: string;
  removed?: boolean;
  created_at: string;
  updated_at?: string;
}

/** One page of a task's thread, walking back from the newest (`work_comments`).
 *  The page is in the order it was written; `next_cursor` reads the page
 *  before it and is absent when this reaches the first comment. */
export interface WorkCommentsAnswer {
  item: string;
  key: string;
  title: string;
  comments: WorkComment[];
  next_cursor?: string;
  read_level?: ReadLevel;
  log_seq?: number;
  applied_through?: number;
  complete?: boolean;
  incomplete?: WorkIncomplete;
}

/** Another task as a reader names it (`worklet.TaskRef`). */
export interface WorkTaskRef {
  id: string;
  key: string;
  /** The key is one another task claimed first, so it opens THAT task and
   *  this one is reached by its id (`itemAddress`). */
  key_collision?: boolean;
  title: string;
  status: WorkStatus;
}

/** One tool a turn called, and how many times (`worklet.TurnTool`). */
export interface WorkTurnTool {
  name: string;
  calls: number;
}

/** One turn charged to a task, its segments folded (`worklet.TaskTurn`). */
export interface WorkItemTurn {
  /** The run the segments share — what the trace link carries. */
  turn_id: string;
  /** "Turn n": the task's own count of turns. Zero for a turn with no counted
   *  segment on this task (more of a turn charged elsewhere). */
  ordinal: number;
  seat: string;
  trigger?: string;
  /** How many completions were charged here; more than one for a turn that
   *  parked on a coding run. */
  segments: number;
  /** Input plus output, over every segment — what the task's spend sums. */
  tokens: number;
  cache_read: number;
  rounds: number;
  wall_ms: number;
  workers?: number;
  sent_back?: number;
  /** How the turn ended, off its newest segment: its decision, `failed`,
   *  `stopped` where a person's pause ended it, or `suspended` while a coding
   *  run is still out. */
  outcome: string;
  phases: string[];
  /** The phase that failed, when `outcome` is `failed` because one did.
   *  `phases` lists it too — it ran — so a card marks that chip rather than
   *  ticking every phase beside a pill that names none. */
  failed_in?: string;
  /** What it did, in the agent's own words. Absent when the turn failed or
   *  parked before any segment wrote one. */
  summary?: string;
  /** What the newest review that sent the work back asked for. */
  review?: string;
  tools?: WorkTurnTool[];
  /** When the newest segment landed. */
  at: string;
}

/** One page of the turns charged to a task, newest first (`work_item_turns`).
 *  From the tracker's own rows — the account the task's spend sums — so it
 *  outlives the event history and answers on any node. */
export interface WorkItemTurnsAnswer {
  item: string;
  key: string;
  /** The key is one ANOTHER task claimed first, so it opens that task and this
   *  one is reached by `item`. Never open the task by `key` directly: hand it
   *  to `itemAddress`, which is the one place that rule is written. */
  key_collision?: boolean;
  turns: WorkItemTurn[];
  next_cursor?: string;
  read_level?: ReadLevel;
  log_seq?: number;
  applied_through?: number;
  complete: boolean;
  incomplete?: WorkIncomplete;
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
   *  its importance, since every subtask a merge re-parents is quiet by
   *  construction. */
  quiet?: boolean;
  /** The item's hand-off counter AS THIS COMMIT LEFT IT — the "3" of "hand-off
   *  3 of 8" on a row that moved the assignee. Absent on a row the answering
   *  node holds no count for, never a zero it did not derive. */
  reassignments?: number;
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
  /** The other end's `key_collision`: its key opens another task, so this end
   *  is reached by `other`. See `linkedItem`. */
  key_collision?: boolean;
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
  /** How many delegated tasks its turns ran, and how many reviews sent the
   *  work back — the two counts that say WHY a task was expensive. */
  workers?: number;
  sent_back?: number;
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
  /** What the tasks this answer's `history` POINTS AT are called, id to item
   *  key — the same map `WorkActivityAnswer.keys` carries, asked of one task's
   *  own rows.
   *
   *  A relation, parent or cascade delta names the other end by its ID,
   *  because a key belongs to that task's own row and a history row is written
   *  once by every data node and repaired by nothing. Neither side can fix
   *  "Parent: — → 1d573f85-…" alone: the engine may not put a key on the
   *  record, and this build holds no map to resolve one with. It is what
   *  `LabelContext.taskKey` is threaded from.
   *
   *  PRESENT ONLY WHERE THE HISTORY IS, since those are the rows it labels,
   *  and an id the answering node holds no row for is simply ABSENT — a
   *  renderer falls back to the id, which is a value somebody set. */
  keys?: Record<string, string>;
  links?: WorkLink[];
  /** The task this one is filed under, named — absent for a top-level task
   *  and for a parent this node holds no row for. */
  parent?: WorkTaskRef;
  /** The task's custom-field values, ANNOTATED — see [WorkFieldValue]. The
   *  raw map stays on the task; this is the reader's view of it. */
  fields?: WorkFieldValue[];
  /** The task's two unit references RESOLVED against the org chart, on exactly
   *  the terms `fields` is the custom-field map's reader view: the strings
   *  stay on the task because they are the record, and this is what a person
   *  reads.
   *
   *  It exists because what those strings hold is the unit's KEY — its `id` on
   *  a company that gave its units one, which is a word chosen so that a
   *  rename moves nothing and therefore a word nobody reads. `key` repeats
   *  exactly what the row holds, so a filter built from it reaches the same
   *  rows, and `resolved: false` is the finding "this names a team the chart
   *  no longer has".
   *
   *  ABSENT for a task filed into no team at all, because that is what its two
   *  empty strings already say. */
  units?: WorkItemUnits;
  /** Pages the thread backwards, and is empty when this page is all of it. */
  comments_cursor?: string;
  /** The SAME predicate WorkSummary.blocked carries — an open dependency edge
   *  — computed by the server in the same transaction as the task, so the
   *  badge here and the badge on the board row cannot disagree. It is on the
   *  ANSWER rather than on the task because it is derived rather than stored:
   *  `links` say what the relations are, not whether any blocker is open. */
  blocked?: boolean;
  /** The task's `key_collision`, on the ANSWER for `blocked`'s reason: it is
   *  derived rather than stored. The task's key opens the task that claimed
   *  it first, so everything this screen hands onward — the board link, a
   *  peek, a call to copy — goes by `detailItem`'s address. */
  key_collision?: boolean;
  /** Where the due date stands against the COMPANY's today, computed where
   *  a row's `overdue` is: whole calendar days to the due day (negative once
   *  it has passed) and whether it is overdue (open work only). Absent for a
   *  task with no due date. A renderer never re-derives it from the
   *  browser's clock, which read "1d ago" beside a board marking the same
   *  task two days late. */
  due_standing?: { days: number; overdue?: boolean };
  /** How many times agents may hand this item on before the engine refuses
   *  the next one — the limit `task.reassignments` counts against, served by
   *  the engine that enforces it so no screen carries a figure of its own. */
  reassignment_budget?: number;
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
  /** How many pages sit directly under this one that the SAME listing would
   *  show (its status and skill narrowing applied), so a tree draws an
   *  expander only where one opens onto something. Absent means none. */
  children?: number;
  /** A page with nothing written in it: its body is empty. Derived by the
   *  engine on every read and never stored, so it cannot disagree with the
   *  page. */
  folder?: boolean;
  /** The last change to what the page SAYS (its create, a save or a rename),
   *  which `updated_at` is not: a label, a watch or a move moves that. Who
   *  made it is named as `author` is: a seat's handle for a person or an
   *  agent, a credential's login for one bound to no seat. */
  saved_by?: string;
  saved_at?: string;
}

export interface PagesAnswer {
  pages: PageSummary[];
  limit: number;
  /** How many pages the filter matches IN ALL — never the window's length,
   *  which is what every screen drew as the size of a container before. */
  total: number;
  /** What to pass as `after` for the next window; absent on the last one. A
   *  cursor rather than an offset, because a listing read while seats write
   *  repeats and skips rows under an offset. */
  after?: string;
  /** Who each listed TOOL SKILL reached as a skill over thirty company days,
   *  keyed by page id — sent only on a `skills: true` listing, for every page
   *  in it (an unloaded skill as an empty list). */
  skill_loaded_by?: Record<string, SkillLoad[]>;
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
   *  number in the tree and the list behind it agree. DERIVED beside the
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
  /** The comment this one answers. */
  reply_to?: string;
  created_at: string;
  /** Moved by an edit, and equal to `created_at` on a comment never edited. */
  updated_at: string;
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
  /** Who stopped following the page. A watch is a seat in `watchers` that is
   *  not here: an unwatch keeps the seat in `watchers` and mutes it, so a
   *  later mention does not re-subscribe them silently. */
  muted?: string[];
  created_at?: string;
  trashed_at?: string;
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
  /** This node's checkpoint on the pages log and the prefix of it whose
   *  records it applied — a tracker answer's two numbers, for the same
   *  reason: a node holding a record it cannot decode moves its checkpoint
   *  past it, and only the applied prefix stopping short says it is behind. */
  log_seq?: number;
  applied_through?: number;
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
  /** What the applier DERIVED the page as — a tool skill, the onboarding
   *  page. Beside `page` rather than on it: neither is anything a writer
   *  wrote. */
  skill: boolean;
  onboarding: boolean;
  /** The page's body is empty: a folder, a page that only groups pages. */
  folder?: boolean;
  /** For a tool-skill page: the seats it reached as a skill over thirty
   *  company days, most recent first. Absent on any other page. */
  skill_loaded_by?: SkillLoad[];
  /** The pages and tasks linking here, from the answering node's index;
   *  absent where the node has no index to ask, and where
   *  `linked_from_status` says why the node that has one could not answer. */
  linked_from?: PageBacklinks;
  linked_from_status?: LinkedFromStatus;
  comments?: PageComment[];
  history?: PageRevision[];
  /** The first page of children by title; `children_total` is all of them. */
  children?: PageSummary[];
  children_total?: number;
  /** The parent chain, outermost first — the breadcrumb. */
  ancestors?: PageSummary[];
}

// ---------------------------------------------------------------------------
// Chat
// ---------------------------------------------------------------------------

/**
 * A point on one of the engine's state logs, as an answer reports it —
 * DECLARED ONCE, in `session.ts`, beside the floor that parses the same point
 * off a write's answer: a position this file declared again would be a second
 * shape for one value, and the floor and a chat cursor would be free to come
 * to mean different things by it.
 *
 * Chatlet needs the shape by name for a reason the other answers do not: a READ
 * CURSOR is the PACKED form of one (`(generation << 40) | seq`), so the client
 * has to carry a whole position from a message to the flush rather than a
 * number it read off a field.
 *
 * Several answers above spell the same shape inline with every field optional,
 * and they are left as they are deliberately: those are answers that may omit
 * the position entirely, and widening this type to match would make a cursor
 * built from one a compile-time promise the engine does not keep.
 */
export type { LogPosition };

/** What every chat answer carries beside its rows — `chatlet.Served`. */
export interface ChatServed {
  /** What the read was SERVED at, never what was asked for. */
  read_level: ReadLevel;
  /** False when a deferred record's scope met this read: rows may be missing. */
  complete: boolean;
  /** The point on the chat log these rows were derived through. */
  position: LogPosition;
  /** ABSENT rather than zero when the broker could not be reached. */
  log_lag?: number;
}

/** A room's kind. OPEN, because a newer peer may write one this build does not
 *  know — and a room whose kind this build cannot classify is one the engine
 *  refuses every write on, so the screen renders it read-only rather than
 *  guessing it is public. */
export type ChatKind = "public" | "private" | "unit" | "dm" | "group" | (string & {});

/** Who said something, as `iam.ActorFor` names a principal: a person — or a
 *  machine the identity directory binds to them — writes as `human`, under
 *  the seat they hold; a seat's own turn as `agent`; and `system` is the
 *  engine speaking as itself. `operator` is a credential acting as itself,
 *  which no chat gesture admits, and is here only because a row a newer peer
 *  wrote may carry it — a screen draws it as a person, never as an agent.
 *  Open for the reason [ChatKind] is. */
export type ChatAuthorKind = "agent" | "human" | "operator" | "system" | (string & {});

/** The record op a live frame carries — `chatlet.OpKind`, the same word the event
 *  store's own column holds. Open for the reason [ChatKind] is. */
export type ChatOp =
  | "create"
  | "patch"
  | "members"
  | "post"
  | "edit"
  | "delete"
  | "react"
  | "erase"
  | "prune"
  | (string & {});

/** A room. The NAME IS IMMUTABLE — there is no rename in the value layer — so
 *  an address built from a room's id is stable and its name is safe to cache. */
export interface ChatChannel {
  v: number;
  id: string;
  kind: ChatKind;
  /** Empty for a direct conversation, which has no name and is titled by who
   *  is in it. */
  name?: string;
  topic?: string;
  purpose?: string;
  /** The unit whose room this is, for `kind: unit`. */
  unit?: string;
  /** This room's own horizon in days, overriding the company's. 0 is "for
   *  ever" and absent is "the company's answer", which is why it is nullable
   *  rather than a number defaulting to zero. */
  retention_days?: number | null;
  created_at: string;
  created_by?: string;
  created_by_kind?: ChatAuthorKind;
  /** Present on an archived room, which is readable for ever and writable by
   *  nobody. */
  archived_at?: string;
}

/** One message, whole. */
export interface ChatMessage {
  v: number;
  id: string;
  channel_id: string;
  /** The message this hangs off, empty for a room post. A thread is ONE LEVEL
   *  DEEP: a reply to a reply carries the same root. */
  thread_root?: string;
  author: string;
  author_kind: ChatAuthorKind;
  /** Empty on a tombstone: a delete blanks the body and keeps the row, so a
   *  thread does not lose the message it hangs off. */
  body?: string;
  links?: string[];
  /** The handles this message named, already resolved by whichever surface
   *  read what somebody typed. */
  mentions?: string[];
  /** `@channel` — the room as a whole. */
  collective?: boolean;
  created_at: string;
  edited_at?: string;
  edited_by?: string;
  edited_by_kind?: ChatAuthorKind;
  deleted_at?: string;
  deleted_by?: string;
  deleted_by_kind?: ChatAuthorKind;
}

/** One emoji on one message, with whether this viewer is among the reactors. */
export interface ChatReaction {
  emoji: string;
  count: number;
  mine?: boolean;
}

/** One message as a transcript draws it: the message, where it sits in the
 *  room, and where the record that wrote it sits on the log. */
export interface ChatMessageView {
  message: ChatMessage;
  /** The room's own per-message number, minted by the applier from log order.
   *  It is what a transcript cursor is built from and what a live frame's
   *  contiguity is checked on. */
  channel_seq: number;
  /** The record's point on the log. Its PACKED form is what a read cursor
   *  stores, which is why the position travels rather than the sequence. */
  position: LogPosition;
  reactions?: ChatReaction[];
}

/** The newest message in a room, as the rail previews it. */
export interface ChatMessagePreview {
  message_id: string;
  author?: string;
  author_kind?: ChatAuthorKind;
  /** Bounded by the engine (`chatlet.MaxExcerpt`), and a cut one ends in an
   *  ellipsis — so a preview never has to claim to be the whole message. */
  excerpt?: string;
  channel_seq: number;
  at: string;
  /** The newest message is a tombstone. The room still moved, and the row
   *  still sorts by it. */
  deleted?: boolean;
  position: LogPosition;
}

/** One of the viewer's rooms, as the rail renders it. */
export interface ChatChannelSummary {
  channel: ChatChannel;
  /** The room's own log revision, which a later edit states as its
   *  expectation. */
  revision: number;
  /** THIS VIEWER'S membership flag: every message in the room reaches them. */
  follow_all?: boolean;
  /** Messages above this viewer's cursor, counted to `chatlet.UnreadLimit`
   *  (100). Their own messages and the tombstones do not count. */
  unread: number;
  /** The count stopped at the limit — the "99+" case, stated rather than
   *  inferred from the number. */
  unread_capped?: boolean;
  last?: ChatMessagePreview;
  /** Who is in a direct conversation, which is how one is titled. */
  participants?: string[];
  /** This viewer silenced the room. It suppresses NOTICE and never delivery. */
  muted?: boolean;
}

/** The viewer's own rooms, newest activity first — `chat_channels`. */
export interface ChatChannelsAnswer extends ChatServed {
  channels: ChatChannelSummary[];
  /**
   * Rooms were left out: this person is in more rooms than
   * `chatlet.MaxRailChannels` (256), or more matched than the `limit` the
   * read asked for (this screen asks for none, and so for all of them).
   */
  truncated?: boolean;
  /** Rooms this build could not present — one a newer peer wrote. */
  unreadable?: number;
  /**
   * What a room created WITHOUT a stated visibility will be, from
   * `chat.chatlet.default_channel_private`.
   *
   * IT IS FOR THE FORM TO BE HONEST WITH, not for the form to enforce. The
   * server resolves an omitted kind itself, so this only decides what the
   * visibility selector OPENS on — and it has to, because a form showing
   * "public" while the company default is private misleads a person about a
   * privacy control at the one moment they are deciding it.
   *
   * Absent means public, which is the field's own default.
   */
  default_private?: boolean;
  /**
   * The badges on these rows are a MEASUREMENT.
   *
   * False means the coordination record could not be read: the counts are
   * then zero, the mutes absent and `read` with them, which is not "you have
   * read everything" but "nobody could look". A screen that renders a zero
   * without reading this says the first when the engine said the second.
   */
  read_state: boolean;
  /**
   * This person's WHOLE read state, as the badges were counted against it —
   * every room they muted (the rows mark only the rooms listed), when their
   * do-not-disturb ends, and the `revision` that orders it against every
   * later `chat_cursor_moved`. It is how a tab opened a moment ago knows the
   * state before anything moves it. Absent when `read_state` is false.
   */
  read?: ChatReadState;
}

/** One member of a room. */
export interface ChatMember {
  handle: string;
  role?: string;
  /**
   * Who decides this membership, derived from the ROOM'S KIND: `org` for
   * every member of a unit room, whose membership the org chart restates
   * WHOLE at every activation and no gesture can change (a join, a leave and
   * a members edit are each refused on a unit room); `explicit` for every
   * member of every other room. A screen must never offer to remove an `org`
   * member: the next apply would put them straight back.
   */
  source?: string;
  follow_all?: boolean;
  joined_at: string;
}

/** One room, its membership and what it is true as of — `chat_channel`. */
export interface ChatChannelDetail extends ChatServed {
  channel: ChatChannel;
  revision: number;
  /** The room's high-water mark: the number the next message will take, minus
   *  one. NOT a count — a prune removes rows and deliberately does not move
   *  it. */
  message_seq: number;
  members?: ChatMember[];
  /** Whether the VIEWER is in the room, which a public room answers no to
   *  without that meaning they cannot read it. */
  member: boolean;
}

/** A page of one room, newest first — `chat_messages`. */
export interface ChatMessagesAnswer extends ChatServed {
  channel_id: string;
  messages: ChatMessageView[];
  /** Where the next OLDER page starts, empty at the start of the room. Pass it
   *  back unchanged: it is the room's own per-message number, never an offset
   *  and never an instant. */
  next_cursor?: string;
  /** Rows on this page this build could not decode. */
  unreadable?: number;
}

/** One thread, its root and its replies, oldest first — `chat_thread`. */
export interface ChatThreadAnswer extends ChatServed {
  channel_id: string;
  /** Read even when it is a tombstone, which is what the tombstone is for. */
  root: ChatMessageView;
  replies?: ChatMessageView[];
  /** Who has spoken here, derived by the applier. It is not a follow. */
  participants?: string[];
  /** Where the next page of replies starts — this one pages FORWARD. */
  next_cursor?: string;
  unreadable?: number;
}

/** One message that named the viewer. */
export interface ChatMention {
  message_id: string;
  channel_id: string;
  channel_name?: string;
  channel_kind: ChatKind;
  thread_root?: string;
  author?: string;
  author_kind?: ChatAuthorKind;
  /** What the message says NOW, after any edit. A delete takes the mention
   *  away rather than leaving a tombstone here: a message somebody took back
   *  no longer names you. */
  excerpt?: string;
  /** WHEN YOU WERE NAMED — the instant of `position`'s record: the message's
   *  own for a post, the edit's for an edit that named you where the words
   *  before it did not. A row's time never contradicts its place. */
  at: string;
  /** The message's last edit, absent where it was never edited: the excerpt
   *  may not be the words that named you. */
  edited_at?: string;
  /** The position of the record that named this handle, and the feed's
   *  cursor. It never moves: an edit keeps the mention of every handle it
   *  still names where it was. */
  position: LogPosition;
}

/** A page of the viewer's own @-mention feed — `chat_mentions`. */
export interface ChatMentionsAnswer extends ChatServed {
  handle: string;
  mentions: ChatMention[];
  /** A LOG POSITION where a transcript's cursor is a room's message number.
   *  The two are not interchangeable; both are opaque here. */
  next_cursor?: string;
}

/** One room the viewer may read, as prose names it. */
export interface ChatRoomName {
  id: string;
  /** The address a sentence writes as `#launch`: lower case, never renamed. */
  name: string;
  kind: ChatKind;
  archived?: boolean;
}

/**
 * The name of every room the viewer may READ, joined or not, the archived
 * ones included — `chat_room_names`. A room's name may be spelled exactly as a
 * colour (`#e2e`) or an issue's number (`#2026`), so prose is read by it
 * (`lib/roomNames.ts`); the room finder offers the same rooms.
 */
export interface ChatRoomNamesAnswer extends ChatServed {
  /** The live rooms in name order, then the archived ones, the most recently
   *  archived first. */
  rooms: ChatRoomName[];
  /** Rooms were left out past `chatlet.MaxRoomNames` (1000), the archived
   *  ones first: a name this answer lacks may still be a room. */
  truncated?: boolean;
}

/** One matched message. */
export interface ChatHit {
  message_id: string;
  channel_id: string;
  /** The thread the match was said in, empty on a room post — which is what
   *  lets a result open the thread rather than the room. */
  thread_root?: string;
  author?: string;
  /** What the message says around the words that matched, bounded by the
   *  index itself and marked where it was cut. */
  snippet?: string;
  at: string;
  score: number;
}

/** A ranked search over the rooms this viewer may read — `chat_search`.
 *
 *  It does NOT extend [ChatServed]: the ranking is a read of this node's own
 *  index, which is not on the log at all, so the only half that can be behind
 *  is the visible-room set — which is what `read_level` and `position` here
 *  describe. */
export interface ChatSearchAnswer {
  hits: ChatHit[];
  /**
   * How many rooms this query actually ran over — the DENOMINATOR a reader
   * needs. "Nothing matched" over three rooms and over three hundred are
   * different answers.
   */
  searched: number;
  /**
   * The answering node's index had not caught up with its transcript while
   * it searched, so the hits are drawn from part of it: "the index is still
   * building", which a screen must say rather than "nobody said that".
   */
  building?: boolean;
  /** What the VISIBLE SET was resolved at. */
  read_level: ReadLevel;
  /** This node's applied position, read BEFORE the visible set was resolved,
   *  so it is at or below the read's rather than above it. */
  position: LogPosition;
}

/**
 * One committed chat record, as the socket announces it.
 *
 * EVIDENCE THAT SOMETHING LANDED, NEVER A COPY OF THE MESSAGE. There is no
 * body here on purpose: a frame carrying one would be a second rendering path
 * for a conversation, and the two would disagree the first time an edit raced
 * a reload. What it carries is enough to decide what to do — the room, the
 * message and the CONTIGUOUS per-room sequence.
 *
 * A FRAME NAMING NO ROOM IS A GAP, and carries nothing else: the engine's word
 * that this socket may have missed frames in any room — the node's live relay
 * switched source, or the hub could not hand it a batch — so every screen
 * reads what it holds again (`Store.applyChatChange`).
 */
export interface ChatChange {
  /** The room. Empty ONLY on a gap. */
  channel_id: string;
  /** The record's own word for what happened. Absent on a gap. */
  op?: ChatOp;
  /** The record's idempotency key, so a client recognises its own effect
   *  coming back and renders it once. Absent on a gap. */
  op_id?: string;
  message_id?: string;
  /** The room's own per-message number. Contiguous, so a client holding 41 and
   *  handed 43 knows it missed one. */
  channel_seq?: number;
  thread_root?: string;
  author?: string;
  author_kind?: ChatAuthorKind;
  /** The BROKER'S own instant, which is what every node renders. Absent on a
   *  gap. */
  at?: string;
  /** Where the record sits on the log — what a read cursor stores. Absent on
   *  a gap. */
  position?: LogPosition;
  /** At most `chatlet.MaxExcerpt` bytes of what a card shows, from the record's
   *  routing snapshot — carried by every post and edit an author made, even
   *  one that woke nobody, and ABSENT on a record that carries none (a system
   *  line, a delete, a reaction, a room record). Never a substitute for
   *  reading the room. */
  excerpt?: string;
  /** The handles the message names, from the same snapshot and absent
   *  wherever `excerpt` is — so a system line's post, which writes mention
   *  rows, carries none. */
  mentions?: string[];
  /** For a toast about a room the tab is not looking at. Empty for a direct
   *  conversation, which has no name. */
  channel_name?: string;
  channel_kind?: ChatKind;
}

/** One seat running a turn against a message in a room. */
export interface ChatWorking {
  handle: string;
  /** The message the work will be answered under, which is what puts the
   *  indicator where the reply will land. */
  thread?: string;
  /** The phrase to render, and an empty one renders the bare indicator. The
   *  engine's hub declares it renders words, so every frame it sends carries
   *  one. */
  status?: string;
}

/** Who is in one room right now. None of it is durable and none of it is on
 *  the log. */
export interface ChatRoomPresence {
  /** The people with this room open. Handles, because a person in chat IS a
   *  seat. */
  viewing?: string[];
  /** Composing in it — a subset of `viewing` that expires on its own. */
  typing?: string[];
  working?: ChatWorking[];
}

/** The presence push's payload: the rooms this socket asked about, WHOLE —
 *  a tab that reconnects has no starting point to apply a delta against. */
export interface ChatPresenceFrame {
  rooms: Record<string, ChatRoomPresence>;
}

/**
 * Where one person's eye has reached — `coord.PersonChatRead`.
 *
 * It is NOT on the log: a cursor is a fact about one reader's attention that
 * nobody replays. It is kept under the SEAT the person holds, so whoever holds
 * the seat next inherits it, as they inherit its inbox. It reaches a tab three
 * ways, in this one form: the rail's `read`, the answer to a read-state write
 * the tab made itself ([ChatReadReceipt]), and the `chat_cursor_moved` push,
 * derived from the coordination record and sent to every socket reading as
 * that seat — the one that made the write included — and nobody else's.
 */
export interface ChatReadState {
  /**
   * WHOSE STATE THIS IS — the seat the engine read it for, on every state it
   * sends a client. The `revision` orders two states of ONE seat and nothing
   * else, so a state naming another seat than the one a tab holds — the seat
   * its session is bound to moved — replaces it rather than being compared
   * with it.
   */
  handle: string;
  /** Channel id to the PACKED log position that person has read through.
   *  Packed because it is only ever compared, never decomposed. */
  cursors?: Record<string, number>;
  /** The rooms that raise no badge. A mute suppresses NOTICE, never
   *  delivery. */
  muted?: string[];
  /** When do-not-disturb ends. Absent is not in it. */
  dnd_until?: string;
  updated_at?: string;
  /** The record's own revision, which ORDERS two states of one seat: its
   *  flushes are taken in one order and their pushes can arrive in another,
   *  so a screen keeps the state with the highest. Absent is a seat with no
   *  record. */
  revision?: number;
}

/**
 * What a chat gesture's tool answered: the `receipt` of
 * `POST /operator/act/{tool}` (`protocol/act.ts`), which is the tool's own
 * JSON. The outcome and the position are lifted out of it by the act
 * transport; what is left is what the screen does next.
 *
 * EVERY ONE NAMES ITS ROOM, by id: the room `create_channel` made, the
 * conversation `open_dm` opened or reached, the room a message was said in or
 * a membership moved. A screen goes there only on an outcome that landed — an
 * id echoed by an answer nobody can vouch for may name nothing.
 */
export interface ChatReceipt {
  channel: string;
  /** A post's or a reply's message, by the id the engine derived from the
   *  press's operation key (its `Idempotency-Key`) — which is why a retry
   *  under that key is the same message. */
  message_id?: string;
  /**
   * `open_dm` only: whether this call made the conversation or reached the
   * one already open. A direct conversation's id is derived from the sorted
   * handles of the people in it, so two people opening one from two nodes
   * converge on one room and the loser of the race is handed the winner's.
   *
   * IT IS NOT AN ERROR FLAG. `created: false` beside a landed outcome is the
   * ordinary answer — the conversation is open, and it is that one.
   */
  created?: boolean;
}

/**
 * What `mark_chat_read` answered: the `receipt` of its press, which names no
 * room — a read state is the seat's, across every room.
 */
export interface ChatReadReceipt {
  /**
   * The seat's whole read state as the write left it, in the form the push
   * and the rail carry. The tab that wrote applies it, and the push the same
   * write raises reaches that tab too: the record's revision makes the two one
   * state, whichever arrives first.
   */
  read: ChatReadState;
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
  /** Which nodes the page was merged from. */
  coverage?: Coverage;
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
  /** Which data nodes the turn was assembled from — a turn resumed on another
   *  node can have rows on two of them, and one that did not answer may hold
   *  the missing part. */
  coverage?: Coverage;
  /** Where it RAN: the nodes that PUBLISHED its events, read off the rows,
   *  sorted. Not the nodes in `coverage`: a turn run on a node without `data`
   *  is held by the data node keeping its events in custody, which never ran
   *  it. Empty when no answering node held any of its events yet. */
  nodes: string[];
}

/**
 * What a running coding run has said, read from its box by the node that owns
 * it (`sandbox.Output`), redacted by the engine: what the asker's CURSOR lacks
 * — the text after the offset it holds through (`start`), or on `reset` the
 * last 256 KiB (what the record will hold) in whole lines, which replaces what
 * it held. Offsets are UTF-8 bytes of the reading named `epoch`; `end` and
 * `digest` go back on the next request.
 *
 * `epoch`, `start`, `end` and `digest` are ALWAYS PRESENT, 0 included — a
 * reading that has settled nothing yet answers at end 0 and is followed from
 * start 0, and an offset the engine left out at 0 once read as absent, so a
 * delta from 0 was taken for one that did not follow and thrown away.
 */
export interface SandboxOutput {
  text: string;
  /** Which of the job's two accounts of itself this is. */
  source: "transcript" | "stderr" | "none";
  /** Output came before what this answer carries and is not in it. */
  cut: boolean;
  /** When the box was read, on the owning node's clock. */
  as_of: string;
  /** The job is over and waiting to be collected; it will not grow again. */
  finished: boolean;
  epoch: string;
  start: number;
  end: number;
  digest: string;
  /** The text replaces what the asker holds rather than following it. */
  reset?: boolean;
  /** The owner's reading began after the job's own start. */
  front?: boolean;
  /** Bytes written but not shown yet, until their redaction is settled. */
  held?: number;
}

/**
 * One `sandbox_tail{turn_id, launch_id}` answer (`sandbox.TailAnswer`): the
 * tail of that job while it runs, `launching` while its box is still being
 * made, `not_running` with the record's own status once it is not,
 * `box_paused` for a running record whose box is paused (never woken to be
 * read), or the node that reads its box NAMED where it did not answer
 * (`owner_silent`).
 */
export interface SandboxTailAnswer {
  outcome: (typeof SANDBOX_TAIL_OUTCOMES)[number];
  turn_id: string;
  launch_id: string;
  /**
   * The node that reads the run's box — its owner, or the node that launched a
   * `direct` or `container` box, whoever holds the seat — the one that
   * answered, or did not. Absent only for an `e2b` run no node holds.
   */
  node?: string;
  /** Why the job is not running: the record's status, `replaced`, or absent
   *  where no record is left. */
  status?: string;
  output?: SandboxOutput;
}

/** A seat's phase history, newest first, with a cursor. */
/** One seat's live state and finished calls, asked by its handle and answered
 *  by the agent id that handle resolves to. */
export interface AgentAnswer {
  /** The handle the seat answers to NOW — a retired one asked for resolves. */
  handle: string;
  agent_id: string;
  role: string;
  /** The live overlay, its call masked as every live call is ([LiveCall.body_withheld]). */
  live: Overlay | null;
  /**
   * The seat's finished phases. Each record's `payload` — the prompt and the
   * answer — is its trail, and absent for a reader the engine withholds the
   * trail from ([body_withheld]); the record's own columns are its status.
   */
  llm_history: EventRecord[];
  /** The engine withheld this seat's trail from this reader. */
  body_withheld?: boolean;
  /** Pass back as `before` to page further into the record. */
  next: string;
  /** Which data nodes the history came from — a seat's history is kept by
   *  every data node it ran on, or that kept its events in custody while it
   *  ran on a node without `data` — or null when it could not be read at
   *  all. */
  coverage?: Coverage | null;
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
  /** The revision's AUTHOR — a seat's handle for a person bound to one, a
   *  login otherwise, `token:<id>` for a Tier A token, the engine's own name
   *  for a write a timer made. */
  created_by: string;
  /**
   * WHAT `created_by` names, in the engine's one vocabulary of authors
   * (`iam.ActorKind`): `agent`, `human`, `operator` or `system` — the last for
   * the engine's own write (a boot seed, the reconcile loop). Always present:
   * the engine refuses to store a revision that does not say who wrote it. A
   * kind a newer engine adds arrives as itself, so this is a string rather
   * than a closed union a newer value would silently fall outside of.
   */
  created_by_kind: string;
  /** The credential the revision was written through — a machine token's
   *  `pat:<id>`, a browser session's `session:<lineage>` — and empty for a
   *  write no credential made. */
  operator_id: string;
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
  /**
   * Whether the engine can check a typed value for this field at the app
   * without storing it (`POST /setup/integrations/{kind}/verify`): the form
   * offers Test key only where this is set.
   */
  checkable?: boolean;
  /**
   * For a credential the engine cannot check ahead without acting on it,
   * the engine's sentence on when the app proves it ("Slack proves it with
   * the first event it signs."). The form says it rather than offering a
   * test it would have to fake.
   */
  check_note?: string;
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
   * narrow to it when a Fix control existed. It does not any more: a tile's
   * one action (Continue, Rotate token) opens the same settings rather than a
   * narrowed copy (see `actionFor` in Integrations.tsx and `rotate` on the
   * setup dialog).
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
  form_complete: boolean;
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

/** What checking one typed credential at the app, without storing it, found. */
export type VerifyOutcome = "works" | "refused" | "unreachable" | "unchecked";

/**
 * The answer of `POST /setup/integrations/{kind}/verify`: `works` (the app
 * accepted it), `refused` (the app answered and refused it), `unreachable`
 * (the app did not answer; nothing was stored) or `unchecked` (the app only
 * proves this value later), with one sentence for a person.
 */
export interface SetupVerdict {
  outcome: VerifyOutcome;
  detail: string;
}

export interface SetupListing {
  tools: SetupToolState[];
  /**
   * Where a THIRD-PARTY APP reaches this deployment: the address every
   * webhook, Slack request URL and GitHub App manifest on this screen is
   * built on. The public listener's `api.public.external_url` where
   * `api.public` is configured, and `api.external_url` otherwise —
   * `config_path` names which key it was read from, since there are two once
   * a public listener is set (the engine's `config.API.WebhookBase` and
   * `WebhookBaseKey`). NEVER the address a person or a cookie uses: that is
   * always `api.external_url`, which a vendor's address may not be.
   *
   * ONE FIELD WHERE THERE WERE FOUR. While this was a key in the company
   * document — a Tier B pointer a node resolved per epoch — the screen also
   * needed `present`, `resolved` and the variable's `reference`, because
   * "configured and resolving to nothing" was a state a banner had to name.
   * Both keys are Tier A, required on a node serving this screen and resolved
   * before the file is decoded, so a node answering at all has an address and
   * those three fields described a state that can no longer occur.
   */
  external_url: {
    value: string;
    config_path: string;
  };
}

export interface SecretRow {
  name: string;
  key_id: string;
  updated_at: string;
  /** Who last stored the value, and — beside them — what sort of author and
   *  the credential they stored it through, as a revision records the same
   *  three. The credential is empty on a row no credential wrote: the
   *  engine's own, and one a `crewlet` command wrote directly with its
   *  node's engine stopped. */
  updated_by: string;
  updated_by_kind?: string;
  operator_id?: string;
  /** Which write path last stored it, in that path's own word — `api`,
   *  `setup`, `provision`, `cli` or `llm-login` — never a word the writer
   *  chose.
   *  What each means is `provenance` in routes/settings/Secrets.tsx. */
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
  health?: EngineHealth;
  tokens?: Rollup;
  /** `null` until a node has reported the counters — see {@link OrgBudget}. */
  budget?: OrgBudget | null;
  schedules?: ScheduleRow[];
}

export interface Frame {
  kind: PushKind;
  data?: unknown;
  id?: number;
  what?: string;
  error?: string;
  ts?: string;
  /** The seat an `inbox_changed` frame is about. Only a socket WATCHING that
   *  seat is sent one, so a frame here is already this tab's own. */
  seat?: string;
  /** On an `unauthorized` error frame: the rule that refused — see
   *  {@link QueryRefusal}. Absent on every other frame. */
  reason?: string;
  /** On an `unauthorized` error frame: the grants any one of which would have
   *  admitted the caller. An EMPTY list is an answer — no grant would. */
  grants?: string[];
  /** On a QUERY's `unavailable` error frame: the state log's own refusal
   *  code, when one is behind the answer, or `no_active_revision` for a work,
   *  page or search question on a node that has no company yet — see
   *  {@link LogRefusal}. Never on a watch's, whose words stay in the node's
   *  log because what the directory says about a login names its seat. */
  refusal?: string;
  /**
   * The refusal's own words, on two error frames and nowhere else: a query's
   * `unavailable` (what the state log's refusal is about and what changes it)
   * and a `bad_params` (the parameter to change and what it accepts). Both are
   * written FOR the caller; every other failure's text stays in the node's
   * log, so this is never a path or a driver's message.
   */
  detail?: string;
  /** On an `unavailable` error frame, a query's or a watch's: seconds before
   *  asking this node again, and ZERO when waiting will not change the
   *  answer. Never omitted there — and waited out, through
   *  `unavailableRetryMs`, by everything that asks again. */
  retry_after?: number;
  /** On an `artifact_join` error frame: the artifact whose room was refused
   *  (with the join's `id`) or withdrawn (with no `id`, because the reader's
   *  access narrowed while they were in it). Absent on every other frame: a
   *  room's pushes name their artifact in their own payload. */
  artifact?: string;
}

/**
 * Why the engine refused a question on AUTHORITY — the machine-readable half of
 * an `unauthorized` answer, which the socket's error frame carries under the
 * same keys the REST envelope does.
 *
 * `grants` are the capabilities any ONE of which would have admitted the
 * caller; an empty list means no capability would, and what the rule asks is
 * something else — a relation the org chart does not hold, an agent's own
 * seat, a seat to act as — which `reason`, the deciding rule's own word
 * (`not_self`, `unseated`, …), names and `contract/authority.ts` says.
 */
export interface QueryRefusal {
  reason: string;
  grants: string[];
}

/**
 * What an `unavailable` answer says beyond its code — the machine-readable half
 * the socket's error frame carries under the keys a REST 503 does
 * (`refusal`, `detail`, and the `Retry-After` header as `retry_after`).
 *
 * `unavailable` covers two opposite facts. A node that is catching up, or whose
 * coordination store blinked, answers the same question a few seconds later;
 * a node whose state log is full, that holds a record it cannot decode or whose
 * broker refused the read's barrier answers it the same however often it is
 * asked. `retryAfter` is what tells them apart, and its ZERO is the answer:
 * asking this node again changes nothing, so a screen stops asking on a timer
 * and says what would.
 */
export interface LogRefusal {
  /** The state log's own code (`behind`, `log_full`, `deferred`, …), or
   *  `no_active_revision` where the node has not been handed a company yet
   *  and the question is over its tracker or knowledge base — the code a REST
   *  503 carries for it — or null where neither is behind the answer. */
  code: string | null;
  /** The refusal's own words: what it is about and what changes it. */
  detail: string | null;
  /** Seconds before asking THIS node again; zero when waiting will not
   *  change the answer, and then nothing re-asks on a timer. Read through
   *  `retryAfterMs`, which bounds it at `RETRY_AFTER_MAX_MS`. */
  retryAfter: number;
}

/**
 * An `inbox_changed` frame's data: one committed tracker batch moved this
 * seat's inbox.
 *
 * IDENTIFIERS AND A COUNT, NEVER CONTENT. The frame says only that the inbox
 * should be asked again; what is in it is read through `work_inbox`, the
 * question that decides who may read it. `unread_delta` is a HINT — it counts
 * the notices the batch wrote, and a redelivery the engine has already
 * collapsed may count one twice — so no screen adds it to a badge.
 */
export interface InboxChange {
  handle: string;
  unread_delta: number;
  /** The id of the object the newest of those notices was written on: the
   *  task for a task commit, the PERSON for a `prioritised` notice — whose
   *  task `work_inbox` names as `task`. */
  subject: string;
  /** The reason that notice was routed under — one of `work_inbox`'s. */
  reason: string;
}

/** The named answers the socket's request/response channel serves. */
/** One field's move, AS TEXT — every renderer of a delta wants
 *  "todo → in_progress", and the typed value is on the row for anything that
 *  needs it. A SET (watchers, an edge kind, tags) carries what it gained and
 *  lost instead, each sorted and each member whole, with `from` and `to`
 *  empty (`worklet.Delta`). */
export interface WorkDelta {
  from: string;
  to: string;
  added?: string[];
  removed?: string[];
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
  /** `subject_key` opens ANOTHER task — one that claimed it first — so the
   *  subject is reached by `subject_id`. See `subjectItem`. */
  subject_key_collision?: boolean;
  project?: string;
  excerpt?: string;
  fields?: Record<string, WorkDelta>;
  comment_id?: string;
  turn_id?: string;
  /** The question this commit asked or answered, as it stands NOW. */
  ask?: WorkAskView;
  /** How a reader tells "nothing was announced" from "nothing happened" —
   *  which is the whole reason a quiet commit still writes a row. */
  notified: boolean;
  /** An announcement a repair duty issued rather than the write it is about
   *  — the mirror of a one-sided dependency, or the "ready" an unblocked
   *  task's assignee never got — so a card read late says why it came then.
   *  It says nothing about when the broker accepted the commit. */
  late?: boolean;
}

export interface WorkActivityAnswer {
  records: WorkActivityRecord[];
  /** The key of every task id these records NAME, resolved on the answering
   *  node — a relation delta carries the other task's id, because a key belongs
   *  to that task's own row and a history row is written once by N nodes and
   *  repaired by nothing. So the id is what the record claims and the key is
   *  what this answer resolves, which is the same split `subject_key` already
   *  takes.
   *
   *  ABSENT IS NOT EMPTY, in both directions. An id the answering node holds no
   *  row for is left OUT rather than mapped to "" — a deferred record names a
   *  task this node has not applied, and a blank key renders as a task with no
   *  name — and a page whose deltas name nothing resolvable carries no map at
   *  all, so a reader guards for undefined rather than for `{}`. */
  keys?: Record<string, string>;
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

/** A structured ask: the question, two to six options, what the asker
 *  recommends and why, what it looked at, and the role the person is asked
 *  in. The engine enforces only its shape and that an answer's `choice`
 *  names one of its options. */
export interface WorkDecision {
  question: string;
  options: WorkDecisionOption[];
  /** An option id, or absent when the asker has no view. */
  recommended?: string;
  rationale?: string;
  evidence?: WorkDecisionEvidence[];
  /** `approver`: the answer IS the decision. `contributor`: it is an input
   *  to a decision somebody else makes. */
  role: "approver" | "contributor";
  /** Where the asker will report what was decided. */
  inform?: { surface: InformSurface; channel: string };
}

export interface WorkDecisionOption {
  id: string;
  label: string;
  detail?: string;
}

/** One thing a decision cites. A task's `ref` is its id — resolved at write,
 *  because a key moves with the task — and a url's is https. */
export interface WorkDecisionEvidence {
  kind: "task" | "page" | "turn" | "run" | "url";
  ref: string;
  label?: string;
}

/** One question waiting on somebody, with what to do about it. */
export interface WorkAskRow extends WorkSummary {
  comment: string;
  asked_by: string;
  asked_at: string;
  body: string;
  /** Present when the ask asks somebody to choose. */
  decision?: WorkDecision;
  /** Whether the ask is still waiting on an answer. */
  open?: boolean;
  /** The literal call that answers it. A model handed a comment id still has
   *  to compose the call, and every one it composes differently is a round
   *  spent being refused. Absent on an answered ask, which nothing answers
   *  again. */
  answer_with?: string;
}

/** One sub-item claimed by somebody. It lives on ANOTHER person's task, which
 *  is why it is its own block: no assignee filter over tasks reaches it. */
export interface WorkChecklistRow {
  task: string;
  task_key: string;
  /** `task_key` opens another task, so the task is reached by `task`. See
   *  `checklistTask`. */
  task_key_collision?: boolean;
  task_title: string;
  checklist: string;
  item: string;
  name: string;
  done: boolean;
}

/** How many rows one `work_my_work` block holds in full. `capped` says the
 *  count stopped at the engine's ceiling, so `total` is a floor, not a fact. */
export interface WorkClaimTotal {
  total: number;
  capped?: boolean;
}

/** One [WorkClaimTotal] per block, keyed by the block's own name. */
export interface WorkMyWorkTotals {
  priorities: WorkClaimTotal;
  assigned: WorkClaimTotal;
  asked_of_me: WorkClaimTotal;
  checklist_items: WorkClaimTotal;
  collaborating: WorkClaimTotal;
  watching_recent: WorkClaimTotal;
  unblocked_recent: WorkClaimTotal;
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
  /** Every block counted in FULL, by the predicate that drew its page and in
   *  the same transaction. A block is a page of at most twenty rows; its total
   *  is the claim — so a count is drawn from here, never from a length. */
  totals: WorkMyWorkTotals;
  read_level?: ReadLevel;
  log_seq?: number;
  applied_through?: number;
  log_lag?: number;
  complete: boolean;
  incomplete?: WorkIncomplete;
}

/** One notice in a person's inbox, as `worklet.InboxNotice` serialises it.
 *
 *  THE ONE FACT NO COMMERCIAL TRACKER RECORDS is `reason`: the applier writes
 *  WHY this change found this person, as one of eighteen, in the precedence
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
  /** The id of the TASK this notice is about, absent when it names none: the
   *  subject for a task commit, and the task a lead put at the top of the list
   *  for a `prioritised` notice — whose subject is the PERSON while
   *  `subject_key` is the task's. See `noticeItem`. */
  task?: string;
  /** The key STORED here now opens a task other than `task`, so the notice is
   *  reached by `task` — asked of this key rather than read off the task,
   *  because a task moved since answers to a new one. See `noticeItem`. */
  subject_key_collision?: boolean;
  excerpt?: string;
  actor?: string;
  actor_kind?: string;
  /** The comment this change wrote and the agent turn that made it. */
  comment_id?: string;
  turn_id?: string;
  /** The question this notice is about — the ask itself for `asked`, the ask
   *  it answered for `answered` — read NOW, so an answered one reads closed. */
  ask?: WorkAskView;
  read: boolean;
  snoozed?: boolean;
  snoozed_until?: string;
}

/** The ask a notice or a feed row is about, as it stands at the read. */
export interface WorkAskView {
  /** The ask's own comment id — what an answer names in `answers`. */
  comment: string;
  asked_of: string;
  open: boolean;
  /** Who closed it and when: the answer's author, or with `resolved` whoever
   *  resolved it without an answer. Absent while open. */
  answered_by?: string;
  answered_at?: string;
  resolved?: boolean;
  /** The option the answer chose, by id. */
  choice?: string;
  decision?: WorkDecision;
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
  /** As `WorkSummary.key_collision`: the key opens another task. */
  key_collision?: boolean;
  title: string;
  project: string;
  type: string;
  status: string;
  assignee?: string;
  /** The item's own priority, drawn as the Board draws it. */
  priority: string;
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
export interface WorkSearchAnswer extends SearchOutcome {
  hits: WorkRanked[];
  available: boolean;
  reason?: string;
  note?: string;
}

/** One person a change reached, and why.
 *
 *  `reason` is the ONE of eighteen that named them — the resolution is ordered
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
   * Absent for a run with no ledgerable trigger (a scheduled fire, a
   * sub-agent).
   */
  work_key?: string;
  agent_id?: string;
  role?: string;
  /** The span of the turn's own EVENTS, which is not its duration: the span
   *  covers the reflection pass that publishes after the turn ends. */
  started_at: string;
  ended_at: string;
  /** The turn's OWN measurement: the sum of every completed segment's, so a
   *  turn that parked and resumed counts both and not the wait between them.
   *  Zero for one that has completed no segment — `complete` and `parked` say
   *  which. */
  duration_ms: number;
  /** Whether the turn ENDED: its newest completion record is not a
   *  suspension. A turn with no completion is either running or died
   *  mid-flight, and those look identical from a list. */
  complete: boolean;
  /** Whether the turn is waiting on a detached coding run: its newest
   *  completion is a suspension, and it completes again when the run is
   *  collected. Never true beside `complete`. */
  parked?: boolean;
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
  /** Why it failed: the message and class of the record that ended it (the
   *  turn's own completion first, then a failed phase or a failure event).
   *  Absent for a turn that carried no failure. */
  error?: string;
  error_kind?: string;
  /** Whether a PERSON ended the turn — a pause with `stop_running` reached it
   *  at a round, or ended the coding run it was parked on. Never a failure:
   *  a stopped completion carries no `failed`. Absent when nobody stopped it. */
  stopped?: boolean;
  input_tokens: number;
  output_tokens: number;
  total_tokens: number;
  /** The share of `input_tokens` the provider's prompt cache served and
   *  stored — a BREAKDOWN of the input, never an addition to it. */
  cache_read_tokens?: number;
  cache_write_tokens?: number;
  /** Every distinct model the turn used, comma-joined — a turn routinely uses
   *  two, a cheap one for the extension judge and the seat's own. */
  models?: string;
  summary?: string;
  trigger?: string;
  /** The one work item the turn is charged to, off its completion record.
   *  Absent for a turn on nothing — which includes one still running, since a
   *  sole write names its item only at the end. */
  work_item?: TurnWorkItem;
}

/** The work item a turn is charged to — `work_item` on the turn events. The
 *  `id` is the identity and `key` the label a person reads; `<backend>:<id>`
 *  is unique across trackers, the bare id is not. `native` is Worklet, the
 *  spelling every stored record keeps, and no screen shows it
 *  (`lib/work.ts`'s `WORKLET_ITEM_BACKEND`). */
export interface TurnWorkItem {
  backend: "native" | "jira" | "github" | "gitlab" | (string & {});
  id: string;
  key: string;
  project: string;
}

export interface TurnsAnswer {
  turns: TurnRow[];
  /** The cursor to resume from — an OPAQUE token, handed back as `before`
   *  exactly as it came and never built from a row, since it names where the
   *  page's last turn is listed and that turn's id rather than any field a row
   *  carries — and `null` on the last page, present only while more turns lie
   *  past this one. The fleet's, so it can be present on an empty page: where
   *  a node's page stopped, with nothing above it left to show. */
  next: string | null;
  /** Which nodes the page was merged from. */
  coverage?: Coverage;
}

/** Who the engine resolved this caller to — see `lib/viewer.ts`. */
export interface Viewer {
  /** The principal's login: a person's dotted name, or a machine's `class:id`. */
  login: string;
  /** The capabilities this caller carries, after the node's ceiling. */
  grants: string[];
  /** The seat the identity directory binds this caller to, or "".
   *  UNBOUND IS AN ORDINARY STATE, not a misconfiguration. */
  handle: string;
  /** That seat's display name, or "". */
  name: string;
  /** That seat's kind — `human` or `agent` — or "" for no seat. */
  kind: string;
  /**
   * The name this caller's OWN record is kept under — their inbox, pins,
   * priorities and personal views: the seat for a bound person, the login for
   * everybody the directory binds to none. Every tool writes their record under
   * it, so every personal read asks by it. "" for a caller with no record.
   */
  owner: string;
  /** The tools `POST /operator/act/{tool}` would serve this caller: the
   *  catalogue's writes the authority table can admit them to before any
   *  object is named, so a screen enables exactly the controls a press of
   *  could be served. NOBODY IS REFUSED FOR BEING UNBOUND (ADR-0024): an
   *  unbound credential acts under its own login. Empty on a node that has
   *  not been handed a company — every press would be refused there. */
  acts: string[];
  /** The project this person's create lands in when it names none — the
   *  engine's own default for their seat, which `create_work_item` applies.
   *  "" for a caller bound to no seat, and when their team and every team
   *  above it owns none. */
  project: string;
  /** Whether this caller's credential may CHANGE the company document —
   *  the engine's own `config.APIAuth.MayWriteCompany`, the rule its config
   *  surface refuses a write with. False for an anonymous caller, and for
   *  every credential a managed document's `api.auth.company_writers` does
   *  not list (ADR-0030): a person's session, a machine token and a lead
   *  among them. It says nothing about authority — Admin, or a lead's
   *  subtree, still decides whether this caller may write at all. */
  config_writer: boolean;
  /** The Tier A token ids that alone may change a managed company document —
   *  who manages it — and EMPTY when it is not managed. An anonymous caller
   *  is told none. */
  config_managed_by: string[];
  /** Whether this caller may ask the Dashboard Assistant, and if not, why:
   *  the first failing condition (`internal/assistant/wire.ViewerAssistant`).
   *  Absent from an engine that does not serve the assistant, which the
   *  client reads as off (`assistantUnavailable`). */
  assistant?: AssistantViewerAssistant;
  /**
   * Which chat the company runs (`chat.backend`, resolved): the engine's own
   * (`chatlet`), a vendor's (`vendor`) or none (`none`) — and "" for a caller
   * not holding the Chatlet app, and on a node with no company yet. Every chat
   * question is served only where it is `chatlet`, so a screen says which it
   * is before it asks one: asked of a vendor company, each would be
   * `unknown_query`, which reads as a node fault rather than a company's
   * choice.
   */
  chat_backend: "chatlet" | "vendor" | "none" | "";
  /** On a `vendor` company, the vendor surfaces it declares (`mattermost`,
   *  `slack`), in that order; empty everywhere else. */
  chat_vendors: string[];
}

// ---------------------------------------------------------------------------
// The sign-in surface — `/auth`, read by `protocol/auth.ts`
// ---------------------------------------------------------------------------

/**
 * What a session opened by a sign-in may do: everything its grants allow, or
 * — on a deployment that requires a second factor the person does not hold —
 * nothing but enrol one. The second is the code the engine refuses every
 * other route with, so one string names it wherever it is met.
 */
export type SessionStatus = "signed_in" | "second_factor_enrolment_required";

/**
 * What every completed sign-in answers — a password, a redemption, a token
 * exchange, a step-up, an enrolment's replacement session.
 *
 * NO BEARER IN IT. The cookie the response set is the credential, and a body
 * carrying it too would put the one value most worth stealing where a script
 * can read it.
 */
export interface SignedIn {
  person: string;
  /** The login as the estate holds it, which is how a screen names them. */
  login: string;
  seat?: string;
  /** The absolute deadline, which no re-issue moves. */
  expires_at: string;
  position: string;
  status: SessionStatus;
}

/** What an invitation's screen renders from, before anything is spent. */
export interface InvitationView {
  /** The address it is for, opened for this one answer. */
  email: string;
  /** Who sent it, or absent. */
  invited_by?: string;
  /** The login this form PROPOSES from the address, or absent when nothing in
   *  it fits the grammar and the person must type one. */
  login?: string;
  /** The floor every password is held to. */
  min_password_length: number;
  /** The seat redeeming it binds the person to, as the chart calls it now —
   *  with no handle where the chart no longer holds it. Every invitation
   *  names one, because every person holds a human seat; one that names none
   *  (absent, or empty) is one whose redemption the engine refuses, and the
   *  screen says so before anybody types a password. */
  seat?: { handle?: string; name?: string };
  /** The login this BROWSER is signed in as, or absent for nobody: redeeming
   *  ends that session, so the form says so first. */
  signed_in_as?: string;
}

/** A password set from a reset link — and no session: the person signs in next. */
export interface PasswordSet {
  status: "password_set";
  login: string;
}

/**
 * A second factor's seed: the first leg of an enrolment, which stores nothing.
 * The second leg sends `secret` back with a code derived from it.
 */
export interface SecondFactorSeed {
  /** The seed in base32, for an app that is typed into. */
  secret: string;
  /** The `otpauth://` form an authenticator app opens, and scans as the setup's QR code. */
  uri: string;
}

/** A completed enrolment. */
export interface SecondFactorEnrolled {
  status: "enrolled";
  /** The whole session that replaced an enrolment-only one, or absent. */
  session?: SignedIn;
}

/** The one and only time a set of recovery codes is readable. */
export interface RecoveryCodes {
  codes: string[];
}

/** Who a session is, as `GET /auth/session` answers — the part a screen reads. */
export interface SessionAnswer {
  person: string;
  /** Their own name as their directory row holds it, or absent — what a screen
   *  calls a person no seat names. */
  name?: string;
  login: string;
  seat?: string;
  /** `person` or `machine`: a machine holds no second factor to manage. */
  kind: string;
  /** What this node lets the caller do: their grants, cut to its ceiling. */
  grants?: string[] | null;
  /** The absolute deadline of the session the request carried, or absent for
   *  a credential that is not a session. */
  expires_at?: string;
  /** The session the request carried, by the id a listing of the person's
   *  sessions names it by — absent exactly where `expires_at` is. */
  lineage?: string;
  status: SessionStatus;
}

/** What a client reads before anybody signs in (`GET /auth/config`). */
export interface AuthConfig {
  /** The floor every password is held to, in characters. */
  min_password_length: number;
  /** Whether a person signing in with a password must hold a second factor. */
  second_factor: "required" | "optional";
}

/**
 * What changing your own password answers: the new session that keeps this
 * browser signed in, or — where this node has not applied the change yet — the
 * plain fact that every session ended, this browser's with them.
 */
export type PasswordChanged = SignedIn | { status: "password_changed"; detail: string };

/** One seat a name could mean, and why. */
export interface ColleagueCandidate {
  handle: string;
  /** The tier that found it, as a person reads it ("handle matches exactly"). */
  why: string;
}

/**
 * A name resolved through the engine's own four tiers — the ones an agent's
 * `lookup_colleague` uses. `match` is set only when EXACTLY ONE seat is meant;
 * several is a list for the person to choose from, never a pick.
 */
export interface ColleagueAnswer {
  match: ColleagueCandidate | null;
  /** Every seat the name could mean, best tier first (the match included). */
  candidates: ColleagueCandidate[];
}

/** One window of the `work_flow` series: the census at its end (at now for
 *  the window now falls in) and how many tasks were delivered in it. */
export interface WorkFlowPoint {
  /** The window's label on the company calendar: `2026-09-23`, `2026-W39`. */
  window: string;
  start: string;
  end: string;
  not_started: number;
  active: number;
  done: number;
  closed: number;
  /** Changes in the window that took a task from not delivered to
   *  delivered: a cancellation is not one, and done → closed is not a
   *  second. */
  completed: number;
}

/** The company's work as a series — `work_flow`, the tracker's history
 *  replayed backward from today's census on the company clock. */
export interface WorkFlowAnswer {
  bucket: "day" | "week";
  project?: string;
  /** Oldest first; the last is the window now falls in. */
  points: WorkFlowPoint[];
  /** Today's open census, with the two shapes of trouble only the present
   *  can answer. */
  now: { not_started: number; active: number; blocked: number; overdue: number };
  /** Always false: nothing records when a task became blocked, so the past
   *  has no blocked count and a chart must not draw one. */
  blocked_history: boolean;
  read_level?: ReadLevel;
  complete: boolean;
}

/** One tracker row of the company feed. */
export interface FeedWorkRow {
  id: string;
  kind: "created" | "completed" | "handoff";
  at: string;
  cursor: string;
  actor?: string;
  actor_kind?: string;
  task: string;
  key?: string;
  /** The key is one another task claimed first, so a link built from it
   *  opens THAT task and this row's is reached by its id (`itemAddress`). */
  key_collision?: boolean;
  title?: string;
  project?: string;
  /** On a completion: the task's running totals, tokens only. */
  spend?: { tokens: number; turns: number };
  /** On a completion with a charged turn: no reviewer ever sent it back. */
  first_pass?: boolean;
  /** On a create: the chat surface and conversation it was filed from. */
  origin?: { surface: string; conversation?: string };
  /** On a hand-off. */
  from?: string;
  to?: string;
  reassignments?: number;
  reassignment_budget?: number;
}

/** One merged row of `company_feed`: exactly one body, the kind's. */
export interface FeedEntry {
  kind: "completed" | "created" | "handoff" | "schedule" | "page";
  at: string;
  work?: FeedWorkRow;
  page?: {
    id: string;
    page_id: string;
    title?: string;
    container?: string;
    change: string;
    actor?: string;
    actor_kind?: string;
    turn_id?: string;
  };
  schedule?: FeedScheduleRun;
}

/**
 * A schedule's RUN in the company feed — a fire the scheduler dispatched,
 * never a tick it skipped — or several in a row: the engine folds consecutive
 * runs of one schedule for one runner that nothing else falls between into the
 * newest of them.
 */
export interface FeedScheduleRun {
  scope_type: string;
  scope_id: string;
  name: string;
  target?: string;
  outcome?: string;
  trace_id?: string;
  turn_id?: string;
  /** How many consecutive runs this row stands for, at least 1. */
  runs: number;
  /** The oldest of them, when `runs` is above 1. */
  since?: string;
}

/** `company_feed`: what the company did, newest first, merged across the
 *  tracker, the pages and the schedules' runs. */
export interface CompanyFeedAnswer {
  rows: FeedEntry[];
  /** Resumes every source where this page stopped; absent at the end. */
  next_cursor?: string;
  read_level?: ReadLevel;
  complete: boolean;
}

/** One thing waiting on a person's decision. */
export interface DecisionItem {
  kind: "ask" | "run";
  at: string;
  ask?: WorkAskRow;
  run?: SandboxRun;
}

/** `decisions`: the open asks put to a person and the coding runs parked on
 *  a question to them, newest first. */
export interface DecisionsAnswer {
  handle: string;
  items: DecisionItem[];
  /** Every such item, not the page. */
  total: number;
  /** The ask count reached the engine's ceiling, so `total` is a floor. */
  capped: boolean;
  /** When the longest-waiting one began waiting. */
  oldest_at?: string;
  read_level?: ReadLevel;
  complete?: boolean;
}

/** What closed one ask (`worklet.AskAnswer`): an answer, by the person asked
 *  or by somebody else, or a resolution without one. */
export interface WorkAskAnswer {
  /** Whoever closed it, by record name: not always the person asked. */
  by: string;
  by_kind?: string;
  at: string;
  /** The answer's own comment id; absent for an ask resolved without one. */
  comment?: string;
  /** The option chosen, by id. */
  choice?: string;
  /** The answer's words: the option's label where nothing else was written. */
  body: string;
  /** Closed without an answer. */
  resolved?: boolean;
}

/** The asker's first record on the task after the ask closed
 *  (`worklet.AskFollowUp`), in the activity feed's terms. */
export interface WorkAskFollowUp {
  at: string;
  kind: string;
  excerpt?: string;
  fields?: Record<string, WorkDelta>;
  comment_id?: string;
}

/** One ask on a work item put to a person and closed lately
 *  (`worklet.AnsweredAsk`). */
export interface AnsweredWorkAsk {
  /** When it closed: `answer.at`. */
  at: string;
  ask: WorkAskRow;
  answer: WorkAskAnswer;
  /** Absent while the asker has written nothing on the task since, and for a
   *  reader the tracker shows no task to. */
  followed_up?: WorkAskFollowUp;
}

/** A coding run's question as the record of its answer states it
 *  (`queries.RunAsked`), asked in a work item's ask's shape (ADR-0068). */
export interface AnsweredRunAsk {
  turn_id: string;
  /** The launch that asked: a turn's run asks again under a new one. */
  launch_id: string;
  /** The seat whose coding run asked. */
  agent_handle: string;
  /** The question, as one sentence. */
  question: string;
  /** What the question says beside its title, as Markdown. Absent when the
   *  run gave none. */
  question_context?: string;
  /** The options the run asked a person to choose between. Absent on an open
   *  question, which was answered in words. */
  decision?: WorkDecision;
  asked_at: string;
  /** The task the run was on, where it was on one. */
  work_item?: WorkItemRef;
}

/** One coding run's question a person answered lately and the run took
 *  (`queries.AnsweredRunAsk`). A run's question has no thread, so it carries
 *  no `followed_up`, and `answer.by` is always the person it is listed for. */
export interface AnsweredRun {
  /** When it was answered: `answer.at`. */
  at: string;
  run: AnsweredRunAsk;
  /** `body` is the person's words, or the chosen option's label where nothing
   *  else was written, as a work item's answer is. */
  answer: WorkAskAnswer;
}

/** One question put to a person and closed lately: a work item's ask, or a
 *  coding run's question, which is the one that carries `run`. */
export type AnsweredDecision = AnsweredWorkAsk | AnsweredRun;

/** `answered_decisions`: the asks put to a person and closed in the last
 *  week and the coding runs' questions they answered in it, newest closing
 *  first, together at most 50. */
export interface AnsweredDecisionsAnswer {
  handle: string;
  items: AnsweredDecision[];
  /** The start of the window. */
  since?: string;
  /** The window held more than the page. */
  truncated?: boolean;
  read_level?: ReadLevel;
  complete?: boolean;
}

/** One company day of one seat's turns, summed across every data node. */
export interface SeatActivityDay {
  /** A company date label, `2026-09-23`. */
  day: string;
  turns: number;
  failed: number;
  tokens: number;
}

/** One seat's totals over the window before, with `previous=true`. */
export interface SeatActivityTotals {
  turns: number;
  failed: number;
  reviewed: number;
  first_pass: number;
  sent_back: number;
  tokens: number;
  /** Every day of the previous window, oldest first — so the fortnight a
   *  week-on-week figure is made over comes from the same answer. */
  per_day: SeatActivityDay[];
}

/**
 * One seat's turns over a window of company days, from the replicated usage
 * domain — every data node's days summed (`seat_activity`).
 */
export interface SeatActivityRow {
  handle: string;
  role?: string;
  agent_id?: string;
  /** False for a seat whose days are in the window and that has left the chart. */
  in_chart: boolean;
  turns: number;
  failed: number;
  reviewed: number;
  first_pass: number;
  /** `first_pass` over REVIEWED turns, 0–100 — ABSENT when none was reviewed,
   *  which is no rate at all rather than 0%. */
  first_pass_pct?: number;
  /** Reviews that sent work back, not turns. */
  sent_back: number;
  /** Duration quantiles within `quantile_resolution`; absent with no turn. */
  p50_ms?: number;
  p90_ms?: number;
  tokens: number;
  /** Every day of the window, oldest first, a quiet day as zeros. */
  per_day: SeatActivityDay[];
  last_turn_at?: string;
  previous?: SeatActivityTotals;
}

/** `seat_activity`: every agent seat's turns over `days` company days. */
export interface SeatActivityAnswer {
  since: string;
  until: string;
  days: number;
  previous_since?: string;
  previous_until?: string;
  seats: SeatActivityRow[];
  /** The worst relative error of a quantile (0.06). */
  quantile_resolution: number;
}

// ---- artifacts ---------------------------------------------------------- //
//
// The artifacts' answers and pushes, as `internal/artifactsdomain`,
// `internal/artifactdata`, `internal/api/artifactsapi`,
// `internal/api/artifactdataapi` and `internal/api/stream` send them. The
// closed sets they are made of are the contract's (`contract/artifacts.ts`),
// held there against the engine; these are composed from them.

/** What an artifact is (`artifacts.Kind`). */
export type ArtifactKind = (typeof ARTIFACT_KINDS)[number];
/** A runtime capability a version declares (`artifacts.Capability`). */
export type ArtifactCapability = (typeof ARTIFACT_CAPABILITIES)[number];
/** A principal's relation to one artifact (`artifacts.Role`). */
export type ArtifactRole = (typeof ARTIFACT_ROLES)[number];
/** A role a share may grant. */
export type ShareableRole = (typeof SHAREABLE_ROLES)[number];
/** Who a share names. */
export type AudienceKind = (typeof AUDIENCE_KINDS)[number];
/** How the artifact origin isolates a page. */
export type OriginMode = (typeof ORIGIN_MODES)[number];
/** What a present member is doing. */
export type PresenceActivity = (typeof PRESENCE_ACTIVITIES)[number];
/** What a listing is about. */
export type ArtifactListScope = (typeof ARTIFACT_LIST_SCOPES)[number];
/** How a listing is ordered. */
export type ArtifactListSort = (typeof ARTIFACT_LIST_SORTS)[number];
/** What an artifact's history row is filed under (`artifactsdomain.HistoryOps`). */
export type ArtifactHistoryOp = (typeof ARTIFACT_HISTORY_OPS)[number];
/**
 * A top-level folder's visibility (`artifactsdomain.FolderVisibility`): a
 * preset a create may ask for, or `custom` for shares no preset describes,
 * which is answered and never asked for.
 */
export type ArtifactFolderVisibility = (typeof ARTIFACT_FOLDER_VISIBILITIES)[number] | "custom";
/** How one datastore field is stored and drawn. */
export type ColumnType = (typeof COLUMN_TYPES)[number];
/** Who reads a collection's rows. */
export type ReadRule = (typeof READ_RULES)[number];
/** A collection policy picked by intent. */
export type CollectionPreset = (typeof COLLECTION_PRESETS)[number];
/** What narrows a column's write rule. */
export type FieldCondition = (typeof FIELD_CONDITIONS)[number];
/** What one datastore op does. */
export type DataOpKind = (typeof DATA_OP_KINDS)[number];
/** What the applier decided about one write. */
export type DataVerdict = (typeof DATA_VERDICTS)[number];

/**
 * Who did something (`iam.Actor`). `kind` is `agent`, `human`, `operator` or
 * `system`; `operator_id` is the credential the author acted through, absent
 * where the row records none.
 */
export interface ArtifactActor {
  name: string;
  kind: string;
  operator_id?: string;
}

/** The principal set one share names (`artifacts.Audience`). `id` is empty for the company. */
export interface ArtifactAudience {
  kind: AudienceKind;
  id?: string;
}

/** One audience's role on one artifact (`artifacts.Share`). */
export interface ArtifactShare {
  audience: ArtifactAudience;
  role: ArtifactRole;
  granted_by: ArtifactActor;
  granted_at: string;
}

/** An artifact's status (`artifactsdomain.Status`). */
export type ArtifactStatus = "live" | "trashed";

/** One artifact's head (`artifactsdomain.Head`). */
export interface ArtifactHead {
  id: string;
  /** The DNS host label: the id in lowercase Crockford base32. */
  label: string;
  kind: ArtifactKind;
  title: string;
  description?: string;
  icon?: string;
  owner: string;
  owner_kind: string;
  source_key?: string;
  /** The folder it is filed in, absent at the top level. A folder grants nothing. */
  folder?: string;
  head_number: number;
  contract?: string;
  /** What the head version declares; empty, never absent, for none. */
  capabilities: ArtifactCapability[];
  status: ArtifactStatus;
  trashed_by: ArtifactActor;
  trashed_at: string;
  /** When its purge began, the zero time for one no purge has begun on: such an artifact is in the trash and is never restored. */
  purge_begun_at: string;
  access_epoch: number;
  bytes_held: number;
  /** The audience, to the owner and editors only; absent below. */
  shares?: ArtifactShare[];
  created_by: ArtifactActor;
  updated_by: ArtifactActor;
  created_at: string;
  updated_at: string;
  /** The packed position of the last write to the artifact's row. */
  version: number;
}

/** A head as a reader is shown it (`artifactsapi.Artifact`). */
export interface ArtifactRow extends ArtifactHead {
  /** The reader's own role. */
  role: ArtifactRole;
  /** How many shares it carries, which a reader below editor is shown in place of the list. */
  share_count: number;
  /** Only the admin path admitted this read, and the engine recorded it. */
  elevated?: boolean;
}

/** `artifact{id}`: one artifact as its page reads it (`artifactsapi.Detail`). */
export interface ArtifactDetail extends ArtifactRow {
  /** The reader may publish a new version: the artifacts app beside the role. */
  writer: boolean;
}

/** One row of a listing (`artifactsapi.ListItem`). */
export interface ArtifactListItem extends ArtifactRow {
  pinned?: boolean;
  /** When this reader last opened it; the zero instant for never. */
  opened_at: string;
  /** The folder it is filed in as this reader sees it (`Marketing/Q4/`); absent at the top level or in a folder the reader cannot see. */
  folder_path?: string;
}

/**
 * One child folder of a one-level listing (`artifactsdomain.CommonPrefix`), as
 * S3's ListObjectsV2 answers a common prefix.
 */
export interface ArtifactCommonPrefix {
  /** The folder's path with a trailing `/` (`Marketing/Q4/Campaigns/`): what to list next. */
  prefix: string;
  /** The folder's id: the address a create or a move files into, and a rename-proof link. */
  folder: string;
  name: string;
  visibility: ArtifactFolderVisibility;
  /** The artifacts directly in it this reader can see. */
  items: number;
  /** The folders directly in it this reader can see. */
  folders: number;
}

/**
 * `artifacts{prefix | folder, delimiter, max-keys, continuation-token, scope, kind, q, sort, status, pinned, work_item}`:
 * one page in S3's ListObjectsV2 terms. With `delimiter: "/"` the page is one
 * level (the prefix's artifacts and its child folders as `common_prefixes`);
 * without it, everything beneath the prefix. `folder` (an id) names the
 * folder in place of `prefix`, which the answer then echoes as the resolved
 * path: a path may name two folders a reader sees (a company "Drafts" and
 * their own private one, `409 folder_ambiguous`), and an id never does.
 */
export interface ArtifactListAnswer {
  /** The query's prefix, echoed; empty for the top level. */
  prefix: string;
  delimiter?: string;
  contents: ArtifactListItem[];
  /** A one-level page's child folders, A to Z, all on the first page. */
  common_prefixes?: ArtifactCommonPrefix[];
  /** What this page shows: contents and common prefixes together. */
  key_count: number;
  is_truncated: boolean;
  /** Send back as `continuation-token` for the next page. */
  next_continuation_token?: string;
  /** How many matched the estate's own filter; a row the authority table withheld is counted and not listed. */
  total: number;
  read_level: string;
  position: LogPosition;
  complete: boolean;
}

/** One folder (`artifactsdomain.Folder`). Shares are shown to its owner and managers only. */
export interface ArtifactFolder {
  id: string;
  /** The folder it sits in; absent for a top-level folder. */
  parent?: string;
  /** Its top-level folder, its own id for a top-level folder. */
  root: string;
  name: string;
  owner: string;
  owner_kind: string;
  /** 1 for a top-level folder. */
  depth: number;
  max_depth: number;
  visibility: ArtifactFolderVisibility;
  /** The top-level folder's owner, whom its visibility is decided on. */
  root_owner?: string;
  root_owner_kind?: string;
  /** Answered only to a reader whose `role` on the folder is `owner`. */
  shares?: ArtifactShare[];
  status: "live" | "removed";
  created_by: ArtifactActor;
  created_at: string;
  updated_by: ArtifactActor;
  updated_at: string;
  version: number;
}

/** One folder of a reader's tree, with what is directly in it that they can see (`artifactsdomain.FolderItem`). */
export interface ArtifactFolderItem extends ArtifactFolder {
  artifacts: number;
  folders: number;
  /**
   * What the reader may do with the folder, as the engine decided it:
   * `owner` renames, moves, removes and unfiles (the folder's owner, the
   * owner's lead, or an administrator), `contributor` files into it and makes
   * subfolders, `viewer` sees it only.
   */
  role: ArtifactFolderRole;
}

/** A reader's decided relation to one folder (`ArtifactFolderItem.role`). */
export type ArtifactFolderRole = "owner" | "contributor" | "viewer";

/** `artifact_folders{}` and `GET /artifact-folders`: every folder the reader can see (`artifactsdomain.FolderTree`). */
export interface ArtifactFoldersAnswer {
  /** Parents before children, siblings A to Z. */
  folders: ArtifactFolderItem[];
  /** The artifacts the reader can see that sit in no folder they can see ("Not in a folder"). */
  top_level: number;
  read_level: string;
  position: LogPosition;
  complete: boolean;
}

/** `POST /artifact-folders`: one folder, or a path created with its missing parents. */
export type ArtifactFolderCreateRequest =
  | { name: string; parent?: string; visibility?: "company" | "private" }
  | { path: string; parents: true; parent?: string; visibility?: "company" | "private" };

/** What a folder create answers (`artifactsdomain.FolderCreated`): the chain, parents first. */
export interface ArtifactFolderCreated {
  folders: ArtifactFolder[];
  /** How many of them this request made: 0 (answered `200`) when the whole path was already there. */
  created: number;
  op_id: string;
  outcome: string;
  position: LogPosition;
}

/** `PATCH /artifact-folders/{id}`: a rename or a move, one a request (a body naming both is refused). */
export interface ArtifactFolderPatchRequest {
  name?: string;
  /** The new parent's id; an empty string moves it to the top level. */
  parent?: string;
  /**
   * The reader saw and accepted that the move widens who sees the folder (a
   * private tree into a company one): without it such a move is refused
   * `409 folder_widens`.
   */
  confirm_widen?: boolean;
}

/** What `PATCH /artifact-folders/{id}` answers. */
export interface ArtifactFolderPatched {
  /** The folder as it is now, absent while the write is still pending here. */
  folder?: ArtifactFolder;
  op_id: string;
  outcome: string;
  position: LogPosition;
}

/** `PUT /artifacts/{id}/folder`: file one artifact; an empty string files it at the top level. */
export interface ArtifactMoveRequest {
  folder: string;
}

/** `POST /artifacts/folder-moves`: file up to `ARTIFACT_MOVE_MAX_ITEMS` artifacts, each decided alone. */
export interface ArtifactMovesRequest {
  items: { artifact: string; folder: string }[];
}

/** What `PUT /artifacts/{id}/folder` answers. */
export interface ArtifactMoved {
  artifact: string;
  /** The folder it is filed in now, `""` for the top level. */
  folder: string;
  op_id: string;
  outcome: string;
  position: LogPosition;
}

/** One item's outcome in `POST /artifacts/folder-moves`. */
export interface ArtifactMoveResult {
  artifact: string;
  /**
   * `moved`, or the refusal's code: `not_found`, `unauthorized`,
   * `folder_not_found`, `artifact_trashed`, `invalid` or `unavailable`.
   */
  outcome: string;
  /** What to do about a refusal, in a sentence. */
  message?: string;
}

/** What `POST /artifacts/folder-moves` answers. */
export interface ArtifactMovesAnswer {
  results: ArtifactMoveResult[];
}

/** A brand colour role's value: `#rrggbb`, lowercase. */
export type BrandColor = string;

/** One typeface of a brand (`brandformat.Face`): exactly one of `kit`, `stack`, or `family` with `files`. */
export interface BrandFace {
  kit?: string;
  stack?: string;
  family?: string;
  files?: { path: string; weight: number; style: "normal" | "italic" }[];
  licensed?: boolean;
}

/**
 * A kit's `brand.json` (`brandformat.Brand`). A DRAFT may lack its primary
 * logo and its primary colour (a publish requires only the name); an adoption
 * requires both.
 */
export interface BrandDocument {
  v: 1;
  name: string;
  tagline?: string;
  logos: { primary?: string; on_dark?: string; mark?: string };
  colors: {
    primary?: BrandColor;
    secondary?: BrandColor;
    text?: BrandColor;
    light_ground?: BrandColor;
    dark_ground?: BrandColor;
    primary_on_dark?: BrandColor;
  };
  type: { heading: BrandFace; body: BrandFace };
  voice: { summary?: string; write_like?: string[]; avoid?: string[] };
}

/** One file the adopted brand serves (`artifactsdomain.BrandFile`), read through `GET /brand/files/{hash}`. */
export interface BrandFileRef {
  /** `css`, `logo.primary`, `logo.on_dark`, `logo.mark`, `font.heading.<n>` or `font.body.<n>`. */
  slot: string;
  content_type: string;
  object: ArtifactObject;
}

/** `brand{}` and `GET /brand`: the company's adopted brand, or `{brand: null}`. */
export interface BrandAnswer {
  brand: {
    kit: string;
    kit_version: number;
    document: BrandDocument;
    css_sha: string;
    files: BrandFileRef[];
    adopted_by: ArtifactActor;
    adopted_at: string;
    /**
     * The newest version of the adopted kit, when it is newer than the
     * adopted one: the Brand page's draft notice. Answered only to a reader
     * who can see the kit or is an administrator.
     */
    newer_version?: number;
    /** Who published `newer_version`, answered with it. */
    newer_by?: ArtifactActor;
  } | null;
}

/** `POST /brand/adoptions`: adopt one version of a kit (an administrator, a person, a recent proof). */
export interface BrandAdoptRequest {
  kit: string;
  version: number;
}

/** The stored bytes behind one file (`objstore.Object`). */
export interface ArtifactObject {
  key?: string;
  hash: string;
  size: number;
}

/** What the linter derived for one HTML file (`artifacts.ScriptPolicy`). */
export interface ArtifactScriptPolicy {
  nonced: boolean;
  handler_hashes?: string[];
  unsafe_eval?: boolean;
  wasm_eval?: boolean;
}

/** One published file of a version (`artifacts.Entry`). */
export interface ArtifactEntry {
  path: string;
  content_type: string;
  object: ArtifactObject;
  carried?: boolean;
  script?: ArtifactScriptPolicy;
}

/** Every file of one version (`artifacts.Manifest`). */
export interface ArtifactManifest {
  /** The path served at the version's root; empty for a native sheet. */
  entry: string;
  files: ArtifactEntry[];
}

/** One retained version without its manifest (`artifactsdomain.VersionSummary`). */
export interface ArtifactVersionSummary {
  artifact: string;
  number: number;
  version_id: string;
  base: number;
  restored_from?: number;
  forced?: boolean;
  label?: string;
  entry?: string;
  contract?: string;
  /** What the version declares; empty, never absent, for none. */
  capabilities: ArtifactCapability[];
  files: number;
  bytes: number;
  author: ArtifactActor;
  created_at: string;
  position: number;
  /**
   * The adopted brand stylesheet's digest this version was published under,
   * which the artifact origin links at serve time. An HTML page's and a
   * design's only; absent where none was adopted, and on every other kind.
   */
  brand?: string;
}

/** `artifact_version{id, n}`: one version with its manifest. */
export interface ArtifactVersion extends ArtifactVersionSummary {
  manifest: ArtifactManifest;
}

/** `artifact_versions{id, cursor, limit}`: newest first. */
export interface ArtifactVersionsAnswer {
  versions: ArtifactVersionSummary[];
  after?: string;
}

/** One live file of an artifact's asset store (`artifactsdomain.Asset`). */
export interface ArtifactAsset {
  artifact: string;
  id: string;
  name: string;
  content_type: string;
  object: ArtifactObject;
  added_by: ArtifactActor;
  added_at: string;
  version: number;
}

/** `artifact_assets{id}`. */
export interface ArtifactAssetsAnswer {
  assets: ArtifactAsset[];
}

/** What one history row says beyond its op (`artifactsdomain.HistorySummary`). */
export interface ArtifactHistorySummary {
  files?: number;
  bytes?: number;
  pruned?: number[];
  forced?: boolean;
  restored_from?: number;
  label?: string;
  fields?: string[];
  audience?: string;
  role?: string;
  from?: string;
  to?: string;
  asset?: string;
  name?: string;
  paths?: string[];
  head?: number;
  limit?: number;
  usage?: number;
  key?: string;
  /** A move's folders by id, absent for the top level: never a folder's name. */
  from_folder?: string;
  to_folder?: string;
}

/** One row of an artifact's change record (`artifactsdomain.HistoryEntry`). */
export interface ArtifactHistoryEntry {
  position: number;
  op_id: string;
  /**
   * An [ArtifactHistoryOp]. Typed as text, because a newer engine may file a
   * row under an op this build has not met, which is still said in its own
   * words.
   */
  op: string;
  outcome: "applied" | "refused";
  reason?: string;
  number?: number;
  actor: ArtifactActor;
  elevated?: boolean;
  origin?: string;
  work_item?: string;
  at: string;
  summary: ArtifactHistorySummary;
}

/** `artifact_history{id, cursor, limit}`: newest first, shaped by the reader's role. */
export interface ArtifactHistoryAnswer {
  entries: ArtifactHistoryEntry[];
  after?: string;
}

/**
 * One row of the company trail (`artifactsdomain.TrailEntry`): a history row
 * of any artifact, with the artifact it is about and its title and owner now.
 */
export interface ArtifactTrailEntry extends ArtifactHistoryEntry {
  artifact: string;
  title: string;
  owner: string;
}

/**
 * `artifact_trail{op, cursor, limit}`: every artifact's history across the
 * company, newest first in the log's order; an administrator's question
 * (`audit.read`).
 */
export interface ArtifactTrailAnswer {
  entries: ArtifactTrailEntry[];
  after?: string;
  position: LogPosition;
  complete: boolean;
}

/** `artifact_seen_by{id}`: who has opened it, or `shown: false` where the company withholds it. */
export interface ArtifactSeenByAnswer {
  shown: boolean;
  reads: { seat: string; opened_at: string }[];
}

/** One share and the seats a unit share reaches today (`artifactsapi.ShareAccess`). */
export interface ArtifactShareAccess extends ArtifactShare {
  members?: string[];
  /**
   * The share reaches nobody: a login named as a seat, which a build before the
   * grant rule wrote. Listed so its owner removes it; never offered a role.
   */
  invalid?: boolean;
}

/** `artifact_access{id}`: the share dialog's list (`artifactsapi.Access`). */
export interface ArtifactAccessAnswer {
  owner: string;
  owner_kind: string;
  role: ArtifactRole;
  /** To the owner and editors; absent below. */
  shares?: ArtifactShareAccess[];
  /** The seats that lead an agent owner, each holding owner on its artifacts. */
  leads?: string[];
  share_count: number;
}

/** The plan quotas in force (`artifacts.Limits`). */
export interface ArtifactLimits {
  company_bytes: number;
  artifacts: number;
  data_bytes: number;
  data_store_bytes: number;
  pending_bytes: number;
}

/** `artifact_usage`: the company's artifacts against its quota. */
export interface ArtifactUsageAnswer {
  usage: {
    artifacts: number;
    bytes: number;
    versions: number;
    assets: number;
    position: LogPosition;
    complete: boolean;
  };
  limits: ArtifactLimits;
}

/** `POST /artifacts/{id}/tickets`: what the viewer embeds (`artifacts.Frame`). */
export interface ArtifactFrame {
  frame_url: string;
  origin: string;
  version: number;
  expires_at: string;
  sandbox: string;
  allow: string;
  mode: OriginMode;
  nonce: string;
  /** The page the frame opens: the entry of a page, one artboard of a design. */
  document: string;
  /**
   * What the frame's ticket grants, in the canonical order, `[]` for none:
   * what the page's runtime is told as it loads, and so the one list its host
   * may tell it at the handshake.
   */
  capabilities: ArtifactCapability[];
}

/** How one declared file's bytes reach the store (`artifacts.Transfer`). */
export interface ArtifactTransfer {
  path: string;
  method: string;
  url: string;
  headers?: Record<string, string>;
  /** Straight to the bucket; false for a put through the engine, the dashboard's only kind. */
  direct: boolean;
}

/** `POST /artifacts/{id}/uploads` (`artifacts.UploadIntent`). */
export interface ArtifactUploadIntent {
  upload: string;
  expires_at: string;
  carried?: string[];
  transfers?: ArtifactTransfer[];
}

/**
 * One thing the publish linter found in a page (`lint.Finding`): a refusal
 * that refuses the publish, or a warning it lands with. `line` is 0 where the
 * finding is about the version rather than one line of it.
 */
export interface ArtifactLintFinding {
  path: string;
  line?: number;
  code: string;
  message: string;
}

/** `POST /artifacts/checks` (`artifactupload.CheckReport`): what a publish of the files would say. */
export interface ArtifactCheckAnswer {
  refusals: ArtifactLintFinding[];
  warnings: ArtifactLintFinding[];
}

/** One tab of a doc, with its Markdown (`docindex.Tab`). */
export interface DocTab {
  path: string;
  title: string;
  text: string;
}

/** A version's doc, tab by tab (`docindex.Doc`). */
export interface ArtifactDoc {
  /** Whether the version opens on a `doc.json` index rather than one Markdown file. */
  indexed: boolean;
  tabs: DocTab[];
}

/** `GET /artifacts/{id}/versions/{n}/doc` (`artifactsapi.DocAnswer`). */
export interface ArtifactDocAnswer {
  artifact: string;
  version: number;
  doc: ArtifactDoc;
}

/** `GET /artifacts/{id}/versions/{n}/source`: a version's entry as text (`artifactsapi.Source`). */
export interface ArtifactSource {
  artifact: string;
  kind: ArtifactKind;
  version: number;
  path: string;
  content_type: string;
  size: number;
  text: string;
}

/** One declaration of a slide's canonical style (`slideformat.Decl`). */
export interface SlideDecl {
  p: string;
  v: string;
}

/** One element, or one run of text, of a slide (`slideformat.Node`): an element has a tag, a run of text has text alone. */
export interface SlideNode {
  tag?: string;
  text?: string;
  attrs?: Record<string, string>;
  style?: SlideDecl[];
  children?: SlideNode[];
}

/** One slide of a deck, read (`slideformat.Slide`). */
export interface DeckSlide {
  id: string;
  path: string;
  title: string;
  hidden?: boolean;
  transition?: string;
  notes?: string;
  /** The `<section>`, its notes taken out. */
  root: SlideNode;
}

/** One run of a deck's outline (`slideformat.Section`). */
export interface DeckSection {
  description: string;
  start: string;
}

/** One typeface a deck loads from a font file of its version (`slideformat.Face`). */
export interface DeckFace {
  family: string;
  src: string;
}

/** A version's deck as the dashboard draws it (`slideformat.Deck`). */
export interface ArtifactDeck {
  title: string;
  /** The theme the deck began in (`deck.json`'s `theme`), absent for a deck that names none. */
  theme?: string;
  cover: string;
  sections: DeckSection[] | null;
  faces: DeckFace[] | null;
  slides: DeckSlide[] | null;
}

/** One slide a starter deck opens with: a layout and its title (`slideformat.PlanSlide`). */
export interface DeckPlanSlide {
  layout: string;
  /** Absent is the layout's own heading; a cover's is the deck's title. */
  title?: string;
}

/** What a starter deck is composed from (`slideformat.Plan`), sent as a create's `deck`. */
export interface DeckPlan {
  theme?: string;
  slides?: DeckPlanSlide[];
}

/** One layout a starter deck can hold (`slideformat.Layout`). */
export interface DeckLayout {
  id: string;
  name: string;
  description: string;
  /** The title a slide of this layout opens with when its plan names none. */
  heading: string;
}

/** One theme a deck can begin in, drawn as a deck of every layout once (`artifactsapi.SlidesStarterTheme`). */
export interface DeckTheme {
  id: string;
  name: string;
  tag: string;
  sample: ArtifactDeck;
  /**
   * The brand theme's cover logo (`artifactsapi.SampleLogo`): the path its
   * sample names and the adopted brand's file slot (`logo.primary`) it is
   * drawn from, since a sample holds no bytes. Absent for every other theme.
   */
  logo?: { path: string; slot: string };
}

/** `GET /artifacts/starters/slides` (`artifactsapi.SlidesStartersAnswer`). */
export interface SlidesStartersAnswer {
  themes: DeckTheme[];
  layouts: DeckLayout[];
  default: DeckPlan;
}

/** `GET /artifacts/{id}/versions/{n}/deck` (`artifactsapi.DeckAnswer`). */
export interface ArtifactDeckAnswer {
  artifact: string;
  version: number;
  deck: ArtifactDeck;
}

/** One slide of a deck's outline (`slideformat.OutlineSlide`). */
export interface DeckOutlineSlide {
  id: string;
  path: string;
  title: string;
  hidden?: boolean;
  notes?: boolean;
}

/** `GET /artifacts/{id}/versions/{n}/outline` (`artifactsapi.OutlineAnswer`). */
export interface ArtifactOutline {
  artifact: string;
  kind: ArtifactKind;
  version: number;
  slides?: {
    title: string;
    count: number;
    cover: string;
    sections: DeckSection[] | null;
    slides: DeckOutlineSlide[] | null;
    cover_slide: DeckSlide;
  };
  /** A doc's tabs and the first tab's opening (`docindex.Outline`). */
  doc?: {
    tabs: { path: string; title: string; bytes: number }[];
    cover: { tab: string; markdown: string };
  };
  design?: DesignOutline;
  /** A board's outline: its widgets and its first stats, never a value (D64). */
  dashboard?: DashboardOutline;
  /** A brand kit's outline: its `brand.json`, read (`brandformat.Brand`). */
  brand?: { document: BrandDocument };
}

// ---- dashboards (`internal/artifacts/dashspec`, `internal/api/datasetsapi`) ----

export type DashboardWidgetType = (typeof DASHBOARD_WIDGET_TYPES)[number];
export type DashboardSourceKind = (typeof DASHBOARD_SOURCE_KINDS)[number];
export type DashboardRefreshMode = (typeof DASHBOARD_REFRESH_MODES)[number];
export type DashboardDatasetStatus = (typeof DASHBOARD_DATASET_STATUSES)[number];
export type DashboardAgg = (typeof DASHBOARD_AGGS)[number];
export type DashboardNumberFormat = (typeof DASHBOARD_NUMBER_FORMATS)[number];
export type DashboardColumnType = (typeof DASHBOARD_COLUMN_TYPES)[number];
export type DashboardColumnWords = (typeof DASHBOARD_COLUMN_WORDS)[number];

/** One column of a dataset (`dashspec.Column`). */
export interface DashboardColumn {
  key: string;
  type: DashboardColumnType;
  label?: string;
  /** The vocabulary the column's ids are drawn in, if any. */
  words?: DashboardColumnWords;
}

/** A value of a row: text, a number, a boolean, a date as its canonical text, or none. */
export type DashboardValue = string | number | boolean | null;

/** One row of a dataset, keyed by column. */
export type DashboardRow = Record<string, DashboardValue>;

/** A dataset's rows as one viewer may read them (`dashspec.Table`). */
export interface DashboardTable {
  columns: DashboardColumn[];
  key?: string;
  rows: DashboardRow[];
  truncated?: boolean;
}

/** A filter the board offers (`dashspec.Param`). */
export interface DashboardParam {
  key: string;
  label: string;
  options: { value: string; label: string }[];
  default: string;
}

/** An engine argument: a literal in its text form, or a filter it is bound to. */
export type DashboardArg = string | number | boolean | { param: string };

/** One calculation step (`dashspec.Step`), flat as written. */
export interface DashboardStep {
  op: "filter" | "group" | "compute" | "join" | "sort" | "limit" | "select";
  where?: { column: string; cmp: string; value?: DashboardArg }[];
  by?: string[];
  aggregates?: { column: string; agg: DashboardAgg; of?: string }[];
  column?: string;
  fn?: string;
  left?: string | number;
  right?: string | number;
  with?: string;
  on?: string;
  columns?: string[];
  order?: { column: string; desc?: boolean }[];
  count?: number;
}

/** One dataset of a board as the spec declares it (`dashspec.Dataset`). */
export interface DashboardDataset {
  id: string;
  title: string;
  source: DashboardSourceKind;
  store?: { collection: string };
  file?: { path: string };
  engine?: { name: string; params?: Record<string, DashboardArg> };
  calc?: { from: string; steps: DashboardStep[] };
  columns?: DashboardColumn[];
  key?: string;
  refresh?: { every_seconds: number };
}

/** One widget of a board (`dashspec.Widget`), flat as written. */
export interface DashboardWidget {
  id: string;
  type: DashboardWidgetType;
  title?: string;
  dataset?: string;
  w: number;
  h?: number;
  value?: string;
  agg?: DashboardAgg;
  format?: DashboardNumberFormat;
  unit?: string;
  max?: number;
  max_field?: string;
  x?: string;
  y?: string[];
  by?: string;
  label?: string;
  limit?: number;
  columns?: string[];
  text?: string;
  tone?: string;
  children?: DashboardWidget[];
}

/** A board's spec (`dashspec.Spec`). */
export interface DashboardSpec {
  v: number;
  title?: string;
  params?: DashboardParam[];
  datasets: DashboardDataset[];
  widgets: DashboardWidget[];
}

/** How a dataset is kept current (`dashspec.Freshness`). */
export interface DashboardFreshness {
  mode: DashboardRefreshMode;
  collection?: string;
  signal?: "agents" | "tokens";
  every_seconds?: number;
  inputs?: string[];
}

/** One parameter of an engine source (`dashspec.SourceParam`). */
export interface DashboardSourceParam {
  key: string;
  type: "text" | "number" | "enum";
  summary: string;
  enum?: string[];
  min?: number;
  max?: number;
  max_bytes?: number;
  default?: string;
}

/** One entry of the engine source catalogue (`dashspec.EngineSource`). */
export interface DashboardEngineSource {
  name: string;
  domain: string;
  summary: string;
  access: string;
  params: DashboardSourceParam[];
  columns: DashboardColumn[];
  key: string;
  signal?: "agents" | "tokens";
  /** The source reads a span of time (spend over company days). */
  windowed?: boolean;
  max_rows: number;
}

/** One dataset as the Sources rail describes it (`artifactsapi.BoardDataset`). */
export interface DashboardBoardDataset {
  id: string;
  title: string;
  source: DashboardSourceKind;
  shape: { columns: DashboardColumn[]; key?: string };
  freshness: DashboardFreshness;
  engine?: DashboardEngineSource;
}

/** A board's outline (`dashspec.Outline`). */
export interface DashboardOutline {
  title?: string;
  widgets: {
    id: string;
    type: DashboardWidgetType;
    title?: string;
    w: number;
    dataset?: string;
    group?: string;
  }[];
  stats: { widget: string; label: string; dataset: string; dataset_title: string }[];
  datasets: { id: string; title: string; source: DashboardSourceKind }[];
}

/** A version's board (`GET …/versions/{n}/dashboard`, `artifactsapi.BoardAnswer`). */
export interface DashboardBoardAnswer {
  artifact: string;
  version: number;
  board: DashboardSpec;
  datasets: DashboardBoardDataset[];
  outline: DashboardOutline;
}

/** What one read of a dataset or a source answered (`datasetsapi.Reading`). */
export interface DashboardReading {
  status: DashboardDatasetStatus;
  as_of?: string;
  table?: DashboardTable;
  refusal?: { dataset: string; reason: string; grants?: string[] };
  detail?: string;
}

/** One dataset as its viewer may read it (`GET …/sources/{dataset}`, `datasetsapi.Answer`). */
export interface DashboardDatasetAnswer extends DashboardReading {
  artifact: string;
  version: number;
  dataset: string;
}

/** One engine source read directly, before any board exists (`GET /artifacts/sources/preview/{name}`, `datasetsapi.Preview`). */
export interface DashboardSourcePreview extends DashboardReading {
  source: string;
}

/** The engine source catalogue (`GET /artifacts/sources`). */
export interface DashboardCatalogueAnswer {
  sources: DashboardEngineSource[];
}

/** A rectangle of a design's canvas, in canvas pixels (`canvasformat.Rect`). */
export interface DesignRect {
  x: number;
  y: number;
  w: number;
  h: number;
}

/** One board of a design's outline (`canvasformat.OutlineBoard`). */
export interface DesignOutlineBoard extends DesignRect {
  path: string;
  title?: string;
}

/** A design's outline (`canvasformat.Outline`): its boards' frames, for a cover. */
export interface DesignOutline {
  title?: string;
  count: number;
  cover: string;
  bounds: DesignRect;
  boards: DesignOutlineBoard[];
  notes: number;
}

/** A create's or a publish's answer (`artifactsapi.publishedAnswer`). */
export interface ArtifactPublished {
  artifact: string;
  label: string;
  number: number;
  version_id: string;
  warnings?: ArtifactLintFinding[];
  /** The version's files by where their bytes came from (`artifactupload.FileCounts`). */
  files?: { stored: number; carried: number };
  op_id: string;
  outcome: "applied" | "pending" | "unknown";
  position: LogPosition;
}

/** One choice of a select, multiselect or status column (`artifacts.Option`). */
export interface DataOption {
  id: string;
  label: string;
  color?: string;
  category?: string;
  archived?: boolean;
}

/** One declared field of a collection (`artifacts.Column`). */
export interface DataColumn {
  key: string;
  type: ColumnType;
  label?: string;
  required?: boolean;
  primary?: boolean;
  hidden?: boolean;
  min?: number;
  max?: number;
  max_length?: number;
  max_items?: number;
  options?: DataOption[];
  default?: unknown;
  write?: ArtifactRole;
  when?: FieldCondition;
  assigned_field?: string;
  mode?: "append";
  immutable?: boolean;
  humans_only?: boolean;
}

/** One named set of documents with its rules (`artifacts.Collection`). */
export interface DataCollection {
  name: string;
  title?: string;
  preset?: CollectionPreset;
  read: ReadRule;
  create: ArtifactRole;
  delete: ArtifactRole;
  max_rows?: number;
  max_rows_per_principal?: number;
  once_per_principal?: boolean;
  /** Absent for a schemaless collection under the closed default. */
  columns?: DataColumn[];
  indexed?: string[];
}

/** A store's catalogue row (`artifactdata.StoreInfo`). */
export interface DataStoreInfo {
  artifact: string;
  docs: number;
  /** `doc_bytes` plus `history_bytes`: what the store's rows cost on every data node. */
  footprint: number;
  doc_bytes: number;
  history_bytes: number;
  body_bytes: number;
  field_entries: number;
  writes: number;
  digest: string;
  history_floor: number;
  tombstones_floor: number;
  version: number;
}

/** `artifact_data_schema{id}`: the collections the reader may see, the store, and the reader's role. */
export interface DataSchemaAnswer {
  collections: DataCollection[];
  store: DataStoreInfo;
  role: ArtifactRole;
}

/** One document (`artifactdata.Doc`). */
export interface DataDoc {
  collection: string;
  id: string;
  /** Canonical JSON of the fields, a column's default filled where absent; absent on a tombstone. */
  fields?: Record<string, unknown>;
  deleted?: boolean;
  /** The document's version: the `if_version` a guarded write names. */
  version: number;
  created_by: ArtifactActor;
  created_at: string;
  updated_by: ArtifactActor;
  updated_at: string;
  defaulted?: string[];
  /** Stored fields the schema no longer admits, shown as a warning. */
  invalid?: string[];
}

/** `artifact_data{id, collection, since, cursor, limit}` and `artifact_data_query`: a page of documents. */
export interface DataDocPage {
  docs: DataDoc[];
  after?: string;
  /** The `since` asked is below what the store still accounts for: read the collection whole. */
  resync?: boolean;
  /**
   * The next catch-up's `since`, adopted once the read's LAST page is in: a
   * whole read's pages answer the version its first page saw, and a catch-up's
   * the version every change at or below which it has returned.
   */
  through: number;
  position: LogPosition;
  complete: boolean;
}

/** One field's change in a history row (`artifactdata.FieldChange`). */
export interface DataFieldChange {
  f: string;
  b?: unknown;
  a?: unknown;
  bh?: string;
  ah?: string;
}

/** One document's change in a history row (`artifactdata.Change`). */
export interface DataChange {
  c: string;
  d?: string;
  /** `create`, `update`, `delete`, `migrate`, `conflict` or `schema`. */
  op: string;
  /** The row's creator at this change: a row deleted and created again by somebody else changes hands. */
  cb?: string;
  bv?: number;
  av?: number;
  f?: DataFieldChange[];
}

/** One row of a store's history (`artifactdata.HistoryEntry`). */
export interface DataHistoryEntry {
  seq: number;
  op_id: string;
  /** `write`, `undo`, `configure` or `purge`. */
  op: string;
  verdict: DataVerdict;
  reason?: string;
  actor: ArtifactActor;
  origin?: string;
  work_item?: string;
  elevated?: boolean;
  client_id?: string;
  at: string;
  changes: DataChange[];
  undoable?: boolean;
  undoes?: number;
  digest: string;
}

/** `artifact_data_history{id, collection, doc, field, actor, cursor, limit}`. */
export interface DataHistoryAnswer {
  entries: DataHistoryEntry[];
  after?: string;
  /** The oldest position the hot history still holds. */
  floor: number;
  position: LogPosition;
  complete: boolean;
}

/** One of the reader's per-viewer state keys (`artifactdata.StateEntry`). */
export interface DataStateEntry {
  key: string;
  value: unknown;
  version: number;
}

/** `artifact_data_state{id}`. */
export interface DataStateAnswer {
  state: DataStateEntry[];
}

/** `artifact_data_usage`: the company's datastore footprint against its quota. */
export interface DataUsageAnswer {
  usage: {
    stores: number;
    docs: number;
    footprint: number;
    position: LogPosition;
    complete: boolean;
  };
  data_bytes: number;
  data_store_bytes: number;
}

/** One op of a datastore write (`artifacts.DataOp`). */
export interface DataOp {
  op: DataOpKind;
  collection: string;
  doc: string;
  if_version?: number;
  fields?: Record<string, unknown>;
  field?: string;
  value?: unknown;
}

/** A datastore write's answer, success or the applier's refusal (`artifactdataapi.answer`). */
export interface DataWriteAnswer {
  op_id: string;
  position?: LogPosition;
  /** How the write ended: the applier's verdict, or `pending` while the node that took it has not applied it. */
  verdict: DataVerdict | "pending";
  /** The write's place in the store's history: what an undo names. */
  seq?: number;
  detail?: string;
  /** The current documents, on a `conflict`. */
  conflict?: DataDoc[];
}

/** Where in an artifact a member is looking (`presence.Focus`). */
export interface PresenceFocus {
  version?: number;
  region?: string;
  collection?: string;
  doc?: string;
  field?: string;
}

/** One open viewer of one artifact (`presence.Member`). */
export interface ArtifactPresenceMember {
  session: string;
  /** The seat handle, which the dashboard resolves to a name, or the login of one acting under no seat. */
  seat: string;
  kind: string;
  activity: PresenceActivity;
  focus: PresenceFocus;
  at: string;
  /** For an agent, the run of the turn whose tool call last put it in the room. */
  turn?: string;
}

/** Who is in one artifact's room (`presence.Roster`). */
export interface ArtifactPresenceRoster {
  members: ArtifactPresenceMember[] | null;
  counts?: { human: number; agent: number };
  truncated?: boolean;
}

/** An `artifact_join`'s answer (`stream.ArtifactJoined`). */
export interface ArtifactJoined {
  artifact: string;
  session: string;
  role: ArtifactRole;
  roster: ArtifactPresenceRoster;
  through: number;
  resync: boolean;
}

/** An `artifact_changed` push (`stream.ArtifactChange`): a hint, never content. */
export interface ArtifactChangedPush {
  artifact: string;
  /**
   * `version`, `meta`, `share`, `asset`, `trash`, `restore`, `purge`,
   * `transfer`, `reset` or `folder` (it was filed in another folder; a
   * folder's removal moves its artifacts up and pushes nothing per artifact).
   */
  what: string;
  head?: number;
  position: LogPosition;
}

/** One changed document, inline in an `artifact_data` push (`artifactdata.DocDelta`). */
export interface DataDocDelta {
  collection: string;
  doc: string;
  version: number;
  deleted?: boolean;
  body?: Record<string, unknown>;
  read: ReadRule;
  created_by: string;
  updated_by: ArtifactActor;
  updated_at: string;
}

/** An `artifact_data` push, as this reader may see it (`stream.ArtifactData`). */
export interface ArtifactDataPush {
  artifact: string;
  deltas?: DataDocDelta[];
  /** The changed documents named and not carried, with `reread`. */
  keys?: { collection: string; doc: string; version: number; deleted?: boolean }[];
  reread?: boolean;
  schema?: boolean;
  resync?: boolean;
  gone?: boolean;
  through?: number;
}

/** An `artifact_presence` push (`stream.ArtifactPresence`). */
export interface ArtifactPresencePush {
  artifact: string;
  upsert: ArtifactPresenceMember[] | null;
  leave: string[] | null;
  counts?: { human: number; agent: number };
  truncated: boolean;
}

export interface QueryMap {
  viewer: Viewer;
  work_inbox: WorkInboxAnswer;
  agent: AgentAnswer;
  live_call: LiveCallAnswer;
  agent_memory: AgentMemory;
  agent_episode: AgentEpisode;
  memory_overview: MemoryOverview;
  events: EventsPage;
  event_series: EventSeries;
  event: EventRecord & { coverage: Coverage };
  trace: TraceAnswer;
  turn: TurnAnswer;
  turns: TurnsAnswer;
  phases: PhasesPage;
  tokens: Rollup;
  token_series: TokenSeries;
  seat_activity: SeatActivityAnswer;
  fleet: FleetAnswer;
  fleet_broker: FleetBrokerAnswer;
  mcp_servers_status: McpServersStatusAnswer;
  backups: BackupsAnswer;
  budgets: BudgetsAnswer;
  schedules: SchedulesAnswer;
  schedule_runs: ScheduleRunsAnswer;
  integrations: IntegrationsAnswer;
  link_hosts: LinkHostsAnswer;
  sandbox_runs: { runs: SandboxRun[] };
  sandbox_tail: SandboxTailAnswer;
  retention: RetentionReport;
  sign_in_policy: SignInPolicyAnswer;
  limits: LimitsAnswer;
  work_items: WorkItemsAnswer;
  work_item: WorkItemDetail;
  work_comments: WorkCommentsAnswer;
  work_item_turns: WorkItemTurnsAnswer;
  work_views: WorkViewsAnswer;
  work_files: WorkFilesAnswer;
  /** Every saved view a viewer can see, across every container. */
  work_saved_views: WorkViewsAnswer;
  work_projects: WorkProjectsAnswer;
  work_project: WorkProjectDetail;
  work_workload: WorkloadAnswer;
  work_activity: WorkActivityAnswer;
  work_my_work: WorkMyWork;
  work_catalogue: WorkCatalogueAnswer;
  work_person: WorkPersonState;
  work_search: WorkSearchAnswer;
  work_routing: WorkRoutingAnswer;
  work_flow: WorkFlowAnswer;
  company_feed: CompanyFeedAnswer;
  decisions: DecisionsAnswer;
  answered_decisions: AnsweredDecisionsAnswer;
  /* THE CHAT FAMILY IS THE CHATLET APP'S, ALL OF IT, and asked for the seat the
     caller acts as and nobody else: none of these takes a handle — there is
     nowhere to write one. A caller without the app is refused `unauthorized`
     naming it; a credential acting as no seat (a deployment's own token, a
     service account bound to nobody) is refused `unauthorized` naming no
     grant, since no grant would cure it — a binding in the identity
     directory (`crewlet iam bind`) would. */
  chat_channels: ChatChannelsAnswer;
  chat_channel: ChatChannelDetail;
  chat_messages: ChatMessagesAnswer;
  /* A THREAD IS ITS OWN QUESTION, not a filter on the room: it is opened
     beside the transcript and paged in the opposite direction, because a
     conversation is read forwards. */
  chat_thread: ChatThreadAnswer;
  chat_mentions: ChatMentionsAnswer;
  /* EVERY ROOM THE CALLER MAY READ, BY NAME, joined or not: what a room
     written in prose is told from a colour or an issue's number by. */
  chat_room_names: ChatRoomNamesAnswer;
  /* REGISTERED ONLY WHERE THE INDEX EXISTS, so a node with rooms and no index
     answers `unknown_query` rather than a ranking that finds nothing. */
  chat_search: ChatSearchAnswer;
  pages: PagesAnswer;
  page: PageDetail;
  containers: { containers: PageContainer[] };
  page_activity: PageActivityAnswer;
  page_reads: PageReadsAnswer;
  page_revision: PageRevisionBody;
  conversations: ConversationsAnswer;
  a2a_channels: A2AAnswer;
  knowledge: KnowledgeAnswer;
  colleague: ColleagueAnswer;
  config: CompanyDocument | null;
  config_audit: RevisionMeta[];
  config_diff: ConfigDiff;
  config_entities: { kind: string; ids?: string[]; id?: string; entity?: unknown };
  artifacts: ArtifactListAnswer;
  artifact_folders: ArtifactFoldersAnswer;
  artifact: ArtifactDetail;
  artifact_versions: ArtifactVersionsAnswer;
  artifact_version: ArtifactVersion;
  artifact_history: ArtifactHistoryAnswer;
  artifact_assets: ArtifactAssetsAnswer;
  artifact_seen_by: ArtifactSeenByAnswer;
  artifact_access: ArtifactAccessAnswer;
  artifact_usage: ArtifactUsageAnswer;
  artifact_trail: ArtifactTrailAnswer;
  artifact_data: DataDocPage;
  artifact_data_doc: DataDoc;
  artifact_data_query: DataDocPage;
  artifact_data_history: DataHistoryAnswer;
  artifact_data_schema: DataSchemaAnswer;
  artifact_data_state: DataStateAnswer;
  artifact_data_usage: DataUsageAnswer;
  brand: BrandAnswer;
}

export type QueryName = keyof QueryMap;
