import { describe, expect, it } from "vitest";
import { DATA_COLOR_OTHER } from "@crewlethq/ui";

import { bandsOf, columnsOf, ghostHeights, unbandedTokens } from "./spend.ts";
import { phaseColor } from "~/ui/charts.tsx";
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

  // ONE SCREEN, ONE COLOUR PER PHASE.
  //
  // Grouped by phase — which is this screen's default — the time chart drew
  // its bands from the positional data ramp while the panel directly below it
  // drew the same phases from `phaseColor`. Two legends on one screen,
  // disagreeing about the same three words, so a reader who learned "execute
  // is indigo" from the lower one read the upper one wrong.
  //
  // The positional half was worse than inconsistent: `dataColor(i)` is keyed
  // on a band's ORDER and `by_group` is biggest-first, so a phase changed
  // colour whenever the window changed which phase was biggest.
  it("draws a phase in the phase palette, not by its position", () => {
    const by_group = [
      { ...bucket(100), group: "execute", other: false, folded: 0 },
      { ...bucket(50), group: "review", other: false, folded: 0 },
      { ...bucket(9), group: "", other: true, folded: 2 },
    ];
    const phases = bandsOf(series({ by_group }), "phase");
    expect(phases[0]?.color).toBe(phaseColor("execute"));
    expect(phases[1]?.color).toBe(phaseColor("review"));
    // THE RESIDUAL IS NEVER A PHASE. It is "the rest", so a fold of three
    // phases drawn in one of their colours would name one of them.
    //
    // Belt and braces rather than load-bearing, and the comment says so
    // because mutating the branch order does not turn this red: the residual
    // carries an EMPTY group, and `phaseColor` falls back to exactly this hue
    // for a phase it does not know. What the assertion holds is the property;
    // what protects it is two independent reasons, which is the right number
    // for the one band that must not be mistaken for a value.
    expect(phases[2]?.color).toBe(DATA_COLOR_OTHER);

    // AND THE ORDER NO LONGER DECIDES IT: the same phase keeps its colour when
    // the window puts it second.
    const swapped = bandsOf(
      series({ by_group: [by_group[1]!, by_group[0]!, by_group[2]!] }),
      "phase",
    );
    expect(swapped.find((b) => b.key === "execute")?.color).toBe(phases[0]?.color);

    // Every OTHER grouping keeps the positional ramp: a seat, a model and a
    // unit have no colour of their own, and giving them one would be colour
    // carrying identity.
    const seats = bandsOf(series({ by_group }), "seat");
    expect(seats[0]?.color).not.toBe(phaseColor("execute"));
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
