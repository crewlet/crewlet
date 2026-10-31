/**
 * A transcript laid out as it is read: by day, in runs of speech, with the New
 * line where the reader left off.
 *
 * PURE, AND APART FROM THE COMPONENT, for the reason `messages.ts` is: jsdom
 * draws no layout, and what has to be right here (which day a message falls
 * on in the reader's zone, where one run of speech ends, which row the New
 * line goes above) is arithmetic a suite can ask directly.
 *
 * A DAY IS THE READER'S CIVIL DAY, in the zone every timestamp in the
 * dashboard is drawn in (`readerDay`), so a message stamped late in the
 * evening is filed under the day its time says, never the browser's. Counting
 * days between two keys is calendar arithmetic over the keys (`civilAt`),
 * where no day is 23 or 25 hours long.
 */

import { civilAt, civilKey, fmtClock, parseUTC, readerDay } from "~/lib/format.ts";
import type { ChatMessageView } from "~/protocol/index.ts";

import { packPosition } from "./cursor.ts";

const DAY_MS = 86_400_000;

/** How long after a message the same author's next one still continues it. */
export const RUN_MS = 5 * 60_000;

/** The reader's civil day an engine timestamp falls on, `YYYY-MM-DD`, or "". */
export function dayOf(ts: string | null | undefined): string {
  const at = parseUTC(ts);
  return at ? readerDay(at) : "";
}

/**
 * A message's time as a row shows it ("10:21 AM", "10:21"), in the reader's
 * zone and their own way of writing a time. The day is the day's rule.
 */
export function timeOf(ts: string): string {
  return fmtClock(ts, Date.parse(ts));
}

/**
 * The time without its day period ("10:21"), for a gutter one face wide. The
 * full time is its title.
 */
export function clockOf(ts: string): string {
  return timeOf(ts).replace(/\s*[ap]\.?\s?m\.?$/i, "");
}

/**
 * Whether `view` goes on from `previous` as one run of speech: the same
 * author within [RUN_MS], the one before it still there to read, and neither
 * a room event. The row then repeats neither the face nor the name. One rule
 * for the room, a thread and a view that lists replies.
 */
export function continues(previous: ChatMessageView | undefined, view: ChatMessageView): boolean {
  return (
    previous !== undefined &&
    !previous.message.event &&
    !view.message.event &&
    !previous.message.deleted_at &&
    previous.message.author === view.message.author &&
    Date.parse(view.message.created_at) - Date.parse(previous.message.created_at) < RUN_MS
  );
}

/** Whole days from `from` to `to`, two civil keys. */
export function daysBetween(from: string, to: string): number {
  const a = civilAt(from);
  const b = civilAt(to);
  return a === null || b === null ? 0 : Math.round((b - a) / DAY_MS);
}

/** The civil key `days` before `key`. */
export function daysBefore(key: string, days: number): string {
  const at = civilAt(key);
  return at === null ? key : civilKey(at - days * DAY_MS);
}

// The day's name in the words the interface is written in: an ordinal suffix
// ("29th") is English, so the weekday and month beside it are English too.
// Formatted at UTC because the key is already the reader's civil day.
const WEEKDAY = new Intl.DateTimeFormat("en-US", { weekday: "long", timeZone: "UTC" });
const MONTH = new Intl.DateTimeFormat("en-US", { month: "long", timeZone: "UTC" });

/** 1st, 2nd, 3rd, 4th, 11th, 12th, 13th, 21st. */
function ordinal(n: number): string {
  const teen = n % 100 >= 11 && n % 100 <= 13;
  const suffix = teen ? "th" : ({ 1: "st", 2: "nd", 3: "rd" } as Record<number, string>)[n % 10];
  return `${n}${suffix ?? "th"}`;
}

/**
 * A day's name as a reader says it: "Today", "Yesterday", "Wednesday, July
 * 29th", and with its year once it is not this year's.
 */
export function dayLabel(key: string, today: string): string {
  const days = daysBetween(key, today);
  if (days === 0) return "Today";
  if (days === 1) return "Yesterday";
  const at = civilAt(key);
  if (at === null) return key;
  const date = new Date(at);
  const name = `${WEEKDAY.format(date)}, ${MONTH.format(date)} ${ordinal(date.getUTCDate())}`;
  return key.slice(0, 4) === today.slice(0, 4) ? name : `${name}, ${key.slice(0, 4)}`;
}

/**
 * Whether a message is one the reader has not read: after the point they had
 * read through when they came in, said by somebody else, and still there to be
 * read (a tombstone asks nothing of anybody).
 */
export function isUnread(view: ChatMessageView, since: number | null, viewer: string): boolean {
  if (since === null) return false;
  const at = packPosition(view.position);
  return at !== null && at > since && view.message.author !== viewer && !view.message.deleted_at;
}

/** One row of a day, with how it is drawn. */
export interface DayRow {
  view: ChatMessageView;
  /** Index in the transcript's oldest-first list, which the window counts in. */
  index: number;
  /**
   * The same author went on within [RUN_MS], with nothing between: the row
   * repeats neither the face nor the name. Never across a day, the New line or
   * a room event.
   */
  continuation: boolean;
  /** The New line stands above this row, inside its day. */
  newAbove: boolean;
}

/** One day of the transcript. */
export interface Day {
  key: string;
  label: string;
  /** The day opens on the first unread message: the New line is its rule. */
  newOnRule: boolean;
  rows: DayRow[];
}

/**
 * The transcript by day.
 *
 * `rows` is oldest first. `firstUnread` is the id of the message the New line
 * stands above, or "" for none; a day whose first row it is carries the line
 * on its own rule, as one line rather than a second one under it.
 */
export function layoutDays(
  rows: readonly ChatMessageView[],
  today: string,
  firstUnread: string,
): Day[] {
  const days: Day[] = [];
  let previous: ChatMessageView | undefined;
  rows.forEach((view, index) => {
    const key = dayOf(view.message.created_at);
    let day = days[days.length - 1];
    let broke = false;
    if (!day || day.key !== key) {
      day = { key, label: dayLabel(key, today), newOnRule: false, rows: [] };
      days.push(day);
      broke = true;
    }
    const isNew = view.message.id === firstUnread;
    if (isNew) {
      if (day.rows.length === 0) day.newOnRule = true;
      broke = true;
    }
    const continuation = !broke && continues(previous, view);
    day.rows.push({ view, index, continuation, newAbove: isNew && day.rows.length > 0 });
    previous = view;
  });
  return days;
}
