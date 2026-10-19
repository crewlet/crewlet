/**
 * Which element scrolls a given one: the nearest ancestor whose overflow
 * scrolls, before the shell's own scroller. A doc's outline measured its
 * headings against the shell while the artifact's body was what moved them,
 * and never left its first entry.
 */

import { afterEach, describe, expect, it } from "vitest";
import { SCREEN_SCROLL_ID, scrollerOf } from "./scroller.ts";

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
