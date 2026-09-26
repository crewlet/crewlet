/**
 * An `unavailable` answer is asked again without anybody reloading — after
 * the wait the ENGINE named.
 *
 * The engine answers `unavailable` for a question it will be able to answer in
 * a moment: a projection catching up after a restart, or a coordination store
 * that did not respond. The banner for it tells a person the screen fills in
 * on its own, and for every query without a poll that was untrue, because
 * nothing asked again. The wait is the frame's `retry_after_seconds` — this
 * node's own lag over its drain rate — and never a constant of the client's.
 */

import { act, cleanup, render } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { ClientContext } from "./store-hooks.ts";
import { useQuery } from "./useQuery.ts";
import { LiveSocket, QueryError, Store } from "~/protocol/index.ts";

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

function Probe({ pollMs }: { pollMs?: number }) {
  const { data, error } = useQuery("fleet", undefined, { pollMs });
  return <span data-testid="probe">{error ?? (data ? "answered" : "waiting")}</span>;
}

/** Mounts a probe over a socket whose answers are scripted, one per ask. */
function mount(answers: Array<() => Promise<unknown>>, pollMs?: number) {
  const store = new Store();
  const socket = new LiveSocket(store);
  // One answer per ask, the last one repeating: a test that wants two asks
  // scripts two, and a test that wants one refusal for ever scripts one.
  let next = 0;
  const asked = vi.fn(() => answers[Math.min(next++, answers.length - 1)]!());
  (socket as unknown as { query: () => Promise<unknown> }).query = asked;
  const view = render(
    <ClientContext.Provider value={{ store, socket }}>
      <Probe pollMs={pollMs} />
    </ClientContext.Provider>,
  );
  return { asked, text: () => view.getByTestId("probe").textContent };
}

const refuse =
  (code: string, hint: number | null = null) =>
  () =>
    Promise.reject(new QueryError(code, hint));
const answer = () => Promise.resolve({ nodes: [] });

test("an unavailable answer is asked again after the engine's wait, and fills in", async () => {
  const { asked, text } = mount([refuse("unavailable", 3), answer]);
  await act(async () => {});
  expect(text()).toBe("unavailable");
  expect(asked).toHaveBeenCalledTimes(1);

  // NOT BEFORE: a node that said three seconds asked sooner is a node asked
  // again before anything could have changed.
  await act(async () => {
    await vi.advanceTimersByTimeAsync(2_999);
  });
  expect(asked).toHaveBeenCalledTimes(1);

  await act(async () => {
    await vi.advanceTimersByTimeAsync(1);
  });
  expect(asked).toHaveBeenCalledTimes(2);
  expect(text()).toBe("answered");
});

// A LONGER WAIT IS HONOURED TOO: a node grinding through a bulk apply that
// asks for twelve seconds is not asked at five.
test("a long hint is waited out", async () => {
  const { asked } = mount([refuse("unavailable", 12), answer]);
  await act(async () => {});
  await act(async () => {
    await vi.advanceTimersByTimeAsync(11_000);
  });
  expect(asked).toHaveBeenCalledTimes(1);
  await act(async () => {
    await vi.advanceTimersByTimeAsync(1_000);
  });
  expect(asked).toHaveBeenCalledTimes(2);
});

// A slow poll does not hold a recovering screen for its whole interval.
test("an unavailable answer comes back sooner than a slow poll would", async () => {
  const { asked } = mount([refuse("unavailable", 5), answer], 60_000);
  await act(async () => {});
  await act(async () => {
    await vi.advanceTimersByTimeAsync(5_000);
  });
  expect(asked).toHaveBeenCalledTimes(2);
});

// A hint below a second is not a loop: zero is "now", against a node that has
// just said it cannot answer.
test("a zero hint waits a second", async () => {
  const { asked } = mount([refuse("unavailable", 0), answer]);
  await act(async () => {});
  await act(async () => {
    await vi.advanceTimersByTimeAsync(999);
  });
  expect(asked).toHaveBeenCalledTimes(1);
  await act(async () => {
    await vi.advanceTimersByTimeAsync(1);
  });
  expect(asked).toHaveBeenCalledTimes(2);
});

// Only a refusal the engine named a wait for is asked again on its own. Any
// other code is a fact about the request or the node that waiting does not
// change, and an `unavailable` with no wait gets no invented one.
test.each([
  ["bad_params", null],
  ["unknown_query", null],
  ["query_failed", null],
  ["unauthorized", null],
  ["unavailable", null],
] as const)("a %s answer with no wait is not asked again", async (code, hint) => {
  const { asked } = mount([refuse(code, hint)]);
  await act(async () => {});
  await act(async () => {
    await vi.advanceTimersByTimeAsync(120_000);
  });
  expect(asked).toHaveBeenCalledTimes(1);
});
