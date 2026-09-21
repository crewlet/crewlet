/**
 * What a gesture that STARTS something answered, rendered honestly.
 *
 * # Three outcomes, and only one of them is "there"
 *
 * Every one of these is a record on the fleet's log, so the engine answers
 * three different facts about it and the transport carries them as three
 * statuses: 200 the record applied HERE, 202 it is durable at its position and
 * this node has not applied it, 504 the broker did not say. `writes.ts`
 * classifies a refusal as a fourth, because a refusal is an answer a person
 * can act on rather than a failure.
 *
 * THE RULE THIS MODULE EXISTS FOR: **an unknown outcome never carries a room
 * to go to.** A create that came back unknown may have landed and may not, so
 * navigating to the id the answer echoed would put a person in a room that
 * might not exist — and the room screen would tell them it is not found, which
 * reads as "your room was destroyed" rather than "nobody knows yet".
 *
 * A PENDING ONE DOES NOT NAVIGATE EITHER, and that is a different reason. The
 * record IS durable — the room exists, and every node will have it — but this
 * node has not applied it, and this node is the one that would serve the read.
 * So the honest move is to say that it is on the log and let the rail bring it
 * in when the applier catches up, which happens on its own: the applier's
 * commit is what the live frame is derived from, and a frame is what the rail
 * re-reads on.
 *
 * # Why the arithmetic is here rather than in each dialog
 *
 * Four surfaces make one of these gestures — create a room, open a direct
 * conversation, join, leave — and each of them would otherwise decide for
 * itself what `pending` looks like. The one that got it wrong would be the one
 * nobody opened that week. It is also the only shape in which the rule can be
 * tested without a browser: these are pure functions over an answer.
 */

import type { ChatDirectAnswer } from "~/protocol/index.ts";
import type { WriteResult } from "./writes.ts";

/** Which gesture answered, because the retry advice differs for each. */
export type StartKind = "channel" | "direct" | "join" | "leave" | "topic";

/**
 * How a report is drawn.
 *
 * THE THREE OUTCOMES' OWN VOCABULARY rather than the design system's, because
 * `pending` is a fact about a record and not a colour: it is durable, it is not
 * here, and nothing is wrong. [calloutFor] is where it becomes a variant.
 */
export type StartTone = "success" | "pending" | "warning" | "danger";

/**
 * Which callout draws a report.
 *
 * PENDING IS INFORMATION, NEVER SUCCESS. A green tick beside "this node has
 * not applied it yet" is the browser half of the lie the durable-versus-applied
 * split exists to prevent — and a warning would be the opposite lie, since
 * nothing about a pending record needs anybody's attention.
 */
export function calloutFor(tone: StartTone): "info" | "success" | "warning" | "danger" {
  return tone === "pending" ? "info" : tone;
}

/** What one answer means for the screen that made the gesture. */
export interface StartReport {
  /** The room to go to, or "" for a gesture that navigates nowhere — which
   *  includes EVERY unknown outcome, whatever id the answer echoed. */
  go: string;
  /** The sentence for the person. The engine's own where it wrote one for a
   *  reader; this module's where the outcome is not a refusal at all. */
  note: string;
  tone: StartTone;
  /**
   * The name is held by another room.
   *
   * ITS OWN FIELD rather than a tone, because it is not a failure: a create
   * arbitrates on the name, so somebody else holding `#launch` is a name to
   * negotiate. The dialog stays open on the name field with what was typed
   * still in it.
   */
  taken: boolean;
  /** Whether pressing the same button again is the right move. True for an
   *  unknown outcome and for nothing else. */
  again: boolean;
  /** Whether the gesture is finished with — the dialog may close itself. */
  settled: boolean;
}

/**
 * How to ask again after an outcome nobody can establish.
 *
 * THE OPERATION ID IS THE ENGINE'S ON EVERY ONE OF THESE. A message carries
 * the caller's own idempotency key because a post arbitrates nothing at the
 * broker; these gestures arbitrate, so the write path mints the id and there
 * is no value for a client to retry under. What makes each of them safe to
 * repeat is a property of the gesture itself, and it is worth saying out loud,
 * because "may have landed" plus "do not repeat it" is a dead end for a person
 * holding a dialog.
 */
function retryAdvice(kind: StartKind): string {
  switch (kind) {
    case "channel":
      // THE NAME IS THE ARBITRATION. If the first record landed, a second
      // create under the same name is refused as taken — which is the answer
      // "it is there" arriving as a refusal.
      return "Ask again with the same name: the create arbitrates on it, so if the first one landed you will be told the name is taken — which means the room is there.";
    case "direct":
      // THE ID IS DERIVED FROM THE PARTICIPANTS, so a second attempt reaches
      // the same room and reports that it did not have to create it.
      return "Ask again with the same people: a direct conversation's id is derived from who is in it, so a second attempt opens that same room rather than a second one.";
    case "join":
    case "leave":
      // MEMBERSHIP IS A SET. A repeat that agrees with what is already there
      // publishes nothing and reports that it applied.
      return "Ask again: membership is a set, so a repeat that agrees with what is already there writes nothing at all.";
    case "topic":
      return "Ask again: a patch that changes nothing publishes nothing and reports that it applied.";
  }
}

