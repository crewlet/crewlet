/**
 * The Dashboard Assistant's protocol, as the engine speaks it
 * (`internal/assistant/wire`).
 *
 * EVERY DECLARATION HERE IS HELD TO ITS GO TWIN, both ways, by the gates in
 * `internal/assistant/wire/client_gate_test.go`: each closed set is the same
 * set, each limit the same number, and each shape carries the same members
 * with the same optionality (a Go field that is `omitempty` is optional here,
 * and every other member is required). A copy that drifts is a failing build
 * on the engine's side until this file follows it.
 *
 * A frame nests its payload under a member named for its kind, so
 * `protocol/assistantWire.ts` narrows a frame by `kind` to exactly one
 * payload. A kind or a card this build does not know is ignored, or drawn as a
 * notice, within one protocol version.
 *
 * A shape only another shape composes is declared without `export`: the gates
 * still read it, and a screen that needs one by name exports it then.
 */

import type { Remedy } from "./remedies.ts";

/** The protocol version every frame and every request carries. */
export const ASSISTANT_PROTOCOL_VERSION = 1;
/** The longest question a turn takes, in characters (Unicode code points). */
export const ASSISTANT_INPUT_MAX_RUNES = 4000;
/** The most a turn's page context may encode to, in bytes. */
export const ASSISTANT_PAGE_CONTEXT_MAX_BYTES = 16384;
/** The most resources a page context names. */
export const ASSISTANT_PAGE_CONTEXT_MAX_RESOURCES = 50;
/** The most page actions a page context offers. */
export const ASSISTANT_PAGE_CONTEXT_MAX_ACCEPTS = 32;
/** The most fields an open draft's snapshot lists; the rest are counted in its `more`. */
export const ASSISTANT_PAGE_DRAFT_MAX_FIELDS = 40;
/** The longest value a draft field carries, in characters; a longer one is cut and marked `truncated`. */
export const ASSISTANT_PAGE_DRAFT_VALUE_MAX_RUNES = 1000;
/** The longest help a screen gives with its draft, in characters. */
export const ASSISTANT_PAGE_DRAFT_HELP_MAX_RUNES = 600;
/**
 * A draft field path that names a credential, read by segment: such a field is
 * sent redacted whatever the screen said (`wire.PageDraftSecretPath`).
 */
export const ASSISTANT_PAGE_DRAFT_SECRET_PATH =
  "(^|[._])(password|passphrase|secret|secrets|token|tokens|credential|credentials|(api|private|signing|access|secret)[._]?key)([._]|$)";
/** The longest reason an administrator gives for a compliance read. */
export const ASSISTANT_AUDIT_REASON_MAX_RUNES = 500;
/** How often a quiet stream writes a keepalive comment, in seconds. */
export const ASSISTANT_KEEPALIVE_SECONDS = 15;
/** The reserved action the server sends after a write lands. */
export const ASSISTANT_ACTION_CONTEXT_INVALIDATE = "context.invalidate";
/** What opens a proposal's digest on the wire. */
export const ASSISTANT_DIGEST_PREFIX = "sha256:";

// --- deep work (im-deep) ---
/** The most lines of a deep run's own account its step carries. */
export const ASSISTANT_DEEP_ACTIVITY_LINES = 8;
/** The largest file a person attaches or a run delivers. */
export const ASSISTANT_FILE_MAX_BYTES = 5242880;
/** The most files one question carries. */
export const ASSISTANT_ATTACHMENTS_PER_TURN = 4;
/** The most files one deep run hands back. */
export const ASSISTANT_DELIVERED_FILES_PER_RUN = 10;

/** A frame's kind on the live stream. */
export type AssistantEventKind =
  | "conversation"
  | "page_context"
  | "status"
  | "delta"
  | "step"
  | "card"
  | "action"
  | "done"
  | "error"
  | "snapshot";

/** Where a turn stands. */
type AssistantTurnState =
  | "running"
  | "waiting_for_approval"
  | "completed"
  | "stopped"
  | "failed"
  | "interrupted"
  | "expired"
  | "budget_exhausted"
  | "superseded";

/** The run chip's state while a segment runs. */
type AssistantStatusState = "thinking" | "working" | "waiting_for_approval" | "applying";

