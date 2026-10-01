// @vitest-environment node

/**
 * What it costs to write a date, and that the cheaper way writes the same one.
 *
 * Every formatter here called `toLocaleString(locale, options)`, which builds
 * an `Intl.DateTimeFormat` per call — the expensive half of formatting a date
 * — and asked the browser's zone by building another. A hundred-row audit
 * built three hundred formatters a render to produce the same handful over
 * and over. They go through one kept formatter per locale and options now
 * (`dateFormatter`), the numbers through one `Intl.NumberFormat` and the
 * grid's text order through one `Intl.Collator`.
 *
 * TWO CLAIMS, and both are needed. That the output is the output the
 * `toLocale*String` call gave — ECMA-402 defines those methods as exactly this
 * construction followed by `format`, and the first group holds it across zones,
 * date shapes and instants either side of a midnight and a new year. And that a
 * formatter is built once for a key rather than once a call, which is the
 * whole of the fix and the thing no output comparison can see.
 */

import { afterEach, describe, expect, test, vi } from "vitest";

import {
  FORMATTERS_KEPT,
  dateFormatter,
  fmtCount,
  fmtDate,
  fmtDateCompactIn,
  fmtDateTime,
  fmtExact,
  fmtMinute,
  fmtTime,
  naturalCompare,
  plural,
} from "./format.ts";
import { setDateFormat, setZone, type DateFormat } from "./prefs.ts";

afterEach(() => {
  vi.restoreAllMocks();
  setZone("");
  setDateFormat("auto");
});

const ZONES = ["UTC", "Asia/Tokyo", "America/Los_Angeles", "Pacific/Auckland"];
const SHAPES: DateFormat[] = ["auto", "iso", "long"];
/** Either side of a midnight and of a new year in most of the zones above. */
const INSTANTS = [
  "2031-04-16T12:34:56Z",
  "2030-12-31T23:59:59Z",
  "2031-01-01T00:00:00Z",
  "2031-06-30T11:00:00Z",
  "2031-03-09T10:30:00Z",
];

/** The shape `dateParts` builds for each choice — the oracle's half of it. */
function parts(shape: DateFormat): Intl.DateTimeFormatOptions {
  if (shape === "iso") return { year: "numeric", month: "2-digit", day: "2-digit" };
  if (shape === "long") return { year: "numeric", month: "long", day: "numeric" };
  return { year: "numeric", month: "short", day: "2-digit" };
}

describe("a kept formatter writes what toLocaleString wrote", () => {
  for (const zone of ZONES) {
    for (const shape of SHAPES) {
      test(`${zone}, ${shape}`, () => {
        setZone(zone);
        setDateFormat(shape);
        const locale = shape === "iso" ? "en-CA" : undefined;
        for (const ts of INSTANTS) {
          const d = new Date(ts);
          const clock = { hour: "2-digit", minute: "2-digit", hour12: false } as const;
          expect(fmtDateTime(ts)).toBe(
            d.toLocaleString(locale, {
              ...parts(shape),
              ...clock,
              second: "2-digit",
              timeZone: zone,
            }),
          );
          expect(fmtMinute(ts)).toBe(
            d.toLocaleString(locale, { ...parts(shape), ...clock, timeZone: zone }),
          );
          expect(fmtDate(ts)).toBe(
            d.toLocaleDateString(locale, { ...parts(shape), timeZone: zone }),
          );
          expect(fmtTime(ts)).toBe(
            d.toLocaleTimeString(undefined, { ...clock, second: "2-digit", timeZone: zone }),
          );
          // BOTH BRANCHES of the compact date: this year's drops the year.
          const short = { month: "short", day: "2-digit", timeZone: zone } as const;
          const year = d.toLocaleDateString("en-US", { year: "numeric", timeZone: zone });
          expect(fmtDateCompactIn(ts, year)).toBe(d.toLocaleDateString(undefined, short));
          expect(fmtDateCompactIn(ts, "1999")).toBe(
            d.toLocaleDateString(undefined, { ...short, year: "numeric" }),
          );
        }
      });
    }
  }

  test("a number is grouped as toLocaleString grouped it", () => {
    for (const n of [0, 7, 999, 1_234, 9_999, -4_321, 12.5, 1_234_567.891, 0.125]) {
      expect(fmtExact(n)).toBe(n.toLocaleString());
      expect(plural(n, "row")).toBe(`${n.toLocaleString()} ${n === 1 ? "row" : "rows"}`);
    }
    expect(fmtCount(9_999)).toBe((9_999).toLocaleString());
  });

  test("toLocaleString()'s own defaults, spelled out, are toLocaleString()", () => {
    // `ui/charts.tsx` writes an axis edge this way rather than through
    // `toLocaleString()`, and this is the equivalence it rests on.
    for (const ts of INSTANTS) {
      const d = new Date(ts);
      expect(
        dateFormatter(undefined, {
          year: "numeric",
          month: "numeric",
          day: "numeric",
          hour: "numeric",
          minute: "numeric",
          second: "numeric",
        }).format(d),
      ).toBe(d.toLocaleString());
    }
  });

  test("the grid's text order is localeCompare's", () => {
    const words = ["seat-10", "seat-2", "Seat-2", "éclair", "eclair", "zeta", "Alpha", "alpha", ""];
    for (const a of words) {
      for (const b of words) {
        expect(Math.sign(naturalCompare(a, b))).toBe(
          Math.sign(a.localeCompare(b, undefined, { numeric: true, sensitivity: "base" })),
        );
      }
    }
  });
});

