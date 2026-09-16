/**
 * A five-field cron expression, in English and as the instants it fires at.
 *
 * The schedules screen printed `0 9 * * 1-5` and nothing else. A reader who
 * knows cron reads it; a founder reads five numbers and a dash and has to
 * trust that somebody got it right — on the one screen that says when the
 * company wakes itself up.
 *
 * TWO ANSWERS, because they are read at different moments. [describe] is a
 * sentence for a cell in a list: "weekdays at 09:00". [nextFires] is the
 * actual instants, for the one schedule a reader has opened and is asking
 * "and when does that mean" about — the engine already sends ONE next fire,
 * and the fires after it are what say whether an expression means what its
 * author thought.
 *
 * IN-TREE for the reason the markdown renderer is: cron is a small closed
 * grammar, and a library for it brings a parser, its OWN timezone database and
 * a recurrence engine for the five fields a schedule actually uses — where
 * `Intl` already holds every zone's rules and is the one this file projects
 * through (`lib/format.ts`'s [zoneOffset]).
 *
 * THE ENGINE REMAINS THE AUTHORITY. `next_run` on a row is the engine's own
 * computation and is what the screen shows as "next"; this is a reading aid
 * beside it, never a second source for the same fact. Where the two disagree
 * the engine is right, and the disagreement is worth seeing — which is why
 * this is drawn beside the engine's answer rather than instead of it.
 */

import { zoneOffset } from "./format.ts";

/** One parsed field: the set of values it matches, ascending. */
type Field = number[];

interface Cron {
  minute: Field;
  hour: Field;
  day: Field;
  month: Field;
  weekday: Field;
  /** True when the day-of-month field is `*`, which changes how weekday reads. */
  anyDay: boolean;
  /** True when the weekday field is `*`. */
  anyWeekday: boolean;
}

const BOUNDS: Record<string, [number, number]> = {
  minute: [0, 59],
  hour: [0, 23],
  day: [1, 31],
  month: [1, 12],
  weekday: [0, 6],
};

const NAMES: Record<string, number> = {
  sun: 0,
  mon: 1,
  tue: 2,
  wed: 3,
  thu: 4,
  fri: 5,
  sat: 6,
  jan: 1,
  feb: 2,
  mar: 3,
  apr: 4,
  may: 5,
  jun: 6,
  jul: 7,
  aug: 8,
  sep: 9,
  oct: 10,
  nov: 11,
  dec: 12,
};

const DAY_LABEL = ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"];
const MONTH_LABEL = [
  "January",
  "February",
  "March",
  "April",
  "May",
  "June",
  "July",
  "August",
  "September",
  "October",
  "November",
  "December",
];

/**
 * Parse one field into the values it matches.
 *
 * Returns null on anything this grammar does not have — which the caller
 * reports as "this build cannot read that expression" rather than guessing.
 * The engine is what runs the schedule, so a reading aid that could not parse
 * an expression must say so; inventing a reading would put a sentence on the
 * screen that the engine does not act on.
 */
function parseField(raw: string, name: string): Field | null {
  const [lo, hi] = BOUNDS[name] as [number, number];
  const out = new Set<number>();
  for (const part of raw.split(",")) {
    const [range, stepRaw] = part.split("/");
    const step = stepRaw === undefined ? 1 : Number(stepRaw);
    if (!Number.isInteger(step) || step < 1) return null;

    let from = lo;
    let to = hi;
    if (range !== "*" && range !== undefined) {
      const ends = range.split("-");
      if (ends.length > 2) return null;
      const first = value(ends[0] ?? "", lo, hi);
      if (first === null) return null;
      from = first;
      if (ends.length === 2) {
        const second = value(ends[1] ?? "", lo, hi);
        if (second === null) return null;
        to = second;
      } else {
        // A BARE VALUE WITH A STEP is a range to the end of the field —
        // `5/10` in the minute field is 5, 15, 25… — while a bare value
        // alone is only itself.
        to = stepRaw === undefined ? first : hi;
      }
    }
    if (from > to) return null;
    for (let v = from; v <= to; v += step) out.add(v);
  }
  if (out.size === 0) return null;
  return [...out].sort((a, b) => a - b);
}

function value(raw: string, lo: number, hi: number): number | null {
  const word = NAMES[raw.trim().toLowerCase()];
  const n = word ?? Number(raw);
  if (!Number.isInteger(n) || n < lo) return null;
  // SUNDAY IS BOTH 0 AND 7 in every cron this engine will meet, and a
  // schedule written `0 9 * * 7` that silently matched nothing would be a
  // schedule that never fires with nothing on the screen to say why.
  if (hi === 6 && n === 7) return 0;
  if (n > hi) return null;
  return n;
}

/** Parse a whole expression, or null when this build cannot read it. */
export function parseCron(expression: string): Cron | null {
  const parts = (expression ?? "").trim().split(/\s+/);
  if (parts.length !== 5) return null;
  const minute = parseField(parts[0] as string, "minute");
  const hour = parseField(parts[1] as string, "hour");
  const day = parseField(parts[2] as string, "day");
  const month = parseField(parts[3] as string, "month");
  const weekday = parseField(parts[4] as string, "weekday");
  if (!minute || !hour || !day || !month || !weekday) return null;
  return {
    minute,
    hour,
    day,
    month,
    weekday,
    anyDay: parts[2] === "*",
    anyWeekday: parts[4] === "*",
  };
}