/** One tool step's state. */
type AssistantStepState = "running" | "done" | "failed" | "refused" | "stopped" | "waiting";

/** An engine-built card's kind. */
export type AssistantCardKind =
  | "table"
  | "entity"
  | "proposal"
  | "open_in_app"
  | "error"
  | "notice"
  | "oversize"
  | "file"
  | "brief";

/** Where a proposal stands. */
type AssistantProposalState =
  | "proposed"
  | "approved"
  | "executing"
  | "applying"
  | "applied"
  | "failed"
  | "unknown"
  | "declined"
  | "expired"
  | "stale";

/** What a proposal changes: the company, or the screen the person has open. */
type AssistantProposalScope = "server" | "page";

/** How much care a proposal asks of the person approving it; absent for an ordinary change. */
type AssistantProposalRisk = "destructive";

/** One row of a proposal's change list, in the before and after grammar. */
type AssistantChangeKind = "fact" | "changed" | "added" | "removed" | "text";

/** What happened to one run of words in a text change. */
type AssistantWordOp = "same" | "added" | "removed";

/**
 * How a table card's column, or an entity's fact, is drawn and exported:
 * `label` is a slug from one of the engine's closed sets (a status, a
 * priority, an outcome), drawn in the words the dashboard's screens use.
 */
type AssistantColumnType = "text" | "number" | "tokens" | "duration" | "datetime" | "label";

/** A table column's alignment. */
type AssistantAlign = "start" | "end";

/** A notice card's tone. */
type AssistantNoticeTone = "info" | "warning";

/** A turn-level failure on an error frame. */
export type AssistantErrorCode =
  | "model_unavailable"
  | "model_signed_out"
  | "model_busy"
  | "model_timeout"
  | "turn_timeout"
  | "step_limit"
  | "budget_exhausted"
  | "turn_interrupted"
  | "grant_ended"
  | "internal_error"
  | "access_withdrawn"
  | "brief_invalid";

/** What a page context's resource or selection names. */
type AssistantResourceKind =
  | "task"
  | "project"
  | "seat"
  | "unit"
  | "page"
  | "container"
  | "artifact"
  | "turn"
  | "run"
  | "schedule"
  | "view"
  | "room";

/**
 * What the person calls the record a screen has open, as the drawer names it
 * ("Brief me on this decision"): an ask in the Inbox is a decision, a seat an
 * agent.
 */
export type AssistantRecordNoun =
  | "decision"
  | "update"
  | "task"
  | "project"
  | "agent"
  | "artifact"
  | "page"
  | "turn"
  | "run"
  | "room";

/** What a question asks for: an answer, or a brief of the page or its open record. */
export type AssistantTurnMode = "ask" | "brief";

/** How loud a brief's row is; danger only where the engine read a fact that says so. */
type AssistantBriefTone = "danger" | "warning";

/** What a brief's action does: open a record's screen, or ask a question. Never a write. */
type AssistantBriefActionKind = "open" | "ask";

/** Why the assistant is not available to the viewer. */
export type AssistantViewerReason =
  "disabled" | "no_model" | "not_a_person" | "no_permission" | "model_not_ready";

/** Whether a page action asks for the person's approval. */
type AssistantActionConfirm = "approve" | "none";

/** What the page reports about a page action it was sent. */
type AssistantActionState = "applying" | "applied" | "stale" | "blocked" | "invalid" | "no_handler";

/** Who said a message. */
type AssistantMessageRole = "user" | "assistant";

/** Why an open in app card may not be opened. */
type AssistantOpenReason = "not_permitted";

/** One event on a turn's live stream: the head and exactly one payload. */
export interface AssistantFrame {
  v: number;
  conversation_id: string;
  turn_id: string;
  seq: number;
  at: string;
  kind: AssistantEventKind;
  conversation?: AssistantConversationEvent;
  page_context?: AssistantPageContextEvent;
  status?: AssistantStatusEvent;
  delta?: AssistantDeltaEvent;
  step?: AssistantStepEvent;
  card?: AssistantCardEvent;
  action?: AssistantActionEvent;
  done?: AssistantDoneEvent;
  error?: AssistantErrorEvent;
  snapshot?: AssistantSnapshotEvent;
}

