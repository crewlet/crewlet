/**
 * The company's calendar: the zone its clock is read in, the date an instant
 * falls on there, and a DAY FIELD (a task's due date and start date) drawn as
 * the calendar date it names.
 *
 * ITS OWN SMALL MODULE, apart from `lib/range.ts`, because a day field is
 * drawn in rows, cards and change sentences that every first screen holds,
 * and `range.ts` is the time-window vocabulary of the charts and logs: pulled
 * into the entry by a row's due date, its windows, buckets and kept ranges
 * rode along into the first download. `range.ts` reads the company's clock
 * through this module.
 */

import { useCallback, useContext, useSyncExternalStore } from "react";
import { fmtCivilDay, parseUTC } from "./format.ts";
import { dateTimeFormat, knownZone, type DateShape } from "./formatters.ts";
import { ClientContext } from "./store-hooks.ts";

/**
 * The zone this runtime reads the company's clock in: `zone` itself, or UTC
 * where it names none (the engine's own reading of an empty name,
 * `internal/period`'s `LoadZone`) or names one this runtime's `Intl` does
 * not know ([knownZone], whose module says how such a name reaches a page).
 *
 * ONE RULE for every reading of the company's clock, so a time and the day it
 * is listed under, or a window and the day it resets, are read on one clock
 * even where that clock is the fallback; and asked once a zone, so a list of
 * rows on a clock this runtime cannot read builds nothing a row.
 */
export function companyZone(zone: string | undefined): string {
  return knownZone(zone || "UTC") ?? "UTC";
}

/** `en-CA` writes a date in this shape as `YYYY-MM-DD`, which sorts as the date does. */
const YMD: DateShape = Object.freeze({ year: "numeric", month: "2-digit", day: "2-digit" });

/** A function reading the `YYYY-MM-DD` an instant falls on in `zone`. */
export function dateIn(zone: string | undefined): (at: number) => string {
  // `en-CA` writes a date as `YYYY-MM-DD`, which sorts as the date does.
  const format = dateTimeFormat("en-CA", companyZone(zone), YMD);
  return (at) => format.format(at);
}

/** The company date (`YYYY-MM-DD`) an instant falls on in `zone`. */
export function dayLabelIn(at: number, zone: string | undefined): string {
  return dateIn(zone)(at);
}

/**
 * The company date a DAY FIELD names (`YYYY-MM-DD`), or "" for no date: a
 * task's due date and start date, and every delta, filter and toast that
 * carries one.
 *
 * A DATE-ONLY VALUE IS A CALENDAR DATE, NEVER AN INSTANT. The engine stores a
 * day it was given as the company's own midnight (`internal/worklet/wake.go`
 * says why the instant is what travels) and leaves the day to the surface
 * that knows the zone; every due band, overdue mark and `due=` token it cuts
 * is cut on that same calendar. So the day is read on the COMPANY's clock:
 * read on the reader's, the company's midnight is the evening before for
 * every reader west of it (UTC included), and a due date picked as Nov 30
 * read Nov 29 in the rail, the list and the history beside an engine
 * standing that said "due today". A bare date (what a date input holds, and
 * what a save sends) is already that date and passes through.
 */
export function companyDayOf(value: string | null | undefined, zone: string | undefined): string {
  const text = (value ?? "").trim();
  if (!text) return "";
  if (/^\d{4}-\d{2}-\d{2}$/.test(text)) {
    const at = Date.parse(`${text}T12:00:00Z`);
    return Number.isFinite(at) && new Date(at).toISOString().startsWith(text) ? text : "";
  }
  const at = parseUTC(text);
  return at ? dayLabelIn(at.getTime(), zone) : "";
}

/**
 * A day field drawn as its company date ([companyDayOf]): in full, or, given
 * the reader's `now`, with the year dropped inside the company's current year
 * (the column form [fmtDateCompact] draws). The value as it came where it is
 * not a date, and the empty mark where there is none.
 */
export function fmtCompanyDay(
  value: string | null | undefined,
  zone: string | undefined,
  now?: number,
): string {
  const day = companyDayOf(value, zone);
  if (!day) return value?.trim() ? value : fmtCivilDay("");
  return fmtCivilDay(day, now === undefined ? undefined : dayLabelIn(now, zone).slice(0, 4));
}

/**
 * The company's zone as the engine names it (empty for UTC), for [companyDayOf].
 *
 * THE CONTEXT, NOT `useOrg`: a row, a cell or a rail drawn outside the shell
 * (a suite, a page outside the app) has no store to read, and reads the
 * engine's own empty zone, UTC, rather than throwing, as a mention drawn there
 * reads its words (`components/InlineToken.tsx`).
 */
export function useCompanyZone(): string {
  const store = useContext(ClientContext)?.store;
  const subscribe = useCallback(
    (fn: () => void) => (store ? store.subscribe(["org"], fn) : () => {}),
    [store],
  );
  return useSyncExternalStore(
    subscribe,
    () => store?.state.org?.timezone ?? "",
    () => "",
  );
}
