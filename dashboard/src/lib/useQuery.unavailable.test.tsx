/**
 * An `unavailable` answer is asked again without anybody reloading.
 *
 * The engine answers `unavailable` for a question it will be able to answer in
 * a moment: a projection catching up after a restart, or a coordination store
 * that did not respond. The banner for it tells a person the screen fills in
 * on its own, and for every query without a poll that was untrue, because
 * nothing asked again.
 */

import { act, cleanup, render } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { ClientContext } from "./store-hooks.ts";
import { UNAVAILABLE_RETRY_MS, useQuery } from "./useQuery.ts";
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

const refuse = (code: string) => () => Promise.reject(new Error(code));
const answer = () => Promise.resolve({ nodes: [] });

test("an unavailable answer is asked again and fills in", async () => {
  const { asked, text } = mount([refuse("unavailable"), answer]);
  await act(async () => {});
  expect(text()).toBe("unavailable");
  expect(asked).toHaveBeenCalledTimes(1);

  await act(async () => {
    await vi.advanceTimersByTimeAsync(UNAVAILABLE_RETRY_MS);
  });
  expect(asked).toHaveBeenCalledTimes(2);
  expect(text()).toBe("answered");
});

// A slow poll does not hold a recovering screen for its whole interval.
test("an unavailable answer comes back sooner than a slow poll would", async () => {
  const { asked } = mount([refuse("unavailable"), answer], 60_000);
  await act(async () => {});
  await act(async () => {
    await vi.advanceTimersByTimeAsync(UNAVAILABLE_RETRY_MS);
  });
  expect(asked).toHaveBeenCalledTimes(2);
});

// Only `unavailable` means "ask again". Any other refusal is a fact about the
// request or the node that waiting does not change, and re-asking it would
// only repeat it.
test.each(["bad_params", "unknown_query", "query_failed", "unauthorized"])(
  "a %s answer is not asked again",
  async (code) => {
    const { asked } = mount([refuse(code)]);
    await act(async () => {});
    await act(async () => {
      await vi.advanceTimersByTimeAsync(UNAVAILABLE_RETRY_MS * 4);
    });
    expect(asked).toHaveBeenCalledTimes(1);
  },
);
