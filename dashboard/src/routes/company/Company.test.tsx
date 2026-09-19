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

import { UnitBlock } from "./Company.tsx";
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
      name: "Engineering",
      type: "department",
      roles: [{ name: "VP Engineering", handle: "vpe" }],
      children: [{ name: "Backend", type: "team", roles: [{ name: "Dev A" }, { name: "Dev B" }] }],
    },
  ],
  derived: {
    seats: [
      { handle: "vpe", name: "VP Engineering", kind: "agent" },
      { handle: "dev-a", name: "Dev A", kind: "agent" },
      { handle: "dev-b", name: "Dev B", kind: "agent" },
    ],
    units: [
      { name: "Engineering", type: "department", seats: ["vpe"] },
      { name: "Backend", type: "team", seats: ["dev-a", "dev-b"] },
    ],
  },
} as unknown as OrgProjection;

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  location.hash = "#/company";
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
