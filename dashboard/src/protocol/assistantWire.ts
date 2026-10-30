/**
 * The Dashboard Assistant's stream as the client reads it: an incremental
 * parser over the bytes a turn's SSE resource sends, the narrowing of a frame
 * to the one payload its kind names, and the bounds a question and a page
 * context are held to before they are sent.
 *
 * THE ENCODER IS THE ENGINE'S (`internal/assistant/wire`). This module never
 * writes a frame; its suite replays the golden stream the engine's encoder
 * writes (`internal/assistant/wire/testdata/frames.sse`), so a change to either
 * side fails until the other follows it.
 *
 * WITHIN ONE PROTOCOL VERSION EVOLUTION IS ADDITIVE: a frame of a kind this
 * build does not know is skipped, never an error, while a frame of another
 * version stops the stream with `protocol_mismatch`, which the drawer answers
 * by asking the person to reload.
 */

import {
  ASSISTANT_ACTION_CONTEXT_INVALIDATE,
  ASSISTANT_AUDIT_REASON_MAX_RUNES,
  ASSISTANT_DIGEST_PREFIX,
  ASSISTANT_INPUT_MAX_RUNES,
  ASSISTANT_KEEPALIVE_SECONDS,
  ASSISTANT_PAGE_CONTEXT_MAX_ACCEPTS,
  ASSISTANT_PAGE_CONTEXT_MAX_BYTES,
  ASSISTANT_PAGE_CONTEXT_MAX_RESOURCES,
  ASSISTANT_PROTOCOL_VERSION,
} from "../contract/assistant.ts";
import type {
  AssistantActionAnswer,
  AssistantActionEvent,
  AssistantActionReport,
  AssistantAuditReadRequest,
  AssistantCard,
  AssistantCardEvent,
  AssistantCardKind,
  AssistantConversationCreated,
  AssistantConversationEvent,
  AssistantConversationList,
  AssistantDeltaEvent,
  AssistantDoneEvent,
  AssistantEntityCard,
  AssistantErrorCard,
  AssistantErrorEvent,
  AssistantEventKind,
  AssistantFileCard,
  AssistantBriefCard,
  AssistantFileStored,
  AssistantFrame,
  AssistantNoticeCard,
  AssistantOversizeCard,
  AssistantOpenInAppCard,
  AssistantPageContext,
  AssistantPageContextEvent,
  AssistantProposalAnswer,
  AssistantProposalCard,
  AssistantProposalDecision,
  AssistantPurgeAnswer,
  AssistantSnapshotEvent,
  AssistantStatusEvent,
  AssistantStepEvent,
  AssistantStopAnswer,
  AssistantTableCard,
  AssistantTranscript,
  AssistantTurnAccepted,
  AssistantTurnRequest,
  AssistantVersioned,
  AssistantModelNotice,
  AssistantViewerAssistant,
  AssistantViewerReason,
} from "../contract/assistant.ts";

/** Each event kind's payload, by the member the frame carries it under. */
export interface AssistantPayloads {
  conversation: AssistantConversationEvent;
  page_context: AssistantPageContextEvent;
  status: AssistantStatusEvent;
  delta: AssistantDeltaEvent;
  step: AssistantStepEvent;
  card: AssistantCardEvent;
  action: AssistantActionEvent;
  done: AssistantDoneEvent;
  error: AssistantErrorEvent;
  snapshot: AssistantSnapshotEvent;
}

/** A frame narrowed to the one payload its kind names. */
export type AssistantFrameOf<K extends AssistantEventKind> = Omit<
  AssistantFrame,
  AssistantEventKind
> & { kind: K } & { [P in K]: AssistantPayloads[P] };

/** Any frame, narrowed by its kind. */
export type AssistantNarrowFrame = {
  [K in AssistantEventKind]: AssistantFrameOf<K>;
}[AssistantEventKind];

/** Each card kind's body, by the member the card carries it under. */
export interface AssistantCardBodies {
  table: AssistantTableCard;
  entity: AssistantEntityCard;
  proposal: AssistantProposalCard;
  open_in_app: AssistantOpenInAppCard;
  error: AssistantErrorCard;
  notice: AssistantNoticeCard;
  oversize: AssistantOversizeCard;
  file: AssistantFileCard;
  brief: AssistantBriefCard;
}

/** A card narrowed to the one body its type names. */
export type AssistantCardOf<K extends AssistantCardKind> = Omit<
  AssistantCard,
  AssistantCardKind
> & {
  type: K;
} & { [P in K]: AssistantCardBodies[P] };

