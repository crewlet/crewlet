/**
 * A cap's change as values: what a typed value is read as for each unit, the
 * patch that writes or removes its key, and that a change is worked out
 * against the document as it is, not against what a page drew.
 */

import { expect, test } from "vitest";
import {
  documentLimit,
  limitPatch,
  limitPlan,
  limitText,
  readLimit,
  type LimitRow,
} from "./limits.ts";

function row(over: Partial<LimitRow> & Pick<LimitRow, "key">): LimitRow {
  return {
    group: "turn",
    unit: "count",
    value: 3,
    default: 3,
    off: false,
    default_off: false,
    settable: true,
    ...over,
  };
}

const SHARE = row({
  key: "turn_engine.delegation.budget_fraction",
  group: "delegation",
  unit: "fraction",
  value: 0.2,
  default: 0.2,
});

test("a share is typed and shown as a percentage", () => {
  expect(limitText(SHARE, 0.35)).toBe("35");
  expect(readLimit(SHARE, "35")).toEqual({ ok: true, value: 0.35 });
  expect(readLimit(SHARE, " 12.5% ")).toEqual({ ok: true, value: 0.125 });
});

test("a count takes a whole number and a duration any number of seconds", () => {
  const count = row({ key: "turn_engine.max_iterations" });
  expect(readLimit(count, "5")).toEqual({ ok: true, value: 5 });
  expect(readLimit(count, "2.5").ok).toBe(false);
  expect(readLimit(count, "-1").ok).toBe(false);
  expect(readLimit(count, "five").ok).toBe(false);
  const seconds = row({ key: "turn_engine.delegation.task_timeout_seconds", unit: "seconds" });
  expect(readLimit(seconds, "90.5")).toEqual({ ok: true, value: 90.5 });
});

test("an empty field is the default, which is no value at all", () => {
  expect(readLimit(SHARE, "  ")).toEqual({ ok: true, value: null });
});

test("a key's patch nests by its path, and null removes it", () => {
  expect(limitPatch("turn_engine.delegation.max_parallel", 2)).toEqual({
    turn_engine: { delegation: { max_parallel: 2 } },
  });
  expect(limitPatch("notification_rate_limit", null)).toEqual({ notification_rate_limit: null });
});

test("the document's own value is read by the key's path", () => {
  const doc = { name: "Acme", turn_engine: { delegation: { max_parallel: 2 } } };
  expect(documentLimit(doc, "turn_engine.delegation.max_parallel")).toBe(2);
  expect(documentLimit(doc, "turn_engine.max_iterations")).toBeUndefined();
  expect(documentLimit(doc, "notification_rate_limit")).toBeUndefined();
});

// AGAINST THE DOCUMENT, never the page: a value the document already writes
// is nothing to save, and so is the default on a document that writes none,
// while the default on a document that writes one removes it.
test("a change is worked out against the document as it is now", () => {
  const cap = row({ key: "turn_engine.max_iterations" });
  const writes = { name: "Acme", turn_engine: { max_iterations: 5 } };
  expect(limitPlan(cap, "Review rounds per turn", 5)(writes)).toBeNull();
  expect(limitPlan(cap, "Review rounds per turn", null)({ name: "Acme" })).toBeNull();
  expect(limitPlan(cap, "Review rounds per turn", null)(writes)).toEqual({
    patch: { turn_engine: { max_iterations: null } },
    summary: "Limits: Review rounds per turn back to the default (turn_engine.max_iterations)",
  });
  expect(limitPlan(SHARE, "Share of the budget a call may spend", 0.35)({ name: "Acme" })).toEqual({
    patch: { turn_engine: { delegation: { budget_fraction: 0.35 } } },
    summary:
      "Limits: Share of the budget a call may spend set to 35% (turn_engine.delegation.budget_fraction)",
  });
});