/** The first frame of every stream. */
export interface AssistantConversationEvent {
  title: string;
  created_at: string;
  resumed: boolean;
}

/** What the turn was told about the screen. */
export interface AssistantPageContextEvent {
  used: boolean;
  trimmed: boolean;
  page?: AssistantPageChip;
}

/** The page a message was sent with, as its chip shows it. */
interface AssistantPageChip {
  title: string;
  screen: string;
}

/** The run chip's state. */
export interface AssistantStatusEvent {
  state: AssistantStatusState;
  steps_done: number;
}

/** Text appended to one assistant message; `restart` discards what it drew. */
export interface AssistantDeltaEvent {
  message_id: string;
  text: string;
  restart?: boolean;
}

/** One tool step, at its start and at its end. */
export interface AssistantStepEvent {
  step_id: string;
  /** The step's turn: set on every transcript and snapshot step; a live frame's head names it. */
  turn_id?: string;
  index: number;
  tool: string;
  label: string;
  request?: AssistantStepRequest;
  state: AssistantStepState;
  started_at: string;
  duration_ms?: number;
  summary?: string;
  refusal?: AssistantRefusal;
  // --- deep work (im-deep) ---
  /** The deep-work step this step was made from. */
  parent?: number;
  /** The run a deep-work step is, re-sent as it progresses. */
  deep?: AssistantDeepProgress;
}

/** The ordinary request a step made: its method and route pattern. */
interface AssistantStepRequest {
  method: string;
  route: string;
}

/** A refusal in the engine's own words. */
interface AssistantRefusal {
  code: string;
  message: string;
  hint?: string;
  grants?: string[];
}

/** One card, attached to the assistant message it belongs to. */
export interface AssistantCardEvent {
  card_id: string;
  message_id: string;
  card: AssistantCard;
}

/** An instruction for the page. */
export interface AssistantActionEvent {
  action_id: string;
  name: string;
  proposal_id?: string;
  source_revision?: string;
  payload?: unknown;
  confirm?: AssistantActionConfirm;
  invalidate?: AssistantInvalidate;
}

/** A write that landed, so open screens read at or after its position. */
interface AssistantInvalidate {
  domain: string;
  position: string;
}

/** The end of a turn segment. */
export interface AssistantDoneEvent {
  state: AssistantTurnState;
  duration_ms: number;
  steps: number;
  message_id?: string;
  trace_id?: string;
}

/** A turn-level failure the person reads. */
export interface AssistantErrorEvent {
  code: AssistantErrorCode;
  message: string;
  hint?: string;
  retryable: boolean;
  retry_after_seconds?: number;
  /** Where the limit that caused the error is changed (a token budget spent). */
  remedy?: Remedy;
}

/** What a resuming client redraws when the frame log cannot serve it. */
export interface AssistantSnapshotEvent {
  messages: AssistantMessage[];
  steps: AssistantStepEvent[];
  cards: AssistantCardEvent[];
}

/** One engine-built card: its kind and exactly that kind's body. */
export interface AssistantCard {
  type: AssistantCardKind;
  table?: AssistantTableCard;
  entity?: AssistantEntityCard;
  proposal?: AssistantProposalCard;
  open_in_app?: AssistantOpenInAppCard;
  error?: AssistantErrorCard;
  notice?: AssistantNoticeCard;
  oversize?: AssistantOversizeCard;
  // --- deep work (im-deep) ---
  file?: AssistantFileCard;
  brief?: AssistantBriefCard;
}

/** Rows from a read. */
export interface AssistantTableCard {
  title: string;
  window?: string;
  columns: AssistantColumn[];
  rows: (string | number | boolean | null)[][];
  total?: (string | number | boolean | null)[];
  row_count: number;
  truncated: boolean;
  /**
   * The answer says rows lie past the page it read, so `row_count` is a page
   * and never the total: the card's screen shows the rest.
   */
  more?: boolean;
  as_of: string;
  exports?: AssistantExports;
  open?: AssistantLink;
}

