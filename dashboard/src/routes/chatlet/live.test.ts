/**
 * What arrived since a screen last looked.
 *
 * A screen holding more than one page reads again only what a frame names, so
 * it has to be told exactly which frames it has not yet answered — and, as
 * plainly, when it cannot be told: a frame it never saw was let go of, and
 * "nothing arrived" would leave a row drawn as it was before that frame
 * changed it.
 */

import { act, renderHook } from "@testing-library/react";
import { createElement, type ReactNode } from "react";
import { describe, expect, test, vi } from "vitest";

import { ClientContext } from "~/lib/store-hooks.ts";
import { Store, type ChatChange } from "~/protocol/index.ts";
import { MAX_CHAT_LIVE } from "~/protocol/store.ts";
import { framesSince, LIVE_REFETCH_MS, useChatFrames } from "./live.ts";

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
    store.forgetSeat();
    expect(framesSince(store.state.chat, MAX_CHAT_LIVE, null)).toBeNull();
  });
});

// A FRAME NAMING NO ROOM IS A GAP: the engine let go of pushes it could not
// send this socket, and says so once rather than per room, since it cannot
// say which rooms they were in. Mutation: treat it as an ordinary frame
// (filed under the empty room) and every case below goes red — a screen that
// answered before it is told "nothing arrived" for a room whose message it
// never heard about.
describe("a gap", () => {
  const gap = { channel_id: "" } as ChatChange;

  test("is a frame nothing since can be said about, in any room", () => {
    const store = new Store();
    store.applyChatChange(change("room-1", "a"));
    const through = store.state.chat.frames;
    store.applyChatChange(gap);
    expect(framesSince(store.state.chat, through, "room-1")).toBeNull();
    expect(framesSince(store.state.chat, through, "room-2")).toBeNull();
    expect(framesSince(store.state.chat, through, null)).toBeNull();
    expect(store.state.chat.rooms[""]).toBeUndefined();

    // A screen that read again after it is told only what came since.
    const after = store.state.chat.frames;
    store.applyChatChange(change("room-1", "b"));
    expect(framesSince(store.state.chat, after, "room-1")?.map((held) => held.op_id)).toEqual([
      "b",
    ]);
  });

  test("moves the signal of a room no frame has reached, which re-reads it whole", async () => {
    vi.useFakeTimers();
    try {
      const store = new Store();
      const wrapper = ({ children }: { children: ReactNode }) =>
        createElement(ClientContext.Provider, { value: { store, socket: {} } as never }, children);
      const task = vi.fn();
      renderHook(() => useChatFrames("room-quiet", task), { wrapper });
      await act(async () => {
        store.applyChatChange(gap);
        await vi.advanceTimersByTimeAsync(LIVE_REFETCH_MS * 2);
      });
      expect(task).toHaveBeenCalledWith(null);
    } finally {
      vi.useRealTimers();
    }
  });
});
