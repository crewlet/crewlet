/**
 * The one formatter cache, held to what its key says.
 *
 * What these protect: a formatter is built once per locale, zone and shape and
 * handed out after, a different zone or shape is a different formatter (never
 * one built for another zone), a frozen shape's key is read once and any
 * other's on every ask, a zone `Intl` cannot read keeps no formatter and is
 * asked about once, so a page on it builds nothing a row, and the one input no
 * key holds — the browser's language — drops every formatter when it changes.
 */

import { afterEach, expect, test, vi } from "vitest";
import {
  collator,
  dateTimeFormat,
  forgetFormatters,
  keptFormattersForTest,
  knownZone,
  numberFormat,
  type DateShape,
} from "./formatters.ts";
import {
  companyDateLabel,
  fmtDate,
  fmtDateCompact,
  fmtDateTime,
  fmtMinute,
  fmtTime,
  readerDay,
  toWall,
} from "./format.ts";
import { cellText, formatDay, formatNumber, scaleFormat } from "./artifacts/dashboard.ts";
import { nextFires } from "./cron.ts";
import { clockIn, companyDays, companyMidnight, dayLabelIn } from "./range.ts";
import { monthDay } from "./timeline.ts";
import { monthLabel, targetLabel } from "./work.ts";
import { budgetCaption } from "~/routes/home/model.ts";
import { shortWhen, snoozePresets } from "~/routes/inbox/model.ts";

afterEach(() => {
  forgetFormatters();
  vi.restoreAllMocks();
});

test("one shape in one zone is built once, and handed out after", () => {
  const built = vi.spyOn(Intl, "DateTimeFormat");
  const first = dateTimeFormat(undefined, "Europe/Berlin", { month: "short", day: "numeric" });
  const again = dateTimeFormat(undefined, "Europe/Berlin", { month: "short", day: "numeric" });
  expect(again).toBe(first);
  expect(built).toHaveBeenCalledTimes(1);
  // AND IT WRITES IN THE ZONE ITS KEY NAMES: Berlin's 00:30 on the 11th is
  // UTC's 23:30 on the 10th.
  expect(first.format(Date.parse("2026-03-10T23:30:00Z"))).toBe("Mar 11");
});

test("another zone, locale or shape is another formatter", () => {
  const shape = { month: "short", day: "numeric" } as const;
  const berlin = dateTimeFormat(undefined, "Europe/Berlin", shape);
  const utc = dateTimeFormat(undefined, "UTC", shape);
  expect(utc).not.toBe(berlin);
  expect(utc.format(Date.parse("2026-03-10T23:30:00Z"))).toBe("Mar 10");
  expect(dateTimeFormat("en-GB", "UTC", shape)).not.toBe(utc);
  expect(dateTimeFormat(undefined, "UTC", { ...shape, year: "numeric" })).not.toBe(utc);
  expect(keptFormattersForTest()).toBe(4);
});

test("a number shape is built once, and kept apart from the dates", () => {
  const built = vi.spyOn(Intl, "NumberFormat");
  const two = numberFormat(undefined, { maximumFractionDigits: 2 });
  expect(numberFormat(undefined, { maximumFractionDigits: 2 })).toBe(two);
  expect(numberFormat(undefined, { maximumFractionDigits: 1 })).not.toBe(two);
  expect(built).toHaveBeenCalledTimes(2);
  expect(two.format(1234.567)).toBe("1,234.57");
});

// A SORT ORDERS ITS WHOLE LIST WITH ONE COLLATOR, where `localeCompare` handed
// options built one a comparison.
test("a collation is built once, kept apart from the formatters, and orders a whole sort", () => {
  const built = vi.spyOn(Intl, "Collator");
  const order = { sensitivity: "base", numeric: true } as const;
  const names = ["item 10", "Item 9", "alpha", "Beta"];
  names.sort((a, b) => collator(undefined, order).compare(a, b));
  expect(names).toEqual(["alpha", "Beta", "Item 9", "item 10"]);
  expect(collator(undefined, { numeric: true })).not.toBe(collator(undefined, order));
  expect(built).toHaveBeenCalledTimes(2);
  expect(keptFormattersForTest()).toBe(2);
});

