/**
 * Formatting, and the ordering rules that go with it.
 *
 * The ordering half is load-bearing. Timestamps arrive in two encodings —
 * aware (`…+00:00`) and naive — often for the same instant, and Go's
 * `RFC3339Nano` trims trailing zeros, so a raw string compare puts `…:07Z`
 * before `…:07.42Z` by comparing `'Z'` (0x5A) against `'.'` (0x2E) and orders
 * the LATER instant first. Store rows are additionally microsecond-truncated
 * while live rows keep nanoseconds. Every list in this product therefore sorts
 * through `tsKey`, never through `<` on the string.
 */

// THE MARK AN ABSENT VALUE WEARS IS THE DESIGN SYSTEM'S, not a literal here.
// This file owns how a value is SPELLED, and "there is no value" is one of
// those spellings — written as a local EMPTY_VALUE it was a SECOND mark beside
// `EmptyValue`'s en dash, and the work trash screen drew both at once: a 12px
// em dash in the table's DUE column and a 6px en dash in the activity feed's
// object column, one fact and two glyphs in one viewport. Importing the
// constant rather than the component keeps this module free of React, which is
// what lets it stay a leaf every screen and every test can reach.
import { EMPTY_VALUE } from "@crewlethq/ui";
import { dates, zone } from "./prefs.ts";

/** Parse an ISO timestamp, tolerating a naive one (the engine emits both). */

export function parseUTC(ts: string | null | undefined): Date | null {
  if (!ts) return null;
  let s = String(ts);
  if (!/[zZ]|[+-]\d\d:?\d\d$/.test(s)) s += "Z";
  const d = new Date(s);
  return Number.isNaN(d.getTime()) ? null : d;
}

/** The ONE ordering key for a timestamp: epoch ms, or 0 when unparseable. */
export function tsKey(ts: string | null | undefined): number {
  const at = parseUTC(ts);
  return at ? at.getTime() : 0;
}

/**
 * Descending `(instant, id)` comparator — feed order.
 *
 * The id tiebreak is not optional: burst writes share a timestamp at
 * microsecond resolution, and a merge keyed on a non-unique value drops or
 * duplicates whatever collided with it. Returns 0 for genuinely equal rows, so
 * the sort stays transitive and a stable sort can do its job.
 */
export function newestFirst(
  a: { timestamp?: string; id?: string },
  b: { timestamp?: string; id?: string },
): number {
  const at = tsKey(a?.timestamp);
  const bt = tsKey(b?.timestamp);
  if (at !== bt) return bt - at;
  const ai = String(a?.id ?? "");
  const bi = String(b?.id ?? "");
  return ai < bi ? 1 : ai > bi ? -1 : 0;
}

/** Ascending `(instant, id)` comparator — trace and transcript order. */
export function oldestFirst(
  a: { timestamp?: string; id?: string },
  b: { timestamp?: string; id?: string },
): number {
  return -newestFirst(a, b);
}

// ---------------------------------------------------------------------------
// Time
// ---------------------------------------------------------------------------

/**
 * The zone and the date shape every formatter below renders in.
 *
 * READ FROM THE MODULE rather than taken as an argument, which is the one
 * place this file departs from its own "pass what you need" rule. A zone is a
 * preference held by one browser and read by three hundred call sites; passing
 * it would mean threading it through every component that renders a timestamp,
 * and the ones that forgot would silently render in a different zone from the
 * ones beside them — the exact inconsistency the preference exists to end.
 *
 * `lib/prefs.ts` announces a change, so a component that must re-render on one
 * subscribes with `useViewerPrefs()`. The formatters themselves stay pure
 * functions of (timestamp, preference): same inputs, same output.
 */
function dateParts(): Intl.DateTimeFormatOptions {
  switch (dates()) {
    case "iso":
      // 2026-06-15. `en-CA` is the locale whose numeric date IS ISO order,
      // which is how this is spelled without hand-assembling the string and
      // losing the runtime's own calendar handling.
      return { year: "numeric", month: "2-digit", day: "2-digit" };
    case "long":
      return { year: "numeric", month: "long", day: "numeric" };
    default:
      return { year: "numeric", month: "short", day: "2-digit" };
  }
}

/** The locale each date shape is written in. */
function dateLocale(): string | undefined {
  return dates() === "iso" ? "en-CA" : undefined;
}

