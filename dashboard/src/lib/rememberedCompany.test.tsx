/**
 * A cold load draws the rail from what this browser last heard: no Code row that
 * vanishes, no product name that is then replaced by the company's.
 */

import { act, cleanup, render } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { ClientContext } from "./store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { STORAGE_KEYS } from "./storage.ts";
import { rememberedCompany, useCompanyName, useServicesRun } from "./services.ts";

afterEach(() => {
  cleanup();
  localStorage.clear();
});

const SERVICES = { tracker: "worklet", knowledge: "doclet", chat: "chatlet", code: "none" };

function Probe() {
  const run = useServicesRun();
  const name = useCompanyName();
  return <p data-testid="probe">{`${name}|${run.code}|${run.tracker}`}</p>;
}

function mount(store: Store) {
  const socket = new LiveSocket(store);
  return render(
    <ClientContext.Provider value={{ store, socket }}>
      <Probe />
    </ClientContext.Provider>,
  );
}

/* Mutation: draw every service until the engine answers, whatever was remembered. */
test("before the engine answers, the rail is what this browser last heard", () => {
  localStorage.setItem(STORAGE_KEYS.company, JSON.stringify({ name: "Acme", services: SERVICES }));
  const { getByTestId } = mount(new Store());
  expect(getByTestId("probe").textContent).toBe("Acme|false|true");
});

test("a first visit still draws every service, under no name", () => {
  const { getByTestId } = mount(new Store());
  expect(getByTestId("probe").textContent).toBe("|true|true");
});

test("what the engine says is drawn, and remembered for the next cold load", () => {
  const store = new Store();
  const { getByTestId } = mount(store);
  act(() => store.applyOrg({ name: "Acme", services: SERVICES, roles: [] } as never));
  expect(getByTestId("probe").textContent).toBe("Acme|false|true");
  expect(rememberedCompany()).toEqual({ name: "Acme", services: SERVICES });
});

test("a malformed or foreign value is no memory at all", () => {
  localStorage.setItem(STORAGE_KEYS.company, JSON.stringify({ name: 3, services: {} }));
  expect(rememberedCompany()).toBeNull();
  localStorage.setItem(STORAGE_KEYS.company, "not json");
  expect(rememberedCompany()).toBeNull();
});
