/**
 * Everything this screen writes, in one place.
 *
 * Chat is the first screen in this dashboard whose ordinary use is WRITING,
 * and three rules travel with every one of these:
 *
 *   - **A caller never names a seat.** There is no author field on any of
 *     these bodies and no query parameter for one. The server resolves the
 *     credential to a `kind: human` seat on every call, so a message is
 *     attributed to the person rather than to the token — and a token bound to
 *     no seat is refused, reads included.
 *   - **Every message carries its own idempotency key.** A post arbitrates
 *     nothing at the broker (a message is additive on its channel's subject),
 *     so an operation id is the only thing that can tell a resubmission from a
 *     second remark. Retrying under the SAME one is the only safe retry: a
 *     fresh id derives a second message id and says it twice.
 *   - **The outcome is three-valued, and the transport carries it as three
 *     statuses.** 200 the record applied here, 202 it is published and this
 *     node has not applied it yet, 504 the broker did not say. Only the first
 *     is "sent".
 */

import { rest, RestError } from "~/protocol/index.ts";
import type { ChatReadFlush, ChatReadState, ChatWriteAnswer } from "~/protocol/index.ts";

/** Whether a write landed, is on its way, or is genuinely unknown. */
export type WriteOutcome = "applied" | "pending" | "unknown" | "refused";

/** What one write answered, classified for a screen. */
export interface WriteResult {
  outcome: WriteOutcome;
  answer: ChatWriteAnswer | null;
  /** The engine's own sentence, on the refusals written for a person to read:
   *  a name somebody else holds, an archived room, a body over the cap. */
  detail: string;
  /** The machine-readable code beside it — `invalid`, `forbidden`,
   *  `name_taken`, `archived`, `conflict`, `not_found`. */
  code: string;
}

/**
 * One chat write, with its outcome classified rather than thrown.
 *
 * A REFUSAL IS AN ANSWER HERE. The screen has something to do with each of
 * these — a room to reopen, a permission to ask for, a message to shorten —
 * and a transport that threw them all as one error would leave every one of
 * them rendered as "something went wrong".
 *
 * THE UNKNOWN CASE IS THE REASON THIS EXISTS. A 504 carries a body: the
 * operation id the caller must retry under, and the outcome word itself. The
 * generic transport reads that status as a failure, which is right, and the
 * screen has to be able to tell it from a refusal — because one of them is
 * "this may well have been said" and the other is "it was not".
 */
async function write(path: string, body?: unknown): Promise<WriteResult> {
  try {
    const answer = (await rest.post(path, body)) as ChatWriteAnswer;
    return {
      outcome: answer?.outcome === "pending" ? "pending" : "applied",
      answer: answer ?? null,
      detail: "",
      code: "",
    };
  } catch (err) {
    if (err instanceof RestError) {
      const answer = err.body as unknown as ChatWriteAnswer;
      if (answer?.outcome === "unknown") {
        return { outcome: "unknown", answer, detail: "", code: "" };
      }
      return {
        outcome: "refused",
        answer: null,
        detail: err.detail || err.message,
        code: err.code,
      };
    }
    return {
      outcome: "refused",
      answer: null,
      detail: err instanceof Error ? err.message : String(err),
      code: "",
    };
  }
}

const room = (channelID: string) => `/chat/channels/${encodeURIComponent(channelID)}`;
const message = (channelID: string, messageID: string) =>
  `${room(channelID)}/messages/${encodeURIComponent(messageID)}`;

/** What somebody said. */
export interface NewMessage {
  body: string;
  links?: string[];
  /** The handles the body named, ALREADY RESOLVED by the composer — the
   *  engine does not re-read the prose, here or for a seat's own tool. */
  mentions?: string[];
  /** `@channel`: the room as a whole. It wakes at most 32 agents and the
   *  engine says when it truncated. */
  collective?: boolean;
  operation_id: string;
}

export function postMessage(channelID: string, body: NewMessage): Promise<WriteResult> {
  return write(`${room(channelID)}/messages`, body);
}

/**
 * A reply in a thread.
 *
 * THE ROOT IS THE MESSAGE BEING ANSWERED, and the write path resolves the
 * thread from it: a thread is one level deep, so answering a reply carries the
 * same root. There is deliberately nowhere to send a root of one's own — a
 * route that took one would let a caller file an answer under a thread it does
 * not belong to.
 */
export function replyToMessage(
  channelID: string,
  messageID: string,
  body: NewMessage,
): Promise<WriteResult> {
  return write(`${message(channelID, messageID)}/replies`, body);
}

/** Rewrite one's own message. The engine re-derives nothing: the mentions
 *  travel again because the body they came from did. */
export function editMessage(
  channelID: string,
  messageID: string,
  body: { body: string; links?: string[]; mentions?: string[] },
): Promise<WriteResult> {
  return write(`${message(channelID, messageID)}/edit`, body);
}

/** Take one's own words back. The row survives as a tombstone, so a thread
 *  does not lose the message it hangs off. */
export function deleteMessage(channelID: string, messageID: string): Promise<WriteResult> {
  return write(`${message(channelID, messageID)}/delete`);
}

/** An emoji on a message. It wakes nobody and answers nothing — a seat cannot
 *  discharge an obligation with a thumb — and it takes the room's own
 *  membership gate like every other write. */
export function react(
  channelID: string,
  messageID: string,
  emoji: string,
  remove = false,
): Promise<WriteResult> {
  return write(`${message(channelID, messageID)}/${remove ? "unreact" : "react"}`, { emoji });
}

/**
 * Write this person's read state.
 *
 * NOT A LOG RECORD, which is why it answers a state rather than an outcome: a
 * cursor lives in coordination, nobody replays it, and the write is a
 * compare-and-set the engine performs on the caller's behalf. It goes through
 * a node because the coordination store is the engine's own embedded broker on
 * the default topology and binds no socket — the same reason the retention
 * acknowledgement and the budget reset do.
 */
export async function flushRead(delta: ChatReadFlush): Promise<ChatReadState> {
  return (await rest.post("/chat/read", delta)) as ChatReadState;
}

/**
 * A fresh idempotency key.
 *
 * `crypto.randomUUID` is SECURE-CONTEXT ONLY and this dashboard is routinely
 * served over plain http on a LAN — the engine writes no certificate of its
 * own — so reaching for it would leave every composer on such a deployment
 * throwing on the first keystroke of the first message. `getRandomValues` is
 * available on both, and a v4 shape is what the engine's own id derivation
 * expects to be handed.
 */
export function operationID(): string {
  const bytes = new Uint8Array(16);
  crypto.getRandomValues(bytes);
  // Version 4, variant 1 — the two fields a v4 uuid pins.
  bytes[6] = (bytes[6]! & 0x0f) | 0x40;
  bytes[8] = (bytes[8]! & 0x3f) | 0x80;
  const hex = [...bytes].map((b) => b.toString(16).padStart(2, "0")).join("");
  return [
    hex.slice(0, 8),
    hex.slice(8, 12),
    hex.slice(12, 16),
    hex.slice(16, 20),
    hex.slice(20),
  ].join("-");
}
