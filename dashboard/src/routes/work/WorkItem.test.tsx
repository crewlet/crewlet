/**
 * What an item's properties and links CLAIM.
 *
 * The two panels here are where a task's stored values meet the words a person
 * uses for them, and every one of these cases is a value whose raw form is
 * meaningless on screen: an option's uuid under a field heading, a dependency
 * labelled from the wrong end, a zero where nobody has written anything.
 */

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { EMPTY_VALUE } from "@crewlethq/ui";
import {
  RemovedNote,
  itemFlags,
  ItemBody,
  ItemLinks,
  ItemPeek,
  ItemProps,
  Routing,
  Subtasks,
  WorkItem as WorkItemPage,
  linkHeading,
} from "./WorkItem.tsx";
import { Router, href } from "~/app/router.tsx";
import { pathOf, refToken } from "~/app/frame/objects.ts";
import { PeekHost, PeekNeighbours } from "~/app/frame/PeekHost.tsx";
import { useClient, useConnection, useOrg } from "~/lib/store-hooks.ts";
import type { QueryName, WorkItem, WorkItemDetail, WorkProjectDetail } from "~/protocol/index.ts";

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

const NOW = Date.parse("2031-04-16T12:00:00Z");
const NOW_ISO = "2031-04-16T12:00:00Z";

const task = (over: Partial<WorkItem> = {}): WorkItem => ({
  id: "t-1",
  key: "ENG-42",
  project: "ENG",
  title: "Fix the login race",
  status: "in_progress",
  version: 1,
  ...over,
});

const detail = (over: Partial<WorkItemDetail> = {}): WorkItemDetail => ({
  task: task(),
  complete: true,
  ...over,
});

const project = (over: Partial<WorkProjectDetail> = {}): WorkProjectDetail => ({
  key: "ENG",
  name: "Engineering",
  unit: { resolved: true },
  lead: {},
  task_counts: { todo: 1, active: 0, done: 0, closed: 0 },
  version: 1,
  statuses: [],
  types: [],
  fields: [
    {
      fields: [
        {
          id: "f-sev",
          slug: "severity",
          name: "Severity",
          type: "dropdown",
          config: { options: [{ id: "o-1", slug: "high", name: "High" }] },
        },
      ],
    },
  ],
  policy_stamp: 1,
  complete: true,
  ...over,
});

// A CHOICE STORES THE OPTION'S ID — which is what lets a company rename an
// option without orphaning every task that chose it — so a panel that showed
// the stored value would print a uuid under a field heading.
test("a custom field renders by its name, with its option's own word", () => {
  render(
    <ItemProps
      detail={detail({
        fields: [
          { id: "f-sev", slug: "severity", name: "Severity", type: "dropdown", value: "o-1" },
        ],
      })}
      chrome={{}}
      project={project()}
    />,
  );
  expect(screen.getByText("Severity")).toBeTruthy();
  expect(screen.getByText("High")).toBeTruthy();
  expect(screen.queryByText("o-1")).toBeNull();
});

// A VALUE A FILTER CANNOT REACH SAYS SO IN A WORD. "Archived", "from another
// tracker" and "undeclared" are three different facts, and dimming all three
// the same would invite somebody to filter on a field that cannot be filtered.
test("a value whose declaration was archived is marked as such", () => {
  render(
    <ItemProps
      detail={detail({
        fields: [
          {
            id: "f-sev",
            slug: "severity",
            name: "Severity",
            type: "dropdown",
            value: "o-1",
            hidden: true,
          },
        ],
      })}
      chrome={{}}
      project={project()}
    />,
  );
  expect(screen.getByText("archived")).toBeTruthy();
});

// AN ABSENT VALUE IS A MARKED ABSENCE, never a zero: zero is a measurement,
// and an unestimated task is one nobody has sized. The task carries a due date
// so the Plan group draws its rows at all — with nothing scheduled the group
// says so in one line instead, which is the case below.
test("an unestimated task shows a marked absence rather than a zero", () => {
  render(
    <ItemProps
      detail={detail({ task: task({ due_at: "2031-04-20T00:00:00Z" }) })}
      chrome={{}}
      project={project()}
    />,
  );
  const estimate = screen.getByText("Estimate").nextElementSibling!;
  expect(estimate.textContent).toBe(`${EMPTY_VALUE}Not set`);
  expect(estimate.textContent).not.toContain("0m");
});

