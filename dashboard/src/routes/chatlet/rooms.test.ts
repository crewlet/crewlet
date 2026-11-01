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

import {
  isConversationWith,
  knownKind,
  roomMark,
  roomTitle,
  roomWhere,
  unreadLabel,
} from "./rooms.ts";
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

// WHAT A SENTENCE CALLS A ROOM, the one place a `#` is written. A channel's
// glyph IS its `#`, so the words drawn beside a glyph are the bare title and
// only words standing alone carry this. A kind this build does not know is a
// channel to every other sentence (`isDirect` says it is not a
// conversation), so it reads as one here too. Mutation: add the `#` for a
// team's room or a conversation, or drop it for a kind not yet known, and the
// matching case goes red.
describe("what a sentence calls a room", () => {
  test.each([
    ["public", "#general"],
    ["private", "#general"],
    ["unit", "general"],
    ["dm", "general"],
    ["group", "general"],
    ["", "#general"],
    ["from-a-newer-engine", "#general"],
  ])("a %s room reads as %s", (kind, said) => {
    expect(roomWhere(kind, "general")).toBe(said);
  });
});

describe("a kind this build does not know", () => {
  test("is not classified, and not drawn as public", () => {
    // An open enum read two-valued falls out as "not private", which is how a
    // newer peer's room becomes writable by anyone. The mark says "unknown"
    // rather than borrowing the public one.
    expect(knownKind("broadcast")).toBe(false);
    expect(roomMark("broadcast")).toBe("circle-question-mark");
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
});

// A SENTENCE ABOUT WHAT SOMEBODY DID IN A CONVERSATION OF THE TWO OF YOU NAMES
// NO ROOM: its title is their own name, so "replied to a thread in Product
// Manager" repeated the name the line began with.
describe("a conversation between the reader and one other", () => {
  test("is the one-to-one conversation with the person who acted, and nothing else", () => {
    expect(isConversationWith("dm", "pm", "ada", ["ada", "pm"])).toBe(true);
    // Its people not in hand, a one-to-one conversation holds the reader and whoever acted.
    expect(isConversationWith("dm", "pm", "ada")).toBe(true);
    expect(isConversationWith("dm", "pm", "ada", ["ada", "bo"])).toBe(false);
    expect(isConversationWith("dm", "ada", "ada", ["ada", "pm"])).toBe(false);
    expect(isConversationWith("group", "pm", "ada", ["ada", "pm", "bo"])).toBe(false);
    expect(isConversationWith("group", "pm", "ada", ["ada", "pm"])).toBe(false);
    for (const kind of ["public", "private", "unit"]) {
      expect(isConversationWith(kind, "pm", "ada", ["ada", "pm"]), kind).toBe(false);
    }
  });
});
