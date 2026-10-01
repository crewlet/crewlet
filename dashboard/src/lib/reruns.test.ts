/**
 * Which triggers ran more than once — the count three tables mark a re-run by.
 */

import { expect, test } from "vitest";

import { rerunCounts, runsOf } from "./reruns.ts";

test("only a trigger that ran more than once is counted", () => {
  const counts = rerunCounts([
    { work_key: "a" },
    { work_key: "b" },
    { work_key: "a" },
    { work_key: "a" },
  ]);
  expect(counts).toEqual({ a: 3 });
  expect(runsOf(counts, "a")).toBe(3);
  expect(runsOf(counts, "b")).toBe(0);
});

// A NEW TURN IS A NEW KEY, and a record that held every key was a new record
// for every turn that arrived — which is what moved the column lists closing
// over it, and drew every row of the turns list for a turn that re-ran nothing.
test("a turn that re-ran nothing leaves the counts as they were", () => {
  const before = rerunCounts([{ work_key: "a" }, { work_key: "a" }]);
  const after = rerunCounts([{ work_key: "c" }, { work_key: "a" }, { work_key: "a" }]);
  expect(after).toEqual(before);
});

test("an empty key is no identity, and an inherited name is no count", () => {
  const counts = rerunCounts([{ work_key: "" }, { work_key: "" }, {}, {}]);
  expect(counts).toEqual({});
  expect(runsOf(counts, "toString")).toBe(0);
  expect(runsOf(counts, "constructor")).toBe(0);
  expect(runsOf(counts, undefined)).toBe(0);
});
