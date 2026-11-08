/**
 * Whom a person's message reached, and who answered: the words a receipt
 * under a message and the reach line over the composer draw with uilet's
 * `DeliveryReceipt`.
 *
 * THE ENGINE DECIDES, THIS FILE WORDS. Whom a message reached is the post's
 * own routing, written onto the message (`message.reach`); who answered is
 * marked as the answers land (`answered`); whom a draft would reach is the
 * post's decision run dry (`chat_reach`). Nothing here routes: a second rule
 * in the browser would be one that could disagree with the send.
 */

import type { DeliveryReceiptRecipient } from "@crewlethq/ui";

import type { ChatAnswer, ChatMessage, ChatQuiet, ChatReach } from "~/protocol/index.ts";

/**
 * The note drawn after the seats a reason reached, for the reasons a person
 * would not guess from the message alone. A mention, a direct message, a reply
 * to the seat's own thread and the room's answerer need none.
 */
const REASON_NOTES: Record<string, string> = {
  follow_all: "every message",
  collective_request: "@channel",
  collective: "@channel",
  reply: "in the thread",
  follow: "following the thread",
};

/** The seats a reach names, as the receipt draws them, answered or not. */
export function reachRecipients(
  reach: ChatReach | undefined,
  answered: readonly ChatAnswer[] | undefined,
  nameOf: (handle: string) => string,
): DeliveryReceiptRecipient[] {
  const done = new Set((answered ?? []).map((a) => a.handle));
  return (reach?.recipients ?? []).map((r) => ({
    id: r.handle,
    name: nameOf(r.handle),
    reason: REASON_NOTES[r.reason],
    answered: done.has(r.handle),
  }));
}

/**
 * Whether a message carries a receipt: a person's own words with a reach of
 * their own (an agent they named, a thread it follows, the room's answerer),
 * an answer to them, or an agent they named and did not reach.
 *
 * "REACHED NO AGENT" ONLY WHERE THE WORDS ASKED ONE. A post that names nobody
 * in a room no agent answers is the room's standing state, which the
 * composer's reach line says before the send; warned under every message, a
 * room of people read as failing.
 *
 * AND NOT THE ROOM'S STANDING AUDIENCE EITHER. Agents that hear every message
 * in a room hear this one for that reason alone; the composer names them
 * before the send, and repeated under every line the receipt took a line of
 * every message in the room.
 */
export function showsReceipt(
  message: ChatMessage,
  isAgent: (handle: string) => boolean,
  answered?: readonly ChatAnswer[],
): boolean {
  const reach = message.reach;
  if (!reach || message.event || message.deleted_at || message.author_kind !== "human") {
    return false;
  }
  const recipients = reach.recipients ?? [];
  if (recipients.length > 0) {
    return (answered ?? []).length > 0 || recipients.some((r) => r.reason !== "follow_all");
  }
  return (message.mentions ?? []).some(isAgent);
}

/** Why a draft reaches no agent, as the composer's line says it, or nothing. */
export function quietHint(quiet: ChatQuiet | undefined): string | undefined {
  switch (quiet) {
    case "no_agent":
      return "No agent in this room will answer. Mention one with @ to ask it.";
    case "addressed_to_people":
      return "It names only people, so it is for them.";
    default:
      return undefined;
  }
}
