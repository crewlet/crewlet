/**
 * The rail keeps the reader's own destination in view.
 *
 * On a phone the rail is a bottom bar whose eight rows scroll sideways
 * (frame.css, 860px), and it shares its row with the engine's foot. A reader
 * on Activity found the Activity row cut at the scroller's edge as "Ac" — the
 * one row a navigation bar must never hide. jsdom computes no layout, so the
 * geometry is stated here as the box sizes a 390px bar measured, and what is
 * held is the arithmetic that brings the row inside the strip.
 */

import { cleanup, render } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { AppRail, revealInline } from "./AppRail.tsx";

afterEach(cleanup);

/** A box of the given geometry, in the strip's own coordinates. */
function boxed(el: HTMLElement, left: number, width: number) {
  el.getBoundingClientRect = () =>
    ({ left, width, right: left + width, top: 0, bottom: 0, height: 0, x: left, y: 0 }) as DOMRect;
}

function strip(scrollWidth: number, clientWidth: number): HTMLElement {
  const el = document.createElement("div");
  Object.defineProperty(el, "scrollWidth", { value: scrollWidth });
  Object.defineProperty(el, "clientWidth", { value: clientWidth });
  boxed(el, 0, clientWidth);
  return el;
}

test("a row past the strip's right edge is scrolled wholly into view", () => {
  // Measured at 390px: the rows' strip was 320px wide over 448px of rows, and
  // Activity sat at 294–350.
  const rows = strip(448, 320);
  const row = document.createElement("a");
  boxed(row, 294, 56);
  revealInline(rows, row);
  expect(rows.scrollLeft).toBe(350 - 320);
});

test("a row before the strip's left edge is scrolled back to it", () => {
  const rows = strip(448, 320);
  rows.scrollLeft = 100;
  const row = document.createElement("a");
  // 20px in content coordinates is -80 on screen once scrolled by 100.
  boxed(row, -80, 56);
  revealInline(rows, row);
  expect(rows.scrollLeft).toBe(20);
});

test("a strip that does not overflow sideways is left alone", () => {
  // The desktop column: the rows scroll vertically if at all, and a sideways
  // reveal there would move nothing a reader asked to move.
  const rows = strip(80, 80);
  rows.scrollLeft = 0;
  const row = document.createElement("a");
  boxed(row, 500, 56);
  revealInline(rows, row);
  expect(rows.scrollLeft).toBe(0);
});

test("the rail marks the reader's workspace as the current page", () => {
  const { container } = render(<AppRail active="activity" collapsed={false} onToggle={() => {}} />);
  const current = container.querySelector('.rail-row[aria-current="page"]');
  expect(current?.classList.contains("active")).toBe(true);
  expect(current?.textContent).toContain("Activity");
});