/** A table card's column. */
interface AssistantColumn {
  key: string;
  label: string;
  type: AssistantColumnType;
  align?: AssistantAlign;
  /** Left out of the drawer to fit it; the downloads and the model keep it. */
  detail?: boolean;
}

/** A table card's download paths. */
interface AssistantExports {
  csv: string;
  xlsx: string;
}

/** A dashboard address the engine built from its own route table. */
interface AssistantLink {
  label: string;
  screen: string;
  href: string;
}

/** One record, or a list of records. */
export interface AssistantEntityCard {
  /** The dashboard object the record is; absent for a generic read's record, which is the API's. */
  kind?: AssistantResourceKind;
  id: string;
  /**
   * The short name the record is written by in prose: a task's key
   * ("GOP-11"), a project's ("GOP"). The answer's first mention of it is the
   * record's chip (`app/assistant/chunk/mentions.ts`); absent for a record
   * with none, a seat included, which the answer names by its name.
   */
  key?: string;
  title: string;
  subtitle?: string;
  facts?: AssistantFact[];
  text?: string;
  items?: AssistantEntityItem[];
  /** How many items the record holds where `items` is only the first of them. */
  items_total?: number;
  /**
   * The answer says items lie past the page it read (a thread's older
   * comments), so `items` is a page and never the whole list: the record's
   * screen shows the rest.
   */
  items_more?: boolean;
  /** What one item and several are called ("policy", "policies"), where the card has a list. */
  items_noun?: AssistantNoun;
  open?: AssistantLink;
}

/** What one thing and several are called, in the lower case a count is said with. */
interface AssistantNoun {
  one: string;
  many: string;
}

/** One labelled value on an entity card. */
interface AssistantFact {
  label: string;
  /** The value as the API answered it, in the form its type names. */
  value: string;
  /** How the value is drawn; absent, it is text. */
  type?: AssistantColumnType;
  mono?: boolean;
  /** Kept on the card for the model (a version an update is proposed against), not drawn. */
  detail?: boolean;
}

/** One row of an entity card's list. */
interface AssistantEntityItem {
  /** The dashboard object the item is; absent for none. */
  kind?: AssistantResourceKind;
  id: string;
  label: string;
  fact?: string;
  depth?: number;
  open?: AssistantLink;
}

/** A change waiting on the person, rendered from the server's own preview. */
export interface AssistantProposalCard {
  proposal_id: string;
  scope: AssistantProposalScope;
  tool: string;
  title: string;
  target?: string;
  changes: AssistantChange[];
  /** What the change does, above the rows, where the title does not say it: what a destructive change takes away. */
  summary?: string;
  /** The consequence, under the rows: whether and how the change can be reverted. */
  note?: string;
  risk?: AssistantProposalRisk;
  /** The exact request, for an approver who administers the company; absent for everybody else. */
  technical?: AssistantTechnicalRequest;
  audience?: string[];
  taint?: string;
  step_up?: boolean;
  digest: string;
  expires_at: string;
  state: AssistantProposalState;
  source_revision?: string;
  page_action?: AssistantPageActionRef;
  approve_label: string;
  receipt?: AssistantReceipt;
  failure?: AssistantFailure;
}

/** A proposal's request as an administrator reads it behind "Technical details". */
interface AssistantTechnicalRequest {
  method: string;
  path: string;
  body?: string;
}

/** The page action a page-scope proposal applies; the screen parses the payload. */
interface AssistantPageActionRef {
  name: string;
  payload: unknown;
}

/** One row of a proposal's change list. */
interface AssistantChange {
  kind: AssistantChangeKind;
  field: string;
  label: string;
  from?: string;
  to?: string;
  /** `to` as a list, one entry per item, where the value is a list of plain values. */
  items?: string[];
  /** `from` as a list, likewise. */
  from_items?: string[];
  words?: AssistantWord[];
  new?: boolean;
  note?: string;
}

/** One run of words in a text change. */
interface AssistantWord {
  op: AssistantWordOp;
  text: string;
}

/** What an applied proposal did. */
interface AssistantReceipt {
  summary: string;
  position?: string;
  open?: AssistantLink;
}

/** A proposal that did not apply. */
interface AssistantFailure {
  refusal: AssistantRefusal;
  retryable: boolean;
}

