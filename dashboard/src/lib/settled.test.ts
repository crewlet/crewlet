/**
 * The rule that keeps a live list readable: the page moves only when the
 * reader is not reading.
 *
 * Every case here is a way that rule was broken in practice — a row held back
 * that the reader had already been shown, or a row let through that shoved the
 * paragraph they were mid-sentence in down the page.
 */

import { renderHook } from "@testing-library/react";
import { afterEach, describe, expect, test } from "vitest";
import { TOP_SLACK_PX, useSettled } from "./settled.ts";

interface Row {
  id: string;
}

const keyOf = (row: Row) => row.id;

/**
 * A scroller at a given offset.
 *
 * The hook reads `.screen` off the document, because the scroller is the shell's
 * and not any one screen's. Absent, it treats the reader as being at the top,
 * which is what a test that does not care about scrolling gets.
 */
function scroller(top: number): void {
  const el = document.createElement("div");
  el.className = "screen";
  Object.defineProperty(el, "scrollTop", { value: top, writable: true });
  el.scrollTo = () => {};
  document.body.append(el);
}

afterEach(() => {
  document.body.innerHTML = "";
});

describe("holding rows back", () => {
  test("what is on screen at mount is admitted", () => {
    const { result } = renderHook(() => useSettled([{ id: "a" }, { id: "b" }], keyOf));
    expect(result.current.items.map(keyOf)).toEqual(["a", "b"]);
    expect(result.current.pending).toBe(0);
  });

  test("a reader scrolled down does not get rows spliced in above them", () => {
    scroller(TOP_SLACK_PX + 1);
    const rows = [{ id: "a" }];
    const { result, rerender } = renderHook(({ items }) => useSettled(items, keyOf), {
      initialProps: { items: rows },
    });
    rerender({ items: [{ id: "new" }, ...rows] });
    expect(result.current.items.map(keyOf)).toEqual(["a"]);
    expect(result.current.pending).toBe(1);
  });

  test("a reader at the top gets them straight away", () => {
    // Holding rows back from somebody watching the feed would look broken.
    scroller(0);
    const rows = [{ id: "a" }];
    const { result, rerender } = renderHook(({ items }) => useSettled(items, keyOf), {
      initialProps: { items: rows },
    });
    rerender({ items: [{ id: "new" }, ...rows] });
    expect(result.current.items.map(keyOf)).toEqual(["new", "a"]);
    expect(result.current.pending).toBe(0);
  });
});

describe("a row that was already on screen elsewhere", () => {
  // THE TURN THAT DISAPPEARED, second half. A running turn lives in its own
  // region above the settled list; when it finishes it leaves that region and
  // arrives here. Keyed on identity alone, this list had never admitted it, so
  // a reader scrolled into the transcript they were following had it replaced
  // by "1 new turn finished while you were reading — show".

  test("a row moving in from the live region is not held back", () => {
    scroller(TOP_SLACK_PX + 1);
    const { result, rerender } = renderHook(
      ({ items, live }: { items: Row[]; live: string[] }) => useSettled(items, keyOf, live),
      { initialProps: { items: [{ id: "old" }], live: ["running"] } },
    );
    // It completes: out of the live list, into this one.
    rerender({ items: [{ id: "running" }, { id: "old" }], live: [] });
    expect(result.current.items.map(keyOf)).toEqual(["running", "old"]);
    expect(result.current.pending).toBe(0);
  });

  test("a row that was never on screen is still held back", () => {
    // The exemption is for what the reader has SEEN, not a way around the rule.
    scroller(TOP_SLACK_PX + 1);
    const { result, rerender } = renderHook(
      ({ items, live }: { items: Row[]; live: string[] }) => useSettled(items, keyOf, live),
      { initialProps: { items: [{ id: "old" }], live: ["running"] } },
    );
    rerender({ items: [{ id: "someone-elses" }, { id: "old" }], live: ["running"] });
    expect(result.current.items.map(keyOf)).toEqual(["old"]);
    expect(result.current.pending).toBe(1);
  });
});