/** Any card, narrowed by its type. */
export type AssistantNarrowCard = {
  [K in AssistantCardKind]: AssistantCardOf<K>;
}[AssistantCardKind];

/**
 * Every kind this build reads, as a record so the compiler holds it to the
 * union: a kind added to the contract is a type error here until it is named.
 */
const EVENT_KINDS: Record<AssistantEventKind, true> = {
  conversation: true,
  page_context: true,
  status: true,
  delta: true,
  step: true,
  card: true,
  action: true,
  done: true,
  error: true,
  snapshot: true,
};

const CARD_KINDS: Record<AssistantCardKind, true> = {
  table: true,
  entity: true,
  proposal: true,
  open_in_app: true,
  error: true,
  notice: true,
  oversize: true,
  file: true,
  brief: true,
};

/**
 * A card narrowed by its type, or null for a type this build does not draw or
 * a card missing the body its type names; the drawer draws null as a notice.
 */
export function narrowCard(card: AssistantCard): AssistantNarrowCard | null {
  if (!Object.hasOwn(CARD_KINDS, card.type) || card[card.type] === undefined) return null;
  return card as AssistantNarrowCard;
}

/**
 * Why the Ask pill is drawn disabled, or null when the assistant is
 * available. A viewer answer with no `assistant` member is an engine that
 * does not serve it, which reads as disabled rather than as available.
 */
export function assistantUnavailable(
  viewer: AssistantViewerAssistant | undefined,
): AssistantAccessReason | null {
  if (viewer === undefined) return "disabled";
  if (viewer.available || viewer.reason === "model_not_ready") return null;
  return viewer.reason ?? "disabled";
}

/**
 * A reason the reader may not use the assistant at all, which draws the Ask
 * pill disabled: every viewer reason but the model's state, which is the
 * drawer's notice ([assistantModelNotice]).
 */
export type AssistantAccessReason = Exclude<AssistantViewerReason, "model_not_ready">;

/**
 * What the reader is told of the assistant's model before they ask, or null
 * when nothing is known to be wrong with it. NOT a reason the Ask pill is drawn
 * disabled: the drawer still opens, with the conversation readable, and says
 * this above its composer with Send off ([assistantUnavailable] lets it
 * through). A `model_not_ready` answer with no notice is an older engine's,
 * read as a model that cannot answer for no named reason.
 */
export function assistantModelNotice(
  viewer: AssistantViewerAssistant | undefined,
): AssistantModelNotice | null {
  if (viewer === undefined || viewer.available || viewer.reason !== "model_not_ready") return null;
  return (
    viewer.model ?? {
      code: "model_unavailable",
      message: "Ask Crewlet's model cannot answer right now.",
    }
  );
}

/** A frame's SSE id, `<turn_id>.<seq>`, which a resume sends as Last-Event-ID. */
export function assistantEventId(turnId: string, seq: number): string {
  return `${turnId}.${seq}`;
}

/** Why a stream was stopped rather than read on. */
export type AssistantStreamFault =
  { code: "protocol_mismatch"; version: number } | { code: "malformed"; detail: string };

/** What the parser hands its caller, one call per event. */
export interface AssistantStreamHandlers {
  frame: (frame: AssistantNarrowFrame) => void;
  fault: (fault: AssistantStreamFault) => void;
  /** The engine's keepalive comment arrived: the turn is still running there. */
  keepalive?: () => void;
}

/**
 * An incremental parser: feed it the stream's text as it arrives, in chunks
 * cut anywhere, and it hands back each complete frame once. After a fault it
 * reads nothing more, because what follows a frame it could not trust is not
 * a frame it can place.
 */
