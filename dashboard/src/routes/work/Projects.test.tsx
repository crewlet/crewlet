/**
 * The directory: where the company's overview went, and what a row claims.
 *
 * The four workspace totals and the ranked bar chart used to sit above every
 * board and every list, on every visit. They are a question about the
 * CONTAINERS rather than about the work, and the chart answered only one of
 * them: it ranked projects by open work, so a project with four open and two
 * hundred done drew the same bar as one with four open and nothing else.
 *
 * Every case here is a rendering that, drawn wrong, says something true about
 * a different project — a missing lead drawn as a blank, a maintained
 * last-change instant drawn as "nothing has happened", a meter over a project
 * nobody has filed anything in — or a claim about the COMPANY that is really a
 * claim about this page: the count in the toolbar, and the row whose click was
 * meant to open a panel beside the list rather than leave it.
 */

import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { Projects } from "./Projects.tsx";
import { Router } from "~/app/router.tsx";
import { peekHref } from "~/app/frame/DetailRail.tsx";
import { useClient, useConnection, useOrg } from "~/lib/store-hooks.ts";
import type { QueryName, WorkProjectRow } from "~/protocol/index.ts";

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

const project = (over: Partial<WorkProjectRow> = {}): WorkProjectRow => ({
  key: "ENG",
  name: "Engineering",
  unit: { key: "platform", name: "Platform", resolved: true },
  lead: { handle: "ada", kind: "agent" },
  task_counts: { open: 3, done: 1, closed: 0 },
  version: 1,
  ...over,
});

const mount = () =>
  render(
    <Router>
      <Projects />
    </Router>,
  );

/** What `work_projects` was actually asked, which is the claim the segment makes. */
function asked(query: ReturnType<typeof serving>): Record<string, unknown> {
  const calls = query.mock.calls as unknown as [string, Record<string, unknown>?][];
  return calls.findLast(([what]) => what === "work_projects")?.[1] ?? {};
}

/** What the rail was opened on, read out of the address the click wrote. */
function peeked(): string | null {
  return new URLSearchParams(location.hash.split("?")[1] ?? "").get("peek");
}

/** The grid row whose key cell holds this project. */
function rowFor(key: string): HTMLElement {
  const cell = screen.getByText(key);
  return cell.closest(".grid-row, tr, [role='row']") as HTMLElement;
}

// THE TOTALS ARE ONE SENTENCE, not four tiles: they are context for the rows
// under them rather than the point of the page. Four tiles above a list is how
// the work itself came to start below the fold.
test("the workspace totals are one line over the rows", async () => {
  serving({
    work_projects: {
      projects: [
        project(),
        project({ key: "PROD", name: "Product", task_counts: { open: 2, done: 5, closed: 1 } }),
      ],
      total: 2,
      complete: true,
    },
  });
  mount();
  await waitFor(() => expect(screen.getByText(/2 projects/)).toBeTruthy());
  expect(screen.getByText(/5 open · 6 done · 1 closed/)).toBeTruthy();
});

// THE COUNT IS THE COMPANY'S, NOT THE PAGE'S. The answer carries `total` and
// `truncated` and the page read neither, so past the engine's own limit the
// sentence said "200 projects" about a company that has more, with nothing on
// screen to say the page had stopped short of it.
test("the project count is the answer's total, and a short page says so", async () => {
  serving({
    work_projects: {
      projects: [project(), project({ key: "PROD", name: "Product" })],
      total: 340,
      truncated: true,
      complete: true,
    },
  });
  mount();
  await waitFor(() => expect(screen.getByText(/2 of 340 projects/)).toBeTruthy());
  // AND WHAT THE OTHER NUMBERS COVER, which the count no longer implies: the
  // sums are over the rows that arrived.
  expect(screen.getByText(/ordered by key/)).toBeTruthy();
  cleanup();

  // A PAGE HOLDING EVERYTHING SAYS NOTHING EXTRA: "2 of 2" is one number
  // printed twice, and the note would name a limit nothing reached.
  serving({
    work_projects: {
      projects: [project(), project({ key: "PROD", name: "Product" })],
      total: 2,
      complete: true,
    },
  });
  mount();
  await waitFor(() => expect(screen.getByText(/2 projects/)).toBeTruthy());
  expect(screen.queryByText(/ordered by key/)).toBeNull();
});