/**
 * The formatter for a locale and a set of options, built once and kept.
 *
 * EVERY DATE IN THIS PRODUCT IS FORMATTED THROUGH HERE, because building an
 * `Intl.DateTimeFormat` is the expensive half of formatting one — the format
 * is a few microseconds, the construction a couple of hundred under the
 * development build — and `toLocaleString(locale, options)` builds one per
 * call. A hundred-row audit built three hundred of them a render (its dates,
 * its tooltips, and a third to learn the browser's zone each time), to
 * produce the same handful of formatters over and over. ECMA-402 defines the
 * `toLocale*String` methods as exactly this construction followed by
 * `format`, which is what makes the output identical and what
 * `format.intl.test.ts` holds across zones and date shapes.
 *
 * KEYED ON THE LOCALE AND THE OPTIONS AS GIVEN — the zone is one of the
 * options — so a reader changing their zone or their date shape is a new key
 * and the old formatters age out rather than answering for the new choice.
 * Callers build their options in one order per call site, which is what lets
 * the serialisation be the key.
 */
export function dateFormatter(
  locale: string | undefined,
  options: Intl.DateTimeFormatOptions,
): Intl.DateTimeFormat {
  const key = JSON.stringify([locale ?? "", options]);
  const held = dateFormatters.get(key);
  if (held) {
    // MOST RECENTLY USED LAST, so the bound evicts what nothing has asked for
    // longest — a zone the reader left, a date shape they changed.
    dateFormatters.delete(key);
    dateFormatters.set(key, held);
    return held;
  }
  const made = new Intl.DateTimeFormat(locale, options);
  dateFormatters.set(key, made);
  if (dateFormatters.size > FORMATTERS_KEPT) {
    const oldest = dateFormatters.keys().next().value;
    if (oldest !== undefined) dateFormatters.delete(oldest);
  }
  return made;
}

/**
 * How many formatters [dateFormatter] keeps.
 *
 * THE LIVE SET IS SMALL AND KNOWN: the six formatters below in the one zone
 * and date shape a reader has chosen, the handful of calendar and axis labels
 * in the browser's own, and one per zone the schedules are evaluated in
 * ([zoneOffset]) — a few dozen at the outside. Sixty-four holds all of that
 * with room for a reader flipping through zones and date shapes in the
 * preferences, and bounds what a session that does so for an hour can hold.
 * A formatter evicted while still in use is rebuilt once, which is the whole
 * cost of the bound being too small; there is no reader-visible difference
 * between this and any larger number, so it is a constant and not a setting.
 */
export const FORMATTERS_KEPT = 64;

const dateFormatters = new Map<string, Intl.DateTimeFormat>();

/**
 * A number grouped in the reader's locale — `12,345` — through one formatter.
 *
 * `n.toLocaleString()` with no arguments is this formatter's `format`, by the
 * same ECMA-402 definition, built afresh per call.
 */
function grouped(n: number): string {
  return numberFormatter().format(n);
}

const numberFormatter = (() => {
  let made: Intl.NumberFormat | null = null;
  return () => (made ??= new Intl.NumberFormat());
})();

/**
 * Two strings in the order a reader expects — `seat-2` before `seat-10`, and
 * case and accents aside — through one collator.
 *
 * `a.localeCompare(b, locale, options)` is `new Intl.Collator(locale,
 * options).compare(a, b)` by definition, so a sort over it built a collator
 * per COMPARISON: a grid sorting a hundred rows by a text column built
 * several hundred of them to order one list.
 */
export function naturalCompare(a: string, b: string): number {
  return naturalCollator().compare(a, b);
}

const naturalCollator = (() => {
  let made: Intl.Collator | null = null;
  return () => (made ??= new Intl.Collator(undefined, { numeric: true, sensitivity: "base" }));
})();

export function fmtTime(ts: string | null | undefined): string {
  const d = parseUTC(ts);
  if (!d) return "";
  return dateFormatter(undefined, {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    hour12: false,
    timeZone: zone(),
  }).format(d);
}

export function fmtDateTime(ts: string | null | undefined): string {
  const d = parseUTC(ts);
  if (!d) return EMPTY_VALUE;
  return dateFormatter(dateLocale(), {
    ...dateParts(),
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    hour12: false,
    timeZone: zone(),
  }).format(d);
}

export function fmtDate(ts: string | null | undefined): string {
  const d = parseUTC(ts);
  if (!d) return EMPTY_VALUE;
  return dateFormatter(dateLocale(), { ...dateParts(), timeZone: zone() }).format(d);
}

