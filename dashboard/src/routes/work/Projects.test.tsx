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

/** A company with nothing filed, as the engine counts it. */
const zero = { active: 0, archived: 0 };

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
  expect(screen.getByText(/in the order asked for/)).toBeTruthy();
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
  expect(screen.queryByText(/in the order asked for/)).toBeNull();
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
    ).toEqual(["Key", "Project", "Lead", "Open", "Done", "Closed", "Progress", "Last change"]),
  );
  expect(within(rowFor("ENG")).getByText("7")).toBeTruthy();
});

// AND UNIT IS THE ONE THAT IS OPTIONAL, because on a chart-owned company it is
// the Project column again.
//
// The engine mints a project the moment a unit declares its `project` key and
// names it after the unit, so every row of such a company read `Core` / `Core`
// and `Executives` / `Executives` — two of nine columns spending their width
// on one fact. It is a column rather than a deletion because the two names do
// differ where a project is a SEAT's, and that company asks for it by address.
test("the unit column is off until cols asks for it", async () => {
  serving({ work_projects: { projects: [project()], total: 1, complete: true } });
  const { container } = mount();
  await waitFor(() => expect(screen.getByText("Engineering")).toBeTruthy());
  expect(
    [...container.querySelectorAll(".grid-head .grid-th")].map((h) => h.textContent),
  ).not.toContain("Unit");
  // AND THE PROJECT'S OWN UNIT IS NOT ON SCREEN EITHER, which is the point:
  // the head going without the cell would leave a value under no name.
  expect(screen.queryByText("Platform")).toBeNull();
  cleanup();

  // `cols=` CARRIES THE ORDER AS WELL AS THE SELECTION, so an address that
  // wants Unit names the whole set it wants.
  location.hash = "#/work/projects?cols=key,name,lead,unit,open,done,closed,progress,last_change";
  serving({ work_projects: { projects: [project()], total: 1, complete: true } });
  const withUnit = mount();
  await waitFor(() => expect(screen.getByText("Engineering")).toBeTruthy());
  expect(
    [...withUnit.container.querySelectorAll(".grid-head .grid-th")].map((h) => h.textContent),
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
  ]);
  expect(screen.getByText("Platform")).toBeTruthy();
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
  // WITH THE COLUMN ASKED FOR, since Unit is `optional`: this is a case about
  // what the CELL claims, and the company that turns the column on is exactly
  // the one whose units and project names can disagree.
  location.hash = "#/work/projects?cols=key,name,lead,unit,open,done,closed,progress,last_change";
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

// THE SEGMENT IS THE QUESTION, and each one names the engine's own mode.
//
// `archived=` SELECTS a set (`tracker.ArchivedMode`), so the segment is a
// parameter rather than a narrowing of a wider answer. It used to be both: the
// page asked for the widened set and then filtered what came back, so past the
// engine's own 200 the page it filtered held no archived row at all and the
// Archived segment said "No project is archived" about a company that had
// retired dozens.
test("each segment asks the engine for its own archival set", async () => {
  for (const [hash, want] of [
    ["#/work/projects", "false"],
    ["#/work/projects?shown=active", "false"],
    ["#/work/projects?shown=archived", "only"],
    ["#/work/projects?shown=all", "true"],
  ] as const) {
    location.hash = hash;
    const query = serving({
      work_projects: { projects: [project()], total: 1, complete: true },
    });
    mount();
    await waitFor(() => expect(screen.getByText("Engineering")).toBeTruthy());
    expect(asked(query).archived).toBe(want);
    cleanup();
  }
});

// AND NOTHING IS NARROWED AFTERWARDS. The rows the engine sent ARE the segment,
// so every one of them is drawn — a client filter over an answer that already
// selected would be a second, invisible narrowing, and on the Archived segment
// it was the one that emptied the screen.
test("the archived segment draws every row the engine answered with", async () => {
  location.hash = "#/work/projects?shown=archived";
  serving({
    work_projects: {
      projects: [
        project({ key: "OLD", name: "Retired", archived: true }),
        // NO `archived` FLAG ON THE WIRE, which is what the engine sends
        // for a row whose column is false — and `omitempty` means a
        // client that re-derived the segment from it would drop a row
        // the engine put in the archived answer.
        project({ key: "GONE", name: "Wound down" }),
      ],
      total: 2,
      complete: true,
    },
  });
  mount();
  await waitFor(() => expect(screen.getByText("Retired")).toBeTruthy());
  expect(screen.getByText("Wound down")).toBeTruthy();
  // AND THE TOTAL IS THE ARCHIVED SET'S OWN, which is what makes "N of M"
  // readable on this segment at all: M used to count the whole company.
  expect(screen.getByText(/2 projects/)).toBeTruthy();
});

// THE ORDER IS THE ENGINE'S, because the answer is a PAGE. A sort applied here
// orders the rows that survived the key order, so `-open` meant "the most open
// work among the projects whose keys sort first".
test("the ordering is sent to the engine and not applied to the page", async () => {
  location.hash = "#/work/projects";
  const query = serving({
    work_projects: { projects: [project()], total: 1, complete: true },
  });
  mount();
  await waitFor(() => expect(screen.getByText("Engineering")).toBeTruthy());
  // WHERE THE PILE IS, which is what this directory opens on.
  expect(asked(query).sort).toBe("-open");
  cleanup();

  location.hash = "#/work/projects?sort=last_change";
  const asc = serving({
    work_projects: { projects: [project()], total: 1, complete: true },
  });
  mount();
  await waitFor(() => expect(screen.getByText("Engineering")).toBeTruthy());
  expect(asked(asc).sort).toBe("last_change");
  cleanup();

  // AND A KEY THE ENGINE DOES NOT TAKE FALLS BACK rather than being sent. A
  // URL outlives a build and is hand-editable; sent, it would meet a
  // `bad_params` refusal, which the frame draws as the screen being at fault
  // and offers no retry for — a whole directory lost to one stale query key.
  location.hash = "#/work/projects?sort=-lead";
  const stale = serving({
    work_projects: { projects: [project()], total: 1, complete: true },
  });
  mount();
  await waitFor(() => expect(screen.getByText("Engineering")).toBeTruthy());
  expect(asked(stale).sort).toBe("-open");
});

// THE ROWS ARE NOT RE-SORTED HERE. `serverSorted` is what says so, and without
// it the grid re-orders the page it was handed the moment `sort=` names a
// column — which on a truncated answer is the same page-ordering bug one layer
// down.
test("the grid draws the engine's order rather than re-sorting it", async () => {
  location.hash = "#/work/projects?sort=-open";
  serving({
    work_projects: {
      // THE ENGINE'S ORDER, deliberately NOT what `-open` would produce
      // on the client: 1 before 9. A grid that re-sorted would put ENG
      // first and the assertion below would catch it.
      projects: [
        project({ key: "PROD", name: "Product", task_counts: { open: 1, done: 0, closed: 0 } }),
        project({ key: "ENG", task_counts: { open: 9, done: 0, closed: 0 } }),
      ],
      total: 2,
      complete: true,
    },
  });
  const { container } = mount();
  await waitFor(() => expect(screen.getByText("Product")).toBeTruthy());
  expect([...container.querySelectorAll(".grid-row .key-mark")].map((c) => c.textContent)).toEqual([
    "PROD",
    "ENG",
  ]);
});

// EVERY ORDERING THE ENGINE TAKES IS A HEAD SOMEBODY CAN CLICK, and no other
// head is one.
//
// `DataGrid` makes a head a BUTTON exactly where the column carries
// `sortValue`, so the two lists have to be the same list. A head offering a
// key the engine refuses turns one click into a `bad_params` refusal over the
// whole screen; a key the engine grew with no head is an ordering nobody can
// reach. The engine half of this pair is a Go gate over `PROJECT_SORT_KEYS`
// (`internal/tracker/client_gate_test.go`).
test("the sortable heads are exactly the orderings the engine takes", async () => {
  // THE WHOLE COLUMN SET, because Unit is `optional` and off by default: the
  // pairing this holds is between the engine's seven keys and the heads the
  // screen CAN draw, and a head the reader has to ask for is still a head.
  // Read against the default set alone, the gate would report `sort=unit` as
  // an ordering nobody can reach — which is the opposite of true, since the
  // address that turns the column on is the address that sorts by it.
  location.hash = "#/work/projects?cols=key,name,lead,unit,open,done,closed,progress,last_change";
  serving({ work_projects: { projects: [project()], total: 1, complete: true } });
  const { container } = mount();
  await waitFor(() => expect(screen.getByText("Engineering")).toBeTruthy());
  const clickable = [...container.querySelectorAll(".grid-head button.grid-th")].map(
    (h) => h.textContent,
  );
  expect(clickable).toEqual(["Key", "Project", "Unit", "Open", "Done", "Closed", "Last change"]);
  // AND THE TWO THAT ARE NOT: a project's Lead is resolved against the org
  // chart at read time and the tracker holds no chart, so there is no column
  // to order by; Progress is a proportion, and one over four tasks and one
  // over four hundred are the same number and not the same fact.
  const plain = [...container.querySelectorAll(".grid-head [role='columnheader']")].map(
    (h) => h.textContent,
  );
  expect(plain).toEqual(["Lead", "Progress"]);
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
//
// AND ONLY `All` CAN SAY IT. Each segment asks for its own set now, so an
// empty Active answer is either a company with no projects or a company that
// has archived every one of them — and telling the second reader "no project
// has been created yet" is the opposite of what the Archived segment beside it
// would show them.
// AND IT IS SAID ON WHICHEVER SEGMENT THE READER IS ON — which is Active,
// because that is where the directory lands.
//
// The census is what makes it sayable from anywhere: `active + archived === 0`
// is the COMPANY having nothing, where an empty answer is only ever the
// SEGMENT having nothing. Gated on All, this greeted a brand-new company with
// a grid's empty state on the one segment it actually opens on.
test("a company with no projects is told what a project is and where one comes from", async () => {
  for (const hash of [
    "#/work/projects",
    "#/work/projects?shown=active",
    "#/work/projects?shown=archived",
    "#/work/projects?shown=all",
  ]) {
    location.hash = hash;
    serving({
      work_projects: { projects: [], total: 0, census: zero, complete: true },
    });
    mount();
    await waitFor(() => expect(screen.getByText("No project has been created yet")).toBeTruthy());
    expect(screen.getByText(/declares its `project` key/)).toBeTruthy();
    expect(screen.queryByText("No work has been filed yet")).toBeNull();
    // AND IT IS SAID ONCE: the lede above the grid is what the page IS, not
    // a second copy of where a project comes from.
    expect(screen.getAllByText(/declares its `project` key/)).toHaveLength(1);
    cleanup();
  }
});

// AN EMPTY ACTIVE SEGMENT ON A COMPANY THAT HAS ARCHIVED EVERYTHING SAYS SO,
// WITH THE COUNT AND A WAY THERE.
//
// This is what the census bought. The page could not tell "no projects" from
// "every project archived", so it hedged — one sentence naming both, sending
// the reader to go and look. A hedge is what a screen writes when it is
// missing a number; it has the number now.
test("an empty active segment says how many are archived and links to them", async () => {
  location.hash = "#/work/projects?shown=active&sort=name";
  serving({
    work_projects: {
      projects: [],
      total: 0,
      census: { active: 0, archived: 4 },
      complete: true,
    },
  });
  mount();
  await waitFor(() => expect(screen.getByText("No project is active")).toBeTruthy());
  // NOT the company-has-nothing page: this company has four.
  expect(screen.queryByText("No project has been created yet")).toBeNull();
  expect(screen.getByText(/All 4 of the company’s projects have been archived/)).toBeTruthy();

  // A REAL LINK, so it is middle-clickable and copyable like every other way
  // into a segment — and THE REST OF THE QUERY SURVIVES, so a reader who
  // sorted does not lose it to a sentence that was only ever about the set.
  const link = screen.getByText("see them under Archived") as HTMLAnchorElement;
  expect(link.tagName).toBe("A");
  const query = new URLSearchParams(link.getAttribute("href")!.split("?")[1]);
  expect(query.get("shown")).toBe("archived");
  expect(query.get("sort")).toBe("name");
});

// AND ONE ARCHIVED PROJECT IS NOT "ALL 1", because a count in a sentence is
// prose and prose has a singular.
test("a company with one archived project is not told about all 1 of them", async () => {
  location.hash = "#/work/projects?shown=active";
  serving({
    work_projects: { projects: [], total: 0, census: { active: 0, archived: 1 }, complete: true },
  });
  mount();
  await waitFor(() =>
    expect(screen.getByText(/The company’s one project has been archived/)).toBeTruthy(),
  );
});

// AND AN EMPTY ARCHIVED SEGMENT IS ITS OWN SENTENCE, which is only reachable
// now that the engine answers the archived set rather than the page this
// client filtered.
test("an empty archived segment says nothing is archived", async () => {
  location.hash = "#/work/projects?shown=archived";
  serving({
    work_projects: { projects: [], total: 0, census: { active: 3, archived: 0 }, complete: true },
  });
  mount();
  await waitFor(() => expect(screen.getByText("No project is archived")).toBeTruthy());
  // NOT the company-has-nothing page: three are active.
  expect(screen.queryByText("No project has been created yet")).toBeNull();
});

// THE SEGMENTS CARRY THE CENSUS, so the switch says what is behind each option
// before it is pressed — on a control whose whole job is to change the set.
test("the segments carry the census counts", async () => {
  serving({
    work_projects: {
      projects: [project()],
      total: 1,
      census: { active: 1, archived: 12 },
      complete: true,
    },
  });
  const { container } = mount();
  await waitFor(() => expect(screen.getByText("Engineering")).toBeTruthy());
  const options = [...container.querySelectorAll(".segmented button")];
  expect(options.map((o) => o.textContent)).toEqual(["Active1", "Archived12", "All"]);
  // THE COUNT IS INSIDE THE RADIO, so the option announces "Archived 12"
  // rather than leaving the figure as loose text beside a control.
  expect(options[1]?.querySelector(".count-chip")?.textContent).toBe("12");
  // AND `All` CARRIES NONE: its count is the two beside it added up, which is
  // arithmetic on screen rather than a fact.
  expect(options[2]?.querySelector(".count-chip")).toBeNull();
});

// AND NO COUNTS BEFORE THE ENGINE HAS ANSWERED. Three zeroes on a control
// read as a company with nothing, which is the one claim a screen must not
// make while it is still asking.
test("the segments carry no counts until the census arrives", async () => {
  serving({ work_projects: { projects: [project()], total: 1, complete: true } });
  const { container } = mount();
  await waitFor(() => expect(screen.getByText("Engineering")).toBeTruthy());
  expect(container.querySelectorAll(".segmented .count-chip")).toHaveLength(0);
});
