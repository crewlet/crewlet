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
import { BANDS, PHASE_BANDS } from "~/contract/spend.ts";
import type { TokenSeries } from "~/protocol/types.ts";

/**
 * The rows of a breakdown of WHERE TOKENS WENT: the buckets that spent some.
 *
 * A bucket with calls and no tokens is not somewhere tokens went — the engine
 * answers one for calls that failed before any model came back, which recorded
 * no model and no usage, and names it as it names every dimension an event did
 * not carry. Listed, it was a bar of nothing named "unknown · 0 · 7 calls"
 * under BY MODEL, as if a model called that had answered seven times. Those
 * calls are the turns' failures, and the turn list says so.
 */
export function spentOnly<T extends { total_tokens: number }>(rows: readonly T[]): T[] {
  return rows.filter((row) => row.total_tokens > 0);
}

/**
 * A phase band's colour: its data hue, from `BANDS`.
 *
 * The ENGINE folds every phase into one of four bands (`tokens.PhaseBand`) and
 * the contract gives each its hue, so a band is the same colour on every chart
 * whatever the window's biggest band was. Anything else is the residual.
 */
export function bandColor(band: string): string {
  const found = BANDS.find((b) => b.value === band);
  return found ? dataColor(found.series - 1) : DATA_COLOR_OTHER;
}

/**
 * A PHASE's colour: the hue of the band the engine folds it into.
 *
 * `PHASE_BANDS` is `tokens.PhaseBand` by phase, gated against it, so a phase
 * is drawn in one colour on every chart — and a phase this build has never
 * heard of takes the Auxiliary band's, which is where the engine counts it.
 */
export function phaseColor(phase: string): string {
  const key = (phase || "").toLowerCase();
  const band: string = Object.hasOwn(PHASE_BANDS, key)
    ? PHASE_BANDS[key as keyof typeof PHASE_BANDS]
    : "auxiliary";
  return bandColor(band);
}

/** A phase band's label, or the key itself for a band this build has not met. */
export function bandLabel(band: string): string {
  return BANDS.find((b) => b.value === band)?.label ?? band;
}

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
 * EXCEPT WHERE THE VALUE HAS A COLOUR OF ITS OWN. A PHASE BAND does:
 * grouped by phase — this screen's DEFAULT — the engine answers its four
 * bands, and `bandColor` gives each the hue `BANDS` assigns it. Positionally,
 * `dataColor(i)` is keyed on a band's ORDER, and a band that changed colour
 * whenever the window changed which one was biggest is a legend nobody can
 * learn.
 *
 * THE ORDER IS NOT RE-DERIVED. `by_group` is already in the engine's order —
 * biggest-first with the residual last, or the phase bands' stacking order —
 * and re-sorting here would put a residual larger than the last band ahead of
 * it, at which point it stops meaning "the rest".
 *
 * The residual is identified by its FLAG, never by its name: it carries an
 * empty key precisely so that a phase, a model or a seat genuinely called
 * "other" cannot be mistaken for it.
 *
 * A SEAT OR UNIT BAND IS LABELLED WITH THE ENGINE'S `label`. The engine keys
 * a seat band on its agent id — the identity a rename does not move — and a
 * unit band on its key, and sends the NAME beside each, resolved against the
 * chart where it answered: a legend of ids beside a table of names names one
 * seat two ways on one screen, and a resolver here would be a second, later
 * reading of the chart than the one the figures were cut against.
 */
export function bandsOf(series: TokenSeries | null, group?: string): Band[] {
  return (series?.by_group ?? []).map((b, i) => ({
    key: b.other ? "" : b.group,
    label: b.other
      ? `other (${b.folded})`
      : group === "phase"
        ? bandLabel(b.group)
        : b.label || b.group,
    // THE RESIDUAL IS NEVER A BAND. It is "the rest", so it keeps the
    // residual hue whatever the grouping is — a fold of three models drawn
    // in one of their colours would name one of them.
    color: b.other ? DATA_COLOR_OTHER : group === "phase" ? bandColor(b.group) : dataColor(i),
    total: b.total_tokens,
  }));
}

/**
 * How much of the window falls under no band at all.
 *
 * Grouping by worker leaves out every phase that is not a worker's. A chart
 * whose bands sum to less than the company's total, with nothing said, reads
 * as spend that went missing — so this is a number the screen states rather
 * than a gap a reader discovers by comparing two screens.
 */
export function unbandedTokens(series: TokenSeries | null): number {
  if (!series) return 0;
  return Math.max(0, series.totals.total_tokens - series.grouped.total_tokens);
}
