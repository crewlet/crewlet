/**
 * Whether a media query matches, as a value that re-renders when it changes.
 *
 * For the one layout decision a stylesheet cannot make on its own — where the
 * peek becomes a column, which turns on the screen's shape and the reader's
 * density as well as on the window (`app/layout.ts`) — and for nothing a
 * media query in a stylesheet CAN decide: a width written in both places is
 * a width that can disagree with itself.
 *
 * `useSyncExternalStore` rather than an effect, so the first render already
 * has the answer: a layout decided in an effect paints the wrong shape for a
 * frame and then moves.
 */

import { useCallback, useSyncExternalStore } from "react";
import { PHONE_BREAKPOINT } from "~/app/layout.ts";

export function useMediaQuery(query: string): boolean {
  const subscribe = useCallback(
    (notify: () => void) => {
      const list = matchMedia(query);
      list.addEventListener("change", notify);
      return () => list.removeEventListener("change", notify);
    },
    [query],
  );
  return useSyncExternalStore(
    subscribe,
    () => matchMedia(query).matches,
    // Never rendered outside a browser; a hook that cannot answer a server
    // snapshot throws in any environment that asks for one.
    () => false,
  );
}

/**
 * Which edge of its trigger a panel lines up with: the caller's choice, and
 * `start` on a phone.
 *
 * ON A PHONE A PANEL IS AS WIDE AS THE WINDOW, so it has no room to hang left
 * of an end-aligned trigger: the kit slides a start-aligned panel back inside
 * the window's edge, which is where it has to be anyway.
 *
 * AND THE KIT CANNOT DRAW IT END-ALIGNED THERE AT ALL. `@crewlethq/ui` 0.5.0's
 * `Popover` and `Menu` ask whether the anchor has left the layer using the
 * END-ALIGNED PANEL's left edge with the ANCHOR's width, so a panel wider than
 * its trigger's right edge plus the trigger's own width reads as an anchor
 * carried off screen and closes on the frame it opened — which is every
 * end-aligned panel whose trigger a phone's toolbar wraps to the left (the
 * work list's Display, at x=12). Raised against uilet; until the kit measures
 * the anchor, a panel that can open near the left edge is start-aligned.
 */
export function usePanelAlign(preferred: "start" | "end"): "start" | "end" {
  const phone = useMediaQuery(`(width < ${PHONE_BREAKPOINT}px)`);
  return phone ? "start" : preferred;
}
