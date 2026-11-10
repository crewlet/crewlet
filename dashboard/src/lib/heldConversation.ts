/**
 * A conversation the engine holds (ADR-0117): it failed the same way twice in a
 * row (its context did not fit the seat's models, or its work ran out of its
 * sandbox's memory), so the engine stopped starting turns on it. The advisor's
 * notices name each hold by its seat and its conversation, and say its cause.
 */

import type { AdvisorNotice } from "~/protocol/advisor.ts";

/** The notice class of a held conversation. */
export const HELD_CLASS = "conversation.held";

/** The cause of a hold whose work ran out of its sandbox's memory. */
export const HELD_OUT_OF_MEMORY = "out_of_memory";

/** Why the engine holds a conversation, in the words the item and Inbox say. */
export function heldReason(n: AdvisorNotice): string {
  return n.cause === HELD_OUT_OF_MEMORY
    ? "its work ran out of its sandbox's memory"
    : "its context did not fit its models";
}

/** What a person changes to release a hold, beside writing on it. */
export function heldSetting(n: AdvisorNotice): string {
  return n.cause === HELD_OUT_OF_MEMORY ? "sandbox" : "models";
}

/** The conversation key the engine gives a work item's own thread. */
export function itemConversation(id: string): string {
  return `task:${id}`;
}

/** The work item a held conversation is about, where it is one. */
export function heldItem(n: AdvisorNotice): string | undefined {
  const name = n.subject.name ?? "";
  return name.startsWith("task:") ? name.slice("task:".length) : undefined;
}

/** The engine's hold on item id's conversation, read off the advisor's notices. */
export function itemHold(
  notices: readonly AdvisorNotice[] | undefined,
  id: string,
): AdvisorNotice | undefined {
  return notices?.find((n) => n.class === HELD_CLASS && n.subject.name === itemConversation(id));
}