/**
 * The expression as a sentence, or null when it cannot be read.
 *
 * A SENTENCE FOR THE COMMON SHAPES and a fallback for the rest: the point is
 * that a founder can read the schedules their company actually has, not that
 * every expression cron admits gets prose. An expression this does not have a
 * phrase for still renders — as its own text, beside the engine's next fire.
 */
export function describe(expression: string): string | null {
  const cron = parseCron(expression);
  if (!cron) return null;

  const when = timeOf(cron);
  const days = daysOf(cron);
  const months = monthsOf(cron);
  return [when, days, months].filter(Boolean).join(" ");
}

function timeOf(cron: Cron): string {
  const everyMinute = cron.minute.length === 60;
  const everyHour = cron.hour.length === 24;
  if (everyMinute && everyHour) return "every minute";
  if (everyMinute) return `every minute of ${list(cron.hour.map(hh))}`;
  const step = stepOf(cron.minute, 0, 59);
  if (step && everyHour) return `every ${step} minutes`;
  if (everyHour) return `${list(cron.minute.map(mm))} past every hour`;
  const hourStep = stepOf(cron.hour, 0, 23);
  if (hourStep && cron.minute.length === 1) {
    return `every ${hourStep} hours at ${mm(cron.minute[0] as number)} past`;
  }
  if (cron.minute.length === 1) {
    return `at ${list(cron.hour.map((h) => clock(h, cron.minute[0] as number)))}`;
  }
  return `at ${list(cron.minute.map(mm))} past ${list(cron.hour.map(hh))}`;
}

function daysOf(cron: Cron): string {
  // BOTH FIELDS RESTRICTED IS AN **OR** IN CRON, which is the one rule people
  // get wrong: `0 0 1 * 1` fires on the first of the month AND on every
  // Monday, not on a Monday that is the first.
  if (!cron.anyDay && !cron.anyWeekday) {
    return `on ${ordinals(cron.day)} and on ${weekdays(cron.weekday)}`;
  }
  if (!cron.anyDay) return `on ${ordinals(cron.day)}`;
  if (!cron.anyWeekday) return `on ${weekdays(cron.weekday)}`;
  return "every day";
}

function monthsOf(cron: Cron): string {
  if (cron.month.length === 12) return "";
  return `in ${list(cron.month.map((m) => MONTH_LABEL[m - 1] as string))}`;
}

function weekdays(days: Field): string {
  if (days.length === 5 && days.every((d) => d >= 1 && d <= 5)) return "weekdays";
  if (days.length === 2 && days.includes(0) && days.includes(6)) return "weekends";
  return list(days.map((d) => DAY_LABEL[d] as string));
}

function ordinals(days: Field): string {
  return list(days.map((d) => `the ${d}${suffix(d)}`));
}

function suffix(n: number): string {
  if (n % 100 >= 11 && n % 100 <= 13) return "th";
  return ["th", "st", "nd", "rd"][n % 10] ?? "th";
}

/** The step of an evenly spaced full-range field, or 0 when it is not one. */
function stepOf(field: Field, lo: number, hi: number): number {
  if (field.length < 2 || field[0] !== lo) return 0;
  const step = (field[1] as number) - lo;
  if (step < 2) return 0;
  for (let i = 0; i < field.length; i++) {
    if (field[i] !== lo + i * step) return 0;
  }
  // The last value must be the last one that fits, or the field is a list
  // that merely starts evenly.
  return (field[field.length - 1] as number) + step > hi ? step : 0;
}

function mm(m: number): string {
  return `:${String(m).padStart(2, "0")}`;
}

function hh(h: number): string {
  return `${String(h).padStart(2, "0")}:00`;
}

function clock(h: number, m: number): string {
  return `${String(h).padStart(2, "0")}:${String(m).padStart(2, "0")}`;
}

function list(items: string[]): string {
  if (items.length <= 2) return items.join(" and ");
  return `${items.slice(0, -1).join(", ")} and ${items[items.length - 1]}`;
}

/**
 * The next `count` instants this expression fires at, after `from`.
 *
 * IN THE SCHEDULE'S OWN ZONE, which is a REQUIRED argument because there is no
 * safe default for it. The engine evaluates a schedule in the zone its row
 * names (`internal/schedule/entries.go` resolves it, `Expr.FireTimes` takes the
 * `*time.Location`), so a reading aid that assumed UTC was not merely wrong
 * across a daylight-saving change — it was wrong by the zone's STANDING offset
 * on every row of every company that does not run in UTC. `0 9 * * 1-5` in
 * `Asia/Tokyo` fires at 00:00Z and this listed 09:00Z, nine hours late, for
 * ever. A parameter with a default would have kept that silent at the one call
 * site that forgot it, so the type refuses one.
 *
 * WALKED IN UTC AND MATCHED ON THE LOCAL PROJECTION, which is the engine's own
 * direction and its whole DST design ([Expr.Next] states it): every UTC instant
 * has exactly one local reading, so a repeated local hour yields two fires and
 * a vanished one yields none, with nothing to invent. Constructing local times
 * instead — the obvious way to skip whole days — has to answer both of those
 * cases out of thin air.
 *
 * Minute by minute, capped: cron's own resolution is a minute, and an
 * expression that fires once a year would otherwise walk half a million
 * iterations. The cap returns what it found rather than throwing, and a
 * caller that got fewer than it asked for says so.
 */
