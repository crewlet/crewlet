/**
 * An `unavailable` answer is asked again without anybody reloading — when the
 * engine said it could be answered, and not before or after.
 *
 * The engine answers `unavailable` for a question it will be able to answer in
 * a moment: a projection catching up after a restart, or a coordination store
 * that did not respond. The banner for it tells a person the screen fills in
 * on its own, and for every query without a poll that was untrue, because
 * nothing asked again. And its frame says WHEN (`retry_after`): the hook
 * re-asked at a fixed five seconds whatever that said, so a node draining a
 * backlog it had said would take twenty seconds was asked four times first,
 * and a refusal only an operator could lift went on being polled.
 */

import { act, cleanup, render } from "~/test/inCase.ts";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { ClientContext } from "./store-hooks.ts";
import { useQuery } from "./useQuery.ts";
import {
  LiveSocket,
  QueryRefusedError,
  RETRY_AFTER_MAX_MS,
  Store,
  UNAVAILABLE_RETRY_MS,
} from "~/protocol/index.ts";

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
  return { asked, store, text: () => view.getByTestId("probe").textContent };
}

/** Moves the fake clock, letting every answer it releases settle. */
async function wait(ms: number) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });
}

const refuse = (code: string) => () => Promise.reject(new Error(code));
const answer = () => Promise.resolve({ nodes: [] });

/**
 * An `unavailable` rejection as the socket makes one from a frame carrying
 * `retry_after`: the state log's code and words, and the hint.
 */
function hinted(retryAfter: number) {
  return () =>
    Promise.reject(
      new QueryRefusedError("unavailable", {
        code: retryAfter === 0 ? "log_full" : "behind",
        detail: "what the refusal is about",
        retryAfter,
      }),
    );
}

// AN ANSWER WITH NO HINT waits what the engine says when it has nothing
// better: a frame from a node older than the field.
test("an unavailable answer with no hint is asked again and fills in", async () => {
  const { asked, text } = mount([refuse("unavailable"), answer]);
  await act(async () => {});
  expect(text()).toBe("unavailable");
  expect(asked).toHaveBeenCalledTimes(1);

  await wait(UNAVAILABLE_RETRY_MS - 1);
  expect(asked).toHaveBeenCalledTimes(1);
  await wait(1);
  expect(asked).toHaveBeenCalledTimes(2);
  expect(text()).toBe("answered");
});

// A slow poll does not hold a recovering screen for its whole interval.
test("an unavailable answer with no hint comes back sooner than a slow poll would", async () => {
  const { asked } = mount([refuse("unavailable"), answer], 60_000);
  await act(async () => {});
  await wait(UNAVAILABLE_RETRY_MS);
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
    await wait(UNAVAILABLE_RETRY_MS * 4);
    expect(asked).toHaveBeenCalledTimes(1);
  },
);

// THE HINT IS WAITED OUT, EXACTLY. A node twelve seconds behind is not asked
// at five, when it could only refuse again, and is asked at twelve.
test("an unavailable answer is asked again when its hint says, not before", async () => {
  const { asked, text } = mount([hinted(12), answer]);
  await act(async () => {});
  expect(text()).toBe("unavailable");
  await wait(11_999);
  expect(asked).toHaveBeenCalledTimes(1);
  await wait(1);
  expect(asked).toHaveBeenCalledTimes(2);
  expect(text()).toBe("answered");
});

// THE HINT REPLACES THE POLL'S NEXT TICK IN BOTH DIRECTIONS. Later than a
// five-second poll: a node that said twenty seconds refuses every tick before
// it. Sooner than a minute's: a recovered node is not held for the minute.
test.each([
  ["later than a quick poll", 5_000, 20],
  ["sooner than a slow poll", 60_000, 3],
])("a hint %s is the next ask", async (_, pollMs, seconds) => {
  const { asked } = mount([hinted(seconds), answer], pollMs);
  await act(async () => {});
  await wait(seconds * 1_000 - 1);
  expect(asked).toHaveBeenCalledTimes(1);
  await wait(1);
  expect(asked).toHaveBeenCalledTimes(2);
});

// A DERIVED HINT IS BOUNDED. A node minutes behind a busy log says so, from a
// drain rate that only rises as it warms up; the screen asks again at the
// bound rather than holding "fills in on its own" for the whole estimate.
test("a hint past the bound is asked again at the bound", async () => {
  const { asked } = mount([hinted(600), answer]);
  await act(async () => {});
  await wait(RETRY_AFTER_MAX_MS - 1);
  expect(asked).toHaveBeenCalledTimes(1);
  await wait(1);
  expect(asked).toHaveBeenCalledTimes(2);
});

// AN `unavailable` THE ENGINE SAID WAITING WILL NOT CLEAR IS NOT ASKED AGAIN
// ON A TIMER AT ALL — NOT EVEN BY THE SCREEN'S OWN POLL. A full log, a record
// the node cannot decode, a barrier its broker refused: each answers the same
// read the same until an operator acts, so a poll of it is a loop rather than
// a retry, and the banner says the screen does not keep asking. What asks
// again is what an operator's fix can come with: a reconnect — the node
// restarted — or anything a person does that refetches.
test("a refusal no wait clears stops the poll until something asks again", async () => {
  const { asked, store, text } = mount([hinted(0), answer], 30_000);
  await act(async () => {});
  expect(text()).toBe("unavailable");
  await wait(30_000 * 10);
  expect(asked).toHaveBeenCalledTimes(1);

  // A RECONNECT is the socket saying the engine moved.
  act(() => store.setConnected(true));
  await act(async () => {});
  expect(asked).toHaveBeenCalledTimes(2);
  expect(text()).toBe("answered");
  // AND THE POLL IS BACK once an answer is.
  await wait(30_000);
  expect(asked).toHaveBeenCalledTimes(3);
});
