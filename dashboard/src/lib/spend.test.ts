import { describe, expect, it } from "vitest";
import { DATA_COLOR_OTHER, dataColor } from "@crewlethq/ui";

import { bandColor, bandLabel, bandsOf, phaseColor, spentOnly, unbandedTokens } from "./spend.ts";
import { BANDS, PHASE_BANDS } from "~/contract/spend.ts";
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

  // A SEAT IS NAMED AS THE REST OF THE SCREEN NAMES IT: the engine keys the
  // band on the handle, and the legend reads the chart's name for it — the
  // handle only for a seat the chart no longer has. Any other grouping keeps
  // the engine's own label.
  it("labels a seat band with the seat's name, and nothing else", () => {
    const by_group = [
      { ...bucket(90), group: "agent-pm", handle: "agent-pm", other: false, folded: 0 },
      { ...bucket(10), group: "gone", other: false, folded: 0 },
    ];
    const names: Record<string, string> = { "agent-pm": "Agent PM" };
    const nameOf = (h: string) => names[h] ?? h;
    expect(bandsOf(series({ by_group }), "seat", nameOf).map((b) => b.label)).toEqual([
      "Agent PM",
      "gone",
    ]);
    expect(bandsOf(series({ by_group }), "model", nameOf).map((b) => b.label)).toEqual([
      "agent-pm",
      "gone",
    ]);
  });

  // THE CONTRACT'S HUES ARE ITS STACKING ORDER: `series` is the 1-based data
  // hue, and a table whose second row took the first hue would draw two bands
  // in one colour.
  it("numbers the bands' hues in stacking order", () => {
    expect(BANDS.map((b) => b.series)).toEqual([1, 2, 3, 4]);
    expect(bandColor("a-band-this-build-never-met")).toBe(DATA_COLOR_OTHER);
    expect(bandLabel("a-band-this-build-never-met")).toBe("a-band-this-build-never-met");
  });

  // A PHASE IS DRAWN IN ITS BAND'S HUE. The design system removed its phase
  // family, so inside a figure a phase is a series — and the only series that
  // keeps one phase one colour on every chart is the band the engine already
  // folds it into: a coding run is Execute on the Spend chart and in the live
  // list alike, and a newer peer's phase is Auxiliary, where the engine counts
  // it. Four hues, and nothing outside the ramp.
  it("draws a phase in the hue of the band it folds into", () => {
    expect(phaseColor("execute")).toBe(dataColor(0));
    expect(phaseColor("Sandbox")).toBe(dataColor(0));
    expect(phaseColor("review")).toBe(dataColor(1));
    expect(phaseColor("subagent")).toBe(dataColor(2));
    for (const aux of ["auxiliary", "judge", "onboarding", "", "a-phase-from-a-newer-peer"]) {
      expect(phaseColor(aux)).toBe(dataColor(3));
    }
    // Never a key of the object's prototype read as a band.
    expect(phaseColor("constructor")).toBe(dataColor(3));
    const drawn = new Set(Object.keys(PHASE_BANDS).map(phaseColor));
    expect([...drawn].sort()).toEqual([0, 1, 2, 3].map(dataColor).sort());
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

// A BREAKDOWN OF WHERE TOKENS WENT lists what spent some: the calls that failed
// before a model answered recorded none, and a bar of nothing named "unknown"
// read as a model that had answered them.
describe("a breakdown of where tokens went", () => {
  it("keeps every bucket that spent, in its order, and drops the ones that spent nothing", () => {
    const rows = [
      { model: "stub-sonnet", total_tokens: 1_900_000, calls: 111 },
      { model: "unknown", total_tokens: 0, calls: 7 },
      { model: "stub-haiku", total_tokens: 12, calls: 1 },
    ];
    expect(spentOnly(rows).map((r) => r.model)).toEqual(["stub-sonnet", "stub-haiku"]);
    expect(spentOnly([])).toEqual([]);
  });
});
