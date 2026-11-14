/**
 * The work on an entry: what the Work tab and the sheet's section say, who is
 * asked for it, and the mark an entry with open work carries on the map and
 * in the catalogue. The tracker is Worklet's, so every case here turns on
 * whether the reader holds it.
 */

import { cleanup, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, test } from "vitest";
import { href } from "~/app/router.tsx";
import {
  entryWorkAnswer,
  grownCompany,
  summary,
  taskRow,
  workAnswer,
  type AtlasFixture,
} from "~/test/atlas.ts";
import { mountAtlas } from "~/test/atlasMount.tsx";
import { Atlas } from "./Atlas.tsx";
import { AtlasPeek } from "./AtlasPeek.tsx";
import { EntryPage } from "./EntryPage.tsx";
import { cardOf } from "./graph.tsx";
import { PEEK_WORK, workHref } from "./Work.tsx";

afterEach(() => {
  cleanup();
  location.hash = "";
});

/** A reader of the atlas, with the apps a case gives them beside it. */
const reader = (...apps: string[]) => ({
  login: "U0ADA",
  grants: ["atlas", ...apps],
  handle: "ada",
  owner: "ada",
  name: "Ada",
  kind: "human",
  acts: [],
});

const OPEN = [
  taskRow("ACME-12", "Move orders-db to the new cluster"),
  taskRow("ACME-14", "Rotate the orders-db credentials", { status: "todo" }),
];
const DONE = [taskRow("ACME-9", "Add a read replica", { status: "done", status_group: "done" })];

function mount(
  hash: string,
  children: React.ReactNode,
  fixture: AtlasFixture,
  answers: Record<string, unknown>,
) {
  return mountAtlas(hash, children, fixture, { answers });
}

describe("the map's notch of open work", () => {
  // A CARD HAS ONE NOTCH, AND HEALTH KEEPS IT. Mutation: let open work take
  // the notch first and an alerting entry reads as merely being worked on.
  test("a quiet entry with open work carries the info notch, and an alerting one keeps its health", () => {
    const quiet = cardOf(summary("datastore", "orders-db"), 0, 2);
    expect(quiet.attention).toEqual({ tone: "info", label: "Being worked on, 2 open tasks" });
    const alerting = cardOf(summary("datastore", "orders-db", { health: "alerting" }), 0, 2);
    expect(alerting.attention).toEqual({ tone: "danger", label: "Alerting" });
    const looked = cardOf(summary("host", "web-1", { health: "needs_look" }), 0, 1);
    expect(looked.attention).toEqual({ tone: "warning", label: "Needs a look" });
    expect(cardOf(summary("host", "web-2"), 0).attention).toBeUndefined();
    expect(cardOf(summary("host", "web-2"), 0, 1).attention?.label).toBe(
      "Being worked on, 1 open task",
    );
  });
});

describe("the page's marks of open work", () => {
  test("a row whose entry open tasks name says how many, as the way to its Work tab", async () => {
    const company = grownCompany();
    const { asked } = mount("#/company/atlas", <Atlas />, company, {
      viewer: reader("worklet"),
      atlas_work: workAnswer({ [company.ordersDb.id]: 2, [company.web1.id]: 1 }),
    });
    const grid = await screen.findByRole("table", { name: /Catalogue/ });
    const mark = await within(grid).findByText("2 open tasks");
    expect(mark.closest("a")?.getAttribute("href")).toBe(workHref(company.ordersDb.id));
    expect(within(grid).getByText("1 open task")).toBeTruthy();
    // ONE QUESTION FOR THE MAP AND THE CATALOGUE ALIKE, with no parameter.
    const marks = asked.filter((a) => a.what === "atlas_work");
    expect(marks.length).toBeGreaterThan(0);
    expect(marks.every((a) => Object.keys(a.params).length === 0)).toBe(true);
  });

  // THE TRACKER IS WORKLET'S. Mutation: ask `atlas_work` of every reader and
  // the ask below is recorded.
  test("a reader without Worklet is asked nothing of the tracker, and no row is marked", async () => {
    const company = grownCompany();
    const { asked } = mount("#/company/atlas", <Atlas />, company, {
      viewer: reader(),
      atlas_work: workAnswer({ [company.ordersDb.id]: 2 }),
    });
    const grid = await screen.findByRole("table", { name: /Catalogue/ });
    await within(grid).findByText("orders-db");
    expect(asked.some((a) => a.what === "viewer")).toBe(true);
    expect(asked.some((a) => a.what === "atlas_work")).toBe(false);
    expect(within(grid).queryByText(/open task/)).toBeNull();
  });

  test("a company whose work is not kept in Worklet marks no row", async () => {
    const company = grownCompany();
    const { asked } = mount("#/company/atlas", <Atlas />, company, {
      viewer: reader("worklet"),
      atlas_work: workAnswer(
        {},
        "this company's work is not kept in Worklet, so there is none to read",
      ),
    });
    const grid = await screen.findByRole("table", { name: /Catalogue/ });
    await waitFor(() => expect(asked.some((a) => a.what === "atlas_work")).toBe(true));
    expect(within(grid).queryByText(/open task/)).toBeNull();
  });
});