/** A link to a screen or a record, and whether this person may open it. */
export interface AssistantOpenInAppCard {
  link: AssistantLink;
  permitted: boolean;
  reason?: AssistantOpenReason;
}

/** A refusal or a failure in the engine's own words. */
export interface AssistantErrorCard {
  refusal: AssistantRefusal;
  phrase?: string;
  hint?: string;
  retryable?: boolean;
}

/** A line the engine adds. */
export interface AssistantNoticeCard {
  tone: AssistantNoticeTone;
  text: string;
}

/**
 * A read whose answer was past what one step reads, so none of it is shown:
 * what it read, the screen that shows all of it, and what would narrow it.
 * The drawer lists it with the turn's other reads (ADR-0061).
 */
export interface AssistantOversizeCard {
  title: string;
  open?: AssistantLink;
  narrow?: string[];
}

/**
 * What a screen tells a turn about itself: what is in view by id, never a
 * record's content, and the draft the person has open with its values.
 */
export interface AssistantPageContext {
  screen: string;
  title: string;
  params: Record<string, string>;
  query: Record<string, string>;
  resources: AssistantResource[];
  selection: AssistantResourceRef | null;
  source_revision: string;
  accepts: string[];
  /**
   * What each accepted action takes on this screen now, by action id: every
   * accepted action but `draft.set`, whose arguments are the open draft's
   * editable fields. The engine holds a proposal to it before any card.
   */
  action_args?: Record<string, AssistantActionArgs>;
  /** Each accepted action's own revision: what it depends on, so it goes stale only when that moves. */
  action_revisions?: Record<string, string>;
  draft?: AssistantPageDraft;
}

/** A JSON type an action's argument takes. */
type AssistantArgType = "object" | "array" | "string" | "number" | "integer" | "boolean" | "null";

/**
 * A page action's arguments as the screen declares them: a closed subset of
 * JSON Schema (`internal/assistant/wire.ActionArgs`), built from the same
 * declaration the screen's parser reads, so the two cannot disagree. The
 * engine refuses a keyword outside this subset.
 */
export interface AssistantActionArgs {
  type?: AssistantArgType | AssistantArgType[];
  description?: string;
  enum?: string[];
  const?: string;
  minLength?: number;
  maxLength?: number;
  minItems?: number;
  maxItems?: number;
  items?: AssistantActionArgs;
  properties?: Record<string, AssistantActionArgs>;
  required?: string[];
  patternProperties?: Record<string, AssistantActionArgs>;
  additionalProperties?: boolean;
  oneOf?: AssistantActionArgs[];
  /** Whole values the screen takes; top level only, each held to the schema itself. */
  examples?: unknown[];
}

/** Where an open draft's values live: in the page until saved, or saved. */
export type AssistantDraftKind = "client" | "server";

/** What is open in a screen's editor: which draft, where the person is, and its values. */
export interface AssistantPageDraft {
  kind: AssistantDraftKind;
  id?: string;
  revision?: string;
  step?: string;
  help?: string;
  fields: AssistantDraftField[];
  more: number;
}

/** A draft field's shape, and so of the value a change to it takes. */
export type AssistantDraftFieldKind = "text" | "list" | "file";

/** One value of an open draft: a list's items one per line, a file's name. */
export interface AssistantDraftField {
  path: string;
  label: string;
  kind: AssistantDraftFieldKind;
  value: string;
  hint?: string;
  editable?: boolean;
  redacted?: boolean;
  truncated?: boolean;
}

/**
 * The value a change to a file field takes: a file of the person's own
 * conversation, by id, never its bytes or its markup.
 */
export interface AssistantDraftFileRef {
  kind: AssistantDraftFieldKind;
  file_id: string;
  conversation_id: string;
  name: string;
  media_type: string;
}

/** One thing in view, by id. */
interface AssistantResource {
  kind: AssistantResourceKind;
  id: string;
  version?: string;
}

/** One thing, named, with what the screen calls it where it says. */
export interface AssistantResourceRef {
  kind: AssistantResourceKind;
  id: string;
  noun?: AssistantRecordNoun;
}

/** A body carrying only the protocol version. */
export interface AssistantVersioned {
  protocol_version: number;
}

