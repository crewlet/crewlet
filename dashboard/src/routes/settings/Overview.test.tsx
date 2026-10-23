/**
 * Company › Overview and Company › Policies, the company's own pages, claim
 * nothing before the chart arrives.
 *
 * "No mission is set", "No policies are set" and a policy count of 0 are
 * statements about the company. Before the org push lands this browser knows
 * nothing about it at all, and the screen said all three to every reader who
 * opened it on a cold tab; the one rule every figure here keeps is that a
 * figure the engine did not answer is absent, never a zero. And the policies
 * are drawn once, on their own page: the Overview drew them a second time.
 */

import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test } from "vitest";

import type { ComponentType } from "react";
import { Overview } from "./Overview.tsx";
import { Policies } from "./Policies.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { withDerived } from "~/test/org.ts";

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
  location.hash = "#/company";
});

afterEach(() => {
  cleanup();
  location.hash = "#/";
});

function mount(Screen: ComponentType = Overview) {
  const store = new Store();
  render(
    <ClientContext.Provider value={{ store, socket: new LiveSocket(store) }}>
      <ViewerProvider>
        <Router>
          <Screen />
        </Router>
      </ViewerProvider>
    </ClientContext.Provider>,
  );
  return store;
}

describe("the overview", () => {
  test("says nothing about the company before the chart arrives", () => {
    mount();
    expect(screen.queryByText(/No mission is set/)).toBeNull();
    expect(screen.queryByText(/No name is set/)).toBeNull();
    expect(screen.getByText("Loading the company's charter")).toBeDefined();
  });

  test("says what the chart says once it has, and leaves the policies to their page", () => {
    const store = mount();
    act(() =>
      store.applyOrg(
        withDerived({
          name: "Acme",
          mission: "Ship it",
          timezone: "Europe/Lisbon",
          policies: ["Every amount is exact to the cent."],
          roles: [],
        }),
      ),
    );
    expect(screen.getByText("Ship it")).toBeDefined();
    expect(screen.getByText("Acme")).toBeDefined();
    expect(screen.getByText("Europe/Lisbon")).toBeDefined();
    expect(screen.queryByText("Every amount is exact to the cent.")).toBeNull();
    expect(screen.queryByText("Loading the company's charter")).toBeNull();
  });
});

describe("the policies", () => {
  test("say nothing about the company before the chart arrives", () => {
    mount(Policies);
    expect(screen.queryByText(/No policies are set/)).toBeNull();
    expect(screen.getByText("Loading the company's policies")).toBeDefined();
  });

  test("list what the chart says, the empty list included, and say where the caps are", () => {
    const store = mount(Policies);
    act(() => store.applyOrg(withDerived({ name: "Acme", mission: "Ship it", roles: [] })));
    expect(screen.getByText("No policies are set")).toBeDefined();
    // THE CAPS LIVE ONCE, in Settings › Limits, and this page only says where.
    const limits = screen.getByRole("link", { name: "Settings › Limits" });
    expect(limits.getAttribute("href")).toBe("#/settings/limits");
    act(() =>
      store.applyOrg(
        withDerived({
          name: "Acme",
          policies: ["Ask Miles before changing the scope."],
          roles: [],
        }),
      ),
    );
    expect(screen.getByText("Ask Miles before changing the scope.")).toBeDefined();
  });
});
