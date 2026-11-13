// @vitest-environment node

/**
 * Formatting, and the ordering rules underneath it.
 *
 * The ordering half is the load-bearing part: every list in this product sorts
 * through `tsKey`, and the reason is a real encoding hazard rather than a
 * stylistic preference.
 */

import { describe, expect, test } from "vitest";
// THE ONE MARK, from the design system rather than re-spelled here. A test
// holding its own copy of the glyph is how a second one gets into the product
// unnoticed: it goes green on whatever is written, not on what is correct.
import { EMPTY_VALUE } from "@crewlethq/ui";
import {
  charactersOver,
  listed,
  fmtSetting,
  asSentence,
  companyMonthLabel,
  configValueKind,
  elapsedMs,
  eventHistoryLabel,
  eventHistorySpan,
  nodeCountLabel,
  formatPhaseLLM,
  fmtAt,
  fmtBytes,
  fmtClock,
  fmtCount,
  fmtCountShort,
  fmtDate,
  fmtDateTime,
  fmtDateTimeFull,
  fmtDuration,
  fmtPct,
  humanize,
  inTime,
  inTimeExact,
  newestFirst,
  oldestFirst,
  parseUTC,
  plural,
  possessive,
  relTime,
  sentence,
  splitConversationKey,
  tsKey,
  fmtElapsed,
  fmtUptime,
  openSentence,
  overWords,
  textBudget,
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

  // A MONTH THE ENGINE CUT ON THE COMPANY'S CLOCK is named from its label,
  // which reads no clock: the suite's Berlin, and any zone a browser cannot
  // read, name it alike. Anything that is not a month is drawn as it came.
  test("a company month label names its month without reading a clock", () => {
    expect(companyMonthLabel("2026-09")).toBe("September");
    expect(companyMonthLabel("2026-01")).toBe("January");
    expect(companyMonthLabel("2026-13")).toBe("2026-13");
    expect(companyMonthLabel("2026-09-29")).toBe("2026-09-29");
    expect(companyMonthLabel("")).toBe("");
  });

  // ONE PHRASE FOR A MOMENT A PERSON PLANS BY: no seconds, no year this year,
  // the weekday with the date, and one 24-hour clock with the rest of the page.
  test("a moment reads as a time today and a weekday and date otherwise", () => {
    const now = Date.parse("2026-11-06T12:00:00Z"); // 13:00 in the suite's Berlin
    expect(fmtAt("2026-11-06T16:37:00Z", now)).toBe("17:37");
    expect(fmtAt("2026-11-06T23:00:00Z", now)).toBe("Sat, Nov 7, 00:00");
    expect(fmtAt("2027-01-05T09:00:00Z", now)).toBe("Tue, Jan 5, 2027, 10:00");
    expect(fmtAt("", now)).toBe(EMPTY_VALUE);
  });

  test("a clock in a sentence is on the 24-hour clock the rest of the page uses", () => {
    const now = Date.parse("2026-11-06T12:00:00Z");
    expect(fmtClock("2026-11-06T09:23:00Z", now)).toBe("10:23");
    expect(fmtClock("2026-11-06T15:30:00Z", now)).toBe("16:30");
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
    expect(relTime("2026-01-01T12:00:05Z", now)).toBe("in 5s");
    expect(inTime("2026-01-01T11:00:00Z", now)).toBe("due");
  });

  // A STAMP A FEW SECONDS AHEAD HAS JUST HAPPENED: written a moment after the
  // tab's clock last ticked, or on an engine whose clock runs a little ahead.
  // Mutation: send every negative age to inTime and these read "in 1s".
  test("a stamp under five seconds ahead reads just now", () => {
    expect(relTime("2026-01-01T12:00:01Z", now)).toBe("just now");
    expect(relTime("2026-01-01T12:00:04Z", now)).toBe("just now");
  });

  test("a missing stamp is an em dash, not the epoch", () => {
    expect(relTime(undefined, now)).toBe(EMPTY_VALUE);
    expect(inTime("", now)).toBe(EMPTY_VALUE);
  });

  // AN UNSET TIME SPELLED AS A VALUE is still unset: the epoch a store's
  // default decodes to and Go's zero time both read as no time, never as a
  // date in 1969 or the year 1. Mutation: drop the epoch bound in parseUTC.
  // THE FULL HINT NAMES THE SECOND on the 24-hour clock, with its weekday.
  // Mutation: drop `timeStyle` and the seconds go; drop `hourCycle` and en-US
  // reads "PM".
  test("a full instant names its weekday and its second on the 24-hour clock", () => {
    const full = fmtDateTimeFull("2026-11-07T15:41:07Z");
    expect(full).toMatch(/^(Mon|Tues|Wednes|Thurs|Fri|Satur|Sun)day, /);
    expect(full).toMatch(/\b\d{2}:41:07$/);
    expect(full).not.toMatch(/[AP]M/);
    expect(fmtDateTimeFull(undefined)).toBe(EMPTY_VALUE);
  });

  test("the epoch and the zero time are no instant", () => {
    for (const unset of ["1970-01-01T00:00:00Z", "0001-01-01T00:00:00Z"]) {
      expect(parseUTC(unset)).toBeNull();
      expect(fmtDateTime(unset)).toBe(EMPTY_VALUE);
      expect(relTime(unset, now)).toBe(EMPTY_VALUE);
      expect(tsKey(unset)).toBe(0);
    }
    expect(parseUTC("1970-01-01T00:00:01Z")?.getTime()).toBe(1000);
  });

  // A DISTANT FUTURE INSTANT REACHES A DATE, exactly as a distant past one does.
  //
  // `inTime`'s own doc said "the same rules, forward" and it was one rule short:
  // backwards, a relative reading stops at thirty days and prints the date;
  // forwards, it counted days for ever. "in 341d" is a number nobody converts
  // back into a month, and it is why a task due next quarter had no date
  // anywhere on its screen.
  test("a distant future instant reaches a DATE, exactly as a distant past one does", () => {
    const far = "2027-01-01T12:00:00Z";
    expect(inTime(far, now)).not.toMatch(/^in \d+d$/);
    expect(inTime(far, now)).toBe(fmtDate(far));
    expect(relTime(far, now)).toBe(fmtDate(far));
    // THE TERMINUS IS THE SAME THIRTY DAYS, and the steps below it are untouched
    // — a forward clock and a backward one changing shape at different points
    // would be two clocks on one screen.
    expect(inTime("2026-01-20T12:00:00Z", now)).toBe("in 19d");
    expect(inTime("2026-01-30T12:00:00Z", now)).toBe("in 29d");
    expect(inTime("2026-01-31T12:00:00Z", now)).toBe(fmtDate("2026-01-31T12:00:00Z"));
  });

  // A FORWARD READING ROUNDS TO THE NEAREST UNIT. Floored, it dropped what was
  // left of its last unit: an invitation good for seven days read "in 6d" the
  // second it was issued. Mutation: floor again and the first two read a day
  // and an hour short.
  test("a forward reading is the nearest unit, not the one below it", () => {
    expect(inTime("2026-01-08T11:59:59Z", now)).toBe("in 7d");
    expect(inTime("2026-01-01T13:59:00Z", now)).toBe("in 2h");
    expect(inTime("2026-01-04T00:00:00Z", now)).toBe("in 3d");
    expect(inTime("2026-01-01T12:04:20Z", now)).toBe("in 4m");
  });

  // A SERIES IS READ ROW AGAINST ROW. Rounded to one unit, a schedule every
  // ten minutes lists the fires at 13:10 and 13:20 both as "in 1h" — the one
  // list whose point is the spacing between its rows.
  test("an exact forward reading tells apart two instants twenty minutes apart", () => {
    expect(inTime("2026-01-01T13:10:00Z", now)).toBe(inTime("2026-01-01T13:20:00Z", now));
    expect(inTimeExact("2026-01-01T13:20:00Z", now)).toBe("in 1h 20m");
    expect(inTimeExact("2026-01-01T13:40:00Z", now)).toBe("in 1h 40m");
    // A whole hour says no minutes, and under an hour it is `inTime`'s own.
    expect(inTimeExact("2026-01-01T14:00:00Z", now)).toBe("in 2h");
    expect(inTimeExact("2026-01-01T12:30:00Z", now)).toBe(inTime("2026-01-01T12:30:00Z", now));
    // Days carry their hours, and the terminus is the same thirty days.
    expect(inTimeExact("2026-01-03T15:00:00Z", now)).toBe("in 2d 3h");
    expect(inTimeExact("2026-01-31T12:00:00Z", now)).toBe(fmtDate("2026-01-31T12:00:00Z"));
    expect(inTimeExact("", now)).toBe(EMPTY_VALUE);
  });
});