// A ROW OPENS THE PEEK, which is the question a directory is read with — "is
// this the one I meant". The handler ignored its event, so the browser followed
// the row's own anchor straight afterwards: the rail was opened and destroyed
// by one click and the reader landed on the project's page every time.
test("a plain click peeks beside the list rather than leaving it", async () => {
  location.hash = "#/work/projects";
  serving({ work_projects: { projects: [project()], total: 1, complete: true } });
  const { container } = mount();
  await waitFor(() => expect(screen.getByText("Engineering")).toBeTruthy());

  // THE HREF IS THE FRAME'S OWN ADDRESS for the object, which is what the
  // peek's `Open ↗` is built from too: a row and the panel it opens can never
  // name different pages.
  const link = container.querySelector<HTMLAnchorElement>("a.row-link");
  expect(link?.getAttribute("href")).toBe(peekHref({ kind: "project", id: "ENG" }));

  const plain = new MouseEvent("click", { bubbles: true, cancelable: true });
  link?.dispatchEvent(plain);
  expect(plain.defaultPrevented).toBe(true);
  expect(peeked()).toBe("project:ENG");
  expect(location.hash.split("?")[0]).toBe("#/work/projects");

  // AND A MODIFIED CLICK FALLS THROUGH to the browser, or the anchor is a lie:
  // ⌘-click, middle-click and "copy link address" are what it is there for.
  const meta = new MouseEvent("click", { bubbles: true, cancelable: true, metaKey: true });
  link?.dispatchEvent(meta);
  expect(meta.defaultPrevented).toBe(false);
});

// THE NINTH COLUMN. It was declared `optional`, and on a screen with no Display
// menu that means reachable only by hand-editing `cols=` — while the sentence
// directly above the grid quotes the number it holds.
test("closed work is a column of the grid, not a hidden one", async () => {
  serving({
    work_projects: {
      projects: [project({ task_counts: { open: 3, done: 1, closed: 7 } })],
      total: 1,
      complete: true,
    },
  });
  const { container } = mount();
  // THE HEAD ITSELF, not the legend under the grid, which spells the same word
  // for the segment beside the fill.
  await waitFor(() =>
    expect(
      [...container.querySelectorAll(".grid-head .grid-th")].map((h) => h.textContent),
    ).toEqual([
      "Key",
      "Project",
      "Lead",
      "Unit",
      "Open",
      "Done",
      "Closed",
      "Progress",
      "Last change",
    ]),
  );
  expect(within(rowFor("ENG")).getByText("7")).toBeTruthy();
});

// A PROJECT WITH NO LEAD ROUTES ITS UNASSIGNED WORK TO NOBODY, which is a
// finding rather than a blank cell — and a unit the chart no longer has is
// the same kind of fact one column over.
test("a project nobody leads says so rather than drawing a blank", async () => {
  serving({
    work_projects: { projects: [project({ lead: {} })], total: 1, complete: true },
  });
  mount();
  await waitFor(() => expect(screen.getByText("Nobody leads this")).toBeTruthy());
});

test("a unit the chart no longer has is marked rather than printed plainly", async () => {
  serving({
    work_projects: {
      projects: [project({ unit: { key: "gone", resolved: false } })],
      total: 1,
      complete: true,
    },
  });
  mount();
  const pill = await screen.findByTitle("The current org chart has no such unit");
  expect(pill.textContent).toBe("gone");
});

