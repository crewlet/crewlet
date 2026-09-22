/**
 * One project: the facts a container has that its rows do not, and the three
 * lenses over them.
 *
 * The screen used to open with the same four-number strip and stacked census
 * card the company-wide list did, above the same board — so the first
 * screenful of a project was an overview nobody asked for and the work started
 * below the fold. What is asserted here is that the header says what the
 * container IS, that the census is drawn as a shape only where there is work
 * to shape, and that each lens answers its own question.
 */

import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { Project, ProjectPeek } from "./Project.tsx";
import { Router } from "~/app/router.tsx";
import { useClient, useConnection, useOrg } from "~/lib/store-hooks.ts";
import type { QueryName, WorkProjectDetail } from "~/protocol/index.ts";

vi.mock("~/lib/store-hooks.ts", async () => {
  const actual =
    await vi.importActual<typeof import("~/lib/store-hooks.ts")>("~/lib/store-hooks.ts");
  return { ...actual, useClient: vi.fn(), useConnection: vi.fn(), useOrg: vi.fn() };
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  location.hash = "#/";
});

function serving(
  answers: Partial<Record<QueryName, unknown>>,
  refusing: Partial<Record<QueryName, string>> = {},
) {
  const query = vi.fn(async (what: string) => {
    const refusal = refusing[what as QueryName];
    if (refusal) throw new Error(refusal);
    return answers[what as QueryName] ?? {};
  });
  vi.mocked(useClient).mockReturnValue({ socket: { query } } as never);
  vi.mocked(useConnection).mockReturnValue({ connected: true } as never);
  vi.mocked(useOrg).mockReturnValue({
    name: "Acme",
    roles: [{ name: "Ada Okonkwo", handle: "ada", kind: "agent" }],
  } as never);
  return query;
}

const detail = (over: Partial<WorkProjectDetail> = {}): WorkProjectDetail => ({
  key: "ENG",
  name: "Engineering",
  purpose: "Build and ship the product",
  unit: { key: "platform", name: "Platform", resolved: true },
  lead: { handle: "ada", kind: "agent" },
  task_counts: { open: 12, done: 40, closed: 3 },
  version: 1,
  statuses: [],
  types: [],
  fields: [],
  policy_stamp: 1,
  complete: true,
  ...over,
});

const mount = (key = "ENG") =>
  render(
    <Router>
      <Project projectKey={key} />
    </Router>,
  );

/** What `work_items` was actually asked, which is what a lens's claim rests on. */
function asked(query: ReturnType<typeof serving>): Record<string, unknown> {
  const calls = query.mock.calls as unknown as [string, Record<string, unknown>?][];
  return calls.findLast(([what]) => what === "work_items")?.[1] ?? {};
}

// A PROJECT IS AN OBJECT, so it wears the header every other object in this
// product wears: who leads it, which unit owns it, and its three counts — the
// facts a board can say none of from its rows. A HANDLE IS THE DATABASE'S WORD
// FOR A PERSON, and every other surface resolves it through the chart.
test("the header says what the container is, in the company's own words", async () => {
  serving({ work_project: detail(), work_items: { items: [], groups: [], complete: true } });
  mount();
  await waitFor(() => expect(screen.getByText("Engineering")).toBeTruthy());
  expect(screen.getByText("Ada Okonkwo")).toBeTruthy();
  expect(screen.getByText("Platform")).toBeTruthy();
  expect(screen.getByText("Build and ship the product")).toBeTruthy();
});

// A PROJECT'S CENSUS IS A SHAPE AS WELL AS THREE NUMBERS, and the bar carries
// its own legend: an unlabelled stack of three colours is three colours.
test("the census is a bar with its legend, and only where there is work", async () => {
  serving({ work_project: detail(), work_items: { items: [], groups: [], complete: true } });
  const { container } = mount();
  await waitFor(() => expect(container.querySelector(".crewlet-stacked-bar")).toBeTruthy());
  expect(container.querySelector(".crewlet-legend")).toBeTruthy();
  cleanup();

  serving({
    work_project: detail({ task_counts: { open: 0, done: 0, closed: 0 } }),
    work_items: { items: [], groups: [], complete: true },
  });
  const fresh = mount();
  await waitFor(() => expect(screen.getByText("Engineering")).toBeTruthy());
  expect(fresh.container.querySelector(".crewlet-stacked-bar")).toBeNull();
});