// A FROZEN SHAPE'S KEY IS READ ONCE. Deriving the key on every ask was most of
// what handing out a kept formatter cost, so a shape that cannot change is
// read once and kept by the object — every module spells its shapes frozen —
// while a literal built for the call is read on each ask, and still finds the
// formatter its shape names.
test("a frozen shape's key is read from it once, and any other's on every ask", () => {
  const shape: DateShape = Object.freeze({ month: "short", day: "numeric" });
  const digits: Intl.NumberFormatOptions = Object.freeze({ maximumFractionDigits: 2 });
  const order: Intl.CollatorOptions = Object.freeze({ numeric: true });
  const kept = dateTimeFormat(undefined, "UTC", shape);
  numberFormat(undefined, digits);
  collator(undefined, order);
  const read = vi.spyOn(JSON, "stringify");
  for (let n = 0; n < 100; n++) {
    expect(dateTimeFormat(undefined, "UTC", shape)).toBe(kept);
    numberFormat(undefined, digits);
    collator(undefined, order);
  }
  expect(read).not.toHaveBeenCalled();
  for (let n = 0; n < 100; n++) {
    expect(dateTimeFormat(undefined, "UTC", { month: "short", day: "numeric" })).toBe(kept);
  }
  expect(read).toHaveBeenCalledTimes(100);
});

// AND ONLY A FROZEN ONE: a shape that can change is read again on every ask,
// so one changed after an ask is the new shape it spells, never the formatter
// its old key named.
test("a shape that is not frozen is read on every ask, so a changed one is a new shape", () => {
  const shape: { month: "short"; day: "numeric"; year?: "numeric" } = {
    month: "short",
    day: "numeric",
  };
  const before = dateTimeFormat(undefined, "UTC", shape);
  shape.year = "numeric";
  const after = dateTimeFormat(undefined, "UTC", shape);
  expect(after).not.toBe(before);
  expect(after.format(Date.parse("2026-03-10T12:00:00Z"))).toBe("Mar 10, 2026");
});

// A ZONE OFF THE WIRE — a company's — can be one this runtime cannot read: the
// build throws as `Intl` does, and nothing is kept to be handed out later in
// its place. Such a zone reaches a formatter through knownZone, below.
test("a zone Intl cannot read throws, and keeps nothing", () => {
  expect(() => dateTimeFormat(undefined, "Not/AZone", { weekday: "short" })).toThrow(RangeError);
  expect(keptFormattersForTest()).toBe(0);
});

// WHETHER THIS RUNTIME READS A ZONE IS ASKED ONCE. A caller that caught the
// throw built a formatter and threw it away on every call, so a list on a
// clock this browser cannot read built one a row; the answer is kept instead,
// a zone it reads and one it refuses alike.
test("whether Intl reads a zone is asked once, and answered from then on", () => {
  const built = vi.spyOn(Intl, "DateTimeFormat");
  for (let n = 0; n < 100; n++) expect(knownZone("Not/AZone")).toBeNull();
  expect(built).toHaveBeenCalledTimes(1);
  for (let n = 0; n < 100; n++) expect(knownZone("Asia/Tokyo")).toBe("Asia/Tokyo");
  expect(built).toHaveBeenCalledTimes(2);
  // And the answers go with the formatters, so a case starts from none.
  forgetFormatters();
  expect(knownZone("Not/AZone")).toBeNull();
  expect(built).toHaveBeenCalledTimes(3);
});

// THE BROWSER'S LANGUAGE is what an undefined locale resolves from when the
// formatter is built, and no key holds it: a language change drops them all.
test("a change of the browser's language drops every kept formatter", () => {
  const before = dateTimeFormat(undefined, "UTC", { weekday: "short" });
  numberFormat(undefined, { maximumFractionDigits: 2 });
  expect(keptFormattersForTest()).toBe(2);
  window.dispatchEvent(new Event("languagechange"));
  expect(keptFormattersForTest()).toBe(0);
  expect(dateTimeFormat(undefined, "UTC", { weekday: "short" })).not.toBe(before);
});

/**
 * Counts every formatter built from here on: an `Intl.DateTimeFormat` or
 * `Intl.NumberFormat`, and a `Date` or number method that builds one on every
 * call it is given a locale or options.
 */
function countFormatterBuilds(): () => number {
  const spies = [
    vi.spyOn(Intl, "DateTimeFormat"),
    vi.spyOn(Intl, "NumberFormat"),
    vi.spyOn(Date.prototype, "toLocaleString"),
    vi.spyOn(Date.prototype, "toLocaleDateString"),
    vi.spyOn(Date.prototype, "toLocaleTimeString"),
    vi.spyOn(Number.prototype, "toLocaleString"),
  ];
  return () => spies.reduce((n, spy) => n + spy.mock.calls.length, 0);
}