/** A question: `POST /assistant/conversations/{id}/turns`. */
export interface AssistantTurnRequest {
  protocol_version: number;
  input: AssistantTurnInput;
  page_context: AssistantPageContext | null;
  /** What the question asks for; absent is `ask`. A brief needs the page context. */
  mode?: AssistantTurnMode;
  // --- deep work (im-deep) ---
  /** Files uploaded to the conversation for this question. */
  attachments?: string[];
}

/** The person's question. */
interface AssistantTurnInput {
  text: string;
}

/** The turn a question started, and where to read it. */
export interface AssistantTurnAccepted {
  turn_id: string;
  stream_url: string;
  after_seq: number;
}

/** An approval or a decline, echoing the digest the card showed. */
export interface AssistantProposalDecision {
  protocol_version: number;
  digest: string;
  /** The screen the person decided from, as it is now: the resumed turn runs on it. */
  page_context?: AssistantPageContext | null;
}

/** A decision's answer: the proposal now, and where a resumed turn streams. */
export interface AssistantProposalAnswer {
  proposal: AssistantProposalRef;
  turn_id: string;
  stream_url?: string;
  after_seq?: number;
}

/** A proposal by id and state. */
interface AssistantProposalRef {
  id: string;
  state: AssistantProposalState;
}

/** The page's claim on, then its outcome of, a page action. */
export interface AssistantActionReport {
  protocol_version: number;
  state: AssistantActionState;
  revision: string;
  reason?: string;
  receipt?: string;
  /** The screen as it is after the action, on the report that settles it. */
  page_context?: AssistantPageContext | null;
}

/** A page action's state after a report. */
export interface AssistantActionAnswer {
  action_id: string;
  state: AssistantActionState;
}

/** A stopped turn's state. */
export interface AssistantStopAnswer {
  state: AssistantTurnState;
}

/** A conversation's purge: the state log's outcome and position. */
export interface AssistantPurgeAnswer {
  outcome: string;
  position: string | null;
}

/** An administrator's compliance read, with the reason they give. */
export interface AssistantAuditReadRequest {
  protocol_version: number;
  reason: string;
}

/** The person's conversations, newest first. */
export interface AssistantConversationList {
  conversations: AssistantConversationSummary[];
  next_cursor?: string;
}

/** A conversation just created. */
export interface AssistantConversationCreated {
  conversation: AssistantConversationSummary;
}

/** One conversation in a list. */
interface AssistantConversationSummary {
  id: string;
  title: string;
  created_at: string;
  updated_at: string;
  /** When it was last used: the moment the list is ordered by, and the one to show. */
  last_activity_at: string;
  last_turn_state?: AssistantTurnState;
}

/** A conversation as its owner reads it after a reload. */
export interface AssistantTranscript {
  conversation: AssistantConversationSummary;
  messages: AssistantMessage[];
  steps: AssistantStepEvent[];
  cards: AssistantCardEvent[];
  open_turn: AssistantOpenTurn | null;
  audit_reads: AssistantAuditRead[];
  /** How each turn stands, oldest first: what a reload draws its outcome line from. */
  turns: AssistantTurnOutcome[];
}

/** How one turn of a transcript stands. */
interface AssistantTurnOutcome {
  turn_id: string;
  state: AssistantDoneEvent["state"];
  /** The turn-level failure it ended with, in its code's sentence. */
  error?: AssistantErrorEvent;
}

/** One message in a transcript. */
interface AssistantMessage {
  id: string;
  turn_id: string;
  role: AssistantMessageRole;
  text: string;
  created_at: string;
  page?: AssistantPageChip;
  // --- deep work (im-deep) ---
  /** The files a user message was sent with. */
  attachments?: AssistantFileRef[];
}

/** A turn still running or parked, and where to read it. */
interface AssistantOpenTurn {
  turn_id: string;
  state: AssistantTurnState;
  stream_url: string;
  after_seq: number;
}

/** One administrator's compliance read of a conversation. */
interface AssistantAuditRead {
  reader: string;
  reason: string;
  at: string;
}

/** The `assistant` member of the viewer answer. */
export interface AssistantViewerAssistant {
  available: boolean;
  reason?: AssistantViewerReason;
  /** What a `model_not_ready` reason means for this person; absent for every other reason. */
  model?: AssistantModelNotice;
}

