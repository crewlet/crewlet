/**
 * A log's own time axis: where in this window did things happen.
 *
 * WHAT A PAGE OF ROWS CANNOT SAY. A listing answers what happened; it has no
 * dimension for when the company was busy, so a burst at four in the morning
 * and a steady trickle across a week read identically once they are a hundred
 * rows in a column. This is the shape every log tool has for exactly that
 * reason, and it is the one the event log did not have.
 *
 * THE BARS ARE THE CALLER'S, never folded here, and WHERE THEY CAME FROM IS A
 * REQUIRED PROP. The event log asks the ENGINE for its series over the whole
 * window, which is the better answer wherever it exists — the same rule
 * `internal/tokens` states for the spend series one screen over — while the
 * tracker's change log has no such read and buckets the pages it is holding.
 * Those are two different claims, so `over` says which, once, in the one
 * sentence on this component a sighted reader never sees. It is required for
 * the reason `FacetRail`'s is: a number whose scope is optional is a number
 * that will be wrong on the second caller, and it was — this component
 * announced "3 events in this window" over a chart whose visible caption said
 * three changes on one page, which is the wrong scope AND the wrong noun on a
 * screen where nothing is an event.
 *
 * A BAR IS A CONTROL, not a picture. Clicking one narrows the window to it,
 * which is the gesture a reader already expects from every log they have used —
 * and the reason the axis is worth having at all, since "what happened in that
 * spike" is the question the spike creates.
 *
 * AND AN AXIS WITHOUT DATES IS NOT AN AXIS. A seven-day window holding one busy
 * day drew a single bar hard against the right edge of an empty plot with
 * nothing under it, which reads as a chart that failed to load rather than as a
 * company that was quiet until yesterday. `axis` puts the dates back — see
 * [ticksFor] for what is labelled and why.
 */

import type { CSSProperties } from "react";

import { fmtDate, fmtDateCompact, fmtMinute, plural, toWall } from "~/lib/format.ts";
import type { FacetScope } from "~/ui/FacetRail.tsx";
import type { Interval } from "~/lib/range.ts";

/** One bar, as the engine reports it. */
export interface Bar {
  /** The bucket's START, RFC3339. */
  at: string;
  count: number;
}

/**
 * The floor is in PIXELS, not in percent, and the height is the TRUE ratio.
 *
 * A percentage floor scales with the panel and swallows real differences: over
 * a day whose busiest hour holds six thousand events, a floor of 8% drew a
 * bucket of 4 and a bucket of 400 at exactly the same height — an order of
 * magnitude erased to make a sliver visible. Two pixels is what "visible"
 * actually means, it does not grow with the chart, and every count above it
 * keeps its real proportion.
 *
 * Empty gets one pixel less than the smallest real count, because the two are
 * different facts: a zero-height element is invisible, so an empty stretch
 * would read as a gap in the CHART rather than as a quiet stretch in the
 * COMPANY, and a reader who has not counted the bars cannot tell those apart.
 */
const EMPTY_PX = 2;
const MIN_PX = 3;

/** What the total is a total OF, as the sentence that says it. */
const SCOPE_SENTENCE: Record<FacetScope, string> = {
  window: "in this window",
  loaded: "on the pages loaded",
};

export function Histogram({
  bars,
  bucket,
  total,
  noun,
  over,
  onPick,
  axis = false,
  now = Date.now(),
  height = 96,
  label = "Events over the window",
}: {
  bars: Bar[];
  /** How wide one bar is, for the tooltip and for what a click selects. */
  bucket: "minute" | "hour" | "day";
  /** How many of them there are, over whatever `over` names. */
  total: number;
  /** What one of them IS — "event", "change" — in the singular. */
  noun: string;
  /** Whether the bars and the total cover the whole window or only the pages
   *  this client is holding. Required, never inferred: see the module doc. */
  over: FacetScope;
  /** Narrow the window to one bar. Omitted leaves the bars as a picture. */
  onPick?: (next: Interval) => void;
  /** Draw the labelled time axis under the bars. Optional because a chart can
   *  be narrower than its own labels — an inline strip beside a figure has no
   *  room for a date — and a tick row that collides with itself is worse than
   *  none. Every full-width log in this product passes it. */
  axis?: boolean;
  /** The clock the axis's own date rule reads — [fmtDateCompact] drops the year
   *  in the reader's CURRENT year, and a component that read the clock itself
   *  would decide that separately from every other date on the screen. Every
   *  caller drawing an axis holds a `useNow()` and passes it. */
  now?: number;
  height?: number;
  label?: string;
}) {
  const peak = Math.max(1, ...bars.map((b) => b.count));
  const step = STEP_MS[bucket];
  return (
    <div className="histogram-figure">
      <div className="histogram" style={{ height }} role="group" aria-label={label}>
        {bars.map((b) => {
          const at = Date.parse(b.at);
          const title = `${when(b.at, bucket)} — ${plural(b.count, noun)}`;
          const size = {
            height: `${(b.count / peak) * 100}%`,
            minHeight: b.count === 0 ? EMPTY_PX : MIN_PX,
          };
          // A BUTTON WHEN IT DOES SOMETHING AND A DIV WHEN IT DOES NOT, rather
          // than a button that ignores the click: a keyboard reader tabbing
          // through eighty inert controls is worse served than one that is
          // told there is nothing to operate.
          return onPick && Number.isFinite(at) ? (
            <button
              key={b.at}
              type="button"
              className="histogram-bar"
              title={title}
              aria-label={title}
              data-empty={b.count === 0 ? "" : undefined}
              style={size}
              onClick={() => onPick({ from: at, to: at + step })}
            />
          ) : (
            <div
              key={b.at}
              className="histogram-bar"
              title={title}
              data-empty={b.count === 0 ? "" : undefined}
              style={size}
            />
          );
        })}
        {/* THE TOTAL IS THE CALLER'S, stated rather than summed by the reader
            or by this component: summed here it would agree today and stop
            agreeing the first time a cap is added to either half — and it
            carries the caller's own noun and scope, because those are facts
            about the question rather than about the drawing. */}
        <span className="sr-only">
          {plural(total, noun)} {SCOPE_SENTENCE[over]}
        </span>
      </div>
      {axis && bars.length > 0 && (
        // ARIA-HIDDEN: every bar already carries its own bucket in `title`,
        // and a screen reader walking five loose dates under a chart it has
        // just been given a total for is told nothing it can place.
        <div className="histogram-axis" aria-hidden="true">
          {ticksFor(bars.length).map((index) => (
            <span
              key={bars[index]!.at}
              className="histogram-tick"
              style={edgeOf(index, bars.length)}
            >
              {tick(bars[index]!.at, bucket, now)}
            </span>
          ))}
        </div>
      )}
    </div>
  );
}

