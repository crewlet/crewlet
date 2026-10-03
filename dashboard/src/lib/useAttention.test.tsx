/**
 * The attention queue as the screens read it: a READING of the clock.
 *
 * The queue reads the clock — a live round goes stale at two minutes, and a
 * parked run counts its pause window down — so the hook is worked out on every
 * tick. Two ways to get that wrong, and each passes the other's case: a hook
 * that hands back a fresh queue every tick draws Home and the Inbox whole once
 * a second, and a hook computed once from its inputs never raises a condition
 * that only time crosses. These cases hold both halves.
 */

import type { ReactNode } from "react";
import { act, cleanup, renderHook } from "~/test/inCase.ts";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { useAttention } from "./useAttention.ts";
import { ClientContext } from "./store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

const T0 = Date.parse("2031-04-16T12:00:00Z");

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["setInterval", "clearInterval", "Date"] });
  vi.setSystemTime(T0);
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

/** A seat on one round, last heard from `updated`. */
function onOneRound(updated: string) {
  return {
    id: "a",
    agent_id: "id-a",
    role: "Dev A",
    handle: "dev-a",
    kind: "agent",
    activity: "working",
    live_call: {
      turn_id: "t1",
      phase: "execute",
      iteration: 1,
      model: "",
      trigger: null,
      prompt: "",
      prompt_messages: null,
      response: "",
      input_tokens: 0,
      output_tokens: 0,
      total_tokens: 0,
      tool_executions: null,
      round_num: 3,
      rounds: 3,
      in_progress: true,
      updated_at: updated,
    },
  };
}

function mount(updated: string) {
  const store = new Store();
  store.applyHealth({ status: "healthy", nodes: 1 } as never);
  store.applyOrg({ name: "Nimbus", timezone: "UTC", roles: [], units: [] } as never);
  store.applySeats([onOneRound(updated)] as never);
  const socket = new LiveSocket(store);
  socket.query = ((what: string) =>
    Promise.resolve(what === "sandbox_runs" ? { runs: [] } : {})) as typeof socket.query;
  const wrapper = ({ children }: { children: ReactNode }) => (
    <ClientContext.Provider value={{ store, socket }}>{children}</ClientContext.Provider>
  );
  return renderHook(() => useAttention(), { wrapper });
}

/** Moves the clock one tick at a time, as an open tab sees it. */
function tick(times: number): void {
  for (let i = 0; i < times; i++) {
    act(() => {
      vi.advanceTimersByTime(1_000);
    });
  }
}

const stale = (queue: readonly { id: string }[]) =>
  queue.filter((item) => item.id.startsWith("stale-"));

// A TICK THAT MOVES NO ITEM HANDS BACK THE QUEUE IT HAD — the same array, so
// nothing that reads it renders: ten seconds of a round heard from a second
// ago cross no threshold.
test("a tick that crosses no threshold hands back the same queue", () => {
  const { result } = mount(new Date(T0 - 1_000).toISOString());
  const first = result.current;
  expect(stale(first)).toEqual([]);

  tick(10);
  expect(result.current).toBe(first);
});

// AND THE TICK THAT CROSSES ONE RAISES IT — not the next poll, and not never,
// which is what a queue computed once from its inputs would do.
test("a round that stops moving is raised on the tick that crosses two minutes", () => {
  const { result } = mount(new Date(T0 - 115_000).toISOString());
  expect(stale(result.current)).toEqual([]);

  tick(4);
  expect(stale(result.current)).toEqual([]);

  tick(1);
  expect(stale(result.current).map((item) => item.id)).toEqual(["stale-id-a-t1"]);
});
