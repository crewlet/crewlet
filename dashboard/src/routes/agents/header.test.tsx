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

// THE SCHEDULES TAB CARRIES ITS FIGURE, from the schedules push this client
// already holds, beside the Roster's and the Teams' — on every Agents tab.
test("every Agents section publishes the roster, teams and schedules figures", () => {
  withClient(<Counts />, 6);
  const index = indexOrg(CHART_ORG);
  expect(vi.mocked(useSectionCounts)).toHaveBeenLastCalledWith({
    roster: String(index.seats.length),
    teams: String(index.units.length),
    schedules: "6",
  });
});

// ON A PHONE ONE ACTION STAYS IN VIEW. Edit org folds into "More" — inline it
// is marked to fold, and the menu offers it — so Add seat is never the control
// past the window's edge.
test("Edit org folds into the phone's More menu and Add seat stays inline", () => {
  const { container } = withClient(<AgentsHeader index={indexOrg(CHART_ORG)} />, 0);
  const entries = vi.mocked(usePageMenu).mock.lastCall?.[0] ?? [];
  expect(entries.map((e) => e.label)).toEqual(["Edit org"]);
  const edit = [...container.querySelectorAll("a")].find((a) => a.textContent === "Edit org")!;
  expect(edit.closest(".page-action-folds")).not.toBeNull();
  const add = [...container.querySelectorAll("a, button")].find((b) =>
    b.textContent?.includes("Add seat"),
  )!;
  expect(add.closest(".page-action-folds")).toBeNull();
});

// THE SAME BAR ON EVERY SECTION, Schedules included. It carried only the star
// and Copy link, so Find a seat, Edit org and Add seat vanished from the bar
// the moment a reader moved to it from the roster, and came back on the way
// out.
test("the Schedules section carries the Agents page bar", () => {
  const { container } = withClient(<Schedules />, 2);
  expect(container.querySelector("input[placeholder='Find a seat']")).not.toBeNull();
  const controls = [...container.querySelectorAll("a, button")].map((el) => el.textContent);
  expect(controls).toContain("Edit org");
  expect(controls.some((text) => text?.includes("Add seat"))).toBe(true);
});
