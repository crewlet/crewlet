/**
 * Whether this reader may ask the assistant, and if not, the words that say
 * why: one module, so the Ask button's reason, the drawer's empty state and
 * a deep link to the drawer all say the same thing.
 *
 * A CONTROL IS NEVER HIDDEN in this product (`lib/useWriteAccess.ts`): the
 * Ask button is drawn for every reader, and where they cannot ask it is drawn
 * disabled with the reason after its name and in its tooltip.
 *
 * THE REASON IS THE ENGINE'S. The viewer answer carries the first condition
 * that fails (`ViewerAssistant` in `internal/assistant/wire`): the company
 * left it off, no model this node can reach, a caller that is not a person
 * signed in through a session, or a person the authority table refuses it.
 * Two more are the frame's own: nobody signed in, and a viewer answer still
 * on its way, which is drawn as "checking" rather than as any of the four, so
 * a reader is never told the assistant is off for the second it takes to ask.
 *
 * THE MODEL'S STATE IS NOT ONE OF THEM. A model the engine knows cannot answer
 * now (signed out, its plan spent, its CLI unable to start) leaves the Ask
 * button lit and the drawer open, with the conversation readable: the drawer
 * says so above its composer and keeps Send off ([assistantModelFor]), in the
 * engine's own sentence, the one a failed answer would have carried.
 */

import type { AssistantErrorCode } from "~/contract/assistant.ts";
import type { AssistantAccessReason } from "~/protocol/assistantWire.ts";
import { fmtMinute } from "~/lib/format.ts";
import type { ViewerState } from "~/lib/viewer.ts";

/** Why the reader may not ask, in the words each surface draws. */
export interface AssistantUnavailable {
  /** The engine's reason, or the frame's own. */
  reason: AssistantAccessReason | "anonymous" | "checking";
  /** After the Ask button's name and in its tooltip: one short sentence. */
  short: string;
  /** The drawer's empty state: a title and what it means for the reader. */
  title: string;
  body: string;
}

/** The sentences for each of the engine's reasons, given the company's name. */
function words(
  reason: AssistantAccessReason,
  company: string,
): Omit<AssistantUnavailable, "reason"> {
  switch (reason) {
    case "disabled":
      return {
        short: "An admin turned Ask Crewlet off.",
        title: `Ask Crewlet is off for ${company}`,
        body: "An admin turned it off. Everything else in Crewlet works as usual. Ask an admin if you need it.",
      };
    case "no_model":
      return {
        short: "Ask Crewlet has no model it can use right now.",
        title: "Ask Crewlet cannot answer right now",
        body: "None of the models it is set up with can be reached from here. Everything else in Crewlet works as usual. Ask an admin to check Ask Crewlet's models.",
      };
    case "not_a_person":
      return {
        short: "Ask Crewlet answers people signed in with their own account.",
        title: "Ask Crewlet is for people",
        body: "This session belongs to an API token or a service account. Sign in with your own account to ask.",
      };
    case "no_permission":
      return {
        short: "Your access does not include Ask Crewlet.",
        title: "Ask Crewlet is not part of your access",
        body: "Everything else you can open in Crewlet works as usual. Ask an admin if you need it.",
      };
  }
}

/**
 * Why `viewer` may not ask, or null when they may. `company` is the name the
 * empty state says it is off for.
 */
export function assistantUnavailableFor(
  viewer: Pick<ViewerState, "anonymous" | "asking" | "assistant">,
  company: string,
): AssistantUnavailable | null {
  if (viewer.asking) {
    return {
      reason: "checking",
      short: "Checking whether you can ask.",
      title: "Checking your access",
      body: "This takes a moment.",
    };
  }
  if (viewer.anonymous) {
    return {
      reason: "anonymous",
      short: "Sign in to ask.",
      title: "Sign in to ask",
      body: "Ask Crewlet answers as the person signed in, with their access.",
    };
  }
  if (viewer.assistant === null) return null;
  return { reason: viewer.assistant, ...words(viewer.assistant, company || "this company") };
}

/** Why the assistant's model cannot answer now, as the drawer draws it. */
export interface AssistantModelProblem {
  /** The error code a question would fail with. */
  code: AssistantErrorCode;
  /** Send's reason, after its name and in its tooltip: a short phrase. */
  short: string;
  /** The notice's heading: the engine's sentence. */
  title: string;
  /** What to do, the engine's hint for this reader, with the reset where one is known. */
  body: string;
}

/** Send's reason for each code the engine can know before a question. */
function shortFor(code: AssistantErrorCode): string {
  switch (code) {
    case "model_signed_out":
      return "Ask Crewlet's model is signed out";
    case "model_busy":
      return "Ask Crewlet's model is at its usage limit";
    default:
      return "Ask Crewlet's model cannot answer right now";
  }
}

/**
 * Why the assistant's model cannot answer now, or null when nothing is known
 * to be wrong with it. The heading and the hint are the engine's (an
 * administrator is handed the command that fixes it, anybody else whom to
 * ask); the reset of a spent plan is drawn in the reader's own time.
 */
export function assistantModelFor(
  viewer: Pick<ViewerState, "assistantModel">,
): AssistantModelProblem | null {
  const notice = viewer.assistantModel;
  if (notice === null) return null;
  const parts = [notice.hint ?? ""];
  if (notice.retry_at) parts.push(`You can ask again from ${fmtMinute(notice.retry_at)}.`);
  return {
    code: notice.code,
    short: shortFor(notice.code),
    title: notice.message,
    body: parts.filter((p) => p !== "").join(" "),
  };
}
