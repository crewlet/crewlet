/**
 * A conversation the engine holds (ADR-0117): its context did not fit the
 * seat's models twice in a row, so the engine stopped starting turns on it.
 * The advisor's notices name each hold by its seat and its conversation.
 */

import type { AdvisorNotice } from "~/protocol/advisor.ts";

/** The notice class of a held conversation. */
export const HELD_CLASS = "conversation.held";

/** Why the engine holds a conversation, in the words the item and Inbox say. */
export const HELD_REASON = "its context did not fit its models";

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