// NOTHING SCHEDULED IS ONE FACT, NOT FOUR. Every Plan row dashes when unset —
// a task HAS a due date, unset — so a fresh task spent a heading, a hairline
// and four lines saying one thing four times, which in a 420px peek is most
// of the space between the header and the description. The heading stays: the
// group exists and is empty, which is not the same claim as a task that has no
// plan at all.
test("a task with no dates, estimate or size says so in one line", () => {
  const { container } = render(<ItemProps detail={detail()} chrome={{}} project={project()} />);
  expect(screen.getByText("Plan")).toBeTruthy();
  expect(screen.getByText("Nothing scheduled: no dates, no estimate, no size.")).toBeTruthy();
  for (const row of ["Start", "Due", "Estimate", "Points"]) {
    expect(screen.queryByText(row)).toBeNull();
  }
  expect(container.querySelectorAll(".props-empty")).toHaveLength(1);
});

// `none` IS A VALUE THE ENGINE MINTS. A create defaults the field to it
// (`internal/tracker/policy.go`: "a real value rather than an absent one") and
// the applier records the delta, so the change log names somebody as having
// set it — and the rail rendered a dash with that attribution underneath, the
// dashboard contradicting the engine about one row on one screen. A rail
// answers what the field is SET TO, which is the same reasoning it already
// applied to `normal`.
test("a priority of none is the word, with the change that set it", () => {
  render(
    <ItemProps
      detail={detail({
        task: task({ priority: "none" }),
        history: [
          {
            id: "h-1",
            kind: "created",
            actor: "agent-ceo",
            at: "2031-04-16T09:00:00Z",
            log_seq: 1,
            fields: { priority: { from: "", to: "none" } },
          },
        ],
      })}
      chrome={{}}
      project={project()}
    />,
  );
  const value = screen.getByText("Priority").nextElementSibling!;
  expect(value.textContent).toContain("none");
  expect(value.textContent).not.toContain(EMPTY_VALUE);
  expect(value.querySelector(".props-setby")!.textContent).toContain("set by agent-ceo");
});

// AN ABSENT PRIORITY IS STILL A DASH. No value reached this build at all,
// which is a different fact from one the engine minted, and the row carries no
// attribution because there is nothing for it to be about.
test("a task carrying no priority at all still dashes", () => {
  const { container } = render(<ItemProps detail={detail()} chrome={{}} project={project()} />);
  expect(screen.getByText("Priority").nextElementSibling!.textContent).toBe(
    `${EMPTY_VALUE}Not set`,
  );
  expect(container.querySelectorAll(".props-setby")).toHaveLength(0);
});

// THE PROJECT IS THE TASK'S ADDRESS, and the peek had no trace of it but the
// prefix inside the key — a reader who does not know the key grammar could
// neither tell which project LEAD-1 is in nor reach it, because the page's own
// way there is in the page bar and the peek renders none. The name is what a
// person calls it and the key is what everything is addressed by, so the row
// carries both.
test("the rail names the project and its key, and links to its board", () => {
  const { container } = render(
    <ItemProps detail={detail()} chrome={{}} project={project({ name: "Engineering" })} />,
  );
  const value = screen.getByText("Project").nextElementSibling!;
  expect(value.textContent).toContain("Engineering");
  expect(value.textContent).toContain("ENG");
  expect(value.querySelector("a")!.getAttribute("href")).toBe(href(["work", "ENG"]));
});

// A PANEL STILL LOADING ITS PROJECT SHOWS THE KEY rather than waiting: the
// `work_project` read is what supplies this rail's vocabulary and it answers
// after the item does, so the row would otherwise be blank on every open.
test("the project row falls back to the key alone", () => {
  render(<ItemProps detail={detail()} chrome={{}} project={null} />);
  expect(screen.getByText("Project").nextElementSibling!.textContent).toBe("ENG");
});

// A HAND-OFF COUNT IS A BUDGET, not trivia: an item handed on too many times
// has stopped being work and started being a hot potato, and the engine
// refuses the next hand-off rather than letting it circle.
test("a task near its hand-off cap is flagged", () => {
  const { container } = render(
    <ItemProps
      detail={detail({ task: task({ reassignments: 7 }) })}
      chrome={{}}
      project={project()}
    />,
  );
  expect(screen.getByText("Hand-offs")).toBeTruthy();
  expect(container.querySelector("dd svg")).toBeTruthy();
});

