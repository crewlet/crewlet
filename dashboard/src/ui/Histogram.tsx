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
 * window, which is the better answer wherever it exists (the same rule
 * `internal/tokens` states for the spend series one screen over), while the
 * tracker's change log has no such read and buckets the pages it is holding.
 * Those are two different claims, so `over` says which, once, in the one
 * sentence on this component a sighted reader never sees. It is required for
 * the reason `FacetRail`'s is: a number whose scope is optional is a number
 * that will be wrong on the second caller, and it was: this component
 * announced "3 events in this window" over a chart whose visible caption said
 * three changes on one page, which is the wrong scope AND the wrong noun on a
 * screen where nothing is an event.
 *
 * A BAR IS A CONTROL, not a picture. Clicking one narrows the window to it,
 * which is the gesture a reader already expects from every log they have used,
 * and the reason the axis is worth having at all, since "what happened in that
 * spike" is the question the spike creates.
 *
 * AND AN AXIS WITHOUT DATES IS NOT AN AXIS. A seven-day window holding one busy
 * day drew a single bar hard against the right edge of an empty plot with
 * nothing under it, which reads as a chart that failed to load rather than as a
 * company that was quiet until yesterday. `axis` puts the dates back; the
 * kit's `histogramTicks` decides which buckets are labelled and why.
 *
 * THE DRAWING IS THE KIT'S `Histogram` (the slot and its fill, the floors, the
 * failed foot, the tick placement). What lives here is what only this product
 * can answer: the engine's bar shape, the bucket a click narrows to, the
 * caller's noun and scope, and every date in the reader's chosen zone.
 */

import { Histogram as KitHistogram, type HistogramBar } from "@crewlethq/ui";

import { fmtDate, fmtDateCompact, fmtMinute, plural, toWall } from "~/lib/format.ts";
import type { FacetScope } from "~/ui/FacetRail.tsx";
import type { Interval } from "~/lib/range.ts";

/** One bar, as the engine reports it. */
export interface Bar {
  /** The bucket's START, RFC3339. */
  at: string;
  count: number;
  /**
   * How many of `count` failed, where the engine split them (`event_series`
   * sends it on every bar). Drawn as the danger-toned foot of the fill and
   * said in the bar's title; absent draws the plain fill, because a split the
   * answer did not carry is not a split of zero.
   */
  failed?: number;
}

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
  /** What one of them IS ("event", "change"), in the singular. */
  noun: string;
  /** Whether the bars and the total cover the whole window or only the pages
   *  this client is holding. Required, never inferred: see the module doc. */
  over: FacetScope;
  /** Narrow the window to one bar. Omitted leaves the bars as a picture. */
  onPick?: (next: Interval) => void;
  /** Draw the labelled time axis under the bars. Optional because a chart can
   *  be narrower than its own labels (an inline strip beside a figure has no
   *  room for a date), and a tick row that collides with itself is worse than
   *  none. Every full-width log in this product passes it. */
  axis?: boolean;
  /** The clock the axis's own date rule reads: [fmtDateCompact] drops the year
   *  in the reader's CURRENT year, and a component that read the clock itself
   *  would decide that separately from every other date on the screen. Every
   *  caller drawing an axis holds a `useNow()` and passes it. */
  now?: number;
  height?: number;
  label?: string;
}) {
  const step = STEP_MS[bucket];
  // THE ENGINE'S ADDRESS FOR A BUCKET IS ITS START, and the kit's is the same
  // instant in milliseconds; the index carries the original string back, so
  // every label reads the engine's own spelling of the bucket.
  const drawn: HistogramBar[] = bars.map((b) => ({
    t: Date.parse(b.at),
    v: b.count,
    part: b.failed,
  }));
  const title = (bar: HistogramBar, index: number) => {
    const failed = Math.min(bar.v, bar.part ?? 0);
    return (
      `${when(bars[index]!.at, bucket)} · ${plural(bar.v, noun)}` +
      (failed > 0 ? `, ${failed.toLocaleString()} failed` : "")
    );
  };
  return (
    <KitHistogram
      bars={drawn}
      label={label}
      height={height}
      // THE TOTAL IS THE CALLER'S, stated rather than summed: summed it would
      // agree today and stop agreeing the first time a cap is added to either
      // half, and it carries the caller's own noun and scope.
      summary={`${plural(total, noun)} ${SCOPE_SENTENCE[over]}`}
      formatTitle={title}
      formatTick={axis ? (_bar, index) => tick(bars[index]!.at, bucket, now) : undefined}
      onSelect={
        onPick
          ? (bar) => {
              if (Number.isFinite(bar.t)) onPick({ from: bar.t, to: bar.t + step });
            }
          : undefined
      }
    />
  );
}

/** How long one bar covers. */
const STEP_MS: Record<"minute" | "hour" | "day", number> = {
  minute: 60_000,
  hour: 60 * 60_000,
  day: 24 * 60 * 60_000,
};

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
 * zone with no date beside it: it is the `datetime-local` spelling,
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
