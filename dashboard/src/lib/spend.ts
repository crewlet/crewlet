/**
 * Turning the engine's spend series into what a chart draws.
 *
 * PURE FUNCTIONS OVER VALUES, in this file rather than inline in the screen,
 * for the reason the engine gives for the same shape one layer down: a
 * derivation that can only be exercised by rendering a component is one nobody
 * re-measures, and every rule here is a rule about a number a reader will act
 * on.
 *
 * The bucketing itself is NOT here and must not come here. The browser holds
 * at most the live window's records, so an axis folded client-side would be
 * correct for one range and absent for every other — which is the fourth copy
 * of an aggregation `internal/tokens` exists to have made one.
 *
 * The WINDOW is not here either any more. This file held Cost's own three-day
 * vocabulary and its own `bucketFor`, which is how a screen showing the last
 * hour of spend and a screen showing the last hour of events came to disagree
 * about how long an hour was. `lib/range.ts` is the one vocabulary now.
 */

import { DATA_COLOR_OTHER, dataColor } from "@crewlethq/ui";
import { phaseColor } from "~/ui/charts.tsx";
import type { SeriesPoint, TokenSeries } from "~/protocol/types.ts";

export interface Band {
  /** The band's key in a point's `groups`. Empty on, and only on, the residual. */
  key: string;
  label: string;
  color: string;
  total: number;
}

/**
 * The legend, in the engine's own order.
 *
 * THE COLOURS ARE uilet's DATA RAMP — five hues and a residual, measured by
 * the package's own palette suite — rather than a ramp of ours beside it. A
 * band's colour is read by three surfaces here (the legend, the stacked
 * columns and the per-phase bar list), so a second spelling of the same five
 * values is the drift `textcut` and `whsec` are named after in this repo.
 *
 * EXCEPT WHERE THE VALUE HAS A COLOUR OF ITS OWN. A PHASE does: `phaseColor`
 * is what the by-phase bar list, the per-seat share bar and the turn page all
 * draw it in. Grouped by phase — which is this screen's DEFAULT — the time
 * chart drew `execute` from the positional ramp and the panel below it drew
 * the same `execute` from the phase palette, so one screen carried two
 * legends that disagreed about the same three words. The positional half was
 * the worse of the two on its own terms as well: `dataColor(i)` is keyed on a
 * band's ORDER, and `by_group` is biggest-first, so a phase changed colour
 * whenever the window changed which phase was biggest.
 *
 * THE ORDER IS NOT RE-DERIVED. `by_group` is already biggest-first with the
 * residual last, and re-sorting here would put a residual larger than the
 * fifth band ahead of it — at which point it stops meaning "the rest".
 *
 * The residual is identified by its FLAG, never by its name: it carries an
 * empty key precisely so that a phase, a model or a seat genuinely called
 * "other" cannot be mistaken for it.
 */
export function bandsOf(series: TokenSeries | null, group?: string): Band[] {
  return (series?.by_group ?? []).map((b, i) => ({
    key: b.other ? "" : b.group,
    label: b.other ? `other (${b.folded})` : b.group,
    // THE RESIDUAL IS NEVER A PHASE. It is "the rest", so it keeps the
    // residual hue whatever the grouping is — a fold of three phases drawn
    // in one of their colours would name one of them.
    color: b.other ? DATA_COLOR_OTHER : group === "phase" ? phaseColor(b.group) : dataColor(i),
    total: b.total_tokens,
  }));
}

export interface Column {
  at: string;
  total: number;
  parts: { key: string; value: number }[];
}

/**
 * One column per bucket, its segments in the legend's order.
 *
 * EVERY BUCKET IS KEPT, including the empty ones: the engine returns the whole
 * window so a quiet hour is a gap of full height rather than a column the
 * chart squeezed out, and dropping them here would undo exactly that.
 *
 * A segment worth nothing is dropped — a zero-height span still renders a
 * border in some engines and puts a meaningless row in the tooltip — but the
 * column's own `total` is the engine's, never the sum of the parts: a band
 * past the cap is in the total and in the residual, and re-summing would be a
 * second answer to a question the wire already answered.
 */
export function columnsOf(series: TokenSeries | null, bands: Band[]): Column[] {
  const keys = bands.filter((b) => b.key !== "").map((b) => b.key);
  return (series?.series ?? []).map((p: SeriesPoint) => ({
    at: p.at,
    total: p.total_tokens,
    parts: [
      ...keys.map((key) => ({ key, value: p.groups[key]?.total_tokens ?? 0 })),
      { key: "", value: p.other.total_tokens },
    ].filter((part) => part.value > 0),
  }));
}

/**
 * How much of the window falls under no band at all.
 *
 * Grouping by worker leaves out every phase that is not a worker's, and
 * grouping by turn every phase that carried no turn id. A chart whose bands
 * sum to less than the company's total, with nothing said, reads as spend that
 * went missing — so this is a number the screen states rather than a gap a
 * reader discovers by comparing two screens.
 */
export function unbandedTokens(series: TokenSeries | null): number {
  if (!series) return 0;
  return Math.max(0, series.totals.total_tokens - series.grouped.total_tokens);
}

/**
 * The prior period's column heights, aligned by POSITION.
 *
 * Exact rather than matched on the timestamp: both windows are the same length
 * in the same bucket, so column i of one is column i of the other — and a
 * match on `at` could never work, since the two windows share no instant.
 */
export function ghostHeights(prior: TokenSeries | null): number[] {
  return (prior?.series ?? []).map((p) => p.total_tokens);
}
