/**
 * The frame's hold on the assistant, for the controls that open it.
 *
 * The frame owns whether the assistant is open (`Shell.tsx`): it is a panel
 * of the frame, not of a screen, and it stays open across a navigation the
 * way the New task sheet does. What a control needs of that is here: whether
 * it is open, why the reader may not ask (null where they may), and the one
 * gesture that opens and closes it. The Ask button in the page bar reads it,
 * and so does a screen that draws its own head (an open artifact), which
 * places the same button. `open` is the one answer to "is Ask Crewlet on
 * screen", and the button draws nothing while it holds, on every route.
 */

import { createContext, useContext, useSyncExternalStore } from "react";
import type { AssistantUnavailable } from "./availability.ts";

/** Where focus goes when the assistant opens: the composer, or the panel's heading. */
export type AssistantFocus = "composer" | "heading";

export interface AssistantControl {
  /**
   * Whether Ask Crewlet is open: its panel beside the page, or its own page
   * as the screen. The Ask button is not drawn while it is.
   */
  open: boolean;
  /** Why the reader may not ask, or null where they may. */
  unavailable: AssistantUnavailable | null;
  /** Open it (focusing where `focus` says), or close it and hand focus back. */
  toggle: (focus?: AssistantFocus) => void;
  /** Open it with a question typed elsewhere (the palette's row), asked in the conversation open now. */
  ask: (question: string) => void;
  /**
   * Ask about something a screen shows ("Ask Crewlet about this"): always in
   * a NEW conversation, with the page attached, never in the one open or used
   * last, which is about something else. `send` asks it at once; without it
   * the question waits in the box for the person to read, change and send,
   * where the screen worded it and it is not to be sent as theirs.
   */
  askAbout: (question: string, how: { send: boolean }) => void;
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
  askAbout: () => {},
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
 * A question typed outside the panel, waiting for the panel to ask it, or
 * (`send: false`) to put in its box for the person to send.
 *
 * THE PALETTE IS ONE CHUNK AND THE PANEL ANOTHER, and the panel may not be
 * loaded when the question is asked. So the frame queues it here (in the
 * entry, a few bytes) as it opens the panel, and the panel takes it once it
 * is drawn: it asks at once where it could ask and was asked to, and
 * otherwise puts the words in its box. One question at a time; a newer one
 * replaces one never taken.
 */
export interface QueuedQuestion {
  text: string;
  /** Ask it at once where the panel can; false puts it in the box. */
  send: boolean;
  /**
   * Start a new conversation for it: a question about something a screen
   * shows. The queue carries this because only the panel holds the
   * conversation, and one taken into whatever it held was the last topic's.
   */
  fresh: boolean;
}

let queued: QueuedQuestion | null = null;
const waiting = new Set<() => void>();

/** Queue a question for the panel to ask in the conversation it holds. */
export function queueQuestion(question: string): void {
  queued = { text: question, send: true, fresh: false };
  for (const listener of waiting) listener();
}

/** Queue a question about what a screen shows, for a new conversation: asked where `send`, else put in its box. */
export function queueAbout(question: string, send: boolean): void {
  queued = { text: question, send, fresh: true };
  for (const listener of waiting) listener();
}

/** How the frame holds the panel, for the verbs that hand it a question. */
export interface PanelHold {
  open: boolean;
  /** Ask Crewlet's own page is the screen: its conversation takes the question and nothing opens. */
  here: boolean;
  /** The reader may not ask: nothing is queued. */
  closed: boolean;
  /** Open the panel with focus in its composer. */
  show: () => void;
  /** Put focus in the open panel's composer. */
  focusComposer: () => void;
}

/** The frame's `ask` and `askAbout`: each queues its question and opens the panel to take it. */
export function askVerbs(hold: PanelHold): Pick<AssistantControl, "ask" | "askAbout"> {
  const reach = (focusBox: boolean) => {
    if (!hold.open && !hold.here) hold.show();
    else if (focusBox) hold.focusComposer();
  };
  return {
    ask: (question) => {
      if (hold.closed) return;
      queueQuestion(question);
      reach(false);
    },
    askAbout: (question, { send }) => {
      if (hold.closed) return;
      queueAbout(question, send);
      // Only a question left in the box takes focus from the screen it came from.
      reach(!send);
    },
  };
}

/** Take the queued question, if any; it is the taker's from here. */
export function takeQueuedQuestion(): QueuedQuestion | null {
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
export function useQueuedQuestion(): QueuedQuestion | null {
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

/**
 * The person opened the closed drawer (any of its doors), as against the drawer
 * drawn again by a reload or a resize: only that open applies `reopenChoice`.
 */
let reopened = false;

/** Note that the person is opening the closed drawer. */
export function markReopened(): void {
  reopened = true;
}

/** Whether the drawer now mounting was opened by the person; asked once. */
export function takeReopened(): boolean {
  const was = reopened;
  reopened = false;
  return was;
}
