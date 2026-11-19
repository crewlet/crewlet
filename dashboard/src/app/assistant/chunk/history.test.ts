/**
 * The page's history, filed by day on the reader's calendar, and what a row
 * says of the turn in it.
 */

import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { grouped, groupOf, type HistoryRow } from "./history.ts";
import { rowState } from "./AskPage.tsx";
import { EMPTY, type ConversationView, type TurnView } from "./turn.ts";

const NOON = Date.parse("2026-11-19T12:00:00Z");
const DAY = 86_400_000;

beforeEach(() => {
  vi.stubEnv("TZ", "UTC");
});
afterEach(() => {
  vi.unstubAllEnvs();
});

const row = (id: string, ago: number, state?: string): HistoryRow =>
  ({
    id,
    title: id,
    created_at: new Date(NOON - ago).toISOString(),
    updated_at: new Date(NOON - ago).toISOString(),
    last_activity_at: new Date(NOON - ago).toISOString(),
    ...(state ? { last_turn_state: state } : {}),
  }) as HistoryRow;

// A DAY IS THE READER'S CALENDAR DAY, never a 24-hour window: a conversation
// used late yesterday evening is Yesterday at noon today, not Today.
test("a conversation is filed under the calendar day it was last used", () => {
  expect(groupOf(new Date(NOON - 60_000).toISOString(), NOON)).toBe("Today");
  expect(groupOf(new Date(NOON - 12.5 * 3_600_000).toISOString(), NOON)).toBe("Yesterday");
  expect(groupOf(new Date(NOON - 2 * DAY).toISOString(), NOON)).toBe("Previous 7 days");
  expect(groupOf(new Date(NOON - 7 * DAY).toISOString(), NOON)).toBe("Previous 7 days");
  expect(groupOf(new Date(NOON - 8 * DAY).toISOString(), NOON)).toBe("Older");
});

test("the groups keep the list's order and leave out a day with nothing in it", () => {
  const groups = grouped([row("a", 60_000), row("b", 3 * DAY), row("c", 4 * DAY)], NOON);
  expect(groups.map((g) => [g.group, g.rows.map((r) => r.id)])).toEqual([
    ["Today", ["a"]],
    ["Previous 7 days", ["b", "c"]],
  ]);
});

const turn = (phase: TurnView["phase"]) => ({ id: "t", phase }) as unknown as TurnView;
const viewOf = (id: string, turns: TurnView[]): ConversationView => ({ ...EMPTY, id, turns });

// THE OPEN CONVERSATION'S ROW IS READ OFF THE STREAM, which is newer than the
// list's last read; every other row off the list.
test("a row says a turn answers or waits on the reader, the open one from its stream", () => {
  const open = viewOf("a", [turn("working")]);
  expect(rowState(row("a", 0, "completed"), open)).toBe("Answering");
  expect(rowState(row("a", 0, "running"), viewOf("a", [turn("completed")]))).toBeNull();
  expect(rowState(row("a", 0), viewOf("a", [turn("awaiting_approval")]))).toBe("Waiting for you");
  expect(rowState(row("b", 0, "running"), open)).toBe("Answering");
  expect(rowState(row("b", 0, "waiting_for_approval"), open)).toBe("Waiting for you");
  expect(rowState(row("b", 0, "completed"), open)).toBeNull();
});
