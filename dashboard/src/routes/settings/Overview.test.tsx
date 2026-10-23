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
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";

import type { ComponentType } from "react";
import { Overview } from "./Overview.tsx";
import { Policies } from "./Policies.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { SessionReading } from "~/lib/frameSession.ts";
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
  // THE SESSION READ IS REFUSED, so what the reader holds is the viewer's.
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => new Response("{}", { status: 404 })),
  );
  location.hash = "#/company";
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

function mount(Screen: ComponentType = Overview) {
  const store = new Store();
  render(
    <ClientContext.Provider value={{ store, socket: new LiveSocket(store) }}>
      <SessionReading>
        <ViewerProvider>
          <Router>
            <Screen />
          </Router>
        </ViewerProvider>
      </SessionReading>
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
    // ONE PROFILE: the name is the card's title and the rest its facts, not a
    // card a sentence.
    expect(screen.getByRole("heading", { name: "Acme" })).toBeDefined();
    expect(document.querySelectorAll(".crewlet-card")).toHaveLength(1);
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

  test("list what the chart says, the empty list included, numbered and in order", () => {
    const store = mount(Policies);
    act(() => store.applyOrg(withDerived({ name: "Acme", mission: "Ship it", roles: [] })));
    expect(screen.getByText("No policies are set")).toBeDefined();
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
