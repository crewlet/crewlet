import { describe, expect, test } from "vitest";

import type { ChatMessageView } from "~/protocol/index.ts";

import { packPosition } from "./cursor.ts";
import {
  continues,
  dayLabel,
  dayOf,
  daysBefore,
  daysBetween,
  isUnread,
  layoutDays,
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
