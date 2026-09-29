/**
 * One guarded read of the org chart, as the five outcomes it can have.
 *
 * What these protect: a chart read is never a nullable value. "Not in the
 * chart", "you may not read it", "the engine could not answer", "not back yet"
 * and "here it is" are different facts, a refusal REPLACES what an earlier
 * read showed rather than sitting beside it, and an answer about one object is
 * never shown under another's name.
 */

import { cleanup, renderHook, waitFor } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { chartSeatPath, chartUnitPath, useChartRead, WITH_RUNTIME } from "./chartReads.ts";

afterEach(() => {
  cleanup();
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
  const { result } = renderHook(() =>
    useChartRead<{ seat: { handle: string } }>(chartSeatPath("ceo"), WITH_RUNTIME, 1),
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
    "a 404 is an object the chart does not hold",
    json({ error: "not_found" }, 404),
    { state: "absent" },
  ],
  [
    "a 403 is a refusal carrying the grants and the rule's reason",
    json({ error: "unauthorized", reason: "no_grant", grants: ["config:read"] }, 403),
    { state: "refused", grants: ["config:read"], reason: "no_grant" },
  ],
  [
    "a 401 is a refusal with no reason, since nothing the engine accepted was presented",
    json({ error: "unauthenticated", reason: "no_credential" }, 401),
    { state: "refused", grants: [], reason: "" },
  ],
  [
    "a 503 is the engine not answering, never a refusal of the reader",
    json({ error: "unavailable" }, 503),
    { state: "failed" },
  ],
])("%s", async (_name, answer, expected) => {
  engine(() => answer.clone());
  const { result } = renderHook(() => useChartRead(chartUnitPath("engineering"), undefined, 1));
  await waitFor(() => expect(result.current.state).not.toBe("unread"));
  expect(result.current).toEqual(expected);
});

test("no path asks nothing and claims nothing", async () => {
  const asked = engine(() => json({}));
  const { result } = renderHook(() => useChartRead(null, WITH_RUNTIME, 1));
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
    { initialProps: { refresh: 1 } },
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
    { initialProps: { handle: "ceo" } },
  );
  await waitFor(() => expect(result.current.state).toBe("read"));
  rerender({ handle: "dev" });
  expect(result.current).toEqual({ state: "unread" });
  release?.();
  await waitFor(() => expect(result.current.state).toBe("read"));
  expect(result.current.state === "read" && result.current.value.seat.handle).toBe("dev");
});
