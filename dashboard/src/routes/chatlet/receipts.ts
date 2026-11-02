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

import { isDirect } from "~/lib/chatRooms.ts";
import type { ChatAnswer, ChatKind, ChatMessage, ChatQuiet, ChatReach } from "~/protocol/index.ts";

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
 * Whether a message carries a receipt: a person's own words with a reach,
 * that reached an agent, or that reached none where one was expected.
 *
 * "REACHED NO AGENT" ONLY WHERE A PERSON WOULD WAIT FOR ONE: a post in a named
 * room that names nobody, which is the message the room's answerer exists for.
 * A message to a person, or a direct conversation with people, reaches no
 * agent by design, and a warning under every one of them would be noise.
 */
export function showsReceipt(message: ChatMessage, kind: ChatKind | undefined): boolean {
  const reach = message.reach;
  if (!reach || message.event || message.deleted_at || message.author_kind !== "human") {
    return false;
  }
  if ((reach.recipients ?? []).length > 0) return true;
  return !!kind && !isDirect(kind) && (message.mentions ?? []).length === 0;
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