// WATCHING AND MUTED ARE DIFFERENT FACTS AND BOTH TRAVEL: a set carrying only
// the difference would silently re-add everybody on the next mention.
test("a muted watcher is listed and said to be muted", () => {
  render(
    <ItemProps
      detail={detail({ task: task({ watchers: ["ada", "bo"], muted: ["bo"] }) })}
      chrome={{ seatName: (h) => h }}
      project={project()}
    />,
  );
  expect(screen.getByText("Watching")).toBeTruthy();
  expect(screen.getByText("(muted)")).toBeTruthy();
});

// ROUTING IS SHOWN ONLY WHERE IT HAS MOVED. The pair being equal is the
// ordinary case, and repeating it in two rows is noise that buries the one
// task whose routing actually changed.
test("a task routed where it was filed says so once", () => {
  const { rerender } = render(
    <ItemProps
      detail={detail({ task: task({ filed_unit: "platform", routing_unit: "platform" }) })}
      chrome={{}}
      project={project()}
    />,
  );
  expect(screen.queryByText("Routes to")).toBeNull();
  rerender(
    <ItemProps
      detail={detail({ task: task({ filed_unit: "platform", routing_unit: "payments" }) })}
      chrome={{}}
      project={project()}
    />,
  );
  expect(screen.getByText("Routes to")).toBeTruthy();
});

// A TEAM IS DRAWN BY ITS NAME AND FOLLOWED BY ITS KEY.
//
// What the row holds is the unit's KEY — its `id` on a company that gave its
// units one, a word chosen so that a rename moves nothing and therefore a word
// nobody reads. The panel rendered that raw and said `eng` while the same
// company's board column said Engineering. The address keeps the key, which
// the engine matches against the whole set of that team's spellings.
test("a unit reads as the team's name and links by the stored key", () => {
  render(
    <ItemProps
      detail={detail({
        task: task({ filed_unit: "eng", routing_unit: "plat" }),
        units: {
          filed: { key: "eng", name: "Core Engineering", resolved: true },
          routing: { key: "plat", name: "Platform", resolved: true },
        },
      })}
      chrome={{}}
      project={project()}
    />,
  );
  const filed = screen.getByText("Core Engineering").closest("a");
  expect(filed?.getAttribute("href")).toContain("unit=eng");
  const routed = screen.getByText("Platform").closest("a");
  expect(routed?.getAttribute("href")).toContain("unit=plat");
  // AND THE KEY IS NOT WHAT IS READ, on either row.
  expect(screen.queryByText("eng")).toBeNull();
  expect(screen.queryByText("plat")).toBeNull();
});

// A TEAM THE CHART NO LONGER HAS IS A FINDING, marked the way the project
// directory marks the same one: the stored key in a warning tag, unlinked,
// because a team that has left the chart is something to correct rather than
// somewhere to go.
test("a unit the chart has lost is marked rather than linked", () => {
  render(
    <ItemProps
      detail={detail({
        task: task({ filed_unit: "dissolved", routing_unit: "dissolved" }),
        units: {
          filed: { key: "dissolved", resolved: false },
          routing: { key: "dissolved", resolved: false },
        },
      })}
      chrome={{}}
      project={project()}
    />,
  );
  const pill = screen.getByTitle("The current org chart has no such unit");
  expect(pill.textContent).toBe("dissolved");
  expect(pill.closest("a")).toBeNull();
});

// AND AN ANSWER WITH NO RESOLUTION BESIDE IT STILL DRAWS THE ROW, because the
// record is on the task either way: a node holding no chart answers the raw
// key, and a row that vanished would be a task filed into nothing.
test("a unit with no resolution beside it still reads and links", () => {
  render(
    <ItemProps
      detail={detail({ task: task({ filed_unit: "eng", routing_unit: "eng" }) })}
      chrome={{}}
      project={project()}
    />,
  );
  const raw = screen.getByText("eng").closest("a");
  expect(raw?.getAttribute("href")).toContain("unit=eng");
});

// ---------------------------------------------------------------------------
// Links
// ---------------------------------------------------------------------------

// THE DERIVED HALF OF A DEPENDENCY IS THE ONE NOBODY AUTHORED, so calling both
// ends "waiting on" would tell a reader their task is blocked by the very task
// it is blocking.
test("a dependency is named from the end it is read at", () => {
  expect(linkHeading({ kind: "waiting_on", other: "x" })).toBe("Waiting on");
  expect(linkHeading({ kind: "waiting_on", other: "x", derived: true })).toBe("Blocks");
  expect(linkHeading({ kind: "duplicates", other: "x" })).toBe("Duplicates");
  expect(linkHeading({ kind: "duplicates", other: "x", derived: true })).toBe("Duplicated by");
});

