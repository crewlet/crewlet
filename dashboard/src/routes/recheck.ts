/**
 * Reading again after asking the engine for something.
 *
 * A WRITE RETURNS BEFORE ITS EFFECT IS VISIBLE. The config surface answers as
 * soon as the revision is stored and activated, and the read that follows can
 * land before the engine has applied it, so the first answer is honestly the
 * old one. An operator pressed Connect, the card said Connect, and nothing
 * moved until they refreshed the page by hand.
 *
 * The screen's own quick cadence could not cover it either: that is derived
 * from the rows, and a surface being connected for the first time has no row
 * to derive anything from. So a write starts its own window. It is the one
 * thing this screen knows that the rows do not.
 */

import { useCallback, useEffect, useRef, useState } from "react";

/** How soon the second read goes out, once the apply has had a moment. */
export const SETTLE_MS = 700;

/**
 * How long the quick cadence is held afterwards.
 *
 * BOUNDED, because a change that never shows up is a fault to read about
 * rather than a reason to poll for ever. Long enough to cover a reconcile
 * tick (fifteen seconds) so the first real phase arrives inside the window.
 */
export const WATCH_MS = 20_000;

/**
 * useRecheck returns whether to keep looking, and the call that starts it.
 *
 * `watch()` reads at once, again once the apply has had a moment, and holds
 * the quick cadence for a while. Every timer it starts is cleared when the
 * screen goes away: a poll that outlives its screen is a request nobody will
 * ever read.
 *
 * A SECOND watch() SUPERSEDES THE FIRST. The window is one boolean with no
 * generation, so an earlier timer firing set it false while a later watch was
 * still holding it — the FIRST timer to fire ended the window rather than the
 * last, and the quick cadence stopped early for the most recent write, which
 * is the one an operator is watching. The handles were also only ever pushed,
 * so a screen an operator worked in for a while accumulated dead ones for the
 * life of the mount. Clearing before arming fixes both.
 */
export function useRecheck(read: () => void): { watching: boolean; watch: () => void } {
  const [watching, setWatching] = useState(false);
  const timers = useRef<ReturnType<typeof setTimeout>[]>([]);

  const clear = useCallback(() => {
    for (const t of timers.current) clearTimeout(t);
    timers.current = [];
  }, []);

  useEffect(() => clear, [clear]);

  const watch = useCallback(() => {
    clear();
    read();
    setWatching(true);
    timers.current = [
      setTimeout(() => read(), SETTLE_MS),
      setTimeout(() => setWatching(false), WATCH_MS),
    ];
  }, [clear, read]);

  return { watching, watch };
}
