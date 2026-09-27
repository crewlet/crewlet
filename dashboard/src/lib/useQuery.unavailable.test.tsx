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
import { useQuery } from "./useQuery.ts";
import { LiveSocket, QueryRefusedError, Store, UNAVAILABLE_RETRY_MS } from "~/protocol/index.ts";

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

// AN `unavailable` THE ENGINE SAID WAITING WILL NOT CLEAR IS NOT ASKED AGAIN
// SOON. A full log, a record the node cannot decode, a barrier its broker
// refused: each answers the same read the same until an operator acts, and a
// five-second re-ask of it is a loop rather than a retry. The engine says so
// with a zero hint; one it says will clear keeps coming back, which is the
// control — and so does a frame from a node too old to carry a hint.
test.each([
  ["a refusal no wait clears", 0, 1],
  ["a refusal that clears, the control", 5, 2],
])("%s", async (_, retryAfter, asks) => {
  const refused = () =>
    Promise.reject(
      new QueryRefusedError("unavailable", {
        code: retryAfter === 0 ? "log_full" : "behind",
        detail: "what the refusal is about",
        retryAfter,
      }),
    );
  const { asked, text } = mount([refused, answer]);
  await act(async () => {});
  expect(text()).toBe("unavailable");
  await act(async () => {
    await vi.advanceTimersByTimeAsync(UNAVAILABLE_RETRY_MS * 4);
  });
  expect(asked).toHaveBeenCalledTimes(asks);
});

// A SCREEN THAT POLLS STILL POLLS: only the five-second re-ask is withheld, so
// a refusal an operator then clears is noticed on the screen's own cadence.
test("a refusal no wait clears is still asked again at the screen's own poll", async () => {
  const refused = () =>
    Promise.reject(
      new QueryRefusedError("unavailable", { code: "log_full", detail: null, retryAfter: 0 }),
    );
  const { asked, text } = mount([refused, answer], 30_000);
  await act(async () => {});
  await act(async () => {
    await vi.advanceTimersByTimeAsync(UNAVAILABLE_RETRY_MS);
  });
  expect(asked).toHaveBeenCalledTimes(1);
  await act(async () => {
    await vi.advanceTimersByTimeAsync(30_000);
  });
  expect(asked).toHaveBeenCalledTimes(2);
  expect(text()).toBe("answered");
});
