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

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { Activity, dayKey } from "./Activity.tsx";
import { setZone } from "~/lib/prefs.ts";
import { fmtDate } from "~/lib/format.ts";
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
        failed: 0,
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

/**
 * A DAY HEADING GROUPS IN THE ZONE IT NAMES.
 *
 * The heading renders through `fmtDate`, which formats in the zone the reader
 * chose; the split that decides where a heading goes used to read
 * `getFullYear/getMonth/getDate` off a `Date`, which is the BROWSER's day. A
 * reader viewing a company from another zone then got rows grouped on one
 * boundary under a heading naming another.
 *
 * The suite runs in Europe/Berlin (see vitest.config.ts, which picks a zone
 * with an offset on purpose), so these three instants separate the two
 * readings cleanly at UTC+14:
 *
 *   09:00Z  Berlin 14 Sep   Kiritimati 14 Sep
 *   11:00Z  Berlin 14 Sep   Kiritimati 15 Sep
 *   +1d 05:00Z  Berlin 15 Sep   Kiritimati 15 Sep
 *
 * So the first pair must SPLIT and the second must GROUP, and the browser's
 * own day says the opposite of both.
 */
test("a day heading splits on the reader's chosen zone, not the browser's", () => {
  setZone("Pacific/Kiritimati");
  try {
    const before = dayKey("2026-09-14T09:00:00Z");
    const after = dayKey("2026-09-14T11:00:00Z");
    const nextMorning = dayKey("2026-09-15T05:00:00Z");
    // One Berlin day, two Kiritimati days: two headings.
    expect(before, "two Kiritimati days were drawn under one heading").not.toBe(after);
    // Two Berlin days, one Kiritimati day: one heading.
    expect(after, "one Kiritimati day was split across two headings").toBe(nextMorning);
  } finally {
    setZone("");
  }
});

// AND THE KEY IS THE LABEL, which is what makes the case above true by
// construction rather than by two pieces of date arithmetic agreeing. Asserted
// against `fmtDate` itself rather than against a format: the shape a date
// takes is the reader's `dates()` preference, and pinning one here would make
// this case fail the day somebody changes that default for reasons that have
// nothing to do with grouping.
test("the key a heading groups on is the string it draws", () => {
  setZone("Pacific/Kiritimati");
  try {
    for (const ts of ["2026-09-14T09:00:00Z", "2026-09-14T11:00:00Z", "2026-09-15T05:00:00Z"]) {
      expect(dayKey(ts)).toBe(fmtDate(ts));
    }
  } finally {
    setZone("");
  }
});

