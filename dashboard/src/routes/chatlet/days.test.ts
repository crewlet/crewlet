import { describe, expect, test } from "vitest";

import type { ChatMessageView } from "~/protocol/index.ts";

import { packPosition } from "./cursor.ts";
import {
  continues,
  dayLabel,
  listDate,
  dayOf,
  daysBefore,
  daysBetween,
  daysInWindow,
  isUnread,
  layoutDays,
  placedHeights,
  RUN_MS,
} from "./days.ts";

/** One message as a page carries it; `seq` is also its log position. */
function view(
  seq: number,
  at: string,
  over: Partial<ChatMessageView["message"]> = {},
): ChatMessageView {
  return {
    message: {
      v: 1,
      id: `m-${seq}`,
      channel_id: "room-1",
      author: "bo",
      author_kind: "agent",
      body: `line ${seq}`,
      created_at: at,
      ...over,
    },
    channel_seq: seq,
    position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq },
  };
}

describe("a day is the reader's civil day", () => {
  test("a message late in the evening UTC files under the next day where the reader lives", () => {
    // The suite runs in Europe/Berlin (vitest.config.ts): 23:30Z in July is
    // half past one the next morning there.
    expect(dayOf("2026-07-29T23:30:00Z")).toBe("2026-07-30");
  });

  test("days count by the calendar, across a change of the clocks", () => {
    expect(daysBetween("2026-03-28", "2026-03-30")).toBe(2);
    expect(daysBefore("2026-03-30", 7)).toBe("2026-03-23");
  });

  test("a day is named as a reader says it, with its year once it is not this year's", () => {
    expect(dayLabel("2026-10-30", "2026-10-30")).toBe("Today");
    expect(dayLabel("2026-10-29", "2026-10-30")).toBe("Yesterday");
    expect(dayLabel("2026-07-29", "2026-10-30")).toBe("Wednesday, July 29th");
    expect(dayLabel("2025-12-21", "2026-10-30")).toBe("Sunday, December 21st, 2025");
    expect(dayLabel("2026-10-12", "2026-10-30")).toBe("Monday, October 12th");
  });
});

describe("a run of speech", () => {
  test("the same author inside the run window goes on without a face", () => {
    const a = view(1, "2026-10-30T09:00:00Z");
    const b = view(2, "2026-10-30T09:04:00Z");
    expect(continues(a, b)).toBe(true);
  });

  test("ends at the window, at another author, after a tombstone and around a room event", () => {
    const a = view(1, "2026-10-30T09:00:00Z");
    expect(
      continues(a, view(2, new Date(Date.parse(a.message.created_at) + RUN_MS).toISOString())),
    ).toBe(false);
    expect(continues(a, view(2, "2026-10-30T09:01:00Z", { author: "ada" }))).toBe(false);
    expect(
      continues(
        view(1, "2026-10-30T09:00:00Z", { deleted_at: "2026-10-30T09:00:30Z" }),
        view(2, "2026-10-30T09:01:00Z"),
      ),
    ).toBe(false);
    const event = view(1, "2026-10-30T09:00:00Z", { event: { kind: "joined", handles: ["bo"] } });
    expect(continues(event, view(2, "2026-10-30T09:01:00Z"))).toBe(false);
  });

  test("the engine's line stored under a seat's name is another speaker", () => {
    // THE KIND TELLS THEM APART (speaker.ts): a `system` line is the
    // engine's, so it starts its own run, under its own face, beside a
    // seat's line stored under the same name.
    //
    // Mutation: compare the stored names alone, and the engine's line goes on
    // the seat's run without a face.
    const seat = view(1, "2026-10-30T09:00:00Z");
    const engine = view(2, "2026-10-30T09:01:00Z", { author_kind: "system" });
    expect(continues(seat, engine)).toBe(false);
    expect(continues(engine, view(3, "2026-10-30T09:02:00Z", { author_kind: "system" }))).toBe(
      true,
    );
  });
});