// THE MAINTAINED FACT, AND ITS TWO DIFFERENT ABSENCES. A project nothing has
// ever been filed into and a project whose work predates the column are not
// the same thing, and only the first is something a reader acts on.
test("when work last changed is the engine's own fact, and its absence says which", async () => {
  serving({
    work_projects: {
      projects: [
        project({
          key: "ENG",
          last_change: { at: "2031-04-16T09:00:00Z", actor: "ada", actor_kind: "agent" },
        }),
        project({ key: "NEW", name: "New", task_counts: { open: 0, done: 0, closed: 0 } }),
        project({ key: "OLD", name: "Old", task_counts: { open: 4, done: 0, closed: 0 } }),
      ],
      total: 3,
      complete: true,
    },
  });
  mount();
  await waitFor(() => expect(screen.getByText("Nothing has been filed here")).toBeTruthy());
  // THE INSTANT AND WHO MADE IT, on the project that has one — and ON ONE LINE
  // with it, because stacked they made every row in the directory a line and a
  // half tall.
  const when = rowFor("ENG").querySelector(".work-lastchange");
  expect(when?.textContent).toMatch(/· Ada Okonkwo$/);
  // ONE LINE: the stacked shape was a `.col`, and the name below the instant.
  expect(when?.querySelector(".col")).toBeNull();
  expect(within(rowFor("NEW")).getByText("Nothing has been filed here")).toBeTruthy();
  expect(within(rowFor("OLD")).getByText("Filed before this node recorded one")).toBeTruthy();
});

// A COMMIT CAN NAME NOBODY, and the wire says so by leaving the actor out: the
// engine did it. Rendered as a blank that is a project whose last change has no
// author, which is not a state this product has.
test("a change the engine made names the engine", async () => {
  serving({
    work_projects: {
      projects: [project({ last_change: { at: "2031-04-16T09:00:00Z" } })],
      total: 1,
      complete: true,
    },
  });
  mount();
  await waitFor(() => expect(screen.getByText(/· the engine/)).toBeTruthy());
});

// AN OPERATOR IS NOT A SEAT. A write made with an API token carries the
// token's own name and the author kind `operator`, which is the whole point of
// the audit trail — so the kind travels beside the name.
test("an operator's write is marked as one", async () => {
  serving({
    work_projects: {
      projects: [
        project({
          last_change: { at: "2031-04-16T09:00:00Z", actor: "founder", actor_kind: "operator" },
        }),
      ],
      total: 1,
      complete: true,
    },
  });
  mount();
  await waitFor(() => expect(screen.getByText(/founder \(operator\)/)).toBeTruthy());
});

// A METER OVER NOTHING IS NOT A CENSUS: three zero segments draw an empty
// track that reads as a chart which failed to load rather than as a project
// nobody has filed anything in.
test("a project with nothing filed draws no meter", async () => {
  serving({
    work_projects: {
      projects: [project({ task_counts: { open: 0, done: 0, closed: 0 } })],
      total: 1,
      complete: true,
    },
  });
  const { container } = mount();
  await waitFor(() => expect(screen.getByText("Nothing filed yet")).toBeTruthy());
  expect(container.querySelector(".crewlet-stacked-bar")).toBeNull();
});

/** Every part the bar actually drew, with the colour it drew it in. */
function drawn(container: HTMLElement): { color: string; width: string }[] {
  return [...container.querySelectorAll<HTMLElement>(".crewlet-stacked-bar__segment")].map((s) => ({
    color: s.style.getPropertyValue("--crewlet-stacked-bar-segment-color"),
    width: s.style.width,
  }));
}

// THE METER IS AN AMOUNT, NOT A SHARE, which is the whole of what it claims: a
// project holding one open item and nothing else used to draw a FULL solid bar
// — 100% of its work is open — and read as a project that had finished
// everything. Nothing done must fill nothing.
test("a project with nothing done fills nothing", async () => {
  serving({
    work_projects: {
      projects: [project({ task_counts: { open: 1, done: 0, closed: 0 } })],
      total: 1,
      complete: true,
    },
  });
  const { container } = mount();
  await waitFor(() => expect(container.querySelector(".crewlet-stacked-bar")).toBeTruthy());
  // The one part drawn is the remainder, and it is the track: untinted.
  expect(drawn(container)).toEqual([{ color: "transparent", width: "100%" }]);
});

