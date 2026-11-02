/**
 * What a unit's number means, on the surface that drew the other one.
 *
 * "Leadership" carried three different unqualified counts across this product:
 * the whole subtree in the workspace rail, the unit's own members on the org
 * chart's block, and whatever was on screen in the roster's group head. None
 * of them said which question it had answered, so a reader moving between two
 * screens concluded their company had changed shape.
 *
 * The arithmetic is `lib/seats.ts`'s and is exercised there over the org
 * fixture; what these cases hold is that the two SCREENS say it, because the
 * defect was never the arithmetic — each surface counted something true.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test } from "vitest";

import { Teams, UnitBlock, UnitPeek, UnitScreen } from "./Company.tsx";
import { ViewerProvider } from "~/lib/viewer.ts";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { indexOrg } from "~/lib/seats.ts";
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
  location.hash = "#/company/org";
});

/** A chart block, in the context its seat links reach for. */
function block(unitName: string) {
  const store = new Store();
  store.applyOrg(ORG);
  const socket = new LiveSocket(store);
  const unit = indexOrg(ORG).units.find((u) => u.name === unitName)!;
  return render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <UnitBlock unit={unit} />
      </Router>
    </ClientContext.Provider>,
  );
}

afterEach(() => {
  cleanup();
  location.hash = "";
});

// THE CHART BLOCK AGREES WITH THE RAIL, AND SAYS WHY. It drew
// `unit.seats.length` bare — the unit's own members — three inches from a rail
// badge holding the subtree, so Engineering was "1 seat" here and "3" there.
test("a unit block leads with the subtree and names the direct count beside it", () => {
  block("Engineering");
  expect(screen.getByText("3 seats, 1 directly")).toBeTruthy();
});

// AND A UNIT WITH NOTHING UNDER IT STILL READS AS ONE FACT. "2 seats, 2
// directly" is two statements about a team that has one, which is how a rule
// like this ends up switched off again.
test("a leaf unit says one number, not the same number twice", () => {
  block("Backend");
  expect(screen.getByText("2 seats")).toBeTruthy();
  expect(screen.queryByText(/directly/)).toBeNull();
});

// ---------------------------------------------------------------------------
// Teams
// ---------------------------------------------------------------------------

/** Teams over `org`, with the tracker filing Backend's work under BE. */
async function teams(org: OrgProjection) {
  const store = new Store();
  store.applyHealth(healthFrame());
  store.applyOrg(org);
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    Promise.resolve(
      what === "work_projects"
        ? {
            projects: [{ key: "BE", unit: { key: "backend", name: "Backend", resolved: true } }],
            total: 1,
          }
        : what === "viewer"
          ? { login: "jane", owner: "jane", grants: ["worklet"], acts: [] }
          : {},
    );
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <ViewerProvider>
        <Router>
          <Teams />
        </Router>
      </ViewerProvider>
    </ClientContext.Provider>,
  );
  await afterMicrotasks();
}

// A LEAD IS THE ENGINE'S, and an inherited one says so: hiding the difference
// is how somebody concludes a team is unmanaged.
test("a unit's lead is the engine's, and an inherited one is marked", async () => {
  await teams({
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
  expect(screen.getAllByText("VP Engineering").length).toBeGreaterThan(0);
  expect(screen.getByText("(inherited)")).toBeTruthy();
});

// A TEAM CARRIES WHERE ITS WORK IS FILED, and the goals it was given.
test("a unit shows its project key and its goals", async () => {
  const withGoals = structuredClone(ORG) as OrgProjection & {
    units: { children: { goals?: string[] }[] }[];
  };
  withGoals.units[0]!.children[0]!.goals = ["Ship the scheduler"];
  await teams(withGoals);
  expect(screen.getByRole("link", { name: "BE" }).getAttribute("href")).toBe("#/worklet/BE");
  expect(screen.getByText("Ship the scheduler")).toBeTruthy();
});

// ---------------------------------------------------------------------------
// A unit's own page and peek
// ---------------------------------------------------------------------------

/** One unit's page or peek, addressed by `id`, over a connected socket. */
async function unitAt(Unit: typeof UnitScreen | typeof UnitPeek, id: string) {
  const store = new Store();
  store.applyHealth(healthFrame());
  store.setConnected(true);
  store.applyOrg(ORG);
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    Promise.resolve(
      what === "work_projects" ? { projects: [], total: 0 } : { login: "", owner: "", acts: [] },
    );
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <ViewerProvider>
        <Router>
          <Unit id={id} />
        </Router>
      </ViewerProvider>
    </ClientContext.Provider>,
  );
  await afterMicrotasks();
}

// A UNIT IS FOUND BY ITS KEY, which is what every link to it writes
// (`unitRoute`) — the page and the peek alike — and never by its name, which
// two units may share. Found by name, every link Teams and the chart drew to a
// unit whose key is not its name opened "no unit". Mutation: look the unit up
// by name and both halves of each case go red.
test("a unit's page and peek are found by its key, never by its name", async () => {
  for (const Unit of [UnitScreen, UnitPeek]) {
    await unitAt(Unit, "backend");
    expect(screen.queryByText(/No unit has the key/)).toBeNull();
    expect(screen.getAllByText("Backend").length).toBeGreaterThan(0);
    cleanup();

    await unitAt(Unit, "Backend");
    expect(screen.getByText("No unit has the key “Backend”")).toBeTruthy();
    cleanup();
  }
});

// A UNIT ROW SAYS ITS KIND AND ITS LEAD AS QUIET FACTS: "Engineering ENG
// team Ada Lovelace" drawn as pills read as four badges on every row.
test("a unit's kind and lead are words, not pills", async () => {
  await teams(ORG);
  for (const row of document.querySelectorAll(".org-unit-head")) {
    expect(row.querySelector(".crewlet-tag")).toBeNull();
  }
  expect(document.querySelectorAll(".org-unit-fact").length).toBeGreaterThan(0);
});