// ITEMS IS THE DEFAULT, because the work is what somebody opening a project
// came for — and it is the same component the company-wide list is, narrowed
// to this container.
test("the work is the lens a project opens on, scoped to this project", async () => {
  const query = serving({
    work_project: detail(),
    work_items: { items: [], groups: [], complete: true },
  });
  mount();
  await waitFor(() => expect(asked(query).container).toBe("project:ENG"));
  // THE LENS ROW BY NAME, because the Items lens brings a tab row of its own:
  // the list's view strip is drawn whether or not anybody has saved a view, and
  // its first tab is this container's own list. Read as "every tab on the
  // screen" this case would fail the day either row gains a member, which is
  // not what it is about.
  const lenses = screen.getByRole("tablist", { name: "Lens" });
  expect(
    within(lenses)
      .getAllByRole("tab")
      .map((el) => el.textContent),
  ).toEqual(["Items", "Overview", "History"]);
  expect(screen.getByRole("tab", { name: "All in this project" })).toBeTruthy();
});

// A LENS IS A SECTION, so it is in the URL: a reader who walked to the
// Overview can send it to somebody, and Back means the lens they came from.
test("a lens is an address rather than a state nobody can link to", async () => {
  location.hash = "#/work/ENG?lens=overview";
  serving({
    work_project: detail({
      statuses: [{ status: "todo", label: "Backlog", group: "not_started", description: "" }],
      types: [{ slug: "bug", name: "Defect" }],
      tags: [{ slug: "api", label: "API" }],
    }),
    work_items: { items: [], groups: [], complete: true },
    work_activity: { records: [], complete: true },
  });
  mount();
  // WHAT THIS PROJECT CALLS THINGS is the half a board cannot say from its
  // rows, and a company that renamed four statuses had no screen that said so.
  await waitFor(() => expect(screen.getByText("Backlog")).toBeTruthy());
  expect(screen.getByText("Defect")).toBeTruthy();
  expect(screen.getByText("API")).toBeTruthy();
});

// A PROJECT THAT DECLARES NO LABELS SAYS SO. An empty row under a heading is a
// project whose labels failed to load, which is a different fact.
test("a vocabulary a project does not declare is a sentence, not a gap", async () => {
  location.hash = "#/work/ENG?lens=overview";
  serving({
    work_project: detail(),
    work_items: { items: [], groups: [], complete: true },
    work_activity: { records: [], complete: true },
  });
  mount();
  await waitFor(() => expect(screen.getByText("This project declares no labels")).toBeTruthy());
  expect(screen.getByText("This project declares no fields of its own")).toBeTruthy();
});

// THE HISTORY LENS IS THE LOG SCREEN, narrowed — written twice the two would
// drift, and the drift would be invisible because both draw rows that look
// right either way.
test("the history lens asks the log about this container", async () => {
  location.hash = "#/work/ENG?lens=history";
  const query = serving({
    work_project: detail(),
    work_activity: { records: [], complete: true },
  });
  mount();
  await waitFor(() => {
    const calls = query.mock.calls as unknown as [string, Record<string, unknown>?][];
    const last = calls.findLast(([what]) => what === "work_activity")?.[1] ?? {};
    expect(last.container).toBe("project:ENG");
  });
});

// A UNIT THE CHART NO LONGER HAS is what leaves a project's work routed to
// nobody — a finding rather than a blank, on the page and in the rail alike.
test("a project naming a unit the chart lost says what that costs", async () => {
  serving({
    work_project: detail({ unit: { key: "gone", resolved: false } }),
    work_items: { items: [], groups: [], complete: true },
  });
  mount();
  await waitFor(() => expect(screen.getByText(/routes to nobody/)).toBeTruthy());
});

// NOT THE GENERIC "there is no such record". A project key reaches this from a
// pasted URL and from a bookmark as often as from a row, and WHICH key
// resolved to nothing is precisely the half a generic banner drops.
test("a key that resolves to nothing names the key", async () => {
  serving({}, { work_project: "not_found" });
  mount("NOPE");
  await waitFor(() => expect(screen.getByText(/No project called “NOPE”/)).toBeTruthy());
});

// THE PEEK AND THE PAGE READ THE SAME FACTS IN THE SAME ORDER, from one
// definition: a reader who opens the rail from the directory must not have to
// re-learn the project because the header put its owners where the row put its
// counts.
test("the rail draws the page's own facts", async () => {
  serving({
    work_project: detail(),
    work_items: { items: [], groups: [], complete: true },
    work_activity: { records: [], complete: true },
  });
  render(
    <Router>
      <ProjectPeek projectKey="ENG" />
    </Router>,
  );
  await waitFor(() => expect(screen.getByText("Engineering")).toBeTruthy());
  expect(screen.getByText("Ada Okonkwo")).toBeTruthy();
  expect(screen.getByText("Platform")).toBeTruthy();
});
