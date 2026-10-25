/**
 * The frame's hold on the assistant, for the controls that open it.
 *
 * The frame owns whether the assistant is open (`Shell.tsx`): it is a panel
 * of the frame, not of a screen, and it stays open across a navigation the
 * way the New task sheet does. What a control needs of that is here: whether
 * it is open, why the reader may not ask (null where they may), and the one
 * gesture that opens and closes it. The Ask button in the page bar reads it,
 * and so does a screen that draws its own head (an open artifact), which
 * places the same button.
 */

import { createContext, useContext } from "react";
import type { AssistantUnavailable } from "./availability.ts";

/** Where focus goes when the assistant opens: the composer, or the panel's heading. */
export type AssistantFocus = "composer" | "heading";

export interface AssistantControl {
  /** Whether the panel is open. */
  open: boolean;
  /** Why the reader may not ask, or null where they may. */
  unavailable: AssistantUnavailable | null;
  /** Open it (focusing where `focus` says), or close it and hand focus back. */
  toggle: (focus?: AssistantFocus) => void;
}

/** A frame with no assistant: nothing opens, and the reason says so. */
const NONE: AssistantControl = {
  open: false,
  unavailable: {
    reason: "checking",
    short: "Checking whether you can ask.",
    title: "Checking your access",
    body: "This takes a moment.",
  },
  toggle: () => {},
};

export const AssistantControlContext = createContext<AssistantControl>(NONE);

/** The frame's hold on the assistant. */
export function useAssistantControl(): AssistantControl {
  return useContext(AssistantControlContext);
}

/** The id the Ask button carries, so focus can be handed back to it. */
export const ASSISTANT_TRIGGER_ID = "assistant-trigger";

/** The id the composer's field carries, so the frame's key can put focus in it. */
export const ASSISTANT_COMPOSER_ID = "assistant-composer";
