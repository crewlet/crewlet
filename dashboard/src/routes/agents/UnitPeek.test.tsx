/**
 * A unit's panel, over Structure's Table: what a unit is, found by its key.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test } from "vitest";

import { UnitPeek } from "./UnitPeek.tsx";
import { ViewerProvider } from "~/lib/viewer.ts";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store, type OrgProjection } from "~/protocol/index.ts";
import { healthFrame } from "~/test/health.ts";
import { afterMicrotasks } from "~/test/settle.ts";

/** A socket that never connects: these cases render, they do not fetch. */
class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

/** Engineering holds one seat of its own and a team of two under it — as the
 *  engine sends it: every seat with its handle and every unit with its key. */
const ORG: OrgProjection = {
  name: "Acme",
  roles: [],
  units: [
    {
      id: "engineering",
      name: "Engineering",
      type: "department",
      roles: [{ name: "VP Engineering", handle: "vpe" }],
      children: [
        {
          id: "backend",
          name: "Backend",
          type: "team",
          roles: [
            { name: "Dev A", handle: "dev-a" },
            { name: "Dev B", handle: "dev-b" },
          ],
        },
      ],
    },
  ],
  derived: {
    seats: [
      { handle: "vpe", name: "VP Engineering", kind: "agent" },
      { handle: "dev-a", name: "Dev A", kind: "agent" },
      { handle: "dev-b", name: "Dev B", kind: "agent" },
    ],
    units: [
      { id: "engineering", name: "Engineering", type: "department", seats: ["vpe"] },
      { id: "backend", name: "Backend", type: "team", seats: ["dev-a", "dev-b"] },
    ],
  },
} as unknown as OrgProjection;

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  location.hash = "#/company/org/table";
});

afterEach(() => {
  cleanup();
  location.hash = "";
});

/** One unit's panel, addressed by `id`, over a connected socket, the tracker filing Backend under BE. */
async function unitAt(id: string, org: OrgProjection = ORG) {
  const store = new Store();
  store.applyHealth(healthFrame());
  store.setConnected(true);
  store.applyOrg(org);
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    Promise.resolve(
      what === "work_projects"
        ? {
            projects: [{ key: "BE", unit: { key: "backend", name: "Backend", resolved: true } }],
            total: 1,
          }
        : { login: "jane", owner: "jane", grants: ["worklet"], acts: [] },
    );
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <ViewerProvider>
        <Router>
          <UnitPeek id={id} />
        </Router>
      </ViewerProvider>
    </ClientContext.Provider>,
  );
  await afterMicrotasks();
}

// A UNIT IS FOUND BY ITS KEY, which is what every link to it writes
// (`unitPlace`), and never by its name, which two units may share. Mutation:
// look the unit up by name and both halves go red.
test("a unit's panel is found by its key, never by its name", async () => {
  await unitAt("backend");
  expect(screen.queryByText(/No unit has the key/)).toBeNull();
  expect(screen.getAllByText("Backend").length).toBeGreaterThan(0);
  cleanup();

  await unitAt("Backend");
  expect(screen.getByText("No unit has the key “Backend”")).toBeTruthy();
});

// WHO A UNIT REPORTS TO IS THE ENGINE'S, and an inherited lead says so: hiding
// the difference is how somebody concludes a team is unmanaged.
test("a unit reports to the engine's lead, and an inherited one is marked", async () => {
  await unitAt("backend", {
    ...ORG,
    derived: {
      ...(ORG as unknown as { derived: object }).derived,
      units: [
        { id: "engineering", name: "Engineering", type: "department", seats: ["vpe"], lead: "vpe" },
        {
          id: "backend",
          name: "Backend",
          type: "team",
          seats: ["dev-a", "dev-b"],
          lead: "vpe",
          lead_inherited: true,
        },
      ],
    },
  } as unknown as OrgProjection);
  expect(screen.getByText("Reports to")).toBeTruthy();
  expect(screen.getAllByText("VP Engineering").length).toBeGreaterThan(0);
  expect(screen.getByText("(inherited)")).toBeTruthy();
});

// A UNIT CARRIES WHERE ITS WORK IS FILED, and the subtree's count with the direct one beside it.
test("a unit's panel shows its project key and both of its counts", async () => {
  await unitAt("engineering");
  expect(screen.getAllByText("3").length).toBeGreaterThan(0);
  cleanup();
  await unitAt("backend");
  expect(screen.getByRole("link", { name: "BE" }).getAttribute("href")).toBe("#/worklet/BE");
});