/**
 * A timestamp to the MINUTE — no seconds.
 *
 * For a value a reader CHOSE at minute resolution, where the seconds are
 * always `:00` and are therefore fourteen characters of noise per window in a
 * label that has to sit in a page bar beside everything else. Distinct from
 * [fmtDateTime], which renders an instant the ENGINE recorded and where the
 * second is a real part of the answer.
 */
export function fmtMinute(ts: string | null | undefined): string {
  const d = parseUTC(ts);
  if (!d) return EMPTY_VALUE;
  return dateFormatter(dateLocale(), {
    ...dateParts(),
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
    timeZone: zone(),
  }).format(d);
}

/**
 * A date with the year dropped when it is the reader's OWN year.
 *
 * For a column of dates — a list of due dates, a calendar's own cells — the
 * year is the same four characters on almost every row, and the one date that
 * is NOT in this year is the only one the reader has to notice. Repeating it
 * everywhere is how that one hides.
 *
 * `now` is a required argument for the reason [relTime]'s is: every date on a
 * screen has to agree about which year is the current one, and a function
 * that read the clock itself would decide that separately per render.
 */
export function fmtDateCompact(ts: string | null | undefined, now: number): string {
  return fmtDateCompactIn(ts, currentYear(now));
}

/**
 * [fmtDateCompact], given the current year rather than the instant.
 *
 * FOR A MARK DRAWN ON EVERY ROW. The only thing a compact date reads off the
 * clock is which year it is, and that changes once a year — so a row's due
 * date subscribes to the YEAR (`useClockReading(currentYear)`) and renders when
 * it turns, rather than taking the second and rendering with it.
 */
export function fmtDateCompactIn(ts: string | null | undefined, thisYear: string): string {
  const d = parseUTC(ts);
  if (!d) return EMPTY_VALUE;
  // BOTH YEARS READ IN THE VIEWER'S ZONE, and the date rendered in it. This
  // read the browser's calendar on both counts while every other formatter in
  // this file rendered in the chosen one, so a reader in `Pacific/Auckland`
  // whose browser sat in `UTC` saw a due date one day earlier here than in the
  // tooltip beside it — and, for thirteen hours a year, a year earlier.
  return dateFormatter(undefined, {
    month: "short",
    day: "2-digit",
    timeZone: zone(),
    ...(calendarYear(d) === thisYear ? {} : { year: "numeric" }),
  }).format(d);
}

/**
 * Which calendar year `now` falls in, IN THE VIEWER'S ZONE.
 *
 * REMEMBERED FOR THE LAST INSTANT ASKED, because every due date on a screen
 * asks it of the same instant in the same tick of the clock: a board of two
 * hundred cards is one reading per second rather than two hundred.
 */
export function currentYear(now: number): string {
  const tz = zone();
  if (lastYear.now !== now || lastYear.zone !== tz) {
    lastYear = { now, zone: tz, year: calendarYear(new Date(now)) };
  }
  return lastYear.year;
}

let lastYear = { now: Number.NaN, zone: "", year: "" };

/**
 * "Sep 9", from a COMPANY DATE LABEL (`2026-09-09`) — a day the engine cut on
 * the company's clock, drawn as that date rather than re-derived from an
 * instant on this browser's: read at noon UTC and formatted in UTC, so no
 * reader's zone moves it to the day before. The label as it came where it is
 * not a date.
 *
 * ONE COPY. Home's completed chart, its projects' target dates and a seat's
 * turns-per-day chart each wrote this privately, which is how three charts
 * come to label one day three ways.
 */
export function companyDateLabel(date: string): string {
  const at = Date.parse(`${date}T12:00:00Z`);
  if (!Number.isFinite(at)) return date;
  return dateFormatter(undefined, { month: "short", day: "numeric", timeZone: "UTC" }).format(at);
}

/** Which calendar year an instant falls in, IN THE VIEWER'S ZONE. */
function calendarYear(at: Date): string {
  return dateFormatter("en-US", { year: "numeric", timeZone: zone() }).format(at);
}

/**
 * "4m ago", "2h ago", "just now".
 *
 * `now` is a REQUIRED argument rather than a call to the clock inside. Every
 * relative time on a screen has to agree with every other, and a component
 * that read the clock itself would re-render on its own schedule and disagree
 * with the row above it. The shell ticks one clock (see `lib/clock.ts`) and
 * passes the instant down — which is also what makes these strings actually
 * advance, instead of freezing at whatever they were when an unrelated push
 * last happened to re-render them.
 */
