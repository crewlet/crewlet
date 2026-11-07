/**
 * Which element scrolls a given one: the nearest ancestor whose overflow
 * scrolls, before the shell's own scroller. A doc's outline measured its
 * headings against the shell while the artifact's body was what moved them,
 * and never left its first entry.
 */

import { afterEach, describe, expect, it } from "vitest";
import { holdScroll, SCREEN_SCROLL_ID, scrollerOf } from "./scroller.ts";

afterEach(() => {
  document.body.innerHTML = "";
});

describe("scrollerOf", () => {
  it("answers the nearest ancestor that scrolls, inside the shell's scroller", () => {
    document.body.innerHTML = `
      <main id="${SCREEN_SCROLL_ID}" style="overflow-y: auto">
        <div id="body" style="overflow-y: auto">
          <article><h2 id="first">First</h2></article>
        </div>
      </main>`;
    expect(scrollerOf(document.getElementById("first"))).toBe(document.getElementById("body"));
  });

  it("passes over an ancestor whose overflow does not scroll", () => {
    document.body.innerHTML = `
      <main id="${SCREEN_SCROLL_ID}" style="overflow-y: auto">
        <div style="overflow-y: hidden"><h2 id="first">First</h2></div>
      </main>`;
    expect(scrollerOf(document.getElementById("first"))).toBe(
      document.getElementById(SCREEN_SCROLL_ID),
    );
  });

  it("answers the shell's scroller where nothing nearer scrolls, and for no element", () => {
    document.body.innerHTML = `<main id="${SCREEN_SCROLL_ID}"></main><h2 id="first">First</h2>`;
    const shell = document.getElementById(SCREEN_SCROLL_ID);
    expect(scrollerOf(document.getElementById("first"))).toBe(shell);
    expect(scrollerOf(null)).toBe(shell);
  });
});

describe("holdScroll", () => {
  /** A scroller whose scroll is clamped to `room()`, as a page that is still filling clamps it. */
  function scroller(room: () => number): { el: HTMLElement; top: () => number } {
    const el = document.createElement("div");
    let top = 0;
    Object.defineProperty(el, "scrollTop", {
      configurable: true,
      get: () => top,
      set: (v: number) => {
        top = Math.min(v, room());
      },
    });
    document.body.append(el);
    return { el, top: () => top };
  }
  const frames = (n: number) =>
    new Promise<void>((resolve) => {
      const step = (left: number) =>
        left === 0 ? resolve() : requestAnimationFrame(() => step(left - 1));
      step(n);
    });

  // A PAGE THAT FILLS LATE: the scroll is clamped at first, and lands once
  // there is room. Mutation: assign once, and it stays clamped.
  it("re-applies the place until the page holds it", async () => {
    let room = 100;
    const s = scroller(() => room);
    holdScroll(s.el, () => 400);
    await frames(3);
    expect(s.top()).toBe(100);
    room = 1000;
    await frames(5);
    expect(s.top()).toBe(400);
  });

  // THE PAGE IS THE READER'S once they touch it, and nothing is held for a
  // place that has gone.
  it("ends at the reader's first touch, and when its place is gone", async () => {
    const touched = scroller(() => 0);
    holdScroll(touched.el, () => 400);
    touched.el.dispatchEvent(new Event("pointerdown"));
    Object.defineProperty(touched.el, "scrollTop", {
      configurable: true,
      value: 7,
      writable: true,
    });
    await frames(3);
    expect(touched.el.scrollTop).toBe(7);

    const s = scroller(() => 1000);
    let place: number | null = 300;
    holdScroll(s.el, () => place);
    await frames(1);
    place = null;
    s.el.scrollTop = 50;
    await frames(3);
    expect(s.top()).toBe(50);
  });
});