test("links are grouped under the word each end earns", () => {
  render(
    <ItemLinks
      detail={detail({
        links: [
          { kind: "waiting_on", other: "t-2", key: "ENG-2", title: "the blocker" },
          { kind: "waiting_on", other: "t-3", key: "ENG-3", title: "the dependent", derived: true },
        ],
      })}
      chrome={{}}
    />,
  );
  expect(screen.getByText("Waiting on")).toBeTruthy();
  expect(screen.getByText("Blocks")).toBeTruthy();
});

// THE REPAIR AND THE FINDING ARE DIFFERENT STATES: the first is a commit the
// tracker duty writes 30 seconds later, the second is an edge whose mirror was
// refused for good and which a person has to resolve.
test("a pending mirror and a permanent one-sided edge read differently", () => {
  render(
    <ItemLinks
      detail={detail({
        links: [
          { kind: "waiting_on", other: "t-2", key: "ENG-2", one_sided: true },
          {
            kind: "waiting_on",
            other: "t-4",
            key: "ENG-4",
            one_sided: true,
            one_sided_final: true,
          },
        ],
      })}
      chrome={{}}
    />,
  );
  expect(screen.getByText("mirror pending")).toBeTruthy();
  expect(screen.getByText("one-sided")).toBeTruthy();
});

// A PAGE LINK LEAVES THE TRACKER, and it has to go to the knowledge base
// rather than to a task key that does not exist.
//
// ASSERTED THROUGH `pathOf`, never against a hand-typed literal. This case
// used to re-compute the component's own string — `#/pages/p-1` — and so
// pinned a head the application has no route for: every task→page link landed
// on NotFound while the suite reported a pass. The frame's map is the one
// definition of where a page lives, and a link that does not agree with it is
// a link to nothing.
test("a linked page points at the page rather than at a task", () => {
  const { container } = render(
    <ItemLinks
      detail={detail({
        links: [{ kind: "page", other: "p-1", key: "ENG/The runbook", title: "The runbook" }],
      })}
      chrome={{}}
    />,
  );
  expect(screen.getByText("Pages")).toBeTruthy();
  expect(container.querySelector("a.mono")?.getAttribute("href")).toBe(
    href(pathOf({ kind: "page", id: "ENG/The runbook" })),
  );
});

// AND AN EDGE THAT CARRIES ONLY AN ID STILL LANDS IN THE KNOWLEDGE BASE. The
// tracker's `readLinks` resolves the other end against the TASK rows, so a
// page edge comes back with no `key` and no `title` and the row is a bare
// uuid — a screen that answers that address honestly is the knowledge base
// saying it holds no such container, never a screen that does not exist.
test("a page edge with no address resolved still leaves the tracker", () => {
  const { container } = render(
    <ItemLinks detail={detail({ links: [{ kind: "page", other: "p-1" }] })} chrome={{}} />,
  );
  expect(container.querySelector("a.mono")?.getAttribute("href")).toBe(
    href(pathOf({ kind: "page", id: "p-1" })),
  );
  expect(container.querySelector("a.mono")?.getAttribute("href")).toContain("#/knowledge/");
});

// NO LINKS IS NO PANEL. A panel headed "Links" over nothing reads as a task
// whose links failed to load.
test("a task with no links draws no panel at all", () => {
  const { container } = render(<ItemLinks detail={detail()} chrome={{}} />);
  expect(container.textContent).toBe("");
});

// A FAILED SUBTREE READ IS NOT AN EMPTY ONE.
//
// The subtasks read is the only thing on this screen that can see a task's
// children, so nothing contradicts it: a refusal that draws no panel states
// that the task is a leaf. The panel took the rows and never the error, so
// every failure rendered as the fact that a task has no subtasks.
test("a refused subtree read says so rather than looking like a leaf", () => {
  render(<Subtasks rows={[]} error="bad_params" now={NOW} chrome={{}} />);
  expect(screen.getByText("Subtasks")).toBeTruthy();
  expect(screen.getByText(/The engine refused this request/)).toBeTruthy();
});

// AND A READ THAT ANSWERED IS ALLOWED TO CONCLUDE IT. An empty answer is a
// fact about the task, so the panel stays away and the screen does not carry
// a heading over nothing.
test("a task that answered with no children draws no panel", () => {
  const { container } = render(<Subtasks rows={[]} error={null} now={NOW} chrome={{}} />);
  expect(container.textContent).toBe("");
});

