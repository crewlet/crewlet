/**
 * The live half of the screen: what the socket says, and what the screen does
 * about it.
 *
 * THE FRAMES ARE EVIDENCE, NOT CONTENT. A chat frame carries no body — the
 * engine says so at the type, and it is right: a frame that carried one would
 * be a second rendering path for a conversation, and the two would disagree
 * the first time an edit raced a reload. So nothing here renders a frame.
 * What the screen does with one is RE-READ, and everything in this file is
 * about doing that at a rate a busy room can afford.
 *
 * NOTHING HERE POLLS WHILE THE SOCKET IS DOWN, and nothing could: every read
 * this screen makes is a socket QUERY, which waits for the connection rather
 * than failing (`LiveSocket.query`), so a poll scheduled while it is down asks
 * once and waits with everybody else. The screen does what every socket-query
 * screen does instead — keeps its last answer on screen while the frame's
 * degraded banner says the live channel is down, and asks again the moment it
 * is back (`useQuery`'s `refetchOnReconnect`).
 *
 * THE FILTERING IS THE SERVER'S. A frame reaches this socket only if the
 * engine decided this viewer may read that room, through the domain's own
 * visibility rule and failing closed. There is therefore no client-side
 * visibility check anywhere in this screen, and there must not be: a second
 * rule would be one that could disagree.
 */

import { useCallback, useEffect, useMemo, useRef } from "react";

import { TYPING_THROTTLE_MS } from "~/contract/chat.ts";
import { useCoalesced } from "~/lib/chatLive.ts";
import { useClient, useSlice } from "~/lib/store-hooks.ts";
import type {
  ChatLive,
  ChatRoomPresence,
  ChatWorking,
  HeldChatChange,
  RoomRoster,
} from "~/protocol/index.ts";

const NO_PRESENCE: ChatRoomPresence = {};

/**
 * The frames this tab accepted after the one numbered `through` — in one room,
 * or in every room when `channelID` is null — newest first; or NULL when it
 * cannot say.
 *
 * NULL IS AN ANSWER, and the one a caller must not read as "nothing": a frame
 * after `through` has been let go of ([ChatLive.dropped]) — pushed off its
 * room's bounded list, dropped with its room, forgotten with a seat, or never
 * sent, which is what a frame naming no room says ([ChatLive.gap]) — so what
 * it changed is unknown, and the honest repair is to read everything a screen
 * holds again. An empty list is the other answer: nothing arrived.
 */
export function framesSince(
  live: ChatLive,
  through: number,
  channelID: string | null,
): HeldChatChange[] | null {
  if (live.dropped > through) return null;
  const rooms = channelID === null ? Object.values(live.rooms) : [live.rooms[channelID]];
  const out: HeldChatChange[] = [];
  for (const room of rooms) {
    // NEWEST FIRST, so the walk stops at the first frame already answered.
    for (const change of room?.changes ?? []) {
      if (change.frame <= through) break;
      out.push(change);
    }
  }
  return out.sort((a, b) => b.frame - a.frame);
}

/**
 * Run `task` with the frames that arrived since it last ran — in one room, or
 * in every room when `channelID` is null — at most once per [LIVE_REFETCH_MS]
 * ([useCoalesced]). `null` is [framesSince]'s: some of them were let go of.
 *
 * WHAT A SCREEN HOLDING MORE THAN ONE PAGE RE-READS ON. A screen that read
 * every page it held again on every frame paid a query per page for a frame
 * that touched none of them — a thread re-read whole for a message in another
 * thread, a mention feed for a reaction in a room it holds nothing from —
 * against a socket that runs four queries at once for the whole screen. With
 * the frames in hand a screen asks only for the page that holds what a frame
 * names; the frame carries no body, so it is still a re-read and never
 * something drawn.
 *
 * THE SIGNAL IS THE COUNT A FRAME MOVES — the room's [ChatRoomLive.frame],
 * or the tab's [ChatLive.frames] — NEVER THE LENGTH OF WHAT IS HELD. The
 * frames are bounded, so past the bound a new frame pushes the oldest out and
 * a length stands still: a room watched by its length stopped re-reading at
 * its two-hundredth frame in a tab. ONE ROOM'S SIGNAL ALSO MOVES ON A GAP
 * ([ChatLive.gap]), the frame that names no room and says frames for any room
 * may have been missed: it cannot move a room's own count, and a room no
 * frame has reached yet has none to move.
 *
 * THE MOUNT IS NOT A CHANGE: a screen's own first read answers for every
 * frame before it, so the first run is handed only what arrived after.
 */
