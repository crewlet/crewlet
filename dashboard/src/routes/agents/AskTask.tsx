/**
 * The seat's second way to write to it, an entry of the seat page's and the
 * seat panel's "More" menus.
 *
 * MESSAGE IS THE DIRECT CONVERSATION where the company runs the engine's own
 * chat and the reader holds Chatlet (`lib/seatMessage.ts`' `useSeatMessage`),
 * so the tracker's ASK, a task the seat owes an answer on whose answer lands
 * in the asker's Inbox, is this entry there: "Ask as a task". Elsewhere
 * Message is that ask already, and there is no entry. Filing work for the
 * seat is Assign task in every company.
 *
 * HELD WITH THE ASK'S OWN REASON, the own-seat hold included, as Message is:
 * a door onto a sheet that then refused teaches the reason one press too
 * late.
 */

import type { PageMenuEntry } from "~/app/header/PageHeader.tsx";
import { MessageCircleQuestionMarkGlyph } from "@crewlethq/icons/glyphs";
import { useSeatAsk } from "~/lib/seatMessage.ts";

/** "Ask as a task", where Message is a direct conversation; none elsewhere. */
export function useAskTaskEntry(handle: string): PageMenuEntry[] {
  const ask = useSeatAsk(handle);
  if (ask === null || !handle) return [];
  return [
    {
      key: "ask",
      label: "Ask as a task",
      description: "A question the seat owes an answer on, in your Inbox",
      icon: <MessageCircleQuestionMarkGlyph size="sm" />,
      onSelect: ask.open,
      ...(ask.held ? { disabled: true, description: ask.held } : {}),
    },
  ];
}
