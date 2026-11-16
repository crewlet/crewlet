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
  limitRefusal,
  limitText,
  limitUnchanged,
  rangeWords,
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
  const count = row({ key: "turn_engine.max_iterations", range: { min: 1 } });
  expect(readLimit(count, "5")).toEqual({ ok: true, value: 5 });
  expect(readLimit(count, "2.5")).toEqual({ ok: false, error: "“2.5” is not a whole number." });
  expect(readLimit(count, "-1")).toEqual({ ok: false, error: "At least 1." });
  expect(readLimit(count, "five")).toEqual({ ok: false, error: "“five” is not a whole number." });
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
  expect(limitPlan(cap, "Review rounds per turn", 5, "Limits")(writes)).toBeNull();
  expect(limitPlan(cap, "Review rounds per turn", null, "Limits")({ name: "Acme" })).toBeNull();
  expect(limitPlan(cap, "Review rounds per turn", null, "Limits")(writes)).toEqual({
    patch: { turn_engine: { max_iterations: null } },
    summary: "Limits: Review rounds per turn back to the default (turn_engine.max_iterations)",
  });
  expect(
    limitPlan(SHARE, "Share of the budget a call may spend", 0.35, "Limits")({ name: "Acme" }),
  ).toEqual({
    patch: { turn_engine: { delegation: { budget_fraction: 0.35 } } },
    summary:
      "Limits: Share of the budget a call may spend set to 35% (turn_engine.delegation.budget_fraction)",
  });
});

// THE ENGINE'S RANGE, IN THE CAP'S OWN WORDS. The row carries the bound the
// validator holds the cap to, and a value outside it is said beside the field
// before anything is sent: 0, -2 and 999999 used to reach the engine and come
// back as its operator's line.
test("a value outside the cap's range is refused in the cap's unit", () => {
  const rounds = row({ key: "turn_engine.max_iterations", range: { min: 1 } });
  expect(readLimit(rounds, "0")).toEqual({ ok: false, error: "At least 1." });
  expect(readLimit(rounds, "999999")).toEqual({ ok: true, value: 999999 });
  const batch = row({ key: "notification_coalesce_max_batch", range: { min: 1, max: 100 } });
  expect(readLimit(batch, "999999")).toEqual({ ok: false, error: "From 1 to 100." });
  const share = { ...SHARE, range: { min: 0, min_exclusive: true, max: 1 } };
  expect(readLimit(share, "0")).toEqual({ ok: false, error: "More than 0%, and at most 100%." });
  expect(readLimit(share, "150")).toEqual({
    ok: false,
    error: "More than 0%, and at most 100%.",
  });
  expect(readLimit(share, "100")).toEqual({ ok: true, value: 1 });
  const timeout = row({
    key: "turn_engine.delegation.task_timeout_seconds",
    unit: "seconds",
    range: { min: 0, min_exclusive: true },
  });
  expect(rangeWords(timeout, timeout.range!)).toBe("More than 0 seconds.");
  expect(readLimit(timeout, "0")).toEqual({ ok: false, error: "More than 0 seconds." });
  // THE DEFAULT IS ALWAYS ADMITTED: it is the key removed.
  expect(readLimit(rounds, "")).toEqual({ ok: true, value: null });
});

test("a value is unchanged when it is the one in force, the default for an empty field", () => {
  const rounds = row({ key: "turn_engine.max_iterations", value: 3, default: 3 });
  expect(limitUnchanged(rounds, 3)).toBe(true);
  expect(limitUnchanged(rounds, null)).toBe(true);
  expect(limitUnchanged(rounds, 4)).toBe(false);
  const set = row({ key: "turn_engine.max_iterations", value: 5, default: 3 });
  expect(limitUnchanged(set, null)).toBe(false);
  expect(limitUnchanged({ ...SHARE, value: 0.2 }, 0.2)).toBe(true);
});