/** One row of everything a screen of rows writes a date or a number with. */
function row(at: number, now: number): string[] {
  const iso = new Date(at).toISOString();
  const day = iso.slice(0, 10);
  const week = {
    period: "week" as const,
    window: "2026-W43",
    starts_at: iso,
    resets_at: iso,
    used: 10,
    limit: 100,
    state: "ok" as const,
  };
  return [
    fmtTime(iso),
    fmtDateTime(iso),
    fmtDate(iso),
    fmtMinute(iso),
    fmtDateCompact(iso, now),
    companyDateLabel(day),
    readerDay(at),
    toWall(at),
    shortWhen(iso, now),
    ...budgetCaption(week, "America/New_York"),
    monthDay(day),
    targetLabel(day),
    monthLabel(day.slice(0, 7)),
    clockIn(at, "Asia/Tokyo"),
    companyDays({ from: at, to: at + 86_400_000 }, "Asia/Tokyo").until,
    formatDay(at, now),
    cellText({ key: "at", type: "date" }, iso),
    cellText({ key: "on", type: "date" }, day),
    cellText({ key: "n", type: "number" }, at / 1e9),
    formatNumber(at / 1e12, "percent"),
    scaleFormat({})(at / 1e10),
  ];
}

// A PAGE OF ROWS BUILDS NO FORMATTER PER ROW. Each of these built one on every
// call — a `toLocaleString` with options, an `Intl.DateTimeFormat` built where
// it was used — and a list draws one or two a row on every render; reading the
// zone a date is written in built one more each time. The rows span both
// shapes a date can take (this year's and an older one's) and both of a
// relative time's (within a week and past it), so the first row of each shape
// builds what the page needs, and the hundred after build nothing — nor read
// a key: every shape a row asks with is frozen, so none is read again.
test("a page of rows builds each formatter once, not once a row", () => {
  const now = Date.parse("2026-10-20T12:00:00Z");
  // Five days apart, so a hundred rows reach back past the turn of the year.
  const at = (n: number) => now - (n + 1) * 5 * 86_400_000 - n * 3_600_000;
  // THE FIRST PAGE BUILDS: one row of each shape.
  row(at(0), now);
  row(at(99), now);
  const kept = keptFormattersForTest();
  const built = countFormatterBuilds();
  const read = vi.spyOn(JSON, "stringify");
  for (let n = 0; n < 100; n++) row(at(n), now);
  expect(built()).toBe(0);
  expect(read).not.toHaveBeenCalled();
  // AND THE CACHE HOLDS SHAPES, NOT ROWS: the hundred rows added nothing.
  expect(keptFormattersForTest()).toBe(kept);
  expect(kept).toBeLessThan(40);
});

/** A zone off the wire that this runtime does not know. */
const UNREADABLE = "Not/AZone";

/** One row of everything a screen reads on a company's or a schedule's clock. */
function rowOnItsClock(zone: string, at: number, now: number): unknown[] {
  const iso = new Date(at).toISOString();
  const week = {
    period: "week" as const,
    window: "2026-W43",
    starts_at: iso,
    resets_at: iso,
    used: 10,
    limit: 100,
    state: "ok" as const,
  };
  return [
    clockIn(at, zone),
    dayLabelIn(at, zone),
    companyMidnight(at, zone),
    companyDays({ from: at, to: at + 86_400_000 }, zone).until,
    ...budgetCaption(week, zone),
    ...snoozePresets(now, zone, undefined).map((p) => p.until),
    nextFires("0 9 * * 1-5", new Date(at), zone),
  ];
}

// A CLOCK THIS RUNTIME CANNOT READ COSTS A LOOKUP A ROW, NOT A BUILD. Each
// reader of a company's or a schedule's zone caught the throw and fell back,
// so a list on a zone this browser does not know built a formatter, and threw
// it away, on every row of every render. Whether it reads the zone is asked
// once instead (knownZone), and the hundred rows after the first build nothing.
test("a page of rows on a clock this runtime cannot read builds nothing a row", () => {
  const now = Date.parse("2026-10-20T12:00:00Z");
  const at = (n: number) => now - n * 3_600_000;
  rowOnItsClock(UNREADABLE, at(0), now);
  const built = countFormatterBuilds();
  const read = vi.spyOn(JSON, "stringify");
  for (let n = 0; n < 100; n++) rowOnItsClock(UNREADABLE, at(n), now);
  expect(built()).toBe(0);
  expect(read).not.toHaveBeenCalled();
});
