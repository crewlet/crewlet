/**
 * One guarded read of the org chart, as the five outcomes it can have.
 *
 * What these protect: a chart read is never a nullable value. "Not in the
 * chart", "you may not read it", "the engine could not answer", "not back yet"
 * and "here it is" are different facts, a refusal REPLACES what an earlier
 * read showed rather than sitting beside it, and an answer about one object is
 * never shown under another's name. And a read that failed is asked again on
 * its own where waiting can clear it, rather than only when the next org push
 * happens to arrive.
 */

import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { chartSeatPath, chartUnitPath, useChartRead, WITH_RUNTIME } from "./chartReads.ts";
import { ClientContext } from "./store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";

let store = new Store();

function wrapper({ children }: { children: ReactNode }) {
  return (
    <ClientContext.Provider value={{ store, socket: new LiveSocket(store) }}>
      {children}
    </ClientContext.Provider>
  );
}

beforeEach(() => {
  store = new Store();
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });

/** Installs `fetch`, answering by path; every request is recorded. */
function engine(answer: (path: string) => Response | Promise<Response>) {
  const asked: URL[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = new URL(String(input));
      asked.push(url);
      return answer(url.pathname);
    }),
  );
  return asked;
}

test("a read answers the value, and asks with the query it was given", async () => {
  const asked = engine(() => json({ seat: { handle: "ceo" }, runtime: true }));
  const { result } = renderHook(
    () => useChartRead<{ seat: { handle: string } }>(chartSeatPath("ceo"), WITH_RUNTIME, 1),
    { wrapper },
  );
  expect(result.current).toEqual({ state: "unread" });
  await waitFor(() => expect(result.current.state).toBe("read"));
  expect(result.current.state === "read" && result.current.value.seat.handle).toBe("ceo");
  expect(asked[0]?.pathname).toBe("/chart/seats/ceo");
  expect(asked[0]?.searchParams.get("runtime")).toBe("true");
});

test("an address is escaped into its path, never spliced", () => {
  expect(chartSeatPath("a/b")).toBe("/chart/seats/a%2Fb");
  expect(chartUnitPath("eng ops")).toBe("/chart/units/eng%20ops");
});

// FIVE OUTCOMES. Each failure is its own answer, and none of them is "empty".
test.each([
  [
    "a 404 the engine wrote is an object the chart does not hold",
    () => json({ error: "not_found" }, 404),
    { state: "absent" },
  ],
  [
    "a 403 is a refusal carrying the grants and the rule's reason",
    () => json({ error: "unauthorized", reason: "no_grant", grants: ["config:read"] }, 403),
    { state: "refused", grants: ["config:read"], reason: "no_grant" },
  ],
  [
    "a 401 is a refusal with no reason, since nothing the engine accepted was presented",
    () => json({ error: "unauthenticated", reason: "no_credential" }, 401),
    { state: "refused", grants: [], reason: "" },
  ],
  [
    "a 503 is the engine not answering, never a refusal of the reader",
    () => json({ error: "unavailable" }, 503),
    { state: "failed", failure: { error: "unavailable" } },
  ],
  // NOT `absent`: a gateway wrote this 404, and it says nothing about the
  // chart. Read as the engine's, it told a reader the company had no such seat.
  [
    "a 404 a gateway wrote is nothing coming back, never an object the chart lacks",
    () => new Response("<html>Not Found</html>", { status: 404 }),
    { state: "failed", failure: { error: "unanswered", refusal: null } },
  ],
])("%s", async (_name, answer, expected) => {
  engine(() => answer());
  const { result } = renderHook(() => useChartRead(chartUnitPath("engineering"), undefined, 1), {
    wrapper,
  });
  await waitFor(() => expect(result.current.state).not.toBe("unread"));
  expect(result.current).toMatchObject(expected);
});

test("no path asks nothing and claims nothing", async () => {
  const asked = engine(() => json({}));
  const { result } = renderHook(() => useChartRead(null, WITH_RUNTIME, 1), { wrapper });
  await new Promise((resolve) => setTimeout(resolve, 10));
  expect(result.current).toEqual({ state: "unread" });
  expect(asked).toHaveLength(0);
});

