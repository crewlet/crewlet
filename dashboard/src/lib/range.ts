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
import { dateFormatter, fmtMinute, parseUTC } from "~/lib/format.ts";
import { useClockReading } from "~/lib/clock.ts";

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
  /**
   * The COMPANY'S DAY SO FAR, from its first instant to now — named rather
   * than two instants, so the URL says `window=today` and the window moves
   * with the clock the way a named duration does. Set only by [todayWindow].
   *
   * Its `to` is the instant it was cut at and NOT an edge: the day so far
   * ends whenever it is asked, which is the instant [windowEdges] is handed.
   */
  today?: true;
}

/** `window=` for the company's day so far. See [Interval.today]. */
export const TODAY = "today";

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
  /**
   * Whether `today` is one of this screen's windows — the company's day so
   * far, cut at midnight on [zone].
   */
  today?: boolean;
  /**
   * Whether a custom window is named in WHOLE COMPANY DAYS on [zone] rather
   * than to the minute: the picker takes two dates, both inclusive, and the
   * window runs from the first instant of the first to the first instant
   * after the last. For a screen whose question is company days — spend is
   * read from the usage domain, whose smallest unit is one — a picker that
   * took minutes would offer a precision the answer cannot have, and a reader
   * who chose 14:00 would be shown the whole day with nothing saying why.
   */
  customDays?: boolean;
  /**
   * The company's zone (`org.timezone`), which is where its day begins. A
   * screen that offers `today` passes it; before the org arrives the day is
   * cut in UTC, which is a label on one screen for one render rather than a
   * reason to draw nothing.
   */
  zone?: string;
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
export function parseWindow(raw: string, offer: Offer, now: number = Date.now()): Window {
  const value = raw.trim();
  if ((offer.ranges as readonly string[]).includes(value)) return value as Range;
  if (offer.today && value === TODAY) return todayWindow(now, offer.zone);
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
  if (w.today) return TODAY;
  return `${new Date(w.from).toISOString()}/${new Date(w.to).toISOString()}`;
}

