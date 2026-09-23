/**
 * A seat's inbox moving asks the answer again — soon, once per run of frames,
 * and only for the seat it moved.
 *
 * The engine pushes `inbox_changed` once per APPLIED BATCH, so a bulk gesture
 * arrives as several frames a few hundred milliseconds apart. Asking once per
 * frame would spend a person's four in-flight questions on one gesture;
 * waiting for the frames to STOP would let a steady run hold the screen stale
 * for as long as it lasted. The window is a bound: the first frame starts it
 * and later ones join it.
 */

import { act, cleanup, render } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { ClientContext } from "./store-hooks.ts";
import { INBOX_SETTLE_MS, useQuery } from "./useQuery.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";

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
  vi.useFakeTimers();
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

function Probe({ seat }: { seat: string }) {
  useQuery("work_inbox", { handle: seat }, { refetchOnInboxOf: seat });
  return null;
}

/** Mounts a probe for `seat` and counts what it asks. */
async function mount(seat: string) {
  const store = new Store();
  const socket = new LiveSocket(store);
  const asked = vi.fn(() => Promise.resolve({ handle: seat, notices: [], primary_reasons: [] }));
  (socket as unknown as { query: () => Promise<unknown> }).query = asked;
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Probe seat={seat} />
    </ClientContext.Provider>,
  );
  await act(async () => {});
  expect(asked).toHaveBeenCalledTimes(1);
  const moved = (handle: string) =>
    store.applyInboxChanged({ handle, unread_delta: 1, subject: "t-1", reason: "assignee" });
  const wait = (ms: number) =>
    act(async () => {
      await vi.advanceTimersByTimeAsync(ms);
    });
  return { asked, moved, wait };
}

test("a movement asks again within the settle window, once for the frames it collected", async () => {
  const { asked, moved, wait } = await mount("ana");
  moved("ana");
  moved("ana");
  moved("ana");
  await wait(INBOX_SETTLE_MS - 1);
  expect(asked).toHaveBeenCalledTimes(1);
  await wait(1);
  expect(asked).toHaveBeenCalledTimes(2);
});

test("a steady run of frames still asks every window rather than waiting for it to stop", async () => {
  const { asked, moved, wait } = await mount("ana");
  // A frame every fifth of the window, for four windows: a debounce would
  // ask once, after the last one.
  for (let i = 0; i < 20; i++) {
    moved("ana");
    await wait(INBOX_SETTLE_MS / 5);
  }
  expect(asked.mock.calls.length).toBeGreaterThanOrEqual(1 + 4);
});

test("a movement of another seat's inbox asks nothing", async () => {
  const { asked, moved, wait } = await mount("ana");
  moved("bo");
  await wait(10 * INBOX_SETTLE_MS);
  expect(asked).toHaveBeenCalledTimes(1);
});
