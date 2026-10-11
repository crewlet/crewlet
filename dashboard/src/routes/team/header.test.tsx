/**
 * What every Team section hands the frame — and the console's Agent setup,
 * which draws the same screens: the tab figures of the shell it is in, and in
 * the console alone the org's writes, Edit org folding into the phone's
 * "More" menu and Add seat held with its reason for a reader who may not use
 * it.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { act } from "react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { ADD_SEAT_REASON, TeamHeader, useTeamCounts } from "./header.tsx";
import { Schedules } from "../admin/agents/Schedules.tsx";
import { Router } from "~/app/router.tsx";
import { usePageMenu, useSectionCounts } from "~/app/page.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { indexOrg } from "~/lib/seats.ts";
import { managedSentence } from "~/protocol/configAnswer.ts";
import { CHART_ORG } from "~/test/orgchart.ts";
import { LiveSocket, Store, type ScheduleRow } from "~/protocol/index.ts";

// THE FRAME'S SLOTS, stood in for so a case can hold what the screen CLAIMS:
// the tabs and the menu that draw them are the page header's.
vi.mock("~/app/page.tsx", () => ({
  usePageMenu: vi.fn(),
  useSectionCounts: vi.fn(),
  usePageLabels: vi.fn(),
}));

const ANONYMOUS = { token_id: "", role: "", reach: "public", acts: [] };
const ADMIN = { token_id: "ops", role: "admin", reach: "admin", linked: true, handle: "jane" };

beforeEach(() => {
  location.hash = "#/admin/agents";
});

afterEach(() => {
  cleanup();
  vi.mocked(useSectionCounts).mockClear();
  vi.mocked(usePageMenu).mockClear();
  location.hash = "";
});

async function withClient(
  ui: React.ReactNode,
  schedules: number,
  viewer: Record<string, unknown> = ANONYMOUS,
) {
  const store = new Store();
  store.applyOrg(CHART_ORG);
  store.applySchedules({
    schedules: Array.from({ length: schedules }, (_, i) => ({ name: `s${i}` }) as ScheduleRow),
  });
  const socket = new LiveSocket(store);
  (socket as unknown as { query: () => Promise<unknown> }).query = () => Promise.resolve(viewer);
  const view = render(
    <ClientContext.Provider value={{ store, socket }}>
      <ViewerProvider>
        <Router>{ui}</Router>
      </ViewerProvider>
    </ClientContext.Provider>,
  );
  await act(async () => {
    for (let i = 0; i < 5; i++) await Promise.resolve();
  });
  return view;
}

function Counts() {
  useTeamCounts(indexOrg(CHART_ORG));
  return null;
}

// EACH SHELL'S SECTIONS CARRY THEIR OWN FIGURES, from what this client already
// holds: the person shell's people and units, and the console's seats and
// schedules — the schedules push is the console's subject, not a teammate's.
test("each shell's sections publish their own figures", async () => {
  const index = indexOrg(CHART_ORG);
  await withClient(<Counts />, 6);
  expect(vi.mocked(useSectionCounts)).toHaveBeenLastCalledWith({
    people: String(index.seats.length),
    schedules: "6",
  });
  cleanup();
  location.hash = "#/team";
  await withClient(<Counts />, 6);
  expect(vi.mocked(useSectionCounts)).toHaveBeenLastCalledWith({
    people: String(index.seats.length),
    units: String(index.units.length),
  });
});

// ON A PHONE ONE ACTION STAYS IN VIEW. Edit org folds into "More" — inline it
// is marked to fold, and the menu offers it — so Add seat is never the control
// past the window's edge.
test("Edit org folds into the phone's More menu and Add seat stays inline", async () => {
  const { container } = await withClient(<TeamHeader index={indexOrg(CHART_ORG)} />, 0);
  const entries = vi.mocked(usePageMenu).mock.lastCall?.[0] ?? [];
  expect(entries.map((e) => e.label)).toEqual(["Edit org"]);
  const edit = [...container.querySelectorAll("a")].find((a) => a.textContent === "Edit org")!;
  expect(edit.closest(".page-action-folds")).not.toBeNull();
  expect(edit.getAttribute("href")).toBe("#/admin/company/org");
  const add = [...container.querySelectorAll("a, button")].find((b) =>
    b.textContent?.includes("Add seat"),
  )!;
  expect(add.closest(".page-action-folds")).toBeNull();
});

// THE PERSON SHELL'S TEAM CARRIES NO ORG WRITE: the document is the
// console's, and the header there is the one that holds them.
test("the person shell's header is the finder alone", async () => {
  location.hash = "#/team";
  const { container } = await withClient(<TeamHeader index={indexOrg(CHART_ORG)} />, 0, ADMIN);
  expect(container.querySelector("input[placeholder='Find a seat']")).not.toBeNull();
  const controls = [...container.querySelectorAll("a, button")].map((el) => el.textContent);
  expect(controls.some((text) => text?.includes("Add seat"))).toBe(false);
  expect(controls).not.toContain("Edit org");
  expect(vi.mocked(usePageMenu)).not.toHaveBeenCalled();
});

// ADD SEAT IS NEVER HIDDEN IN THE CONSOLE: a reader who may not change the
// org sees it held, with the reason.
test("Add seat is held with its reason for a reader with no key", async () => {
  await withClient(<TeamHeader index={indexOrg(CHART_ORG)} />, 0);
  const add = screen.getByRole("button", { name: /Add seat/ });
  expect(add.getAttribute("aria-disabled") === "true" || add.hasAttribute("disabled")).toBe(true);
  expect(add.getAttribute("title")).toBe(ADD_SEAT_REASON);
});

test("an admin's Add seat goes to the builder, adding an agent", async () => {
  await withClient(<TeamHeader index={indexOrg(CHART_ORG)} />, 0, ADMIN);
  const add = screen.getByRole("link", { name: /Add seat/ });
  expect(add.getAttribute("href")).toBe("#/admin/company/org?add=agent");
});

// A MANAGED DOCUMENT (ADR-0030) holds Add seat for an admin whose key is not
// one of its writers — the builder it opens would be read-only — with the
// sentence that names who manages it; a writer still adds.
test("Add seat is held with the managed sentence for an admin who is not a writer", async () => {
  await withClient(<TeamHeader index={indexOrg(CHART_ORG)} />, 0, {
    ...ADMIN,
    config_writer: false,
    config_managed_by: ["gitops"],
  });
  const add = screen.getByRole("button", { name: /Add seat/ });
  expect(add.getAttribute("aria-disabled") === "true" || add.hasAttribute("disabled")).toBe(true);
  expect(add.getAttribute("title")).toBe(managedSentence(["gitops"]));
  cleanup();

  await withClient(<TeamHeader index={indexOrg(CHART_ORG)} />, 0, {
    ...ADMIN,
    token_id: "gitops",
    config_writer: true,
    config_managed_by: ["gitops"],
  });
  expect(screen.getByRole("link", { name: /Add seat/ }).getAttribute("href")).toBe(
    "#/admin/company/org?add=agent",
  );
});

// THE SAME BAR ON EVERY CONSOLE SECTION, Schedules included. It carried only
// the star and Copy link, so Find a seat, Edit org and Add seat vanished from
// the bar the moment a reader moved to it from the seats, and came back on
// the way out.
test("the Schedules section carries the console's seats bar", async () => {
  location.hash = "#/admin/agents/schedules";
  const { container } = await withClient(<Schedules />, 2);
  expect(container.querySelector("input[placeholder='Find a seat']")).not.toBeNull();
  const controls = [...container.querySelectorAll("a, button")].map((el) => el.textContent);
  expect(controls).toContain("Edit org");
  expect(controls.some((text) => text?.includes("Add seat"))).toBe(true);
});
