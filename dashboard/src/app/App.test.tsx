/**
 * The application mounts, routes, and survives an empty engine.
 *
 * A smoke test rather than a screenshot: what it catches is a broken import, a
 * hook-order violation, and — the one that actually happens — a screen that
 * throws on the state a FRESHLY STARTED company is in, where every list is
 * empty and no query has answered yet. That state is the first thing anybody
 * sees, and it is the one least likely to be exercised by hand.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test } from "vitest";
import { App } from "./App.tsx";
import { Router } from "./router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { ALL_NAV } from "./nav.ts";
import { buildHash } from "./router.tsx";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

function mount() {
  const store = new Store();
  const socket = new LiveSocket(store);
  const view = render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <App />
      </Router>
    </ClientContext.Provider>,
  );
  return { store, socket, view };
}

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  location.hash = "#/";
});

afterEach(() => {
  // Explicit, because the suite runs with `globals: false` — testing-library
  // only registers its own auto-cleanup when a global afterEach exists, so
  // without this every render stacks up in one document and a getByText that
  // should find one node finds five.
  cleanup();
  location.hash = "#/";
});

describe("the shell", () => {
  test("mounts against an engine that has answered nothing", () => {
    mount();
    // The chrome is present and honest: not connected, and saying so.
    expect(screen.getByText("engine unreachable")).toBeDefined();
    expect(screen.getAllByText("Overview").length).toBeGreaterThan(0);
  });

  test("an empty company still shows every section", () => {
    // A fresh company has no seats, no events and no spend. Each panel says
    // what would fill it rather than rendering an unexplained blank.
    mount();
    expect(screen.getByText("No seat is mid-turn")).toBeDefined();
    expect(screen.getByText("Nothing has happened yet")).toBeDefined();
  });

  test("a disconnected engine is itself the first thing needing a person", () => {
    // It is not a footnote in a popover: the page cannot tell the truth about
    // anything else while the socket is down, so it leads.
    mount();
    expect(screen.getByText("No connection to the engine")).toBeDefined();
  });

  test("a connected, quiet company has nothing waiting", () => {
    const { store, view } = mount();
    store.applyHealth({ status: "ok" });
    view.rerender(
      <ClientContext.Provider value={{ store, socket: new LiveSocket(store) }}>
        <Router>
          <App />
        </Router>
      </ClientContext.Provider>,
    );
    expect(screen.getByText("Nothing is waiting on you")).toBeDefined();
  });

  test("the overview leads with what needs a person", () => {
    // The order is the argument: obligations first, then what the company is
    // doing, then what it has cost.
    mount();
    const headings = screen.getAllByText(/Needs a person|Live seats|Spend by phase/);
    expect(headings[0]?.textContent).toContain("Needs a person");
  });
});

describe("routing", () => {
  // DERIVED FROM THE NAV, not a hand-written list. The list was one, and a
  // hand-written one covers exactly the screens somebody remembered to add to
  // it — so a new nav entry that renders a blank ships green, which is the one
  // failure this test exists to catch.
  test("every nav route renders a screen rather than a blank", () => {
    const visited = ALL_NAV.map((item) => buildHash(item.path));
    // The two newest screens are the reason the list is derived: a hand-written
    // one would not have them.
    expect(visited).toContain(buildHash(["work"]));
    expect(visited).toContain(buildHash(["pages"]));
    for (const hash of visited) {
      location.hash = hash;
      const { view } = mount();
      expect(view.container.querySelector(".screen-inner")?.children.length, hash).toBeGreaterThan(
        0,
      );
      view.unmount();
    }
  });

  test("an unknown screen says so instead of rendering nothing", () => {
    location.hash = "#/nonsense";
    mount();
    expect(screen.getByText("Not a screen")).toBeDefined();
  });

  test("a seat that does not exist explains itself", () => {
    location.hash = "#/seats/ghost";
    mount();
    expect(screen.getByText(/No seat called/)).toBeDefined();
  });
});

describe("live state reaches the screen", () => {
  test("a pushed roster renders its seats", () => {
    const { store, view } = mount();
    location.hash = "#/people";
    store.applyOrg({
      name: "Acme",
      roles: [{ name: "CEO", handle: "ceo", goal: "Set direction" }],
    });
    store.applyAgents([{ role: "CEO", state: "working" }]);
    view.rerender(
      <ClientContext.Provider value={{ store, socket: new LiveSocket(store) }}>
        <Router>
          <App />
        </Router>
      </ClientContext.Provider>,
    );
    expect(screen.getAllByText("CEO").length).toBeGreaterThan(0);
  });
});

describe("a turn watched to its end", () => {
  /**
   * THE REPORTED BUG, end to end.
   *
   * A reader opens a seat's Model activity tab and watches a turn run. The
   * engine pushes the phase's rounds; the turn renders live. Then the review
   * lands: the projection clears `live_call` and pushes the overlay, and the
   * durable `agent_phase_completed` arrives on the same socket a beat earlier.
   *
   * Before this, the tab read only the overlay and a query answered once at
   * mount — so the turn vanished. On a seat's FIRST turn, the one where
   * onboarding runs, the mount-time history is empty and the page was left
   * claiming the seat had never taken a turn at all.
   */
  const seat = { name: "Acme", roles: [{ name: "CEO", handle: "ceo", goal: "Set direction" }] };

  const phaseEnvelope = (id: string, phase: string, ts: string) => ({
    id,
    type: "agent_phase_completed",
    timestamp: ts,
    source: "engine",
    actor: "CEO",
    summary: `${phase} finished`,
    category: "lifecycle",
    trace_id: "",
    span_id: "",
    parent_span_id: "",
    topic: "",
    failed: false,
    payload: {
      turn_id: "t1",
      phase,
      iteration: phase === "onboarding" ? 0 : 1,
      role: "CEO",
      model: "claude-sonnet-5",
      total_tokens: 100,
    },
  });

  function seatView() {
    location.hash = "#/seats/ceo?tab=model";
    const { store, view } = mount();
    store.applyOrg(seat);
    const redraw = () =>
      view.rerender(
        <ClientContext.Provider value={{ store, socket: new LiveSocket(store) }}>
          <Router>
            <App />
          </Router>
        </ClientContext.Provider>,
      );
    return { store, view, redraw };
  }

  test("the turn is still there once its review completes", () => {
    const { store, redraw } = seatView();

    // Onboarding and execute have landed; the review is live.
    store.applyEvent(phaseEnvelope("p1", "onboarding", "2026-01-01T00:00:01Z") as never);
    store.applyEvent(phaseEnvelope("p2", "execute", "2026-01-01T00:00:05Z") as never);
    store.applyAgents([
      {
        role: "CEO",
        state: "working",
        live_call: {
          turn_id: "t1",
          phase: "review",
          iteration: 1,
          model: "claude-sonnet-5",
          in_progress: true,
          round_num: 0,
          rounds: 1,
          started_at: "2026-01-01T00:00:07Z",
          updated_at: "2026-01-01T00:00:08Z",
        },
      },
    ] as never);
    redraw();
    expect(screen.getByText("Running now")).toBeDefined();
    expect(screen.getAllByText("review").length).toBeGreaterThan(0);

    // The review lands. The event goes out first, then the overlay that clears
    // the live call — the order internal/api/stream.Ingest publishes them in.
    store.applyEvent(phaseEnvelope("p3", "review", "2026-01-01T00:00:09Z") as never);
    store.applyAgents([{ role: "CEO", state: "idle", live_call: null }] as never);
    redraw();

    // STILL THERE, all three phases of it, and no longer running.
    expect(screen.queryByText("Running now")).toBeNull();
    expect(screen.getAllByText("onboarding").length).toBeGreaterThan(0);
    expect(screen.getAllByText("execute").length).toBeGreaterThan(0);
    expect(screen.getAllByText("review").length).toBeGreaterThan(0);
    // And the page does not claim the seat has never run.
    expect(screen.queryByText("No phases in the record for this seat")).toBeNull();
  });

  test("it is not held behind a 'new turns' button either", () => {
    // The reader was watching it: it is not a new row, whichever list it is in.
    // SCROLLED DOWN, which is the whole condition — at the top of the scroller
    // the settled list admits everything anyway, so an assertion taken there
    // passes whether the rule holds or not.
    const { store, redraw } = seatView();
    const scroller = document.querySelector(".screen");
    if (!scroller) throw new Error("no scroller to scroll: the shell's layout moved");
    Object.defineProperty(scroller, "scrollTop", { value: 400, configurable: true });
    store.applyAgents([
      {
        role: "CEO",
        state: "working",
        live_call: {
          turn_id: "t1",
          phase: "execute",
          iteration: 1,
          model: "claude-sonnet-5",
          in_progress: true,
          round_num: 0,
          rounds: 1,
          started_at: "2026-01-01T00:00:01Z",
          updated_at: "2026-01-01T00:00:02Z",
        },
      },
    ] as never);
    redraw();
    store.applyEvent(phaseEnvelope("p1", "execute", "2026-01-01T00:00:05Z") as never);
    store.applyAgents([{ role: "CEO", state: "idle", live_call: null }] as never);
    redraw();
    expect(screen.queryByText(/finished while you were reading/)).toBeNull();
    expect(screen.getAllByText("execute").length).toBeGreaterThan(0);
    // And the transcript the reader had open is still open: the card is
    // remounted when it crosses lists, so its latched state does not travel.
    expect(screen.getAllByText(/^turn t1/).length).toBeGreaterThan(0);
  });
});
