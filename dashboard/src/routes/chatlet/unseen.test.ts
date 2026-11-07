/**
 * The rooms with news the reader cannot see: what is counted as above and
 * below the list's view, and how far the list moves to bring the nearest in.
 */

import { describe, expect, test } from "vitest";

import { CALLING_ROW, nearestUnseen, unseenIn } from "./unseen.ts";

/** A box that reports `top` to `top + height` on screen. */
function boxed<T extends HTMLElement>(el: T, top: number, height: number): T {
  el.getBoundingClientRect = () =>
    ({
      top,
      bottom: top + height,
      height,
      left: 0,
      right: 200,
      width: 200,
      x: 0,
      y: top,
    }) as DOMRect;
  return el;
}

/**
 * A list viewed from 100 to 400 on screen, scrolled 500 down, holding 28px
 * rows at the given screen tops; the calling ones carry the class.
 */
function list(rows: { top: number; calling: boolean }[]): HTMLElement {
  const scroller = boxed(document.createElement("div"), 100, 300);
  Object.defineProperty(scroller, "scrollTop", { value: 500, writable: true });
  for (const { top, calling } of rows) {
    const row = boxed(document.createElement("div"), top, 28);
    if (calling) row.className = CALLING_ROW;
    row.append(document.createElement("a"));
    scroller.append(row);
  }
  return scroller;
}

describe("the rooms with news the reader cannot see", () => {
  test("are those wholly above or below the view; a row partly in view is seen", () => {
    const scroller = list([
      { top: 20, calling: true }, // wholly above
      { top: 72, calling: true }, // bottom edge at the view's top: above
      { top: 90, calling: true }, // partly in view
      { top: 200, calling: true }, // in view
      { top: 380, calling: true }, // partly in view at the foot
      { top: 400, calling: true }, // wholly below
      { top: 450, calling: false }, // below, but nothing calls there
    ]);
    expect(unseenIn(scroller)).toEqual({ above: 2, below: 1 });
  });

  test("a row that is not drawn (no height) is not counted", () => {
    const scroller = list([{ top: 500, calling: true }]);
    boxed(scroller.firstElementChild as HTMLElement, 0, 0);
    expect(unseenIn(scroller)).toEqual({ above: 0, below: 0 });
  });

  test("below, the list moves the least that brings the nearest row in, clear of the pill", () => {
    const scroller = list([
      { top: 420, calling: true },
      { top: 460, calling: true },
    ]);
    const next = nearestUnseen(scroller, "below", 44)!;
    expect(next.row).toBe(scroller.children[0]);
    // Its bottom (448) lands 44px inside the view's bottom (400).
    expect(next.top).toBe(500 + (448 - 400) + 44);
  });

  test("above, the nearest is the last one above the view, and lands clear of the top", () => {
    const scroller = list([
      { top: -200, calling: true },
      { top: 30, calling: true },
      { top: 200, calling: false },
    ]);
    const next = nearestUnseen(scroller, "above", 44)!;
    expect(next.row).toBe(scroller.children[1]);
    expect(next.top).toBe(500 - (100 - 30) - 44);
  });

  test("nothing hidden on that side is nowhere to go", () => {
    const scroller = list([{ top: 200, calling: true }]);
    expect(nearestUnseen(scroller, "below", 44)).toBeNull();
    expect(nearestUnseen(scroller, "above", 44)).toBeNull();
  });
});
