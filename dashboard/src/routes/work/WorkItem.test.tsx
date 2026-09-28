/**
 * One task, whole: what its page and its peek CLAIM, and the changes a person
 * makes on it as themselves.
 *
 * The rail is where a task's stored values meet the words a person uses for
 * them, and every case there is a value whose raw form is meaningless on
 * screen: an option's uuid under a field heading, a dependency labelled from
 * the wrong end, a zero where nobody wrote anything, a team's key where its
 * name belongs. The page is where the task's three histories meet: a turn card
 * that must say what the turn did and what the reviewer asked for, a thread
 * past its first page, and the one turn running on THIS task now.
 */

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import type { ReactNode } from "react";
import { EMPTY_VALUE, LayerHost, ToastProvider } from "@crewlethq/ui";

import { ItemPeek, RemovedNote, WorkItem as WorkItemPage, itemFlags, liveOn } from "./WorkItem.tsx";
import { Subtasks } from "./item/Body.tsx";
import { ItemRail, Relations, linkHeading, standingWords } from "./item/Rail.tsx";
import { Routing, TurnCard, changeSentence, turnPills } from "./item/Activity.tsx";
import { Router, href } from "~/app/router.tsx";
import { pathOf, refToken } from "~/app/frame/objects.ts";
import { PeekHost, PeekNeighbours } from "~/app/frame/PeekHost.tsx";
import { FrameReadings } from "~/app/Shell.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { reloadForTest } from "~/lib/prefs.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type {
  WorkActivityRecord,
  WorkItem,
  WorkItemDetail,
  WorkItemTurn,
  WorkProjectDetail,
  WorkRoutingAnswer,
} from "~/protocol/index.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

const NOW = Date.parse("2031-04-16T12:00:00Z");

const EVERY_TOOL = [
  "update_work_item",
  "comment_on_work_item",
  "create_work_item",
  "restore_work_item",
  "write_page",
];

const JANE = {
  operator_id: "U0FOUNDER",
  operator: true,
  handle: "jane",
  name: "Jane Founder",
  kind: "human",
  acts: EVERY_TOOL,
};
/** A token no seat is bound to: it may read, and every write says why not. */
const UNBOUND = { operator_id: "U0OPS", operator: true, handle: "", name: "", acts: [] };

const ORG = {
  name: "Acme",
  timezone: "UTC",
  roles: [
    { name: "Jane Founder", handle: "jane", kind: "human" },
    { name: "Ada Okonkwo", handle: "ada", kind: "agent" },
    { name: "SWE", handle: "swe", kind: "agent" },
  ],
  units: [],
};

const task = (over: Partial<WorkItem> = {}): WorkItem => ({
  id: "t-1",
  key: "ENG-42",
  project: "ENG",
  title: "Fix the login race",
  status: "in_progress",
  version: 7,
  ...over,
});