/** How long one bar covers. */
const STEP_MS: Record<"minute" | "hour" | "day", number> = {
  minute: 60_000,
  hour: 60 * 60_000,
  day: 24 * 60 * 60_000,
};

/**
 * How many labels an axis carries at most.
 *
 * FIVE, which is what the narrowest layout this product supports can hold: a
 * chart there is about 320px wide and a date at the axis's own size measures
 * about 45px — the width `lib/timeline.ts` measured for the roadmap's own
 * ticks — so five labels sit 80px apart and a sixth starts crowding the fifth.
 * A cap rather than a pixel calculation because a bar's width is the flex
 * layout's answer and is not known here; the one thing that IS known is how
 * many labels fit, and dropping a tick changes what the axis SAYS rather than
 * how it looks.
 */
const MAX_TICKS = 5;

/**
 * Which bars get a label: the first, the last, and an even spread between.
 *
 * EVENLY OVER THE BARS rather than on the calendar. `lib/timeline.ts` rules its
 * axis on weeks because a roadmap's columns are days and a reader navigates it
 * by date; a histogram's columns are whatever bucket the window chose — minutes,
 * hours or days — and a calendar rule over minute buckets would put one tick on
 * an axis of sixty. So the spread is positional, and every tick names the bucket
 * it actually sits over.
 *
 * BOTH ENDS ALWAYS, because they are the two the window's own control already
 * names: an axis whose first and last labels disagree with the picker above it
 * is the one failure a reader cannot diagnose.
 */
export function ticksFor(bars: number): number[] {
  if (bars <= 0) return [];
  const count = Math.min(MAX_TICKS, bars);
  if (count === 1) return [0];
  return Array.from({ length: count }, (_, i) => Math.round((i * (bars - 1)) / (count - 1)));
}

/**
 * Where one tick sits, as a style.
 *
 * ON THE BUCKET'S OWN CENTRE, which is `(i + 0.5) / n` of the width — a label
 * placed at `i / n` names the boundary between two buckets rather than either
 * of them. The two ENDS are pinned to the edges instead: centred, the first
 * label would hang half its width off the left of the chart and the last off
 * the right, which is the one place a tick row overflows its card.
 */
function edgeOf(index: number, bars: number): CSSProperties {
  if (index === 0) return { left: 0 };
  if (index === bars - 1) return { right: 0 };
  return { left: `${((index + 0.5) / bars) * 100}%`, transform: "translateX(-50%)" };
}

/**
 * What one tick is called.
 *
 * A DAY BUCKET IS A DATE and anything shorter is a wall clock. The date drops
 * the year in the reader's own year ([fmtDateCompact], which is what every
 * column of dates in this product uses), and the clock carries no date at all:
 * five repetitions of today's date under a chart of minutes is the noise the
 * compact form exists to remove, and the window the axis covers is named by the
 * range control directly above it.
 *
 * [toWall] is the one formatter that renders an instant in the VIEWER'S chosen
 * zone with no date beside it — it is the `datetime-local` spelling,
 * `YYYY-MM-DDTHH:mm`, so the eleven characters after the `T` are that zone's
 * own wall clock. Reading the browser's clock instead would put a tick an hour
 * out for every reader who set the preference.
 */
function tick(at: string, bucket: "minute" | "hour" | "day", now: number): string {
  if (bucket === "day") return fmtDateCompact(at, now);
  const ms = Date.parse(at);
  return Number.isFinite(ms) ? toWall(ms).slice(11) : fmtMinute(at);
}

/**
 * What to call a bucket in its tooltip.
 *
 * A DAY BUCKET NAMES A DAY and a minute bucket names a minute. "Jun 15, 2026,
 * 00:00:00" over a bar that covers the whole of the fifteenth invites a reader
 * to read it as midnight, which is the one instant in the bucket that is not
 * representative of it.
 */
function when(at: string, bucket: "minute" | "hour" | "day"): string {
  return bucket === "day" ? fmtDate(at) : fmtMinute(at);
}
