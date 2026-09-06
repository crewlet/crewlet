// @vitest-environment node

/**
 * Formatting, and the ordering rules underneath it.
 *
 * The ordering half is the load-bearing part: every list in this product sorts
 * through `tsKey`, and the reason is a real encoding hazard rather than a
 * stylistic preference.
 */

import { describe, expect, test } from "vitest";
import {
  elapsedMs,
  fmtCount,
  fmtDuration,
  fmtPct,
  humanize,
  inTime,
  newestFirst,
  oldestFirst,
  parseUTC,
  plural,
  relTime,
  splitConversationKey,
  tsKey,
  fmtElapsed,
} from "./format.ts";

describe("timestamps", () => {
  test("a naive stamp is read as UTC", () => {
    // The engine emits both aware and naive forms, often for the same instant.
    // Read as local time, a naive stamp is wrong by the reader's own offset.
    expect(tsKey("2026-01-01T00:00:00")).toBe(tsKey("2026-01-01T00:00:00Z"));
  });

  test("a trimmed fractional second still sorts as the earlier instant", () => {
    // Go's RFC3339Nano trims trailing zeros, so a raw string compare puts
    // `…:07Z` after `…:07.42Z` — it compares 'Z' (0x5A) against '.' (0x2E).
    expect(tsKey("2026-01-01T00:00:07Z")).toBeLessThan(tsKey("2026-01-01T00:00:07.42Z"));
    expect("2026-01-01T00:00:07Z" > "2026-01-01T00:00:07.42Z").toBe(true);
  });

  test("an unparseable stamp is 0 rather than NaN", () => {
    // NaN poisons a comparator's transitivity, which makes a sort's output
    // depend on the input order — a list that shuffles itself.
    expect(tsKey("not a time")).toBe(0);
    expect(tsKey(undefined)).toBe(0);
    expect(parseUTC("")).toBeNull();
  });

  test("newestFirst breaks a tie on the id, and returns 0 for equals", () => {
    // Burst writes share a timestamp at microsecond resolution, so the id
    // tiebreak is what stops a merge dropping or duplicating whatever
    // collided.
    const a = { timestamp: "2026-01-01T00:00:00Z", id: "a" };
    const b = { timestamp: "2026-01-01T00:00:00Z", id: "b" };
    expect(newestFirst(a, b)).toBeGreaterThan(0);
    expect(newestFirst(b, a)).toBeLessThan(0);
    expect(newestFirst(a, { ...a })).toBe(0);
    expect(oldestFirst(a, b)).toBeLessThan(0);
  });

  test("a sort through newestFirst is stable and idempotent", () => {
    const rows = [
      { timestamp: "2026-01-01T00:00:02Z", id: "b" },
      { timestamp: "2026-01-01T00:00:01Z", id: "a" },
      { timestamp: "2026-01-01T00:00:02Z", id: "c" },
    ];
    const once = [...rows].sort(newestFirst).map((r) => r.id);
    const twice = [...rows]
      .sort(newestFirst)
      .sort(newestFirst)
      .map((r) => r.id);
    expect(once).toEqual(twice);
  });
});

describe("relative time", () => {
  const now = Date.parse("2026-01-01T12:00:00Z");

  test("reads the instant it is GIVEN, never the clock", () => {
    // Every relative time on a screen has to agree with every other, and a
    // component reading its own clock re-renders on its own schedule and
    // disagrees with the row above it. It is also what makes these strings
    // actually advance instead of freezing until an unrelated push lands.
    expect(relTime("2026-01-01T11:56:00Z", now)).toBe("4m ago");
    expect(relTime("2026-01-01T12:00:00Z", now)).toBe("just now");
    expect(relTime("2026-01-01T09:00:00Z", now)).toBe("3h ago");
  });

  test("a future stamp reads forwards rather than as a negative age", () => {
    expect(relTime("2026-01-01T12:30:00Z", now)).toBe("in 30m");
    expect(inTime("2026-01-01T11:00:00Z", now)).toBe("due");
  });

  test("a missing stamp is an em dash, not the epoch", () => {
    expect(relTime(undefined, now)).toBe("—");
    expect(inTime("", now)).toBe("—");
  });
});

