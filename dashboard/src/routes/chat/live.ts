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
 * THE FILTERING IS THE SERVER'S. A frame reaches this socket only if the
 * engine decided this viewer may read that room, through the domain's own
 * visibility rule and failing closed. There is therefore no client-side
 * visibility check anywhere in this screen, and there must not be: a second
 * rule would be one that could disagree.
 */

import { useCallback, useEffect, useMemo, useRef } from "react";

import { useClient, useSlice } from "~/lib/store-hooks.ts";
import type {
  ChatChange,
  ChatReadState,
  ChatRoomLive,
  ChatRoomPresence,
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

/**
 * How often a tab re-asserts that somebody is typing.
 *
 * THREE SECONDS, half the engine's own `ChatTypingTTL` of six: the indicator
 * expires on its own if a lid closes mid-word, so a client that re-asserted
 * at the TTL would blink off and on under somebody who never stopped typing.
 * Half of it means one lost frame is invisible and two are not.
 *
 * It is also a rate limit on a FLEET-WIDE probe: a focus frame makes the node
 * re-ask its peers who is in the room, so one frame per keystroke would be a
 * scatter per character.
 */
export const TYPING_THROTTLE_MS = 3_000;

/**
 * How often this screen re-reads WHILE THE SOCKET IS DOWN.
 *
 * FIVE SECONDS, which is the cadence the client's own degraded fallback
 * already polls the snapshot at (`LiveSocket`'s `FALLBACK_MS`) — so a reader
 * whose socket is refusing to upgrade sees one staleness across the whole
 * page rather than a transcript a minute behind the header above it.
 *
 * It is undefined while the socket is up, and that is not an optimisation: a
 * poll on top of a push is two answers to one question, and the one that
 * arrives second wins whether or not it is newer.
 */
export const DEGRADED_POLL_MS = 5_000;

const NO_PRESENCE: ChatRoomPresence = {};

/** What this tab has seen happen in one room since it connected. */
export function useChatLive(channelID: string): ChatRoomLive | undefined {
  return useSlice(
    ["chat"],
    useCallback((state) => state.chat.rooms[channelID], [channelID]),
  );
}

/**
 * How much has happened in the company's chat since this tab connected, as one
 * number.
 *
 * FOR THE RAIL, which cares that SOMETHING landed and not what: a message in
 * any room changes that list's order, its preview and its badge, and the rail
 * has no way to learn any of those from a frame — the counts are the engine's,
 * counted against a cursor this tab does not hold. So it re-reads, and this is
 * the signal it re-reads on.
 *
 * A COUNT RATHER THAN THE SLICE. The rooms are mutated in place, so their
 * identity says nothing about whether anything moved.
 */
export function useChatActivity(): number {
  return useSlice(["chat"], (state) =>
    Object.values(state.chat.rooms).reduce((n, room) => n + room.changes.length, 0),
  );
}

/** The newest frame this tab holds for a room, or undefined. */
export function newestChange(live: ChatRoomLive | undefined): ChatChange | undefined {
  return live?.changes[0];
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
 * It is what settles an outgoing message, and it is memoised because
 * [useOutbox] takes it as an effect dependency: rebuilt every render, that
 * effect would run on every render of the screen rather than when a page
 * actually merged.
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
