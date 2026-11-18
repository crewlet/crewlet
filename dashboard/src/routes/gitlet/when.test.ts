// @vitest-environment node
import { afterEach, describe, expect, test } from "vitest";
import { pinZoneForTest } from "~/lib/prefs.ts";
import { COUNTED_DAYS, WHEN, dayBoundsFor, sinceOf } from "./when.ts";

/** The engine's bound on `day_bounds` (`gitlet.MaxDayBounds`). */
const MAX_DAY_BOUNDS = 32;
const QUARTER_HOUR_MS = 15 * 60_000;

afterEach(() => pinZoneForTest(""));

describe("a window's day bounds", () => {
  // Mutation: count Any time's days as 90, and the engine refuses 91 bounds.
  test.each([
    ["", 32],
    ["today", 2],
    ["7d", 8],
    ["30d", 31],
    ["90d", 32],
  ])("for when=%j are %i of the reader's midnights, never past the engine's bound", (when, n) => {
    const bounds = dayBoundsFor(when, "2026-11-16", "UTC");
    expect(bounds).toHaveLength(n);
    expect(bounds.at(-1)).toBe("2026-11-17T00:00:00Z");
    expect(bounds.length).toBeLessThanOrEqual(MAX_DAY_BOUNDS);
  });

  // A +5:45 and a -3:30 zone, the second across November's clock change.
  // Mutation: send UTC midnights, and the first bound is the UTC day's.
  test.each([
    ["Asia/Kathmandu", "7d", "2026-11-16", "2026-11-09T18:15:00Z", "2026-11-16T18:15:00Z"],
    ["America/St_Johns", "", "2026-11-05", "2026-10-06T02:30:00Z", "2026-11-06T03:30:00Z"],
  ])(
    "are the reader's midnights in %s, each on a quarter-hour",
    (zone, when, today, first, last) => {
      const bounds = dayBoundsFor(when, today, zone);
      expect([bounds[0], bounds.at(-1)]).toEqual([first, last]);
      const at = bounds.map((b) => Date.parse(b));
      for (let i = 0; i < at.length; i++) {
        expect(at[i]! % QUARTER_HOUR_MS, bounds[i]).toBe(0);
        if (i > 0) expect(at[i]!).toBeGreaterThan(at[i - 1]!);
      }
    },
  );

  // THE ENGINE REFUSES A BOUND BEFORE `since`: every window's first bound is at or after it.
  // Mutation: start the window at now less its days, and since falls after the first bound.
  test.each(WHEN.map((w) => [w.value] as const))(
    "for when=%j lie inside the window since names",
    (when) => {
      pinZoneForTest("Asia/Kathmandu");
      const since = sinceOf(when, "2026-11-16");
      const bounds = dayBoundsFor(when, "2026-11-16");
      if (since === undefined) return;
      expect(Date.parse(bounds[0]!)).toBeGreaterThanOrEqual(Date.parse(since));
      if (when !== "90d") expect(Date.parse(bounds[0]!)).toBe(Date.parse(since));
    },
  );

  test("count at most the days the engine counts in one read", () => {
    expect(COUNTED_DAYS + 1).toBe(MAX_DAY_BOUNDS);
  });
});
