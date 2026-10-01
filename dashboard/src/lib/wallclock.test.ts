/**
 * The wall clock a range picker types in, and the instant it names.
 *
 * NOT `@vitest-environment node`, unlike the rest of `format.test.ts`: the
 * conversion reads the VIEWER'S chosen zone out of `lib/prefs.ts`, which is
 * browser storage, and the whole hazard here is that the browser's own zone
 * and the chosen one differ. The suite itself runs in `Europe/Berlin` (see
 * `vitest.config.ts`), so every case that chooses another zone is also a case
 * where the naive `new Date(value)` answer is wrong.
 */

import { beforeEach, describe, expect, it } from "vitest";
import { fromWall, readerDay, toWall } from "./format.ts";
import { dayOf, daysBetween, shiftDay } from "./timeline.ts";
import { calendarWeeks, dayKey, dayLabel, gridRange } from "./work.ts";
import { reloadForTest, setZone } from "./prefs.ts";

function inZone(zone: string) {
  setZone(zone);
  reloadForTest();
}

beforeEach(() => {
  localStorage.clear();
  reloadForTest();
});

describe("a wall-clock reading and the instant it names", () => {
  it("renders an instant in the CHOSEN zone, not the browser's", () => {
    // The browser is in Europe/Berlin. A reader whose company runs in Tokyo
    // set the preference, and the input has to show them Tokyo's clock —
    // `toISOString().slice(0, 16)` would show them UTC and
    // `new Date(...).toISOString()` after a local parse would show Berlin.
    const at = Date.parse("2026-06-15T00:30:00Z");
    inZone("Asia/Tokyo");
    expect(toWall(at)).toBe("2026-06-15T09:30");
    inZone("America/New_York");
    expect(toWall(at)).toBe("2026-06-14T20:30");
    inZone("UTC");
    expect(toWall(at)).toBe("2026-06-15T00:30");
  });

  it("reads a typed reading as that zone's clock", () => {
    // The other direction, and the one that decides what window the engine is
    // asked for: 09:00 typed by a reader in Tokyo is midnight UTC, not 07:00
    // UTC — which is what the browser's own zone would have made it.
    inZone("Asia/Tokyo");
    expect(fromWall("2026-06-15T09:00")).toBe(Date.parse("2026-06-15T00:00:00Z"));
    inZone("America/New_York");
    expect(fromWall("2026-06-15T09:00")).toBe(Date.parse("2026-06-15T13:00:00Z"));
    inZone("UTC");
    expect(fromWall("2026-06-15T09:00")).toBe(Date.parse("2026-06-15T09:00:00Z"));
  });

  it("round-trips every instant it renders", () => {
    // The two are used as a pair — the picker renders the window it was given
    // and reads back what the reader left alone — so a reader who opens the
    // dialog and presses Apply must get the window they already had.
    for (const zone of ["UTC", "Asia/Tokyo", "America/New_York", "Australia/Lord_Howe"]) {
      inZone(zone);
      for (const iso of [
        "2026-06-15T12:34:00Z",
        "2026-01-01T00:00:00Z",
        "2026-12-31T23:59:00Z",
        "2026-03-29T00:30:00Z",
      ]) {
        const at = Date.parse(iso);
        expect(fromWall(toWall(at)), `${zone} ${iso}`).toBe(at);
      }
    }
  });

  it("corrects across a DST change rather than landing an hour out", () => {
    // THE SECOND PASS IS WHAT THIS BUYS. New York moves its clocks forward at
    // 02:00 on 8 March 2026. A single-pass conversion reads the wall time as
    // UTC, subtracts the offset AT THAT GUESS — which is still the winter
    // offset for a wall time in the afternoon — and lands an hour out on
    // every reading for the rest of the summer.
    inZone("America/New_York");
    expect(fromWall("2026-03-07T12:00")).toBe(Date.parse("2026-03-07T17:00:00Z"));
    expect(fromWall("2026-03-09T12:00")).toBe(Date.parse("2026-03-09T16:00:00Z"));
    // And the boundary itself, in both directions.
    expect(fromWall("2026-03-08T01:30")).toBe(Date.parse("2026-03-08T06:30:00Z"));
    expect(fromWall("2026-03-08T03:30")).toBe(Date.parse("2026-03-08T07:30:00Z"));
  });

  it("names no instant for a reading that is not one", () => {
    // An empty field is not midnight in 1970, and a half-typed date is not a
    // window: the dialog says so rather than applying a window nobody chose.
    inZone("UTC");
    for (const bad of ["", "   ", "2026-06-15", "tomorrow", "15/06/2026 09:00"]) {
      expect(fromWall(bad), bad).toBeNull();
    }
  });

  it("keeps midnight on its own day", () => {
    // `hour12: false` renders midnight as 24 in some runtimes, which is the
    // previous day's twenty-fourth hour — fed back unfolded it rolls the day
    // forward and lands a whole day out.
    inZone("Asia/Tokyo");
    const midnight = Date.parse("2026-06-14T15:00:00Z"); // 2026-06-15T00:00 in Tokyo
    expect(toWall(midnight)).toBe("2026-06-15T00:00");
    expect(fromWall("2026-06-15T00:00")).toBe(midnight);
  });
});

/**
 * A DAY is filed in the zone every timestamp is drawn in.
 *
 * The calendar's cells and the timeline's columns used to be keyed in the
 * browser's own calendar while every spelling followed the chosen zone, which
 * was harmless only while no control could choose one. Now one can, so a task
 * stamped in the evening UTC is on the NEXT day for a reader who chose Tokyo —
 * in its cell as well as in its label.
 */
describe("a day is the reader's day", () => {
  it("files an instant under the chosen zone's date, not the browser's", () => {
    const at = "2026-09-22T20:00:00Z";
    inZone("Asia/Tokyo");
    expect(readerDay(Date.parse(at))).toBe("2026-09-23");
    expect(dayKey(at)).toBe("2026-09-23");
    expect(dayOf(at)).toBe("2026-09-23");
    // The browser is in Berlin, where it is still the 22nd.
    inZone("");
    expect(dayKey(at)).toBe("2026-09-22");
    inZone("America/Los_Angeles");
    expect(dayKey("2026-09-23T03:00:00Z")).toBe("2026-09-22");
  });

  it("lays a grid out the same in every zone, because a day key is a civil date", () => {
    // Walked across Berlin's own DST change (25 October 2026), where a count
    // over local midnights meets a 25-hour day.
    for (const zone of ["", "Pacific/Kiritimati", "America/Los_Angeles", "UTC"]) {
      inZone(zone);
      expect(daysBetween("2026-10-24", "2026-10-26"), zone).toBe(2);
      expect(shiftDay("2026-10-24", 2), zone).toBe("2026-10-26");
      const weeks = calendarWeeks("2026-10", "");
      expect(weeks[0]![0]!.key, zone).toBe("2026-09-28");
      expect(gridRange(weeks), zone).toEqual({ from: "2026-09-28", to: "2026-11-02" });
      expect(dayLabel("2026-10-01"), zone).toContain("1");
      expect(dayLabel("2026-10-01"), zone).toMatch(/Thursday/);
    }
  });
});
