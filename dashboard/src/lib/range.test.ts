import { describe, expect, it } from "vitest";
import {
  RANGES,
  RANGE_LABEL,
  RANGE_MS,
  BUCKETS,
  BUCKET_MS,
  bucketFor,
  cutInto,
  isRange,
  parseWindow,
  spanOf,
  spanWords,
  stepOf,
  windowEdges,
  windowLabel,
  windowParam,
} from "./range.ts";
import type { Offer } from "./range.ts";

/** A screen that offers everything, so a case can isolate one rule. */
const ANY: Offer = { ranges: RANGES, custom: true, fallback: "7d" };

describe("the window a URL asked for", () => {
  it("takes the values the control offers", () => {
    for (const value of RANGES) expect(parseWindow(value, ANY)).toBe(value);
  });

  it("falls back for anything outside the vocabulary", () => {
    // `#/cost?window=` is produced by a route transition for one render, and
    // read as a number it is 0 — a window whose two edges are the same
    // instant, which is half-open and names no rows at all.
    for (const bad of ["", "0", "3", "-7", "all", "NaN", "7", "3h"]) {
      expect(parseWindow(bad, ANY), bad).toBe("7d");
    }
  });

  it("falls back for a value this SCREEN does not offer", () => {
    // The rule the vocabulary's own set cannot state: `90d` is a window some
    // screen has, and a client-buffered strip is not one of them. Answering
    // it would put a heading the control cannot show over the rows, and the
    // reader could never get back to it.
    const strip: Offer = { ranges: ["15m", "1h", "6h"], custom: false, fallback: "1h" };
    expect(parseWindow("90d", strip)).toBe("1h");
    expect(parseWindow("6h", strip)).toBe("6h");
  });

  it("takes the caller's own fallback", () => {
    // A screen whose natural window is a day says so, rather than every
    // screen sharing one default because the helper picked it.
    expect(parseWindow("", { ...ANY, fallback: "1h" })).toBe("1h");
    expect(parseWindow("nonsense", { ...ANY, fallback: "30d" })).toBe("30d");
  });

  it("has a length and a label for every value, and for nothing else", () => {
    // The three are read together — a control renders the label, the chart
    // reads the length — so a value missing from either is a blank button or
    // an empty window.
    expect(Object.keys(RANGE_MS).sort()).toEqual([...RANGES].sort());
    expect(Object.keys(RANGE_LABEL).sort()).toEqual([...RANGES].sort());
  });
});

describe("the interval a reader named", () => {
  const SPAN = "2026-06-01T00:00:00.000Z/2026-06-08T00:00:00.000Z";

  it("reads the ISO interval form", () => {
    const got = parseWindow(SPAN, ANY);
    expect(isRange(got)).toBe(false);
    expect(got).toEqual({
      from: Date.parse("2026-06-01T00:00:00Z"),
      to: Date.parse("2026-06-08T00:00:00Z"),
    });
  });

  it("refuses an empty or inverted one", () => {
    // The same rule the empty `window=` taught: the engine's windows are
    // half-open, so `to <= from` names no rows at all and the screen would
    // render the engine's refusal rather than its data.
    for (const bad of [
      "2026-06-08T00:00:00Z/2026-06-01T00:00:00Z",
      "2026-06-01T00:00:00Z/2026-06-01T00:00:00Z",
      "2026-06-01T00:00:00Z/",
      "/2026-06-08T00:00:00Z",
      "2026-06-01T00:00:00Z",
      "not-a-date/also-not",
    ]) {
      expect(parseWindow(bad, ANY), bad).toBe("7d");
    }
  });

  it("refuses one on a screen that does not offer it", () => {
    expect(parseWindow(SPAN, { ...ANY, custom: false })).toBe("7d");
  });

  it("round-trips through the URL", () => {
    // A reader copying the URL copies the window, and reading it back has to
    // land on the same two instants — otherwise a shared link shows a
    // different chart from the one that was shared.
    const w = parseWindow(SPAN, ANY);
    expect(windowParam(w)).toBe(SPAN);
    expect(parseWindow(windowParam(w), ANY)).toEqual(w);
    for (const r of RANGES) expect(windowParam(r)).toBe(r);
  });

  it("is as long as its two instants are apart", () => {
    expect(spanOf(parseWindow(SPAN, ANY))).toBe(7 * 86_400_000);
    for (const r of RANGES) expect(spanOf(r), r).toBe(RANGE_MS[r]);
  });

  it("names itself by its edges rather than by a duration", () => {
    // A heading reading "7 days" over a window somebody chose in March is
    // the one thing a custom range must not say.
    expect(windowLabel("7d")).toBe(RANGE_LABEL["7d"]);
    expect(windowLabel(parseWindow(SPAN, ANY))).toContain("—");
  });
});

