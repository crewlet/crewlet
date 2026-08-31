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
  test("every nav route renders a screen rather than a blank", () => {
    for (const hash of [
      "#/people",
      "#/org",
      "#/runs",
      "#/conversations",
      "#/schedules",
      "#/model",
      "#/activity",
      "#/knowledge",
      "#/spend",
      "#/fleet",
      "#/integrations",
      "#/tools",
      "#/config",
      "#/secrets",
    ]) {
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
