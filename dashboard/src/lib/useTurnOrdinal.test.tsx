/**
 * A turn's number on its task is its story's, asked once by the turn's id.
 *
 * THE CASES ARE THE ONES THE WALK GOT WRONG OR PAID FOR: a turn charged to
 * another task walked every page of this one's history to learn it was not
 * there, and a running turn was numbered "the newest plus one" from whatever
 * page was in hand. One question by the turn's id answers both, with the
 * engine's count.
 */

import { act, cleanup, renderHook } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, test } from "vitest";

import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { useTurnOrdinal } from "./useTurnOrdinal.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
});
afterEach(cleanup);

/** A story on `key`, numbered `ordinal` there. */
const story = (key: string, ordinal?: number) => ({
  complete: true,
  story: {
    turn_id: "run-1",
    seat: "swe",
    work_item: { backend: "native", id: "t-1", key, ...(ordinal ? { ordinal } : {}) },
    started_at: "2026-09-28T10:00:00Z",
    duration_ms: 0,
    why: { origin: "task" },
    did: [],
    left_behind: [],
    outcome: "running",
    helpers: 0,
    consulted: [],
  },
});

function mount(itemKey: string, answer: () => Promise<unknown>) {
  const store = new Store();
  const socket = new LiveSocket(store);
  const asked: { what: string; params: Record<string, unknown> }[] = [];
  (
    socket as unknown as { query: (w: string, p: Record<string, unknown>) => Promise<unknown> }
  ).query = (what, params) => {
    asked.push({ what, params });
    return answer();
  };
  const wrapper = ({ children }: { children: ReactNode }) => (
    <ClientContext.Provider value={{ store, socket }}>{children}</ClientContext.Provider>
  );
  const hook = renderHook(() => useTurnOrdinal(itemKey, "run-1"), { wrapper });
  return { asked, hook };
}

async function settle() {
  for (let i = 0; i < 10; i++) {
    await act(async () => {
      await Promise.resolve();
    });
  }
}

test("the number is the story's, asked once by the turn", async () => {
  const { asked, hook } = mount("ENG-1", () => Promise.resolve(story("ENG-1", 3)));
  await settle();
  expect(hook.result.current).toBe(3);
  expect(asked.map((a) => a.what)).toEqual(["turn_story"]);
  expect(asked[0]!.params).toMatchObject({ turn_id: "run-1" });
});

test("a turn whose story is on another task has no number here", async () => {
  const { hook } = mount("ENG-1", () => Promise.resolve(story("ENG-2", 3)));
  await settle();
  expect(hook.result.current).toBeNull();
});

test("a task that does not count its turns, or a turn that is not a story, says nothing", async () => {
  const uncounted = mount("ENG-1", () => Promise.resolve(story("ENG-1")));
  await settle();
  expect(uncounted.hook.result.current).toBeNull();
  const unknown = mount("ENG-1", () => Promise.reject(new Error("not_found")));
  await settle();
  expect(unknown.hook.result.current).toBeNull();
});

test("off a task nothing is asked", async () => {
  const { asked, hook } = mount("", () => Promise.resolve(story("ENG-1", 3)));
  await settle();
  expect(hook.result.current).toBeNull();
  expect(asked).toEqual([]);
});
