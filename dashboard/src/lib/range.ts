/**
 * A time window as a URL carries it, and as the engine takes it.
 *
 * ONE VOCABULARY for every screen that has a range: the spend series, the
 * activity strip, the event log, the turns list. Each had its own — Spend's
 * `window=` in whole days, the live strip's `STRIP_MINUTES` constant, and the
 * event log's implicit "whatever happens to be loaded" — so three screens
 * showing the same hour disagreed about how long an hour was and none of them
 * could be linked to the others.
 *
 * THE VALUE IS A CLOSED SET, never a free-form number, and `lib/spend.ts` is
 * where that was learned: `?window=`, which a route transition produces for one
 * render, read as `Number("") === 0` — a window whose two edges are the same
 * instant. Half-open, that names no rows at all; the engine refused it, and
 * the chart rendered the refusal on a screen whose data was fine.
 *
 * THE SET IS THE SCREEN'S, not the vocabulary's. A screen declares which
 * windows it has and whether an explicit interval is one of them, and anything
 * the URL names outside that is the screen's own fallback — because a value
 * the control cannot show would put a heading nobody chose over the rows, and
 * a reader who then pressed the control could never get back to it.
 *
 * ONE PARAMETER, still, for the custom case: `window=<from>/<to>` is ISO 8601's
 * own interval spelling, so a reader copying a URL copies one value and a
 * screen reading it branches in one place. Two parameters would have made
 * every screen carry the arithmetic of "which of these three wins".
 */

import { useMemo } from "react";
import { useParam } from "~/app/router.tsx";
import { fmtMinute, parseUTC } from "~/lib/format.ts";

/** The windows every time-ranged screen offers, in the order a control reads. */
export const RANGES = ["15m", "1h", "6h", "1d", "7d", "30d", "90d"] as const;
export type Range = (typeof RANGES)[number];

/** How long each one is. */
export const RANGE_MS: Record<Range, number> = {
  "15m": 15 * 60_000,
  "1h": 60 * 60_000,
  "6h": 6 * 60 * 60_000,
  "1d": 24 * 60 * 60_000,
  "7d": 7 * 24 * 60 * 60_000,
  "30d": 30 * 24 * 60 * 60_000,
  "90d": 90 * 24 * 60 * 60_000,
};

/** What each one is called, spelled out. */
export const RANGE_LABEL: Record<Range, string> = {
  "15m": "15 minutes",
  "1h": "1 hour",
  "6h": "6 hours",
  "1d": "24 hours",
  "7d": "7 days",
  "30d": "30 days",
  "90d": "90 days",
};

/**
 * Two instants a reader named, in epoch milliseconds.
 *
 * A VALUE RATHER THAN A PAIR OF PARAMETERS: it is one thing a URL carries, one
 * thing a control edits and one thing a screen branches on.
 */
export interface Interval {
  from: number;
  to: number;
}

/** What `window=` means: one of the closed set, or an interval a reader named. */
export type Window = Range | Interval;

/** Whether a window is one of the named durations. */
export function isRange(w: Window): w is Range {
  return typeof w === "string";
}

/** How long a window is. */
export function spanOf(w: Window): number {
  return isRange(w) ? RANGE_MS[w] : w.to - w.from;
}

/** What a screen will accept from the URL, and offer in its control. */
export interface Offer {
  /** Which of the named durations this screen has. */
  ranges: readonly Range[];
  /** Whether a reader may name two instants of their own. */
  custom: boolean;
  /** The window when the URL names none, or names one this screen refuses. */
  fallback: Range;
  /** Which bucket widths this screen's own question accepts. See [bucketFor]. */
  buckets?: readonly Bucket[];
}

/**
 * The window a URL asked for, as something this screen can show.
 *
 * Anything outside the offer is the fallback — never a value derived from it.
 * `window=3h` is not a window any screen has, and answering it with three
 * hours would put a heading the control cannot show over the rows.
 *
 * An INVERTED OR EMPTY interval is refused for the reason the empty `window=`
 * was: the engine's windows are half-open, so `to <= from` names no rows at
 * all and the screen would render the engine's refusal rather than its data.
 */
export function parseWindow(raw: string, offer: Offer): Window {
  const value = raw.trim();
  if ((offer.ranges as readonly string[]).includes(value)) return value as Range;
  if (offer.custom) {
    const cut = value.indexOf("/");
    if (cut > 0) {
      const from = parseUTC(value.slice(0, cut));
      const to = parseUTC(value.slice(cut + 1));
      if (from && to && to.getTime() > from.getTime()) {
        return { from: from.getTime(), to: to.getTime() };
      }
    }
  }
  return offer.fallback;
}

/** A window back as the URL spells it. */
export function windowParam(w: Window): string {
  if (isRange(w)) return w;
  return `${new Date(w.from).toISOString()}/${new Date(w.to).toISOString()}`;
}

