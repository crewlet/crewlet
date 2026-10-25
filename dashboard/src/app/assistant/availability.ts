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
 */

import type { AssistantViewerReason } from "~/contract/assistant.ts";
import type { ViewerState } from "~/lib/viewer.ts";

/** Why the reader may not ask, in the words each surface draws. */
export interface AssistantUnavailable {
  /** The engine's reason, or the frame's own. */
  reason: AssistantViewerReason | "anonymous" | "checking";
  /** After the Ask button's name and in its tooltip: one short sentence. */
  short: string;
  /** The drawer's empty state: a title and what it means for the reader. */
  title: string;
  body: string;
}

/** The sentences for each of the engine's reasons, given the company's name. */
function words(
  reason: AssistantViewerReason,
  company: string,
): Omit<AssistantUnavailable, "reason"> {
  switch (reason) {
    case "disabled":
      return {
        short: "An admin turned the assistant off.",
        title: `The assistant is off for ${company}`,
        body: "An admin turned it off. Everything else in Crewlet works as usual. Ask an admin if you need it.",
      };
    case "no_model":
      return {
        short: "The assistant has no model it can use right now.",
        title: "The assistant cannot answer right now",
        body: "None of the models it is set up with can be reached from here. Everything else in Crewlet works as usual. Ask an admin to check the assistant's models.",
      };
    case "not_a_person":
      return {
        short: "The assistant answers people signed in with their own account.",
        title: "The assistant is for people",
        body: "This session belongs to an API token or a service account. Sign in with your own account to ask.",
      };
    case "no_permission":
      return {
        short: "Your access does not include the assistant.",
        title: "The assistant is not part of your access",
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
      body: "The assistant answers as the person signed in, with their access.",
    };
  }
  if (viewer.assistant === null) return null;
  return { reason: viewer.assistant, ...words(viewer.assistant, company || "this company") };
}
