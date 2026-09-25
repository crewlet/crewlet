import { describe, expect, it } from "vitest";
import { DATA_COLOR_OTHER, dataColor } from "@crewlethq/ui";

import {
  bandColor,
  bandLabel,
  bandsOf,
  columnsOf,
  ghostHeights,
  spendDays,
  unbandedTokens,
} from "./spend.ts";
import { BANDS } from "~/contract/spend.ts";
import type { Bucket, SeriesPoint, TokenSeries } from "~/protocol/types.ts";

function bucket(total: number, extra: Partial<Bucket> = {}): Bucket {
  return {
    input_tokens: total,
    output_tokens: 0,
    total_tokens: total,
    calls: total > 0 ? 1 : 0,
    ...extra,
  };
}

function series(over: Partial<TokenSeries> = {}): TokenSeries {
  return {
    group: "phase",
    bucket: "day",
    since: "2026-06-14T00:00:00Z",
    until: "2026-06-17T00:00:00Z",
    from: "2026-06-14",
    to: "2026-06-16",
    days: 3,
    horizon: { days: 181, floor: "2025-12-15" },
    series: [],
    by_group: [],
    totals: bucket(0),
    grouped: bucket(0),
    ...over,
  };
}

/** One day's point. */
function point(at: string, b: Bucket, over: Partial<SeriesPoint> = {}): SeriesPoint {
  return { ...b, at, window: at.slice(0, 10), days: 1, groups: {}, other: bucket(0), ...over };
}

describe("the legend", () => {
  it("keeps the engine's order and never re-sorts the residual into it", () => {
    // `by_group` is already biggest-first with the residual LAST. A residual
    // larger than the fifth band would jump the legend if this re-sorted, at
    // which point it stops meaning "the rest".
    const bands = bandsOf(
      series({
        by_group: [
          { ...bucket(100), group: "plan", other: false, folded: 0 },
          { ...bucket(999), group: "", other: true, folded: 12 },
        ],
      }),
    );
    expect(bands.map((b) => b.label)).toEqual(["plan", "other (12)"]);
  });

  it("identifies the residual by its flag, never by its name", () => {
    // A phase genuinely called "other" must not be drawn as the fold.
    const bands = bandsOf(
      series({
        by_group: [
          { ...bucket(10), group: "other", other: false, folded: 0 },
          { ...bucket(5), group: "", other: true, folded: 3 },
        ],
      }),
    );
    expect(bands[0]).toMatchObject({ key: "other", label: "other" });
    expect(bands[1]).toMatchObject({ key: "", label: "other (3)" });
    expect(bands[0]?.color).not.toBe(bands[1]?.color);
  });

  // ONE COLOUR PER BAND, from the contract, whatever the window's order.
  //
  // The engine folds every phase into four bands and `BANDS` gives each its
  // data hue. Positionally, `dataColor(i)` is keyed on a band's ORDER, so a
  // band would change colour whenever a window changed which one was biggest.
  it("draws a phase band in its own hue, not by its position", () => {
    const by_group = [
      { ...bucket(100), group: "execute", other: false, folded: 0 },
      { ...bucket(50), group: "workers", other: false, folded: 0 },
      { ...bucket(9), group: "", other: true, folded: 2 },
    ];
    const bands = bandsOf(series({ by_group }), "phase");
    expect(bands[0]).toMatchObject({ label: "Execute", color: dataColor(0) });
    expect(bands[1]).toMatchObject({ label: "Workers", color: dataColor(2) });
    // THE RESIDUAL IS NEVER A BAND.
    expect(bands[2]?.color).toBe(DATA_COLOR_OTHER);

    // Every OTHER grouping keeps the positional ramp: a seat, a model and a
    // unit have no colour of their own.
    const seats = bandsOf(series({ by_group }), "seat");
    expect(seats[1]?.color).toBe(dataColor(1));
    expect(seats[1]?.label).toBe("workers");
  });

  // THE CONTRACT'S HUES ARE ITS STACKING ORDER: `series` is the 1-based data
  // hue, and a table whose second row took the first hue would draw two bands
  // in one colour.
  it("numbers the bands' hues in stacking order", () => {
    expect(BANDS.map((b) => b.series)).toEqual([1, 2, 3, 4]);
    expect(bandColor("a-band-this-build-never-met")).toBe(DATA_COLOR_OTHER);
    expect(bandLabel("a-band-this-build-never-met")).toBe("a-band-this-build-never-met");
  });
});

describe("the days a window asks for", () => {
  it("is the named range's own count of company days", () => {
    expect(spendDays("1d")).toBe(1);
    expect(spendDays("7d")).toBe(7);
    expect(spendDays("90d")).toBe(90);
  });
});

describe("the columns", () => {
  const s = series({
    by_group: [
      { ...bucket(30), group: "plan", other: false, folded: 0 },
      { ...bucket(4), group: "", other: true, folded: 2 },
    ],
    series: [
      point("2026-06-14T00:00:00Z", bucket(30), { groups: { plan: bucket(30) } }),
      point("2026-06-15T00:00:00Z", bucket(0)),
      point("2026-06-16T00:00:00Z", bucket(4), { other: bucket(4) }),
    ],
  });

  it("keeps the empty buckets, so a quiet day is a gap and not a missing column", () => {
    const cols = columnsOf(s, bandsOf(s));
    expect(cols.map((c) => c.at)).toHaveLength(3);
    expect(cols[1]).toMatchObject({ total: 0, parts: [] });
  });

  it("drops a segment worth nothing but keeps the engine's own total", () => {
    const cols = columnsOf(s, bandsOf(s));
    expect(cols[0]?.parts).toEqual([{ key: "plan", value: 30 }]);
    expect(cols[2]?.parts).toEqual([{ key: "", value: 4 }]);
    expect(cols[2]?.total).toBe(4);
  });

  it("orders each column's segments the way the legend reads", () => {
    const two = series({
      by_group: [
        { ...bucket(9), group: "b", other: false, folded: 0 },
        { ...bucket(1), group: "a", other: false, folded: 0 },
      ],
      series: [
        point("2026-06-14T00:00:00Z", bucket(10), { groups: { a: bucket(1), b: bucket(9) } }),
      ],
    });
    expect(columnsOf(two, bandsOf(two))[0]?.parts.map((p) => p.key)).toEqual(["b", "a"]);
  });
});

describe("what falls under no band", () => {
  it("is the gap between the window's total and what the bands cover", () => {
    // Grouping by worker leaves out every phase that is not a worker's. A
    // chart whose bands sum to less than the total, with nothing said, reads
    // as spend that went missing.
    expect(unbandedTokens(series({ totals: bucket(100), grouped: bucket(30) }))).toBe(70);
  });

  it("is never negative, whatever the wire says", () => {
    expect(unbandedTokens(series({ totals: bucket(10), grouped: bucket(40) }))).toBe(0);
  });
});

describe("the ghost", () => {
  it("is the prior window's heights in order, since the two share no instant", () => {
    const prior = series({
      series: [point("2026-06-10T00:00:00Z", bucket(5)), point("2026-06-11T00:00:00Z", bucket(7))],
    });
    expect(ghostHeights(prior)).toEqual([5, 7]);
    expect(ghostHeights(null)).toEqual([]);
  });
});
