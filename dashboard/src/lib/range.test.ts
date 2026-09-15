import { describe, expect, it } from "vitest";
import { RANGES, RANGE_LABEL, rangeOf, rangeWindows } from "./range.ts";

describe("the window a URL asked for", () => {
  it("takes the values the control offers", () => {
    for (const value of RANGES) expect(rangeOf(value)).toBe(value);
  });

  it("falls back for anything outside the set", () => {
    // `#/cost?window=` is produced by a route transition for one render, and
    // read as a number it is 0 — a window whose two edges are the same
    // instant, which is half-open and names no rows at all.
    for (const bad of ["", "0", "3", "-7", "all", "NaN", "7 "]) {
      expect(rangeOf(bad), bad).toBe("7");
    }
  });

  it("takes the caller's own fallback", () => {
    // A screen whose natural window is a day says so, rather than every
    // screen sharing one default because the helper picked it.
    expect(rangeOf("", "1")).toBe("1");
    expect(rangeOf("nonsense", "30")).toBe("30");
  });

  it("has a label for every value, and no label for anything else", () => {
    // The two are read together — a control renders the label beside the
    // value — so a value with no label is a blank button.
    expect(Object.keys(RANGE_LABEL).sort()).toEqual([...RANGES].sort());
  });
});

describe("the two windows a range names", () => {
  const NOW = new Date("2026-06-15T12:00:00Z").getTime();

  it("ends at the caller's own now", () => {
    const got = rangeWindows("7", NOW);
    expect(got.until).toBe("2026-06-15T12:00:00.000Z");
    expect(got.since).toBe("2026-06-08T12:00:00.000Z");
  });

  it("makes the comparison window the SAME WIDTH, ending where this one starts", () => {
    // A comparison whose two windows are different lengths compares two
    // numbers that do not answer the same question — which is how a 28-day
    // February outperforms every other month in every dashboard that gets
    // this wrong.
    for (const window of RANGES) {
      const got = rangeWindows(window, NOW);
      const width = (a: string, b: string) => Date.parse(b) - Date.parse(a);
      expect(width(got.since, got.until), window).toBe(
        width(got.previous.since, got.previous.until),
      );
      // AND THEY MEET rather than overlapping or leaving a gap: a record on
      // the boundary belongs to exactly one of them.
      expect(got.previous.until, window).toBe(got.since);
    }
  });

  it("scales with the window rather than being a fixed step back", () => {
    const day = rangeWindows("1", NOW);
    const quarter = rangeWindows("90", NOW);
    expect(Date.parse(day.since) - Date.parse(day.previous.since)).toBe(86_400_000);
    expect(Date.parse(quarter.since) - Date.parse(quarter.previous.since)).toBe(90 * 86_400_000);
  });
});
