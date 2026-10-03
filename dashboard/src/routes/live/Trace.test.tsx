/**
 * Where one trace sends a reader next, and what it says it is missing.
 *
 * A trace is every event sharing one id, assembled from every node's own store
 * at read time — so its page answers two questions beyond the spans it draws:
 * where the same events are in the log, and whether a node's share of the
 * chain is absent.
 */

import { act, cleanup, fireEvent, render, screen } from "~/test/inCase.ts";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { TraceScreen } from "./Trace.tsx";
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
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

const TRACE = "4bf92f3577b34da6a3ce929d0e0e4736";

async function mount(answer: Record<string, unknown>) {
  location.hash = `#/live/traces/${TRACE}`;
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what: string) =>
    Promise.resolve(what === "trace" ? answer : {});
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <TraceScreen traceId={TRACE} />
      </Router>
    </ClientContext.Provider>,
  );
  await act(async () => {
    await Promise.resolve();
  });
}

const span = {
  id: "e-1",
  type: "agent_turn_started",
  timestamp: "2026-09-13T10:00:00Z",
  source: "engine",
  actor: "CEO",
  summary: "CEO started a turn",
  category: "system",
  trace_id: TRACE,
  span_id: "s-1",
  parent_span_id: "",
  topic: "",
};

// "IN THE LOG" IS THE LOG NARROWED TO THIS TRACE.
//
// It carried the trace id as the log's TEXT search, which matches no summary
// the engine writes, so it opened on an empty log. The engine filters by the
// trace id every event carries (`trace_id`), and the log names it `trace=`.
//
// Mutation: send the id as `q` again, and the address has no `trace=`.
test("in the log opens the event log narrowed to this trace", async () => {
  await mount({ trace_id: TRACE, events: [span], truncated: false });
  fireEvent.click(screen.getByRole("button", { name: "In the log" }));
  expect(location.hash).toBe(`#/live/events?trace=${TRACE}`);
});

// A NODE THAT DID NOT ANSWER IS A GAP IN THE CHAIN, and says so — a trace
// shown short with no note reads as a causal chain that simply ends.
test("a trace missing a node names it", async () => {
  await mount({
    trace_id: TRACE,
    events: [span],
    truncated: false,
    coverage: {
      nodes: [
        { id: "node-a", answered: true, error: "" },
        { id: "node-b", answered: false, error: "no answer inside the read budget" },
      ],
      complete: false,
    },
  });
  expect(screen.getByText(/This trace is missing one node/)).toBeTruthy();
  expect(screen.getByText("node-b")).toBeTruthy();
});

function ev(
  id: string,
  at: string,
  spanId: string,
  parent: string,
  summary: string,
  failed = false,
): Record<string, unknown> {
  return {
    ...span,
    id,
    timestamp: `2026-09-13T10:00:${at}Z`,
    span_id: spanId,
    parent_span_id: parent,
    summary,
    actor: "Agent PM",
    failed,
  };
}

// ONE ROW PER EVENT, NESTED BY SPAN. Every event of a phase carries that
// phase's span id, and the arranger keyed its tree on `span_id` as though a
// span were one event: it kept the last event per span, and pushed that node
// into its parent once per event — so a 25-event trace drew 344 rows, the same
// few repeated, and both failures the header counted were never drawn.
//
// Mutation: key the tree by span id with one node per span (as it was), and
// the rows repeat the phase's last event while the failure disappears.
test("every event is one row, and the failed one is drawn and marked", async () => {
  const events = [
    ev("e-1", "00", "root", "", "Scheduled sweep fired"),
    ev("e-2", "01", "turn", "root", "Agent PM started a turn"),
    ev("e-3", "02", "phase", "turn", "Agent PM searched knowledge"),
    ev("e-4", "03", "phase", "turn", "Agent PM execute → blocked"),
    ev("e-5", "04", "phase", "turn", "guard stall tripped", true),
    ev("e-6", "05", "turn", "root", "Agent PM completed turn"),
  ];
  await mount({ trace_id: TRACE, events, truncated: false });
  const rows = document.querySelectorAll("a.trace-row");
  expect(rows).toHaveLength(events.length);
  expect([...rows].map((r) => r.getAttribute("href"))).toEqual(
    events.map((e) => `#/live/events/${e.id as string}`),
  );
  // THE ROW, not the header: the opening event's sentence is also the title.
  const row = (text: string) =>
    screen
      .getAllByText(text)
      .map((el) => el.closest("a.trace-row"))
      .find(Boolean) as HTMLElement;
  const failed = row("guard stall tripped");
  expect(failed.classList.contains("failed")).toBe(true);
  expect(failed.querySelector('[aria-label="failed"]')).toBeTruthy();
  // THE NESTING IS THE SPAN'S: the phase's events sit under the turn's.
  const pad = (text: string) =>
    (row(text).querySelector(".feed-actor") as HTMLElement).style.paddingLeft;
  expect(pad("Scheduled sweep fired")).toBe("0px");
  expect(pad("Agent PM completed turn")).toBe("12px");
  expect(pad("guard stall tripped")).toBe("24px");
});

// A PARENT CHAIN THAT LOOPS HIDES NOTHING: every span still reaches a row.
test("a looping parent chain still draws every event", async () => {
  const events = [ev("e-1", "00", "a", "b", "first"), ev("e-2", "01", "b", "a", "second")];
  await mount({ trace_id: TRACE, events, truncated: false });
  expect(document.querySelectorAll("a.trace-row")).toHaveLength(2);
});