export function useChatFrames(
  channelID: string | null,
  task: (frames: readonly HeldChatChange[] | null) => void,
): void {
  const { store } = useClient();
  const signal = useSlice(
    ["chat"],
    useCallback(
      (state) =>
        channelID === null
          ? state.chat.frames
          : Math.max(state.chat.rooms[channelID]?.frame ?? 0, state.chat.gap),
      [channelID],
    ),
  );
  // THE LAST FRAME ANSWERED FOR, read once at the first render — the frame
  // the screen's own first read was asked after.
  const through = useRef<number | null>(null);
  if (through.current === null) through.current = store.state.chat.frames;
  useCoalesced(signal, () => {
    const live = store.state.chat;
    const frames = framesSince(live, through.current ?? 0, channelID);
    through.current = live.frames;
    task(frames);
  });
}

/** Who is looking at, typing in, or working on a message in one room. */
export function useChatPresence(channelID: string): ChatRoomPresence {
  return useSlice(
    ["chatPresence"],
    useCallback((state) => chatRoomPresence(state.chatPresence.rooms[channelID]), [channelID]),
  );
}

/**
 * A chat room's members, as the room's screen draws them.
 *
 * A PERSON is in `viewing` whatever they are doing in the room — one with it
 * open in another tab and away is still in it — and in `typing` while their
 * member is editing, which the engine puts back to viewing once nothing has
 * re-asserted it. A SEAT is `working` while its member is editing, which is
 * what the working indicator is (the engine raises it as the seat's member and
 * withdraws it as the turn lets go), under the thread its answer will land in
 * and with the line the room shows beside it. Each person and each seat's
 * thread once, however many sessions present them.
 */
export function chatRoomPresence(roster: RoomRoster | undefined): ChatRoomPresence {
  const members = roster?.members ?? [];
  if (members.length === 0) return NO_PRESENCE;
  const viewing = new Set<string>();
  const typing = new Set<string>();
  const working = new Map<string, ChatWorking>();
  for (const member of members) {
    if (member.kind === "agent") {
      if (member.activity !== "editing") continue;
      const thread = member.focus?.thread || undefined;
      const key = `${member.seat} ${thread ?? ""}`;
      if (!working.has(key))
        working.set(key, { handle: member.seat, thread, status: member.status });
      continue;
    }
    viewing.add(member.seat);
    if (member.activity === "editing") typing.add(member.seat);
  }
  return { viewing: [...viewing], typing: [...typing], working: [...working.values()] };
}

/**
 * Tell the engine which room this tab is looking at, and whether somebody is
 * typing in it: the chat room's `presence` frame, viewing or, while somebody
 * types, editing — re-asserted at most every `TYPING_THROTTLE_MS`, inside the
 * engine's lapse.
 *
 * THE FRAME IS TRUSTED ABOUT ATTENTION AND NEVER ABOUT PERMISSION. A tab may
 * name any room it likes; the engine puts the socket in the room's presence
 * only once the room's own visibility rule admits its reader, the rule every
 * committed frame is filtered by, so naming a room somebody is not in produces
 * nothing at all.
 *
 * Withdrawn on the way out — an empty room id — because a tab that navigated
 * away is not looking at anything, and leaving the last room asserted would
 * show a reader as present in a room they closed until their socket dropped.
 */
export function useChatFocus(channelID: string): (typing: boolean) => void {
  const { socket } = useClient();
  const typedAt = useRef(0);

  useEffect(() => {
    socket.focus(channelID, false);
    typedAt.current = 0;
    return () => socket.focus("", false);
  }, [socket, channelID]);

  return useCallback(
    (typing: boolean) => {
      if (!channelID) return;
      const now = Date.now();
      if (typing && now - typedAt.current < TYPING_THROTTLE_MS) return;
      typedAt.current = typing ? now : 0;
      socket.focus(channelID, typing);
    },
    [socket, channelID],
  );
}

/**
 * The ids the room and the thread currently carry, as one set.
 *
 * It is what settles an outgoing message, and it is memoised because the
 * room's own settle effect (`Room.tsx`, which hands it to the outbox's
 * `settle`) depends on it: rebuilt every render, that effect would run on
 * every render of the screen rather than when a page actually merged.
 *
 * TWO LISTS RATHER THAN A REST PARAMETER, because the dependency array of a
 * memo may not change length between renders — and a thread that opens and
 * closes is exactly a list that would come and go.
 */
export function useSettledIDs(
  room: readonly { message: { id: string } }[],
  thread: readonly { message: { id: string } }[],
): Set<string> {
  return useMemo(
    () => new Set([...room, ...thread].map((view) => view.message.id)),
    [room, thread],
  );
}