/**
 * The model problem a person is told before they ask: the error code a turn
 * would fail with, and the same sentence and hint.
 */
export interface AssistantModelNotice {
  code: AssistantErrorCode;
  message: string;
  hint?: string;
  /** When a spent plan is tried again, where its reset is known. */
  retry_at?: string;
}

// --- page actions (WP12) ---

/**
 * Every page action a screen may declare, by id, and whether it asks the
 * person's approval first (D11): `none` for a pure view change, applied at once
 * with an Undo, and `approve` for a change into a draft the person then saves.
 *
 * THE DASHBOARD'S HALF OF ONE LIST: the engine accepts a proposal only for an
 * id `catalog.PageActions` holds, and the page context offers only the ids
 * declared here, so `internal/assistant/catalog`'s client gate holds the two
 * equal in both directions, id and confirm alike.
 */
export const ASSISTANT_PAGE_ACTIONS = {
  "home.range.set": { confirm: "none" },
  "home.feed.set": { confirm: "none" },
  "work.filter.apply": { confirm: "none" },
  "work.filters.clear": { confirm: "none" },
  "work.arrange.set": { confirm: "none" },
  "project.lens.set": { confirm: "none" },
  "item.activity.set": { confirm: "none" },
  "spend.range.set": { confirm: "none" },
  "spend.group.set": { confirm: "none" },
  "live.filter.apply": { confirm: "none" },
  "turns.filter.apply": { confirm: "none" },
  "knowledge.search.set": { confirm: "none" },
  "draft.set": { confirm: "approve" },
  "org.draft.apply_ops": { confirm: "approve" },
  "budgets.ceiling.prefill": { confirm: "approve" },
} as const satisfies Record<string, { confirm: AssistantActionConfirm }>;

// --- deep work (im-deep) ---

/** What a person may attach: images, sniffed from their bytes by the engine. */
export type AssistantAttachmentMediaType = "image/png" | "image/jpeg" | "image/webp" | "image/gif";

/** Where a deep run stands. */
type AssistantDeepState = "starting" | "running" | "waiting" | "finished" | "stopped" | "failed";

/** A deep run as its step shows it. */
export interface AssistantDeepProgress {
  state: AssistantDeepState;
  goal: string;
  started_at: string;
  deadline_at: string;
  steps: number;
  max_steps: number;
  /** The share of the run's token cap it has used, 0..100. */
  budget_percent: number;
  activity?: string[];
  reason?: string;
}

/** One stored file as a message or a card names it. */
export interface AssistantFileRef {
  file_id: string;
  name: string;
  media_type: string;
  size: number;
  /** The owner's route for its bytes, served as an attachment. */
  download: string;
}

/** The files a deep run delivered. */
export interface AssistantFileCard {
  files: AssistantFileRef[];
}

/**
 * A brief: what the page is about, and what the person has to do, ranked
 * (each row a record the engine read and resolved for them, the first the
 * one to do first), then questions to ask next. Whether anything needs them
 * is the number of rows, never a field. Its actions only open an address the
 * engine built or ask a question.
 */
export interface AssistantBriefCard {
  scope: AssistantBriefScope;
  about: string;
  needs: AssistantBriefNeed[];
  context?: string;
  follow_ups: string[];
  as_of: string;
}

/** What a brief covers: the page, or the one record it has open. */
interface AssistantBriefScope {
  screen: string;
  title: string;
  record?: AssistantResourceRef;
}

/** One thing the person has to do, citing its record: what to do, why now, and its press. */
interface AssistantBriefNeed {
  ref: AssistantResourceRef;
  text: string;
  detail?: string;
  tone: AssistantBriefTone;
  action: AssistantBriefAction;
}

/** The one press a brief row offers: an engine-built link to open, or a question to ask. */
export interface AssistantBriefAction {
  kind: AssistantBriefActionKind;
  label: string;
  link?: AssistantLink;
  question?: string;
}

/** An attachment's upload answer: `POST /assistant/conversations/{id}/files`. */
export interface AssistantFileStored {
  file: AssistantFileRef;
}
