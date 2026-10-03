/**
 * The crumb's ordinal is found without walking a task's whole history.
 *
 * THE CASE IS A TURN CHARGED ELSEWHERE: it is on no page of this task's list,
 * and the walk used to read every page to learn that — one query per fifty
 * turns, to render a crumb. The list is newest first by when each turn landed
 * and a turn lands after it starts, so a page reaching a row that landed
 * before this turn began is the last page that could hold it.
 */

import { act, cleanup, renderHook } from "~/test/inCase.ts";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, test } from "vitest";

import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store, type WorkItemRef } from "~/protocol/index.ts";
import { taskPath, useTurnOrdinal } from "./useTurnOrdinal.ts";

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

/** The task the walked history is about, as a turn names the one it is charged to. */
const TASK: WorkItemRef = { backend: "native", id: "i-1", key: "ENG-1", project: "ENG" };

function mount(startedAt: number, { task = TASK, turnId = "elsewhere", running = false } = {}) {
  const store = new Store();
  const socket = new LiveSocket(store);
  const asked: string[] = [];
  const ids: unknown[] = [];
  (
    socket as unknown as { query: (w: string, p: Record<string, unknown>) => Promise<unknown> }
  ).query = (what, params) => {
    if (what !== "work_item_turns") return Promise.resolve({});
    const cursor = String(params.cursor ?? "");
    asked.push(cursor);
    ids.push(params.id);
    return Promise.resolve(page(cursor ? Number(cursor.slice(1)) : 0));
  };
  const wrapper = ({ children }: { children: ReactNode }) => (
    <ClientContext.Provider value={{ store, socket }}>{children}</ClientContext.Provider>
  );
  const hook = renderHook(() => useTurnOrdinal(task, turnId, running, startedAt), {
    wrapper,
  });
  return { asked, ids, hook };
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

// A NATIVE TASK IS ASKED FOR BY ITS ID. Two tasks can hold one key and the
// engine answers a key with the one that claimed it first, so a turn on the
// other one, asked by its key, was numbered against the claimant's history.
//
// Mutation: ask by `task.key` again, and the walk asks for "ENG-1".
test("a native turn's task is asked for by its id, never by the key two tasks may hold", async () => {
  const { ids } = mount(0);
  await settle();
  expect(ids).toEqual(["i-1"]);
});

// AND A PAGE ABOUT ANOTHER TASK NUMBERS NOTHING: the engine answering a
// different task than the turn names — the claimant of its key, or the task
// the seat was on a moment ago — is not this turn's history.
//
// Mutation: drop the `answersFor` check, and the running turn reads "Turn 101".
test("a page answered for another task gives the turn no number", async () => {
  const other = { ...TASK, id: "i-2" };
  const { hook } = mount(0, { task: other, turnId: "running-now", running: true });
  await settle();
  expect(hook.result.current).toBeNull();
  const { hook: own } = mount(0, { turnId: "running-now", running: true });
  await settle();
  expect(own.result.current).toBe(101);
});

// WHERE THE TASK OPENS: through `itemAddress` once a read carried its
// `key_collision` — the key for an unshared one, the id for a flagged one —
// and by its id until then, which opens the right task whichever claimed the
// key. A vendor's item keeps its key.
test("a turn's task opens by its address once read, and by its identity before", () => {
  expect(taskPath(TASK, null)).toEqual(["work", "i-1"]);
  expect(taskPath(TASK, { id: "i-1", key: "ENG-1" })).toEqual(["work", "ENG-1"]);
  expect(taskPath(TASK, { id: "i-1", key: "ENG-1", key_collision: true })).toEqual(["work", "i-1"]);
  expect(taskPath({ ...TASK, backend: "jira", id: "10042" }, null)).toEqual(["work", "ENG-1"]);
});
