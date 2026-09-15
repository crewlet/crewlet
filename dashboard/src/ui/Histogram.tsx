/**
 * A log's own time axis: where in this window did things happen.
 *
 * WHAT A PAGE OF ROWS CANNOT SAY. A listing answers what happened; it has no
 * dimension for when the company was busy, so a burst at four in the morning
 * and a steady trickle across a week read identically once they are a hundred
 * rows in a column. This is the shape every log tool has for exactly that
 * reason, and it is the one the event log did not have.
 *
 * THE BARS ARE THE ENGINE'S, never folded here. The browser holds at most a
 * page and the store's window it never holds, so an axis folded client-side
 * would be right for one window and absent for every other — the same rule
 * `internal/tokens` states for the spend series one screen over. It also means
 * the bars and the rows below them are counted from ONE predicate, so a bar can
 * never claim rows the list would not show.
 *
 * A BAR IS A CONTROL, not a picture. Clicking one narrows the window to it,
 * which is the gesture a reader already expects from every log they have used —
 * and the reason the axis is worth having at all, since "what happened in that
 * spike" is the question the spike creates.
 */

import { fmtDate, fmtMinute, plural } from "~/lib/format.ts";
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

export function Histogram({
  bars,
  bucket,
  total,
  onPick,
  height = 96,
  label = "Events over the window",
}: {
  bars: Bar[];
  /** How wide one bar is, for the tooltip and for what a click selects. */
  bucket: "minute" | "hour" | "day";
  /** The window's whole count, as the engine reports it. */
  total: number;
  /** Narrow the window to one bar. Omitted leaves the bars as a picture. */
  onPick?: (next: Interval) => void;
  height?: number;
  label?: string;
}) {
  const peak = Math.max(1, ...bars.map((b) => b.count));
  const step = STEP_MS[bucket];
  return (
    <div className="histogram" style={{ height }} role="group" aria-label={label}>
      {bars.map((b) => {
        const at = Date.parse(b.at);
        const title = `${when(b.at, bucket)} — ${plural(b.count, "event")}`;
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
      {/* THE TOTAL IS THE ENGINE'S, stated rather than summed by the reader
          or by this component: the bars are the whole window, so a sum here
          would agree today and stop agreeing the first time a cap is added
          to either half. */}
      <span className="sr-only">{plural(total, "event")} in this window</span>
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