// ONE SEAT'S EVENTS, which is where a profile's "Events" lands. The engine
// narrows every page and the axis by the handle; the LIVE rows arrive on the
// socket for every seat, so the log narrows those itself — by the id the
// engine stamps on each row, never by the actor's display name.
test("a seat's log asks for that seat and shows only its live rows", async () => {
  location.hash = "#/live/events?seat=swe";
  const asked: { what: string; params: Record<string, unknown> }[] = [];
  const store = new Store();
  store.applyOrg({
    roles: [
      { name: "SWE", handle: "swe" },
      { name: "CTO", handle: "cto" },
    ],
  });
  store.applyAgents([
    { role: "SWE", handle: "swe", agent_id: "a-swe" },
    { role: "CTO", handle: "cto", agent_id: "a-cto" },
  ]);
  const live = (id: string, agentId: string, summary: string) => ({
    id,
    type: "task_created",
    category: "task",
    source: "engine",
    actor: "engine",
    summary,
    timestamp: new Date(Date.now() - 1_000).toISOString(),
    trace_id: "",
    span_id: "",
    parent_span_id: "",
    topic: "",
    failed: false,
    agent_id: agentId,
  });
  store.applyEvent(live("l-1", "a-swe", "swe opened a task"));
  store.applyEvent(live("l-2", "a-cto", "cto opened a task"));
  const socket = new LiveSocket(store);
  (
    socket as unknown as {
      query: (what: string, params?: Record<string, unknown>) => Promise<unknown>;
    }
  ).query = (what: string, params: Record<string, unknown> = {}) => {
    asked.push({ what, params });
    if (what === "events") return Promise.resolve({ events: [], next: null, exhausted: true });
    return Promise.resolve({ bucket: "hour", bars: [], total: 0, failed: 0, by_category: {} });
  };
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Activity />
      </Router>
    </ClientContext.Provider>,
  );
  await act(async () => {
    await Promise.resolve();
  });
  expect(asked.find((a) => a.what === "events")?.params.seat).toBe("swe");
  expect(asked.find((a) => a.what === "event_series")?.params.seat).toBe("swe");
  expect(screen.getByText("swe opened a task")).toBeTruthy();
  expect(screen.queryByText("cto opened a task")).toBeNull();
  // THE FILTER SAYS WHOSE LOG THIS IS, by name, and takes itself off.
  fireEvent.click(screen.getByRole("button", { name: /Only SWE's events/ }));
  expect(location.hash).not.toContain("seat=");
  expect(screen.getByText("cto opened a task")).toBeTruthy();
  location.hash = "#/";
});

/**
 * Mount the log at `hash` over a store holding `live` rows, answering the
 * pages and the axis with `answer`; returns what each question was asked.
 */
async function mountAt(
  hash: string,
  live: Record<string, unknown>[],
  answer: (what: string) => unknown = (what) =>
    what === "events"
      ? { events: [], next: null, exhausted: true }
      : { bucket: "hour", bars: [], total: 0, failed: 0, by_category: {} },
) {
  location.hash = hash;
  const asked: { what: string; params: Record<string, unknown> }[] = [];
  const store = new Store();
  for (const row of live) store.applyEvent(row as never);
  const socket = new LiveSocket(store);
  (
    socket as unknown as {
      query: (what: string, params?: Record<string, unknown>) => Promise<unknown>;
    }
  ).query = (what: string, params: Record<string, unknown> = {}) => {
    asked.push({ what, params });
    return Promise.resolve(answer(what));
  };
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Activity />
      </Router>
    </ClientContext.Provider>,
  );
  await act(async () => {
    await Promise.resolve();
  });
  return asked;
}

function liveRow(id: string, summary: string, over: Record<string, unknown> = {}) {
  return {
    id,
    type: "a2a_message_sent",
    category: "a2a",
    source: "engine",
    actor: "engine",
    summary,
    timestamp: new Date(Date.now() - 1_000).toISOString(),
    trace_id: "",
    span_id: "",
    parent_span_id: "",
    topic: "",
    failed: false,
    ...over,
  };
}

// ONE TRACE AND ONE CHANNEL ARE THE ENGINE'S FILTERS, asked by the wire's
// names, and the live rows are narrowed by the same values each row carries.
// A trace's "In the log" and a channel's "Read this channel's events" land
// here; before these, both carried the id as the text search, which matches
// no summary the engine writes, and opened on an empty log.
//
// Mutation: drop `trace_id` or `channel_id` from the filters, and the pages
// and the axis are asked for the whole log.
test("a trace and a channel narrow the pages, the axis and the live rows", async () => {
  let asked = await mountAt("#/live/events?trace=tr-1", [
    liveRow("l-1", "in the trace", { trace_id: "tr-1" }),
    liveRow("l-2", "another trace", { trace_id: "tr-2" }),
  ]);
  for (const what of ["events", "event_series"]) {
    expect(asked.find((a) => a.what === what)?.params.trace_id).toBe("tr-1");
  }
  expect(screen.getByText("in the trace")).toBeTruthy();
  expect(screen.queryByText("another trace")).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: /Only trace tr-1's events/ }));
  expect(location.hash).not.toContain("trace=");
  cleanup();

  asked = await mountAt("#/live/events?channel=ch-1", [
    liveRow("l-3", "on the channel", { channel_id: "ch-1" }),
    liveRow("l-4", "on another channel", { channel_id: "ch-2" }),
  ]);
  for (const what of ["events", "event_series"]) {
    expect(asked.find((a) => a.what === what)?.params.channel_id).toBe("ch-1");
  }
  expect(screen.getByText("on the channel")).toBeTruthy();
  expect(screen.queryByText("on another channel")).toBeNull();
  location.hash = "#/";
});

// "FAILURES ONLY" IS ASKED OF THE ENGINE, on the pages and the axis alike.
//
// It narrowed the rows this tab held, so the axis counted every event in the
// window while the list showed the failures among the newest hundred — and
// every older page came back unfiltered for the mark to hide.
//
// Mutation: drop `failed` from the filters, and neither question carries it.
test("failures only is a filter the engine applies", async () => {
  const asked = await mountAt("#/live/events?failed=true", [
    liveRow("l-5", "it broke", { failed: true }),
    liveRow("l-6", "it worked"),
  ]);
  for (const what of ["events", "event_series"]) {
    expect(asked.find((a) => a.what === what)?.params.failed).toBe("true");
  }
  expect(screen.getByText("it broke")).toBeTruthy();
  expect(screen.queryByText("it worked")).toBeNull();
  // THE END OF A FILTERED ANSWER IS THE FILTER'S, not the store's retention.
  //
  // Mutation: word the exhausted footer for the store again.
  await act(async () => {
    for (let i = 0; i < 4; i++) await Promise.resolve();
  });
  expect(screen.getByText("No older event in this window matches these filters.")).toBeTruthy();
  expect(screen.queryByText(/retained history/)).toBeNull();
  location.hash = "#/";
});

// A NODE THAT DID NOT ANSWER IS NAMED, off the page's coverage and the axis's.
test("a log missing a node names it", async () => {
  const coverage = {
    nodes: [
      { id: "node-a", answered: true, error: "" },
      { id: "node-b", answered: false, error: "no answer inside the read budget" },
    ],
    complete: false,
  };
  await mountAt("#/live/events", [], (what) =>
    what === "events"
      ? { events: [], next: null, exhausted: true, coverage }
      : { bucket: "hour", bars: [], total: 0, failed: 0, by_category: {}, coverage },
  );
  expect(screen.getByText("node-b")).toBeTruthy();
  expect(screen.getByText(/This log is missing one node/)).toBeTruthy();
  location.hash = "#/";
});