export function createAssistantStreamParser(handlers: AssistantStreamHandlers): {
  push: (chunk: string) => void;
} {
  let buffer = "";
  let stopped = false;
  const fail = (fault: AssistantStreamFault) => {
    stopped = true;
    handlers.fault(fault);
  };
  const event = (block: string) => {
    let id = "";
    let name = "";
    let data = "";
    for (const line of block.split("\n")) {
      if (line.startsWith(":")) {
        handlers.keepalive?.();
        continue;
      }
      if (line === "") continue;
      if (line.startsWith("id: ")) id = line.slice(4);
      else if (line.startsWith("event: ")) name = line.slice(7);
      else if (line.startsWith("data: ")) {
        if (data !== "")
          return fail({ code: "malformed", detail: `event ${id} has two data lines` });
        data = line.slice(6);
      } else return fail({ code: "malformed", detail: `unknown field in event ${id}` });
    }
    if (data === "") return;
    let frame: AssistantFrame;
    try {
      frame = JSON.parse(data) as AssistantFrame;
    } catch {
      return fail({ code: "malformed", detail: `event ${id} is not JSON` });
    }
    if (frame.v !== ASSISTANT_PROTOCOL_VERSION) {
      return fail({ code: "protocol_mismatch", version: frame.v });
    }
    if (!Object.hasOwn(EVENT_KINDS, frame.kind)) return;
    if (name !== frame.kind || id !== assistantEventId(frame.turn_id, frame.seq)) {
      return fail({ code: "malformed", detail: `event ${id} disagrees with its frame` });
    }
    if (frame[frame.kind] === undefined) {
      return fail({ code: "malformed", detail: `a ${frame.kind} frame carries no payload` });
    }
    handlers.frame(frame as AssistantNarrowFrame);
  };
  return {
    push(chunk: string) {
      if (stopped) return;
      buffer += chunk.replace(/\r\n/g, "\n");
      for (;;) {
        const end = buffer.indexOf("\n\n");
        if (end < 0 || stopped) return;
        const block = buffer.slice(0, end);
        buffer = buffer.slice(end + 2);
        event(block);
      }
    },
  };
}

/**
 * How long a stream may stay silent before it counts as stalled and is
 * resumed: two missed keepalives.
 */
export const ASSISTANT_STALL_MS = 2 * ASSISTANT_KEEPALIVE_SECONDS * 1000;

/** The number of characters (code points) in a string, as the engine counts runes. */
function runes(text: string): number {
  let n = 0;
  for (const _ of text) n++;
  return n;
}

/** Whether a question is within the length a turn takes. */
export function questionFits(text: string): boolean {
  return runes(text) <= ASSISTANT_INPUT_MAX_RUNES;
}

/** Whether a compliance read's reason is given and within its bound. */
export function auditReasonFits(reason: string): boolean {
  return reason.trim() !== "" && runes(reason) <= ASSISTANT_AUDIT_REASON_MAX_RUNES;
}

/**
 * Whether a page context is within every bound the engine holds it to, so a
 * screen that registered too much is trimmed by the frame rather than refused
 * by the server after the person pressed Send.
 */
export function pageContextFits(context: AssistantPageContext): boolean {
  if (context.resources.length > ASSISTANT_PAGE_CONTEXT_MAX_RESOURCES) return false;
  if (context.accepts.length > ASSISTANT_PAGE_CONTEXT_MAX_ACCEPTS) return false;
  if (context.accepts.includes(ASSISTANT_ACTION_CONTEXT_INVALIDATE)) return false;
  return (
    new TextEncoder().encode(JSON.stringify(context)).length <= ASSISTANT_PAGE_CONTEXT_MAX_BYTES
  );
}

/** Whether a string is a proposal digest in the wire's form. */
export function isProposalDigest(digest: string): boolean {
  return (
    digest.startsWith(ASSISTANT_DIGEST_PREFIX) &&
    /^[0-9a-f]{64}$/.test(digest.slice(ASSISTANT_DIGEST_PREFIX.length))
  );
}

/**
 * The assistant's routes, each with the body it takes and the answer it
 * gives: the one table the drawer's requests are typed by, so a route and its
 * shapes are named together once.
 */
export interface AssistantRoutes {
  "GET /assistant/conversations": { body: null; answer: AssistantConversationList };
  "POST /assistant/conversations": {
    body: AssistantVersioned;
    answer: AssistantConversationCreated;
  };
  "GET /assistant/conversations/{id}": { body: null; answer: AssistantTranscript };
  "DELETE /assistant/conversations/{id}": { body: null; answer: AssistantPurgeAnswer };
  "POST /assistant/conversations/{id}/turns": {
    body: AssistantTurnRequest;
    answer: AssistantTurnAccepted;
  };
  /**
   * An image the person attaches to their next question, uploaded at once:
   * the raw bytes as the body, typed by the file, under an operation key.
   */
  "POST /assistant/conversations/{id}/files": { body: Blob; answer: AssistantFileStored };
  "POST /assistant/turns/{id}/stop": { body: AssistantVersioned; answer: AssistantStopAnswer };
  "POST /assistant/proposals/{id}/approve": {
    body: AssistantProposalDecision;
    answer: AssistantProposalAnswer;
  };
  "POST /assistant/proposals/{id}/decline": {
    body: AssistantProposalDecision;
    answer: AssistantProposalAnswer;
  };
  "PUT /assistant/actions/{id}": { body: AssistantActionReport; answer: AssistantActionAnswer };
  "POST /assistant/audit/conversations/{id}/read": {
    body: AssistantAuditReadRequest;
    answer: AssistantTranscript;
  };
}
