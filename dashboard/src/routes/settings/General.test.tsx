/**
 * Settings › General — the charter — claims nothing before the chart arrives.
 *
 * "No mission is set", "No policies are set" and a policy count of 0 are
 * statements about the company. Before the org push lands this browser knows
 * nothing about it at all, and the screen said all three to every reader who
 * opened it on a cold tab — the one rule every figure here keeps is that a
 * figure the engine did not answer is absent, never a zero.
 */

import { act, cleanup, render, screen } from "~/test/inCase.ts";
import { afterEach, beforeEach, describe, expect, test } from "vitest";

import { General } from "./General.tsx";
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
  location.hash = "#/settings";
});

afterEach(() => {
  cleanup();
  location.hash = "#/";
});

function mount() {
  const store = new Store();
  render(
    <ClientContext.Provider value={{ store, socket: new LiveSocket(store) }}>
      <Router>
        <General />
      </Router>
    </ClientContext.Provider>,
  );
  return store;
}

describe("the charter", () => {
  test("says nothing about the company before the chart arrives", () => {
    mount();
    expect(screen.queryByText(/No mission is set/)).toBeNull();
    expect(screen.queryByText(/No policies are set/)).toBeNull();
    expect(screen.queryByText("Policies")).toBeNull();
    expect(screen.getByText("Loading the company's charter")).toBeDefined();
  });

  test("says what the chart says once it has, the empty parts included", () => {
    const store = mount();
    act(() => store.applyOrg({ name: "Acme", mission: "Ship it", roles: [] }));
    expect(screen.getByText("Ship it")).toBeDefined();
    expect(screen.getByText("No policies are set")).toBeDefined();
    expect(screen.queryByText("Loading the company's charter")).toBeNull();
  });
});
