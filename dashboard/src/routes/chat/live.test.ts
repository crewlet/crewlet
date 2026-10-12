/**
 * What arrived since a screen last looked.
 *
 * A screen holding more than one page reads again only what a frame names, so
 * it has to be told exactly which frames it has not yet answered — and, as
 * plainly, when it cannot be told: a frame it never saw was let go of, and
 * "nothing arrived" would leave a row drawn as it was before that frame
 * changed it.
 */

import { describe, expect, test } from "vitest";

import { Store, type ChatChange } from "~/protocol/index.ts";
import { MAX_CHAT_LIVE } from "~/protocol/store.ts";
import { framesSince } from "./live.ts";

function change(channel: string, op: string): ChatChange {
  return {
    channel_id: channel,
    op: "post",
    op_id: op,
    at: "2026-01-01T00:00:00Z",
    position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 1 },
  };
}

describe("the frames since one a screen answered", () => {
  test("in one room, newest first, and none it already answered", () => {
    const store = new Store();
    store.applyChatChange(change("room-1", "a"));
    store.applyChatChange(change("room-2", "b"));
    store.applyChatChange(change("room-1", "c"));
    store.applyChatChange(change("room-1", "d"));
    expect(framesSince(store.state.chat, 1, "room-1")?.map((held) => held.op_id)).toEqual([
      "d",
      "c",
    ]);
    expect(framesSince(store.state.chat, 4, "room-1")).toEqual([]);
  });

  test("in every room, in the order this tab accepted them", () => {
    const store = new Store();
    store.applyChatChange(change("room-1", "a"));
    store.applyChatChange(change("room-2", "b"));
    store.applyChatChange(change("room-1", "c"));
    expect(framesSince(store.state.chat, 1, null)?.map((held) => held.op_id)).toEqual(["c", "b"]);
  });

  test("a frame let go of since is said, rather than read as nothing having arrived", () => {
    const store = new Store();
    for (let i = 0; i <= MAX_CHAT_LIVE; i++) store.applyChatChange(change("room-1", `op-${i}`));
    // The first frame was pushed off, so a screen that answered none of them
    // cannot be told what they changed.
    expect(framesSince(store.state.chat, 0, "room-1")).toBeNull();
    // One that answered it can.
    expect(framesSince(store.state.chat, 1, "room-1")).toHaveLength(MAX_CHAT_LIVE);
    store.forgetPerson();
    expect(framesSince(store.state.chat, MAX_CHAT_LIVE, null)).toBeNull();
  });
});
