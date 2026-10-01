/**
 * When a query asks the engine again.
 *
 * The interval is for answers that drift on their own. It is the wrong tool
 * for an answer a person changes SOMEWHERE ELSE — setting an integration up
 * means leaving for the third-party app, doing something there, and coming
 * back — and coming back is a stronger signal than any cadence can be.
 */

import { cleanup, renderHook } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { useQuery } from "./useQuery.ts";
import { useClient, useConnection } from "./store-hooks.ts";

vi.mock("./store-hooks.ts", () => ({
  useClient: vi.fn(),
  useConnection: vi.fn(),
}));

/** visible drives the tab between hidden and visible, as a browser does. */
function visible(is: boolean) {
  Object.defineProperty(document, "visibilityState", {
    configurable: true,
    get: () => (is ? "visible" : "hidden"),
  });
  document.dispatchEvent(new Event("visibilitychange"));
}

/** asked counts the queries one render made, and answers them all. */
function asked() {
  const query = vi.fn().mockResolvedValue({});
  vi.mocked(useClient).mockReturnValue({ socket: { query } } as never);
  vi.mocked(useConnection).mockReturnValue({ connected: true } as never);
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
  await vi.waitFor(() => expect(query).toHaveBeenCalledTimes(1));

  visible(false);
  visible(true);
  await vi.waitFor(() => expect(query).toHaveBeenCalledTimes(2));
});

// AND A HIDDEN TAB DOES NOT.
//
// The event fires in both directions and only one of them is a person
// arriving to read the answer.
test("going away does not ask", async () => {
  const query = asked();
  renderHook(() => useQuery("integrations", undefined, { refetchOnFocus: true }));
  await vi.waitFor(() => expect(query).toHaveBeenCalledTimes(1));

  visible(false);
  await new Promise((r) => setTimeout(r, 10));
  expect(query).toHaveBeenCalledTimes(1);
});

// AND A QUERY THAT DID NOT ASK FOR IT IS UNTOUCHED.
//
// Default off, because most answers are not changed from outside the screen
// and a tab switch is not a reason to re-ask every one of them.
test("focus is opt-in", async () => {
  const query = asked();
  renderHook(() => useQuery("integrations"));
  await vi.waitFor(() => expect(query).toHaveBeenCalledTimes(1));

  visible(false);
  visible(true);
  await new Promise((r) => setTimeout(r, 10));
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
    min_position: "CREWLET_PAGES_LOG@1:77",
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
  await vi.waitFor(() => expect(query).toHaveBeenCalledTimes(2));
  session.written("tracker", "CREWLET_TRACKER_LOG@1:5", []);
  await vi.waitFor(() => expect(query).toHaveBeenCalledTimes(3));
  await new Promise((r) => setTimeout(r, 10));
  expect(query).toHaveBeenCalledTimes(3);
  expect(query.mock.calls[2]).toEqual([
    "work_items",
    { read_level: "session", min_position: "CREWLET_TRACKER_LOG@1:5" },
  ]);
});
