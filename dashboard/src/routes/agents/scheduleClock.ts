/**
 * A schedule's times, on ONE clock that is named.
 *
 * A schedule fires on its own clock (its `timezone`, or the company's, which
 * the engine resolves onto every row), and every other time on the screen is
 * drawn on the reader's. The one schedule's page said "next 5 runs, in
 * Europe/Berlin" over a list drawn on the reader's clock, with a footnote
 * saying so: a reader in Los Angeles read 00:30 under a heading that named
 * Berlin, for a standup at 09:30 there. The grid's ticks did the same under a
 * title holding the Berlin minute.
 *
 * SO A SCHEDULE'S TIME IS DRAWN ON THE SCHEDULE'S CLOCK, and the zone is said
 * beside it: the time its author wrote the cron against is the time a reader
 * recognises a fire by. The reader's own time is secondary, labelled as theirs,
 * and only where the two clocks read the instant differently.
 */

import { zoneOffset } from "~/lib/format.ts";
import { dateTimeFormat, knownZone, type DateShape } from "~/lib/formatters.ts";
import { zone as readerZone } from "~/lib/prefs.ts";

/** A fire's instant to the minute, with its weekday: "Mon, Jun 15, 09:00". */
const AT: DateShape = Object.freeze({
  weekday: "short",
  month: "short",
  day: "numeric",
  hour: "2-digit",
  minute: "2-digit",
  hourCycle: "h23",
});

/** And with the zone's short name, for a cell with no heading to name it: "Mon, Jun 15, 09:00 GMT+9". */
const AT_NAMED: DateShape = Object.freeze({ ...AT, timeZoneName: "short" });

/** The same instant's time alone, with its weekday, for the reader's line: "Mon 00:00". */
const YOURS: DateShape = Object.freeze({
  weekday: "short",
  hour: "2-digit",
  minute: "2-digit",
  hourCycle: "h23",
});

/** An instant of the history: an ISO string, or milliseconds. */
type Instant = string | number;

function msOf(at: Instant): number | null {
  const ms = typeof at === "number" ? at : Date.parse(at);
  return Number.isFinite(ms) ? ms : null;
}

/**
 * The instant on `tz`'s clock, to the minute, or "" for one that is not an
 * instant or a zone this browser cannot read.
 */
export function onClock(at: Instant, tz: string): string {
  const ms = msOf(at);
  const known = knownZone(tz);
  if (ms === null || known === null) return "";
  return dateTimeFormat(undefined, known, AT).format(ms);
}

/**
 * [onClock] with the zone's short name after it, for a time drawn where
 * nothing else on the line names its clock.
 */
export function onClockNamed(at: Instant, tz: string): string {
  const ms = msOf(at);
  const known = knownZone(tz);
  if (ms === null || known === null) return "";
  return dateTimeFormat(undefined, known, AT_NAMED).format(ms);
}

/**
 * Whether the reader's clock reads the instant differently from `tz`'s: a
 * different offset at that instant, whatever the zones are called. Two names
 * for one offset read alike, and a zone that is the reader's in winter and not
 * in summer differs on exactly the instants it should.
 */
export function clocksDiffer(at: Instant, tz: string): boolean {
  const ms = msOf(at);
  const known = knownZone(tz);
  const mine = knownZone(readerZone());
  if (ms === null || known === null || mine === null) return false;
  return zoneOffset(ms, known) !== zoneOffset(ms, mine);
}

/**
 * The instant as the reader's own clock reads it, labelled theirs ("Sun 17:00
 * your time"), or "" where it reads the same on `tz`'s clock.
 */
export function yourTime(at: Instant, tz: string): string {
  const ms = msOf(at);
  if (ms === null || !clocksDiffer(ms, tz)) return "";
  const mine = knownZone(readerZone()) ?? "UTC";
  return `${dateTimeFormat(undefined, mine, YOURS).format(ms)} your time`;
}

/**
 * A zone as a person says it in a line of words: its place, "Berlin" for
 * Europe/Berlin and "Los Angeles" for America/Los_Angeles, and a zone that
 * names no place as itself ("UTC"). For a sentence with little room (a cron's
 * reading in a grid cell); a heading with room names the zone whole.
 */
export function zoneWords(tz: string): string {
  const place = tz.split("/").at(-1) ?? tz;
  return place.replace(/_/g, " ");
}

/**
 * One instant of a schedule for a hint: on its clock with the zone named, and
 * the reader's time beside it where that differs ("Mon, Jun 15, 09:00,
 * Asia/Tokyo time. Sun 17:00 your time.").
 */
export function scheduleTimeHint(at: Instant, tz: string): string {
  const on = onClock(at, tz);
  if (!on) return "";
  const yours = yourTime(at, tz);
  return yours ? `${on}, ${tz} time. ${yours}.` : `${on}, ${tz} time.`;
}
