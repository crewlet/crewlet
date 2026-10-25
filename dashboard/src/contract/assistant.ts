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

/** The protocol version every frame and every request carries. */
export const ASSISTANT_PROTOCOL_VERSION = 1;
/** The longest question a turn takes, in characters (Unicode code points). */
export const ASSISTANT_INPUT_MAX_RUNES = 4000;
/** The most a turn's page context may encode to, in bytes. */
export const ASSISTANT_PAGE_CONTEXT_MAX_BYTES = 8192;
/** The most resources a page context names. */
export const ASSISTANT_PAGE_CONTEXT_MAX_RESOURCES = 50;
/** The most page actions a page context offers. */
export const ASSISTANT_PAGE_CONTEXT_MAX_ACCEPTS = 32;
/** The longest reason an administrator gives for a compliance read. */
export const ASSISTANT_AUDIT_REASON_MAX_RUNES = 500;
/** How often a quiet stream writes a keepalive comment, in seconds. */
export const ASSISTANT_KEEPALIVE_SECONDS = 15;
/** The reserved action the server sends after a write lands. */
export const ASSISTANT_ACTION_CONTEXT_INVALIDATE = "context.invalidate";
/** What opens a proposal's digest on the wire. */
export const ASSISTANT_DIGEST_PREFIX = "sha256:";

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
  | "budget_exhausted";

/** The run chip's state while a segment runs. */
type AssistantStatusState = "thinking" | "working" | "waiting_for_approval" | "applying";

/** One tool step's state. */
type AssistantStepState = "running" | "done" | "failed" | "refused" | "stopped" | "waiting";

/** An engine-built card's kind. */
export type AssistantCardKind =
  "table" | "entity" | "proposal" | "open_in_app" | "error" | "notice";

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

/** One row of a proposal's change list, in the before and after grammar. */
type AssistantChangeKind = "fact" | "changed" | "added" | "removed" | "text";

/** What happened to one run of words in a text change. */
type AssistantWordOp = "same" | "added" | "removed";

/** How a table card's column is drawn and exported. */
type AssistantColumnType = "text" | "number" | "tokens" | "duration" | "datetime";

/** A table column's alignment. */
type AssistantAlign = "start" | "end";

/** A notice card's tone. */
type AssistantNoticeTone = "info" | "warning";

/** A turn-level failure on an error frame. */
type AssistantErrorCode =
  | "model_unavailable"
  | "turn_timeout"
  | "step_limit"
  | "budget_exhausted"
  | "turn_interrupted"
  | "grant_ended"
  | "internal_error";

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
  | "view";

/** Why the assistant is not available to the viewer. */
export type AssistantViewerReason = "disabled" | "no_model" | "not_a_person" | "no_permission";

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
  index: number;
  tool: string;
  label: string;
  request?: AssistantStepRequest;
  state: AssistantStepState;
  started_at: string;
  duration_ms?: number;
  summary?: string;
  refusal?: AssistantRefusal;
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
  kind: AssistantResourceKind;
  id: string;
  title: string;
  subtitle?: string;
  facts?: AssistantFact[];
  text?: string;
  items?: AssistantEntityItem[];
  open?: AssistantLink;
}

/** One labelled value on an entity card. */
interface AssistantFact {
  label: string;
  value: string;
  mono?: boolean;
}

/** One row of an entity card's list. */
interface AssistantEntityItem {
  kind: AssistantResourceKind;
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
  note?: string;
  audience?: string[];
  taint?: string;
  step_up?: boolean;
  digest: string;
  expires_at: string;
  state: AssistantProposalState;
  source_revision?: string;
  approve_label: string;
  receipt?: AssistantReceipt;
  failure?: AssistantFailure;
}

/** One row of a proposal's change list. */
interface AssistantChange {
  kind: AssistantChangeKind;
  field: string;
  label: string;
  from?: string;
  to?: string;
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

/** What a screen tells a turn about itself, by id and never by content. */
export interface AssistantPageContext {
  screen: string;
  title: string;
  params: Record<string, string>;
  query: Record<string, string>;
  resources: AssistantResource[];
  selection: AssistantResourceRef | null;
  source_revision: string;
  accepts: string[];
}

/** One thing in view, by id. */
interface AssistantResource {
  kind: AssistantResourceKind;
  id: string;
  version?: string;
}

/** One thing, named. */
interface AssistantResourceRef {
  kind: AssistantResourceKind;
  id: string;
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
}

/** One message in a transcript. */
interface AssistantMessage {
  id: string;
  turn_id: string;
  role: AssistantMessageRole;
  text: string;
  created_at: string;
  page?: AssistantPageChip;
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
}