describe("an entry's Work tab", () => {
  test("lists the open tasks, then the ones finished lately, each a link to its task", async () => {
    const company = grownCompany();
    const { asked } = mount(
      `#/company/atlas/${company.ordersDb.id}?tab=work`,
      <EntryPage id={company.ordersDb.id} />,
      company,
      { viewer: reader("worklet"), atlas_entry_work: entryWorkAnswer(OPEN, DONE) },
    );
    const open = await screen.findByRole("region", { name: "Open tasks (2)" });
    const done = screen.getByRole("region", { name: "Finished in the last 30 days (1)" });
    expect(open.compareDocumentPosition(done) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    const first = within(open).getByText("Move orders-db to the new cluster").closest("a")!;
    expect(first.getAttribute("href")).toBe(href(["worklet", "ACME-12"]));
    expect(within(open).getByText("ACME-14")).toBeTruthy();
    expect(within(done).getByText("ACME-9")).toBeTruthy();
    expect(asked.find((a) => a.what === "atlas_entry_work")?.params).toEqual({
      id: company.ordersDb.id,
    });
    // THE TAB COUNTS THE OPEN WORK AND SAYS IT IN WORDS.
    const tab = screen.getByRole("tab", { name: /Work/ });
    expect(tab.textContent).toContain("2");
    expect(tab.textContent).toContain("2 open tasks name this entry");
  });

  test("a list the engine cut says how many of its whole count are shown", async () => {
    const company = grownCompany();
    mount(
      `#/company/atlas/${company.ordersDb.id}?tab=work`,
      <EntryPage id={company.ordersDb.id} />,
      company,
      {
        viewer: reader("worklet"),
        atlas_entry_work: entryWorkAnswer(OPEN, DONE, {
          open_total: { total: 120 },
          recent_total: { total: 500, capped: true },
        }),
      },
    );
    expect(await screen.findByRole("region", { name: "Open tasks (2 of 120)" })).toBeTruthy();
    expect(
      screen.getByRole("region", { name: "Finished in the last 30 days (1 of 500+)" }),
    ).toBeTruthy();
  });

  test("an entry no task names says so, and how to link one", async () => {
    const company = grownCompany();
    mount(
      `#/company/atlas/${company.ordersDb.id}?tab=work`,
      <EntryPage id={company.ordersDb.id} />,
      company,
      { viewer: reader("worklet") },
    );
    expect(await screen.findByText("No task names this entry yet")).toBeTruthy();
    expect(screen.getByText(/open the task and choose Link an atlas entry/)).toBeTruthy();
  });

  // LOCKED, NEVER HIDDEN, AND NEVER ASKED. Mutation: drop the Worklet gate in
  // `useEntryWork` and the tracker is asked for a reader it is closed to.
  test("is locked for a reader without Worklet, with the access to ask for, and asks nothing", async () => {
    const company = grownCompany();
    const { asked } = mount(
      `#/company/atlas/${company.ordersDb.id}?tab=work`,
      <EntryPage id={company.ordersDb.id} />,
      company,
      { viewer: reader(), atlas_entry_work: entryWorkAnswer(OPEN, DONE) },
    );
    expect(
      await screen.findByText(
        "The work on an entry needs Worklet access. An admin can give it to you.",
      ),
    ).toBeTruthy();
    const tab = screen.getByRole("tab", { name: /Work/ });
    expect(tab.textContent).toContain("Needs Worklet access");
    expect(asked.some((a) => a.what === "atlas_entry_work")).toBe(false);
    expect(screen.queryByText("ACME-12")).toBeNull();
  });

  test("says the engine's own reason where it read no work", async () => {
    const company = grownCompany();
    mount(
      `#/company/atlas/${company.ordersDb.id}?tab=work`,
      <EntryPage id={company.ordersDb.id} />,
      company,
      {
        viewer: reader("worklet"),
        atlas_entry_work: entryWorkAnswer([], [], {
          unread: "this company's work is not kept in Worklet, so there is none to read",
        }),
      },
    );
    expect(
      await screen.findByText(
        "No work was read: this company's work is not kept in Worklet, so there is none to read.",
      ),
    ).toBeTruthy();
  });
});

describe("an entry's sheet", () => {
  test("lists the first open tasks and points at the Work tab for the rest", async () => {
    const company = grownCompany();
    const many = Array.from({ length: PEEK_WORK + 2 }, (_, i) =>
      taskRow(`ACME-${20 + i}`, `Task ${i + 1} on orders-db`),
    );
    mount(
      `#/company/atlas?peek=atlas:${company.ordersDb.id}`,
      <AtlasPeek id={company.ordersDb.id} />,
      company,
      { viewer: reader("worklet"), atlas_entry_work: entryWorkAnswer(many) },
    );
    const section = await screen.findByRole("region", { name: "Work" });
    expect(await within(section).findByText("ACME-20")).toBeTruthy();
    expect(within(section).getAllByRole("link", { name: /Task \d on orders-db/ })).toHaveLength(
      PEEK_WORK,
    );
    const more = within(section).getByRole("link", { name: "And 2 more" });
    expect(more.getAttribute("href")).toBe(workHref(company.ordersDb.id));
  });

  test("draws the section locked for a reader without Worklet", async () => {
    const company = grownCompany();
    const { asked } = mount(
      `#/company/atlas?peek=atlas:${company.ordersDb.id}`,
      <AtlasPeek id={company.ordersDb.id} />,
      company,
      { viewer: reader(), atlas_entry_work: entryWorkAnswer(OPEN) },
    );
    const section = await screen.findByRole("region", { name: /Work.*Needs Worklet access/ });
    expect(within(section).getByText(/needs Worklet access/)).toBeTruthy();
    expect(asked.some((a) => a.what === "atlas_entry_work")).toBe(false);
  });
});