// AND AN ERROR OUTRANKS ROWS ALREADY ON SCREEN, so a failing poll is never
// presented as the current shape of the tree.
test("a refused poll says so even when children were listed before", () => {
  render(
    <Subtasks
      rows={[
        {
          id: "t-2",
          key: "ENG-43",
          project: "ENG",
          title: "A child",
          type: "task",
          status: "todo",
          updated: "2031-04-16T09:00:00Z",
          version: 1,
        },
      ]}
      error="query_failed"
      now={NOW}
      chrome={{}}
    />,
  );
  expect(screen.getByText(/The engine tried to answer and failed/)).toBeTruthy();
  expect(screen.queryByText("A child")).toBeNull();
});

// WHO SET IT, WHICH IS THE THING THIS PRODUCT CAN SAY AND A TRACKER CANNOT.
// Every write here is attributed and carries its turn, so a property row can
// name the change that put the value there. The field was declared on
// `Property`, the markup was written, the class was in the stylesheet — and
// nothing ever passed one, so the line rendered on zero rows.
test("a property names the change that set it", () => {
  render(
    <ItemProps
      detail={detail({
        task: task({ assignee: "ada" }),
        history: [
          {
            id: "h-2",
            kind: "assignee",
            actor: "bo",
            actor_kind: "human",
            turn_id: "turn-7",
            at: "2031-04-16T09:00:00Z",
            log_seq: 2,
            fields: { assignee: { from: "", to: "ada" } },
          },
        ],
      })}
      chrome={{}}
      project={project()}
    />,
  );
  expect(screen.getByText(/bo/)).toBeTruthy();
});

// A PROPERTY THE VISIBLE HISTORY DOES NOT NAME CARRIES NOTHING. `history` is
// the newest fifty changes, so a value last moved before that window is a fact
// about the page size — and borrowing the oldest entry still visible would
// read as a confident sentence about something nobody can see.
test("a property no visible change names carries no attribution", () => {
  const { container } = render(
    <ItemProps
      detail={detail({
        task: task({ points: 3 }),
        history: [
          {
            id: "h-1",
            kind: "status",
            actor: "ada",
            at: "2031-04-16T09:00:00Z",
            log_seq: 1,
            fields: { status: { to: "in_progress" } },
          },
        ],
      })}
      chrome={{}}
      project={project()}
    />,
  );
  // One line, for the one property a change names — never a dash-shaped one
  // under every other row.
  expect(container.querySelectorAll(".props-setby").length).toBe(1);
});

// ---------------------------------------------------------------------------
// The two frames of one task
// ---------------------------------------------------------------------------

/** One socket answering each question with a fixture. */
function serving(answers: Partial<Record<QueryName, unknown>>) {
  const query = vi.fn(async (what: string) => answers[what as QueryName] ?? {});
  vi.mocked(useClient).mockReturnValue({ socket: { query } } as never);
  vi.mocked(useConnection).mockReturnValue({ connected: true } as never);
  vi.mocked(useOrg).mockReturnValue({
    name: "Acme",
    roles: [{ name: "Ada Okonkwo", handle: "ada", kind: "agent" }],
  } as never);
  return query;
}

// THE WAY OUT HAS TO LAND WHERE IT SAYS. "Open on the board" carried
// `?project=&item=`, the two keys this screen retired when a project became a
// path and the rail moved into the frame — so the one control promising "this
// task, on its own board" dropped the reader on the company-wide board with
// the rail shut. The project is a PATH SEGMENT and the task is the frame's
// own `peek=` token; asserted through `refToken` so the spelling has one
// definition.
test("the way out to the board names the project and opens the task", async () => {
  serving({
    work_item: { task: task({ assignee: "ada" }), complete: true },
    work_project: { key: "ENG", name: "Engineering", complete: true },
  });
  const { container } = render(
    <Router>
      <WorkItemPage id="ENG-42" />
    </Router>,
  );
  await waitFor(() => expect(screen.getByText("Open on the board →")).toBeTruthy());
  const out = [...container.querySelectorAll("a")].find(
    (a) => a.textContent === "Open on the board →",
  );
  expect(out?.getAttribute("href")).toBe(
    href(["work", "ENG"], { peek: refToken({ kind: "item", id: "ENG-42" }) }),
  );
});

