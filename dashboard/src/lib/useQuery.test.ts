/**
 * When a query asks the engine again.
 *
 * The interval is for answers that drift on their own. It is the wrong tool
 * for an answer a person changes SOMEWHERE ELSE — setting an integration up
 * means leaving for the third-party app, doing something there, and coming
 * back — and coming back is a stronger signal than any cadence can be.
 */

import { act, cleanup, renderHook } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { useQuery } from "./useQuery.ts";
import { useClient, useConnection } from "./store-hooks.ts";
import { eventually, settled } from "~/test/settle.ts";

vi.mock("./store-hooks.ts", () => ({
  useClient: vi.fn(),
  useConnection: vi.fn(),
}));

/**
 * visible drives the tab between hidden and visible, as a browser does —
 * INSIDE `act`, so everything the event causes (a refetch, the render it
 * schedules, the effect that asks) has happened when it returns, and a case
 * that asserts nothing was asked asserts it of a hook that has finished.
 */
function visible(is: boolean) {
  Object.defineProperty(document, "visibilityState", {
    configurable: true,
    get: () => (is ? "visible" : "hidden"),
  });
  act(() => {
    document.dispatchEvent(new Event("visibilitychange"));
  });
}

/** asked counts the queries one render made, and answers them all. */
function asked() {
  const query = vi.fn().mockResolvedValue({});
  vi.mocked(useClient).mockReturnValue({ socket: { query } } as never);
  vi.mocked(useConnection).mockReturnValue({ connected: true, accessRefused: null } as never);
  return query;
}

afterEach(() => {
  // EXPLICIT, because this tree does not configure auto-cleanup: a hook left
  // mounted keeps its visibilitychange listener, so the next test's toggle
  // fires every earlier test's query too. Written the wrong way first, and
  // the counts it produced looked exactly like the feature misbehaving.
  cleanup();
  vi.restoreAllMocks();
  visible(true);
});

// A TAB COMING BACK ASKS AGAIN.
//
// Measured without it: a GitHub App installed in about eight seconds, then a
// card still asking for the install, because the screen held what it had read
// before the operator left and its poll was a minute away.
test("a query that watches focus re-asks when the tab comes back", async () => {
  const query = asked();
  renderHook(() => useQuery("integrations", undefined, { refetchOnFocus: true }));
  await eventually(() => expect(query).toHaveBeenCalledTimes(1));

  visible(false);
  visible(true);
  await eventually(() => expect(query).toHaveBeenCalledTimes(2));
});

// AND A HIDDEN TAB DOES NOT.
//
// The event fires in both directions and only one of them is a person
// arriving to read the answer.
test("going away does not ask", async () => {
  const query = asked();
  renderHook(() => useQuery("integrations", undefined, { refetchOnFocus: true }));
  await eventually(() => expect(query).toHaveBeenCalledTimes(1));

  visible(false);
  await settled(() => query.mock.calls.length);
  expect(query).toHaveBeenCalledTimes(1);
});

// AND A QUERY THAT DID NOT ASK FOR IT IS UNTOUCHED.
//
// Default off, because most answers are not changed from outside the screen
// and a tab switch is not a reason to re-ask every one of them.
test("focus is opt-in", async () => {
  const query = asked();
  renderHook(() => useQuery("integrations"));
  await eventually(() => expect(query).toHaveBeenCalledTimes(1));

  visible(false);
  visible(true);
  await settled(() => query.mock.calls.length);
  expect(query).toHaveBeenCalledTimes(1);
});

// A WRITE FROM THIS TAB IS READ BACK AT ITS POSITION — every question that
// takes a floor names it from then on, a poll included, because a poll that
// came round a second after the write on a node that had not applied it would
// redraw the row as it was before the press.
test("a question that takes a floor names this tab's, and a caller's own freshness wins", async () => {
  const { withFloor } = await import("./useQuery.ts");
  const { session } = await import("~/protocol/session.ts");
  session.written("pages", "CREWLET_PAGES_LOG@1:77", []);
  expect(withFloor("page", JSON.stringify({ id: "p1" }))).toEqual({
    id: "p1",
    read_level: "session",
    min_position: ["CREWLET_PAGES_LOG@1:77"],
  });
  // A question with no floor to take is sent as it was asked.
  expect(withFloor("viewer", "{}")).toEqual({});
  // A caller that named its own freshness asked for something specific, and
  // a staleness bound beside `session` is a request the engine refuses.
  expect(withFloor("page", JSON.stringify({ id: "p1", max_lag_seq: 5 }))).toEqual({
    id: "p1",
    max_lag_seq: 5,
  });
});

test("a write from this tab asks the questions it moved again, and no others", async () => {
  const { session } = await import("~/protocol/session.ts");
  const query = asked();
  renderHook(() => useQuery("work_items", {}));
  renderHook(() => useQuery("pages", {}));
  await eventually(() => expect(query).toHaveBeenCalledTimes(2));
  act(() => session.written("tracker", "CREWLET_TRACKER_LOG@1:5", []));
  await settled(() => query.mock.calls.length);
  expect(query).toHaveBeenCalledTimes(3);
  expect(query.mock.calls[2]).toEqual([
    "work_items",
    { read_level: "session", min_position: ["CREWLET_TRACKER_LOG@1:5"] },
  ]);
});

// A QUESTION JUST ENABLED IS LOADING FROM ITS FIRST RENDER, before its effect
// has sent it. A read gated on the viewer's answer turns on in a render whose
// state still says it was off, and every render of that frame read "not
// loading, no data" — which a screen draws as answered with nothing. The
// CONTROL is the same question still disabled, which is not loading.
// Mutation: return the effect's state as it stands and the first render after
// enabling reads `loading: false`.
test("a question just enabled is loading before it is sent", () => {
  asked();
  const seen: boolean[] = [];
  const { rerender } = renderHook(
    ({ on }: { on: boolean }) => {
      const result = useQuery("integrations", undefined, { enabled: on });
      seen.push(result.loading);
      return result;
    },
    { initialProps: { on: false } },
  );
  expect(seen.at(-1)).toBe(false);
  const before = seen.length;
  rerender({ on: true });
  expect(seen[before]).toBe(true);
});

// AND AN ANSWER OF NULL IS AN ANSWER: the company document is null where no
// company is active, and a question that answered it is not still loading —
// read off "no data" instead, a node with no company drew a skeleton for ever.
// Mutation: decide "not yet sent" from the data and this never settles.
test("a question answered null is not loading", async () => {
  const query = vi.fn().mockResolvedValue(null);
  vi.mocked(useClient).mockReturnValue({ socket: { query } } as never);
  vi.mocked(useConnection).mockReturnValue({ connected: true, accessRefused: null } as never);
  const { result } = renderHook(() => useQuery("config"));
  expect(result.current.loading).toBe(true);
  await eventually(() => expect(result.current.loading).toBe(false));
  expect(result.current.data).toBeNull();
});
