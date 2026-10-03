/**
 * The shared REST read: what it holds through a failure, how it draws one, and
 * when it asks again.
 *
 * Four screens' loaders became this one hook, and the case none of them could
 * express is the one these lead with: a request no answer came back to while
 * the live socket stayed up — thirty seconds on a slow engine, or one request
 * dropped. It was drawn `closed`, which promises a read once the socket is
 * back, and the socket never went away, so a read nothing polls was read
 * again only on a reload.
 */

import { act, cleanup, renderHook } from "~/test/inCase.ts";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { useRestRead, type RestReadOptions } from "./restRead.ts";
import { ClientContext } from "./store-hooks.ts";
import { LiveSocket, rest, RestError, REQUEST_TIMEOUT_MS, Store } from "~/protocol/index.ts";

let store = new Store();

function wrapper({ children }: { children: ReactNode }) {
  return (
    <ClientContext.Provider value={{ store, socket: new LiveSocket(store) }}>
      {children}
    </ClientContext.Provider>
  );
}

/** The instants each read began at, on the fake clock. */
let began: number[] = [];

/**
 * A read that answers what `script` says, in order — the last repeating. Each
 * entry either answers, throws, or never settles until its signal ends it.
 */
function scripted<T>(script: Array<(signal: AbortSignal) => Promise<T>>) {
  let at = 0;
  return (signal: AbortSignal): Promise<T> => {
    began.push(Date.now());
    const next = script[Math.min(at, script.length - 1)]!;
    at++;
    return next(signal);
  };
}

const answers =
  <T,>(value: T) =>
  () =>
    Promise.resolve(value);
const fails = (err: unknown) => () => Promise.reject(err);

/** The whole-second gaps between one read's start and the next. */
const gaps = () => began.slice(1).map((t, i) => Math.floor((t - began[i]!) / 1_000));

async function wait(ms: number) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });
}

function mount<T>(
  read: (signal: AbortSignal) => Promise<T>,
  options?: RestReadOptions<T>,
  key = "/thing",
) {
  return renderHook(({ k, o }: { k: string; o?: RestReadOptions<T> }) => useRestRead(k, read, o), {
    wrapper,
    initialProps: { k: key, o: options },
  });
}

