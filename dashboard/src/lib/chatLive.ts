/**
 * The company's chat as the live channel reports it, read by the FRAME as well
 * as by Chatlet's screens: how much has happened, this person's own read
 * state, and the one rate every re-read of a chat answer is held to.
 *
 * IN `lib/`, NOT IN THE SCREEN'S `live.ts`, because the frame reads them: the
 * sidebar's Chatlet badge is counted from the reader's rooms on every page
 * (`lib/chatRooms.ts`), and the frame may not import a screen's module (it
 * would carry the whole workspace into the first paint). The screen's own
 * live half (`routes/chatlet/live.ts`) builds on these.
 */

import { useEffect, useRef } from "react";

import { useSlice } from "~/lib/store-hooks.ts";
import type { ChatReadState } from "~/protocol/index.ts";

/**
 * How long live frames pile up before the screen re-reads the room.
 *
 * A THIRD OF A SECOND, and it is a rate limit rather than a delay budget: a
 * room in the middle of a conversation produces a burst of frames (a post, a
 * reaction, an edit, three more posts), and one read per frame would put a
 * query per message on the socket, which is exactly the load the frames exist
 * to avoid. Under this window a burst is ONE read.
 *
 * The first frame is not delayed at all (see [useCoalesced]): a single message
 * in a quiet room appears as fast as the round trip allows, and only a second
 * one inside the window waits, for at most this long.
 */
export const LIVE_REFETCH_MS = 350;

/**
 * How much has happened in the company's chat since this tab connected, as one
 * number.
 *
 * FOR THE RAIL, which cares that SOMETHING landed and not what: a message in
 * any room changes the rail's order, its preview and its badge, and the rail
 * has no way to learn any of those from a frame: the counts are the
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
 * trailing one the last frame of a burst (which is the message somebody just
 * sent) is the one that never causes a read.
 *
 * ONLY THE SIGNAL IS A CHANGE. The window is read when the signal moves, so a
 * caller whose rate changes (the frame's reading of the rooms, at the room's
 * rate while a screen draws them and the badge's otherwise) re-reads nothing
 * for the change itself.
 */
export function useCoalesced(signal: number, task: () => void, window = LIVE_REFETCH_MS): void {
  const latest = useRef(task);
  latest.current = task;
  const span = useRef(window);
  span.current = window;
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
    if (since >= span.current) {
      ranAt.current = Date.now();
      latest.current();
      return;
    }
    if (timer.current) return;
    timer.current = setTimeout(() => {
      timer.current = 0;
      ranAt.current = Date.now();
      latest.current();
    }, span.current - since);
  }, [signal]);

  useEffect(() => () => clearTimeout(timer.current), []);
}
