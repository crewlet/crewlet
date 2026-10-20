/**
 * What a gesture that STARTS something answered, rendered honestly.
 *
 * # Three outcomes, and only one of them is "there"
 *
 * Every one of these is a tool called through the act transport
 * (`protocol/act.ts`, ADR-0024), and the engine answers three different facts
 * about it: `applied` — the record landed and this node holds it; `pending` —
 * it is durable on the log and this node has not applied it; `unknown` —
 * nobody can say whether it landed. A refusal is a fourth, because a refusal
 * is an answer a person can act on rather than a failure.
 *
 * THE RULE THIS MODULE EXISTS FOR: **an unknown outcome never carries a room
 * to go to.** A create that came back unknown may have landed and may not, so
 * navigating to an id would put a person in a room that might not exist — and
 * the room screen would tell them it is not found, which reads as "your room
 * was destroyed" rather than "nobody knows yet".
 *
 * A PENDING ONE DOES NOT NAVIGATE EITHER, and that is a different reason. The
 * record IS durable — the room exists, and every data node will have it — but
 * this node has not applied it, and this node is the one that would serve the
 * read. So the honest move is to say that it is on the log and let the rail
 * bring it in: the write raised this tab's read floor for the chat log
 * (`protocol/session.ts`), and the rail is asked again at it.
 *
 * # Why the arithmetic is here rather than in each dialog
 *
 * Five surfaces make one of these gestures — create a room, open a direct
 * conversation, join, leave, change a topic — and each of them would
 * otherwise decide for itself what `pending` looks like. The one that got it
 * wrong would be the one nobody opened that week. It is also the only shape in
 * which the rule can be tested without a browser: these are pure functions
 * over an answer.
 */

import type { ActResult } from "~/protocol/act.ts";
import type { ChatReceipt } from "~/protocol/index.ts";

/**
 * What a text field past the engine's cap tells the person to do: the remedy
 * `textBudget` closes its line with, for a message's body and for the two
 * dialogs that hold a room's topic alike, so one refusal is said one way.
 * Nothing is cut to fit.
 */
export const SHORTEN = "Nothing is cut to fit. Shorten it.";

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
 * A PRESS AGAIN IS A NEW OPERATION, and that is safe for every one of these
 * for a reason that is a property of the gesture itself rather than of its
 * key: a message arbitrates nothing and needs its own operation key to be recognised
 * as a retry (the outbox keeps one per row), while these arbitrate on
 * something the engine can see. It is worth saying out loud, because "may have landed"
 * plus "do not repeat it" is a dead end for a person holding a dialog.
 */
function retryAdvice(kind: StartKind): string {
  switch (kind) {
    case "channel":
      // THE NAME IS THE ARBITRATION. If the first record landed, a second
      // create under the same name is refused as taken — which is the answer
      // "it is there" arriving as a refusal.
      return "Ask again with the same name. If the first try worked, you’ll be told the name is taken, which means the room is there.";
    case "direct":
      // THE ID IS DERIVED FROM THE PARTICIPANTS, so a second attempt reaches
      // the same room and reports that it did not have to create it.
      return "Ask again with the same people. It opens the same conversation, never a second one.";
    case "join":
    case "leave":
      // MEMBERSHIP IS A SET. A repeat that agrees with what is already there
      // publishes nothing and reports that it applied.
      return "Ask again. If the first try worked, asking again changes nothing.";
    case "topic":
      return "Ask again. If the first try worked, asking again changes nothing.";
  }
}

/** What the gesture is called in a sentence. */
function what(kind: StartKind): string {
  switch (kind) {
    case "channel":
      return "the room was created";
    case "direct":
      return "the conversation was opened";
    case "join":
      return "you joined";
    case "leave":
      return "you left";
    case "topic":
      return "the change was saved";
  }
}

/** Whether this gesture lands somebody in a room when it applies. */
function navigates(kind: StartKind): boolean {
  return kind === "channel" || kind === "direct" || kind === "join";
}

/** The room an answer names, or "" for a receipt that names none. */
function roomOf(receipt: unknown): string {
  const room = (receipt as Partial<ChatReceipt> | null)?.channel;
  return typeof room === "string" ? room : "";
}

/**
 * One answer, read for the screen that caused it.
 *
 * THE REFUSAL'S OWN TEXT SURVIVES, and that is deliberate: the act transport
 * carries the tool's sentence where it names the field and the rule that
 * refused it — "this company holds 1000 live channels and the maximum is
 * 1000", "a private room's membership is the only way into it" — and that
 * sentence is the whole of its value. A screen that replaced it with a
 * category would throw away the only part a person can act on.
 */
export function reportFor(kind: StartKind, result: ActResult): StartReport {
  switch (result.kind) {
    case "applied":
      return {
        // A DIRECT CONVERSATION THAT WAS ALREADY OPEN IS NOT AN ERROR AND NOT
        // A SECOND ROOM. Its id is derived from the participants, so the loser
        // of the race is handed the room the winner made, and the receipt's
        // `created` says which happened — which is why THIS BRANCH DOES NOT
        // READ IT: both are the same instruction to the screen, go to that
        // room, and a sentence about which one it was would be a sentence
        // nobody is left on the dialog to read.
        go: navigates(kind) ? roomOf(result.receipt) : "",
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
        note: "Saved, but not shown here yet. Your rooms will update in a moment.",
        tone: "pending",
        taken: false,
        again: false,
        settled: true,
      };
    case "unknown":
      return {
        // NEVER A ROOM. See this module's own note: an id an answer
        // established nothing about is an id that may name nothing.
        go: "",
        note: `Couldn’t confirm ${what(kind)}. ${result.reason} ${retryAdvice(kind)}`,
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
function refusal(kind: StartKind, result: Extract<ActResult, { kind: "refused" }>): StartReport {
  const said = [result.sentence, result.hint].filter((part) => part.trim() !== "").join(" ");
  if (kind === "channel" && result.code === "exists") {
    return {
      go: "",
      // A NAME TO NEGOTIATE. The create arbitrates on the address, so this is
      // the arbitration working: somebody holds it. Drawn as a refusal in red
      // it reads as a fault, and the field a person has to change is the one
      // they are already looking at. The sentence is this module's rather than
      // the transport's "That already exists", which names no field.
      note: "Another room already has that name. Pick another.",
      tone: "warning",
      taken: true,
      again: false,
      settled: false,
    };
  }
  return {
    go: "",
    note: result.code === "not_found" ? NOT_FOUND : said,
    tone: "danger",
    taken: false,
    again: false,
    settled: false,
  };
}

/**
 * What a room that is not there means here.
 *
 * A ROOM THE VIEWER MAY NOT READ ANSWERS EXACTLY AS ONE THAT DOES NOT EXIST,
 * which is the read side's rule carried into every write: a private room's
 * EXISTENCE is information. The transport's own sentence for the class ("It
 * is not there any more") would tell a person the room was removed, which is
 * one of the two things it may mean and the one the engine is careful not to
 * say.
 */
const NOT_FOUND = "There’s no such room, or you can’t read it.";
