/**
 * The rooms with news the reader cannot see: the Rooms panel's rows marked
 * calling (unread and not muted) that lie wholly outside its list's view,
 * above it or below it, and how to bring the nearest one in.
 *
 * MEASURED FROM THE ROWS AS DRAWN, so a folded section (which still draws
 * its calling rows) and a filter (which draws only matches) count what the
 * reader would actually find by scrolling. A row partly in view is seen.
 */

import { useLayoutEffect, useState, type RefObject } from "react";

/** The class a room's row carries while it has news the reader has not muted. */
export const CALLING_ROW = "chat-room-calling";

/** How many calling rows lie wholly above and wholly below the list's view. */
export interface Unseen {
  above: number;
  below: number;
}

const NONE: Unseen = { above: 0, below: 0 };

function callingRows(scroller: HTMLElement): HTMLElement[] {
  return [...scroller.querySelectorAll<HTMLElement>(`.${CALLING_ROW}`)].filter(
    (row) => row.getBoundingClientRect().height > 0,
  );
}

/** Counts the calling rows wholly outside `scroller`'s view. */
export function unseenIn(scroller: HTMLElement): Unseen {
  const view = scroller.getBoundingClientRect();
  let above = 0;
  let below = 0;
  for (const row of callingRows(scroller)) {
    const box = row.getBoundingClientRect();
    if (box.bottom <= view.top + 1) above += 1;
    else if (box.top >= view.bottom - 1) below += 1;
  }
  return above === 0 && below === 0 ? NONE : { above, below };
}

/**
 * The nearest calling row hidden on `side`, and the scroll that brings it
 * wholly into view `clear` px inside that edge (clear of the pill that asked):
 * the least the list has to move, so the reader keeps their place.
 */
export function nearestUnseen(
  scroller: HTMLElement,
  side: "above" | "below",
  clear: number,
): { row: HTMLElement; top: number } | null {
  const view = scroller.getBoundingClientRect();
  const rows = callingRows(scroller);
  const hidden =
    side === "below"
      ? rows.find((row) => row.getBoundingClientRect().top >= view.bottom - 1)
      : rows.findLast((row) => row.getBoundingClientRect().bottom <= view.top + 1);
  if (!hidden) return null;
  const box = hidden.getBoundingClientRect();
  const top =
    side === "below"
      ? scroller.scrollTop + (box.bottom - view.bottom) + clear
      : scroller.scrollTop - (view.top - box.top) - clear;
  return { row: hidden, top: Math.max(0, Math.round(top)) };
}

/**
 * What waits outside the list's view, measured again (once a frame at most)
 * on every scroll, every resize, and every change to the rows drawn: a room
 * that gains or loses news, a section folded, a filter typed.
 */
export function useUnseen(scroller: RefObject<HTMLElement | null>): Unseen {
  const [unseen, setUnseen] = useState<Unseen>(NONE);
  useLayoutEffect(() => {
    const el = scroller.current;
    if (!el) return;
    let frame = 0;
    const measure = () => {
      frame = 0;
      const next = unseenIn(el);
      setUnseen((was) => (was.above === next.above && was.below === next.below ? was : next));
    };
    const schedule = () => {
      if (frame === 0) frame = requestAnimationFrame(measure);
    };
    measure();
    el.addEventListener("scroll", schedule, { passive: true });
    const resized = typeof ResizeObserver === "undefined" ? null : new ResizeObserver(schedule);
    resized?.observe(el);
    const changed = new MutationObserver(schedule);
    changed.observe(el, {
      childList: true,
      subtree: true,
      attributes: true,
      attributeFilter: ["class"],
    });
    return () => {
      el.removeEventListener("scroll", schedule);
      resized?.disconnect();
      changed.disconnect();
      if (frame !== 0) cancelAnimationFrame(frame);
    };
  }, [scroller]);
  return unseen;
}
