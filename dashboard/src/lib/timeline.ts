/**
 * The arithmetic behind the date axis: where a task's bar sits, and which of
 * them can be joined by an arrow.
 *
 * PURE FUNCTIONS OVER ROWS, for the reason `lib/diff.ts` and `lib/cron.ts` are:
 * a layout exercised only by rendering it is a layout nobody re-measures, and
 * every interesting case here — a task dated one way round, an edge whose
 * blocker the filter excluded, a window a single row defines — is a value
 * problem with a value answer.
 *
 * THE UNIT IS A DAY. A week-scaled axis is days drawn narrow with a rule every
 * seventh, not a week-sized bucket: bucketing by week cannot place a
 * three-day task inside one, and the first thing anybody does with a timeline
 * is look at this week.
 *
 * DAYS ARE LOCAL, through `lib/format.ts`'s [browserDay] — the SAME function
 * `lib/work.ts`'s calendar keys its cells with, rather than a second copy of
 * it, so a bar and a calendar cell cannot come to disagree about which day a
 * task is due. The engine resolves a bare date in the COMPANY's zone and this
 * buckets in the READER's; the screen says so, exactly as the calendar does.
 */

import { browserDay } from "./format.ts";
import type { WorkSummary } from "~/protocol/index.ts";

/** A local day key back to a `Date` at local midnight, or null. */
function dayAt(key: string): Date | null {
  if (!/^\d{4}-\d{2}-\d{2}$/.test(key)) return null;
  const at = new Date(`${key}T00:00:00`);
  return Number.isNaN(at.getTime()) ? null : at;
}

/** The local day an instant falls on, or empty when it is unreadable. */
export function dayOf(ts: string | undefined): string {
  if (!ts) return "";
  const at = new Date(ts);
  return Number.isNaN(at.getTime()) ? "" : browserDay(at);
}

/** Whole days from `from` to `to`, negative when `to` is earlier. */
export function daysBetween(from: string, to: string): number {
  const a = dayAt(from);
  const b = dayAt(to);
  if (!a || !b) return 0;
  // ROUNDED, not truncated: a DST boundary inside the span makes the
  // millisecond difference 23 or 25 hours per day, and a truncating divide
  // turns that into an off-by-one on every bar crossing the change.
  return Math.round((b.getTime() - a.getTime()) / 86_400_000);
}

/** `key` shifted by whole days. */
export function shiftDay(key: string, by: number): string {
  const at = dayAt(key);
  if (!at) return key;
  at.setDate(at.getDate() + by);
  return browserDay(at);
}

/**
 * How a bar's two ends were arrived at.
 *
 * A NAMED SET rather than two booleans, because the three shapes a reader has
 * to tell apart are not independent: a task with both dates is a SPAN, one
 * with a due date alone is a DEADLINE with no stated beginning, and one with a
 * start alone is work that BEGAN and was never given an end. Rendering them
 * identically — which a bar drawn from two clamped numbers does — is what
 * makes a timeline look like it knows more than it does.
 */
export type BarKind = "span" | "deadline" | "began" | "inverted";

/** One task's bar, in day columns from the window's first day. */
export interface TimelineBar {
  row: WorkSummary;
  /** The first day column the bar covers, 0-based. */
  from: number;
  /** ONE PAST the last day it covers, so a single-day bar is `from + 1` and
   *  a width is always `to - from`. */
  to: number;
  kind: BarKind;
  /** The local day the bar starts on, for a label and a tooltip. */
  fromDay: string;
  /** The local day it ends on — INCLUSIVE, unlike `to`, because that is the
   *  date a person reads off the screen. */
  toDay: string;
}

/** An arrow between two bars on the same axis. */
export interface TimelineEdge {
  /** The blocker's row id. */
  from: string;
  /** The dependent's row id — the task that waits. */
  to: string;
  /** Whether the blocker is still holding the dependent up. */
  open: boolean;
  /** The blocker does not list this dependent back. Drawn dashed, because it
   *  is an edge one end cannot see. */
  oneSided: boolean;
}

/** Everything a timeline needs to draw itself. */
export interface Timeline {
  /** The window's first day, inclusive. */
  from: string;
  /** Its last day, INCLUSIVE — the day a reader sees at the right edge. */
  to: string;
  /** How many day columns that is. Never zero when there is a window at all. */
  days: number;
  bars: TimelineBar[];
  /** Rows with neither date. They have no bar and are not a band of one day
   *  at today: a timeline that placed them would invent a schedule. */
  unscheduled: WorkSummary[];
  edges: TimelineEdge[];
  /** Edges whose blocker has no bar on this axis — off the page, or
   *  unscheduled. COUNTED rather than dropped silently: a reader who cannot
   *  see the omission reads the arrows as every dependency there is. */
  edgesOffAxis: number;
  /** Today's column, or -1 when today is outside the window. */
  today: number;
}

