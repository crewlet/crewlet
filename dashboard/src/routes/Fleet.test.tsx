/**
 * The fleet screen says what is wrong, and nothing at all when nothing is.
 *
 * This screen is read when nodes are dying, so what it draws when everything
 * is fine matters as much as what it draws when it is not: a mark on a healthy
 * fleet is a reader sent looking for a fault that does not exist.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { Fleet } from "./Fleet.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store, type FleetAnswer } from "~/protocol/index.ts";

const answer = (over: Partial<FleetAnswer> = {}): FleetAnswer => ({
  nodes: [
    {
      id: "node-0",
      roles: ["ingress"],
      seats: 1,
      in_flight: 0,
      posture: "serve",
      config_epoch: 2,
      config_status: "ok",
      started_at: "2026-09-14T10:00:00Z",
    },
  ],
  seats: [{ handle: "sre-lead", node: "node-0", owner: "node-0:1", epoch: 2, expires_in: 32 }],
  duties: [{ duty: "integration-reconcile", node: "node-0", expires_in: 257 }],
  unplaceable: [],
  unmanned_roles: [],
  this_node: "node-0",
  target_epoch: 2,
  ...over,
});

function mount(fleet: FleetAnswer) {
  location.hash = "#/fleet";
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    Promise.resolve(what === "fleet" ? fleet : null);
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Fleet />
      </Router>
    </ClientContext.Provider>,
  );
}

afterEach(() => {
  cleanup();
  location.hash = "#/";
});

test("a fleet with nothing stranded draws no mark, and no bare zero", async () => {
  // `{n && <Panel/>}` renders the NUMBER when n is 0, so an empty list drew a
  // stray "0" on the page where the panel would have been. The engine always
  // answers with both arrays, so every healthy fleet carried one.
  mount(answer());
  await screen.findByRole("heading", { name: "Fleet" });
  expect(screen.queryByText("Not running anywhere")).toBeNull();
  const content = document.querySelector("main") ?? document.body;
  const stray = [...content.childNodes]
    .concat([...(content.firstElementChild?.childNodes ?? [])])
    .filter((node) => node.nodeType === Node.TEXT_NODE)
    .map((node) => (node.textContent ?? "").trim())
    .filter((text) => text !== "");
  expect(stray).toEqual([]);
});

test("a role with no seat running anywhere is still called out", async () => {
  // The other half: the panel the guard decides about has to appear when the
  // list is not empty, or the fix above would be a panel nobody ever sees.
  mount(answer({ unmanned_roles: ["reviewer"] }));
  expect(await screen.findByText("Not running anywhere")).toBeDefined();
  expect(screen.getByText("reviewer")).toBeDefined();
});
