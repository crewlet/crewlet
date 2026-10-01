/**
 * A new answer is made of the old answer's objects wherever it says the same.
 *
 * The invariant every grid in this product leans on: a row is drawn again
 * only when the object it is drawn from is a different object, so an answer
 * that brought a row back unchanged must hand back the row it had. These
 * cases hold the walk to that, and to the one thing it may never do — say
 * something other than what the new answer says.
 */

import { createElement } from "react";
import { expect, test } from "vitest";

import { share } from "./share.ts";

/** A row as an answer carries it: parsed afresh off the wire each time. */
function row(id: string, summary = `summary ${id}`) {
  return { id, summary, labels: ["a", "b"], meta: { phases: 2, failed: false } };
}

/** The same value, as new objects — what a poll hands over. */
function reparsed<T>(value: T): T {
  return JSON.parse(JSON.stringify(value)) as T;
}

test("an answer that changed nothing is the answer already held", () => {
  const before = { rows: [row("t-1"), row("t-2"), row("t-3")], next_cursor: "", complete: true };
  const after = reparsed(before);
  expect(share(before, after)).toBe(before);
});

test("an answer that moved one row keeps every other row's object", () => {
  const before = { rows: [row("t-1"), row("t-2"), row("t-3")], complete: true };
  const after = reparsed(before);
  after.rows[1]!.summary = "changed";

  const kept = share(before, after);
  expect(kept).toEqual(after);
  // A NEW ANSWER AND A NEW LIST, because something in them moved…
  expect(kept).not.toBe(before);
  expect(kept.rows).not.toBe(before.rows);
  // …and the rows that did not move are the rows that were drawn.
  expect(kept.rows[0]).toBe(before.rows[0]);
  expect(kept.rows[2]).toBe(before.rows[2]);
  // THE ROW THAT MOVED IS NEW, and what inside it did not move is still old.
  expect(kept.rows[1]).not.toBe(before.rows[1]);
  expect(kept.rows[1]!.labels).toBe(before.rows[1]!.labels);
  expect(kept.rows[1]!.meta).toBe(before.rows[1]!.meta);
});

// A FEED IS NEWEST FIRST. One new row at the top moves every other row down a
// place, so compared index by index nothing matches and every row is drawn
// again for an answer that changed by one.
test("a new row at the top keeps every row it pushed down", () => {
  const before = [row("t-1"), row("t-2"), row("t-3")];
  const after = reparsed([row("t-0"), ...before]);

  const kept = share(before, after);
  expect(kept).toEqual(after);
  expect(kept[1]).toBe(before[0]);
  expect(kept[2]).toBe(before[1]);
  expect(kept[3]).toBe(before[2]);
  expect(before).not.toContain(kept[0]);
});

test("a row that left takes nothing with it but itself", () => {
  const before = [row("t-1"), row("t-2"), row("t-3")];
  const after = reparsed([before[0], before[2]]);

  const kept = share(before, after);
  expect(kept).toEqual(after);
  expect(kept).not.toBe(before);
  expect(kept[0]).toBe(before[0]);
  expect(kept[1]).toBe(before[2]);
});

// THE SAME KEYS, NOT MERELY THE SAME VALUES. An object that lost a field is a
// different answer even where every field it kept is unchanged.
test("an object that lost a field is a new object", () => {
  const before = { id: "t-1", summary: "x", task_id: "ENG-1" };
  const after: Record<string, unknown> = { id: "t-1", summary: "x" };
  const kept = share(before, after);
  expect(kept).toEqual(after);
  expect(kept).not.toBe(before);
  expect("task_id" in kept).toBe(false);
});

test("a value is kept only where it is the same value", () => {
  expect(share(Number.NaN, Number.NaN)).toBeNaN();
  // -0 IS NOT 0 TO `Object.is`, and a number cell may well draw the sign.
  expect(Object.is(share(0, -0), -0)).toBe(true);
  expect(share({ n: 1 }, { n: "1" })).toEqual({ n: "1" });
  expect(share([1, 2], { 0: 1, 1: 2 })).toEqual({ 0: 1, 1: 2 });
  expect(share(null, { a: 1 })).toEqual({ a: 1 });
});

// ONLY DATA IS WALKED. A Map or a Date is kept where it is the SAME object and
// never compared by contents; a React element is an object literal whose
// owner is a fiber reaching the whole tree, so walking one would be a walk of
// the application — and a row a screen derives may carry one.
test("what is not data is never looked inside", () => {
  const map = new Map([["a", 1]]);
  const kept = share({ m: map }, { m: new Map([["a", 1]]) });
  expect(kept.m).not.toBe(map);

  const element = createElement("span", null, "x");
  const before = [{ id: "t-1", cell: element }];
  const after = [{ id: "t-1", cell: createElement("span", null, "x") }];
  const shared = share(before, after);
  expect(shared[0]).not.toBe(before[0]);
  expect(shared[0]!.cell).toBe(after[0]!.cell);
  // AND THE SAME ELEMENT IS STILL THE SAME VALUE.
  expect(share(before, [{ id: "t-1", cell: element }])).toBe(before);
});

// TWO ROWS THAT PRINT ALIKE ARE TOLD APART BY THE COMPARISON, not by the
// fingerprint: a bucket is where the match is looked for, not the verdict.
test("a row reused from elsewhere is one that is deep-equal, never one that merely looks it", () => {
  const before = [
    { id: "a", n: 1 },
    { id: "b", n: 2 },
  ];
  const after = [
    { id: "c", n: 3 },
    { id: "a", n: 1 },
    { id: "b", n: 2 },
    { id: "b", n: 2 },
  ];
  const kept = share(before, after);
  expect(kept).toEqual(after);
  expect(kept[1]).toBe(before[0]);
  expect(kept[2]).toBe(before[1]);
  // A DUPLICATE IS THE SAME VALUE TWICE, and one object can be both.
  expect(kept[3]).toBe(before[1]);

  // AND A PRINT IS SHALLOW: a row whose difference is inside a nested object,
  // or past the fields a print reads, lands in the same bucket as the row it
  // differs from — and is still not that row.
  const nested = [{ id: "a", meta: { phases: 1 } }];
  const moved = share(nested, [
    { id: "z", meta: { phases: 0 } },
    { id: "a", meta: { phases: 2 } },
  ]);
  expect(moved).toEqual([
    { id: "z", meta: { phases: 0 } },
    { id: "a", meta: { phases: 2 } },
  ]);
  expect(moved[1]).not.toBe(nested[0]);
  const long = "x".repeat(200);
  const tail = [{ id: "a", body: `${long}1` }];
  const sharedTail = share(tail, [
    { id: "z", body: "" },
    { id: "a", body: `${long}2` },
  ]);
  expect(sharedTail[1]).not.toBe(tail[0]);
  expect(sharedTail[1]!.body).toBe(`${long}2`);
});

test("neither argument is changed", () => {
  const before = { rows: [row("t-1"), row("t-2")] };
  const after = reparsed({ rows: [row("t-0"), row("t-1"), row("t-2", "moved")] });
  const beforeCopy = reparsed(before);
  const afterCopy = reparsed(after);
  share(before, after);
  expect(before).toEqual(beforeCopy);
  expect(after).toEqual(afterCopy);
});
