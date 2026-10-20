/**
 * The reader's artifact pins: one listing every surface reads, and one signal
 * that a press landed. The press itself is `pinToggle.ts`.
 *
 * A pin is the reader's own record, kept by the engine across devices. It is
 * drawn in three places at once: the sidebar's Pinned group, the head of All,
 * and the Pin item of a row's or a page's menu. A press in one of them used
 * to leave the other two showing the state from before it until their next
 * poll, so a pinned row sat in the main list unmarked for a minute and the
 * sidebar promised by the menu never moved. Every press now goes through
 * `usePinToggle`, which tells every reader of [usePinsChanged] to ask again.
 *
 * THE PRESS IS A MODULE OF ITS OWN because the sidebar reads this one, so
 * this one is in the entry, and a bundler places a module whole: with the
 * press here, its write and every artifact REST call beside it in
 * `api.ts` rode every reader's first screen, although only an artifact's
 * screens ever press.
 */

import { useEffect, useRef } from "react";
import { useQuery } from "../useQuery.ts";

/** The most pins the sidebar lists, and the page's own check reads. */
export const PINS_LIMIT = 200;

/** What a pin list is asked as: every artifact the reader can open that they pinned. */
export const PINS_QUERY = { scope: "all", pinned: true, limit: PINS_LIMIT } as const;

const listeners = new Set<() => void>();

/** Tells every pin reader a pin changed. */
export function announcePinsChanged(): void {
  for (const listener of [...listeners]) listener();
}

/** Calls `refetch` whenever a pin lands anywhere in this tab. */
export function usePinsChanged(refetch: () => void): void {
  const latest = useRef(refetch);
  latest.current = refetch;
  useEffect(() => {
    const listener = () => latest.current();
    listeners.add(listener);
    return () => void listeners.delete(listener);
  }, []);
}

/** The reader's pins, kept current by every press in this tab. */
export function usePins(enabled = true) {
  const pins = useQuery("artifacts", PINS_QUERY, { enabled, pollMs: 120_000 });
  usePinsChanged(pins.refetch);
  return pins;
}
