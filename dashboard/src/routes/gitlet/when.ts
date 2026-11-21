/** The windows a Gitlet history is narrowed to by time (`when=`), on the reader's days. */

import { civilAt, civilKey } from "~/lib/format.ts";
import { zone } from "~/lib/prefs.ts";
import { dayStartIn, edgeAt, type ClockEdge } from "~/lib/range.ts";
import { pushDayBounds } from "./activity.tsx";

/** Each window: its address value, its label, how many of the reader's days, its words. */
export const WHEN = [
  { value: "", label: "Any time", days: 0, words: "" },
  { value: "today", label: "Today", days: 1, words: "today" },
  { value: "7d", label: "Last 7 days", days: 7, words: "in the last 7 days" },
  { value: "30d", label: "Last 30 days", days: 30, words: "in the last 30 days" },
  { value: "90d", label: "Last 90 days", days: 90, words: "in the last 90 days" },
] as const;

/** The most days one read counts by day: 32 bounds, the engine's bound. */
export const COUNTED_DAYS = 31;

const DAY_MS = 86_400_000;

/** The first instant of the reader's day `days - 1` days before `today`, RFC 3339; none for any time. */
export function sinceOf(when: string, today: string): ClockEdge | undefined {
  const days = WHEN.find((w) => w.value === when)?.days ?? 0;
  return days === 0 ? undefined : dayStartBack(today, days - 1);
}

/** The first instant of the reader's day `days` days before `today`, RFC 3339. */
export function dayStartBack(today: string, days: number): ClockEdge | undefined {
  const todayAt = civilAt(today);
  if (todayAt === null) return undefined;
  const start = dayStartIn(civilKey(todayAt - days * DAY_MS), zone());
  return start === null ? undefined : edgeAt(start);
}

/**
 * The reader's midnights a window's pushes are counted by day at: its days, at most COUNTED_DAYS,
 * ending with tomorrow's midnight, so every bound lies inside the window.
 */
export function dayBoundsFor(when: string, today: string, inZone: string = zone()): ClockEdge[] {
  const days = WHEN.find((w) => w.value === when)?.days ?? 0;
  return pushDayBounds(today, days === 0 ? COUNTED_DAYS : Math.min(days, COUNTED_DAYS), inZone);
}
