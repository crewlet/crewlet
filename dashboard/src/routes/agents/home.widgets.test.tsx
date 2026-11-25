import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store, type AgentRow, type OrgProjection } from "~/protocol/index.ts";
import { CHART_ORG } from "~/test/orgchart.ts";
import { widgets } from "./home.widgets.tsx";

afterEach(cleanup);

const pause = { by: "jane", by_kind: "human", at: "2026-01-01T00:00:00Z", stop_running: false };
const seat = (handle: string, over: Partial<AgentRow>) =>
  ({ id: handle, agent_id: handle, handle, role: handle, ...over }) as AgentRow;

// A PAUSED SEAT IS IN THE PAUSED TILE while its turn finishes or its executor
// asks, as Structure's chips count it. Mutation: bucket on `activityOf` again,
// and CTO lands in Working and SWE in Needs you.
test("the roster tiles count a seat paused mid-turn as paused", () => {
  const store = new Store();
  store.applyOrg(CHART_ORG as OrgProjection);
  store.applySeats([
    seat("cto", { activity: "working", paused: pause }),
    seat("swe", { activity: "needs", paused: pause }),
    seat("fe", { activity: "working" }),
  ]);
  const Roster = widgets.find((w) => w.key === "org.roster")!.Component;
  render(
    <ClientContext.Provider value={{ store, socket: new LiveSocket(store) }}>
      <Roster params={{}} />
    </ClientContext.Provider>,
  );
  const tile = (label: string) => screen.getByText(label).parentElement!.textContent;
  expect(tile("Working")).toBe("Working1FE");
  expect(tile("Needs you")).toBe("Needs you0None");
  expect(tile("Paused or stopped")).toBe("Paused or stopped2CTO, SWE");
});
