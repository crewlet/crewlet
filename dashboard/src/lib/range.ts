/**
 * A time window as a URL carries it, and as the engine takes it.
 *
 * Three screens ask the same question of three different answers — the spend
 * series, the turns list, the activity feed — and each had its own spelling of
 * "the last N days". The engine takes two instants; a reader picks a word. This
 * is the one translation between them.
 *
 * THE VALUE IS A CLOSED SET, never a free-form number, and `lib/spend.ts` is
 * where that was learned: `#/cost?window=` — which a route transition produces
 * for one render — read as `Number("") === 0`, so the screen asked for a window
 * whose two edges were the same instant. Half-open, that names no rows at all;
 * the engine refused it and the chart rendered the refusal on a screen whose
 * data was fine. A value outside the set is the DEFAULT rather than a number
 * derived from it: `window=3` is not a window any screen here has, and
 * answering it with three days would put a heading the control cannot show
 * over the rows.
 */

import { useMemo } from "react";
import { useParam } from "~/app/router.tsx";

/** The windows every time-ranged screen offers, in the order a control reads. */
export const RANGES = ["1", "7", "30", "90"] as const;
export type Range = (typeof RANGES)[number];

/** What each one is called. */
export const RANGE_LABEL: Record<Range, string> = {
  "1": "24 hours",
  "7": "7 days",
  "30": "30 days",
  "90": "90 days",
};

/** The value a URL asked for, as one of the set. */
export function rangeOf(raw: string, fallback: Range = "7"): Range {
  return (RANGES as readonly string[]).includes(raw) ? (raw as Range) : fallback;
}

/** A window, as the engine's two parameters. */
export interface TimeRange {
  /** The chosen word, for a control and a heading. */
  window: Range;
  /** The inclusive start, RFC3339. */
  since: string;
  /** The exclusive end, RFC3339 — always the caller's own now. */
  until: string;
  /** The window immediately before this one, for a compare-to-previous. */
  previous: { since: string; until: string };
  /** Set the window, which is a SECTION rather than a filter: a reader who
   *  widened the range and pressed back means the narrower one. */
  set: (next: Range) => void;
}

/**
 * The two windows a range names, as instants.
 *
 * SEPARATED FROM THE HOOK because this is the arithmetic and the hook is the
 * URL binding: a claim about where a comparison window sits should not need a
 * router and a render to exercise.
 *
 * THE PREVIOUS WINDOW IS THE SAME WIDTH ENDING WHERE THIS ONE STARTS, never
 * "the same dates a month ago". A comparison whose two windows are different
 * lengths compares two numbers that do not answer the same question, which is
 * how a 28-day February outperforms every other month in every dashboard that
 * gets it wrong.
 */
export function rangeWindows(window: Range, now: number): Omit<TimeRange, "set"> {
  const days = Number(window);
  const span = days * 86_400_000;
  const since = new Date(now - span);
  const before = new Date(now - span * 2);
  return {
    window,
    since: since.toISOString(),
    until: new Date(now).toISOString(),
    previous: { since: before.toISOString(), until: since.toISOString() },
  };
}

/** `window=` as two instants, and the setter that moves it. */
export function useTimeRange(now: number, fallback: Range = "7"): TimeRange {
  const [raw, set] = useParam("window", fallback, "section");
  const window = rangeOf(raw, fallback);
  return useMemo(() => ({ ...rangeWindows(window, now), set }), [window, now, set]);
}