beforeEach(() => {
  vi.useFakeTimers();
  store = new Store();
  began = [];
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

// THE FINDING, through the real transport: a request whose deadline passes
// with the socket up the whole time. It is `unanswered` — never `closed` — and
// it is asked again on its own, a second after the first deadline, two after
// the second, and so on, until the engine answers.
test("a read past its deadline with the socket up is asked again on its own, backing off", async () => {
  act(() => store.setConnected(true));
  let calls = 0;
  vi.stubGlobal(
    "fetch",
    vi.fn((_: RequestInfo | URL, init?: RequestInit) => {
      began.push(Date.now());
      calls++;
      if (calls <= 3) {
        // NEVER ANSWERED: only the request's own deadline ends it.
        return new Promise<Response>((_resolve, reject) => {
          init?.signal?.addEventListener("abort", () =>
            reject(new DOMException("aborted", "AbortError")),
          );
        });
      }
      return Promise.resolve(new Response(JSON.stringify({ ok: true }), { status: 200 }));
    }),
  );
  const { result } = mount((signal) => rest.get("/thing", signal));
  await wait(REQUEST_TIMEOUT_MS);
  expect(result.current.failure).toEqual({ error: "unanswered", refusal: null });
  expect(result.current.loading).toBe(false);

  await wait(10 * REQUEST_TIMEOUT_MS);
  // Thirty seconds of deadline, then one, two and four of backoff.
  expect(gaps()).toEqual([31, 32, 34]);
  expect(result.current.data).toEqual({ ok: true });
  expect(result.current.failure).toBeNull();
});

// AND WITH THE SOCKET DOWN TOO: the banner says the screen asks again on its
// own, which must not depend on an event that may never come. The socket
// coming back asks at once, ahead of the backoff.
test("a read nobody answered backs off with the socket down, and asks the moment it is back", async () => {
  const unreachable = new RestError(0, { error: "unreachable" });
  const { result } = mount(
    scripted([fails(unreachable), fails(unreachable), fails(unreachable), answers("held")]),
  );
  await wait(0);
  await wait(3_500);
  expect(gaps()).toEqual([1, 2]);
  expect(result.current.failure?.error).toBe("unanswered");

  // Half a second into a four-second wait: only the socket explains a read now.
  act(() => store.setConnected(true));
  await wait(0);
  expect(began).toHaveLength(4);
  expect(result.current.data).toBe("held");
});

// THE COUNT STARTS AGAIN AFTER ANY ANSWER: a node that recovered and then
// dropped one request is not asked at the end of an earlier run's backoff.
test("the backoff starts again after an answer", async () => {
  const unreachable = new RestError(0, { error: "unreachable" });
  const { result } = mount(
    scripted([fails(unreachable), fails(unreachable), answers(1), fails(unreachable), answers(2)]),
  );
  await wait(0);
  await wait(3_000);
  expect(result.current.data).toBe(1);
  act(() => result.current.refetch());
  await wait(1_000);
  expect(gaps()).toEqual([1, 2, 0, 1]);
  expect(result.current.data).toBe(2);
});

// THE LAST ANSWER IS KEPT through a failure — a re-read that failed is no
// claim about what it failed to read — and DROPPED on a refusal on authority,
// because a reader refused is shown nothing they were refused.
test.each([
  ["a fault", new RestError(500, { error: "internal_error" }), "query_failed", "held"],
  ["a read nobody answered", new RestError(0, { error: "unreachable" }), "unanswered", "held"],
  [
    "the engine's 503",
    new RestError(503, { error: "identity_unavailable" }, 2),
    "unavailable",
    "held",
  ],
  ["a refusal on authority", new RestError(403, { error: "unauthorized" }), "unauthorized", null],
])("after %s, the answer held is what the screen still shows", async (_, err, code, kept) => {
  const { result } = mount(scripted([answers("held"), fails(err)]));
  await wait(0);
  expect(result.current.data).toBe("held");
  act(() => result.current.refetch());
  await wait(0);
  expect(result.current.failure?.error).toBe(code);
  expect(result.current.error).toBe(err);
  expect(result.current.data).toBe(kept);
});

// THE ENGINE'S OWN HINT, and at zero NOTHING: the engine is saying waiting
// will not change it, so the read waits for a person.
test("a 503 the engine wrote is asked again when it says, and never at its zero", async () => {
  const { result } = mount(
    scripted([
      fails(new RestError(503, { error: "identity_unavailable" }, 12)),
      fails(new RestError(503, { error: "log_full" })),
    ]),
  );
  await wait(0);
  await wait(600_000);
  expect(gaps()).toEqual([12]);
  expect(result.current.failure?.error).toBe("unavailable");
});

// A FAILURE WITH NO HINT KEEPS THE CADENCE OF WHAT IS HELD: a pass that was
// running when it was last read is still followed, and nothing held asks
// nothing.
test("a fault waits the cadence the answer held sets", async () => {
  const fault = new RestError(500, { error: "internal_error" });
  mount(scripted([answers("running"), fails(fault), answers("done"), fails(fault)]), {
    cadence: (held) => (held === "running" ? 4_000 : null),
  });
  await wait(0);
  await wait(60_000);
  // Followed at four seconds through the fault, and not at all once it ended.
  expect(gaps()).toEqual([4, 4]);
});

// A READ DISABLED ASKS NOTHING AND HOLDS NOTHING — and one in flight when it
// was disabled writes nothing and arms nothing when it lands, which is what a
// closed peek used to do for as long as the node refused.
test("a disabled read asks nothing, and the one in flight when it closed arms nothing", async () => {
  let land: (value: never) => void = () => {};
  let signal: AbortSignal | null = null;
  const read = vi.fn((s: AbortSignal) => {
    signal = s;
    return new Promise<never>((_resolve, reject) => {
      land = () => reject(new RestError(503, { error: "identity_unavailable" }, 2));
    });
  });
  const view = mount(read);
  expect(read).toHaveBeenCalledTimes(1);

  view.rerender({ k: "/thing", o: { enabled: false } });
  expect(signal!.aborted).toBe(true);
  await act(async () => land(undefined as never));
  await wait(60_000);
  expect(read).toHaveBeenCalledTimes(1);
  expect(view.result.current).toMatchObject({ data: null, loading: false, failure: null });

  // NOR DOES THE SOCKET COMING BACK, OR A REFETCH: nothing is being read.
  act(() => store.setConnected(true));
  act(() => view.result.current.refetch());
  await wait(0);
  expect(read).toHaveBeenCalledTimes(1);
});

// THE ANSWER BELONGS TO ITS KEY: what one pass's read held is not a reading of
// the next one, and a re-read under a new key starts from nothing.
test("a new key starts from nothing and is read at once", async () => {
  const read = vi.fn((s: AbortSignal) => {
    void s;
    return Promise.resolve(`answer ${read.mock.calls.length}`);
  });
  const view = mount(read, undefined, "/runs/r1");
  await wait(0);
  expect(view.result.current.data).toBe("answer 1");

  view.rerender({ k: "/runs/r2", o: undefined });
  expect(view.result.current).toMatchObject({ data: null, loading: true });
  await wait(0);
  expect(view.result.current.data).toBe("answer 2");
});

// AND NOT ONE RENDER SAYS OTHERWISE. The key's effect resets what is held, a
// render late: the render that first carried a new key handed back the last
// key's answer, finished, and a read closed or opened again did the same — a
// closed rail still holding what it read, and one opened again drawn as a read
// that had answered nothing. Every render is recorded here, not only the one
// after the effects have settled, because the one before them is drawn too.
test("no render under a new key, or of a read closed or opened, holds another's answer", async () => {
  const seen: Array<{ k: string; data: unknown; loading: boolean }> = [];
  const view = renderHook(
    ({ k, enabled }: { k: string; enabled: boolean }) => {
      const reading = useRestRead(k, () => Promise.resolve(`answer for ${k}`), { enabled });
      seen.push({ k, data: reading.data, loading: reading.loading });
      return reading;
    },
    { wrapper, initialProps: { k: "/runs/r1", enabled: true } },
  );
  await wait(0);
  expect(view.result.current.data).toBe("answer for /runs/r1");

  seen.length = 0;
  view.rerender({ k: "/runs/r2", enabled: true });
  expect(seen.length).toBeGreaterThan(0);
  expect(seen.every((r) => r.data === null && r.loading)).toBe(true);
  await wait(0);
  expect(view.result.current.data).toBe("answer for /runs/r2");

  seen.length = 0;
  view.rerender({ k: "/runs/r2", enabled: false });
  expect(seen.length).toBeGreaterThan(0);
  expect(seen.every((r) => r.data === null && !r.loading)).toBe(true);

  seen.length = 0;
  view.rerender({ k: "/runs/r2", enabled: true });
  expect(seen.length).toBeGreaterThan(0);
  expect(seen.every((r) => r.data === null && r.loading)).toBe(true);
  await wait(0);
  expect(view.result.current.data).toBe("answer for /runs/r2");
});

// AND SO DOES ITS CADENCE: a failure under the new key is waited by what THAT
// key holds — nothing yet — never by the pass the last key was following.
test("a new key's first failure is not waited by the last key's cadence", async () => {
  const fault = new RestError(500, { error: "internal_error" });
  const read = scripted([answers("running"), fails(fault)]);
  const view = renderHook(
    ({ k }: { k: string }) =>
      useRestRead(k, read, { cadence: (held) => (held === "running" ? 4_000 : null) }),
    { wrapper, initialProps: { k: "/runs/r1" } },
  );
  await wait(0);
  view.rerender({ k: "/runs/r2" });
  await wait(60_000);
  expect(began).toHaveLength(2);
  expect(view.result.current.failure?.error).toBe("query_failed");
});

// A READ A PERSON'S WRITE ASKED FOR holds `loading` until it answers; a read
// nobody asked for never touches it, so nothing on screen blinks.
test("only a blank refetch holds loading", async () => {
  let release: () => void = () => {};
  const read = vi.fn(
    () =>
      new Promise<string>((resolve) => {
        release = () => resolve("again");
      }),
  );
  const view = mount(read);
  act(() => release());
  await wait(0);
  expect(view.result.current.loading).toBe(false);

  act(() => view.result.current.refetch());
  expect(view.result.current.loading).toBe(false);
  act(() => release());
  await wait(0);

  act(() => view.result.current.refetch(true));
  expect(view.result.current.loading).toBe(true);
  act(() => release());
  await wait(0);
  expect(view.result.current.loading).toBe(false);
});

// A SUPERSEDED READ WRITES NOTHING: two in flight — a write's re-read and the
// tab coming back — answer in whatever order, and only the later ask counts.
test("an answer a newer read overtook is dropped, and its request ended", async () => {
  const pending: Array<{ resolve: (v: string) => void; signal: AbortSignal }> = [];
  const read = (signal: AbortSignal) =>
    new Promise<string>((resolve) => pending.push({ resolve, signal }));
  const view = mount(read);
  act(() => view.result.current.refetch());
  expect(pending[0]!.signal.aborted).toBe(true);
  act(() => pending[1]!.resolve("newer"));
  await wait(0);
  act(() => pending[0]!.resolve("older"));
  await wait(0);
  expect(view.result.current.data).toBe("newer");
});

// AND WHERE ASKED FOR, THE TAB COMING BACK asks — never a tab going away.
test("the tab coming back asks again where the read asked for it", async () => {
  const read = vi.fn(() => Promise.resolve("x"));
  mount(read, { refetchOnFocus: true });
  await wait(0);
  Object.defineProperty(document, "visibilityState", { value: "hidden", configurable: true });
  act(() => void document.dispatchEvent(new Event("visibilitychange")));
  expect(read).toHaveBeenCalledTimes(1);
  Object.defineProperty(document, "visibilityState", { value: "visible", configurable: true });
  act(() => void document.dispatchEvent(new Event("visibilitychange")));
  expect(read).toHaveBeenCalledTimes(2);
});

// AN ANSWER BELONGS TO THE KEY THAT ASKED FOR IT: a read under a key the
// screen has left is aborted, and if it lands after the new key's answer it
// does not put the old key's answer on a screen showing the new one.
test("an older key's answer landing late is dropped, and its request ended", async () => {
  const pending: Array<{ resolve: (v: string) => void; signal: AbortSignal }> = [];
  const read = (signal: AbortSignal) =>
    new Promise<string>((resolve) => pending.push({ resolve, signal }));
  const view = mount(read, undefined, "/a");
  view.rerender({ k: "/b", o: undefined });
  expect(pending).toHaveLength(2);
  expect(pending[0]!.signal.aborted).toBe(true);
  act(() => pending[1]!.resolve("b"));
  await wait(0);
  act(() => pending[0]!.resolve("a"));
  await wait(0);
  expect(view.result.current.data).toBe("b");
});

// A SCREEN THAT HAS GONE has nothing to write into: its read is ended, and an
// answer that lands anyway writes nothing and arms nothing.
test("an unmounted screen's read is ended and writes nothing", async () => {
  let land: (value: string) => void = () => {};
  let signal: AbortSignal | null = null;
  const read = vi.fn((s: AbortSignal) => {
    signal = s;
    return new Promise<string>((resolve) => {
      land = resolve;
    });
  });
  const view = mount(read);
  const before = view.result.current;
  view.unmount();
  expect(signal!.aborted).toBe(true);
  await act(async () => land("late"));
  await wait(60_000);
  expect(view.result.current).toBe(before);
  expect(read).toHaveBeenCalledTimes(1);
});

// A READ THAT THROWS SOMETHING ELSE failed to make sense of what it was given
// — a transform over a body shaped unlike its type — which is a failure the
// screen draws, never nothing.
test("a read that throws something other than a refusal is a failure", async () => {
  const thrown = new TypeError("rows is not iterable");
  const { result } = mount(scripted([fails(thrown)]));
  await wait(0);
  expect(result.current.failure?.error).toBe("query_failed");
  expect(result.current.error).toBe(thrown);
  expect(result.current.loading).toBe(false);
});

// A NEWER READ SUPERSEDES THE WAIT AN OLDER ONE ARMED: the engine's hint is a
// wait for the read it answered, and a read that has since answered needs it
// no more.
test("a newer read cancels the wait an older refusal armed", async () => {
  const { result } = mount(
    scripted([fails(new RestError(503, { error: "identity_unavailable" }, 5)), answers("rows")]),
  );
  await wait(0);
  act(() => result.current.refetch());
  await wait(0);
  expect(result.current.data).toBe("rows");
  await wait(10_000);
  expect(began).toHaveLength(2);
});

// A READ ON ITS OWN CADENCE IS QUIET: what is on screen stays, and nothing
// blinks into a skeleton to say nothing new.
test("a cadence re-read keeps what is on screen", async () => {
  const seen: boolean[] = [];
  const read = scripted([answers("one"), answers("two")]);
  const view = renderHook(
    () => {
      const reading = useRestRead("/runs", read, { cadence: () => 4_000 });
      seen.push(reading.loading);
      return reading;
    },
    { wrapper },
  );
  await wait(0);
  expect(view.result.current.data).toBe("one");
  seen.length = 0;
  await wait(4_000);
  expect(began).toHaveLength(2);
  expect(view.result.current.data).toBe("two");
  expect(seen.every((loading) => !loading)).toBe(true);
});

// A PAGE LOAD IS NOT A RECONNECT: the socket first coming up under a read
// that answered over HTTP asks nothing more, and a reconnect after it does.
test("a read that answered is not asked again when the socket first comes up", async () => {
  const read = vi.fn(() => Promise.resolve("rows"));
  mount(read);
  await wait(0);
  act(() => store.setConnected(true));
  await wait(0);
  expect(read).toHaveBeenCalledTimes(1);
  act(() => store.setConnected(false));
  act(() => store.setConnected(true));
  await wait(0);
  expect(read).toHaveBeenCalledTimes(2);
});
