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
 * nobody has filed anything in.
 */

import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { Projects } from "./Projects.tsx";
import { Router } from "~/app/router.tsx";
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
  // THE INSTANT AND WHO MADE IT, on the project that has one.
  expect(within(rowFor("ENG")).getAllByText("Ada Okonkwo").length).toBeGreaterThan(0);
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
  await waitFor(() => expect(screen.getByText("the engine")).toBeTruthy());
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

// AND THE LEGEND IS DRAWN ONCE FOR THE COLUMN rather than once per row: an
// unlabelled stack of three colours is three colours, and forty legends is not
// forty facts.
test("the progress column carries one legend under the grid", async () => {
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

test("a listing that answered with nothing says the company has filed nothing", async () => {
  serving({ work_projects: { projects: [], total: 0, complete: true } });
  mount();
  await waitFor(() => expect(screen.getByText("No work has been filed yet")).toBeTruthy());
});
