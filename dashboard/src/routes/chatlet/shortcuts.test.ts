/**
 * Moving between rooms by key: through the rooms in the order the reader sees
 * them, wrapping at either end, and through only the unread ones with Shift.
 */

import { describe, expect, test } from "vitest";

import { chatWaiting } from "~/lib/chatRooms.ts";
import type { ChatChannelsAnswer, ChatChannelSummary } from "~/protocol/index.ts";

import { markable, stepRoom } from "./shortcuts.ts";

/** The counts, measured: what waits on the reader, by the engine's places. */
function counted(waiting: ChatChannelsAnswer["waiting"] = []) {
  return chatWaiting({ read_state: true, channels: [], waiting, waiting_capped: false } as never);
}
const measured = counted();

function row(id: string, over: Partial<ChatChannelSummary> = {}): ChatChannelSummary {
  return {
    channel: { v: 1, id, kind: "public", name: id, created_at: "2026-01-01T00:00:00Z" },
    revision: 1,
    unread: 0,
    ...over,
  } as ChatChannelSummary;
}

const rooms = [row("a"), row("b", { unread: 2 }), row("c"), row("d", { unread: 1 })];

describe("a step between rooms", () => {
  test("goes to the next and the previous room, wrapping at either end", () => {
    const every = { unread: false, waiting: measured };
    expect(stepRoom(rooms, "a", 1, every)).toBe("b");
    expect(stepRoom(rooms, "a", -1, every)).toBe("d");
    expect(stepRoom(rooms, "d", 1, every)).toBe("a");
  });

  test("from a view, Down is the first room and Up the last", () => {
    const every = { unread: false, waiting: measured };
    expect(stepRoom(rooms, "", 1, every)).toBe("a");
    expect(stepRoom(rooms, "", -1, every)).toBe("d");
  });

  test("with unread, steps only through unread rooms, counting the open one as a place to step from", () => {
    const unread = { unread: true, waiting: measured };
    expect(stepRoom(rooms, "a", 1, unread)).toBe("b");
    expect(stepRoom(rooms, "b", 1, unread)).toBe("d");
    expect(stepRoom(rooms, "d", 1, unread)).toBe("b");
  });

  test("a room whose only unread is replies waiting in its threads is one to step to", () => {
    // Mutation: key the step on the room's unread posts alone and it skips c.
    const replies = counted([{ channel_id: "c", root_id: "t", count: 2, capped: false }]);
    expect(stepRoom(rooms, "b", 1, { unread: true, waiting: replies })).toBe("c");
  });

  test("a muted room's unread messages are no reason to stop there", () => {
    // Mutation: drop the mute from `unreadIn` and this goes red.
    const muted = [row("a"), row("b", { unread: 4, muted: true }), row("c", { unread: 1 })];
    expect(stepRoom(muted, "a", 1, { unread: true, waiting: measured })).toBe("c");
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
    expect(stepRoom(archived, "a", 1, { unread: false, waiting: measured })).toBe("c");
    expect(stepRoom([row("a")], "a", 1, { unread: false, waiting: measured })).toBeNull();
  });
});

describe("what Escape has to mark read", () => {
  test("a room with unread posts, muted or not", () => {
    expect(markable(row("a", { unread: 2 }), measured)).toBe(true);
    expect(markable(row("a", { unread: 2, muted: true }), measured)).toBe(true);
  });

  test("a room whose only unread is replies waiting in its threads", () => {
    const replies = counted([{ channel_id: "a", root_id: "t", count: 1, capped: false }]);
    expect(markable(row("a"), replies)).toBe(true);
  });

  test("nothing in a room the counts say is read, or one the rail does not list", () => {
    // Mutation: return true for a measured zero and this goes red.
    expect(markable(row("a"), measured)).toBe(false);
    expect(markable(undefined, measured)).toBe(false);
  });

  test("a room whose counts were not measured, since its zero says nothing", () => {
    expect(markable(row("a"), null)).toBe(true);
  });
});
