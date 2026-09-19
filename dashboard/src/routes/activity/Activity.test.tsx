/**
 * What the event log asks the engine, and how often.
 *
 * This screen is the one place in the dashboard where a time window is
 * deliberately NOT aligned to a bucket — a list's newest row is the newest row
 * — so its two edges are the clock itself, moving once a second under the
 * shared ticker. Everything else on the screen has to be keyed on the window's
 * IDENTITY rather than on those instants, and nothing on screen says when that
 * stops being true: a log that silently re-pages itself looks exactly like a
 * log that works, right up until a reader presses "Load older" and watches the
 * rows they fetched vanish a second later.
 *
 * So the invariant is counted rather than rendered: one window, one first page,
 * one axis query, however long the tab stays open.
 */

import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { Activity } from "./Activity.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
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
  vi.unstubAllGlobals();
});

/** Mount the log over a socket that counts what it is asked, and for what. */
function mount() {
  const asked: { what: string; params: Record<string, unknown> }[] = [];
  const store = new Store();
  const socket = new LiveSocket(store);
  (
    socket as unknown as {
      query: (what: string, params?: Record<string, unknown>) => Promise<unknown>;
    }
  ).query = (what: string, params: Record<string, unknown> = {}) => {
    asked.push({ what, params });
    if (what === "events") {
      return Promise.resolve({
        events: [
          {
            id: "e-1",
            type: "task_created",
            category: "task",
            source: "engine",
            actor: "CEO",
            summary: "opened a task",
            timestamp: new Date(Date.now() - 60_000).toISOString(),
            failed: false,
          },
        ],
        next: { before_time: new Date(Date.now() - 60_000).toISOString(), before_id: "e-1" },
        exhausted: false,
      });
    }
    if (what === "event_series") {
      return Promise.resolve({
        bucket: "hour",
        since: String(params.since ?? ""),
        until: String(params.until ?? ""),
        bars: [],
        total: 0,
        by_category: {},
      });
    }
    return Promise.resolve({});
  };
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Activity />
      </Router>
    </ClientContext.Provider>,
  );
  return { asked, count: (what: string) => asked.filter((a) => a.what === what).length };
}

// THE CLOCK IS NOT A NEW WINDOW.
//
// `useTimeRange(..., false)` recomputes `since`/`until` from `Date.now()` on
// every tick of the shared ticker, so both the paging reset and the axis query
// used to change identity once a second: the reset dropped `older`, `cursor`
// and `exhausted` and the guarded first-page effect immediately asked for page
// one again, while the axis re-asked the engine for bars it had just drawn.
// One tab, two queries a second, and a reader's fetched history gone.
test("a passing second is not a new window: the log pages once and asks its axis once", async () => {
  const { count } = mount();
  // The mount's own round trips, and nothing else.
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
  expect(count("events")).toBe(1);
  expect(count("event_series")).toBe(1);

  // Five ticks of the shared clock — five re-renders with five fresh pairs of
  // instants behind them.
  await act(async () => {
    await vi.advanceTimersByTimeAsync(5_000);
  });
  expect(count("events")).toBe(1);
  expect(count("event_series")).toBe(1);
});

// AN EMPTY LIST IS A CLAIM, AND A REFUSED PAGE IS NOT ONE.
//
// The first page of a window is asked for at mount, so an empty-state rendered
// on "no rows and not loading" fired on every load and, worse, stayed up when
// the page came back refused: the footer drew the engine's refusal and the body
// above it told the reader their company had published nothing. Three states,
// one of which is an answer.
test("a refused first page is not a company that has published nothing", async () => {
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what: string) =>
    what === "events" ? Promise.reject(new Error("query_failed")) : new Promise(() => {});
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Activity />
      </Router>
    </ClientContext.Provider>,
  );
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
  expect(screen.getByText(/The engine tried to answer and failed/)).toBeTruthy();
  expect(screen.queryByText("Nothing has been published yet")).toBeNull();
});

// AND A PAGE STILL IN FLIGHT IS NOT ONE EITHER.
test("a first page still in flight is not a company that has published nothing", () => {
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: () => Promise<unknown> }).query = () => new Promise(() => {});
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Activity />
      </Router>
    </ClientContext.Provider>,
  );
  expect(screen.queryByText("Nothing has been published yet")).toBeNull();
  expect(screen.getByText("Loading…")).toBeTruthy();
});

// AND THE CONTROL: a window that answered with no rows says so.
test("a window the engine answered with no rows says the log is empty", async () => {
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what: string) =>
    what === "events"
      ? Promise.resolve({ events: [], next: null, exhausted: true })
      : new Promise(() => {});
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Activity />
      </Router>
    </ClientContext.Provider>,
  );
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
  expect(screen.getByText("Nothing has been published yet")).toBeTruthy();
});

// AND THE AXIS ASKS ON WHOLE BUCKETS.
//
// The other half of the same fix, and the half a call count cannot see: the
// edges handed to `event_series` are snapped OUT to the bucket the chart draws
// in, which is both what keeps the query's identity still and what makes the
// bars cover the window the badge names. Asked with the raw clock they carried
// a millisecond, which is what re-keyed the query every tick.
test("the axis is asked for whole buckets, never for the clock's own millisecond", async () => {
  const { asked } = mount();
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
  const series = asked.find((a) => a.what === "event_series");
  expect(series).toBeTruthy();
  const since = Date.parse(String(series!.params.since));
  const until = Date.parse(String(series!.params.until));
  // The default window is `1d`, whose bucket is the hour.
  expect(series!.params.bucket).toBe("hour");
  expect(until % 3_600_000).toBe(0);
  expect(since % 3_600_000).toBe(0);
  expect(until - since).toBe(24 * 3_600_000);
});