/**
 * The widest window a set of rows spans, at most this many days.
 *
 * A CAP, because the window is derived from the DATA and one task due in 2031
 * would otherwise compress a fortnight into four pixels. Three hundred and
 * seventy days is a year plus a fortnight: the longest span anybody reads as
 * one picture, and enough that an annual plan's two ends are both on it.
 */
export const MaxTimelineDays = 370;

/**
 * The narrowest one, so a window is never a single column.
 *
 * Fourteen days: a bar in a one-day window has nothing to be read against, and
 * a fortnight is the smallest window in which "this is late" is visible
 * without counting. It pads AROUND the data rather than extending one edge, so
 * a single dated task sits in the middle rather than against a wall.
 */
export const MinTimelineDays = 14;

/** What `timelineOf` is given beyond the rows. */
export interface TimelineOptions {
  /** The reader's now, as epoch ms. Passed rather than read, so the layout is
   *  a pure function and "today" is testable. */
  now: number;
}

/** The two dates a row carries, as local days, either possibly empty. */
function endsOf(row: WorkSummary): { start: string; due: string } {
  return { start: dayOf(row.start), due: dayOf(row.due) };
}

/**
 * The bar a row would occupy, in DAYS, before a window is known — or null when
 * the row carries no date at all.
 *
 * INVERTED DATES ARE NOT SWAPPED. A task starting after it is due is somebody's
 * mistake and the bar covers what the two dates enclose, kind `inverted`, so
 * the screen can say so. Swapping them would render a coherent plan out of
 * incoherent data, and the person who typed it would never find out.
 */
function spanOf(row: WorkSummary): { from: string; to: string; kind: BarKind } | null {
  const { start, due } = endsOf(row);
  if (start && due) {
    if (daysBetween(start, due) < 0) return { from: due, to: start, kind: "inverted" };
    return { from: start, to: due, kind: "span" };
  }
  if (due) return { from: due, to: due, kind: "deadline" };
  if (start) return { from: start, to: start, kind: "began" };
  return null;
}

/**
 * The bars, the window they sit in, and the arrows between them.
 *
 * THE WINDOW IS THE DATA'S, padded to [MinTimelineDays] and capped at
 * [MaxTimelineDays]. A fixed window — this month, this quarter — is the
 * obvious alternative and it is wrong for the same reason a fixed calendar
 * month is right: a calendar answers "what is due in October" and a timeline
 * answers "how does this work lay out", which is a question about the rows
 * rather than about the calendar.
 *
 * ROWS ARE NOT RE-SORTED. They arrive in the order the engine answered, which
 * for the timeline view is by start — see `implicitViews` — and re-sorting
 * here would make a page's order depend on which rows the page happened to
 * hold.
 */
