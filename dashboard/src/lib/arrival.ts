/**
 * Arriving at a section of a screen: the section an address names (`section=`)
 * is brought into view, focused, and marked as the one the reader came for.
 *
 * THE ADDRESS SAYS WHICH, so a link opened in a new tab lands where a press in
 * the drawer does, and Back returns to the screen as it was before.
 *
 * A PRESS ON THE ADDRESS ALREADY OPEN ARRIVES AGAIN. A reader who scrolled
 * away and presses the same link a second time changes no hash, so nothing in
 * the route moves; [arriveAgain] is what such a press calls, and every screen
 * waiting on an arrival reveals its section once more.
 *
 * A SECTION THAT IS NOT DRAWN IS NOT AN ERROR: a seat the screen does not list,
 * or a card the reader's answer leaves out, reveals nothing and the screen is
 * read as it is.
 */

import { useCallback, useEffect, useRef, useState, useSyncExternalStore } from "react";
import { useRoute } from "~/app/router.tsx";
import { reveal } from "./scroller.ts";
import { SECTION_PARAM } from "./remedy.ts";

let arrivals = 0;
const listeners = new Set<() => void>();

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

/** Arrive again at the section the open address names, for a press on that same address. */
export function arriveAgain(): void {
  arrivals += 1;
  for (const listener of listeners) listener();
}

/** The section the open address names, or "" for none. */
export function useArrivedSection(): string {
  return useRoute().query.get(SECTION_PARAM) ?? "";
}

/**
 * The element a section is drawn in, and whether the address names it: a
 * callback ref to put on the section, and `arrived` for its mark (a card's
 * `selected`). Once the section is drawn under an address naming it, it is
 * brought into view and focused, and again on every [arriveAgain].
 *
 * `find` picks the element to reveal inside the section, where that is not the
 * section itself: a table's current row.
 */
export function useSectionArrival(
  section: string,
  find?: (el: HTMLElement) => HTMLElement | null,
): { ref: (el: HTMLElement | null) => void; arrived: boolean } {
  const arrived = useArrivedSection() === section && section !== "";
  const again = useSyncExternalStore(
    subscribe,
    () => arrivals,
    () => 0,
  );
  const [el, setEl] = useState<HTMLElement | null>(null);
  const ref = useCallback((node: HTMLElement | null) => setEl(node), []);
  // READ AT ARRIVAL, and not a reason to arrive again: a screen hands a new
  // function on every render.
  const finder = useRef(find);
  finder.current = find;
  useEffect(() => {
    if (!arrived || !el) return;
    const target = finder.current?.(el) ?? el;
    reveal(target);
    // THE READER CAME FOR THIS SECTION, so a keyboard or a screen reader
    // lands on it; the scroll is [reveal]'s, which leaves a section already
    // in view where it is.
    target.focus({ preventScroll: true });
  }, [arrived, el, again]);
  return { ref, arrived };
}
