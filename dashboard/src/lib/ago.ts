// A relative time as a sentence says it, on the reader's calendar: "42 minutes
// ago", "yesterday at 17:40". Cells and chips keep relTime's short form.
import { EMPTY_VALUE } from "@crewlethq/ui";
import { civilAt, fmtDateCompact, parseUTC, plural, readerDay } from "./format.ts";
import { dateTimeFormat, type DateShape } from "./formatters.ts";
import { zone } from "./prefs.ts";

export interface AgoOptions {
  /** Name the time of day on the day before or after: "yesterday at 17:40". */
  readonly clock?: boolean;
  /** After "since": "since a moment ago", "since Oct 02"; an instant ahead is clock skew. */
  readonly since?: boolean;
  /** A time something falls due: once it has arrived it reads "due", as inTime's does. */
  readonly due?: boolean;
}

// h23 rather than hour12: false, so midnight reads 00, never 24.
const CLOCK: DateShape = Object.freeze({ hour: "2-digit", minute: "2-digit", hourCycle: "h23" });

const DAY_MS = 86_400_000;

/** `now` is the one clock's instant (`lib/clock.ts`), passed in as relTime's is. */
export function ago(ts: string | null | undefined, now: number, options?: AgoOptions): string {
  const d = parseUTC(ts);
  if (!d) return EMPTY_VALUE;
  const secs = Math.round((now - d.getTime()) / 1000);
  // Calendar days between the two readings, counted at UTC midnight so DST never shifts them.
  const days = Math.round(((civilAt(readerDay(now)) ?? 0) - (civilAt(readerDay(d)) ?? 0)) / DAY_MS);
  const at = () =>
    options?.clock ? ` at ${dateTimeFormat(undefined, zone(), CLOCK).format(d)}` : "";
  if (options?.since && secs < 60) return "a moment ago";
  if (options?.due && secs > -5) return secs >= 0 ? "due" : "in less than a minute";
  if (secs > -5) {
    if (secs < 60) return "just now";
    if (secs < 3600) return `${plural(Math.floor(secs / 60), "minute")} ago`;
    if (days === 0) return `${plural(Math.floor(secs / 3600), "hour")} ago`;
    if (days === 1) return `yesterday${at()}`;
    if (days < 30) return `${plural(days, "day")} ago`;
    return options?.since ? fmtDateCompact(ts, now) : `on ${fmtDateCompact(ts, now)}`;
  }
  const ahead = -secs;
  if (ahead < 60) return "in less than a minute";
  const mins = Math.round(ahead / 60);
  if (mins < 60) return `in ${plural(mins, "minute")}`;
  if (days === 0) return `in ${plural(Math.round(ahead / 3600), "hour")}`;
  if (days === -1) return `tomorrow${at()}`;
  if (days > -30) return `in ${plural(-days, "day")}`;
  return `on ${fmtDateCompact(ts, now)}`;
}