// A PERSON'S WORDS, NEVER THE DOCUMENT'S: a range at this cap as the field
// says it, a rule's own words where it wrote them, and anything else plainly.
test("the engine's refusal of a cap is said without the document's keys", () => {
  const rounds = row({ key: "turn_engine.max_iterations" });
  const at = (path: string, extra: object = {}) => ({
    path,
    segments: path.split("."),
    kind: "out_of_range" as const,
    message: `${path}: value out of range: must be at least 1, got 0`,
    ...extra,
  });
  expect(limitRefusal(rounds, [at("turn_engine.max_iterations", { range: { min: 1 } })])).toBe(
    "At least 1.",
  );
  expect(
    limitRefusal(rounds, [
      at("turn_engine.onboarding_max_tool_rounds_ceiling", { said: "Rounds can be at most 20." }),
    ]),
  ).toBe("Rounds can be at most 20.");
  expect(limitRefusal(rounds, [at("mcp_servers.0.name")])).toBe(
    "This value does not fit the rest of the company’s configuration.",
  );
});

// A RANGE WITH A HOLE: 0 is admitted beside 30 to 3650 days (keep for ever),
// while 1 to 29 is refused in words that name both. Mutation: drop the zero
// carve-out from rangeAdmits and 0 is refused; drop it from rangeWords and the
// refusal of 12 never offers for ever.
test("a range that also admits zero takes 0 and refuses the hole below its least", () => {
  const horizon = row({
    key: "chat.chatlet.message_retention_days",
    group: "chat",
    unit: "days",
    value: 365,
    default: 365,
    range: { min: 30, max: 3650, or_zero: true },
  });
  const zero = "0 to keep them for ever";
  expect(readLimit(horizon, "0", zero)).toEqual({ ok: true, value: 0 });
  expect(readLimit(horizon, "30", zero)).toEqual({ ok: true, value: 30 });
  for (const typed of ["1", "12", "29", "3651", "-1"]) {
    expect(readLimit(horizon, typed, zero)).toEqual({
      ok: false,
      error: "From 30 days to 3,650 days, or 0 to keep them for ever.",
    });
  }
  // THE ENGINE'S REFUSAL carrying the range is said the same way.
  expect(
    limitRefusal(
      horizon,
      [
        {
          path: "chat.chatlet.message_retention_days",
          segments: null,
          kind: "out_of_range",
          message:
            "chat.chatlet.message_retention_days: out of range: must be 0 or between 30 and 3650, got 12",
          range: { min: 30, max: 3650, or_zero: true },
        },
      ],
      zero,
    ),
  ).toBe("From 30 days to 3,650 days, or 0 to keep them for ever.");
});

// A DURATION IS TYPED AS ONE: seconds bare, or minutes, hours and days by their
// letter, and a whole one opens in its largest unit.
test("a duration takes a unit letter and opens in its largest whole unit", () => {
  const d = row({ key: "sign_in.session_lifetime_seconds", unit: "seconds" });
  expect(readLimit(d, "90")).toEqual({ ok: true, value: 90 });
  expect(readLimit(d, "15m")).toEqual({ ok: true, value: 900 });
  expect(readLimit(d, "12 h")).toEqual({ ok: true, value: 43_200 });
  expect(readLimit(d, "7D")).toEqual({ ok: true, value: 604_800 });
  expect(readLimit(d, "1.5h")).toEqual({ ok: true, value: 5_400 });
  expect(readLimit(d, "a week")).toEqual({ ok: false, error: "“a week” is not a duration." });
  expect(limitText(d, 604_800)).toBe("7d");
  expect(limitText(d, 5_400)).toBe("90m");
  expect(limitText(d, 90)).toBe("90");
});

// A BOUND THIS NODE'S OPERATOR SET is named as theirs.
test("a bound the node's operator set says so", () => {
  const d = row({
    key: "sign_in.session_lifetime_seconds",
    unit: "seconds",
    range: { min: 3_600, max: 1_209_600, operator_max: true },
  });
  expect(readLimit(d, "30d")).toEqual({
    ok: false,
    error: "From 1 hour to 14 days. This node’s operator allows no more.",
  });
  const p = row({
    key: "sign_in.min_password_length",
    range: { min: 14, max: 256, operator_min: true },
  });
  expect(readLimit(p, "12")).toEqual({
    ok: false,
    error: "From 14 to 256. This node’s operator allows no less.",
  });
});
