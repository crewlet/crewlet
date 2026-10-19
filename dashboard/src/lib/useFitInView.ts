/**
 * How tall a box may be so that all of it is in view.
 *
 * FOR A BOX THAT MUST BE SEEN WHOLE, such as a slide being presented in the
 * page: the slide is letterboxed into the box, so the box's height decides
 * whether the slide's foot and the controls under it are on screen. No length
 * in a stylesheet can say it, because it is the view's height less whatever
 * the page sets above the box (a title, a description, tabs), and that wraps
 * differently on every window. So it is MEASURED, after layout and again
 * whenever the view or the box is resized.
 *
 * The box asks for `need` (the height it would use at its full width) and is
 * given that or the room from its top to the foot of its scrollport, measured
 * with the scrollport at its top, whichever is less. When that room would
 * shrink it below half of what it asks, it is too low on the page to be read
 * there: it takes the scrollport's whole height instead and is scrolled into
 * view. The box's `scroll-margin-top` and `scroll-margin-bottom` are kept
 * clear, so a sticky bar over the view and the page's own foot are never
 * covered.
 */

import { useLayoutEffect, useState, type RefObject } from "react";

export interface Fit {
  /** The box's height, in CSS pixels. */
  height: number;
  /** Whether the box only fits once it is scrolled into view. */
  scroll: boolean;
}

/** The nearest ancestor that scrolls vertically, or null when the window does. */
export function scrollportOf(el: Element): HTMLElement | null {
  for (let p = el.parentElement; p; p = p.parentElement) {
    if (/^(auto|scroll)$/.test(getComputedStyle(p).overflowY)) return p;
  }
  return null;
}

const px = (value: string) => {
  const n = parseFloat(value);
  return Number.isFinite(n) ? n : 0;
};

export function fitInView(el: HTMLElement, need: number): Fit {
  const port = scrollportOf(el);
  const box = el.getBoundingClientRect();
  // THE BOX'S TOP IN THE SCROLLED CONTENT, so the answer does not move as the
  // reader scrolls.
  const top = port
    ? box.top - port.getBoundingClientRect().top - port.clientTop + port.scrollTop
    : box.top + window.scrollY;
  const view = port ? port.clientHeight : document.documentElement.clientHeight;
  const margins = getComputedStyle(el);
  const above = px(margins.scrollMarginTop);
  const below = px(margins.scrollMarginBottom);
  const room = view - top - below;
  if (room >= need / 2) return { height: Math.max(0, Math.min(need, room)), scroll: false };
  return { height: Math.max(0, Math.min(need, view - above - below)), scroll: true };
}

/**
 * The height `box` may take to be seen whole, given what it asks for at a
 * width; null before it is first measured. A box that only fits once scrolled
 * into view is scrolled there when it is first measured.
 */
export function useFitInView(
  box: RefObject<HTMLElement | null>,
  need: (el: HTMLElement) => number,
): number | null {
  const [height, setHeight] = useState<number | null>(null);
  useLayoutEffect(() => {
    const el = box.current;
    if (!el) return;
    let first = true;
    const measure = () => {
      const fit = fitInView(el, need(el));
      setHeight(Math.floor(fit.height));
      if (first && fit.scroll) el.scrollIntoView({ block: "start" });
      first = false;
    };
    measure();
    const watch = new ResizeObserver(measure);
    watch.observe(el);
    watch.observe(scrollportOf(el) ?? document.documentElement);
    return () => watch.disconnect();
  }, [box, need]);
  return height;
}