describe("a formatter is built once for its key, not once a call", () => {
  test("a hundred dates in one zone and shape build one formatter", () => {
    // A ZONE NO OTHER CASE USES, so the first call here is the one that builds.
    setZone("Asia/Kolkata");
    const built = vi.spyOn(Intl, "DateTimeFormat");
    for (let i = 0; i < 100; i++) {
      fmtDateTime(new Date(Date.UTC(2031, 3, 16, 0, i)).toISOString());
    }
    expect(built).toHaveBeenCalledTimes(1);
    // AND A NEW CHOICE IS A NEW KEY rather than an old formatter answering
    // for it: the reader's zone is one of the options.
    // (Choosing a zone builds one of its own, to ask `Intl` whether the zone
    // exists, so the count is taken after the choice.)
    setZone("Europe/Lisbon");
    const chosen = built.mock.calls.length;
    fmtDateTime("2031-04-16T12:00:00Z");
    fmtDateTime("2031-04-16T13:00:00Z");
    expect(built.mock.calls.length - chosen).toBe(1);
  });

  test("the bound evicts the formatter nothing has asked for longest", () => {
    const zones = Intl.supportedValuesOf("timeZone").slice(0, FORMATTERS_KEPT + 1);
    const ask = (tz: string) => dateFormatter("en-GB", { timeZone: tz, hour: "numeric" });
    // FILL THE CACHE with keys no other case builds, then touch the first.
    for (const tz of zones.slice(0, FORMATTERS_KEPT)) ask(tz);
    ask(zones[0]!);
    const built = vi.spyOn(Intl, "DateTimeFormat");
    // ONE PAST THE BOUND: the second key is the least recently used now.
    ask(zones[FORMATTERS_KEPT]!);
    expect(built).toHaveBeenCalledTimes(1);
    ask(zones[0]!);
    expect(built).toHaveBeenCalledTimes(1);
    ask(zones[1]!);
    expect(built).toHaveBeenCalledTimes(2);
  });

  test("the number formatter and the collator are built once", () => {
    // BUILT BY THE FIRST USE, wherever in the run that was.
    fmtExact(1);
    naturalCompare("a", "b");
    const numbers = vi.spyOn(Intl, "NumberFormat");
    const collators = vi.spyOn(Intl, "Collator");
    for (let i = 0; i < 100; i++) {
      fmtExact(i * 1_000);
      plural(i, "row");
      naturalCompare(`seat-${i}`, `seat-${i + 1}`);
    }
    expect(numbers).not.toHaveBeenCalled();
    expect(collators).not.toHaveBeenCalled();
  });
});