export function relTime(ts: string | null | undefined, now: number): string {
  const at = tsKey(ts);
  if (!at) return EMPTY_VALUE;
  const secs = Math.round((now - at) / 1000);
  if (secs < 0) return inTime(ts, now);
  if (secs < 5) return "just now";
  if (secs < 60) return `${secs}s ago`;
  const mins = Math.floor(secs / 60);
  if (mins < 60) return `${mins}m ago`;
  const hours = Math.floor(mins / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.floor(hours / 24);
  if (days < 30) return `${days}d ago`;
  return fmtDate(ts);
}

/**
 * "in 4m" — the same rules as [relTime], forward, INCLUDING ITS TERMINUS.
 *
 * That last clause is what this did not have, while the comment above it claimed
 * it did. Backwards, a relative reading stops counting days at thirty and prints
 * the date; forwards it ran on for ever, so "in 341d" was the whole of what a
 * yearly schedule's next run said and a task due next quarter had no date on its
 * screen at all.
 *
 * A relative time is the better answer only while the reader can still hold the
 * interval: past a month it is a subtraction against a calendar they cannot see,
 * and the absolute date is both shorter and exact. The threshold is [relTime]'s
 * own thirty days rather than a second number — a forward reading and a backward
 * one that changed shape at different points would put two clocks on one screen.
 */
export function inTime(ts: string | null | undefined, now: number): string {
  const at = tsKey(ts);
  if (!at) return EMPTY_VALUE;
  const secs = Math.round((at - now) / 1000);
  if (secs <= 0) return "due";
  if (secs < 60) return `in ${secs}s`;
  const mins = Math.floor(secs / 60);
  if (mins < 60) return `in ${mins}m`;
  const hours = Math.floor(mins / 60);
  if (hours < 24) return `in ${hours}h`;
  const days = Math.floor(hours / 24);
  if (days < 30) return `in ${days}d`;
  return fmtDate(ts);
}

/**
 * "in 1h 20m" — [inTime] with a second unit, for instants that are read
 * against EACH OTHER in a series.
 *
 * `inTime` rounds to one unit, which is the right answer for one instant a
 * reader holds in their head and the wrong one for a list whose rows are
 * compared: a schedule firing every twenty minutes read "in 1h", "in 1h" for
 * the fires at 20:40 and 21:00, on the one list whose whole point is the
 * spacing between its rows. Two units tell apart any two instants a minute or
 * more apart inside a day. The terminus is [inTime]'s, for the reason given
 * there.
 */
export function inTimeExact(ts: string | null | undefined, now: number): string {
  const at = tsKey(ts);
  if (!at) return EMPTY_VALUE;
  const secs = Math.round((at - now) / 1000);
  if (secs < 60 * 60) return inTime(ts, now);
  const mins = Math.floor(secs / 60);
  const hours = Math.floor(mins / 60);
  if (hours < 24) return mins % 60 ? `in ${hours}h ${mins % 60}m` : `in ${hours}h`;
  const days = Math.floor(hours / 24);
  if (days < 30) return hours % 24 ? `in ${days}d ${hours % 24}h` : `in ${days}d`;
  return fmtDate(ts);
}

/**
 * A duration in ms as the shortest honest string.
 *
 * UNIT LETTERS WITH NO SPACE AT EVERY MAGNITUDE — "340ms", "1.2s", "45s",
 * "1m 2s", "2h 3m" — which is how [fmtElapsed] and a waterfall's axis
 * (`tickLabel`) write one too. This wrote "340 ms" and "1.2 s" under a minute
 * and "1m 2s" above it, so a trace's duration column and the ruler over it
 * read as three conventions on one screen.
 *
 * NO ZERO FIELD — "30m", "24h", never "30m 0s" — because a finished span has
 * no second hand to keep steady, and "inside the 30m 0s window" read as a
 * measurement to the second of a setting nobody wrote that way. And ONE
 * ROUNDING PER MAGNITUDE, split afterwards, so 59.6s is "1m" and 1h 59m 45s is
 * "2h" rather than "60s" and "1h 60m" — the carry `attention.ts` documents.
 * The engine's alarm sentences spell a duration by the same rule
 * (`internal/statelog`'s `spoken`), so a figure and the sentence beside it
 * agree.
 */
export function fmtDuration(ms: number | null | undefined): string {
  if (ms == null || !Number.isFinite(ms) || ms < 0) return EMPTY_VALUE;
  const pair = (a: number, au: string, b: number, bu: string) =>
    b === 0 ? `${a}${au}` : `${a}${au} ${b}${bu}`;
  if (Math.round(ms) < 1000) return `${Math.round(ms)}ms`;
  const tenths = Math.round(ms / 100);
  if (tenths < 100)
    return `${Number.isInteger(tenths / 10) ? tenths / 10 : (tenths / 10).toFixed(1)}s`;
  const secs = Math.round(ms / 1000);
  if (secs < 60) return `${secs}s`;
  if (secs < 3600) return pair(Math.floor(secs / 60), "m", secs % 60, "s");
  const mins = Math.round(ms / 60_000);
  return pair(Math.floor(mins / 60), "h", mins % 60, "m");
}

/**
 * How long something has been running, for a clock that reruns every second.
 *
 * Distinct from [fmtDuration], which measures a FINISHED span and is free to
 * be precise: sub-second milliseconds and a tenth of a second are meaningful
 * for a call that took 340 ms. On a live counter they are noise — the number
 * churns through "0 ms", "1.4 s", "1.9 s" and reads as a glitch rather than a
 * clock. Whole seconds from the first tick, and never below zero, because a
 * seat's clock and the browser's disagree by a few hundred milliseconds and a
 * "-1 s" or an "in 1s" is the one reading that is certainly wrong.
 */
export function fmtElapsed(ms: number | null | undefined): string {
  if (ms == null || !Number.isFinite(ms)) return EMPTY_VALUE;
  const s = Math.max(0, Math.floor(ms / 1000));
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${s % 60}s`;
  const h = Math.floor(m / 60);
  return `${h}h ${m % 60}m`;
}

/** Elapsed between two ISO stamps, or null when either is missing. */
export function elapsedMs(from?: string | null, to?: string | null): number | null {
  const a = tsKey(from);
  const b = tsKey(to);
  if (!a || !b) return null;
  return b - a;
}

// ---------------------------------------------------------------------------
// Days
// ---------------------------------------------------------------------------

/**
 * The `YYYY-MM-DD` an instant falls on IN THE READER'S ZONE — the one
 * [zone] names, which is the browser's until the reader picks another.
 *
 * ONE DEFINITION. The calendar's cell keys (`lib/work.ts`) and the timeline
 * axis's (`lib/timeline.ts`) both come from here, which is the shape this
 * repository keeps paying for when it is not (`textcut`, `whsec`,
 * `httpjson`): two copies of one rule, agreeing today, and nothing that
 * notices the day one of them changes. A bar and a calendar cell disagreeing
 * about which day a task is due is a silent wrong answer, not a broken screen.
 *
 * THE SAME ZONE EVERY TIMESTAMP IS DRAWN IN. It used to be the browser's own
 * calendar (`getFullYear`/`getDate`) while every spelling went through
 * [zone], which was harmless only while nothing could set a zone: once the
 * preference had a control, a reader in Berlin who chose `Asia/Tokyo` saw a
 * task stamped "Sep 23" filed in the cell for the 22nd.
 */
export function readerDay(at: Date | number): string {
  const p = wallParts(typeof at === "number" ? at : at.getTime(), zone());
  return `${p.year}-${p.month}-${p.day}`;
}

/*
 * A DAY KEY IS A CIVIL DATE, and arithmetic over one is zone-free. Only the
 * step from an INSTANT to a key needs a zone ([readerDay]); stepping a key by
 * days, counting days between two, or laying a month out on a grid is
 * calendar arithmetic, done here at UTC midnight where no day is 23 or 25
 * hours long and no reader's zone can move a cell. The browser's own `Date`
 * setters would bring the browser's DST back into it.
 */

/** A `YYYY-MM-DD` key as the UTC midnight that starts it, or null. */
export function civilAt(key: string): number | null {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(key);
  if (!m) return null;
  const at = Date.UTC(Number(m[1]), Number(m[2]) - 1, Number(m[3]));
  return Number.isNaN(at) ? null : at;
}

/** The civil `YYYY-MM-DD` of a UTC-midnight instant — [civilAt]'s inverse. */
export function civilKey(at: number): string {
  const d = new Date(at);
  const y = String(d.getUTCFullYear()).padStart(4, "0");
  const m = String(d.getUTCMonth() + 1).padStart(2, "0");
  const day = String(d.getUTCDate()).padStart(2, "0");
  return `${y}-${m}-${day}`;
}

// ---------------------------------------------------------------------------
// Numbers
// ---------------------------------------------------------------------------

/**
 * A count, abbreviated once it stops being readable in full.
 *
 * The threshold is 10,000 rather than 1,000: a four-digit token count is
 * something an operator reads exactly, and rounding it to "1.2k" throws away
 * the digit they were looking at.
 */
export function fmtCount(n: number | null | undefined): string {
  if (n == null || !Number.isFinite(n)) return EMPTY_VALUE;
  const abs = Math.abs(n);
  if (abs < 10_000) return grouped(n);
  if (abs < 1_000_000) return `${whole((n / 1000).toFixed(abs < 100_000 ? 1 : 0))}k`;
  if (abs < 1_000_000_000) return `${whole((n / 1_000_000).toFixed(1))}M`;
  return `${whole((n / 1_000_000_000).toFixed(2))}B`;
}

/** A shortened figure that is whole at its precision says so: `60M`, not
 *  `60.0M`. The `.0` was a digit carrying nothing, on the ceilings a founder
 *  writes as round numbers above all ("60.0M/day and 900.0M/month"). */
const whole = (fixed: string) => fixed.replace(/\.0+$/, "");

/** Always the exact figure, grouped. For a cell a reader is comparing. */
export function fmtExact(n: number | null | undefined): string {
  return n == null || !Number.isFinite(n) ? EMPTY_VALUE : grouped(n);
}

export function fmtPct(part: number, whole: number, digits = 0): string {
  if (!whole) return EMPTY_VALUE;
  return `${((part / whole) * 100).toFixed(digits)}%`;
}

/**
 * How many UTF-8 BYTES a string takes — the unit every engine text cap is
 * counted in. `length` counts UTF-16 code units, which agree with it only for
 * ASCII: a title in Japanese reaches a 256-byte cap at about 85 characters.
 */
export function utf8Bytes(s: string): number {
  return encoder.encode(s).length;
}

const encoder = new TextEncoder();

export function fmtBytes(n: number | null | undefined): string {
  if (n == null || !Number.isFinite(n)) return EMPTY_VALUE;
  const units = ["B", "KB", "MB", "GB"];
  let v = n;
  let u = 0;
  while (v >= 1024 && u < units.length - 1) {
    v /= 1024;
    u++;
  }
  return `${v < 10 && u > 0 ? v.toFixed(1) : Math.round(v)} ${units[u]}`;
}

// ---------------------------------------------------------------------------
// Text
// ---------------------------------------------------------------------------

/** A human label from a snake_case or dotted engine identifier. */
export function humanize(key: string | null | undefined): string {
  if (!key) return "";
  return String(key)
    .replace(/[._]/g, " ")
    .replace(/\s+/g, " ")
    .trim()
    .replace(/^\w/, (c) => c.toUpperCase());
}

/**
 * A conversation key's two halves.
 *
 * The grammar is `{source}:{local}` — `jira:POC-7`, `slack:C9:1718.001`,
 * `github:acme/api#42` — and the local half may itself contain colons, so this
 * splits ONCE.
 */
export function splitConversationKey(key: string): { source: string; local: string } {
  const idx = (key ?? "").indexOf(":");
  if (idx < 0) return { source: "", local: key ?? "" };
  return { source: key.slice(0, idx), local: key.slice(idx + 1) };
}

/** A uuid anywhere inside an identifier. */
const UUID_IN = /[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/gi;

/**
 * A conversation key as a row prints it: every uuid inside it cut to its first
 * eight characters — `work:task:4d631f6d` — and everything else as it is, so
 * `work:ENG-32` and `slack:C9:1718.001` are untouched.
 *
 * THE KEY IS THE THREAD'S ONLY IDENTITY on a list of threads, and a native task
 * nobody gave a key is `work:task:` and a uuid — 46 unbreakable characters that
 * no row has room for, so a list of them read `work:task…` five times over.
 * The head of a uuid is how this product names one everywhere a row is narrow
 * (`workItemLabel`'s "task 4d631f6d"); the whole key belongs on the title, and
 * every caller puts it there.
 */
export function conversationLabel(key: string): string {
  return (key ?? "").replace(UUID_IN, (id) => id.slice(0, 8));
}

/**
 * "1 seat", "2 seats" — a count and its noun, agreeing.
 *
 * A helper rather than a ternary at every call site: the previous product
 * printed "1 humans", "1 seats" and "1 phases loaded" on three different
 * screens, which is the kind of thing nobody fixes one at a time.
 */
export function plural(n: number, one: string, many?: string): string {
  return `${grouped(n)} ${n === 1 ? one : (many ?? `${one}s`)}`;
}

// ---------------------------------------------------------------------------
// Wall clock
// ---------------------------------------------------------------------------

/**
 * An instant as `<input type="datetime-local">` carries it, and back.
 *
 * THE INPUT IS ALWAYS IN THE BROWSER'S ZONE and the dashboard renders in the
 * VIEWER'S CHOSEN one, so the naive `new Date(value)` / `toISOString().slice()`
 * pair is wrong for every reader who set the preference: a reader in `UTC`
 * whose company runs in `Asia/Tokyo` typed 09:00, meant 09:00 Tokyo, and got a
 * window starting nine hours late. These two convert through the chosen zone,
 * so what a reader types is what the heading above the rows says.
 */

/** The wall-clock reading of an instant, `YYYY-MM-DDTHH:mm`. */
export function toWall(at: number): string {
  const p = wallParts(at, zone());
  return `${p.year}-${p.month}-${p.day}T${p.hour}:${p.minute}`;
}

/**
 * The instant a wall-clock reading names, or null when it names none.
 *
 * TWO PASSES, because the offset is a function of the instant and the instant
 * is what is being solved for. Read the wall time as if it were UTC, subtract
 * the zone's offset AT THAT GUESS to land near the answer, then subtract the
 * offset at the answer — which is the correction that matters on the two days
 * a year the first guess straddles a DST change. Inside a skipped hour there
 * is no such instant and inside a repeated one there are two; both land on a
 * defensible reading rather than on an error nobody could act on.
 */
export function fromWall(wall: string): number | null {
  const m = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})/.exec(wall.trim());
  if (!m) return null;
  const naive = Date.UTC(Number(m[1]), Number(m[2]) - 1, Number(m[3]), Number(m[4]), Number(m[5]));
  if (Number.isNaN(naive)) return null;
  const tz = zone();
  const near = naive - zoneOffset(naive, tz);
  return naive - zoneOffset(near, tz);
}

/**
 * How far ahead of UTC a zone is at one instant, in milliseconds.
 *
 * Derived by formatting rather than by a table: `Intl` already holds every
 * zone's rules including the ones that changed last year, and a second copy of
 * them here would be wrong the first time a government moved a date.
 *
 * EXPORTED for `lib/cron.ts`, which projects a walked UTC instant into a
 * SCHEDULE's zone rather than into the reader's and so cannot go through
 * [toWall]/[fromWall]. It throws on a zone `Intl` does not know — the caller
 * decides what to do about that, and cron's answer is to report that it cannot
 * read the schedule rather than to quietly evaluate it somewhere else.
 */
export function zoneOffset(at: number, tz: string): number {
  const p = wallParts(at, tz);
  const asUTC = Date.UTC(
    Number(p.year),
    Number(p.month) - 1,
    Number(p.day),
    Number(p.hour),
    Number(p.minute),
    Number(p.second),
  );
  return asUTC - at;
}

/** One instant's calendar fields in a zone, zero-padded. */
function wallParts(at: number, tz: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const part of wallFormatter(tz).formatToParts(new Date(at))) {
    if (part.type !== "literal") out[part.type] = part.value;
  }
  return out;
}

