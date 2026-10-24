/**
 * What every Agents section hands the frame: the same three tab figures on
 * every tab, and Edit org in the phone's "More" menu.
 */

import { cleanup, render } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { AgentsHeader, useAgentsCounts } from "./header.tsx";
import { Schedules } from "./Schedules.tsx";
import { Router } from "~/app/router.tsx";
import { usePageMenu, useSectionCounts } from "~/app/Shell.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { indexOrg } from "~/lib/seats.ts";
import { CHART_ORG } from "~/test/orgchart.ts";
import { LiveSocket, Store, type ScheduleRow } from "~/protocol/index.ts";

// THE FRAME'S SLOTS, stood in for so a case can hold what the screen CLAIMS:
// the tabs and the menu that draw them are the page header's.
vi.mock("~/app/Shell.tsx", () => ({
  usePageMenu: vi.fn(),
  useSectionCounts: vi.fn(),
  usePageLabels: vi.fn(),
}));

afterEach(() => {
  cleanup();
  vi.mocked(useSectionCounts).mockClear();
  vi.mocked(usePageMenu).mockClear();
});

function withClient(ui: React.ReactNode, schedules: number) {
  const store = new Store();
  store.applyOrg(CHART_ORG);
  store.applySchedules({
    schedules: Array.from({ length: schedules }, (_, i) => ({ name: `s${i}` }) as ScheduleRow),
  });
  const socket = new LiveSocket(store);
  (socket as unknown as { query: () => Promise<unknown> }).query = () =>
    Promise.resolve({ login: "", owner: "", acts: [] });
  return render(
    <ClientContext.Provider value={{ store, socket }}>
      <ViewerProvider>
        <Router>{ui}</Router>
      </ViewerProvider>
    </ClientContext.Provider>,
  );
}

function Counts() {
  useAgentsCounts(indexOrg(CHART_ORG));
  return null;
}

// THE ORG CHART'S TABS CARRY THEIR FIGURES, the Roster's and the Teams'.
// Schedules is its own row of Company and keeps no tab here, so the schedules
// push this client holds is not counted beside them.
test("every Org chart section publishes the roster and teams figures, and no schedules", () => {
  withClient(<Counts />, 6);
  const index = indexOrg(CHART_ORG);
  expect(vi.mocked(useSectionCounts)).toHaveBeenLastCalledWith({
    roster: String(index.seats.length),
    teams: String(index.units.length),
  });
});

// ON A PHONE ONE ACTION STAYS IN VIEW. Edit org folds into "More" — inline it
// is marked to fold, and the menu offers it — so Add seat is never the control
// past the window's edge.
test("Edit org folds into the phone's More menu and Add seat stays inline", () => {
  const { container } = withClient(<AgentsHeader index={indexOrg(CHART_ORG)} />, 0);
  const entries = vi.mocked(usePageMenu).mock.lastCall?.[0] ?? [];
  expect(entries.map((e) => e.label)).toEqual(["Edit org"]);
  const edit = [...container.querySelectorAll("a, button")].find(
    (a) => a.textContent === "Edit org",
  )!;
  expect(edit.closest(".page-action-folds")).not.toBeNull();
  const add = [...container.querySelectorAll("a, button")].find((b) =>
    b.textContent?.includes("Add seat"),
  )!;
  expect(add.closest(".page-action-folds")).toBeNull();
});

// SCHEDULES IS ITS OWN ROW OF COMPANY, not a tab of the Org chart, and its
// bar is its own: it carried the chart's Find a seat, Edit org and Add seat,
// so a schedules page offered "Add seat" as its main action.
test("the Schedules page carries none of the Org chart's controls", () => {
  const { container } = withClient(<Schedules />, 2);
  expect(container.querySelector("input[placeholder='Find a seat']")).toBeNull();
  const controls = [...container.querySelectorAll("a, button")].map((el) => el.textContent);
  expect(controls).not.toContain("Edit org");
  expect(controls.some((text) => text?.includes("Add seat"))).toBe(false);
});

// EDIT ORG ASKS THE BUILDER'S GATE: a reader who may not write the chart was
// offered it enabled, beside a disabled Add seat, and the page it opened
// refused them. Mutation: draw the link whatever the access says.
test("Edit org is held with its reason for a reader who may not write the chart", () => {
  const { container } = withClient(<AgentsHeader index={indexOrg(CHART_ORG)} />, 0);
  const edit = [...container.querySelectorAll("a, button")].find(
    (a) => a.textContent === "Edit org",
  )!;
  expect(edit.tagName).toBe("BUTTON");
  expect(edit.getAttribute("aria-disabled")).toBe("true");
});
