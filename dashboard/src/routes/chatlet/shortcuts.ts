/**
 * Chatlet's keys, bound for as long as the screen is open. Which key does
 * what is `app/keymap.ts`'s `CHAT` rows, the one place a key is spelled, so
 * the legend and Chatlet's own list of keys say what is bound here.
 */

import { useToast } from "@crewlethq/ui";

import { usePeek } from "~/app/frame/peekAddress.ts";
import { useKeymap } from "~/app/keymap.ts";
import type { ChatChannelSummary } from "~/protocol/index.ts";

import { openRoomOf, type ChatFrame, type ChatNav } from "./nav.tsx";
import { roomTitle, sectionRooms, unreadIn } from "./rooms.ts";

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

/**
 * Whether marking `room` read has anything to mark: unread posts, muted or
 * not. A room the rail does not list has no cursor here, and an unmeasured
 * zero says nothing, so that room may still hold some.
 */
export function markable(room: ChatChannelSummary | undefined, measured: boolean): boolean {
  if (!room) return false;
  return !measured || room.unread > 0;
}

/** The one toast a mark by key says, which a repeat replaces rather than stacks under. */
const READ_TOAST = "chat-marked-read";

export function useChatShortcuts(nav: ChatNav, frame: ChatFrame): void {
  const toast = useToast();
  // A PEEK OPEN OVER THE SCREEN TAKES ESCAPE, which closes it and marks
  // nothing read (`app/keymap.ts`'s `SCOPES_APART`).
  const peek = usePeek();
  const room = openRoomOf(nav.place);
  const toMark = markable(
    frame.channels.find((summary) => summary.channel.id === room),
    frame.measured,
  );
  const step = (unread: boolean) => (e: KeyboardEvent) => {
    // IN THE PANEL'S ORDER, section by section, so the next room is the row
    // below the open one (`rooms.ts`'s `sectionRooms`).
    const listed = sectionRooms(frame.channels, {
      options: frame.list,
      starred: frame.starred,
      measured: frame.measured,
      current: room,
      titleOf: (summary) =>
        roomTitle(summary.channel, {
          participants: summary.participants,
          viewer: frame.viewer,
          nameOf: frame.nameOf,
        }),
    }).flatMap((section) => section.rooms);
    const next = stepRoom(listed, room, e.key.toLowerCase() === "arrowup" ? -1 : 1, {
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
        toast.ok("Marked as read", { id: READ_TOAST });
      },
      // ONLY WHEN NOTHING ELSE WAS LISTENING FOR IT: a pane, the slid-in
      // Rooms panel and a peek each close on Escape, and closing one is just
      // closing it. A menu or a dialog answers it first and the press never
      // arrives here (`lib/keys.ts`). And only with something to mark: a
      // reflexive Escape in a room already read changes nothing to confirm.
      when: room !== "" && toMark && nav.pane === null && !nav.rooms.open && peek === null,
    },
  });
}