describe("numbers", () => {
  // POWERS OF 1024 ARE LABELLED AS SUCH, as the engine labels them: the harness
  // and the node's own sentences say "8.0 GiB", and a banner that drew the same
  // size as "8.0 GB" read as a different number beside it. Mutation: put the
  // decimal labels back.
  test("a size is labelled in binary units, as the engine labels it", () => {
    expect(fmtBytes(512)).toBe("512 B");
    expect(fmtBytes(3_789)).toBe("3.7 KiB");
    expect(fmtBytes(8 * 1024 ** 3)).toBe("8.0 GiB");
    expect(fmtBytes(2 * 1024 ** 4)).toBe("2.0 TiB");
  });

  test("a four-digit count stays exact", () => {
    // A token count in the thousands is something an operator reads exactly;
    // rounding it to "1.2k" throws away the digit they were looking at.
    expect(fmtCount(9999)).toBe((9999).toLocaleString());
    expect(fmtCount(12_400)).toBe("12.4k");
    expect(fmtCount(1_240_000)).toBe("1.2M");
  });

  // A PLACE FOUR CHARACTERS WIDE shortens a step sooner, and never past four.
  test("a short count fits four characters at every magnitude", () => {
    expect(fmtCountShort(999)).toBe("999");
    expect(fmtCountShort(2000)).toBe("2k");
    expect(fmtCountShort(2400)).toBe("2.4k");
    expect(fmtCountShort(12_400)).toBe("12k");
    expect(fmtCountShort(123_456)).toBe("123k");
    expect(fmtCountShort(1_240_000)).toBe("1.2M");
  });

  // A WHOLE FIGURE CARRIES NO ZERO FRACTION: a founder writes a ceiling as a
  // round number, and "60.0M/day and 900.0M/month" was a digit that said
  // nothing on every read of it.
  test("a shortened figure that is whole drops its zero fraction", () => {
    expect(fmtCount(60_000_000)).toBe("60M");
    expect(fmtCount(900_000_000)).toBe("900M");
    expect(fmtCount(3_000_000_000)).toBe("3B");
    expect(fmtCount(20_000)).toBe("20k");
    expect(fmtCount(1_500_000_000)).toBe("1.5B");
  });

  // ONE RULE AT EVERY MAGNITUDE. Three company ceilings side by side read
  // "of 2.50B", "of 2.90B" and "of 3B" beside readings of "31.1M": billions
  // kept a second decimal and a trailing zero no other magnitude keeps.
  // Mutation: format billions with toFixed(2) again.
  test("a figure keeps at most one decimal, and no zero one, at every magnitude", () => {
    expect([2_500_000_000, 2_900_000_000, 3_000_000_000].map(fmtCount)).toEqual([
      "2.5B",
      "2.9B",
      "3B",
    ]);
    expect(fmtCount(2_680_000_000)).toBe("2.7B");
    expect(fmtCount(31_100_000)).toBe("31.1M");
  });

  // Mutation: count `length`, and the Korean case asks for one character where it
  // takes one, or none.
  test("the characters to cut from a text over a byte cap are a person's", () => {
    expect(charactersOver("a".repeat(80), 80)).toBe(0);
    expect(charactersOver("a".repeat(82), 80)).toBe(2);
    // 27 three-byte letters are 81 bytes: one letter cut is 78, and fits.
    expect(charactersOver("한".repeat(27), 80)).toBe(1);
    // An emoji is two UTF-16 units and four bytes, and one character to a reader.
    expect(charactersOver("a".repeat(78) + "🙂", 80)).toBe(1);
  });

  test("an absent number is an em dash rather than a zero", () => {
    // Zero is a measurement. "Nobody looked" is not.
    expect(fmtCount(null)).toBe(EMPTY_VALUE);
    expect(fmtCount(Number.NaN)).toBe(EMPTY_VALUE);
    expect(fmtPct(1, 0)).toBe(EMPTY_VALUE);
  });

  test("durations pick the shortest honest unit", () => {
    expect(fmtDuration(420)).toBe("420ms");
    expect(fmtDuration(4_200)).toBe("4.2s");
    expect(fmtDuration(95_000)).toBe("1m 35s");
    // NO ZERO FIELD, and a carry at every grain boundary.
    expect(fmtDuration(1_000)).toBe("1s");
    expect(fmtDuration(999.6)).toBe("1s");
    expect(fmtDuration(9_960)).toBe("10s");
    expect(fmtDuration(11_390)).toBe("11s");
    expect(fmtDuration(59_600)).toBe("1m");
    expect(fmtDuration(30 * 60_000)).toBe("30m");
    expect(fmtDuration(24 * 3_600_000)).toBe("24h");
    expect(fmtDuration((85 * 60 + 58) * 60_000)).toBe("85h 58m");
    expect(fmtDuration(2 * 3_600_000 - 15_000)).toBe("2h");
    expect(fmtDuration(null)).toBe(EMPTY_VALUE);
    expect(fmtDuration(-1)).toBe(EMPTY_VALUE);
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

  // A HINT IS A CLAUSE, and said before a sentence of a screen's own it has to
  // end as one, or the two run together. A clause already stopped keeps its
  // one mark, and an empty one is not a lone full stop.
  test("a clause becomes a sentence of its own", () => {
    expect(sentence("take the backup again")).toBe("Take the backup again.");
    expect(sentence("  the S3 bucket; take the backup again ")).toBe(
      "The S3 bucket; take the backup again.",
    );
    expect(sentence("Check the path. A route this build does not have.")).toBe(
      "Check the path. A route this build does not have.",
    );
    expect(sentence("is it there?")).toBe("Is it there?");
    expect(sentence("If-None-Match asks for a create")).toBe("If-None-Match asks for a create.");
    expect(sentence("")).toBe("");
    expect(sentence("   ")).toBe("");
  });
});

// A SEAT TITLE OFTEN ENDS IN A PLURAL: "...Podcast Clips's budget" read as a
// mistake. Mutation: append "'s" always and the second case fails.
describe("a name made possessive", () => {
  test("takes 's, or a lone apostrophe after a final s", () => {
    expect(possessive("Agent PM")).toBe("Agent PM's");
    expect(possessive("Analyst for Film, Television and Podcast Clips")).toBe(
      "Analyst for Film, Television and Podcast Clips'",
    );
    expect(possessive("Evenings and Weekends")).toBe("Evenings and Weekends'");
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

describe("an uptime is said coarsely", () => {
  test("seconds, then minutes, then hours and minutes, then days", () => {
    expect(fmtUptime(12_000)).toBe("12s");
    expect(fmtUptime(59_999)).toBe("59s");
    // A panel open past the first minute stops ticking through seconds.
    expect(fmtUptime(61_000)).toBe("1m");
    expect(fmtUptime(3_599_000)).toBe("59m");
    expect(fmtUptime(3_600_000)).toBe("1h");
    expect(fmtUptime(3_780_000)).toBe("1h 3m");
    expect(fmtUptime(86_399_000)).toBe("23h 59m");
    expect(fmtUptime(86_400_000)).toBe("1 day");
    expect(fmtUptime(2 * 86_400_000 + 3_600_000)).toBe("2 days");
  });

  test("a start in the future is a clock skew, and nothing is not a zero", () => {
    expect(fmtUptime(-800)).toBe("0s");
    expect(fmtUptime(Number.NaN)).toBe(fmtUptime(null));
    expect(fmtUptime(null)).not.toBe("0s");
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
    expect(fmtElapsed(null)).toBe(EMPTY_VALUE);
    expect(fmtElapsed(undefined)).toBe(EMPTY_VALUE);
    expect(fmtElapsed(NaN)).toBe(EMPTY_VALUE);
  });
});

// ---------------------------------------------------------------------------
// Values read out of the company document
// ---------------------------------------------------------------------------

// `config.PhaseLLM` marshals as a STRING for one provider, an ARRAY for a
// fallback chain, and an OBJECT keyed on phase for a per-phase mapping. The
// seat screen rendered the field as a React child, which drew a chain as its
// keys glued together and THREW on the mapping, taking the whole page with it.
describe("a seat's model setting, as rows a person reads", () => {
  test("one key and a chain are one row covering every phase", () => {
    expect(formatPhaseLLM("fast")).toEqual([{ phase: "", chain: "fast" }]);
    // The ORDER is the whole meaning of a chain, so it reads as one.
    expect(formatPhaseLLM(["fast", "backup"])).toEqual([{ phase: "", chain: "fast, then backup" }]);
  });

  test("a mapping is one row per phase, in the engine's own order", () => {
    expect(formatPhaseLLM({ judge: "tiny", default: ["big", "fast"] })).toEqual([
      { phase: "default", chain: "big, then fast" },
      { phase: "judge", chain: "tiny" },
    ]);
  });

  // THE ONE READER standing between a field a newer engine may shape
  // differently and a render that must not throw.
  test("anything else is no rows rather than a throw", () => {
    for (const odd of [undefined, null, 0, true, {}, [], "", [""], { default: "" }]) {
      expect(formatPhaseLLM(odd)).toEqual([]);
    }
  });
});

describe("a value read from the redacted document", () => {
  test("the mask says something is set and never what", () => {
    expect(configValueKind("__redacted__")).toBe("hidden");
    expect(configValueKind("")).toBe("empty");
    expect(configValueKind(undefined)).toBe("empty");
  });

  test("one whole reference is a reference, and part of one is not", () => {
    expect(configValueKind("${DEV_A_SLACK_BOT_TOKEN}")).toBe("reference");
    expect(configValueKind("  ${TOKEN}  ")).toBe("reference");
    expect(configValueKind("Bearer sk-live-${SUFFIX}")).toBe("literal");
  });

  // A CREDENTIAL FIELD SHOWS A WHOLE REFERENCE OR NOTHING, whatever the engine
  // sent. Its redaction took any value CONTAINING `${` for a reference, so
  // `Bearer sk-live-${SUFFIX}` reached the wire with its literal half intact —
  // and what this dashboard prints must not depend on every engine it talks to
  // having got that right.
  test("in a credential field anything but a whole reference is hidden", () => {
    expect(configValueKind("Bearer sk-live-${SUFFIX}", { secret: true })).toBe("hidden");
    expect(configValueKind("U0FOUNDER", { secret: true })).toBe("hidden");
    expect(configValueKind("${TOKEN}", { secret: true })).toBe("reference");
    // And a field that is NOT a credential still shows its literal: a contact
    // identity is a public handle at a vendor.
    expect(configValueKind("U0FOUNDER")).toBe("literal");
  });
});

describe("how many nodes", () => {
  test("a count the engine reported is said as one", () => {
    expect(nodeCountLabel(1)).toBe("1 node");
    expect(nodeCountLabel(3)).toBe("3 nodes");
  });

  // NEVER 0, and never a guessed 1: an absent count is a presence read that
  // did not happen.
  test("an absent count says it is unavailable", () => {
    for (const absent of [undefined, null, 0, Number.NaN]) {
      expect(nodeCountLabel(absent)).toBe("node count unavailable");
    }
  });
});

describe("how far back the log goes", () => {
  test("the floor is the engine's own, said in days", () => {
    expect(eventHistoryLabel(30 * 24 * 3600)).toBe("the store keeps 30 days");
    // THE SPAN A READ COVERS names the same number, never a second rounding.
    expect(eventHistorySpan(30 * 24 * 3600)).toBe("the last 30 days");
    expect(eventHistorySpan(6 * 3600)).toBe("the last 6 hours");
    expect(eventHistorySpan(undefined)).toBe("the event log's window");
    expect(eventHistoryLabel(24 * 3600)).toBe("the store keeps 1 day");
    expect(eventHistoryLabel(6 * 3600)).toBe("the store keeps 6 hours");
  });

  // AN OPERATOR TOLD THE WRONG FLOOR STOPS PAGING EARLY, so the honest answer
  // to "I do not know yet" — no health read, the socket down — is that
  // sentence, never a number this client picked.
  test("a floor not known yet is said so rather than guessed", () => {
    for (const absent of [undefined, null, 0, -1, Number.NaN]) {
      expect(eventHistoryLabel(absent)).toBe("how far back the log goes is not known yet");
    }
  });
});

describe("the engine's words as a sentence", () => {
  test("a clause is opened and closed", () => {
    expect(asSentence("no verified backup has been recorded")).toBe(
      "No verified backup has been recorded.",
    );
    expect(asSentence("  already closed!  ")).toBe("Already closed!");
    expect(asSentence("   ")).toBe("");
    expect(openSentence("the code does not match")).toBe("The code does not match");
  });

  // A LITERAL FIRST WORD IS A VALUE, and a capital letter makes it another
  // one: the sign-in screen read "Http://localhost:5199 isn't an address".
  test("a first word that is a literal is written as the engine wrote it", () => {
    expect(asSentence("http://localhost:5199 isn't an address for this deployment.")).toBe(
      "http://localhost:5199 isn't an address for this deployment.",
    );
    for (const literal of [
      "example.com answered 502",
      "token:review holds no seat",
      "api.external_url is unset",
      "backup_age: no verified backup",
      "@maya is not a seat",
      "/var/lib/crewlet is not writable",
      "`crewlet iam` refused the login",
    ]) {
      expect(openSentence(literal)).toBe(literal);
    }
  });

  test("prose that only looks punctuated is still opened", () => {
    expect(openSentence("isn't an address")).toBe("Isn't an address");
    expect(openSentence("done, then saved")).toBe("Done, then saved");
  });
});

// A SETTING READS AS IT WAS WRITTEN: whole days, hours or minutes where the
// seconds divide, and the measured reading only where they do not.
test("a configured duration reads in the largest unit that divides it", () => {
  expect(fmtSetting(604_800)).toBe("7 days");
  expect(fmtSetting(43_200)).toBe("12 hours");
  expect(fmtSetting(3_600)).toBe("1 hour");
  expect(fmtSetting(300)).toBe("5 minutes");
  expect(fmtSetting(90)).toBe("1m 30s");
  expect(fmtSetting(0)).toBe("0ms");
});

test('a list keeps the comma before its last item, so an item carrying its own "and" stays one item', () => {
  expect(listed([])).toBe("");
  expect(listed(["coding"])).toBe("coding");
  expect(listed(["coding", "reviewing and evaluations"])).toBe(
    "coding and reviewing and evaluations",
  );
  expect(listed(["coding", "reviewing and evaluations", "task delegations and routines"])).toBe(
    "coding, reviewing and evaluations, and task delegations and routines",
  );
});

describe("a text past its cap is said in characters, however it is measured", () => {
  test("exactly, where each character is a byte", () => {
    expect(overWords("x".repeat(105), 80)).toBe("25 characters too long");
    expect(overWords("x".repeat(81), 80)).toBe("1 character too long");
  });
  test("about, where they take more: the cap is bytes, and a person counts neither", () => {
    expect(overWords("語".repeat(30), 80)).toBe("about 4 characters too long");
  });
  test("the line under a field never names a byte", () => {
    const over = textBudget("x".repeat(90), 80, "a folder's name");
    expect(over.over).toBe(true);
    expect(over.line).toBe("This folder's name is 10 characters too long. Shorten it.");
    expect(textBudget("x".repeat(70), 80, "a folder's name").line).toBe("10 characters left");
    expect(textBudget("hello", 80, "a folder's name").line).toBeUndefined();
  });
});
