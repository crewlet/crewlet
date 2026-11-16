// lib/ago.ts: a relative time in words, on the reader's calendar. The suite runs in Europe/Berlin.
import { afterEach, describe, expect, test, vi } from "vitest";
import { EMPTY_VALUE } from "@crewlethq/ui";
import { ago } from "./ago.ts";
import { relTime } from "./format.ts";
import { pinZoneForTest } from "./prefs.ts";

// 13:00 in Berlin on Monday, November 16, 2026.
const NOW = Date.parse("2026-11-16T12:00:00Z");
const back = (secs: number) => new Date(NOW - secs * 1000).toISOString();
const ahead = (secs: number) => new Date(NOW + secs * 1000).toISOString();

afterEach(() => {
  pinZoneForTest("");
  vi.useRealTimers();
});

describe("ago", () => {
  test("says each past form", () => {
    expect(ago(back(0), NOW)).toBe("just now");
    expect(ago(back(59), NOW)).toBe("just now");
    expect(ago(ahead(4), NOW)).toBe("just now");
    expect(ago(back(60), NOW)).toBe("1 minute ago");
    expect(ago(back(42 * 60), NOW)).toBe("42 minutes ago");
    expect(ago(back(3600), NOW)).toBe("1 hour ago");
    expect(ago(back(3 * 3600), NOW)).toBe("3 hours ago");
    expect(ago("2026-11-15T16:40:00Z", NOW)).toBe("yesterday");
    expect(ago("2026-11-15T16:40:00Z", NOW, { clock: true })).toBe("yesterday at 17:40");
    expect(ago("2026-11-14T12:00:00Z", NOW)).toBe("2 days ago");
    expect(ago("2026-10-18T12:00:00Z", NOW)).toBe("29 days ago");
    expect(ago("2026-10-17T12:00:00Z", NOW)).toBe("on Oct 17");
    expect(ago("2025-10-02T12:00:00Z", NOW)).toBe("on Oct 02, 2025");
  });

  test("says each future form", () => {
    expect(ago(ahead(30), NOW)).toBe("in less than a minute");
    expect(ago(ahead(5 * 60), NOW)).toBe("in 5 minutes");
    expect(ago(ahead(4 * 60 + 40), NOW)).toBe("in 5 minutes");
    expect(ago(ahead(3 * 3600), NOW)).toBe("in 3 hours");
    expect(ago("2026-11-17T08:00:00Z", NOW)).toBe("tomorrow");
    expect(ago("2026-11-17T08:00:00Z", NOW, { clock: true })).toBe("tomorrow at 09:00");
    expect(ago("2026-11-20T12:00:00Z", NOW)).toBe("in 4 days");
    // Exactly 30 calendar days ahead is past the cut-off.
    expect(ago("2026-12-16T12:00:00Z", NOW)).toBe("on Dec 16");
    expect(ago("2026-12-20T12:00:00Z", NOW)).toBe("on Dec 20");
  });

  test("reads after since", () => {
    const since = { since: true };
    expect(ago(back(30), NOW, since)).toBe("a moment ago");
    expect(ago(ahead(4), NOW, since)).toBe("a moment ago");
    expect(ago(ahead(3 * 60), NOW, since)).toBe("a moment ago");
    expect(ago(back(3 * 3600), NOW, since)).toBe("3 hours ago");
    expect(ago("2026-11-15T16:40:00Z", NOW, { since: true, clock: true })).toBe(
      "yesterday at 17:40",
    );
    expect(ago("2026-10-17T12:00:00Z", NOW, since)).toBe("Oct 17");
    expect(ago("2025-10-02T12:00:00Z", NOW, since)).toBe("Oct 02, 2025");
  });

  test("leaves every other form alone after since", () => {
    for (const ts of [back(60), back(3 * 3600), "2026-11-15T16:40:00Z", "2026-11-11T12:00:00Z"]) {
      expect(ago(ts, NOW, { since: true })).toBe(ago(ts, NOW));
    }
  });

  test("reads due once a due time has arrived", () => {
    const due = { due: true };
    expect(ago(back(0), NOW, due)).toBe("due");
    expect(ago(back(3 * 60), NOW, due)).toBe("due");
    expect(ago(ahead(3), NOW, due)).toBe("in less than a minute");
    expect(ago(ahead(30), NOW, due)).toBe("in less than a minute");
    expect(ago(ahead(4 * 60), NOW, due)).toBe("in 4 minutes");
  });

  test("keeps minutes inside the first hour and the calendar after it, across midnight", () => {
    // 00:10 on the 16th in Berlin.
    const now = Date.parse("2026-11-15T23:10:00Z");
    expect(ago("2026-11-15T22:55:00Z", now)).toBe("15 minutes ago");
    expect(ago("2026-11-15T21:30:00Z", now)).toBe("yesterday");
    expect(ago("2026-11-15T21:30:00Z", now, { clock: true })).toBe("yesterday at 22:30");
    expect(ago("2026-11-14T22:59:00Z", now)).toBe("2 days ago");
  });

  test("counts calendar days across the 23-hour spring day", () => {
    // 00:30 on March 30 against 23:50 on March 28, 23 hours 40 minutes apart.
    const now = Date.parse("2026-03-29T22:30:00Z");
    expect(ago("2026-03-28T22:50:00Z", now)).toBe("2 days ago");
    expect(relTime("2026-03-28T22:50:00Z", now)).toBe("23h ago");
  });

  test("counts hours within the 25-hour autumn day", () => {
    // 23:30 on October 25 against 00:10 the same day.
    expect(ago("2026-10-24T22:10:00Z", Date.parse("2026-10-25T22:30:00Z"))).toBe("24 hours ago");
  });

  test("reads yesterday across a midnight that does not exist", () => {
    // Santiago skips 00:00 to 01:00 on September 6, 2026.
    pinZoneForTest("America/Santiago");
    const now = Date.parse("2026-09-06T04:30:00Z");
    expect(ago("2026-09-06T03:30:00Z", now, { clock: true })).toBe("yesterday at 23:30");
  });

  test("depends on now and the zone only, never the machine clock", () => {
    const ts = "2026-11-15T16:00:00Z";
    const now = Date.parse("2026-11-16T08:00:00Z");
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2027-11-16T08:00:00Z"));
    expect(ago(ts, now)).toBe("yesterday");
    pinZoneForTest("Asia/Tokyo");
    expect(ago(ts, now)).toBe("16 hours ago");
  });

  test("draws the empty value for an unset time", () => {
    expect(ago(undefined, NOW)).toBe(EMPTY_VALUE);
    expect(ago("", NOW)).toBe(EMPTY_VALUE);
    expect(ago("0001-01-01T00:00:00Z", NOW)).toBe(EMPTY_VALUE);
  });

  test("writes the time after midnight as 00, never 24", () => {
    expect(ago("2026-11-14T23:15:00Z", NOW, { clock: true })).toBe("yesterday at 00:15");
  });
});
