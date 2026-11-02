/**
 * Moving between rooms by key: through the rooms in the order the reader sees
 * them, wrapping at either end, and through only the unread ones with Shift.
 */

import { describe, expect, test } from "vitest";

import type { ChatChannelSummary } from "~/protocol/index.ts";

import { stepRoom } from "./shortcuts.ts";

function row(id: string, over: Partial<ChatChannelSummary> = {}): ChatChannelSummary {
  return {
    channel: { v: 1, id, kind: "public", name: id, created_at: "2026-01-01T00:00:00Z" },
    revision: 1,
    unread: 0,
    mentions: 0,
    ...over,
  } as ChatChannelSummary;
}

const rooms = [row("a"), row("b", { unread: 2 }), row("c"), row("d", { unread: 1 })];

describe("a step between rooms", () => {
  test("goes to the next and the previous room, wrapping at either end", () => {
    const every = { unread: false, measured: true };
    expect(stepRoom(rooms, "a", 1, every)).toBe("b");
    expect(stepRoom(rooms, "a", -1, every)).toBe("d");
    expect(stepRoom(rooms, "d", 1, every)).toBe("a");
  });

  test("from a view, Down is the first room and Up the last", () => {
    const every = { unread: false, measured: true };
    expect(stepRoom(rooms, "", 1, every)).toBe("a");
    expect(stepRoom(rooms, "", -1, every)).toBe("d");
  });

  test("with unread, steps only through unread rooms, counting the open one as a place to step from", () => {
    const unread = { unread: true, measured: true };
    expect(stepRoom(rooms, "a", 1, unread)).toBe("b");
    expect(stepRoom(rooms, "b", 1, unread)).toBe("d");
    expect(stepRoom(rooms, "d", 1, unread)).toBe("b");
  });

  test("a muted room's unread messages are no reason to stop there", () => {
    // Mutation: drop the mute from `unreadIn` and this goes red.
    const muted = [row("a"), row("b", { unread: 4, muted: true }), row("c", { unread: 1 })];
    expect(stepRoom(muted, "a", 1, { unread: true, measured: true })).toBe("c");
  });

  test("an archived room is never stepped to, and a lone room goes nowhere", () => {
    const archived = [
      row("a"),
      row("b", {
        channel: {
          v: 1,
          id: "b",
          kind: "public",
          name: "b",
          created_at: "2026-01-01T00:00:00Z",
          archived_at: "2026-02-01T00:00:00Z",
        },
      }),
      row("c"),
    ];
    expect(stepRoom(archived, "a", 1, { unread: false, measured: true })).toBe("c");
    expect(stepRoom([row("a")], "a", 1, { unread: false, measured: true })).toBeNull();
  });
});