/** What to call a window in a heading. */
export function windowLabel(w: Window): string {
  if (isRange(w)) return RANGE_LABEL[w];
  // TO THE MINUTE, because that is the resolution the picker has: the seconds
  // are always `:00` and are fourteen characters of noise in a label that has
  // to sit in a page bar beside everything else.
  return `${fmtMinute(new Date(w.from).toISOString())} — ${fmtMinute(new Date(w.to).toISOString())}`;
}

/**
 * What to call a window given only its two instants.
 *
 * FOR AN ANSWER'S OWN LABEL rather than for a control: a rollup reports the
 * window it covered, which is not necessarily the one that was asked for — the
 * store floors a request at its retention — so the heading over the figures is
 * built from what came back, never from what went out.
 *
 * Named in the vocabulary's own words when the span is one of them, so "24
 * hours" reads the same here as on the control that produced it, and in whole
 * units otherwise. Rounded DOWN with a "+", because a window of 30 days and
 * one second is thirty days to every reader and "30.00001 days" is nobody's
 * heading — while calling 29½ days "30 days" would overstate what was counted.
 */
export function spanWords(since: string, until: string): string {
  const from = parseUTC(since);
  const to = parseUTC(until);
  if (!from || !to) return "an unknown window";
  const ms = to.getTime() - from.getTime();
  if (ms <= 0) return "no window at all";
  for (const r of RANGES) if (RANGE_MS[r] === ms) return RANGE_LABEL[r];
  const units: [number, string][] = [
    [86_400_000, "day"],
    [3_600_000, "hour"],
    [60_000, "minute"],
  ];
  // THE COARSEST UNIT THE SPAN DIVIDES INTO EXACTLY, so a window an engine
  // widened to whole buckets reads as what it is. A histogram over "24 hours"
  // snaps its edges out to the hour and comes back covering twenty-five of
  // them: "1+ days" is true and tells a reader nothing, while "25 hours" says
  // exactly what the bars add up to.
  for (const [size, name] of units) {
    if (ms < size || ms % size) continue;
    const n = ms / size;
    return `${n} ${name}${n === 1 ? "" : "s"}`;
  }
  // Nothing divides it — a window somebody typed to the minute over several
  // days. The coarsest unit with a whole number in it, marked as more.
  for (const [size, name] of units) {
    if (ms < size) continue;
    return `${Math.floor(ms / size)}+ ${name}s`;
  }
  return "under a minute";
}

/** The bucket widths an engine series can be drawn in, FINEST FIRST. */
export const BUCKETS = ["minute", "hour", "day"] as const;
export type Bucket = (typeof BUCKETS)[number];

/** How many milliseconds a bucket covers. */
export const BUCKET_MS: Record<Bucket, number> = {
  minute: 60_000,
  hour: 60 * 60_000,
  day: 24 * 60 * 60_000,
};

/**
 * An hour of minutes, a day of hours, or a longer range of days.
 *
 * TIED TO THE WINDOW rather than offered as a second control: 30 days of hourly
 * bars is 720 columns on a chart eight hundred pixels wide, and one day of
 * daily bars is a single column. The reader picks a range; the bucket follows.
 *
 * AND THE SET IS THE QUESTION'S, the same rule an [Offer]'s ranges follow. A
 * bucket is a closed set at the engine too — an axis with an arbitrary bucket
 * width is one nobody can label — and the two questions drawn this way accept
 * different sets: the spend series has `hour` and `day`, because a minute
 * bucket over a week is ten thousand points nobody can read, and the event log
 * has `minute` as well, because "what just happened" is the commonest question
 * asked of a log and an hour is the whole of that answer's window. A screen
 * declares what its question takes and this COARSENS to it, never past it.
 */
export function bucketFor(w: Window, offer: readonly Bucket[] = BUCKETS): Bucket {
  const span = spanOf(w);
  const want: Bucket = span <= RANGE_MS["1h"] ? "minute" : span <= RANGE_MS["1d"] ? "hour" : "day";
  const from = BUCKETS.indexOf(want);
  // The first offered bucket at or after the one the window wants, and the
  // coarsest offered otherwise — never finer than the question accepts,
  // because the engine refuses a value outside its own set rather than
  // guessing which branch its switch should end on.
  return BUCKETS.slice(from).find((b) => offer.includes(b)) ?? coarsest(offer);
}

/** The widest bucket a caller offers, for a window wider than any of them. */
function coarsest(offer: readonly Bucket[]): Bucket {
  return [...BUCKETS].reverse().find((b) => offer.includes(b)) ?? "day";
}

/** How many milliseconds a bucket covers. */
export function stepOf(bucket: Bucket): number {
  return BUCKET_MS[bucket];
}

/** How a window is cut into columns: how wide each is, and how many there are. */
export interface Cut {
  /** One column's width in milliseconds — always a whole number of minutes. */
  cell: number;
  /** How many columns cover the window. */
  cells: number;
}