// A PEEK RESOLVES ITS OWN PEOPLE. The frame mounts it from a `peek=` token and
// knows nothing about an org chart, so a peek that waited to be handed a
// resolver rendered every handle raw — assignee, reporter, watchers, comment
// authors and history actors — while the page for the same task named them.
// One person under two names, depending on which frame you opened.
test("the rail names a person, exactly as the page does", async () => {
  serving({
    work_item: {
      task: task({ assignee: "ada" }),
      // A CHANGE TOO, because the set-by line under a property is the one
      // place the rail used to print the raw handle — and without a history
      // entry the line is never drawn, so this case passed with the bug in.
      history: [
        {
          id: "h-1",
          kind: "status",
          actor: "ada",
          actor_kind: "agent",
          at: "2031-04-16T09:00:00Z",
          log_seq: 1,
          fields: { status: { from: "todo", to: "in_progress" } },
        },
      ],
      complete: true,
    },
    work_project: { key: "ENG", name: "Engineering", complete: true },
  });
  const { container } = render(
    <Router>
      <ItemPeek itemKey="ENG-42" />
    </Router>,
  );
  await waitFor(() => expect(screen.getAllByText("Ada Okonkwo").length).toBeGreaterThan(0));
  expect(screen.queryByText("ada")).toBeNull();
  // AND ON THE SET-BY LINE, read off its own element: the line is one span
  // holding the actor, the age and the turn link, so an exact-text query
  // for the bare handle matched nothing whether or not the handle was there.
  const setBy = [...container.querySelectorAll(".props-setby")].map((el) => el.textContent ?? "");
  expect(setBy.length).toBeGreaterThan(0);
  expect(setBy.every((line) => line.includes("Ada Okonkwo"))).toBe(true);
  expect(setBy.some((line) => /\bada\b/.test(line))).toBe(false);
});

// THE RAIL'S BODY IS KEYED ON ITS SUBJECT. `[` and `]` move the peek from
// task A to task B by changing one query key, so React reconciles one body
// rather than mounting another — and everything that body remembers, an open
// disclosure or a chosen tab, described A until something cleared it. Rule 14
// for a route, kept for the rail: a different object is a different mount,
// asserted on DOM identity because that is the only thing that tells a
// remount from a re-render.
test("moving the rail to another task mounts a new body", async () => {
  serving({
    work_item: { task: task({ assignee: "ada" }), complete: true },
    work_project: { key: "ENG", name: "Engineering", complete: true },
  });
  location.hash = `#/work?peek=${refToken({ kind: "item", id: "ENG-42" })}`;
  const { container } = render(
    <Router>
      <PeekNeighbours>
        <PeekHost />
      </PeekNeighbours>
    </Router>,
  );
  await waitFor(() => expect(container.querySelector(".object-head")).toBeTruthy());
  const before = container.querySelector(".object-head");
  location.hash = `#/work?peek=${refToken({ kind: "item", id: "ENG-43" })}`;
  await waitFor(() => expect(container.querySelector(".object-head")).not.toBe(before));
});

// A HEADER AND A RAIL STACKED IN ONE COLUMN ARE ONE READING. The peek's header
// carried the same five facts the rail states below it, so Status, Type and
// Assignee were each on screen twice within about a hundred pixels — a third
// of the panel above the fold spent saying the same thing again, with the
// description under it. The header keeps the identity and the state marks;
// every property is the rail's, once.
test("the peek states each property once", async () => {
  serving({
    work_item: { task: task({ type: "task" }), complete: true },
    work_project: { key: "ENG", name: "Engineering", complete: true },
  });
  const { container } = render(
    <Router>
      <ItemPeek itemKey="ENG-42" />
    </Router>,
  );
  await waitFor(() => expect(screen.getByText("Fix the login race")).toBeTruthy());
  expect(container.querySelector(".fact-line")).toBeNull();
  // The rail's row, and no second copy of it above.
  expect(screen.getAllByText("Unassigned")).toHaveLength(1);
  expect(screen.getAllByText("Status")).toHaveLength(1);
  // The identity and the state marks stay: they are what a reader lands on.
  expect(screen.getByText("ENG-42")).toBeTruthy();
});

// THE PAGE KEEPS ITS FACT LINE, which is the whole of the difference between
// the two frames. There the rail is a sticky side COLUMN beside the body
// rather than a block under the header, so the line and the rows are read
// across a gap — two readings of one object, the line to scan and the rows to
// study.
test("the page heads itself with the facts the peek leaves to the rail", async () => {
  serving({
    work_item: { task: task({ assignee: "ada" }), complete: true },
    work_project: { key: "ENG", name: "Engineering", complete: true },
  });
  const { container } = render(
    <Router>
      <WorkItemPage id="ENG-42" />
    </Router>,
  );
  await waitFor(() => expect(container.querySelector(".fact-line")).not.toBeNull());
  const line = container.querySelector(".fact-line")!;
  expect(line.textContent).toContain("Status");
  expect(line.textContent).toContain("Assignee");
});

