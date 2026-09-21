/**
 * What a room is called, and what its badge says.
 *
 * Both are answered in five places — the rail, the room's own header, a search
 * result, the mention feed and the browser tab — and the cases here are the
 * ones where a second implementation goes wrong: a conversation with no name,
 * a count the engine stopped counting, and a room this build has never heard
 * of.
 */

import { describe, expect, test } from "vitest";

import { badgesAreMeasured, knownKind, roomMark, roomTitle, unreadLabel } from "./rooms.ts";
import type { ChatChannel } from "~/protocol/index.ts";

const channel = (over: Partial<ChatChannel> = {}): ChatChannel => ({
  v: 1,
  id: "room-1",
  kind: "public",
  name: "general",
  created_at: "2026-01-01T00:00:00Z",
  ...over,
});

describe("what a room is called", () => {
  test("a direct conversation is who is in it, and never the reader themself", () => {
    // A direct conversation HAS no name — its identity is derived from the
    // sorted handles of the people in it — so the title is built from the
    // participants everywhere it appears. Reading your own name as the name of
    // a conversation is the failure a second implementation always ships.
    const title = roomTitle(channel({ kind: "dm", name: "" }), {
      participants: ["ada", "bo"],
      viewer: "ada",
      nameOf: (handle) => (handle === "bo" ? "Bo Chen" : handle),
    });
    expect(title).toBe("Bo Chen");
  });

  test("a conversation with nobody else in it still has a title", () => {
    expect(
      roomTitle(channel({ kind: "dm", name: "" }), { participants: ["ada"], viewer: "ada" }),
    ).toBe("Just you");
  });

  test("a named room is its name, without the sigil", () => {
    // The `#` is drawn beside the glyph. Carried in the string it would reach
    // a page title and a browser tab as punctuation somebody typed.
    expect(roomTitle(channel())).toBe("general");
  });

  test("a room whose name has not arrived is addressed by its id rather than by nothing", () => {
    expect(roomTitle(channel({ name: "" }))).toBe("room-1");
  });
});

describe("a kind this build does not know", () => {
  test("is not classified, and not drawn as public", () => {
    // An open enum read two-valued falls out as "not private", which is how a
    // newer peer's room becomes writable by anyone. The mark says "unknown"
    // rather than borrowing the public one.
    expect(knownKind("broadcast")).toBe(false);
    expect(roomMark("broadcast")).toBe("help");
    expect(roomMark("private")).not.toBe(roomMark("public"));
  });
});

describe("the unread badge", () => {
  test("says 99+ only when the engine says it stopped counting", () => {
    expect(unreadLabel(100, true)).toBe("99+");
    // The same number WITHOUT the flag is a real count of a hundred.
    expect(unreadLabel(100, false)).toBe("100");
    expect(unreadLabel(0, false)).toBe("");
  });

  test("a rail with no read state behind it is not a rail with nothing unread", () => {
    expect(badgesAreMeasured({ read_state: true })).toBe(true);
    expect(badgesAreMeasured({ read_state: false })).toBe(false);
    expect(badgesAreMeasured(null)).toBe(false);
  });
});