export function timelineOf(rows: WorkSummary[], opts: TimelineOptions): Timeline {
  const spans = rows.map((row) => ({ row, span: spanOf(row) }));
  const dated = spans.filter((e) => e.span !== null);
  const unscheduled = spans.filter((e) => e.span === null).map((e) => e.row);

  if (dated.length === 0) {
    return {
      from: "",
      to: "",
      days: 0,
      bars: [],
      unscheduled,
      edges: [],
      edgesOffAxis: 0,
      today: -1,
    };
  }

  let earliest = dated[0]!.span!.from;
  let latest = dated[0]!.span!.to;
  for (const { span } of dated) {
    if (span!.from < earliest) earliest = span!.from;
    if (span!.to > latest) latest = span!.to;
  }

  // PADDED TO THE MINIMUM AROUND THE DATA, half each side, so one dated task
  // sits in the middle of its window rather than against the left wall.
  let days = daysBetween(earliest, latest) + 1;
  if (days < MinTimelineDays) {
    const short = MinTimelineDays - days;
    const before = Math.floor(short / 2);
    earliest = shiftDay(earliest, -before);
    latest = shiftDay(latest, short - before);
    days = MinTimelineDays;
  }
  if (days > MaxTimelineDays) {
    // CUT AT THE FAR END, never the near one: the rows before the cut are
    // the ones somebody is working on, and a window trimmed from the left
    // would drop today off an axis whose whole point is where today is.
    latest = shiftDay(earliest, MaxTimelineDays - 1);
    days = MaxTimelineDays;
  }

  const bars: TimelineBar[] = [];
  for (const { row, span } of dated) {
    const from = daysBetween(earliest, span!.from);
    const to = daysBetween(earliest, span!.to) + 1;
    // A BAR OUTSIDE THE WINDOW IS CLIPPED, NOT DROPPED. Only the far-end cut
    // above can produce one, and a task the cut pushed off the axis still has
    // a row: a timeline missing rows the list shows is a timeline nobody
    // trusts.
    //
    // THE FAR END IS THE ONLY ONE THAT NEEDS CLAMPING, and a floor on either
    // would be unreachable code rather than a guard: `from` is a distance
    // from the window's own earliest day so it is never negative, and `to` is
    // strictly greater than `from`, so pinning `from` to the last column
    // leaves `to` at the one past it. `fromDay` and `toDay` still carry the
    // TRUE dates, so a clipped bar's label says what it really spans.
    const clippedFrom = Math.min(from, days - 1);
    const clippedTo = Math.min(to, days);
    bars.push({
      row,
      from: clippedFrom,
      to: clippedTo,
      kind: span!.kind,
      fromDay: span!.from,
      toDay: span!.to,
    });
  }

  const onAxis = new Set(bars.map((bar) => bar.row.id));
  const edges: TimelineEdge[] = [];
  let edgesOffAxis = 0;
  for (const bar of bars) {
    for (const blocker of bar.row.waiting_on ?? []) {
      if (!onAxis.has(blocker.id)) {
        edgesOffAxis++;
        continue;
      }
      edges.push({
        from: blocker.id,
        to: bar.row.id,
        open: blocker.open === true,
        oneSided: blocker.one_sided === true,
      });
    }
  }

  const todayKey = browserDay(new Date(opts.now));
  const todayColumn = daysBetween(earliest, todayKey);
  return {
    from: earliest,
    to: latest,
    days,
    bars,
    unscheduled,
    edges,
    edgesOffAxis,
    today: todayColumn >= 0 && todayColumn < days ? todayColumn : -1,
  };
}

/** One tick on the axis: the first day of a week inside the window. */
export interface TimelineTick {
  /** Its day column. */
  at: number;
  /** The local day it falls on. */
  day: string;
  /** What to write above it. */
  label: string;
}

/**
 * The week rules, Monday first.
 *
 * MONDAY for `lib/work.ts`'s reason: `internal/tracker/dates.go` pins the
 * week's start there, so the relative tokens a saved view can carry mean
 * Monday and an axis ruled on Sunday would cut "this week" in two.
 *
 * The FIRST tick is the window's own first day even when it is mid-week —
 * a ruled axis whose leftmost column has no label reads as a column that is
 * not there — UNLESS a Monday falls close enough that the two labels would
 * overlap. A window starting on a Sunday rendered "Sep 2Sep 28", which is not
 * a date at all.
 */
export function weekTicks(line: Timeline): TimelineTick[] {
  if (line.days === 0) return [];
  const out: TimelineTick[] = [];
  for (let i = 0; i < line.days; i++) {
    const day = shiftDay(line.from, i);
    const at = dayAt(day);
    if (!at) continue;
    if (i !== 0 && at.getDay() !== 1) continue;
    if (i !== 0 && out[0]?.at === 0 && i < MinTickGap) out.shift();
    out.push({ at: i, day, label: monthDay(day) });
  }
  return out;
}

/**
 * How many day columns two tick labels need between them.
 *
 * Three: a label is "28 Sep" at the axis's own small size, about 45px, and a
 * day column is 26px wide — so two ticks two columns apart run into each
 * other and three is the first gap that does not. It is the ONE place the
 * arithmetic knows a pixel width, and it is here rather than in the renderer
 * because dropping a tick changes what the axis says, not how it looks.
 */
const MinTickGap = 3;

/** `2026-06-15` as `15 Jun`, in the reader's own locale. */
export function monthDay(day: string): string {
  const at = dayAt(day);
  if (!at) return day;
  return at.toLocaleDateString(undefined, { day: "numeric", month: "short" });
}

/**
 * What a bar's dates say, as a sentence.
 *
 * ONE PLACE, because a bar's title, its tooltip and the unscheduled band's
 * explanation are three renderings of the same fact and three copies is how
 * one of them ends up saying "starts" about a deadline.
 */
export function barTitle(bar: TimelineBar): string {
  const from = monthDay(bar.fromDay);
  const to = monthDay(bar.toDay);
  switch (bar.kind) {
    case "span":
      return from === to ? `${from}` : `${from} → ${to}`;
    case "deadline":
      return `due ${to}, with no start date`;
    case "began":
      return `started ${from}, with no due date`;
    case "inverted":
      // NAMED, not smoothed over: the dates are the wrong way round and the
      // person who typed them is the only one who can fix it.
      return `due ${from} but starting ${to} — the dates are the wrong way round`;
  }
}
