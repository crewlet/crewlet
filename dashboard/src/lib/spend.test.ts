import { describe, expect, it } from "vitest";
import {
  SPEND_WINDOWS,
  bandsOf,
  bucketFor,
  columnsOf,
  ghostHeights,
  spendWindow,
  unbandedTokens,
} from "./spend.ts";
import type { Bucket, TokenSeries } from "~/protocol/types.ts";

function bucket(total: number, extra: Partial<Bucket> = {}): Bucket {
  return {
    input_tokens: total,
    output_tokens: 0,
    total_tokens: total,
    calls: total > 0 ? 1 : 0,
    cost_usd: 0,
    priced_calls: 0,
    ...extra,
  };
}

function series(over: Partial<TokenSeries> = {}): TokenSeries {
  return {
    group: "phase",
    bucket: "hour",
    since: "2026-06-14T12:00:00Z",
    until: "2026-06-14T14:00:00Z",
    series: [],
    by_group: [],
    totals: bucket(0),
    grouped: bucket(0),
    ...over,
  };
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
});

describe("the columns", () => {
  const s = series({
    by_group: [
      { ...bucket(30), group: "plan", other: false, folded: 0 },
      { ...bucket(4), group: "", other: true, folded: 2 },
    ],
    series: [
      {
        ...bucket(30),
        at: "2026-06-14T12:00:00Z",
        groups: { plan: bucket(30) },
        other: bucket(0),
      },
      { ...bucket(0), at: "2026-06-14T13:00:00Z", groups: {}, other: bucket(0) },
      { ...bucket(4), at: "2026-06-14T14:00:00Z", groups: {}, other: bucket(4) },
    ],
  });

  it("keeps the empty buckets, so a quiet hour is a gap and not a missing column", () => {
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
        {
          ...bucket(10),
          at: "2026-06-14T12:00:00Z",
          groups: { a: bucket(1), b: bucket(9) },
          other: bucket(0),
        },
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
      series: [
        { ...bucket(5), at: "2026-06-14T10:00:00Z", groups: {}, other: bucket(0) },
        { ...bucket(7), at: "2026-06-14T11:00:00Z", groups: {}, other: bucket(0) },
      ],
    });
    expect(ghostHeights(prior)).toEqual([5, 7]);
    expect(ghostHeights(null)).toEqual([]);
  });
});

describe("the window control", () => {
  // A SEGMENTED CONTROL'S VALUE IS A CLOSED SET. Reading it as a free-form
  // number let a URL decide the shape of the request: `#/cost?window=` gave
  // `Number("") === 0`, so the screen asked for a window whose two edges were
  // the same instant — which the engine refuses, half-open, and the chart
  // rendered as "the engine refused this request" on data that was fine.
  it("falls back to the default for anything outside its own set", () => {
    expect(spendWindow("")).toBe("1");
    expect(spendWindow("3")).toBe("1");
    expect(spendWindow("nonsense")).toBe("1");
    expect(spendWindow("0")).toBe("1");
  });

  it("keeps a window it does offer", () => {
    for (const w of SPEND_WINDOWS) expect(spendWindow(w)).toBe(w);
  });

  it("buckets a day by the hour and anything longer by the day", () => {
    expect(bucketFor("1")).toBe("hour");
    expect(bucketFor("7")).toBe("day");
    expect(bucketFor("30")).toBe("day");
  });
});
