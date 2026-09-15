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
  configValueKind,
  elapsedMs,
  formatPhaseLLM,
  REDACTED,
  fmtCount,
  fmtDuration,
  fmtPct,
  humanize,
  newestFirst,
  oldestFirst,
  parseUTC,
  plural,
  splitConversationKey,
  tsKey,
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

describe("a seat's model setting, in all three shapes the engine writes", () => {
  test("one key and one chain are a single row for every phase", () => {
    expect(formatPhaseLLM("fast")).toEqual([{ phase: "", chain: "fast" }]);
    // THE ORDER IS THE MEANING of a chain, so it reads as one, not as a set.
    expect(formatPhaseLLM(["fast", "backup"])).toEqual([{ phase: "", chain: "fast, then backup" }]);
  });

  test("a per-phase mapping is one row per phase, in the engine's phase order", () => {
    // Rendered as a React child, this shape threw and blanked the whole page.
    expect(formatPhaseLLM({ review: "big", default: ["fast", "backup"], judge: "cheap" })).toEqual([
      { phase: "default", chain: "fast, then backup" },
      { phase: "review", chain: "big" },
      { phase: "judge", chain: "cheap" },
    ]);
  });

  test("a phase this build does not know is kept, after the known ones", () => {
    expect(formatPhaseLLM({ planner: "big", default: "fast" })).toEqual([
      { phase: "default", chain: "fast" },
      { phase: "planner", chain: "big" },
    ]);
  });

  test("nothing configured, or a shape nobody sends, is no rows rather than a throw", () => {
    expect(formatPhaseLLM(undefined)).toEqual([]);
    expect(formatPhaseLLM(null)).toEqual([]);
    expect(formatPhaseLLM("")).toEqual([]);
    expect(formatPhaseLLM([])).toEqual([]);
    expect(formatPhaseLLM(42)).toEqual([]);
    expect(formatPhaseLLM({ default: 7, review: [" ", 3] })).toEqual([]);
  });
});

describe("a value read from the redacted document", () => {
  test("the mask is a literal that is set and hidden, never a value to print", () => {
    expect(configValueKind(REDACTED)).toBe("hidden");
  });

  test("only a whole reference is a reference", () => {
    expect(configValueKind("${SLACK_BOT_TOKEN}")).toBe("reference");
    expect(configValueKind("Bearer ${TOKEN}")).toBe("literal");
    expect(configValueKind("${TOKEN}${OTHER}")).toBe("literal");
    expect(configValueKind("${TOKEN")).toBe("literal");
  });

  // DEFENCE IN DEPTH. An engine whose redaction let a partial reference
  // through would otherwise have its literal half printed on this page.
  test("in a credential field, anything but one whole reference is hidden", () => {
    expect(configValueKind("Bearer sk-live-${SUFFIX}", { secret: true })).toBe("hidden");
    expect(configValueKind("plain-token", { secret: true })).toBe("hidden");
    expect(configValueKind("${TOKEN}", { secret: true })).toBe("reference");
    expect(configValueKind("", { secret: true })).toBe("empty");
  });

  test("an unset field is empty, and an identity is a literal", () => {
    expect(configValueKind("")).toBe("empty");
    expect(configValueKind(undefined)).toBe("empty");
    expect(configValueKind("U0FOUNDER")).toBe("literal");
  });
});