/** What the gesture is called in a sentence. */
function what(kind: StartKind): string {
  switch (kind) {
    case "channel":
      return "The room";
    case "direct":
      return "The conversation";
    case "join":
      return "Joining";
    case "leave":
      return "Leaving";
    case "topic":
      return "The change";
  }
}

/** Whether this gesture lands somebody in a room when it applies. */
function navigates(kind: StartKind): boolean {
  return kind === "channel" || kind === "direct" || kind === "join";
}

/**
 * One answer, read for the screen that caused it.
 *
 * THE REFUSAL'S OWN TEXT SURVIVES, and that is deliberate: the write path's
 * refusals name the field and the rule that refused it — "this company holds
 * 1000 live channels and the maximum is 1000", "a private room's membership is
 * the only way into it" — and that sentence is the whole of their value. A
 * screen that replaced it with a category would throw away the only part a
 * person can act on.
 */
export function reportFor(kind: StartKind, result: WriteResult<ChatDirectAnswer>): StartReport {
  const room = result.answer?.channel?.id ?? "";
  switch (result.outcome) {
    case "applied":
      return {
        // A DIRECT CONVERSATION THAT WAS ALREADY OPEN IS NOT AN ERROR AND NOT
        // A SECOND ROOM. Its id is derived from the participants, so the loser
        // of the race is handed the room the winner made, and the answer's
        // `created` says which happened — which is why THIS BRANCH DOES NOT
        // READ IT: both are the same instruction to the screen, go to that
        // room, and a sentence about which one it was would be a sentence
        // nobody is left on the dialog to read.
        go: navigates(kind) ? room : "",
        note: "",
        tone: "success",
        taken: false,
        again: false,
        settled: true,
      };
    case "pending":
      return {
        // DURABLE, AND NOT HERE. The room exists on the log at the position
        // the answer carries; this node has not applied it, and this node is
        // the one that would serve the read. Sending somebody to it now is a
        // "not found" for a room that is perfectly real.
        go: "",
        note: `${what(kind)} is on the log and this node has not applied it yet. Nothing is wrong and nothing needs doing: every node applies the same record, and your rooms re-read themselves the moment this one does.`,
        tone: "pending",
        taken: false,
        again: false,
        settled: true,
      };
    case "unknown":
      return {
        // NEVER A ROOM. See this module's own note: an id echoed back by an
        // answer that established nothing is an id that may name nothing.
        go: "",
        note: `The broker did not answer, so nothing can be established from this node: ${what(kind).toLowerCase()} may have landed and may not. ${retryAdvice(kind)}`,
        tone: "warning",
        taken: false,
        again: true,
        settled: false,
      };
    case "refused":
      return refusal(kind, result);
  }
}

/** A refusal, classified by the code the engine put beside its sentence. */
function refusal(kind: StartKind, result: WriteResult<ChatDirectAnswer>): StartReport {
  const detail = result.detail.trim();
  if (result.code === "name_taken") {
    return {
      go: "",
      // A NAME TO NEGOTIATE. The create arbitrates on the address, so this is
      // the arbitration working: somebody holds it. Drawn as a refusal in red
      // it reads as a fault, and the field a person has to change is the one
      // they are already looking at.
      note:
        detail ||
        "That name is already a room. A name is the address a create claims, and exactly one room may hold it — pick another.",
      tone: "warning",
      taken: true,
      again: false,
      settled: false,
    };
  }
  return {
    go: "",
    note: detail || fallback(kind, result.code),
    tone: "danger",
    taken: false,
    again: false,
    settled: false,
  };
}

/**
 * What to say when the engine refused without a sentence.
 *
 * THE UNCLASSIFIED FAILURES ARE THE ONES WITH NO TEXT, by design: the write
 * path's own refusals are written for a person and travel whole, while a
 * failure that could carry a database path or a driver's message reaches the
 * node's log instead and arrives here as a bare code. So this is never a
 * paraphrase of a sentence the engine wrote — it is what fills the silence.
 */
function fallback(kind: StartKind, code: string): string {
  switch (code) {
    case "forbidden":
      return `${what(kind)} was refused: this room's own rule does not admit it.`;
    case "not_found":
      // A ROOM THE VIEWER MAY NOT READ ANSWERS EXACTLY AS ONE THAT DOES NOT
      // EXIST, which is the read side's rule carried into every write: a
      // private room's EXISTENCE is information.
      return "There is no such room, or it is not one you can read — the two answer identically, because a private room's existence is itself something to know.";
    case "archived":
      return "That room is archived. It stays readable for ever and takes no new messages.";
    case "conflict":
      return "The room moved while this was being written. Read it again and repeat the change.";
    case "invalid":
      return `${what(kind)} was refused as malformed.`;
    case "unreachable":
      return "The engine could not be reached, so nothing was said to it.";
    default:
      return `${what(kind)} could not be written${code ? ` (${code})` : ""}.`;
  }
}