/**
 * Cut a window into at most `cap` columns of whole minutes.
 *
 * FOR A STRIP THE CLIENT DRAWS ITSELF, not for the engine's series — which has
 * two bucket widths and picks between them (see [bucketFor]). A live strip is
 * folded from the events this tab is holding, so its column is whatever makes
 * the strip readable rather than whatever the store indexes on.
 *
 * BOTH HALVES MOVE, which is the part that is easy to get wrong and was: a
 * fixed column width makes six hours 360 sub-pixel slivers, and a fixed column
 * COUNT makes fifteen minutes into sixty one-minute columns — an hour of strip
 * under a heading that says fifteen minutes. So the column is the window over
 * the cap, floored at a minute, and the COUNT then follows from the window, so
 * the strip covers what its heading claims and no more.
 *
 * Whole minutes because a column boundary is a wall-clock instant a reader can
 * point at — `13:42`, not "seventeen seconds after the tab loaded".
 */
export function cutInto(span: number, cap: number): Cut {
  const columns = Math.max(1, cap);
  const cell = Math.max(1, Math.round(span / columns / 60_000)) * 60_000;
  return { cell, cells: Math.max(1, Math.min(columns, Math.round(span / cell))) };
}

/** A window, as the engine's two parameters. */
export interface TimeRange {
  /** What was chosen, for a control and a heading. */
  window: Window;
  /** The inclusive start, RFC3339. */
  since: string;
  /** The exclusive end, RFC3339. */
  until: string;
  /** The window immediately before this one, for a compare-to-previous. */
  previous: { since: string; until: string };
  /** The bucket a chart over this window draws in. */
  bucket: Bucket;
  /** What this screen offers, so one declaration reaches the control too. */
  offer: Offer;
  /** Set the window, which is a SECTION rather than a filter: a reader who
   *  widened the range and pressed back means the narrower one. */
  set: (next: Window) => void;
}

/**
 * The two windows a range names, as instants.
 *
 * SEPARATED FROM THE HOOK because this is the arithmetic and the hook is the
 * URL binding: a claim about where a comparison window sits should not need a
 * router and a render to exercise.
 *
 * ALIGNED TO `step` WHEN ONE IS GIVEN, which a chart needs and a list does
 * not. The top edge becomes the END of the bucket in progress, so the current
 * hour is on the chart while it is still being spent rather than appearing
 * once it ends — and the window's identity, and therefore the query, changes
 * once per column rather than once per second.
 *
 * AN INTERVAL IS NOT ALIGNED AND DOES NOT MOVE. A reader who named two
 * instants asked for those instants; rounding them to a bucket would answer a
 * question they did not ask, and `now` does not enter it at all — which is
 * also what makes a custom window a stable thing to link to.
 *
 * THE PREVIOUS WINDOW IS THE SAME WIDTH ENDING WHERE THIS ONE STARTS, never
 * "the same dates a month ago". A comparison whose two windows are different
 * lengths compares two numbers that do not answer the same question, which is
 * how a 28-day February outperforms every other month in every dashboard that
 * gets it wrong.
 */
export function windowEdges(
  w: Window,
  now: number,
  step = 0,
): Pick<TimeRange, "since" | "until" | "previous"> {
  const span = spanOf(w);
  const edge = isRange(w) ? (step > 0 ? Math.ceil(now / step) * step : now) : w.to;
  const since = new Date(edge - span);
  const before = new Date(edge - span * 2);
  return {
    since: since.toISOString(),
    until: new Date(edge).toISOString(),
    previous: { since: before.toISOString(), until: since.toISOString() },
  };
}

/**
 * `window=` as two instants, and the setter that moves it.
 *
 * `align` is what separates a chart from a list: a chart asks for edges on the
 * bucket it draws so the query changes once per column, and a list asks for
 * the clock so its newest row is the newest row.
 */
export function useTimeRange(now: number, offer: Offer, align = true): TimeRange {
  const [raw, setRaw] = useParam("window", offer.fallback, "section");
  const { ranges, custom, fallback, buckets } = offer;
  // Rebuilt from the fields rather than held by identity: every caller writes
  // its offer inline, so a new object arrives on every render and an offer in
  // the dependency list would rebuild the window on every one of them.
  const settled = useMemo(
    () => ({ ranges, custom, fallback, buckets }),
    [ranges, custom, fallback, buckets],
  );
  const window = parseWindow(raw, settled);
  const bucket = bucketFor(window, buckets);
  const step = align ? stepOf(bucket) : 0;
  // THE WINDOW AS A STRING is what the memo holds, because `parseWindow`
  // returns a fresh object for an interval and a dependency compared by
  // identity would rebuild on every render.
  const key = windowParam(window);
  // AND A RANGE'S ANCHOR IS THE CLOCK WHILE AN INTERVAL'S IS NOTHING. A
  // reader who named two instants asked for those instants, so the tick that
  // advances every other clock on the screen must not give this one a new
  // identity — which is what makes a custom window stable to link to and to
  // hold a query open on.
  const anchor = isRange(window) ? now : 0;
  return useMemo(() => {
    const w = parseWindow(key, settled);
    return {
      window: w,
      ...windowEdges(w, anchor, step),
      bucket,
      offer: settled,
      set: (next: Window) => setRaw(windowParam(next)),
    };
  }, [key, settled, anchor, step, bucket, setRaw]);
}
