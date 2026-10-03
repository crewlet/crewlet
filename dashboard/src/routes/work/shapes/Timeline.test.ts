import { expect, test } from "vitest";
import { asideAt, barBox } from "./Timeline.tsx";

// A NARROW BAR'S LABEL NEVER RUNS OFF THE STRIP. Written after a bar four days
// from the end, it was cut mid-word at the strip's edge ("GPU schedu") with
// nothing to say there was more; a label is placed on the side that has room
// and bounded by that room, so the sheet's ellipsis is what shortens it.
test("a label goes after its bar where there is room, and is bounded by it", () => {
  // A 60-day strip at 26px a day, a bar on days 10–11: all the room after it.
  const at = asideAt(10, 11, 60);
  expect(at.left).toBe(11 * 26 + 4);
  expect(at.right).toBeUndefined();
  expect(at.maxWidth).toBe(49 * 26 - 8);
});

test("a label near the strip's end goes before its bar", () => {
  // Days 56–57 of 60: 78px after it, 1456px before it.
  const at = asideAt(56, 57, 60);
  expect(at.left).toBeUndefined();
  expect(at.right).toBe(4 * 26 + 4);
  expect(at.maxWidth).toBe(56 * 26 - 8);
});

// A STRIP TOO SHORT FOR EITHER SIDE keeps the side with more, never a negative
// width.
test("a cramped label takes the roomier side and never a negative bound", () => {
  const late = asideAt(3, 4, 5);
  expect(late.right).toBe(2 * 26 + 4);
  expect(late.maxWidth).toBe(3 * 26 - 8);
  const edge = asideAt(0, 1, 1);
  expect(edge.maxWidth).toBe(0);
});

// A DEADLINE IS A POINT, drawn as one: a milestone centred on its day rather
// than a one-day box dashed on every side, which read as an empty placeholder
// over the one date the timeline knows for certain. Every other kind covers
// the days it spans.
test("a deadline is a milestone centred on its day; a span covers its days", () => {
  const milestone = barBox({ from: 10, to: 11, kind: "deadline" }, 2);
  expect(milestone.width).toBe(12);
  expect(milestone.left + milestone.width / 2).toBe(10 * 26 + 13);
  expect(milestone.top).toBe(2 * 30 + 4);
  const span = barBox({ from: 10, to: 11, kind: "span" }, 0);
  expect(span).toEqual({ left: 260, width: 26, top: 4 });
  expect(barBox({ from: 3, to: 7, kind: "began" }, 0).width).toBe(4 * 26);
});
