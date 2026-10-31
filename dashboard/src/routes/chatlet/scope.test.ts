import { describe, expect, test } from "vitest";

import type { ChatChannel } from "~/protocol/index.ts";

import { NEWER_ROOM } from "./rooms.ts";
import {
  ARCHIVED,
  DIRECT_MEMBERS,
  ORG_MEMBERS,
  OUTSIDE_MEMBERS,
  OUTSIDER,
  READING,
  roomRules,
  UNREAD,
} from "./scope.tsx";

const room = (over: Partial<ChatChannel> = {}): ChatChannel => ({
  v: 1,
  id: "room-1",
  kind: "public",
  name: "general",
  created_at: "2026-01-01T00:00:00Z",
  ...over,
});

const detail = (member: boolean) => ({ data: { member } as never, error: null });

describe("a room's rules, one per change", () => {
  test("a room not yet read takes nothing yet, and one whose read failed says so", () => {
    expect(Object.values(roomRules(undefined, null))).toEqual(Array(6).fill(READING));
    expect(roomRules(undefined, { data: null, error: "query_failed" }).post).toBe(UNREAD);
  });

  test("an archived room takes no message but still takes a change to one, and its settings", () => {
    const rules = roomRules(room({ archived_at: "2026-02-01T00:00:00Z" }), detail(true));
    expect(rules.post).toBe(ARCHIVED);
    expect([rules.react, rules.edit, rules.remove, rules.settings]).toEqual(["", "", "", ""]);
    expect(rules.members).toBe(ARCHIVED);
  });

  test("a private room the reader is outside refuses posts, reactions and settings, never moderation", () => {
    const rules = roomRules(room({ kind: "private" }), detail(false));
    expect([rules.post, rules.react, rules.settings]).toEqual([OUTSIDER, OUTSIDER, OUTSIDER]);
    expect([rules.edit, rules.remove]).toEqual(["", ""]);
    expect(rules.members).toBe(OUTSIDE_MEMBERS);
  });

  test("a kind this build cannot classify refuses every change", () => {
    const rules = roomRules(room({ kind: "galaxy" }), detail(true));
    expect(Object.values(rules)).toEqual(Array(6).fill(NEWER_ROOM));
  });

  test("members are the org chart's in a team's room and the people in a direct conversation", () => {
    expect(roomRules(room({ kind: "unit", unit: "eng" }), detail(true)).members).toBe(ORG_MEMBERS);
    expect(roomRules(room({ kind: "dm", name: undefined }), detail(true)).members).toBe(
      DIRECT_MEMBERS,
    );
    expect(roomRules(room(), detail(true)).members).toBe("");
    expect(roomRules(room(), detail(false)).members).toBe(OUTSIDE_MEMBERS);
  });
});
