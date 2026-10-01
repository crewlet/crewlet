/**
 * The engine's own health is ONE read per tab.
 *
 * `useQuery` is a poller per call, so the shell and the inbox each polled
 * `stream` every fifteen seconds while three screens polled it every five,
 * and one frame drew two readings of one engine taken up to ten seconds
 * apart — the rail saying the company was configured while the panel in front
 * of it said it was not.
 */

import { act, cleanup, render } from "~/test/inCase.ts";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { HEALTH_POLL_MS, useEngineHealth } from "./engineHealth.ts";
import { ClientContext } from "./store-hooks.ts";
import { LiveSocket, QueryRefusedError, Store } from "~/protocol/index.ts";

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

/** A socket whose `stream` answers count themselves, one configured flag each. */
function client() {
  const store = new Store();
  store.setConnected(true);
  const socket = new LiveSocket(store);
  const asked: string[] = [];
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) => {
    asked.push(what);
    return Promise.resolve({ configured: asked.length % 2 === 1, status: "healthy" });
  };
  return { store, socket, asked };
}

function Reading({ label }: { label: string }) {
  const { data } = useEngineHealth();
  return <p>{`${label}: ${data === null ? "unread" : data.configured ? "configured" : "not"}`}</p>;
}

async function flush() {
  await act(async () => {
    await Promise.resolve();
    await Promise.resolve();
  });
}

test("every surface reading it shares one poll, and the same answer", async () => {
  const { store, socket, asked } = client();
  const view = render(
    <ClientContext.Provider value={{ store, socket }}>
      <Reading label="rail" />
      <Reading label="inbox" />
      <Reading label="panel" />
    </ClientContext.Provider>,
  );
  await flush();
  expect(asked).toEqual(["stream"]);
  expect(view.getByText("rail: configured")).toBeDefined();
  expect(view.getByText("panel: configured")).toBeDefined();

  // ONE TICK, ONE READ — and every surface moves with it.
  await act(async () => {
    await vi.advanceTimersByTimeAsync(HEALTH_POLL_MS);
  });
  expect(asked).toEqual(["stream", "stream"]);
  expect(view.getByText("rail: not")).toBeDefined();
  expect(view.getByText("inbox: not")).toBeDefined();
  expect(view.getByText("panel: not")).toBeDefined();
});

test("the poll stops with the last surface reading it, and keeps what it knew", async () => {
  const { store, socket, asked } = client();
  const view = render(
    <ClientContext.Provider value={{ store, socket }}>
      <Reading label="rail" />
    </ClientContext.Provider>,
  );
  await flush();
  view.unmount();
  await act(async () => {
    await vi.advanceTimersByTimeAsync(HEALTH_POLL_MS * 3);
  });
  expect(asked).toEqual(["stream"]);

  // THE NEXT SURFACE draws what is known at once, and asks again.
  const again = render(
    <ClientContext.Provider value={{ store, socket }}>
      <Reading label="rail" />
    </ClientContext.Provider>,
  );
  expect(again.getByText("rail: configured")).toBeDefined();
  await flush();
  expect(asked).toEqual(["stream", "stream"]);
});

test("a reconnect asks again: an answer from before it is about an engine that moved", async () => {
  const { store, socket, asked } = client();
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Reading label="rail" />
    </ClientContext.Provider>,
  );
  await flush();
  act(() => store.setConnected(false));
  act(() => store.setConnected(true));
  await flush();
  expect(asked).toEqual(["stream", "stream"]);
});

/**
 * A socket whose `stream` answers are scripted, one per ask, the last one
 * repeating.
 */
function scripted(answers: Array<() => Promise<unknown>>) {
  const store = new Store();
  const socket = new LiveSocket(store);
  let next = 0;
  const asked = vi.fn(() => answers[Math.min(next++, answers.length - 1)]!());
  (socket as unknown as { query: () => Promise<unknown> }).query = asked;
  return { store, socket, asked };
}

const healthy = () => Promise.resolve({ configured: true, status: "healthy" });

/** An `unavailable` rejection carrying the engine's hint, as the socket makes one. */
function unavailable(retryAfter: number) {
  return () =>
    Promise.reject(new QueryRefusedError("unavailable", { code: null, detail: null, retryAfter }));
}

async function wait(ms: number) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });
}

// THE SHARED READ WAITS WHAT THE ENGINE SAID, like every other question: a
// node that said twelve seconds is not asked at the next five-second tick,
// when it could only refuse again, and the tick resumes once it answers.
test("an unavailable answer is asked again when its hint says, in place of the tick", async () => {
  const { store, socket, asked } = scripted([unavailable(12), healthy]);
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Reading label="rail" />
    </ClientContext.Provider>,
  );
  await flush();
  expect(asked).toHaveBeenCalledTimes(1);
  await wait(11_999);
  expect(asked).toHaveBeenCalledTimes(1);
  await wait(1);
  expect(asked).toHaveBeenCalledTimes(2);
  await wait(HEALTH_POLL_MS);
  expect(asked).toHaveBeenCalledTimes(3);
});

// AND A ZERO IS NOT ASKED ON A TIMER AT ALL: the node said waiting will not
// change it, and a five-second poll of that is a loop. A reconnect asks again,
// which is the control.
test("an unavailable answer no wait clears stops the poll until a reconnect", async () => {
  const { store, socket, asked } = scripted([unavailable(0), healthy]);
  store.setConnected(true);
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Reading label="rail" />
    </ClientContext.Provider>,
  );
  await flush();
  await wait(HEALTH_POLL_MS * 10);
  expect(asked).toHaveBeenCalledTimes(1);

  act(() => store.setConnected(false));
  act(() => store.setConnected(true));
  await flush();
  expect(asked).toHaveBeenCalledTimes(2);
});

test("two tabs are two sockets, and each has its own read", async () => {
  const one = client();
  const two = client();
  render(
    <>
      <ClientContext.Provider value={{ store: one.store, socket: one.socket }}>
        <Reading label="one" />
      </ClientContext.Provider>
      <ClientContext.Provider value={{ store: two.store, socket: two.socket }}>
        <Reading label="two" />
      </ClientContext.Provider>
    </>,
  );
  await flush();
  expect(one.asked).toEqual(["stream"]);
  expect(two.asked).toEqual(["stream"]);
});

// A TICK THAT SAID NOTHING NEW WAKES NOBODY. The read is polled every five
// seconds for as long as anything shows it, and every answer is parsed afresh:
// handed over whole, each tick re-rendered every surface reading it — the rail,
// the inbox, the panel — to change no pixel. Shared with the last answer
// (`~/protocol/share.ts`), an unchanged tick is the reading already held.
test("a tick that brought back the same health renders no reader", async () => {
  const store = new Store();
  store.setConnected(true);
  const socket = new LiveSocket(store);
  (socket as unknown as { query: () => Promise<unknown> }).query = () =>
    Promise.resolve({ configured: true, status: "healthy" });
  let renders = 0;
  function Counted() {
    useEngineHealth();
    renders += 1;
    return null;
  }
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Counted />
    </ClientContext.Provider>,
  );
  await flush();
  const settled = renders;
  await act(async () => {
    await vi.advanceTimersByTimeAsync(HEALTH_POLL_MS * 3);
  });
  expect(renders).toBe(settled);
});
