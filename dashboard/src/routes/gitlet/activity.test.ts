// @vitest-environment node
import { describe, expect, test } from "vitest";
import { PUSHER_DAYS, SPARK_DAYS, pushDayBounds } from "./activity.tsx";

/** The engine's bound on `day_bounds` (`gitlet.MaxDayBounds`). */
const MAX_DAY_BOUNDS = 32;
const QUARTER_HOUR_MS = 15 * 60_000;

describe("the bounds of a reader's days", () => {
  test.each([
    ["UTC", "2026-11-16", "2026-10-18T00:00:00Z", "2026-11-17T00:00:00Z"],
    ["Asia/Kathmandu", "2026-11-16", "2026-10-17T18:15:00Z", "2026-11-16T18:15:00Z"],
    ["America/St_Johns", "2026-11-16", "2026-10-18T02:30:00Z", "2026-11-17T03:30:00Z"],
    ["Pacific/Chatham", "2026-11-16", "2026-10-17T10:15:00Z", "2026-11-16T10:15:00Z"],
  ])("are its midnights in %s, every one on a quarter-hour", (zone, today, first, last) => {
    const bounds = pushDayBounds(today, PUSHER_DAYS, zone);
    expect(bounds).toHaveLength(PUSHER_DAYS + 1);
    expect([bounds[0], bounds[PUSHER_DAYS]]).toEqual([first, last]);
    const at = bounds.map((b) => Date.parse(b));
    for (let i = 0; i < at.length; i++) {
      expect(at[i]! % QUARTER_HOUR_MS, bounds[i]).toBe(0);
      if (i > 0) expect(at[i]!).toBeGreaterThan(at[i - 1]!);
    }
    expect(at[at.length - 1]! - at[0]!).toBeLessThanOrEqual(MAX_DAY_BOUNDS * 86_400_000);
  });

  // A CLOCK CHANGE INSIDE THE WINDOW keeps each bound on the reader's midnight: a 25-hour day.
  test("keep the reader's midnight across a clock change", () => {
    const bounds = pushDayBounds("2026-11-16", PUSHER_DAYS, "Europe/London");
    expect(bounds.indexOf("2026-10-25T23:00:00Z")).toBe(-1);
    expect(bounds).toContain("2026-10-24T23:00:00Z");
    expect(bounds).toContain("2026-10-26T00:00:00Z");
    const i = bounds.indexOf("2026-10-24T23:00:00Z");
    expect(bounds[i + 1]).toBe("2026-10-26T00:00:00Z");
  });

  test("are asked at most as many as the engine reads, the spark inside them", () => {
    expect(PUSHER_DAYS + 1, "day_bounds is at most gitlet.MaxDayBounds").toBeLessThanOrEqual(
      MAX_DAY_BOUNDS,
    );
    expect(SPARK_DAYS).toBeLessThanOrEqual(PUSHER_DAYS);
    expect(pushDayBounds("not a day", PUSHER_DAYS, "UTC")).toEqual([]);
  });
});
