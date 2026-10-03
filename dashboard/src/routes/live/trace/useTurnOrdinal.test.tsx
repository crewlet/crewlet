/**
 * The crumb's ordinal is found without walking a task's whole history.
 *
 * THE CASE IS A TURN CHARGED ELSEWHERE: it is on no page of this task's list,
 * and the walk used to read every page to learn that — one query per fifty
 * turns, to render a crumb. The list is newest first by when each turn landed
 * and a turn lands after it starts, so a page reaching a row that landed
 * before this turn began is the last page that could hold it.
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

const T0 = Date.parse("2026-09-28T10:00:00Z");
const iso = (ms: number) => new Date(T0 + ms).toISOString();

/** Page n of a long history: two turns each, an hour apart, newest first. */
function page(n: number) {
  const hour = 3_600_000;
  return {
    item: "i-1",
    key: "ENG-1",
    complete: true,
    turns: [0, 1].map((i) => ({
      turn_id: `old-${n}-${i}`,
      ordinal: 100 - n * 2 - i,
      at: iso(-(n * 2 + i) * hour),
    })),
    next_cursor: n < 20 ? `p${n + 1}` : "",
  };
}

function mount(startedAt: number) {
  const store = new Store();
  const socket = new LiveSocket(store);
  const asked: string[] = [];
  (
    socket as unknown as { query: (w: string, p: Record<string, unknown>) => Promise<unknown> }
  ).query = (what, params) => {
    if (what !== "work_item_turns") return Promise.resolve({});
    const cursor = String(params.cursor ?? "");
    asked.push(cursor);
    return Promise.resolve(page(cursor ? Number(cursor.slice(1)) : 0));
  };
  const wrapper = ({ children }: { children: ReactNode }) => (
    <ClientContext.Provider value={{ store, socket }}>{children}</ClientContext.Provider>
  );
  const hook = renderHook(() => useTurnOrdinal("ENG-1", "elsewhere", false, startedAt), {
    wrapper,
  });
  return { asked, hook };
}

async function settle() {
  for (let i = 0; i < 30; i++) {
    await act(async () => {
      await Promise.resolve();
    });
  }
}

test("a turn that began after a page's oldest row stops the walk there", async () => {
  // Began three and a half hours ago: page 1's oldest row landed three hours
  // ago, so the turn could still be on page 2 — whose oldest landed five
  // hours ago, before the turn began, so it cannot be on page 3. Twenty-one
  // pages exist.
  const { asked, hook } = mount(T0 - 3.5 * 3_600_000);
  await settle();
  expect(asked).toEqual(["", "p1", "p2"]);
  expect(hook.result.current).toBeNull();
});

test("a turn whose start nothing stamped is looked for on the newest page alone", async () => {
  const { asked } = mount(0);
  await settle();
  expect(asked).toEqual([""]);
});
