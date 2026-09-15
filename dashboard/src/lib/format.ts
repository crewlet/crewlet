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

export function fmtTime(ts: string | null | undefined): string {
  const d = parseUTC(ts);
  if (!d) return "";
  return d.toLocaleTimeString(undefined, {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    hour12: false,
    timeZone: zone(),
  });
}

export function fmtDateTime(ts: string | null | undefined): string {
  const d = parseUTC(ts);
  if (!d) return "—";
  return d.toLocaleString(dateLocale(), {
    ...dateParts(),
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    hour12: false,
    timeZone: zone(),
  });
}

export function fmtDate(ts: string | null | undefined): string {
  const d = parseUTC(ts);
  if (!d) return "—";
  return d.toLocaleDateString(dateLocale(), { ...dateParts(), timeZone: zone() });
}

/**
 * A timestamp's full identity, for a `title` on anything that shows a short
 * one.
 *
 * NAMES THE ZONE, which is the whole point: a reader comparing a screenshot
 * with a colleague's, or reading a log beside this screen, has no way to know
 * which zone a bare `14:32` is in — and the answer differs per reader, which
 * is exactly why it cannot be left implicit.
 */
export function fmtStamp(ts: string | null | undefined): string {
  const full = fmtDateTime(ts);
  return full === "—" ? full : `${full} (${zone()})`;
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
  const d = parseUTC(ts);
  if (!d) return "—";
  const thisYear = new Date(now).getFullYear();
  return d.toLocaleDateString(undefined, {
    month: "short",
    day: "2-digit",
    ...(d.getFullYear() === thisYear ? {} : { year: "numeric" }),
  });
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
  if (!at) return "—";
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

/** "in 4m" — the same rules, forward. */
export function inTime(ts: string | null | undefined, now: number): string {
  const at = tsKey(ts);
  if (!at) return "—";
  const secs = Math.round((at - now) / 1000);
  if (secs <= 0) return "due";
  if (secs < 60) return `in ${secs}s`;
  const mins = Math.floor(secs / 60);
  if (mins < 60) return `in ${mins}m`;
  const hours = Math.floor(mins / 60);
  if (hours < 24) return `in ${hours}h`;
  return `in ${Math.floor(hours / 24)}d`;
}

/** A duration in ms as the shortest honest string. */
export function fmtDuration(ms: number | null | undefined): string {
  if (ms == null || !Number.isFinite(ms) || ms < 0) return "—";
  if (ms < 1000) return `${Math.round(ms)} ms`;
  const s = ms / 1000;
  if (s < 60) return `${s < 10 ? s.toFixed(1) : Math.round(s)} s`;
  const m = Math.floor(s / 60);
  const rem = Math.round(s % 60);
  if (m < 60) return `${m}m ${rem}s`;
  const h = Math.floor(m / 60);
  return `${h}h ${m % 60}m`;
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
  if (ms == null || !Number.isFinite(ms)) return "—";
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
  if (n == null || !Number.isFinite(n)) return "—";
  const abs = Math.abs(n);
  if (abs < 10_000) return n.toLocaleString();
  if (abs < 1_000_000) return `${(n / 1000).toFixed(abs < 100_000 ? 1 : 0)}k`;
  if (abs < 1_000_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  return `${(n / 1_000_000_000).toFixed(2)}B`;
}

/**
 * A price, or an honest blank where nobody quoted one.
 *
 * TWO INPUTS, because zero dollars is two different facts. Only a subscription
 * coding CLI reports a price, so most phases carry none at all — and rendering
 * that as "$0.00" states a price nobody quoted, under a company that may well
 * be spending thousands. `priced` is the count of calls that said anything;
 * at zero the answer is that nothing was quoted.
 *
 * Four decimals below a cent because that is the scale these come back at: a
 * phase costs $0.0374, and two decimals round every one of them to nothing.
 */
export function fmtSpend(usd: number | null | undefined, priced: number): string {
  if (!priced || usd == null || !Number.isFinite(usd)) return "—";
  if (usd > 0 && usd < 0.01) return `$${usd.toFixed(4)}`;
  return `$${usd.toFixed(2)}`;
}

/** Always the exact figure, grouped. For a cell a reader is comparing. */
export function fmtExact(n: number | null | undefined): string {
  return n == null || !Number.isFinite(n) ? "—" : n.toLocaleString();
}

export function fmtPct(part: number, whole: number, digits = 0): string {
  if (!whole) return "—";
  return `${((part / whole) * 100).toFixed(digits)}%`;
}

export function fmtBytes(n: number | null | undefined): string {
  if (n == null || !Number.isFinite(n)) return "—";
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

/**
 * "1 seat", "2 seats" — a count and its noun, agreeing.
 *
 * A helper rather than a ternary at every call site: the previous product
 * printed "1 humans", "1 seats" and "1 phases loaded" on three different
 * screens, which is the kind of thing nobody fixes one at a time.
 */
export function plural(n: number, one: string, many?: string): string {
  return `${n.toLocaleString()} ${n === 1 ? one : (many ?? `${one}s`)}`;
}