/** What to call a window in a heading. */
export function windowLabel(w: Window): string {
  if (isRange(w)) return RANGE_LABEL[w];
  if (w.today) return "Today";
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
  // TODAY IS A DAY HOWEVER MUCH OF IT HAS PASSED: its chart draws hours from
  // the first one, rather than minutes until one o'clock and hours after —
  // a bucket that changed under the reader mid-morning.
  const want: Bucket =
    !isRange(w) && w.today
      ? "hour"
      : span <= RANGE_MS["1h"]
        ? "minute"
        : span <= RANGE_MS["1d"]
          ? "hour"
          : "day";
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

/**
 * A window as a screen holds it: what was chosen, and how to choose another.
 *
 * NO INSTANTS, and that is the point of it being a type of its own. The two
 * edges of a named range are a function of WHEN they are read, and a screen
 * that read them at render read them once a second: the one-second clock
 * (`lib/clock.ts`) re-rendered it, a fresh pair of millisecond instants came
 * out, and every question keyed on them was a new question. The audit asked
 * the tracker for its feed once a second where it meant once a minute, and
 * drew every row it held again each time. What a screen holds is the CHOICE;
 * the edges are computed where they are spent — a chart's on its bucket
 * ([useTimeRange]), a list's at the instant it asks (`useQuery`'s `window`).
 */
export interface WindowChoice {
  /** What was chosen, for a control and a heading. */
  window: Window;
  /** The bucket a chart over this window draws in. */
  bucket: Bucket;
  /** What this screen offers, so one declaration reaches the control too. */
  offer: Offer;
  /** Set the window, which is a SECTION rather than a filter: a reader who
   *  widened the range and pressed back means the narrower one. */
  set: (next: Window) => void;
}

/** A window as the engine's two parameters — a chart's, on its bucket. */
export interface TimeRange extends WindowChoice {
  /** The inclusive start, RFC3339. */
  since: string;
  /** The exclusive end, RFC3339. */
  until: string;
  /** The window immediately before this one, for a compare-to-previous. */
  previous: { since: string; until: string };
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
 * once per column rather than once per second. With no step the top edge is
 * `now` itself, which is right only where `now` is the instant of ASKING:
 * read at render, it is a different window every second.
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
 *
 * `days` IS A SCREEN OF WHOLE COMPANY DAYS, which aligns to neither a bucket
 * nor the clock but to the company's midnights — see [dayEdges].
 */
export function windowEdges(
  w: Window,
  now: number,
  step = 0,
  days?: { zone: string | undefined },
): Pick<TimeRange, "since" | "until" | "previous"> {
  if (days) return dayEdges(w, now, days.zone);
  // A NAMED WINDOW MOVES WITH THE CLOCK — a duration, and today — and an
  // interval a reader typed does not.
  const moving = isRange(w) || w.today === true;
  const edge = moving ? (step > 0 ? Math.ceil(now / step) * step : now) : w.to;
  // TODAY STARTS WHERE THE COMPANY'S DAY DID, whatever the clock says now, so
  // its span is measured from that edge rather than carried in the value.
  const span = !isRange(w) && w.today ? edge - w.from : spanOf(w);
  const since = new Date(edge - span);
  const before = new Date(edge - span * 2);
  return {
    since: since.toISOString(),
    until: new Date(edge).toISOString(),
    previous: { since: before.toISOString(), until: since.toISOString() },
  };
}

/**
/**
 * The edges of a window on a screen of WHOLE COMPANY DAYS (`Offer.customDays`).
 *
 * EVERY EDGE IS A COMPANY MIDNIGHT, because that is what the question is: the
 * engine answers `days=30` as the thirty company days ending TODAY on the
 * company's clock, so the window runs from the first instant of the first of
 * them to the first instant after today. Aligning a named range to a UTC day
 * bucket instead named a different window — in Berlin, for twenty-two hours of
 * every twenty-four, an edge at 02:00 TOMORROW, so thirty-one company days
 * ending tomorrow — and the custom dialog, which is prefilled from these
 * edges, then offered to ask for that.
 *
 * The comparison window is the SAME NUMBER OF DAYS ending where this one
 * starts — counted in days rather than milliseconds, since a window across a
 * clock change is a day of 23 or 25 hours and is still one day.
 */
function dayEdges(
  w: Window,
  now: number,
  zone: string | undefined,
): Pick<TimeRange, "since" | "until" | "previous"> {
  // The first and last company day, both inclusive.
  let first: string;
  let last: string;
  if (isRange(w)) {
    last = dayLabelIn(now, zone);
    first = shiftDay(last, 1 - Math.max(1, Math.round(RANGE_MS[w] / 86_400_000)));
  } else {
    const covered = companyDays(w.today ? { from: w.from, to: now } : w, zone);
    first = covered.since;
    last = covered.until;
  }
  const count = Math.round((Date.parse(last) - Date.parse(first)) / 86_400_000) + 1;
  const at = (label: string) =>
    new Date(dayStartIn(label, zone) ?? Date.parse(label)).toISOString();
  return {
    since: at(first),
    until: at(shiftDay(last, 1)),
    previous: { since: at(shiftDay(first, -count)), until: at(first) },
  };
}

/** A company date `n` days after `label` (before it for a negative `n`). */
function shiftDay(label: string, n: number): string {
  // CALENDAR ARITHMETIC ON THE LABEL, in UTC where every day is 24 hours: a
  // date is not an instant, and stepping an instant by 86,400,000 across a
  // clock change lands on the wrong side of a midnight.
  return new Date(Date.parse(`${label}T00:00:00Z`) + n * 86_400_000).toISOString().slice(0, 10);
}

/**
 * `window=`, as this screen can show it, and the setter that moves it.
 *
 * THE CLOCK DOES NOT ENTER IT, so its identity changes when the reader picks
 * another window and at no other time. A LIST holds this and nothing more: it
 * computes its edges when it asks (`useQuery`'s `window` option), so its
 * newest row is the newest row as of the ask, and a second passing between
 * two asks changes no question and redraws nothing.
 *
 * ONE EXCEPTION, and it is a day long: `today` begins at the company's
 * midnight, which is an instant, so the window is re-cut when the company's
 * day turns — read off the clock as that midnight (`useClockReading`), which
 * moves once a day — and at no other tick. Cut once and held, a list left
 * open across midnight would go on asking for yesterday as "today".
 */
export function useWindow(offer: Offer): WindowChoice {
  const [raw, setRaw] = useParam("window", offer.fallback, "section");
  const { ranges, custom, fallback, buckets, today, zone, customDays } = offer;
  // Rebuilt from the fields rather than held by identity: every caller writes
  // its offer inline, so a new object arrives on every render and an offer in
  // the dependency list would rebuild the window on every one of them.
  const settled = useMemo(
    () => ({ ranges, custom, fallback, buckets, today, zone, customDays }),
    [ranges, custom, fallback, buckets, today, zone, customDays],
  );
  const cutsToday = settled.today === true && raw.trim() === TODAY;
  const midnight = useClockReading((now) => (cutsToday ? companyMidnight(now, settled.zone) : 0));
  // THE WINDOW AS A STRING is what the memo holds, because `parseWindow`
  // returns a fresh object for an interval and a dependency compared by
  // identity would rebuild on every render.
  const key = windowParam(parseWindow(raw, settled, midnight));
  return useMemo(() => {
    const window = parseWindow(key, settled, midnight);
    return {
      window,
      bucket: bucketFor(window, settled.buckets),
      offer: settled,
      set: (next: Window) => setRaw(windowParam(next)),
    };
  }, [key, settled, midnight, setRaw]);
}

/**
 * `window=` as a CHART draws it: two instants on the bucket it draws in.
 *
 * ALWAYS ALIGNED. The top edge is the end of the bucket in progress, so the
 * edges change once per column, and a question keyed on them is asked again
 * when a column rolls rather than when a second passes. A list that asks the
 * engine has no column to roll; it takes [useWindow] and computes its edges
 * when it asks. On a screen of WHOLE COMPANY DAYS (`Offer.customDays`) the
 * column is a company day, so the edges move at the company's midnight.
 *
 * AND IT READS THE CLOCK AS THAT EDGE, not as the second (`useClockReading`):
 * a screen that held `now` to hand in here rendered once a second — its grid,
 * its axis and every row under them — to arrive at edges that move once an
 * hour, and it renders now when the column rolls.
 *
 * AN INTERVAL'S ANCHOR IS NOTHING: a reader who named two instants asked for
 * those instants, so the tick that advances every other clock on the screen
 * must not give this one new edges. `today` is not one: it is the company's
 * day SO FAR, so its top edge moves with the clock as a named range's does.
 */
export function useTimeRange(offer: Offer): TimeRange {
  const choice = useWindow(offer);
  const { window, bucket, offer: settled } = choice;
  const step = stepOf(bucket);
  const days = settled.customDays ? { zone: settled.zone } : undefined;
  const moving = isRange(window) || window.today === true;
  const edge = useClockReading((now) =>
    !moving ? 0 : days ? companyMidnight(now, days.zone) : Math.ceil(now / step) * step,
  );
  const { since, until, previous } = windowEdges(window, edge, step, days);
  // `previous.until` IS `since`, so these three strings are every instant the
  // value carries, and the memo holds on exactly them.
  const before = previous.since;
  return useMemo(
    () => ({ ...choice, since, until, previous: { since: before, until: since } }),
    [choice, since, until, before],
  );
}

/**
 * A histogram's bars over a window, bucketed from instants this client holds.
 *
 * WHO NEEDS THIS AND WHO DOES NOT. The event log and the spend chart ask the
 * ENGINE for their series, over the whole window, and that is the better
 * answer wherever it exists: a client can only bucket what it was sent. The
 * tracker's change log has no such read — `work_activity` answers with rows —
 * so its axis is over the page the screen is holding, and the screen drawing
 * it says so rather than implying the engine counted.
 *
 * EVERY BUCKET IN THE WINDOW, including the empty ones. A series built only
 * from the buckets that have something in them draws a quiet week as a solid
 * run of bars, which is the opposite of what the chart is for; `Histogram`
 * gives a zero its own floor in pixels precisely so an empty bucket is
 * visible as one.
 *
 * ALIGNED TO THE BUCKET, from `since` forward. The caller's window is already
 * aligned by `useTimeRange` for a chart, so the first bar starts where the
 * window does and the last one is the bucket in progress.
 *
 * BOUNDED, because a window and a bucket are two independent values and a
 * hand-edited address can pair a custom interval of a year with an hourly
 * bucket: 8,760 bars is a chart nobody can read and a loop that builds it is
 * one nobody asked for. The cap is [MAX_BARS] and what it drops is the
 * OLDEST, so the newest end — which is the end a log is read from — is always
 * drawn.
 */
export function barsOver(
  at: readonly string[],
  since: string,
  until: string,
  bucket: Bucket,
): { at: string; count: number }[] {
  const start = Date.parse(since);
  const end = Date.parse(until);
  const step = BUCKET_MS[bucket];
  if (!Number.isFinite(start) || !Number.isFinite(end) || end <= start) return [];

  const whole = Math.ceil((end - start) / step);
  const cells = Math.min(whole, MAX_BARS);
  // THE NEWEST END IS THE ONE KEPT: a log is read from its most recent row, so
  // a window too wide for the bucket loses its oldest bars rather than its
  // latest.
  const first = start + (whole - cells) * step;

  const counts = new Array<number>(cells).fill(0);
  for (const instant of at) {
    const ms = Date.parse(instant);
    // THE UPPER EDGE IS INCLUSIVE AND THE LOWER ONE IS NOT, which is not a
    // choice made here: it is the edge the ROWS came back on. The engine
    // compiles a window to `h.created_at >= from AND h.created_at <= to`
    // (`internal/tracker/activityread.go`), so a change made in the instant
    // the window closed IS in the answer — and a chart that excluded it would
    // draw fewer changes than the list under it, with nothing saying why.
    if (!Number.isFinite(ms) || ms < first || ms > end) continue;
    // WHICH IS WHY THE INDEX IS CLAMPED. That last instant divides to exactly
    // `cells`, one past the final bucket, and the clamp is what puts it in the
    // bucket it belongs to rather than off the end of the array.
    const index = Math.min(Math.floor((ms - first) / step), cells - 1);
    counts[index] = (counts[index] ?? 0) + 1;
  }
  return counts.map((count, i) => ({ at: new Date(first + i * step).toISOString(), count }));
}

/**
 * How many bars one chart draws at most.
 *
 * NINETY, which is the widest ordinary window this product offers at its own
 * bucket: `90d` at a daily bucket. A chart wider than that is a column per
 * pixel, where a bar has no hover target and the axis has no labels a reader
 * can place.
 */
export const MAX_BARS = 90;

/**
 * The first instant of the company's day that `now` falls in, on the
 * company's own clock (`org.timezone`, ADR-0018).
 *
 * THE FIRST INSTANT OF THE DATE, found rather than assumed to be local
 * midnight: in a zone that moves its clock across midnight the day does not
 * begin at 00:00 — Santiago springs from 00:00 straight to 01:00, so 00:00
 * does not exist, and Amman falls back from 01:00 to 00:00, so it exists
 * twice and the day began at the first. `time.Date` gets both wrong in the
 * engine's own calendar for the same reason (`internal/period`), and a
 * "Today" that disagreed with the engine's day by an hour would count a
 * turn the engine charged to yesterday.
 *
 * So the date `now` falls on is read in the zone, and the earliest instant
 * that reads as the same date is searched for across the day before it: the
 * date an instant falls on only moves forward as the instant does, which is
 * what a binary search needs. A zone this runtime cannot format in falls back
 * to UTC rather than throwing on a screen that only wanted a label.
 */
export function companyMidnight(now: number, zone: string | undefined): number {
  const day = dateIn(zone);
  const target = day(now);
  // A DAY IS AT MOST 25 HOURS on any clock, so its start is inside the 26
  // hours before `now`.
  let lo = now - 26 * 3_600_000;
  let hi = now;
  while (hi - lo > 1) {
    const mid = Math.floor((lo + hi) / 2);
    if (day(mid) < target) lo = mid;
    else hi = mid;
  }
  return hi;
}

/** Today on the company's clock, from its first instant to `now`. */
export function todayWindow(now: number, zone: string | undefined): Interval {
  return { from: companyMidnight(now, zone), to: now, today: true };
}

/** The company date (`YYYY-MM-DD`) an instant falls on in `zone`. */
export function dayLabelIn(at: number, zone: string | undefined): string {
  return dateIn(zone)(at);
}

/**
 * The first instant of the company date `label` in `zone`, or null for a
 * value that is not a date.
 *
 * FOUND, NOT ASSUMED, for the reason [companyMidnight] gives: noon UTC of the
 * date is within a day of noon wherever the zone is (every offset is under
 * fifteen hours), so it lands on the date itself or on a neighbour, one step
 * away. Stepping onto the date and taking its company midnight is then exact
 * in the zones that move their clocks across midnight too.
 */
export function dayStartIn(label: string, zone: string | undefined): number | null {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(label.trim());
  if (!m) return null;
  const day = dateIn(zone);
  let at = Date.UTC(Number(m[1]), Number(m[2]) - 1, Number(m[3]), 12);
  if (Number.isNaN(at) || new Date(at).toISOString().slice(0, 10) !== label.trim()) return null;
  for (let step = 0; step < 2 && day(at) !== label.trim(); step++) {
    at += day(at) < label.trim() ? 86_400_000 : -86_400_000;
  }
  if (day(at) !== label.trim()) return null;
  return companyMidnight(at, zone);
}

/**
 * The company days an interval covers, both inclusive: the date its first
 * instant falls on and the date its last instant does. The end is EXCLUSIVE,
 * so the last instant is one millisecond before it — an interval ending at a
 * midnight covers the day before and not the day it ends on.
 */
export function companyDays(
  w: Interval,
  zone: string | undefined,
): { since: string; until: string } {
  const day = dateIn(zone);
  return { since: day(w.from), until: day(Math.max(w.from, w.to - 1)) };
}

/** Two company dates as a heading says them: one date, or the first and last. */
export function daysLabel(days: { since: string; until: string }): string {
  return days.since === days.until ? days.since : `${days.since} – ${days.until}`;
}

/**
 * The interval two company dates name, both inclusive — from the first
 * instant of `since` to the first instant after `until` — or null when either
 * is not a date or the second is before the first.
 */
export function daysInterval(
  since: string,
  until: string,
  zone: string | undefined,
): Interval | null {
  const from = dayStartIn(since, zone);
  const last = dayStartIn(until, zone);
  if (from === null || last === null || last < from) return null;
  // A DAY IS AT MOST 25 HOURS, so 26 past the last day's start is inside the
  // day after it, whose midnight is where this window ends.
  return { from, to: companyMidnight(last + 26 * 3_600_000, zone) };
}

/**
 * A function reading the `YYYY-MM-DD` an instant falls on in `zone`.
 *
 * THROUGH `lib/format.ts`'s kept formatters, like every other date in the
 * product: [companyMidnight] reads a date some twenty-seven times per call and
 * a chart over company days asks it once a tick, so a formatter built per
 * call was the construction cost `dateFormatter` exists to pay once.
 */
function dateIn(zone: string | undefined): (at: number) => string {
  let format: Intl.DateTimeFormat;
  try {
    format = dateFormatter("en-CA", dateOptions(zone || "UTC"));
  } catch {
    // A ZONE THIS RUNTIME CANNOT FORMAT IN is never kept by `dateFormatter`,
    // so it throws here every time and is read in UTC every time.
    format = dateFormatter("en-CA", dateOptions("UTC"));
  }
  // `en-CA` writes a date as `YYYY-MM-DD`, which sorts as the date does.
  return (at) => format.format(at);
}

/** The options [dateIn] formats with, in one order so they key one formatter. */
function dateOptions(timeZone: string): Intl.DateTimeFormatOptions {
  return { timeZone, year: "numeric", month: "2-digit", day: "2-digit" };
}