// AND THE FILL IS DONE AGAINST EVERYTHING FILED. Three of ten done is three
// tenths of the track, with closed work muted beside it and open work left as
// the track — not a third of a bar over done + closed.
test("done fills against everything filed, closed beside it", async () => {
  serving({
    work_projects: {
      projects: [project({ task_counts: { open: 6, done: 3, closed: 1 } })],
      total: 1,
      complete: true,
    },
  });
  const { container } = mount();
  await waitFor(() => expect(container.querySelector(".crewlet-stacked-bar")).toBeTruthy());
  expect(drawn(container)).toEqual([
    { color: "var(--positive)", width: "30%" },
    { color: "var(--color-data-other)", width: "10%" },
    { color: "transparent", width: "60%" },
  ]);
});

// AND THE LEGEND IS DRAWN ONCE FOR THE COLUMN rather than once per row: an
// unlabelled stack of colours is colours, and forty legends is not forty facts.
// It names WHAT FILLS the bar and nothing else — a swatch for the untinted
// remainder would be a colour that is not on it.
test("the progress column carries one legend, naming what fills", async () => {
  serving({
    work_projects: {
      projects: [project(), project({ key: "PROD", name: "Product" })],
      total: 2,
      complete: true,
    },
  });
  const { container } = mount();
  await waitFor(() => expect(container.querySelectorAll(".crewlet-stacked-bar").length).toBe(2));
  expect(container.querySelectorAll(".crewlet-legend")).toHaveLength(1);
  expect(
    [...container.querySelectorAll(".crewlet-legend__label")].map((l) => l.textContent),
  ).toEqual(["Done", "Closed"]);
});

// AN ARCHIVED PROJECT KEEPS ITS WORK AND STOPS TAKING NEW ITEMS, so it is not
// what a reader means by "the projects" — but it is still reachable, because a
// directory that hides half the company is not a directory.
//
// AND THE SEGMENT IS A QUESTION rather than a filter over the page. The engine
// omits archived projects unless it is ASKED for them (`archived` in
// `internal/tracker/projectsread.go`), so a segment that only hid rows this
// client already held showed an empty Archived tab on every company that has
// ever retired a project — the rows it was filtering were never in the answer.
test("archived projects are behind their own segment", async () => {
  const listing = {
    work_projects: {
      projects: [project(), project({ key: "OLD", name: "Retired", archived: true })],
      total: 2,
      complete: true,
    },
  };
  const query = serving(listing);
  mount();
  await waitFor(() => expect(screen.getByText("Engineering")).toBeTruthy());
  expect(screen.queryByText("Retired")).toBeNull();
  // THE ACTIVE SEGMENT ASKS FOR THE DEFAULT ANSWER, which is the active ones:
  // sending `archived=false` would be a third state the grammar does not have.
  expect(asked(query).archived).toBeUndefined();
  cleanup();

  location.hash = "#/work/projects?shown=all";
  const both = serving(listing);
  mount();
  await waitFor(() => expect(screen.getByText("Retired")).toBeTruthy());
  expect(asked(both).archived).toBe(true);
});

// A REFUSED READ IS NOT A COMPANY THAT HAS FILED NOTHING — the same rule the
// list screen keeps, and for the same reason: a reader acts on "no work" by
// filing the duplicate.
test("a refused listing is said to be a refusal", async () => {
  serving({}, { work_projects: "unavailable" });
  mount();
  await waitFor(() => expect(screen.getByText(/This is not an empty company/)).toBeTruthy());
  expect(screen.queryByText("No work has been filed yet")).toBeNull();
});

// A PAGE ABOUT CONTAINERS SAYS SO WHEN IT HAS NONE. This drew the list
// screen's "No work has been filed yet" — a sentence about ITEMS on the one
// screen whose rows are projects — so a reader was sent looking for work rather
// than for the configuration that mints a project.
test("a company with no projects is told what a project is and where one comes from", async () => {
  serving({ work_projects: { projects: [], total: 0, complete: true } });
  mount();
  await waitFor(() => expect(screen.getByText("No project has been created yet")).toBeTruthy());
  expect(screen.getByText(/declares its `project` key/)).toBeTruthy();
  expect(screen.queryByText("No work has been filed yet")).toBeNull();
  // AND IT IS SAID ONCE: the lede above the grid is what the page IS, not a
  // second copy of where a project comes from.
  expect(screen.getAllByText(/declares its `project` key/)).toHaveLength(1);
});
