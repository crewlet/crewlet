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
import { useClient, useSlice } from "~/lib/store-hooks.ts";
import type {
  ChatLive,
  ChatReadState,
  ChatRoomPresence,
  HeldChatChange,
} from "~/protocol/index.ts";

/**
 * How long live frames pile up before the screen re-reads the room.
 *
 * A THIRD OF A SECOND, and it is a rate limit rather than a delay budget: a
 * room in the middle of a conversation produces a burst of frames — a post, a
 * reaction, an edit, three more posts — and one read per frame would put a
 * query per message on the socket, which is exactly the load the frames exist
 * to avoid. Under this window a burst is ONE read.
 *
 * The first frame is not delayed at all (see [useCoalesced]): a single message
 * in a quiet room appears as fast as the round trip allows, and only a second
 * one inside the window waits — for at most this long.
 */
export const LIVE_REFETCH_MS = 350;

const NO_PRESENCE: ChatRoomPresence = {};

/**
 * How much has happened in the company's chat since this tab connected, as one
 * number.
 *
 * FOR THE RAIL, which cares that SOMETHING landed and not what: a message in
 * any room changes the rail's order, its preview and its badge, and the rail
 * has no way to learn any of those from a frame — the counts are the
 * engine's, counted against a cursor this tab does not hold. So it re-reads
 * its one question, and this is the signal it re-reads on.
 *
 * THE STORE'S OWN COUNT ([ChatLive.frames]), never one added up from the
 * rooms: each room keeps a bounded number of frames and the tab a bounded
 * number of rooms, so past either bound a new frame and the one it pushes out
 * cancel in any sum of what is held, and the rail would stop moving exactly
 * when the company got busy.
 */
export function useChatActivity(): number {
  return useSlice(["chat"], (state) => state.chat.frames);
}

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
    useCallback((state) => state.chatPresence.rooms[channelID] ?? NO_PRESENCE, [channelID]),
  );
}

/** This person's own read state, or null when nothing has said. */
export function useChatRead(): ChatReadState | null {
  return useSlice(["chatRead"], (state) => state.chatRead);
}

/**
 * Run `task` when `signal` moves, at most once per window.
 *
 * LEADING EDGE, then a trailing one for whatever arrived inside the window.
 * The two halves are different failures: without the leading edge every single
 * message in a quiet room waits out the window for no reason, and without the
 * trailing one the last frame of a burst — which is the message somebody just
 * sent — is the one that never causes a read.
 */
export function useCoalesced(signal: number, task: () => void, window = LIVE_REFETCH_MS): void {
  const latest = useRef(task);
  latest.current = task;
  const ranAt = useRef(0);
  const timer = useRef<ReturnType<typeof setTimeout> | 0>(0);
  const first = useRef(true);

  useEffect(() => {
    // The mount is not a change. The screen's own first read is the query's,
    // and running here as well would double every screen's opening cost.
    if (first.current) {
      first.current = false;
      return;
    }
    const since = Date.now() - ranAt.current;
    if (since >= window) {
      ranAt.current = Date.now();
      latest.current();
      return;
    }
    if (timer.current) return;
    timer.current = setTimeout(() => {
      timer.current = 0;
      ranAt.current = Date.now();
      latest.current();
    }, window - since);
  }, [signal, window]);

  useEffect(() => () => clearTimeout(timer.current), []);
}

/**
 * Tell the engine which room this tab is looking at, and whether somebody is
 * typing in it.
 *
 * THE FRAME IS TRUSTED ABOUT ATTENTION AND NEVER ABOUT PERMISSION. A tab may
 * name any room it likes; what comes back is filtered by the same visibility
 * rule every committed frame is, so naming a room somebody is not in produces
 * nothing at all. What it buys is that the fleet-wide presence probe is
 * bounded to the rooms somebody actually has open.
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
