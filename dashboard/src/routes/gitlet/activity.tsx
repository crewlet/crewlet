/**
 * Pushes on the reader's own days: the bounds a facts read asks with (`day_bounds`), and the
 * spark that draws the counts it answers. Shared by every Gitlet screen that draws a spark.
 */

import { useMemo, type ReactNode } from "react";
import { Histogram } from "@crewlethq/ui";
import { useClock } from "~/lib/clock.ts";
import { civilAt, civilKey, fmtDateCompact, plural, readerDay } from "~/lib/format.ts";
import { zone } from "~/lib/prefs.ts";
import { dayStartIn } from "~/lib/range.ts";
import "~/styles/gitlet.css";

/** Who pushes here counts the reader's last 30 days, today included. */
export const PUSHER_DAYS = 30;
/** The spark draws the last 14 of them. */
export const SPARK_DAYS = 14;
/** The spark's plot height in pixels: the kit chart takes a number. */
const SPARK_HEIGHT = 32;

const DAY_MS = 86_400_000;

/**
 * The first instant of each of the reader's `days` days ending with `today` (a `YYYY-MM-DD` in
 * `inZone`), then tomorrow's: `days + 1` RFC 3339 instants, each a midnight found in the zone.
 */
export function pushDayBounds(today: string, days: number, inZone: string = zone()): string[] {
  const at = civilAt(today);
  if (at === null) return [];
  const bounds: string[] = [];
  for (let i = 1 - days; i <= 1; i++) {
    const start = dayStartIn(civilKey(at + i * DAY_MS), inZone);
    if (start === null) return [];
    bounds.push(new Date(start).toISOString().replace(".000Z", "Z"));
  }
  return bounds;
}

/** [pushDayBounds] for the reader's today, moving once a day rather than every tick. */
export function useDayBounds(days: number): readonly string[] {
  const today = useClock((now) => readerDay(now));
  const inZone = zone();
  return useMemo(() => pushDayBounds(today, days, inZone), [today, days, inZone]);
}

/**
 * Pushes a day as one series in the first data colour, inside a figure its caption names; the
 * first day and Today under the bars. `starts` are each count's day's first instant, oldest first.
 */
export function PushSpark({
  counts,
  starts,
  now,
  caption,
}: {
  counts: readonly number[];
  starts: readonly string[];
  now: number;
  caption: ReactNode;
}) {
  const total = counts.reduce((a, b) => a + b, 0);
  const last = counts.length - 1;
  const day = (i: number) => (i === last ? "Today" : fmtDateCompact(starts[i], now));
  return (
    <figure className="code-spark">
      <figcaption>{caption}</figcaption>
      <Histogram
        bars={counts.map((v, i) => ({ t: Date.parse(starts[i] ?? ""), v }))}
        label={`Pushes a day over the last ${plural(counts.length, "day")}`}
        summary={`${plural(total, "push", "pushes")} over the last ${plural(counts.length, "day")}`}
        formatTitle={(bar, i) =>
          `${day(i)} · ${plural(bar.v, "push", "pushes")}${i === last ? " so far" : ""}`
        }
        height={SPARK_HEIGHT}
      />
      <div className="code-spark__ends" aria-hidden="true">
        <span>{day(0)}</span>
        <span>Today</span>
      </div>
    </figure>
  );
}