describe("numbers", () => {
  test("a four-digit count stays exact", () => {
    // A token count in the thousands is something an operator reads exactly;
    // rounding it to "1.2k" throws away the digit they were looking at.
    expect(fmtCount(9999)).toBe((9999).toLocaleString());
    expect(fmtCount(12_400)).toBe("12.4k");
    expect(fmtCount(1_240_000)).toBe("1.2M");
  });

  test("an absent number is an em dash rather than a zero", () => {
    // Zero is a measurement. "Nobody looked" is not.
    expect(fmtCount(null)).toBe("—");
    expect(fmtCount(Number.NaN)).toBe("—");
    expect(fmtPct(1, 0)).toBe("—");
  });

  test("durations pick the shortest honest unit", () => {
    expect(fmtDuration(420)).toBe("420 ms");
    expect(fmtDuration(4_200)).toBe("4.2 s");
    expect(fmtDuration(95_000)).toBe("1m 35s");
    expect(fmtDuration(null)).toBe("—");
    expect(fmtDuration(-1)).toBe("—");
  });

  test("elapsed needs both ends", () => {
    expect(elapsedMs("2026-01-01T00:00:00Z", "2026-01-01T00:00:05Z")).toBe(5000);
    expect(elapsedMs(undefined, "2026-01-01T00:00:05Z")).toBeNull();
  });
});

describe("text", () => {
  test("a conversation key splits ONCE", () => {
    // The grammar is `{source}:{local}` and the local half may itself contain
    // colons — a Slack thread is `slack:C9:1718.001`.
    expect(splitConversationKey("slack:C9:1718.001")).toEqual({
      source: "slack",
      local: "C9:1718.001",
    });
    expect(splitConversationKey("github:acme/api#42").local).toBe("acme/api#42");
    expect(splitConversationKey("bare")).toEqual({ source: "", local: "bare" });
  });

  test("an engine identifier becomes a label", () => {
    expect(humanize("agent_phase_completed")).toBe("Agent phase completed");
    expect(humanize("phase.tool_skill_blocked")).toBe("Phase tool skill blocked");
    expect(humanize("")).toBe("");
  });
});

describe("counts and their nouns", () => {
  test("a count agrees with its noun", () => {
    // Three screens printed "1 humans", "1 seats" and "1 phases loaded". That
    // is the shape of thing nobody fixes one at a time, so it is one helper.
    expect(plural(1, "seat")).toBe("1 seat");
    expect(plural(2, "seat")).toBe("2 seats");
    expect(plural(0, "seat")).toBe("0 seats");
  });

  test("an irregular plural is given explicitly", () => {
    expect(plural(1, "entity", "entities")).toBe("1 entity");
    expect(plural(3, "entity", "entities")).toBe("3 entities");
  });

  test("a large count is grouped", () => {
    expect(plural(12000, "event")).toBe(`${(12000).toLocaleString()} events`);
  });
});

describe("a live counter reads as a clock, not as a glitch", () => {
  test("whole seconds — never milliseconds or tenths", () => {
    // The live row churned through "0 ms", "1.4 s", "1.9 s" once a second.
    expect(fmtElapsed(0)).toBe("0s");
    expect(fmtElapsed(340)).toBe("0s");
    expect(fmtElapsed(1400)).toBe("1s");
    expect(fmtElapsed(1900)).toBe("1s");
    expect(fmtElapsed(59_000)).toBe("59s");
  });

  test("a clock skew never shows a negative or a future", () => {
    // A seat's clock and the browser's disagree by a few hundred
    // milliseconds, and "in 1s" for something already running is the one
    // reading that is certainly wrong.
    expect(fmtElapsed(-800)).toBe("0s");
  });

  test("minutes and hours", () => {
    expect(fmtElapsed(72_000)).toBe("1m 12s");
    expect(fmtElapsed(3_800_000)).toBe("1h 3m");
  });

  test("a missing span is a dash, not a zero", () => {
    expect(fmtElapsed(null)).toBe("—");
    expect(fmtElapsed(undefined)).toBe("—");
    expect(fmtElapsed(NaN)).toBe("—");
  });
});