export function nextFires(expression: string, from: Date, zone: string, count = 5): Date[] {
  const cron = parseCron(expression);
  if (!cron || count <= 0) return [];
  const project = projector(zone);
  if (!project) return [];
  const out: Date[] = [];
  // Start at the next whole minute: a fire at the current minute has either
  // happened or is happening, and neither is "next".
  let at = Math.floor(from.getTime() / 60_000) * 60_000 + 60_000;
  if (!Number.isFinite(at)) return [];

  // Two years of minutes, which covers every expression a schedule can hold
  // — the coarsest is one day of one month.
  const limit = 366 * 2 * 24 * 60;
  // ONE reused Date for the projection. The walk allocates nothing per minute
  // that it does not return, because the full horizon is a million of them.
  const local = new Date(0);
  for (let i = 0; i < limit && out.length < count; i++) {
    local.setTime(project(at));
    if (matches(cron, local)) out.push(new Date(at));
    at += 60_000;
  }
  return out;
}

/**
 * A walked instant's LOCAL reading in `zone`, as an epoch this file can take
 * `getUTC…` off — or null when the runtime cannot read that zone at all.
 *
 * NOT AN `Intl` CALL PER MINUTE. Formatting each of the horizon's 1,054,080
 * minutes costs seconds, and the horizon is walked in full by exactly the
 * expression a reader is most likely to have mistyped — `0 0 31 2 *` fires
 * never. A zone's offset is instead piecewise constant with a handful of
 * breakpoints a year, so this measures it once per PROBE window and finds the
 * breakpoint EXACTLY, by bisection, on the rare window that straddles one.
 *
 * Six hours is the window because two offset changes inside one would go
 * unseen, and no zone in the modern database has two: the busiest is Morocco
 * at four a year, whose closest pair is weeks apart. It is not a guess about
 * how a zone behaves at a boundary — that is bisected to the minute — only
 * about how often a government can move a clock.
 *
 * Returns a CLOSURE rather than a cache keyed on the zone, because the state
 * it holds is one walk's position and the next walk starts somewhere else.
 */
function projector(zone: string): ((at: number) => number) | null {
  try {
    zoneOffset(0, zone);
  } catch {
    // A ZONE THIS RUNTIME CANNOT READ IS NOT UTC. Falling back would put a
    // list of instants on the screen that the engine does not fire at, with
    // nothing saying so — the same trade [parseCron] already refuses for an
    // expression it cannot read.
    return null;
  }
  const probe = 6 * 3_600_000;
  let offset = 0;
  // The offset is known to hold on [_, until). `-Infinity` makes the first
  // call measure, whatever instant the walk starts at.
  let until = -Infinity;
  return (at) => {
    if (at >= until) {
      offset = zoneOffset(at, zone);
      until = at + probe;
      if (zoneOffset(until, zone) !== offset) until = changeover(at, until, offset, zone);
    }
    return at + offset;
  };
}

/**
 * The first whole minute in `(lo, hi]` whose offset is no longer `offset`.
 *
 * Both ends are minute-aligned and the walk is too, so the answer is exactly
 * the instant the walk must re-measure at — a half-hour zone (`Australia/
 * Lord_Howe` moves by thirty minutes) is no different from a whole-hour one.
 */
function changeover(lo: number, hi: number, offset: number, zone: string): number {
  while (hi - lo > 60_000) {
    const mid = Math.floor((lo + hi) / 2 / 60_000) * 60_000;
    if (mid <= lo || mid >= hi) break;
    if (zoneOffset(mid, zone) === offset) lo = mid;
    else hi = mid;
  }
  return hi;
}

/**
 * Whether `local` — a walked instant already PROJECTED into the schedule's
 * zone — is one this expression fires at.
 *
 * The `getUTC…` readers are how a projection is read back: the projection is
 * an epoch shifted by the zone's offset, so its UTC fields ARE the schedule's
 * wall clock. Reading the local ones instead would apply the browser's zone
 * on top of the schedule's.
 */
function matches(cron: Cron, local: Date): boolean {
  if (!cron.minute.includes(local.getUTCMinutes())) return false;
  if (!cron.hour.includes(local.getUTCHours())) return false;
  if (!cron.month.includes(local.getUTCMonth() + 1)) return false;
  const day = cron.day.includes(local.getUTCDate());
  const weekday = cron.weekday.includes(local.getUTCDay());
  // THE **OR** AGAIN, and it is the rule this whole file exists to make
  // visible: with both fields restricted a fire needs only one of them.
  if (!cron.anyDay && !cron.anyWeekday) return day || weekday;
  return day && weekday;
}
