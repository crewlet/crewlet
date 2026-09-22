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

/** Whether the list was rendered at all — it is what asks `work_items`. */
function listRan(query: ReturnType<typeof serving>): boolean {
  const calls = query.mock.calls as unknown as [string, Record<string, unknown>?][];
  return calls.some(([what]) => what === "work_items");
}

/** Document order, which is what "under the header" means. */
function precedes(first: Element | null, second: Element | null): boolean {
  if (!first || !second) return false;
  return (first.compareDocumentPosition(second) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0;
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

// THE PURPOSE IS THE OBJECT'S LEDE, and it sits where a seat's goal sits: a
// PageNote under the header, above everything else the page draws. It used to
// come AFTER the census, so a chart stood between the object's name and the
// sentence saying what it is for.
test("the purpose is the sentence under the name, above the census", async () => {
  serving({ work_project: detail(), work_items: { items: [], groups: [], complete: true } });
  const { container } = mount();
  await waitFor(() => expect(screen.getByText("Build and ship the product")).toBeTruthy());
  const note = container.querySelector(".page-note");
  expect(note?.textContent).toBe("Build and ship the product");
  expect(precedes(container.querySelector(".object-head"), note)).toBe(true);
  expect(precedes(note, container.querySelector(".work-census"))).toBe(true);
});

// AND A PROJECT THAT DECLARES NONE STILL HAS A LEDE. A project minted from a
// SEAT's `project` key never carries a purpose at all — the chart has nowhere
// to write one — so a page with a hole where its lede goes was the ordinary
// case, not the exception. The fallback names the unit that owns it, and says
// when nothing has been filed.
test("a project with no purpose says whose it is, and whether anything is in it", async () => {
  serving({
    work_project: detail({ purpose: undefined, task_counts: { open: 0, done: 0, closed: 0 } }),
    work_items: { items: [], groups: [], complete: true },
  });
  const { container } = mount();
  await waitFor(() => expect(container.querySelector(".page-note")).toBeTruthy());
  expect(container.querySelector(".page-note")?.textContent).toBe(
    "ENG is Platform's project. Nothing has been filed in it yet.",
  );
  cleanup();

  // WITH WORK IN IT, the second half is what would fill the missing sentence.
  serving({
    work_project: detail({ purpose: undefined }),
    work_items: { items: [], groups: [], complete: true },
  });
  const filled = mount();
  await waitFor(() => expect(filled.container.querySelector(".page-note")).toBeTruthy());
  expect(filled.container.querySelector(".page-note")?.textContent).toMatch(
    /^ENG is Platform's project\. A `purpose` on that unit/,
  );
});

// A PROJECT WITH NOTHING FILED IN IT IS ITS OWN STATE, drawn from the
// container's maintained counts rather than from a list that came back short —
// so it is on screen before the grid has answered, and it REPLACES the list,
// whose own "Nothing matches" is a claim about filters nobody set.
test("an empty project says so instead of running the list", async () => {
  const query = serving({
    work_project: detail({ task_counts: { open: 0, done: 0, closed: 0 } }),
    work_items: { items: [], groups: [], complete: true },
  });
  mount();
  await waitFor(() =>
    expect(screen.getByText("No work has been filed in Engineering yet")).toBeTruthy(),
  );
  expect(screen.getByText(/create_work_item/)).toBeTruthy();
  // AND THE WAY OUT, in the state itself rather than only in the page's own
  // actions: a reader who opened the wrong key is one click from the company's.
  const state = screen.getByText("No work has been filed in Engineering yet").closest("div");
  expect(state?.querySelector("a")?.textContent).toBe("All work →");
  expect(listRan(query)).toBe(false);
});

// EXCEPT IN THE TRASH, which the maintained counts cannot see: a removed task
// leaves them, so a project whose every item was removed counts zero while the
// trash has rows. Replacing the list there would hide what the reader went
// looking for.
test("a project whose work was all removed still opens its trash", async () => {
  location.hash = "#/work/ENG?removed=true";
  const query = serving({
    work_project: detail({ task_counts: { open: 0, done: 0, closed: 0 } }),
    work_items: { items: [], groups: [], complete: true },
    work_activity: { records: [], complete: true },
  });
  mount();
  await waitFor(() => expect(listRan(query)).toBe(true));
  expect(screen.queryByText("No work has been filed in Engineering yet")).toBeNull();
});

// THE LENS SAYS HOW MUCH IS BEHIND IT, from the count the header already
// holds. Overview is a description rather than a collection and History is
// paged, so neither takes one — a count of a loaded page would read as a count
// of the lens.
test("the Items lens carries the open count, and the other two carry none", async () => {
  serving({ work_project: detail(), work_items: { items: [], groups: [], complete: true } });
  mount();
  // THE LENS ROW BY NAME. The Items lens draws the list's own view strip
  // whether or not anybody has saved a view, so "every tab on the screen" is
  // more than these three and is not what this case is about.
  await waitFor(() => expect(screen.getByRole("tablist", { name: "Lens" })).toBeTruthy());
  const tabs = within(screen.getByRole("tablist", { name: "Lens" })).getAllByRole("tab");
  expect(tabs.length).toBe(3);
  expect(tabs[0]?.textContent).toBe("Items12");
  expect(tabs[1]?.textContent).toBe("Overview");
  expect(tabs[2]?.textContent).toBe("History");
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
  ).toEqual(["Items12", "Overview", "History"]);
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
// counts. THE PARTS ARE ONE ORDER TOO — header, lede, findings, census — and
// they were two: the page drew the census before the purpose and the rail drew
// it after, and the page's warning callout sat above the object's own name.
test("the rail draws the page's own facts, in the page's own order", async () => {
  serving({
    work_project: detail({ unit: { key: "gone", resolved: false } }),
    work_items: { items: [], groups: [], complete: true },
    work_activity: { records: [], complete: true },
  });
  const { container } = render(
    <Router>
      <ProjectPeek projectKey="ENG" />
    </Router>,
  );
  await waitFor(() => expect(screen.getByText("Engineering")).toBeTruthy());
  expect(screen.getByText("Ada Okonkwo")).toBeTruthy();
  const head = container.querySelector(".object-head");
  const note = container.querySelector(".page-note");
  const banner = screen.getByText(/routes to nobody/).closest(".crewlet-callout");
  expect(precedes(head, note)).toBe(true);
  expect(precedes(note, banner)).toBe(true);
  expect(precedes(banner, container.querySelector(".work-census"))).toBe(true);
});

// AND THE RAIL SAYS WHY THERE IS NO CENSUS, which the page does not: the page
// draws its own empty state in place of the list a few lines below, and the
// rail has no lens under it to carry the sentence.
test("the rail says an empty project is empty, where the page's list does", async () => {
  serving({
    work_project: detail({ task_counts: { open: 0, done: 0, closed: 0 } }),
    work_activity: { records: [], complete: true },
  });
  const { container } = render(
    <Router>
      <ProjectPeek projectKey="ENG" />
    </Router>,
  );
  await waitFor(() => expect(screen.getByText("No work has been filed here yet.")).toBeTruthy());
  expect(container.querySelector(".work-census")).toBeNull();
});
