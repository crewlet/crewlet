/**
 * The room a box is given to be seen whole: what it asks for, or the room
 * from its top to the foot of its scrollport, or the whole scrollport once
 * scrolled to when it sits too low to be read where it is.
 */

import { afterEach, expect, test, vi } from "vitest";

import { fitInView, scrollportOf } from "./useFitInView.ts";

afterEach(() => {
  document.body.replaceChildren();
  vi.restoreAllMocks();
});

/** A box `top` pixels down a scrollport `view` pixels tall, scrolled by `scrolled`. */
function laidOut(top: number, view: number, scrolled = 0) {
  const port = document.createElement("div");
  port.style.overflowY = "auto";
  const box = document.createElement("div");
  box.style.scrollMarginTop = "48px";
  box.style.scrollMarginBottom = "16px";
  port.append(box);
  document.body.append(port);
  Object.defineProperty(port, "clientHeight", { value: view });
  Object.defineProperty(port, "scrollTop", { value: scrolled });
  port.getBoundingClientRect = () => new DOMRect(0, 10, 0, view);
  box.getBoundingClientRect = () => new DOMRect(0, 10 + top - scrolled, 0, 0);
  return { port, box };
}

test("the scrollport is the nearest ancestor that scrolls, or the window", () => {
  const { port, box } = laidOut(0, 800);
  expect(scrollportOf(box)).toBe(port);
  const loose = document.createElement("div");
  document.body.append(loose);
  expect(scrollportOf(loose)).toBeNull();
});

test("a box gets what it asks for, or the room below its top less its bottom margin", () => {
  expect(fitInView(laidOut(300, 800).box, 400)).toEqual({ height: 400, scroll: false });
  expect(fitInView(laidOut(300, 800).box, 600)).toEqual({ height: 484, scroll: false });
});

test("the room is measured with the scrollport at its top, so scrolling does not change it", () => {
  expect(fitInView(laidOut(300, 800, 250).box, 600)).toEqual({ height: 484, scroll: false });
});

test("a box too low to get half of what it asks takes the whole view, scrolled to", () => {
  expect(fitInView(laidOut(700, 800).box, 600)).toEqual({ height: 600, scroll: true });
  expect(fitInView(laidOut(700, 500).box, 600)).toEqual({ height: 436, scroll: true });
});
