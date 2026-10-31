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

import { createContext, useContext, useSyncExternalStore } from "react";
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
  /** Open it with a question typed elsewhere (the palette's row), for the panel to ask. */
  ask: (question: string) => void;
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
  ask: () => {},
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

/**
 * A question typed outside the panel, waiting for the panel to ask it.
 *
 * THE PALETTE IS ONE CHUNK AND THE PANEL ANOTHER, and the panel may not be
 * loaded when the question is asked. So the frame queues it here (in the
 * entry, a few bytes) as it opens the panel, and the panel takes it once it
 * is drawn: it asks at once where it could ask, and otherwise puts the words
 * in its box. One question at a time; a newer one replaces one never taken.
 */
let queued: string | null = null;
const waiting = new Set<() => void>();

/** Queue a question for the panel. */
export function queueQuestion(question: string): void {
  queued = question;
  for (const listener of waiting) listener();
}

/** Take the queued question, if any; it is the taker's from here. */
export function takeQueuedQuestion(): string | null {
  const question = queued;
  queued = null;
  if (question !== null) for (const listener of waiting) listener();
  return question;
}

function subscribe(listener: () => void): () => void {
  waiting.add(listener);
  return () => {
    waiting.delete(listener);
  };
}

/** The queued question, as a value the panel re-renders on. */
export function useQueuedQuestion(): string | null {
  return useSyncExternalStore(
    subscribe,
    () => queued,
    () => null,
  );
}

/**
 * A conversation started outside the panel, waiting for the panel to show it:
 * a room's catch up (`roomBrief.ts`) asked in its own pane and continued in
 * Ask Crewlet. Queued here for the reason a question is, and taken the same
 * way; a newer one replaces one never taken.
 */
let queuedConversation: string | null = null;

/** Queue a conversation for the panel to open. */
export function queueConversation(id: string): void {
  queuedConversation = id;
  for (const listener of waiting) listener();
}

/** Take the queued conversation, if any; it is the taker's from here. */
export function takeQueuedConversation(): string | null {
  const id = queuedConversation;
  queuedConversation = null;
  if (id !== null) for (const listener of waiting) listener();
  return id;
}

/** The queued conversation, as a value the panel re-renders on. */
export function useQueuedConversation(): string | null {
  return useSyncExternalStore(
    subscribe,
    () => queuedConversation,
    () => null,
  );
}