describe("the two windows a range names", () => {
  const NOW = new Date("2026-06-15T12:00:00Z").getTime();

  it("ends at the caller's own now", () => {
    const got = windowEdges("7d", NOW);
    expect(got.until).toBe("2026-06-15T12:00:00.000Z");
    expect(got.since).toBe("2026-06-08T12:00:00.000Z");
  });

  it("makes the comparison window the SAME WIDTH, ending where this one starts", () => {
    // A comparison whose two windows are different lengths compares two
    // numbers that do not answer the same question — which is how a 28-day
    // February outperforms every other month in every dashboard that gets
    // this wrong.
    for (const window of RANGES) {
      const got = windowEdges(window, NOW);
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
    const day = windowEdges("1d", NOW);
    const quarter = windowEdges("90d", NOW);
    expect(Date.parse(day.since) - Date.parse(day.previous.since)).toBe(86_400_000);
    expect(Date.parse(quarter.since) - Date.parse(quarter.previous.since)).toBe(90 * 86_400_000);
  });

  it("puts an interval exactly where it was named, whatever the clock says", () => {
    // A reader who named two instants asked for those instants. This is also
    // what makes a custom window a stable thing to link to: the tick that
    // advances every other clock on the screen must not move it.
    const w = parseWindow("2026-01-02T03:04:00.000Z/2026-01-09T03:04:00.000Z", ANY);
    for (const clock of [NOW, NOW + 86_400_000, 0]) {
      const got = windowEdges(w, clock, stepOf("day"));
      expect(got.since).toBe("2026-01-02T03:04:00.000Z");
      expect(got.until).toBe("2026-01-09T03:04:00.000Z");
      // And its comparison window is still the same width, ending where it
      // begins — the rule does not lapse because the reader typed the dates.
      expect(got.previous.until).toBe(got.since);
      expect(got.previous.since).toBe("2025-12-26T03:04:00.000Z");
    }
  });
});

describe("the bucket and the alignment", () => {
  const NOW2 = new Date("2026-06-15T12:34:56Z").getTime();

  it("buckets an hour and under by minute, a day by hour, longer by day", () => {
    // 30 days of hourly bars is 720 columns on a chart eight hundred pixels
    // wide, and one day of daily bars is a single column. The reader picks a
    // range; the bucket follows.
    for (const window of ["15m", "1h"] as const) {
      expect(bucketFor(window), window).toBe("minute");
    }
    for (const window of ["6h", "1d"] as const) {
      expect(bucketFor(window), window).toBe("hour");
    }
    for (const window of ["7d", "30d", "90d"] as const) {
      expect(bucketFor(window), window).toBe("day");
    }
  });

  it("coarsens to the set the caller's own question accepts, never past it", () => {
    // The spend series has two buckets and the event log has three, because a
    // minute bucket over a week is ten thousand points nobody can read and an
    // hour is the whole window of "what just happened". A screen declares what
    // its question takes; a value outside the engine's set is refused, not
    // guessed.
    const spend = ["hour", "day"] as const;
    expect(bucketFor("15m", spend)).toBe("hour");
    expect(bucketFor("1h", spend)).toBe("hour");
    expect(bucketFor("1d", spend)).toBe("hour");
    expect(bucketFor("30d", spend)).toBe("day");
    // And never FINER than asked for, whichever way the window leans.
    expect(bucketFor("15m", ["day"])).toBe("day");
    expect(bucketFor("90d", ["minute"])).toBe("minute");
  });

  it("buckets an interval by how long it is, not by how it was written", () => {
    const short = parseWindow("2026-06-15T00:00:00Z/2026-06-15T06:00:00Z", ANY);
    const long = parseWindow("2026-06-01T00:00:00Z/2026-06-15T00:00:00Z", ANY);
    const tiny = parseWindow("2026-06-15T00:00:00Z/2026-06-15T00:20:00Z", ANY);
    expect(bucketFor(tiny)).toBe("minute");
    expect(bucketFor(short)).toBe("hour");
    expect(bucketFor(long)).toBe("day");
  });

  it("aligns the top edge to the bucket in progress when asked", () => {
    // The current hour belongs on a chart while it is still being spent, and
    // the window's identity — and therefore the query — should change once
    // per column rather than once per second.
    const aligned = windowEdges("6h", NOW2, stepOf("hour"));
    expect(aligned.until).toBe("2026-06-15T13:00:00.000Z");
    expect(aligned.since).toBe("2026-06-15T07:00:00.000Z");
  });

  it("ends at the caller's own instant when not", () => {
    // A LIST has no columns to align to, and rounding its window up would ask
    // the engine for rows that do not exist yet.
    const plain = windowEdges("6h", NOW2);
    expect(plain.until).toBe("2026-06-15T12:34:56.000Z");
  });

  it("keeps the comparison window the same width once aligned", () => {
    const aligned = windowEdges("7d", NOW2, stepOf("day"));
    const width = (a: string, b: string) => Date.parse(b) - Date.parse(a);
    expect(width(aligned.since, aligned.until)).toBe(RANGE_MS["7d"]);
    expect(width(aligned.previous.since, aligned.previous.until)).toBe(RANGE_MS["7d"]);
    expect(aligned.previous.until).toBe(aligned.since);
  });

  it("has a step for every bucket, and they differ", () => {
    expect(stepOf("minute")).toBe(60_000);
    expect(stepOf("hour")).toBe(3_600_000);
    expect(stepOf("day")).toBe(86_400_000);
    expect(Object.keys(BUCKET_MS).sort()).toEqual([...BUCKETS].sort());
    // FINEST FIRST is what [bucketFor]'s coarsening walks, so the order is
    // load-bearing rather than cosmetic.
    expect([...BUCKETS]).toEqual([...BUCKETS].sort((a, b) => BUCKET_MS[a] - BUCKET_MS[b]));
  });
});

describe("what an answer's own window is called", () => {
  const label = (since: string, until: string) => spanWords(since, until);

  it("uses the vocabulary's word when the span is one of them", () => {
    // The heading over the figures and the control that produced it have to
    // read the same, or a reader comparing them sees two windows.
    expect(label("2026-06-08T12:00:00Z", "2026-06-15T12:00:00Z")).toBe(RANGE_LABEL["7d"]);
    expect(label("2026-06-15T11:00:00Z", "2026-06-15T12:00:00Z")).toBe(RANGE_LABEL["1h"]);
  });

  it("names any other span in whole units, marked when it is not exact", () => {
    expect(label("2026-06-01T00:00:00Z", "2026-06-04T00:00:00Z")).toBe("3 days");
    expect(label("2026-06-01T00:00:00Z", "2026-06-04T00:00:01Z")).toBe("3+ days");
    expect(label("2026-06-01T00:00:00Z", "2026-06-01T02:00:00Z")).toBe("2 hours");
    expect(label("2026-06-01T00:00:00Z", "2026-06-02T00:00:00Z")).toBe(RANGE_LABEL["1d"]);
  });

  it("drops to the unit the span divides into rather than saying 'and a bit'", () => {
    // An engine that snaps a histogram's edges out to whole buckets answers a
    // "24 hours" window covering twenty-five of them. "1+ days" is true and
    // tells a reader nothing; "25 hours" says what the bars add up to.
    expect(label("2026-06-01T13:00:00Z", "2026-06-02T14:00:00Z")).toBe("25 hours");
    expect(label("2026-06-01T13:00:00Z", "2026-06-01T14:30:00Z")).toBe("90 minutes");
    // And a span nothing divides is still marked rather than rounded away.
    expect(label("2026-06-01T00:00:00Z", "2026-06-04T01:00:30Z")).toBe("3+ days");
  });

  it("says so rather than inventing a window it cannot read", () => {
    // A rollup that arrived without its window is a rollup whose numbers have
    // no heading, and "0 days" is the reading that looks like data.
    expect(label("", "2026-06-15T12:00:00Z")).toBe("an unknown window");
    expect(label("2026-06-15T12:00:00Z", "2026-06-15T12:00:00Z")).toBe("no window at all");
    expect(label("2026-06-15T12:00:00Z", "2026-06-15T11:00:00Z")).toBe("no window at all");
    expect(label("2026-06-15T12:00:00Z", "2026-06-15T12:00:30Z")).toBe("under a minute");
  });
});

describe("cutting a window into columns", () => {
  it("covers exactly the window, at every range the strip offers", () => {
    // BOTH HALVES MOVE. A fixed column COUNT made fifteen minutes into sixty
    // one-minute columns — an hour of strip under a heading that said fifteen
    // minutes, and the 15m and 6h strips rendered identically.
    for (const window of ["15m", "1h", "6h", "1d"] as const) {
      const { cell, cells } = cutInto(RANGE_MS[window], 60);
      expect(cell * cells, window).toBe(RANGE_MS[window]);
      expect(cells, window).toBeLessThanOrEqual(60);
    }
  });

  it("widens the column rather than adding columns past the cap", () => {
    // A fixed column WIDTH makes six hours 360 sub-pixel slivers, which renders
    // as one flat smear rather than as a strip.
    expect(cutInto(RANGE_MS["15m"], 60)).toEqual({ cell: 60_000, cells: 15 });
    expect(cutInto(RANGE_MS["1h"], 60)).toEqual({ cell: 60_000, cells: 60 });
    expect(cutInto(RANGE_MS["6h"], 60)).toEqual({ cell: 6 * 60_000, cells: 60 });
    expect(cutInto(RANGE_MS["1d"], 60)).toEqual({ cell: 24 * 60_000, cells: 60 });
  });

  it("never cuts below a whole minute", () => {
    // A column boundary is a wall-clock instant a reader can point at, not an
    // offset from whenever the tab happened to load.
    const { cell, cells } = cutInto(5 * 60_000, 60);
    expect(cell).toBe(60_000);
    expect(cells).toBe(5);
  });

  it("is a strip of one rather than of none for a window under a minute", () => {
    expect(cutInto(30_000, 60)).toEqual({ cell: 60_000, cells: 1 });
    expect(cutInto(0, 60)).toEqual({ cell: 60_000, cells: 1 });
  });
});
