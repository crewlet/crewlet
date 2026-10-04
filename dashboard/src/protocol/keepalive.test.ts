/**
 * The keepalive: a tab that only listens to the socket still makes the engine
 * hear from it once per re-issue interval, so the session's idle deadline
 * moves while somebody is looking at the page.
 *
 * What these protect, in the order they cost when they go: a listening tab is
 * kept signed in WHETHER OR NOT IT IS VISIBLE (an open live view is somebody
 * using the dashboard); a tab whose socket is down asks nothing (there is no
 * live view to keep); and a tab another request already renewed asks nothing
 * either, because that answer re-issued the cookie just as well.
 */

import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { SESSION_KEEPALIVE_MS, SessionKeepAlive } from "./keepalive.ts";
import { rest } from "./rest.ts";

/** Every path the engine was asked, in order. */
let asked: string[] = [];

beforeEach(() => {
  asked = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      asked.push(new URL(String(input), "http://engine.test").pathname);
      return new Response(JSON.stringify({ login: "jane.doe" }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      });
    }),
  );
});

afterEach(() => {
  vi.unstubAllGlobals();
  Object.defineProperty(document, "visibilityState", { configurable: true, value: "visible" });
});

/** Every request in flight lands. */
const settle = () => new Promise((resolve) => setTimeout(resolve, 0));

/** An instant a whole interval past anything this module has heard answered. */
const later = () => Date.now() + SESSION_KEEPALIVE_MS + 1;

test("an open socket nothing else renewed reads the session, hidden or not", async () => {
  Object.defineProperty(document, "visibilityState", { configurable: true, value: "hidden" });
  new SessionKeepAlive({ connected: true }).tick(later());
  await settle();
  expect(asked).toEqual(["/auth/session"]);
});

test("a socket that is down asks nothing", async () => {
  new SessionKeepAlive({ connected: false }).tick(later());
  await settle();
  expect(asked).toEqual([]);
});

test("a request answered inside the interval already renewed the session", async () => {
  await rest.get("/health");
  asked = [];
  new SessionKeepAlive({ connected: true }).tick(Date.now());
  await settle();
  expect(asked).toEqual([]);
});

// THE CADENCE IS THE ENGINE'S RE-ISSUE INTERVAL: sooner re-issues nothing,
// later leaves an interval unrenewed.
test("ticks once per re-issue interval while started, and not after stop", () => {
  vi.useFakeTimers();
  try {
    const keep = new SessionKeepAlive({ connected: true });
    const tick = vi.spyOn(keep, "tick").mockImplementation(() => {});
    keep.start();
    vi.advanceTimersByTime(SESSION_KEEPALIVE_MS - 1);
    expect(tick).not.toHaveBeenCalled();
    vi.advanceTimersByTime(1);
    expect(tick).toHaveBeenCalledTimes(1);
    keep.stop();
    vi.advanceTimersByTime(SESSION_KEEPALIVE_MS * 3);
    expect(tick).toHaveBeenCalledTimes(1);
  } finally {
    vi.useRealTimers();
  }
});