describe("the New line", () => {
  test("stands above the first message somebody else said after the reader's place", () => {
    const since = packPosition(view(2, "2026-10-30T09:00:00Z").position);
    expect(isUnread(view(2, "2026-10-30T09:00:00Z"), since, "ada")).toBe(false);
    expect(isUnread(view(3, "2026-10-30T09:00:00Z"), since, "ada")).toBe(true);
    // The reader's own words and a tombstone ask nothing of them.
    expect(isUnread(view(3, "2026-10-30T09:00:00Z", { author: "ada" }), since, "ada")).toBe(false);
    expect(
      isUnread(
        view(3, "2026-10-30T09:00:00Z", { deleted_at: "2026-10-30T09:01:00Z" }),
        since,
        "ada",
      ),
    ).toBe(false);
    // The engine's line is never the reader's own, whatever name it is
    // stored under.
    expect(
      isUnread(
        view(3, "2026-10-30T09:00:00Z", { author: "ada", author_kind: "system" }),
        since,
        "ada",
      ),
    ).toBe(true);
    // No place (a room the reader is not in) is no line at all.
    expect(isUnread(view(3, "2026-10-30T09:00:00Z"), null, "ada")).toBe(false);
  });

  test("is the day's own rule when the day opens on it, and a line inside the day otherwise", () => {
    const rows = [
      view(1, "2026-10-29T09:00:00Z"),
      view(2, "2026-10-30T09:00:00Z"),
      view(3, "2026-10-30T09:01:00Z"),
    ];
    const onRule = layoutDays(rows, "2026-10-30", "m-2");
    expect(onRule.map((d) => [d.label, d.newOnRule])).toEqual([
      ["Yesterday", false],
      ["Today", true],
    ]);
    expect(onRule[1]!.rows.some((r) => r.newAbove)).toBe(false);

    const inside = layoutDays(rows, "2026-10-30", "m-3");
    expect(inside[1]!.newOnRule).toBe(false);
    expect(inside[1]!.rows.map((r) => [r.view.message.id, r.newAbove])).toEqual([
      ["m-2", false],
      ["m-3", true],
    ]);
  });

  test("breaks a run of speech, so the message under it says who said it", () => {
    const rows = [view(1, "2026-10-30T09:00:00Z"), view(2, "2026-10-30T09:01:00Z")];
    expect(layoutDays(rows, "2026-10-30", "")[0]!.rows[1]!.continuation).toBe(true);
    expect(layoutDays(rows, "2026-10-30", "m-2")[0]!.rows[1]!.continuation).toBe(false);
  });

  test("never runs a continuation across a day", () => {
    const rows = [view(1, "2026-10-29T22:58:00Z"), view(2, "2026-10-29T23:01:00Z")];
    const days = layoutDays(rows, "2026-10-30", "");
    expect(days).toHaveLength(2);
    expect(days[1]!.rows[0]!.continuation).toBe(false);
  });
});

describe("when a list row was last spoken in", () => {
  test("the time today, Yesterday, then the date long and short, with its year once it is not this year's", () => {
    const today = "2026-11-17";
    // 09:00Z is 10:00 in Europe/Berlin, where the suite runs.
    expect(listDate("2026-11-17T09:00:00Z", today)).toEqual({ long: "10:00", short: "10:00" });
    expect(listDate("2026-11-16T09:00:00Z", today)).toEqual({
      long: "Yesterday",
      short: "Yesterday",
    });
    expect(listDate("2026-11-09T09:00:00Z", today)).toEqual({ long: "November 9", short: "Nov 9" });
    expect(listDate("2025-12-21T09:00:00Z", today)).toEqual({
      long: "December 21, 2025",
      short: "Dec 21, 2025",
    });
    expect(listDate("", today)).toEqual({ long: "", short: "" });
  });
});

// WHAT IS NOT DRAWN IS STOOD FOR AT ITS DRAWN HEIGHT. Three days of three rows,
// each row as tall as its index plus ten, under a 40px label and rule.
describe("the days a window draws", () => {
  const days = layoutDays(
    [
      view(1, "2026-01-01T09:00:00Z"),
      view(2, "2026-01-01T10:00:00Z"),
      view(3, "2026-01-01T11:00:00Z"),
      view(4, "2026-01-02T09:00:00Z"),
      view(5, "2026-01-02T10:00:00Z"),
      view(6, "2026-01-02T11:00:00Z"),
      view(7, "2026-01-03T09:00:00Z"),
      view(8, "2026-01-03T10:00:00Z"),
      view(9, "2026-01-03T11:00:00Z"),
    ],
    "2026-01-05",
    "",
  );
  const height = (i: number) => 10 + i;
  const CHROME = 40;

  test("places a day's label and rule above its first row, and nowhere else", () => {
    const placed = placedHeights(days, height, CHROME);
    expect([0, 1, 2, 3, 4, 5, 6, 7, 8].map(placed)).toEqual([50, 11, 12, 53, 14, 15, 56, 17, 18]);
  });

  // Mutation: count a left-out day's rows alone, and the spacer is a rule short.
  test("stands for a whole day left out with its rows and its label and rule", () => {
    const laid = daysInWindow(days, 4, 5, height, CHROME);
    expect(laid.days.map((d) => d.day.key)).toEqual(["2026-01-02"]);
    expect(laid.padTop).toBe(CHROME + 10 + 11 + 12);
    expect(laid.padBottom).toBe(CHROME + 16 + 17 + 18);
    const [drawn] = laid.days;
    expect(drawn!.rows.map((r) => r.index)).toEqual([4]);
    expect(drawn!.above).toBe(13);
    expect(drawn!.below).toBe(15);
  });

  // The window's coordinates and the page's agree: whatever the window, the
  // space above its first drawn row is the same.
  test("puts every drawn row where the window placed it", () => {
    const placed = placedHeights(days, height, CHROME);
    for (let start = 0; start < 9; start++) {
      const laid = daysInWindow(days, start, 9, height, CHROME);
      const [first] = laid.days;
      // Above the first drawn row: the left-out days, the first drawn day's
      // label and rule, and its rows above the window.
      const drawnAbove = laid.padTop + CHROME + first!.above;
      let windowAbove = 0;
      for (let i = 0; i < start; i++) windowAbove += placed(i);
      const opensDay = first!.day.rows[0]!.index === start;
      expect(drawnAbove).toBe(windowAbove + (opensDay ? CHROME : 0));
    }
  });
});
