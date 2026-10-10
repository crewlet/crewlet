// @vitest-environment node

import { describe, expect, test } from "vitest";
import type { BudgetWindow } from "~/protocol/types.ts";
import { resetsWords, turnsOverWords, waitedOn, windowOf } from "./budget.ts";

function w(over: Partial<BudgetWindow>): BudgetWindow {
  return {
    period: "day",
    window: "2026-09-23",
    starts_at: "2026-09-23T00:00:00Z",
    resets_at: "2026-09-24T00:00:00Z",
    used: 0,
    limit: 100,
    state: "ok",
    ...over,
  };
}

describe("which window a scope waits on", () => {
  // THE ENGINE'S TIE-BREAK. A seat refused in its day and its month has room
  // again only when the month turns over, so naming the day would tell a
  // reader it can run tonight when the gate will still refuse it.
  test("the refusing window that turns over last", () => {
    const day = w({ state: "refusing" });
    const month = w({
      period: "month",
      window: "2026-09",
      resets_at: "2026-10-01T00:00:00Z",
      state: "refusing",
    });
    expect(waitedOn([day, month], "refusing")).toBe(month);
    expect(waitedOn([month, day], "refusing")).toBe(month);
  });

  test("the longer period where two turn over together", () => {
    const week = w({ period: "week", resets_at: "2026-10-01T00:00:00Z", state: "near" });
    const month = w({ period: "month", resets_at: "2026-10-01T00:00:00Z", state: "near" });
    expect(waitedOn([week, month], "near")).toBe(month);
  });

  test("nothing in that state is nothing", () => {
    expect(waitedOn([w({ state: "near" })], "refusing")).toBeUndefined();
    expect(waitedOn(undefined, "refusing")).toBeUndefined();
  });
});

// ON THE COMPANY'S CALENDAR. A week cut in Tokyo turns over at Monday's
// midnight there, which is Sunday afternoon in UTC: read on any clock but the
// company's, the date is a day early.
describe("when a window turns over", () => {
  const week = w({ period: "week", window: "2026-W40", resets_at: "2026-10-04T15:00:00Z" });
  const day = w({ resets_at: "2026-09-23T15:00:00Z" });

  test("a week or a month on the company date its first instant falls on", () => {
    expect(turnsOverWords(week, "Asia/Tokyo")).toBe("on Oct 5");
    expect(resetsWords(week, "Asia/Tokyo")).toBe("resets Oct 5");
    expect(turnsOverWords(week, "UTC")).toBe("on Oct 4");
  });

  test("a day at the company's midnight, the zone named", () => {
    expect(turnsOverWords(day, "Asia/Tokyo")).toBe("at midnight (Asia/Tokyo)");
    expect(resetsWords(day, "Asia/Tokyo")).toBe("resets at midnight (Asia/Tokyo)");
    // A COMPANY THAT WRITES NO ZONE RUNS ON UTC, and is told so.
    expect(turnsOverWords(day, "")).toBe("at midnight (UTC)");
  });

  test("an instant that does not parse is not given a moment", () => {
    expect(turnsOverWords(w({ resets_at: "" }), "UTC")).toBe("");
    expect(resetsWords(w({ period: "month", resets_at: "soon" }), "UTC")).toBe("");
  });
});

describe("finding a window", () => {
  test("a period is found by name", () => {
    const week = w({ period: "week" });
    expect(windowOf([w({}), week], "week")).toBe(week);
    expect(windowOf([w({})], "month")).toBeUndefined();
  });
});
