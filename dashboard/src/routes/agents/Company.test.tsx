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

import { act, cleanup, render, screen } from "~/test/inCase.ts";
import { afterEach, beforeEach, expect, test } from "vitest";

import { LEAD_NOT_REPORTED, Teams, UnitBlock, UnitScreen } from "./Company.tsx";
import { ViewerProvider } from "~/lib/viewer.ts";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { indexOrg } from "~/lib/seats.ts";
import { LiveSocket, Store, type OrgProjection } from "~/protocol/index.ts";

/** A socket that never connects: these cases render, they do not fetch. */
class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

/** Engineering holds one seat of its own and a team of two under it. */
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
          roles: [{ name: "Dev A" }, { name: "Dev B" }],
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
  location.hash = "#/agents";
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
// A unit's page is found by its KEY
// ---------------------------------------------------------------------------

/**
 * Two teams called Platform, one declaring the id `infra`.
 *
 * Both halves are legal: a name is prose nothing holds unique, and a unit's
 * key is its id where it declares one. The page was looked up by NAME, so
 * `#/agents/teams/infra` answered "no unit" and the second Platform had no
 * page at all.
 */
const TWINS = {
  name: "Acme",
  roles: [],
  units: [
    { id: "platform", name: "Platform", purpose: "Runs the web tier", roles: [{ name: "Web" }] },
    { id: "infra", name: "Platform", purpose: "Runs the metal", roles: [{ name: "Metal" }] },
  ],
  derived: {
    seats: [
      { handle: "web", name: "Web", kind: "agent" },
      { handle: "metal", name: "Metal", kind: "agent" },
    ],
    units: [
      { id: "platform", name: "Platform", type: "team", seats: ["web"] },
      { id: "infra", name: "Platform", type: "team", seats: ["metal"] },
    ],
  },
} as unknown as OrgProjection;

function unitPage(id: string) {
  const store = new Store();
  store.applyOrg(TWINS);
  const socket = new LiveSocket(store);
  return render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <UnitScreen id={id} />
      </Router>
    </ClientContext.Provider>,
  );
}

test("a unit's page is the unit its key names, not the first sharing its name", () => {
  unitPage("infra");
  expect(screen.getByText("Runs the metal")).toBeTruthy();
  expect(screen.queryByText("Runs the web tier")).toBeNull();
});

test("the other unit of that name is reached by its own key", () => {
  unitPage("platform");
  expect(screen.getByText("Runs the web tier")).toBeTruthy();
  expect(screen.queryByText("Runs the metal")).toBeNull();
});

// ---------------------------------------------------------------------------
// Teams
// ---------------------------------------------------------------------------

/** Teams over `org`, with the tracker filing Backend's work under BE. */
async function teams(org: OrgProjection) {
  const store = new Store();
  store.applyHealth({ status: "healthy" });
  store.applyOrg(org);
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    Promise.resolve(
      what === "work_projects"
        ? {
            projects: [{ key: "BE", unit: { key: "backend", name: "Backend", resolved: true } }],
            total: 1,
          }
        : { login: "", grants: [], acts: [] },
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
  await act(async () => {
    for (let i = 0; i < 5; i++) await Promise.resolve();
  });
}

// NOT REPORTED IS NOT NOBODY, AND NOT A COUNT OF ZERO. Without the engine's
// derived block an inherited lead is unknowable here, so a unit declaring none
// says the engine did not report one — never a blank that reads as a team
// nobody leads — while the headcounts, which the document does state, stay.
test("no derived block is not a count of zero or a unit without a lead", async () => {
  const { derived: _drop, ...flat } = ORG as OrgProjection & { derived: unknown };
  await teams(flat as OrgProjection);
  expect(screen.getAllByText(LEAD_NOT_REPORTED).length).toBe(2);
  expect(screen.getByText("3 seats, 1 directly")).toBeTruthy();
  expect(screen.getByText("2 seats")).toBeTruthy();
});

test("with the derived block a unit's lead is the engine's, and nothing says unreported", async () => {
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
  expect(screen.queryByText(LEAD_NOT_REPORTED)).toBeNull();
  expect(screen.getAllByText("VP Engineering").length).toBeGreaterThan(0);
});

// A TEAM CARRIES WHERE ITS WORK IS FILED, and the goals it was given.
test("a unit shows its project key and its goals", async () => {
  const withGoals = structuredClone(ORG) as OrgProjection & {
    units: { children: { goals?: string[] }[] }[];
  };
  withGoals.units[0]!.children[0]!.goals = ["Ship the scheduler"];
  await teams(withGoals);
  expect(screen.getByRole("link", { name: "BE" }).getAttribute("href")).toBe("#/work/BE");
  expect(screen.getByText("Ship the scheduler")).toBeTruthy();
});
