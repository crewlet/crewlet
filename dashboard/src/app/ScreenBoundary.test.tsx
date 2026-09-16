/**
 * One screen that throws cannot blank the shell.
 *
 * The failure this guards was real: a seat whose `llm` was a per-phase mapping
 * reached React as an object child, React unmounted the whole application,
 * and the reader had a white page with no navigation on it. The Tools screen
 * is replaced here by one that throws, because what is under test is the
 * wiring around a screen rather than any one screen's fault.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { App, ScreenBoundary } from "./App.tsx";
import { Router } from "./router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";

vi.mock("~/routes/Tools.tsx", () => ({
  Tools: () => {
    throw new Error("roles[0].llm: an object is not a valid child");
  },
}));

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
  // React reports a caught render error to the console by design; the report
  // is expected here and is not what is being tested.
  vi.spyOn(console, "error").mockImplementation(() => {});
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  location.hash = "#/";
});

function mountApp(hash: string) {
  location.hash = hash;
  const store = new Store();
  return render(
    <ClientContext.Provider value={{ store, socket: new LiveSocket(store) }}>
      <Router>
        <App />
      </Router>
    </ClientContext.Provider>,
  );
}

describe("the shell around a failing screen", () => {
  test("the navigation stays, and the failure says what happened", () => {
    mountApp("#/tools");
    expect(screen.getByText("This screen could not be drawn")).toBeDefined();
    expect(screen.getByText("roles[0].llm: an object is not a valid child")).toBeDefined();
    // The way somewhere else is still on the page.
    expect(screen.getByRole("navigation", { name: "Sections" })).toBeDefined();
    expect(screen.getAllByText("Overview").length).toBeGreaterThan(0);
  });

  test("going somewhere else renders that screen", async () => {
    mountApp("#/tools");
    location.hash = "#/people";
    window.dispatchEvent(new HashChangeEvent("hashchange"));
    expect(await screen.findByPlaceholderText(/Filter by name/)).toBeDefined();
    expect(screen.queryByText("This screen could not be drawn")).toBeNull();
  });
});

describe("ScreenBoundary", () => {
  let fail = true;
  function Flaky() {
    if (fail) throw new Error("malformed");
    return <span>drawn</span>;
  }

  test("try again renders the screen once it can be drawn", () => {
    fail = true;
    render(
      <ScreenBoundary resetKey="#/a">
        <Flaky />
      </ScreenBoundary>,
    );
    expect(screen.getByText("malformed")).toBeDefined();
    fail = false;
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(screen.getByText("drawn")).toBeDefined();
  });

  test("a new reset key clears the failure", () => {
    fail = true;
    const view = render(
      <ScreenBoundary resetKey="#/a">
        <Flaky />
      </ScreenBoundary>,
    );
    fail = false;
    view.rerender(
      <ScreenBoundary resetKey="#/b">
        <Flaky />
      </ScreenBoundary>,
    );
    expect(screen.getByText("drawn")).toBeDefined();
  });
});
