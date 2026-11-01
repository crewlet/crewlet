/**
 * Chatlet's keys, bound for as long as the screen is open. Which key does
 * what is `app/keymap.ts`'s `CHAT` rows, the one place a key is spelled, so
 * the legend and Chatlet's own list of keys say what is bound here.
 */

import { useToast } from "@crewlethq/ui";

import { usePeek } from "~/app/frame/peekAddress.ts";
import { useKeymap } from "~/app/keymap.ts";
import type { ChatChannelSummary } from "~/protocol/index.ts";

import type { ChatFrame, ChatNav } from "./nav.tsx";
import { unreadIn } from "./rooms.ts";

/**
 * The room a step from the open one, through `rooms` in the order the
 * reader sees them, wrapping at either end; or null when there is nowhere
 * to go.
 *
 * WITH `unread`, only the rooms with unread messages are stepped through,
 * and the open room counts as one of them, so the step is taken from where
 * the reader is rather than from the first unread room.
 */
export function stepRoom(
  rooms: readonly ChatChannelSummary[],
  current: string,
  step: -1 | 1,
  opts: { unread: boolean; measured: boolean },
): string | null {
  const list = rooms.filter(
    (room) =>
      !room.channel.archived_at &&
      (!opts.unread || unreadIn(room, opts.measured) || room.channel.id === current),
  );
  if (list.length === 0) return null;
  const at = list.findIndex((room) => room.channel.id === current);
  // FROM NOWHERE (a view, not a room), Down is the first room and Up the last.
  const next =
    at < 0 ? (step === 1 ? 0 : list.length - 1) : (at + step + list.length) % list.length;
  const id = list[next]!.channel.id;
  return id === current ? null : id;
}

export function useChatShortcuts(nav: ChatNav, frame: ChatFrame): void {
  const toast = useToast();
  // A PEEK OPEN OVER THE SCREEN TAKES ESCAPE, which closes it and marks
  // nothing read (`app/keymap.ts`'s `SCOPES_APART`).
  const peek = usePeek();
  const room = nav.place.kind === "room" ? nav.place.id : "";
  const step = (unread: boolean) => (e: KeyboardEvent) => {
    const next = stepRoom(frame.channels, room, e.key.toLowerCase() === "arrowup" ? -1 : 1, {
      unread,
      measured: frame.measured,
    });
    if (next) nav.openRoom(next);
  };
  useKeymap({
    "chat.room": step(false),
    "chat.unread": step(true),
    "chat.search": () => nav.go(room ? { kind: "search", room } : { kind: "search" }),
    "chat.keys": () => nav.openDialog({ kind: "shortcuts" }),
    "chat.read": {
      run: () => {
        frame.markRead(room);
        toast.ok("Marked as read");
      },
      // ONLY WHEN NOTHING ELSE WAS LISTENING FOR IT: a pane, the slid-in
      // Rooms panel and a peek each close on Escape, and closing one is just
      // closing it. A menu or a dialog answers it first and the press never
      // arrives here (`lib/keys.ts`).
      when: room !== "" && nav.pane === null && !nav.rooms.open && peek === null,
    },
  });
}