// A REFUSAL REPLACES WHAT WAS READ: a guarded answer a reader has since lost
// the right to must not stay on screen.
test("a refused re-read replaces the value an earlier read showed", async () => {
  let refuse = false;
  engine(() =>
    refuse
      ? json({ error: "unauthorized", reason: "no_grant", grants: ["config:read"] }, 403)
      : json({ unit: { key: "engineering" } }),
  );
  const { result, rerender } = renderHook(
    ({ refresh }) => useChartRead(chartUnitPath("engineering"), WITH_RUNTIME, refresh),
    { wrapper, initialProps: { refresh: 1 } },
  );
  await waitFor(() => expect(result.current.state).toBe("read"));
  refuse = true;
  rerender({ refresh: 2 });
  await waitFor(() => expect(result.current.state).toBe("refused"));
  // The control: the same refresh identity asks nothing new.
  refuse = false;
  rerender({ refresh: 2 });
  await new Promise((resolve) => setTimeout(resolve, 10));
  expect(result.current.state).toBe("refused");
});

// AN ANSWER ABOUT ANOTHER OBJECT IS NO ANSWER: moving from one seat to the next
// reads `unread` until the new seat's answer arrives.
test("an answer about the previous object is never shown under the next one's path", async () => {
  let release: (() => void) | undefined;
  const held = new Promise<void>((resolve) => {
    release = resolve;
  });
  engine(async (path) => {
    if (path === "/chart/seats/dev") await held;
    return json({ seat: { handle: path.split("/").pop() } });
  });
  const { result, rerender } = renderHook(
    ({ handle }) => useChartRead<{ seat: { handle: string } }>(chartSeatPath(handle), undefined, 1),
    { wrapper, initialProps: { handle: "ceo" } },
  );
  await waitFor(() => expect(result.current.state).toBe("read"));
  rerender({ handle: "dev" });
  expect(result.current).toEqual({ state: "unread" });
  release?.();
  await waitFor(() => expect(result.current.state).toBe("read"));
  expect(result.current.state === "read" && result.current.value.seat.handle).toBe("dev");
});

// A READ THAT FAILED IS ASKED AGAIN ON ITS OWN, with the socket up and the
// chart quiet: no org push arrives, and the next push was the only thing that
// read it again. A request nothing answered backs off from a second; a `503`
// the engine wrote is asked when it says.
test.each([
  [
    "a request nothing answered",
    (): Response => {
      throw new TypeError("Failed to fetch");
    },
    1,
  ],
  [
    "a 503 the engine wrote",
    () =>
      new Response(JSON.stringify({ error: "unavailable" }), {
        status: 503,
        headers: { "Retry-After": "7" },
      }),
    7,
  ],
])("%s is read again on its own, with no push to prompt it", async (_name, failure, after) => {
  vi.useFakeTimers();
  act(() => store.setConnected(true));
  let calls = 0;
  engine(() => (++calls === 1 ? failure() : json({ unit: { key: "engineering" } })));
  const { result } = renderHook(
    () => useChartRead(chartUnitPath("engineering"), undefined, "the one push"),
    { wrapper },
  );
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
  expect(result.current.state).toBe("failed");
  await act(async () => {
    await vi.advanceTimersByTimeAsync(after * 1_000 - 1);
  });
  expect(calls).toBe(1);
  await act(async () => {
    await vi.advanceTimersByTimeAsync(1);
  });
  expect(calls).toBe(2);
  expect(result.current.state).toBe("read");
});

// AND A PUSH STILL ASKS AT ONCE, without blanking what is drawn while it does.
test("an org push reads again, keeping the answer on screen until the new one lands", async () => {
  let release: (() => void) | undefined;
  let calls = 0;
  engine(async () => {
    calls++;
    if (calls === 2) {
      await new Promise<void>((resolve) => {
        release = resolve;
      });
    }
    return json({ unit: { key: `read ${calls}` } });
  });
  const { result, rerender } = renderHook(
    ({ refresh }) =>
      useChartRead<{ unit: { key: string } }>(chartUnitPath("e"), undefined, refresh),
    { wrapper, initialProps: { refresh: 1 } },
  );
  await waitFor(() => expect(result.current.state).toBe("read"));
  rerender({ refresh: 2 });
  await waitFor(() => expect(calls).toBe(2));
  expect(result.current.state === "read" && result.current.value.unit.key).toBe("read 1");
  release?.();
  await waitFor(() =>
    expect(result.current.state === "read" && result.current.value.unit.key).toBe("read 2"),
  );
});