// THE BODY IS NOT REBUILT ON EVERY TICK OF THE SCREEN'S CLOCK. Both frames of
// a task hold a live `now` for their relative times, so this component
// re-renders once a second — and the wrapper around the description used to be
// a component DEFINED INSIDE the render, so its type identity was new each
// time and React tore the subtree down and built it again. A reader selecting
// a sentence to copy lost the selection within a second. Asserted on DOM node
// identity, which is the only thing that distinguishes a re-render from a
// remount.
test("the description keeps its own DOM across a clock tick", async () => {
  serving({ work_items: { items: [], complete: true } });
  const body = (now: number) => (
    <Router>
      <ItemBody
        detail={detail({ task: task({ body: "the runbook is **here**" }) })}
        chrome={{}}
        now={now}
      />
    </Router>
  );
  const { container, rerender } = render(body(NOW));
  await waitFor(() => expect(container.querySelector(".prose")).toBeTruthy());
  const before = container.querySelector(".prose");
  rerender(body(NOW + 1000));
  expect(container.querySelector(".prose")).toBe(before);
});

// A TASK IN THE TRASH IS SAID TO BE IN THE TRASH.
//
// The detail read does not filter removed tasks, so a removed task's page
// resolves and answers exactly like a live one's — and the wire has always
// carried the tombstone while the client type had no field for it. A task
// somebody deleted rendered as an ordinary open task: a reader could comment
// on it, wonder why it was on no board, and never be told.
test("a removed task is marked, and a live one carries no such mark", () => {
  const { container } = render(
    <>
      {itemFlags(
        detail({
          task: task({ removed: { by: "ada", kind: "human", at: "2031-04-15T09:00:00Z" } }),
        }),
      )}
    </>,
  );
  expect(container.textContent).toContain("In the trash");

  cleanup();
  const live = render(<>{itemFlags(detail())}</>);
  expect(live.container.textContent ?? "").not.toContain("In the trash");
});

// AND THE NOTE SAYS WHO, WHEN, AND THAT IT CAN BE UNDONE — the last being the
// fact that decides what a reader does next. A removal is reversible at any
// age and nothing is destroyed, so this is a state rather than the end of the
// record.
test("the removal note names its author and says it is reversible", () => {
  render(<RemovedNote tomb={{ by: "ada", kind: "human", at: "2031-04-15T09:00:00Z" }} now={NOW} />);
  expect(screen.getByText(/ada/)).toBeTruthy();
  expect(screen.getByText(/reversible at any age/)).toBeTruthy();
});

// A TASK THAT WENT WITH ITS CONTAINER SAYS SO. `removed_with` names the parent
// whose removal took this one along, which is the difference between somebody
// deleting this task and somebody deleting the epic above it — and it decides
// whether restoring this one alone is even the right move.
test("a task removed alongside its parent names the parent", () => {
  render(
    <RemovedNote
      tomb={{ by: "ada", kind: "human", at: "2031-04-15T09:00:00Z", removed_with: "ENG-1" }}
      now={NOW}
    />,
  );
  expect(screen.getByText("ENG-1")).toBeTruthy();
});

// THE ACCENT SAYS WHERE THE READER IS, and this panel is about other people.
//
// `variant="brand"` resolves to `--color-brand-accent-soft` under
// `--color-brand-accent-ink`, which is the pair a pressed FilterChip takes — so
// a brand pill here drew the ground of a switched-on filter, beside a list that
// really is a selection. Whether a notice ASKED is a fact about the notice: it
// gets the caution tone and a word of its own.
test("the Woke panel spends no accent, and 'asks' is its own mark", () => {
  const { container } = render(
    <Routing
      answer={{
        record_id: "r-1",
        held: true,
        notified: true,
        delivery: "reached",
        addressed: 1,
        fallback: 0,
        recipients: [
          { handle: "fe", reason: "assignee", addressed: true },
          { handle: "pm", reason: "reporter", addressed: false },
        ],
      }}
      chrome={{}}
    />,
  );
  expect(container.querySelectorAll(".crewlet-tag--brand")).toHaveLength(0);
  // The fact the tint carried is still on the screen, in a word.
  expect(screen.getAllByText("asks")).toHaveLength(1);
  expect(screen.getByText("1 asked")).toBeTruthy();
  // Both reasons read the same: the word is the reason, the obligation is the
  // mark beside it.
  for (const reason of ["assignee", "reporter"]) {
    const pill = screen.getByText(reason).closest(".crewlet-tag");
    expect(pill?.className, reason).toContain("crewlet-tag--outline");
    expect(pill?.className, reason).not.toContain("crewlet-tag--brand");
  }
});

