// @vitest-environment node

import { describe, expect, test } from "vitest";
import type { BudgetWindow } from "~/protocol/types.ts";
import { stateTone, waitedOn, windowOf } from "./budget.ts";

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

describe("drawing a state", () => {
  // No fraction picks a tone: a refusing window below its ceiling is critical
  // (a refused charge increments nothing), and a near one is caution.
  test("each state is its own tone", () => {
    expect(stateTone("refusing")).toBe("critical");
    expect(stateTone("near")).toBe("caution");
    expect(stateTone("ok")).toBe("neutral");
  });

  test("a period is found by name", () => {
    const week = w({ period: "week" });
    expect(windowOf([w({}), week], "week")).toBe(week);
    expect(windowOf([w({})], "month")).toBeUndefined();
  });
});