const detail = (over: Partial<WorkItemDetail> = {}): WorkItemDetail => ({
  task: task(),
  reassignment_budget: 8,
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
  tags: [
    { slug: "bug", label: "bug" },
    { slug: "provisioner", label: "provisioner" },
  ],
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

const turn = (over: Partial<WorkItemTurn> = {}): WorkItemTurn => ({
  turn_id: "run-1",
  ordinal: 1,
  seat: "swe",
  segments: 1,
  tokens: 38_400,
  cache_read: 0,
  rounds: 7,
  wall_ms: 842_000,
  outcome: "done",
  phases: ["execute", "review"],
  at: "2031-04-15T16:40:00Z",
  ...over,
});

type Answer = unknown | ((params: Record<string, unknown>) => Promise<unknown>);

const QUIET: Record<string, Answer> = {
  // THE SHELL'S OWN READS — the inbox count the frame keeps.
  work_inbox: { handle: "jane", notices: [], primary_reasons: [], unread: 0, primary: 0 },
  decisions: { handle: "jane", items: [], total: 0, capped: false },
  work_item: detail(),
  work_project: project(),
  work_items: { items: [], complete: true },
  work_activity: { records: [], complete: true },
  work_comments: { item: "t-1", key: "ENG-42", title: "Fix the login race", comments: [] },
  work_item_turns: { item: "t-1", key: "ENG-42", turns: [], complete: true },
};

let asked: { kind: string; params: Record<string, unknown> }[];
let posted: { tool: string; args: Record<string, unknown> }[];

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  vi.spyOn(Date, "now").mockReturnValue(NOW);
  asked = [];
  posted = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      const tool = decodeURIComponent(String(url).split("/operator/act/")[1] ?? "");
      const body = JSON.parse(init.body as string) as { args: Record<string, unknown> };
      posted.push({ tool, args: body.args });
      return new Response(
        JSON.stringify({
          tool,
          outcome: "applied",
          position: "CREWLET_TRACKER_LOG@1:99",
          receipt: {},
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      );
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  localStorage.clear();
  reloadForTest();
  location.hash = "#/";
});

/** Everything a task's frames read, answered from fixtures, around `children`. */
function mount(
  children: ReactNode,
  {
    answers = {},
    viewer = JANE,
    agents = [],
  }: {
    answers?: Record<string, Answer>;
    viewer?: Record<string, unknown>;
    agents?: Record<string, unknown>[];
  } = {},
) {
  const store = new Store();
  store.applyHealth({ status: "healthy", nodes: 1 } as never);
  store.applyOrg(ORG as never);
  store.applyAgents(agents as never);
  const socket = new LiveSocket(store);
  socket.query = ((what: string, params?: Record<string, unknown>) => {
    asked.push({ kind: what, params: params ?? {} });
    const all: Record<string, Answer> = { ...QUIET, viewer, ...answers };
    if (what in all) {
      const answer = all[what];
      return typeof answer === "function"
        ? (answer as (p: Record<string, unknown>) => Promise<unknown>)(params ?? {})
        : Promise.resolve(answer);
    }
    return Promise.resolve({});
  }) as typeof socket.query;
  const view = render(
    <ToastProvider>
      <LayerHost>
        <ClientContext.Provider value={{ store, socket }}>
          <FrameReadings>
            <Router>{children}</Router>
          </FrameReadings>
        </ClientContext.Provider>
      </LayerHost>
    </ToastProvider>,
  );
  return { ...view, store };
}

async function settle() {
  await act(async () => {
    for (let i = 0; i < 10; i++) await Promise.resolve();
  });
}

/** The reason a write control is disabled with, off the kit's described-by. */
function reasonOf(button: HTMLElement): string {
  return (button.getAttribute("aria-describedby") ?? "")
    .split(/\s+/)
    .map((id) => document.getElementById(id)?.textContent ?? "")
    .join(" ");
}

/** The rail alone, read-only (no page write around it). */
function rail(d: WorkItemDetail, p: WorkProjectDetail | null = project(), chrome = {}) {
  return mount(<ItemRail detail={d} chrome={chrome} project={p} now={NOW} />);
}

/** The value cell of a rail row, found by its label. */
const rowValue = (label: string) => screen.getByText(label).nextElementSibling as HTMLElement;

// ---------------------------------------------------------------------------
// The rail: what a value CLAIMS
// ---------------------------------------------------------------------------

// A DUE DATE STANDS ON THE COMPANY'S CALENDAR, AS THE BOARD'S DOES. The rail
// read "1d ago" in grey for a task two company days late that every row and
// calendar day drew red, and printed a far date twice ("Nov 09, 2026 /
// Nov 09, 2026"). It now prints the engine's standing — whole days, overdue
// only for open work — in the danger tone when it is late.
test("the due date reads the engine's standing, in the danger tone when late", async () => {
  rail(
    detail({
      task: task({ due_at: "2031-04-14T12:00:00Z" }),
      due_standing: { days: -2, overdue: true },
    }),
  );
  await settle();
  const due = rowValue("Due");
  expect(due.textContent).toContain("2 days overdue");
  expect(due.querySelector(".task-date.overdue")).toBeTruthy();
  cleanup();
  rail(detail({ task: task({ due_at: "2031-11-09T12:00:00Z" }), due_standing: { days: 207 } }));
  await settle();
  const far = rowValue("Due");
  expect(far.textContent).toContain("in 207 days");
  expect(far.querySelector(".task-date.overdue")).toBeNull();
  // THE DATE ONCE: the absolute form is drawn a single time.
  const date = far.querySelector(".task-date [title]")?.textContent ?? "";
  expect(far.textContent!.split(date).length - 1).toBe(1);
});

test("a standing reads in whole calendar days, and late only where the engine says so", () => {
  expect(standingWords({ days: 0 })).toBe("today");
  expect(standingWords({ days: 1 })).toBe("tomorrow");
  expect(standingWords({ days: 42 })).toBe("in 42 days");
  expect(standingWords({ days: -1, overdue: true })).toBe("1 day overdue");
  expect(standingWords({ days: -2, overdue: true })).toBe("2 days overdue");
  // FINISHED WORK PAST ITS DATE IS DONE, NOT LATE.
  expect(standingWords({ days: -2 })).toBe("2 days ago");
  expect(standingWords({ days: -1 })).toBe("yesterday");
});

// A CHOICE STORES THE OPTION'S ID — which is what lets a company rename an
// option without orphaning every task that chose it — so a rail that showed
// the stored value would print a uuid under a field heading.
test("a custom field renders by its name, with its option's own word", async () => {
  rail(
    detail({
      fields: [{ id: "f-sev", slug: "severity", name: "Severity", type: "dropdown", value: "o-1" }],
    }),
  );
  await settle();
  expect(screen.getByText("Severity")).toBeTruthy();
  expect(screen.getByText("High")).toBeTruthy();
  expect(screen.queryByText("o-1")).toBeNull();
});

// A VALUE A FILTER CANNOT REACH SAYS SO IN A WORD. "Archived", "from another
// tracker" and "undeclared" are three different facts.
test("a value whose declaration was archived is marked as such", async () => {
  rail(
    detail({
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
    }),
  );
  await settle();
  expect(screen.getByText("archived")).toBeTruthy();
});

// AN ABSENT VALUE IS A MARKED ABSENCE, never a zero: zero is a measurement,
// and an unestimated task is one nobody has sized.
test("an unestimated task shows a marked absence rather than a zero", async () => {
  rail(detail());
  await settle();
  const estimate = rowValue("Estimate");
  expect(estimate.textContent).toContain("Not estimated");
  expect(estimate.textContent).not.toContain("0m");
  expect(estimate.textContent).not.toMatch(/\b0 points\b/);
});

// `none` IS A VALUE THE ENGINE MINTS — a create defaults the field to it and
// a change can set it back — so the rail answers what the field is SET TO, in
// the word, with the change that set it.
test("a priority of none is the word, with the change that set it", async () => {
  rail(
    detail({
      task: task({ priority: "none" }),
      history: [
        {
          id: "h-1",
          kind: "fields",
          actor: "agent-ceo",
          at: "2031-04-16T09:00:00Z",
          log_seq: 1,
          fields: { priority: { from: "high", to: "none" } },
        },
      ],
    }),
  );
  await settle();
  const value = rowValue("Priority");
  expect(value.textContent).toContain("No priority");
  expect(value.textContent).not.toContain(EMPTY_VALUE);
  expect(value.querySelector(".props-setby")!.textContent).toContain("set by agent-ceo");
});

// AN ABSENT PRIORITY IS STILL A DASH, with no attribution: no value reached
// this build at all.
test("a task carrying no priority at all still dashes", async () => {
  const { container } = rail(detail());
  await settle();
  expect(rowValue("Priority").textContent).toBe(`${EMPTY_VALUE}Not set`);
  expect(container.querySelectorAll(".props-setby")).toHaveLength(0);
});

// THE PROJECT IS THE TASK'S ADDRESS: the name is what a person calls it and
// the key is what everything is addressed by, so the row carries both — and
// the key alone while the project read has not answered.
test("the rail names the project and its key, and links to its board", async () => {
  rail(detail());
  await settle();
  const value = rowValue("Project");
  expect(value.textContent).toContain("Engineering");
  expect(value.textContent).toContain("ENG");
  expect(value.querySelector("a")!.getAttribute("href")).toBe(href(["work", "ENG"]));
});

test("the project row falls back to the key alone", async () => {
  rail(detail(), null);
  await settle();
  expect(rowValue("Project").textContent).toBe("ENG");
});

// A HAND-OFF COUNT IS A BUDGET: the engine refuses the next hand-off at its
// own limit, served beside the count, so the warning is exactly where the
// refusals start — and moves with the budget, which is read, never held.
test("the hand-off warning is at the engine's budget", async () => {
  const at = (reassignments: number, budget: number) => {
    cleanup();
    rail(detail({ task: task({ reassignments }), reassignment_budget: budget }));
  };
  at(7, 8);
  await settle();
  expect(screen.getByText(/Hand-offs 7 of 8/)).toBeTruthy();
  expect(screen.queryByText(/the next hand-off will be refused/)).toBeNull();

  at(8, 8);
  await settle();
  expect(screen.getByText(/Hand-offs 8 of 8/)).toBeTruthy();
  expect(screen.getByText(/the next hand-off will be refused/)).toBeTruthy();

  at(3, 3);
  await settle();
  expect(screen.getByText(/the next hand-off will be refused/)).toBeTruthy();
});

// WHAT A TASK HAS COST IS ITS OWN COUNTERS, in tokens — the task's `spend`,
// the same count its turn cards and its board card read — and never money.
test("the cost is the task's own turns, tokens and agent time, and no price", async () => {
  const { container } = rail(
    detail({ task: task({ spend: { turns: 2, tokens: 79_600, wall_ms: 1_200_000 } }) }),
  );
  await settle();
  const cost = container.querySelector(".task-cost")!;
  // TERM THEN FIGURE in each group, as a definition list requires; the
  // stylesheet draws the figure above.
  const pairs = [...cost.children].map((group) =>
    [...group.children].map((el) => `${el.tagName}:${el.textContent}`),
  );
  expect(pairs).toEqual([
    ["DT:turns", "DD:2"],
    ["DT:tokens", "DD:79.6k"],
    ["DT:agent time", "DD:20m 0s"],
  ]);
  expect(container.textContent).not.toMatch(/[$€£]|USD|cost_usd/);

  cleanup();
  rail(detail());
  await settle();
  expect(screen.getByText("No agent turn has been charged to it yet.")).toBeTruthy();

  // A CHARGE WITH NO TURN IS STILL A CHARGE: a segment continuing a turn
  // counted elsewhere adds tokens and time and no turn, and the panel shows it.
  cleanup();
  const charged = rail(
    detail({ task: task({ spend: { turns: 0, tokens: 5_000, wall_ms: 60_000 } }) }),
  );
  await settle();
  expect(charged.container.querySelector(".task-cost")?.textContent).toContain("5,000");
  expect(screen.queryByText("No agent turn has been charged to it yet.")).toBeNull();
});

// WATCHING AND MUTED ARE DIFFERENT FACTS AND BOTH TRAVEL.
test("a muted watcher is listed and said to be muted", async () => {
  rail(detail({ task: task({ watchers: ["ada", "swe"], muted: ["swe"] }) }), project(), {
    seatName: (h: string) => (h === "swe" ? "SWE" : h),
  });
  await settle();
  expect(screen.getByText("Watching")).toBeTruthy();
  expect(screen.getByText("Muted: SWE")).toBeTruthy();
});

// ROUTING IS SHOWN ONLY WHERE IT HAS MOVED.
test("a task routed where it was filed says so once", async () => {
  rail(detail({ task: task({ filed_unit: "platform", routing_unit: "platform" }) }));
  await settle();
  expect(screen.queryByText("Routes to")).toBeNull();
  cleanup();
  rail(detail({ task: task({ filed_unit: "platform", routing_unit: "payments" }) }));
  await settle();
  expect(screen.getByText("Routes to")).toBeTruthy();
});

// A TEAM IS DRAWN BY ITS NAME AND FOLLOWED BY ITS KEY; one the chart has lost
// is a finding, unlinked; and a row with no resolution still reads and links.
test("a unit reads as the team's name and links by the stored key", async () => {
  rail(
    detail({
      task: task({ filed_unit: "eng", routing_unit: "plat" }),
      units: {
        filed: { key: "eng", name: "Core Engineering", resolved: true },
        routing: { key: "plat", name: "Platform", resolved: true },
      },
    }),
  );
  await settle();
  expect(screen.getByText("Core Engineering").closest("a")?.getAttribute("href")).toContain(
    "unit=eng",
  );
  expect(screen.getByText("Platform").closest("a")?.getAttribute("href")).toContain("unit=plat");
  expect(screen.queryByText("eng")).toBeNull();
});

test("a unit the chart has lost is marked rather than linked", async () => {
  rail(
    detail({
      task: task({ filed_unit: "dissolved", routing_unit: "dissolved" }),
      units: {
        filed: { key: "dissolved", resolved: false },
        routing: { key: "dissolved", resolved: false },
      },
    }),
  );
  await settle();
  const pill = screen.getByTitle("The current org chart has no such unit");
  expect(pill.textContent).toBe("dissolved");
  expect(pill.closest("a")).toBeNull();
});

// WHO SET IT: a property names the change that put the value there, and one
// the visible history does not name carries nothing rather than borrowing the
// oldest line still visible.
test("a property names the change that set it, and only that change", async () => {
  const { container } = rail(
    detail({
      task: task({ assignee: "ada", points: 3 }),
      history: [
        {
          id: "h-2",
          kind: "assignee",
          actor: "bo",
          actor_kind: "human",
          at: "2031-04-16T09:00:00Z",
          log_seq: 2,
          fields: { assignee: { from: "", to: "ada" } },
        },
      ],
    }),
  );
  await settle();
  const lines = [...container.querySelectorAll(".props-setby")].map((el) => el.textContent);
  expect(lines).toHaveLength(1);
  expect(lines[0]).toContain("bo");
});

// ---------------------------------------------------------------------------
// Relations and pages
// ---------------------------------------------------------------------------

// THE DERIVED HALF OF A DEPENDENCY IS THE ONE NOBODY AUTHORED, so calling both
// ends "waiting on" would tell a reader their task is blocked by the task it
// is blocking.
test("a dependency is named from the end it is read at", () => {
  expect(linkHeading({ kind: "waiting_on", other: "x" })).toBe("Waiting on");
  expect(linkHeading({ kind: "waiting_on", other: "x", derived: true })).toBe("Blocks");
  expect(linkHeading({ kind: "duplicates", other: "x" })).toBe("Duplicates");
  expect(linkHeading({ kind: "duplicates", other: "x", derived: true })).toBe("Duplicated by");
  expect(linkHeading({ kind: "linked", other: "x" })).toBe("Related");
});

// THE TASK IT IS FILED UNDER, BY NAME — "Part of LEAD-12 · 2.4 release" — off
// the detail's own `parent`, and each relation under the word its end earns.
test("relations name the parent and each link from this end", async () => {
  mount(
    <Relations
      detail={detail({
        task: task({ parent: "t-12" }),
        parent: { id: "t-12", key: "LEAD-12", title: "2.4 release", status: "in_progress" },
        links: [
          { kind: "waiting_on", other: "t-2", key: "ENG-2", title: "the blocker" },
          { kind: "waiting_on", other: "t-3", key: "ENG-3", title: "the dependent", derived: true },
        ],
      })}
      chrome={{}}
    />,
  );
  await settle();
  const part = screen.getByText("Part of").closest("a")!;
  expect(part.textContent).toContain("LEAD-12");
  expect(part.textContent).toContain("2.4 release");
  expect(part.getAttribute("href")).toBe(href(["work", "LEAD-12"]));
  expect(screen.getByText("Waiting on")).toBeTruthy();
  expect(screen.getByText("Blocks")).toBeTruthy();
});

// THE REPAIR AND THE FINDING ARE DIFFERENT STATES.
test("a pending mirror and a permanent one-sided edge read differently", async () => {
  mount(
    <Relations
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
  await settle();
  expect(screen.getByText("mirror pending")).toBeTruthy();
  expect(screen.getByText("one-sided")).toBeTruthy();
});

// NO RELATIONS IS NO SECTION: a heading over nothing reads as a task whose
// relations failed to load.
test("a task with no relations draws no section", async () => {
  const { container } = mount(<Relations detail={detail()} chrome={{}} />);
  await settle();
  expect(container.textContent).toBe("");
});

// A PAGE LINK GOES TO THE PAGE'S ONE ADDRESS, BY ITS ID (`other`) — asserted
// through `pathOf`, the frame's one definition of where a page lives — and is
// named by the page's own title.
test("a linked page points at the page by its id and reads its title", async () => {
  mount(
    <ItemRail detail={detail({ links: [{ kind: "page", other: "p-1" }] })} chrome={{}} now={NOW} />,
    { answers: { page: { page: { id: "p-1", title: "Provisioner runbook" } } } },
  );
  await settle();
  const link = screen.getByText("Provisioner runbook").closest("a")!;
  expect(link.getAttribute("href")).toBe(href(pathOf({ kind: "page", id: "p-1" })));
  expect(link.getAttribute("href")).toBe("#/knowledge/pages/p-1");
  expect(asked.find((a) => a.kind === "page")?.params).toEqual({ id: "p-1" });
});

// ---------------------------------------------------------------------------
// Sub-tasks
// ---------------------------------------------------------------------------

// A FAILED SUBTREE READ IS NOT AN EMPTY ONE: the error is drawn where the rows
// would have been, and outranks rows a previous poll listed.
test("a refused subtree read says so rather than looking like a leaf", async () => {
  mount(<Subtasks rows={[]} error="bad_params" chrome={{}} />);
  await settle();
  expect(screen.getByText("Sub-tasks")).toBeTruthy();
  expect(screen.getByText(/The engine refused this request/)).toBeTruthy();
});

test("a refused poll says so even when children were listed before", async () => {
  mount(
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
      chrome={{}}
    />,
  );
  await settle();
  expect(screen.getByText(/The engine tried to answer and failed/)).toBeTruthy();
  expect(screen.queryByText("A child")).toBeNull();
});

// AND A READ THAT ANSWERED MAY CONCLUDE THE TASK IS A LEAF: no card, and — for
// a task nobody may add to — nothing at all.
test("a task that answered with no children draws no card", async () => {
  const { container } = mount(<Subtasks rows={[]} error={null} chrome={{}} />);
  await settle();
  expect(container.textContent).toBe("");
});

test("the sub-task card counts what is done", async () => {
  mount(
    <Subtasks
      rows={[
        {
          id: "a",
          key: "ENG-413",
          project: "ENG",
          title: "Backoff helper",
          type: "task",
          status: "done",
          status_group: "done",
          updated: "",
          version: 1,
        },
        {
          id: "b",
          key: "ENG-414",
          project: "ENG",
          title: "Emit the count",
          type: "task",
          status: "in_progress",
          updated: "",
          version: 1,
        },
        {
          id: "c",
          key: "ENG-416",
          project: "ENG",
          title: "e2e",
          type: "task",
          status: "todo",
          updated: "",
          version: 1,
        },
      ]}
      chrome={{}}
    />,
  );
  await settle();
  expect(screen.getByText("1 / 3")).toBeTruthy();
  expect(screen.getByRole("img", { name: "1 of 3 sub-tasks done" })).toBeTruthy();
});

// ---------------------------------------------------------------------------
// Activity
// ---------------------------------------------------------------------------

/** A change record of the task's own feed. */
function record(over: Partial<WorkActivityRecord>): WorkActivityRecord {
  return {
    id: "r-1",
    log_seq: 1,
    log_stream: "CREWLET_TRACKER_LOG",
    log_generation: 1,
    at: "2031-04-10T09:00:00Z",
    effective_at: "2031-04-10T09:00:00Z",
    kind: "created",
    actor: "jane",
    subject_kind: "task",
    subject_id: "t-1",
    project: "ENG",
    notified: false,
    ...over,
  };
}

// A HAND-OFF SAYS WHY. The reason a person gave is the change's excerpt — the
// one line the new holder is woken with — and it is quoted beside the change.
test("a hand-off is a sentence with its reason", () => {
  const chrome = { seatName: (h: string) => ({ swe: "SWE" })[h] ?? h };
  expect(changeSentence(record({ kind: "created" }), task(), chrome)).toEqual({
    what: "filed this in ENG",
    why: "",
  });
  expect(
    changeSentence(
      record({
        kind: "assignee",
        excerpt: "owns the provisioner",
        fields: { assignee: { from: "", to: "swe" } },
      }),
      task(),
      chrome,
    ),
  ).toEqual({ what: "assigned it to SWE", why: "owns the provisioner" });
});

// A CHANGE ROW IS A SENTENCE, with the actor in front of it: a checklist move
// says what happened to which list rather than printing the stored counts on
// both sides of an arrow, and a field sits mid-sentence in lower case.
test("a change reads as a sentence, a checklist move included", () => {
  const chrome = {};
  const said = (fields: WorkActivityRecord["fields"]) =>
    changeSentence(record({ kind: "checklist", fields }), task(), chrome).what;
  expect(said({ checklists: { from: "", to: "Acceptance: 0 of 4 done" } })).toBe(
    "added the Acceptance checklist (4 items)",
  );
  expect(
    said({ checklists: { from: "Acceptance: 0 of 4 done", to: "Acceptance: 1 of 4 done" } }),
  ).toBe("ticked 1 on Acceptance — 1 of 4 done");
  expect(
    said({
      checklists: {
        from: "Acceptance: 1 of 4 done, Rollout: 0 of 2 done",
        to: "Acceptance: 1 of 5 done",
      },
    }),
  ).toBe("added 1 item to Acceptance and removed the Rollout checklist");
  expect(
    changeSentence(
      record({ kind: "fields", fields: { priority: { from: "normal", to: "high" } } }),
      task(),
      chrome,
    ).what,
  ).toBe("changed the priority from normal to high");
  expect(
    changeSentence(
      record({ kind: "fields", fields: { due: { from: "2031-04-20T00:00:00Z", to: "" } } }),
      task(),
      chrome,
    ).what,
  ).toBe("cleared the due date");
  // A relation says which task joined it, by its key.
  expect(
    changeSentence(
      record({ kind: "relations", fields: { blocking: { from: "", to: "t-4" } } }),
      task(),
      { taskKey: (id: string) => (id === "t-4" ? "ENG-4" : "") },
    ).what,
  ).toBe("made it block ENG-4");
});

// WHO A CHANGE REACHED IS A QUIET DISCLOSURE AT THE END OF ITS LINE: a button
// that says it expands, inside the sentence, rather than a bold row of its own
// under every change.
test("who a change reached opens from the end of its line", async () => {
  mount(<WorkItemPage id="ENG-42" />, {
    answers: {
      work_item: detail(),
      work_activity: {
        records: [
          record({
            id: "r-9",
            kind: "status",
            notified: true,
            fields: { status: { from: "todo", to: "in_progress" } },
          }),
        ],
        complete: true,
      },
    },
  });
  await settle();
  const reached = screen.getByRole("button", { name: /Who this reached/ });
  expect(reached.getAttribute("aria-expanded")).toBe("false");
  expect(reached.closest("p")?.textContent).toContain("moved it to In progress");
  act(() => reached.click());
  expect(reached.getAttribute("aria-expanded")).toBe("true");
});

// A TURN CARD SAYS WHAT THE TURN DID AND WHAT THE REVIEWER ASKED FOR: "SWE ran
// turn 1", its phases, "sent back for another pass" with the reviewer's
// request quoted, the tools it called, wall time and tokens, and its trace.
test("a turn card shows the send-back notes", async () => {
  mount(
    <ol>
      <TurnCard
        turn={turn({
          sent_back: 1,
          summary: "Added a backoff helper to provisioner/dhcp.",
          review: "the timeout path has no test.",
          tools: [
            { name: "create_branch", calls: 1 },
            { name: "run_sandbox", calls: 4 },
          ],
        })}
        chrome={{ seatName: (h: string) => (h === "swe" ? "SWE" : h) }}
        now={NOW}
      />
    </ol>,
  );
  await settle();
  const card = screen.getByRole("article", { name: "Turn 1" });
  expect(within(card).getByText("ran turn 1")).toBeTruthy();
  expect(within(card).getByText("sent back for another pass")).toBeTruthy();
  expect(within(card).getByText("the timeout path has no test.")).toBeTruthy();
  expect(within(card).getByText("Reviewer:")).toBeTruthy();
  expect(within(card).getByText("run_sandbox ×4")).toBeTruthy();
  expect(within(card).getByText("14m 2s · 38.4k tokens")).toBeTruthy();
  expect(within(card).getByText("Trace").getAttribute("href")).toBe(
    href(["live", "turns", "run-1"]),
  );
  // Tokens only, never money.
  expect(card.textContent).not.toMatch(/[$€£]/);
});

// A TURN WITH NO COUNT HERE IS NOT NUMBERED, and a turn still out on a coding
// run says it is parked rather than done.
test("a turn's pills say how it went, and only what is worth a pill", () => {
  expect(turnPills(turn())).toEqual([]);
  expect(turnPills(turn({ sent_back: 2 })).map((p) => p.label)).toEqual([
    "sent back 2 times for another pass",
  ]);
  expect(turnPills(turn({ outcome: "suspended" })).map((p) => p.label)).toEqual([
    "parked on a coding run",
  ]);
  expect(turnPills(turn({ outcome: "failed" }))[0]?.variant).toBe("danger");
  expect(turnPills(turn({ outcome: "failed" }))[0]?.label).toBe("failed");
  expect(turnPills(turn({ outcome: "failed", failed_in: "review" }))[0]?.label).toBe(
    "failed in review",
  );
});

// A FAILED TURN SAYS WHICH STEP BROKE: the phase it failed in wears the cross
// and says "failed" to a screen reader, the phases that held keep their tick,
// and the wall time and tokens sit on their own line under the chips.
test("a failed turn marks the phase it broke in", async () => {
  mount(
    <ol>
      <TurnCard
        turn={turn({
          outcome: "failed",
          failed_in: "review",
          summary: "",
          tools: [{ name: "run_sandbox", calls: 1 }],
        })}
        chrome={{}}
        now={NOW}
      />
    </ol>,
  );
  await settle();
  const card = screen.getByRole("article", { name: "Turn 1" });
  const phases = [...card.querySelectorAll(".task-phase")];
  expect(phases.map((p) => p.hasAttribute("data-failed"))).toEqual([false, true]);
  expect(phases[1]!.textContent).toContain("(failed)");
  expect(within(card).getByText("failed in review")).toBeTruthy();
  // No account recorded: one quiet line, the reason on hover.
  expect(within(card).getByText("No summary recorded — see the trace")).toBeTruthy();
  // The meta line is its own row, after the tool chips.
  const meta = card.querySelector(".task-turn-meta")!;
  expect(meta.previousElementSibling?.classList.contains("task-turn-tools")).toBe(true);
  expect(within(meta as HTMLElement).getByText("Trace")).toBeTruthy();
});

// THE LIVE ROW IS THE TURN RUNNING ON *THIS* TASK: joined on the item the
// engine charges the turn to, and only while the seat is working. A seat
// working on another task, or one stopped mid-turn on this one, draws nothing.
test("the live row appears only for a turn on this item", async () => {
  const on = (key: string, activity = "working") => ({
    handle: "swe",
    activity,
    turn: {
      turn_id: "run-9",
      started_at: "2031-04-16T11:57:00Z",
      stage: "phases",
      work_item: { backend: "native", id: key === "ENG-42" ? "t-1" : "t-9", key, project: "ENG" },
    },
    // THE SEVENTH ROUND IN FLIGHT: `round_num` is zero-based.
    live_call: { turn_id: "run-9", phase: "execute", round_num: 6, rounds_used: 6, max_rounds: 25 },
  });
  expect(liveOn([on("ENG-9")] as never, { id: "t-1", key: "ENG-42" })).toBeNull();
  expect(liveOn([on("ENG-42", "stopped")] as never, { id: "t-1", key: "ENG-42" })).toBeNull();
  expect(liveOn([on("ENG-42")] as never, { id: "t-1", key: "ENG-42" })?.handle).toBe("swe");

  mount(<WorkItemPage id="ENG-42" />, {
    agents: [on("ENG-42")],
    answers: {
      work_item: detail({ task: task({ spend: { turns: 2 } }) }),
      work_item_turns: {
        item: "t-1",
        key: "ENG-42",
        turns: [turn({ ordinal: 2, turn_id: "run-2" })],
        complete: true,
      },
    },
  });
  await settle();
  const live = screen.getByRole("link", { name: /is on turn 3 of ENG-42/ });
  expect(live.textContent).toContain("SWE is on turn 3");
  expect(live.textContent).toContain("executing · round 7 of 25");
  expect(live.getAttribute("href")).toBe(href(["live", "turns", "run-9"]));
  // AND THE PAGE BAR'S WAY TO WATCH IT.
  expect(screen.getByRole("link", { name: "Watch live" }).getAttribute("href")).toBe(
    href(["live", "turns", "run-9"]),
  );

  cleanup();
  mount(<WorkItemPage id="ENG-42" />, { agents: [on("ENG-9")] });
  await settle();
  expect(screen.queryByRole("link", { name: /is on turn/ })).toBeNull();
  expect(screen.queryByRole("link", { name: "Watch live" })).toBeNull();
});

// THE LIVE ROW NAMES THE ROUND AND THE PHASE THE SEAT'S PROFILE NAMES. It kept
// a private reading of the live call that took the zero-based `round_num` raw
// — no round at all during the first, one lower than the stepper and the peek
// after it — and called every phase but review "executing". It reads the task
// card's strip's words (`doingWords`), so a turn is described once.
test("the live row counts rounds from one and names the phase running", async () => {
  const working = (live_call: Record<string, unknown>) => ({
    handle: "swe",
    activity: "working",
    turn: {
      turn_id: "run-9",
      started_at: "2031-04-16T11:57:00Z",
      stage: "phases",
      work_item: { backend: "native", id: "t-1", key: "ENG-42", project: "ENG" },
    },
    live_call: { turn_id: "run-9", max_rounds: 25, ...live_call },
  });
  const liveText = async (live_call: Record<string, unknown>) => {
    cleanup();
    mount(<WorkItemPage id="ENG-42" />, { agents: [working(live_call)] });
    await settle();
    return screen.getByRole("link", { name: /is on turn \d+ of ENG-42/ }).textContent ?? "";
  };
  // The first round, in flight before any has come back.
  expect(await liveText({ phase: "execute", round_num: 0, rounds_used: 0 })).toContain(
    "executing · round 1 of 25",
  );
  expect(await liveText({ phase: "execute", round_num: 6, rounds_used: 6 })).toContain(
    "executing · round 7 of 25",
  );
  // Reading context is not executing.
  const reading = await liveText({ phase: "context", round_num: -1 });
  expect(reading).toContain("reading context");
  expect(reading).not.toContain("executing");
});

// HOW LONG IT HAS RUN IS THE PROFILE CARD'S CLOCK: seconds under a minute.
// The list's short age read a sub-minute turn as "for 0m" while the seat's own
// card said "0s" of the same turn.
test("the live row says a sub-minute turn's seconds, as the profile does", async () => {
  mount(<WorkItemPage id="ENG-42" />, {
    agents: [
      {
        handle: "swe",
        activity: "working",
        turn: {
          turn_id: "run-9",
          started_at: new Date(NOW - 30_000).toISOString(),
          stage: "phases",
          work_item: { backend: "native", id: "t-1", key: "ENG-42", project: "ENG" },
        },
        live_call: { turn_id: "run-9", phase: "execute", round_num: 1, max_rounds: 24 },
      },
    ] as never,
  });
  await settle();
  // THE PAGE'S CLOCK re-reads (the mocked) `Date.now` when the tab is shown.
  act(() => {
    document.dispatchEvent(new Event("visibilitychange"));
  });
  const live = screen.getByRole("link", { name: /is on turn \d+ of ENG-42/ }).textContent ?? "";
  expect(live).toContain("for 30s");
  expect(live).not.toContain("0m");
});

// OLDER COMMENTS ARE REACHABLE. The detail returns the newest page of a thread
// and a cursor nothing followed, so a conversation past fifty was invisible;
// "Earlier activity" asks for the page before the oldest one shown.
test("older comments are reachable", async () => {
  const comment = (id: string, at: string, body: string) => ({
    id,
    task: "t-1",
    author: "ada",
    author_kind: "agent",
    body,
    created_at: at,
  });
  mount(<WorkItemPage id="ENG-42" />, {
    answers: {
      work_comments: (params: Record<string, unknown>) =>
        Promise.resolve(
          params.cursor === "c-50"
            ? {
                item: "t-1",
                key: "ENG-42",
                title: "x",
                comments: [comment("c-1", "2031-04-01T09:00:00Z", "the very first word")],
              }
            : {
                item: "t-1",
                key: "ENG-42",
                title: "x",
                comments: [comment("c-51", "2031-04-12T09:00:00Z", "the newest word")],
                next_cursor: "c-50",
              },
        ),
    },
  });
  await settle();
  expect(screen.getByText("the newest word")).toBeTruthy();
  expect(screen.queryByText("the very first word")).toBeNull();
  const earlier = screen.getByRole("button", { name: "Earlier activity" });
  // A CONTROL THAT LOOKS LIKE ONE: its chevron, without which the label read
  // as a heading over the feed.
  expect(earlier.querySelector("svg")).toBeTruthy();
  fireEvent.click(earlier);
  await settle();
  expect(asked.some((a) => a.kind === "work_comments" && a.params.cursor === "c-50")).toBe(true);
  expect(screen.getByText("the very first word")).toBeTruthy();
});

// THE FOUR ANSWERS OF "WHO THIS REACHED" KEEP THEIR SENTENCES: reached nobody,
// no such change here, beyond the retention window, and cannot say — four
// different facts, which a tracker that listed recipients would draw as one
// empty list.
test("the Woke answers keep their four empty sentences", async () => {
  const empty = (over: Partial<WorkRoutingAnswer>): WorkRoutingAnswer => ({
    record_id: "r-1",
    held: true,
    notified: true,
    delivery: "reached",
    addressed: 0,
    fallback: 0,
    recipients: [],
    ...over,
  });
  for (const [answer, title] of [
    [empty({}), "It announced, and reached nobody"],
    [empty({ held: false }), "No such change here"],
    [empty({ delivery: "swept" }), "Beyond the retention window"],
    [empty({ delivery: "unknown" }), "Cannot say"],
  ] as const) {
    cleanup();
    mount(<Routing answer={answer} chrome={{}} />);
    await settle();
    expect(screen.getByText(title)).toBeTruthy();
  }
});

// THE ACCENT SAYS WHERE THE READER IS, and this panel is about other people:
// whether a notice ASKED is its own mark, never a brand tint on the reason.
test("the Woke panel spends no accent, and 'asks' is its own mark", async () => {
  const { container } = mount(
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
  await settle();
  expect(container.querySelectorAll(".crewlet-tag--brand")).toHaveLength(0);
  expect(screen.getAllByText("asks")).toHaveLength(1);
  expect(screen.getByText("1 asked")).toBeTruthy();
});

// ---------------------------------------------------------------------------
// Changes, as the signed-in person
// ---------------------------------------------------------------------------

// A READER WHO CANNOT CHANGE THE TASK SEES THE SAME PAGE, every control drawn
// and disabled with the sentence that says what would change that — never a
// hidden button, never a picker that silently does nothing — for each of the
// three readers who cannot act.
test.each([
  [
    "an anonymous reader",
    { operator_id: "", operator: false, handle: "", name: "", acts: [], anonymous: true },
    WRITE_REASONS.anonymous,
  ],
  ["an unbound token", UNBOUND, WRITE_REASONS.unbound],
  ["a person the engine does not serve", { ...JANE, acts: [] }, WRITE_REASONS.not_served],
])("%s sees disabled controls with the reason", async (_who, viewer, reason) => {
  mount(<WorkItemPage id="ENG-42" />, { viewer });
  await settle();
  expect(document.querySelector(".task-readonly")?.textContent).toContain(reason);
  expect(document.querySelector(".task-readonly")?.textContent).toContain(
    "The fields, checklists and description are read-only here.",
  );
  const comment = screen.getByRole("button", { name: /^Comment$/ });
  expect(comment.getAttribute("aria-disabled")).toBe("true");
  expect(reasonOf(comment)).toContain(reason);
  const assign = screen.getByRole("button", { name: /^Assign$/ });
  expect(reasonOf(assign)).toContain(reason);
  // No pencil offers an edit the engine would refuse, and no pick is sent.
  expect(screen.queryByRole("button", { name: "Edit the title" })).toBeNull();
  expect(screen.queryByRole("button", { name: /Status of ENG-42/ })).toBeNull();
  expect(posted).toEqual([]);
});

// A WRITER'S FIELD IS CHANGED WHERE IT IS READ, conditional on the version the
// page was drawn from — so a race lost to somebody else is refused, not
// overwritten.
test("a status picked in the rail is sent as the person, at the page's version", async () => {
  mount(<WorkItemPage id="ENG-42" />);
  await settle();
  fireEvent.click(screen.getByRole("button", { name: /Status of ENG-42/ }));
  fireEvent.click(
    await screen
      .findByRole("menuitemradio", { name: "In review" })
      .catch(() => screen.findByRole("menuitem", { name: "In review" })),
  );
  await settle();
  expect(posted).toEqual([
    { tool: "update_work_item", args: { item: "ENG-42", if_match: 7, status: "in_review" } },
  ]);
});

// A TICK IS A GESTURE, applied to the checklists as they are when it lands —
// so it carries no version, and never loses to an agent that moved the status.
test("a checklist tick is sent without a version", async () => {
  mount(<WorkItemPage id="ENG-42" />, {
    answers: {
      work_item: detail({
        task: task({
          checklists: [
            {
              id: "l-1",
              name: "Done when",
              items: [
                { id: "i-1", name: "Retries with backoff", done: true },
                { id: "i-2", name: "An integration test covers it" },
              ],
            },
          ],
        }),
      }),
    },
  });
  await settle();
  expect(screen.getByText("1 / 2")).toBeTruthy();
  fireEvent.click(screen.getByRole("checkbox", { name: "An integration test covers it" }));
  await settle();
  expect(posted).toEqual([
    {
      tool: "update_work_item",
      args: { item: "ENG-42", checklist: { op: "set_done", item: "i-2", done: true } },
    },
  ]);
});

// "+" FILES A SUB-TASK UNDER THIS TASK, in its project.
test("a sub-task is filed under the task it was added from", async () => {
  mount(<WorkItemPage id="ENG-42" />);
  await settle();
  fireEvent.click(screen.getByRole("button", { name: "Add a sub-task" }));
  fireEvent.change(screen.getByRole("textbox", { name: "New sub-task of ENG-42" }), {
    target: { value: "e2e: a slow switch port" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Add" }));
  await settle();
  expect(posted).toEqual([
    {
      tool: "create_work_item",
      args: { title: "e2e: a slow switch port", project: "ENG", parent: "t-1" },
    },
  ]);
});

// FOLLOWING A TASK IS THE PERSON'S OWN, and carries no version either.
test("watch follows the task as the person", async () => {
  mount(<WorkItemPage id="ENG-42" />);
  await settle();
  fireEvent.click(screen.getByRole("button", { name: "Watch" }));
  await settle();
  expect(posted).toEqual([{ tool: "update_work_item", args: { item: "ENG-42", watch: true } }]);
});

// A COMMENT IS POSTED ON THE TASK — and a question put to one seat is an ask.
test("a comment is posted on the task", async () => {
  mount(<WorkItemPage id="ENG-42" />);
  await settle();
  fireEvent.change(screen.getByRole("combobox", { name: "Comment on ENG-42" }), {
    target: { value: "Holding the port for 20s reproduces it." },
  });
  fireEvent.click(screen.getByRole("button", { name: /^Comment$/ }));
  await settle();
  expect(posted).toEqual([
    {
      tool: "comment_on_work_item",
      args: { item: "ENG-42", body: "Holding the port for 20s reproduces it." },
    },
  ]);
});

/**
 * Holds every write until `release` answers them all `applied` — the window a
 * second Enter lands in.
 */
function holdWrites(): { release: () => void } {
  const waiting: ((r: Response) => void)[] = [];
  vi.mocked(fetch).mockImplementation(async (url, init) => {
    const tool = decodeURIComponent(String(url).split("/operator/act/")[1] ?? "");
    const body = JSON.parse((init as RequestInit).body as string) as {
      args: Record<string, unknown>;
    };
    posted.push({ tool, args: body.args });
    return new Promise<Response>((resolve) => waiting.push(resolve));
  });
  return {
    release: () => {
      for (const answer of waiting.splice(0)) {
        answer(
          new Response(
            JSON.stringify({
              outcome: "applied",
              position: "CREWLET_TRACKER_LOG@1:99",
              receipt: {},
            }),
            { status: 200, headers: { "Content-Type": "application/json" } },
          ),
        );
      }
    },
  };
}

// EVERY WAY INTO A PRESS TAKES THE BUTTON'S OWN GATE. A one-line form files on
// Enter and a comment sends on ⌘Enter, neither touching the button that
// refuses a press while one is out — so a second key before the first answer
// filed the sub-task twice and posted the comment twice, each under a new
// request id the engine rightly took for a second change.
test("a second Enter while the first is out files one sub-task and one comment", async () => {
  mount(<WorkItemPage id="ENG-42" />);
  await settle();
  const writes = holdWrites();
  fireEvent.click(screen.getByRole("button", { name: "Add a sub-task" }));
  const line = screen.getByRole("textbox", { name: "New sub-task of ENG-42" });
  fireEvent.change(line, { target: { value: "e2e: a slow switch port" } });
  fireEvent.submit(line.closest("form")!);
  await settle();
  fireEvent.submit(line.closest("form")!);
  await settle();
  const comment = screen.getByRole("combobox", { name: "Comment on ENG-42" });
  fireEvent.change(comment, { target: { value: "Holding the port reproduces it." } });
  fireEvent.keyDown(comment, { key: "Enter", ctrlKey: true });
  await settle();
  fireEvent.keyDown(comment, { key: "Enter", ctrlKey: true });
  await settle();
  expect(posted.map((p) => p.tool)).toEqual(["create_work_item", "comment_on_work_item"]);
  await act(async () => writes.release());
  await settle();
});

// A CONDITIONAL EDIT WHILE A PRESS IS OUT IS NOT SENT: it carries the version
// the first press is about to move, so the engine could only refuse it — and
// the page then drew the person's own first press as somebody else's change.
test("a second Enter on the title sends one rename and reports no conflict", async () => {
  mount(<WorkItemPage id="ENG-42" />);
  await settle();
  const writes = holdWrites();
  fireEvent.click(screen.getByRole("button", { name: "Edit the title" }));
  const title = screen.getByRole("textbox", { name: "Title of ENG-42" });
  fireEvent.change(title, { target: { value: "Switch ports flap under load" } });
  fireEvent.submit(title.closest("form")!);
  await settle();
  fireEvent.submit(title.closest("form")!);
  await settle();
  expect(posted).toEqual([
    {
      tool: "update_work_item",
      args: { item: "ENG-42", if_match: 7, title: "Switch ports flap under load" },
    },
  ]);
  await act(async () => writes.release());
  await settle();
});

// ---------------------------------------------------------------------------
// The two frames of one task
// ---------------------------------------------------------------------------

// A PEEK RESOLVES ITS OWN PEOPLE: the frame mounts it knowing nothing about an
// org chart, and a peek that waited to be handed a resolver drew every handle
// raw — the set-by lines included.
test("the peek names a person, exactly as the page does", async () => {
  const { container } = mount(<ItemPeek itemKey="ENG-42" />, {
    answers: {
      work_item: detail({
        task: task({ assignee: "ada" }),
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
      }),
    },
  });
  await settle();
  expect(screen.getAllByText("Ada Okonkwo").length).toBeGreaterThan(0);
  const setBy = [...container.querySelectorAll(".props-setby")].map((el) => el.textContent ?? "");
  expect(setBy.length).toBeGreaterThan(0);
  expect(setBy.every((line) => line.includes("Ada Okonkwo"))).toBe(true);
});

// AN OPERATOR IS A PERSON, AND IS DRAWN AS ONE: the reporter a token filed as
// is in no chart, and takes the kind its writes carry.
test("an operator reporter is drawn with a person's circle", async () => {
  const { container } = mount(<ItemPeek itemKey="ENG-42" />, {
    answers: {
      work_item: detail({
        task: task({ assignee: "ada", reporter: "founder" }),
        history: [
          {
            id: "h-1",
            kind: "created",
            actor: "founder",
            actor_kind: "operator",
            at: "2031-04-16T09:00:00Z",
            log_seq: 1,
          },
        ],
      }),
    },
  });
  await settle();
  const chip = [...container.querySelectorAll("a.seat-chip")].find(
    (a) => a.querySelector(".truncate")?.textContent === "founder",
  );
  expect(chip, "the reporter is drawn as a seat chip").toBeTruthy();
  expect(chip!.querySelector(".crewlet-avatar--human")).not.toBeNull();
});

// ONE PERSON, ONE NAME ON ONE PAGE. A task a bound token filed names the
// credential as its reporter; the rail draws the seat the engine names beside
// it (`reporter_seat`) and every set-by line the seat the history row names
// (`actor_seat`) — the same resolution the activity column makes, so the rail
// can no longer say "founder" where the feed says Jane Founder. And the create
// itself draws no set-by line: the Reporter row already says who filed it.
test("an operator-authored task names the person in the reporter and set-by lines", async () => {
  const { container } = mount(<ItemPeek itemKey="ENG-42" />, {
    answers: {
      work_item: detail({
        task: task({ reporter: "founder", priority: "high", assignee: "ada" }),
        reporter_seat: "jane",
        history: [
          {
            id: "h-2",
            kind: "fields",
            actor: "founder",
            actor_kind: "operator",
            actor_seat: "jane",
            at: "2031-04-16T10:00:00Z",
            log_seq: 2,
            fields: { priority: { from: "normal", to: "high" } },
          },
          {
            id: "h-1",
            kind: "created",
            actor: "founder",
            actor_kind: "operator",
            actor_seat: "jane",
            at: "2031-04-16T09:00:00Z",
            log_seq: 1,
            fields: { assignee: { from: "", to: "ada" }, reporter: { from: "", to: "founder" } },
          },
        ],
      }),
    },
  });
  await settle();
  const reporter = rowValue("Reporter");
  expect(reporter.textContent).toContain("Jane Founder");
  expect(reporter.textContent).not.toContain("founder");
  const lines = [...container.querySelectorAll(".props-setby")].map((el) => el.textContent ?? "");
  expect(lines).toHaveLength(1);
  expect(lines[0]).toContain("set by Jane Founder");
  expect(rowValue("Priority").querySelector(".props-setby")).not.toBeNull();
});

// THE PEEK STATES EACH PROPERTY ONCE: its header is the identity and the state
// marks, and every field is the rail's, in the same column.
test("the peek states each property once", async () => {
  const { container } = mount(<ItemPeek itemKey="ENG-42" />);
  await settle();
  expect(container.querySelector(".fact-line")).toBeNull();
  expect(screen.getAllByText("Status")).toHaveLength(1);
  expect(screen.getByText("ENG-42")).toBeTruthy();
});

// THE RAIL'S BODY IS KEYED ON ITS SUBJECT: `[` and `]` move the peek from task
// A to task B, and a different object is a different mount.
test("moving the rail to another task mounts a new body", async () => {
  location.hash = `#/work?peek=${refToken({ kind: "item", id: "ENG-42" })}`;
  const { container } = mount(
    <PeekNeighbours>
      <PeekHost />
    </PeekNeighbours>,
  );
  // THE PEEK'S BODY IS A LAZY CHUNK, so it is waited for rather than settled.
  await waitFor(() => expect(container.querySelector(".object-head")).toBeTruthy(), {
    timeout: 5000,
  });
  const before = container.querySelector(".object-head");
  location.hash = `#/work?peek=${refToken({ kind: "item", id: "ENG-43" })}`;
  await waitFor(() => expect(container.querySelector(".object-head")).not.toBe(before), {
    timeout: 5000,
  });
});

// THE DESCRIPTION IS NOT REBUILT ON EVERY TICK OF THE PAGE'S CLOCK: a reader
// selecting a sentence to copy would lose it within a second.
test("the description keeps its own DOM across a clock tick", async () => {
  const { container } = mount(<WorkItemPage id="ENG-42" />, {
    answers: { work_item: detail({ task: task({ body: "the runbook is **here**" }) }) },
  });
  await settle();
  const before = container.querySelector(".task-prose");
  expect(before).toBeTruthy();
  await act(async () => {
    vi.mocked(Date.now).mockReturnValue(NOW + 1000);
    await new Promise((r) => setTimeout(r, 1100));
  });
  expect(container.querySelector(".task-prose")).toBe(before);
});

// ---------------------------------------------------------------------------
// A removed task
// ---------------------------------------------------------------------------

// A TASK IN THE TRASH IS SAID TO BE IN THE TRASH — the detail read does not
// filter removed tasks, so without the flag it rendered as an ordinary one.
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

test("the removal note names its author and says it is reversible", () => {
  render(<RemovedNote tomb={{ by: "ada", kind: "human", at: "2031-04-15T09:00:00Z" }} now={NOW} />);
  expect(screen.getByText(/ada/)).toBeTruthy();
  expect(screen.getByText(/reversible at any age/)).toBeTruthy();
});

test("a task removed alongside its parent names the parent", () => {
  render(
    <RemovedNote
      tomb={{ by: "ada", kind: "human", at: "2031-04-15T09:00:00Z", removed_with: "ENG-1" }}
      now={NOW}
    />,
  );
  expect(screen.getByText("ENG-1")).toBeTruthy();
});