// THE HISTORY DRAWS A MARK PER KIND AND NEVER REPRINTS THE THREAD.
//
// All eight rows drew the same timeline glyph — pixel-identical across every one
// — so a comment and a field change were visually the same event. And only the
// twelve task fields `TaskDeltas` compares produce deltas, so every other kind
// fell through to the change's EXCERPT, which for the whole comment family is
// the comment body: the History tab printed the Thread tab back, one clipped
// line per comment, and the row never said what the change WAS.
test("the history draws a mark per kind and never reprints the thread", async () => {
  serving({ work_items: { items: [], complete: true } });
  const { container } = render(
    <Router>
      <ItemBody
        detail={detail({
          history: [
            {
              id: "h1",
              kind: "comment",
              at: NOW_ISO,
              log_seq: 1,
              excerpt: "the login race is a double-submit",
            },
            {
              id: "h2",
              kind: "status",
              at: NOW_ISO,
              log_seq: 2,
              fields: { status: { from: "todo", to: "done" } },
            },
            { id: "h3", kind: "watchers", at: NOW_ISO, log_seq: 3 },
            {
              id: "h4",
              kind: "assignee",
              at: NOW_ISO,
              log_seq: 4,
              turn_id: "turn-7",
              fields: { assignee: { from: "", to: "ada" } },
            },
          ],
        })}
        chrome={{}}
        now={NOW}
      />
    </Router>,
  );
  fireEvent.click(await screen.findByRole("tab", { name: /History/ }));

  // FOUR KINDS, FOUR DRAWINGS. Today every row renders the same `timeline`
  // path, so this set has one member.
  const drawings = new Set(
    [...container.querySelectorAll(".work-hist-row > svg path")].map((p) => p.getAttribute("d")),
  );
  expect(drawings.size).toBe(4);

  // THE COMMENT ROW SAYS WHAT THE CHANGE WAS, not what was said.
  expect(screen.queryByText(/double-submit/)).toBeNull();
  expect(screen.getByText(/commented/)).toBeTruthy();
  // AND A KIND WITH NO DELTAS IS A SENTENCE, not a bare noun after a name.
  expect(screen.getByText(/changed the watchers/)).toBeTruthy();

  // THE CONTROL IS OUT OF THE TRUNCATING CELL, so an entry long enough to fill
  // the track cannot eat it.
  expect(container.querySelector(".work-hist-what a")).toBeNull();
  expect(container.querySelector(".work-hist-tail a")).toBeTruthy();
});

// AND IT NAMES THE OTHER TASK, rather than printing the uuid the delta holds.
//
// A re-parent, a cascade removal and every relation delta carry the other end
// by its ID — a key belongs to that task's own row, and a history row is
// written once by every node and repaired by nothing — so the ANSWER resolves
// what this node holds and the sentence reads it from `detail.keys`. Handed no
// resolver, these two tabs drew `Parent: — → 1d573f85-…` while `#/work/history`
// drew `Parent: — → ENG-1` for the same commit, one click away.
test("a re-parent names the parent, and an unresolved id stays an id", async () => {
  serving({ work_items: { items: [], complete: true } });
  render(
    <Router>
      <ItemBody
        detail={detail({
          keys: { "t-parent": "ENG-1" },
          history: [
            {
              id: "h1",
              kind: "reparented",
              at: NOW_ISO,
              log_seq: 1,
              fields: { parent: { from: "", to: "t-parent" } },
            },
            // AN ID THE ANSWER DID NOT RESOLVE RENDERS AS THE ID — past the
            // map's cap, or a counterparty this node has not applied. A blank
            // there would read as a task with no name.
            {
              id: "h2",
              kind: "relations",
              at: NOW_ISO,
              log_seq: 2,
              fields: { waiting_on: { from: "", to: "t-unapplied" } },
            },
          ],
        })}
        chrome={{}}
        now={NOW}
      />
    </Router>,
  );
  fireEvent.click(await screen.findByRole("tab", { name: /History/ }));

  expect(screen.getByText(new RegExp(`Parent: ${EMPTY_VALUE} → ENG-1`))).toBeTruthy();
  expect(screen.queryByText(/t-parent/)).toBeNull();
  expect(screen.getByText(new RegExp(`Waiting on: ${EMPTY_VALUE} → t-unapplied`))).toBeTruthy();
});
