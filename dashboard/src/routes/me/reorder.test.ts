// @vitest-environment node

/**
 * A reorder is a new whole list made from the STORED one, where everything
 * the reader cannot see keeps its place around the entry that moved.
 */

import { expect, test } from "vitest";
import { dropPlace, moveTo, sameOrder, stepPlace } from "./reorder.ts";

/** Three drawn entries with a finished one between them and one past the page. */
const STORED = ["a", "done", "b", "c", "late"];
const DRAWN = ["a", "b", "c"];

// THE HIDDEN ENTRIES KEEP THEIR PLACES. The drawn rows are the open entries up
// to a page; a move written over them alone would drop `done` and `late`.
test("a move keeps every entry the reader cannot see where it was", () => {
  expect(moveTo(STORED, "c", { before: "b" })).toEqual(["a", "done", "c", "b", "late"]);
  expect(moveTo(STORED, "a", { after: "c" })).toEqual(["done", "b", "c", "a", "late"]);
});

// A LIST NAMING ONE ENTRY TWICE — a record written whole by a path that does
// not deduplicate — comes out naming it once, at the place chosen.
test("a doubled entry is moved once and kept once", () => {
  expect(moveTo(["a", "b", "a", "c"], "a", { after: "c" })).toEqual(["b", "c", "a"]);
});

// NULL, NEVER THE LIST AS IT WAS: a move naming something the stored list does
// not hold is a screen out of step with the record, and the answer is to read
// again rather than to write the old order back.
test("a move naming what the list does not hold is no move", () => {
  expect(moveTo(STORED, "x", { before: "a" })).toBeNull();
  expect(moveTo(STORED, "a", { before: "x" })).toBeNull();
  expect(moveTo(STORED, "a", { before: "a" })).toBeNull();
});

// A DROP ONTO ITSELF, or onto the gap directly under itself, is where it
// already is — a write for it would be a reorder that changed nothing.
test("a drop where the row already is moves nothing", () => {
  expect(dropPlace(DRAWN, "b", "b")).toBeNull();
  expect(dropPlace(DRAWN, "b", "c")).toBeNull();
  expect(dropPlace(DRAWN, "c", null)).toBeNull();
});

test("a drop above a row lands before it, and at the foot after the last drawn", () => {
  expect(dropPlace(DRAWN, "c", "a")).toEqual({ before: "a" });
  // AFTER THE LAST ROW DRAWN, not the end of the stored list: `late` is not
  // drawn, and a row sent behind it would vanish from the list.
  expect(dropPlace(DRAWN, "a", null)).toEqual({ after: "c" });
  expect(moveTo(STORED, "a", dropPlace(DRAWN, "a", null)!)).toEqual([
    "done",
    "b",
    "c",
    "a",
    "late",
  ]);
});

test("a step from the keyboard is one drawn place, and nothing past either end", () => {
  expect(stepPlace(DRAWN, "b", true)).toEqual({ before: "a" });
  expect(stepPlace(DRAWN, "b", false)).toEqual({ after: "c" });
  expect(stepPlace(DRAWN, "a", true)).toBeNull();
  expect(stepPlace(DRAWN, "c", false)).toBeNull();
});

test("the same entries in the same order are the same order", () => {
  expect(sameOrder(["a", "b"], ["a", "b"])).toBe(true);
  expect(sameOrder(["a", "b"], ["b", "a"])).toBe(false);
  expect(sameOrder(["a"], ["a", "b"])).toBe(false);
});