// One formatter per zone, kept by [dateFormatter] with every other one: a range
// picker rebuilds its two fields on every keystroke, and the cron preview works
// out a schedule's fires through here. A zone `Intl` does not know throws at
// construction and is never kept, which is what lets [zoneOffset] say so.
function wallFormatter(tz: string): Intl.DateTimeFormat {
  return dateFormatter("en-US", {
    timeZone: tz,
    // `hourCycle: "h23"` RATHER THAN `hour12: false`, which is the legacy
    // spelling and selects h24 in some engines: midnight comes back as "24",
    // the previous day's twenty-fourth hour, and fed to Date.UTC it rolls the
    // day forward and lands a whole day out. `h23` is the explicit 00–23
    // cycle, so there is no reading to fold back — and `hour12` takes
    // precedence over `hourCycle` where both are given, so it is absent
    // rather than set to false.
    hourCycle: "h23",
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

// ---------------------------------------------------------------------------
// Values read out of a redacted answer
// ---------------------------------------------------------------------------

/**
 * The mask the engine writes in place of a credential it will not send.
 *
 * `config.Redacted` in Go. A redacted document carries this in every
 * credential field holding a literal; a field holding one whole `${VAR}`
 * reference carries the reference, which NAMES a secret rather than being one.
 */
export const REDACTED = "__redacted__";

/** How a value read from the redacted document may be shown. */
export type ConfigValueKind = "empty" | "hidden" | "reference" | "literal";

/**
 * What a value read from the redacted document is, for rendering.
 *
 * `hidden` is the mask: a literal is set and its value is not on the wire. A
 * `reference` is ONE whole `${NAME}`, the only form the engine sends unmasked
 * in a credential field. Anything else is a `literal`.
 *
 * `secret` says the value comes from a CREDENTIAL field, and there a literal
 * is `hidden` as well. The engine is meant never to send one, but its
 * redaction took any value CONTAINING `${` for a reference, so
 * `Bearer sk-live-${SUFFIX}` reached the wire with its literal half intact.
 * What this dashboard prints must not depend on every engine it talks to
 * having got that right: a credential field shows a whole reference or
 * nothing.
 */
export function configValueKind(
  value: string | null | undefined,
  { secret = false }: { secret?: boolean } = {},
): ConfigValueKind {
  if (value == null || value === "") return "empty";
  if (value === REDACTED) return "hidden";
  // The grammar and the trim are `envref.Whole`'s, so what this calls a
  // reference is exactly what the engine resolves as one.
  if (/^\$\{[A-Za-z_][A-Za-z0-9_]*\}$/.test(value.trim())) return "reference";
  return secret ? "hidden" : "literal";
}

/**
 * How many nodes the fleet has, as the health push counted them — or that the
 * count is not known.
 *
 * `nodes` is ABSENT when the engine could not read the fleet's presence, and
 * on an older node that predates the field; it is never 0, because the node
 * answering is itself one. So an absence is said as an absence rather than
 * guessed at: "0 nodes" beside a page this node is serving is false, and "1
 * node" is a guess that hides a coordination plane nobody can reach.
 */
export function nodeCountLabel(nodes: number | null | undefined): string {
  if (nodes == null || !Number.isFinite(nodes) || nodes <= 0) return "node count unavailable";
  return plural(nodes, "node");
}

/**
 * How far back the event log can be read, as a sentence, from what the engine
 * REPORTED.
 *
 * NOT A LITERAL. Three screens said "the store keeps 30 days" in their own
 * copy while `store.EventHistory` was the only thing that decided it, so a
 * change to the retention would have left all three lying with nothing to
 * catch it. An engine that did not report the floor says so rather than
 * having a number guessed for it: an operator told the wrong floor stops
 * paging early.
 */
export function eventHistoryLabel(seconds: number | null | undefined): string {
  const span = historySpan(seconds);
  return span ? `the store keeps ${span}` : "this engine did not report how far back the log goes";
}

/**
 * The span a read of the event log covers, as the tail of a sentence ("the last
 * 30 days"), from the floor the engine REPORTED — for a count or an empty state
 * that is bounded by that floor and must say by how much. The same rounding as
 * {@link eventHistoryLabel}, so the two can never name different spans; an
 * engine that did not report one gets the window's name, not a guessed number.
 */
export function eventHistorySpan(seconds: number | null | undefined): string {
  const span = historySpan(seconds);
  return span ? `the last ${span}` : "the event log's window";
}

function historySpan(seconds: number | null | undefined): string | null {
  if (seconds == null || !Number.isFinite(seconds) || seconds <= 0) return null;
  const days = Math.round(seconds / 86_400);
  if (days >= 1) return plural(days, "day");
  return plural(Math.max(1, Math.round(seconds / 3_600)), "hour");
}

/**
 * The first line of a body that says anything — what a row or a picker shows
 * of a comment or a question when there is one line to show it in.
 *
 * ONE COPY. It was three: the Inbox's list, its composer (which imported it
 * from the list) and the Home decision row, which kept a private duplicate.
 */
export function firstLine(body: string): string {
  return (
    body
      .split("\n")
      .find((l) => l.trim())
      ?.trim() ?? ""
  );
}
