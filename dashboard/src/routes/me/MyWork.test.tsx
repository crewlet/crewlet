/**
 * Whose day this screen is showing, how it says so, and what a tab promises.
 *
 * # Whose day
 *
 * The screen is read two ways — a person reading their own day, and an
 * operator reading a report's — and one wording cannot serve both. "Nothing is
 * waiting on them" on your own screen reads as a page describing somebody
 * else, which is precisely the confusion the `viewer` question exists to end.
 *
 * It matters more here than anywhere: this screen used to fall back to the
 * ALPHABETICALLY FIRST SEAT, so a page titled "My work" showed every reader a
 * stranger's day with no indication that it had guessed.
 *
 * # What a section promises
 *
 * The seven claims were stacked cards, each ABSENT when empty, so the page's
 * shape changed with the day and a person with two hundred assignments never
 * saw their asks. The promise that no claim can crowd out another is kept by
 * the SECTION TABS in the page header now — every section carries its count,
 * always — so these cases are about the counts the screen publishes, being
 * honest about what they count, and a section with nothing in it still saying
 * its own name.
 */

import { readFileSync } from "node:fs";
import { join } from "node:path";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";

import { MyWork } from "./MyWork.tsx";
import { Router } from "~/app/router.tsx";
import { usePageCoverage, useSectionCounts } from "~/app/Shell.tsx";
import type { MeSection } from "~/app/routes.ts";
import { useClient, useConnection, useOrg } from "~/lib/store-hooks.ts";
import { Store, type QueryName, type WorkSummary } from "~/protocol/index.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { QueueCountProvider, queueCountParams } from "~/lib/useQueueCount.ts";
import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { ACT_ERRORS } from "~/contract/errors.ts";
import { withDerived } from "~/test/org.ts";
import { settled } from "~/test/settle.ts";
import { applyPageAction, resetPageContextForTest } from "~/app/assistant/context.ts";
import { ENGINE_HEADERS } from "~/test/engineAnswer.ts";

// THE FRAME'S ONE COVERAGE SLOT, stood in for so a case can say WHAT was
// published rather than only that something was drawn: the state bar renders
// nothing at all on a healthy answer, by design, so the published fact is the
// only thing a test can hold.
//
// AND ITS SECTION FIGURES, for the same reason: the tabs that draw them are
// the page header's, so what the screen hands the frame is what it claims.
vi.mock("~/app/Shell.tsx", () => ({
  usePageCoverage: vi.fn(),
  usePageMenu: vi.fn(),
  useSectionCounts: vi.fn(),
}));

vi.mock("~/lib/store-hooks.ts", async () => {
  const actual =
    await vi.importActual<typeof import("~/lib/store-hooks.ts")>("~/lib/store-hooks.ts");
  // THE AGENTS PUSH, which the list reads for the turn running on each card:
  // no seat is working in these cases, so the push is empty.
  return {
    ...actual,
    useClient: vi.fn(),
    useConnection: vi.fn(),
    useOrg: vi.fn(),
    useAgents: () => [],
  };
});

/** Every change the screen sent through `/operator/act`, in order. */
let posted: { tool: string; args: Record<string, unknown> }[];
/** What the act transport answers the next press with. */
let reply: { status: number; body: unknown };

beforeEach(() => {
  posted = [];
  reply = {
    status: 200,
    body: { tool: "set_priorities", outcome: "applied", position: "CREWLET_TRACKER_LOG@1:9" },
  };
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      const tool = decodeURIComponent(String(url).split("/operator/act/")[1] ?? "");
      const body = JSON.parse(init.body as string) as { args: Record<string, unknown> };
      posted.push({ tool, args: body.args });
      return new Response(JSON.stringify(reply.body), {
        status: reply.status,
        headers: { ...ENGINE_HEADERS, "Content-Type": "application/json" },
      });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.mocked(usePageCoverage).mockClear();
  vi.mocked(useSectionCounts).mockClear();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

/** An answer, or how to answer from the question's own parameters. */
type Answer = unknown | ((params: Record<string, unknown>) => unknown);

/** One socket answering each question with a fixture. */
function serving(answers: Partial<Record<QueryName, Answer>>, org: unknown = flatOrg) {
  const query = vi.fn(async (what: string, params?: Record<string, unknown>) => {
    const answer = answers[what as QueryName];
    return typeof answer === "function" ? answer(params ?? {}) : (answer ?? {});
  });
  vi.mocked(useClient).mockReturnValue({ store: new Store(), socket: { query } } as never);
  vi.mocked(useConnection).mockReturnValue({ connected: true, accessRefused: null } as never);
  vi.mocked(useOrg).mockReturnValue(org as never);
  return query;
}

/** One seat of the engine's derived block. */
const derivedSeat = (
  handle: string,
  name: string,
  reports: string[] = [],
  managers: string[] = [],
) => ({
  handle,
  name,
  kind: "human",
  placed_by_ref: false,
  manager: managers[0] ?? "",
  managers: managers.length ? managers : null,
  reports: reports.length ? reports : null,
  auto_reports: null,
  onboarding_chain: null,
});

/** Two seats, and nobody reports to anybody. */
const flatOrg = {
  name: "Acme",
  roles: [
    { name: "Ada Okonkwo", handle: "ada", kind: "human", contact: { slack_user_id: "U0A" } },
    { name: "Rui Santos", handle: "rui", kind: "human", contact: { slack_user_id: "U0R" } },
  ],
  derived: {
    units: [],
    seats: [derivedSeat("ada", "Ada Okonkwo"), derivedSeat("rui", "Rui Santos")],
  },
};

/** The same company with the ENGINE's own hierarchy: Ada leads Rui, not Bo. */
const ledOrg = {
  name: "Acme",
  roles: [
    { name: "Ada Okonkwo", handle: "ada", kind: "human" },
    { name: "Rui Santos", handle: "rui", kind: "human" },
    { name: "Bo Nakamura", handle: "bo", kind: "human" },
  ],
  units: [],
  derived: {
    units: [],
    seats: [
      derivedSeat("ada", "Ada Okonkwo", ["rui"]),
      // THE ENGINE STATES BOTH ENDS of a reporting line, so Rui's manager is
      // Ada as surely as Ada's report is Rui.
      derivedSeat("rui", "Rui Santos", [], ["ada"]),
      derivedSeat("bo", "Bo Nakamura"),
    ],
  },
};

const emptyDay = {
  handle: "ada",
  priorities: [],
  assigned: [],
  asked_of_me: [],
  checklist_items: [],
  collaborating: [],
  watching_recent: [],
  unblocked_recent: [],
  totals: {
    priorities: { total: 0 },
    assigned: { total: 0 },
    asked_of_me: { total: 0 },
    checklist_items: { total: 0 },
    collaborating: { total: 0 },
    watching_recent: { total: 0 },
    unblocked_recent: { total: 0 },
  },
  complete: true,
};

/** The assignments, which are the tracker's own question rather than a block. */
const noWork = { items: [], groups: [], total_hint: 0, complete: true };

const ada = {
  login: "ops-1",
  grants: ["admin", "worklet", "doclet"],
  handle: "ada",
  owner: "ada",
  name: "Ada Okonkwo",
  kind: "human",
};

/** A principal the directory binds to no seat: their record is their login. */
const unbound = {
  login: "token:ops-7",
  grants: ["worklet", "doclet", "admin"],
  handle: "",
  owner: "token:ops-7",
  name: "",
  kind: "",
};

/** Nobody signed in. */
const anonymous = { login: "", grants: [], handle: "", owner: "", name: "", kind: "" };

/** One task with the fields a row and a band are decided from. */
function task(over: Partial<WorkSummary> = {}): WorkSummary {
  return {
    id: over.key ?? "t1",
    key: over.key ?? "ENG-1",
    project: "ENG",
    title: "Ship the thing",
    type: "task",
    status: "todo",
    updated: "2031-04-16T09:00:00Z",
    version: 1,
    ...over,
  } as WorkSummary;
}

/** Mount one section, the way the router's resolver hands it over — inside
 *  the two frame readings it reads: who the viewer is, and their own Queue
 *  count. */
function mount(section: MeSection = "queue") {
  return render(
    <ViewerProvider>
      <QueueCountProvider>
        <Router>
          <MyWork section={section} />
        </Router>
      </QueueCountProvider>
    </ViewerProvider>,
  );
}

/** The figures the screen last handed the page header, once it has any. */
async function counts(): Promise<Record<string, string>> {
  return await waitFor(() => {
    const last = vi
      .mocked(useSectionCounts)
      .mock.calls.map((c) => c[0])
      .filter((m) => Object.keys(m).length > 0)
      .at(-1);
    if (!last) throw new Error("no figures published yet");
    return last;
  });
}

// ---------------------------------------------------------------------------
// Whose day
// ---------------------------------------------------------------------------

// THE VIEWER DECIDES WHOSE DAY IT IS, with no handle in the URL. Before the
// `viewer` question existed there was nothing for this to resolve from, and
// the screen picked the first seat in the roster.
test("with no handle it shows the viewer's own day, and says it is theirs", async () => {
  serving({ viewer: ada, work_my_work: emptyDay, work_items: noWork });
  mount();
  // THE PICKER NAMES THEM, and the day speaks in the SECOND PERSON.
  await waitFor(() =>
    expect(screen.getByRole("combobox", { name: "Whose day" }).textContent).toContain(
      "Ada Okonkwo",
    ),
  );
  await waitFor(() => expect(screen.getByText("Nothing is assigned to you")).toBeTruthy());
});

// AN OPERATOR READING SOMEBODY ELSE'S DAY is a real thing to do, and the
// screen has to say so: a page called "My work" showing a colleague's without
// naming them is how a reader acts on work that is not theirs.
test("an explicit handle names whose day it is, in the third person", async () => {
  location.hash = "#/me?handle=rui";
  serving({ viewer: ada, work_my_work: { ...emptyDay, handle: "rui" }, work_items: noWork });
  mount();
  // NAMED BY THE PICKER, and the day speaks in the THIRD PERSON.
  await waitFor(() =>
    expect(screen.getByRole("combobox", { name: "Whose day" }).textContent).toContain("Rui Santos"),
  );
  await waitFor(() => expect(screen.getByText("Nothing is assigned to them")).toBeTruthy());
});

// AN UNBOUND READER HAS A DAY OF THEIR OWN, kept under their login — the name
// their assistant writes their priorities and marks under. Asked for by the
// SEAT, an unbound reader was shown nothing but a sentence about binding while
// the record sat under a name the screen never asked for. It shows the day
// now, and says what binding would add above it.
test("an unbound reader's own day is the one kept under their login", async () => {
  const query = serving({
    viewer: unbound,
    work_my_work: { ...emptyDay, handle: "token:ops-7" },
    work_items: noWork,
  });
  mount();
  await waitFor(() => expect(screen.getByText("Nothing is assigned to you")).toBeTruthy());
  expect(query).toHaveBeenCalledWith("work_my_work", { handle: "token:ops-7" });
  // AND WHAT BINDING WOULD ADD, with the command that binds it, rather than
  // reporting a fault, which is a person's (`Inbox.test.tsx`).
  expect(screen.getByText(/bind this account to an open seat/)).toBeTruthy();
  expect(screen.getByText("crewlet iam bind ID SEAT")).toBeTruthy();
  expect(screen.queryByText(/Every person holds a human seat/)).toBeNull();
  // THE LOGIN IS NAMED BY THE NOTE ABOVE THE DAY, never as a link: a login is
  // not a seat, and a seat page for it would answer "no such seat".
  expect(screen.getAllByText(/token:ops-7/).length).toBeGreaterThan(0);
  expect(screen.queryByRole("link", { name: /token:ops-7/ })).toBeNull();
});

// AND THE THREE VIEWER STATES TAKE THREE SENTENCES: nobody signed in is not an
// unbound reader, whose day is theirs, and neither is a reader who has not
// chosen somebody.
test("nobody signed in is a different sentence from an unbound reader", async () => {
  serving({ viewer: anonymous });
  mount();
  await waitFor(() => expect(screen.getByText(/Nobody is signed in/)).toBeTruthy());
  expect(screen.queryByText(/not bound to a seat/)).toBeNull();
});

// THE DAY MOVES WITH THE READER'S INBOX. The record that hands somebody work,
// asks them something or reorders their queue is the record that writes them a
// notice, so the frame that moves their inbox is what asks the day again —
// every read the strip and the banner are drawn from, not only one of them,
// since a tab count that moved while the banner did not is two answers about
// one change.
test("the reader's own day is asked again when their inbox moves", async () => {
  const store = new Store();
  const answers: Partial<Record<QueryName, unknown>> = {
    viewer: ada,
    work_my_work: emptyDay,
    work_items: noWork,
    work_person: { handle: "ada", version: 1, held: true, complete: true },
  };
  const query = vi.fn(
    async (what: string, _params?: Record<string, unknown>) => answers[what as QueryName] ?? {},
  );
  vi.mocked(useClient).mockReturnValue({ store, socket: { query } } as never);
  vi.mocked(useConnection).mockReturnValue({ connected: true, accessRefused: null } as never);
  vi.mocked(useOrg).mockReturnValue(flatOrg as never);
  mount();
  await counts();
  // THE SECTION COUNTS, not the list under them: the list carries an
  // arrangement, and it is the list's own question rather than the day's.
  const asked = (what: string) =>
    query.mock.calls.filter(
      (c) => c[0] === what && (c[1] as Record<string, unknown> | undefined)?.group_by === undefined,
    ).length;
  await waitFor(() => expect(asked("work_person")).toBeGreaterThan(0));
  const before = {
    day: asked("work_my_work"),
    person: asked("work_person"),
    count: asked("work_items"),
  };

  act(() => {
    store.applyInboxChanged({ handle: "ada", unread_delta: 1, subject: "t-1", reason: "assignee" });
  });
  await waitFor(() => expect(asked("work_my_work")).toBeGreaterThan(before.day));
  await waitFor(() => expect(asked("work_person")).toBeGreaterThan(before.person));
  await waitFor(() => expect(asked("work_items")).toBeGreaterThan(before.count));
});

// NO ROW REPEATS WHOSE DAY THIS IS. The picker names the person and every
// held control on somebody else's day says whose it is; a row under them
// repeating the seat's chip, its handle and "yours" or "their day" said it a
// third time, and it is gone. Mutation: draw the row again and this fails.
test("no row under the bar repeats whose day it is", async () => {
  location.hash = "#/me?handle=rui";
  serving({
    viewer: ada,
    work_my_work: { ...emptyDay, handle: "rui" },
    work_items: noWork,
    work_person: { handle: "rui", version: 1, held: true, complete: true },
  });
  mount();
  await waitFor(() => expect(screen.getByText("Nothing is assigned to them")).toBeTruthy());
  expect(screen.queryByRole("link", { name: /Rui Santos/ })).toBeNull();
  expect(document.querySelector('a[href="#/company/org/seats/rui"]')).toBeNull();
  expect(screen.queryByText("their day")).toBeNull();
  expect(screen.queryByText("yours")).toBeNull();
});

// ---------------------------------------------------------------------------
// The strip
// ---------------------------------------------------------------------------

// EVERY CLAIM IS A SECTION AND EVERY SECTION CARRIES ITS COUNT. This is what
// replaces the stacking: an unanswered question is visible as a number on a
// section nobody has opened, where a card that vanished when empty took its
// own name with it. The priorities are the queue in somebody's order, so they
// are the queue's figure rather than a section of their own.
test("every claim is a section, and its count is published unopened", async () => {
  serving({
    viewer: ada,
    // THE TWO LIST SECTIONS ARE THE TRACKER'S OWN COUNTS, each its own
    // question: the work held, and the work a question is waiting on.
    work_items: (p: Record<string, unknown>) => ({ ...noWork, total_hint: p.asked_by ? 3 : 7 }),
    work_my_work: {
      ...emptyDay,
      asked_of_me: [
        {
          key: "ENG-9",
          title: "t",
          comment: "c1",
          asked_by: "rui",
          asked_at: "2031-04-16T09:00:00Z",
          body: "why?",
          open: true,
          answer_with: "answer_work_question(...)",
        },
      ],
      watching_recent: [task({ key: "ENG-3" }), task({ key: "ENG-4" })],
      totals: {
        ...emptyDay.totals,
        asked_of_me: { total: 1 },
        watching_recent: { total: 2 },
      },
    },
  });
  mount();
  await waitFor(async () => expect((await counts()).queue).toBe("7"));
  await waitFor(async () => expect((await counts())["asked-by-me"]).toBe("3"));
  expect(await counts()).toEqual({
    queue: "7",
    "asked-of-me": "1",
    "asked-by-me": "3",
    unblocked: "0",
    collaborating: "0",
    watching: "2",
    checklist: "0",
  });
});

// A BLOCK'S COUNT IS THE ENGINE'S TOTAL, NOT ITS PAGE. `work_my_work` answers
// twenty rows per claim, so a length says `20` on a person holding a hundred
// and thirty — the page size drawn as a fact about their day. The engine counts
// each block in full beside its page, and that is the number on the tab.
test("a claim's count is the engine's total, not the length of its page", async () => {
  serving({
    viewer: ada,
    work_items: { ...noWork, total_hint: 0 },
    work_my_work: {
      ...emptyDay,
      collaborating: Array.from({ length: 20 }, (_, i) => task({ key: `ENG-${i}` })),
      totals: { ...emptyDay.totals, collaborating: { total: 130 } },
    },
  });
  mount();
  expect((await counts()).collaborating).toBe("130");
});

// AND A `+` ONLY WHERE THE ENGINE'S OWN COUNT STOPPED: a capped total is a
// floor, and drawn bare it reads as exactly the ceiling.
test("a capped claim total says it is a floor", async () => {
  serving({
    viewer: ada,
    work_items: { ...noWork, total_hint: 0 },
    work_my_work: {
      ...emptyDay,
      totals: { ...emptyDay.totals, watching_recent: { total: 10000, capped: true } },
    },
  });
  mount();
  expect((await counts()).watching).toBe(`${(10000).toLocaleString()}+`);
});

// AND ASSIGNED ESCAPES IT, by asking the tracker's own question: `total_hint`
// is a count over the matching set rather than over a page, so the tab says
// what the person actually holds.
test("the assignments are the tracker's count, not a block's page", async () => {
  serving({
    viewer: ada,
    work_my_work: { ...emptyDay, assigned: [task()] },
    work_items: { ...noWork, items: [task()], total_hint: 137 },
  });
  mount();
  await waitFor(async () => expect((await counts()).queue).toBe("137"));
});

// AND THE TRACKER'S CEILING READS THE SAME WAY: a `total_capped` count is a
// floor on the Queue exactly as it is on the claims beside it.
test("a capped assignment count says it is a floor", async () => {
  serving({
    viewer: ada,
    work_my_work: { ...emptyDay, assigned: [task()] },
    work_items: { ...noWork, items: [task()], total_hint: 10000, total_capped: true },
  });
  mount();
  await waitFor(async () => expect((await counts()).queue).toBe(`${(10000).toLocaleString()}+`));
});

// ONE FIGURE, ONE READ. On the reader's own day the Queue's count is the
// frame's reading — the one the sidebar's My work row draws — so the tab and
// the row cannot name two numbers after a change; this screen asks its own
// only for somebody else's day. Two reads of one question poll on two clocks.
test("on your own day the Queue's count is the sidebar's reading, not a second one", async () => {
  const query = serving({
    viewer: ada,
    work_my_work: emptyDay,
    work_items: { ...noWork, total_hint: 4 },
  });
  mount();
  await waitFor(async () => expect((await counts()).queue).toBe("4"));
  const counted = query.mock.calls.filter(
    (c) =>
      c[0] === "work_items" &&
      (c[1] as Record<string, unknown>)?.limit === 1 &&
      (c[1] as Record<string, unknown>)?.assignee,
  );
  expect(counted.map((c) => (c[1] as Record<string, unknown>).assignee)).toEqual(["ada"]);
});

test("on somebody else's day the Queue counts their work, with its own read", async () => {
  location.hash = "#/me?handle=rui";
  const query = serving({
    viewer: ada,
    work_my_work: { ...emptyDay, handle: "rui" },
    work_items: (p: Record<string, unknown>) => ({
      ...noWork,
      total_hint: p.assignee === "rui" ? 9 : 4,
    }),
  });
  mount();
  await waitFor(async () => expect((await counts()).queue).toBe("9"));
  const assignees = query.mock.calls
    .filter(
      (c) =>
        c[0] === "work_items" &&
        (c[1] as Record<string, unknown>)?.limit === 1 &&
        (c[1] as Record<string, unknown>)?.assignee,
    )
    .map((c) => (c[1] as Record<string, unknown>).assignee);
  // THE FRAME'S READ IS THE VIEWER'S, whatever day is on screen.
  expect(new Set(assignees)).toEqual(new Set(["ada", "rui"]));
});

// NOTHING IS CLAIMED WHILE THE READ IS IN FLIGHT. A zero on the section a
// reader lands on is "your day is empty", which is a claim — and it is a false
// one for as long as the answer has not arrived.
test("the queue claims no count until the tracker answers", async () => {
  const query = vi.fn(async (what: string) =>
    what === "work_items"
      ? new Promise(() => {})
      : what === "viewer"
        ? ada
        : what === "work_my_work"
          ? emptyDay
          : {},
  );
  vi.mocked(useClient).mockReturnValue({ store: new Store(), socket: { query } } as never);
  vi.mocked(useConnection).mockReturnValue({ connected: true, accessRefused: null } as never);
  vi.mocked(useOrg).mockReturnValue(
    withDerived({
      name: "Acme",
      roles: [{ name: "Ada Okonkwo", handle: "ada", kind: "human" }],
    }) as never,
  );
  mount();
  // ABSENT, not an empty string the header has to know to skip: the other
  // sections answered, and the queue is simply not among them.
  const figures = await counts();
  expect(figures.watching).toBe("0");
  expect(figures).not.toHaveProperty("queue");
});

// ---------------------------------------------------------------------------
// What a tab draws
// ---------------------------------------------------------------------------

// THE ASSIGNED TAB IS THE WORK LIST, narrowed to one person — not a second
// renderer. Written twice it had no Filter menu, no Display menu, no scope
// switch, no chips, no count line and no way past its two hundredth row, and
// each of those is a rule the work list already keeps.
test("the queue draws the work list's own toolbar", async () => {
  serving({ viewer: ada, work_my_work: emptyDay, work_items: { ...noWork, items: [task()] } });
  mount();
  await waitFor(() => expect(screen.getByText("Ship the thing")).toBeTruthy());
  expect(screen.getByRole("button", { name: "Filter" })).toBeTruthy();
  expect(screen.getByText("Open")).toBeTruthy();
  expect(screen.getByText("1 in Open")).toBeTruthy();
});

// THE WORK LIST IS ONE BOX on every host: its row of shapes, the band that
// cuts the answer and the answer stand together in `.work-view`, which spaces
// them by nothing, so the band's own padding is all the room either side of
// its controls. Returned as loose rows, they took the content column's 20px
// gap here (and `.work-main`'s 16px on Worklet), and the band stood in 28px
// of empty space above and below. Mutation: return the rows loose, or give
// the box a gap, and this goes red.
test("the queue's work list is one box, its rows standing together", async () => {
  serving({ viewer: ada, work_my_work: emptyDay, work_items: { ...noWork, items: [task()] } });
  mount();
  await waitFor(() => expect(screen.getByText("Ship the thing")).toBeTruthy());
  const box = document.querySelector(".toolbar.work-bar")!.parentElement!;
  expect(box.classList.contains("work-view")).toBe(true);
  expect([...box.children].map((row) => row.classList[0])).toEqual([
    "work-tabs",
    "toolbar",
    "work-body",
  ]);
  const sheet = readFileSync(join(process.cwd(), "src/styles/screens.css"), "utf8");
  const rule = /\.work-view \{([^}]*)\}/.exec(sheet)?.[1] ?? "";
  expect(rule).toContain("flex-direction: column");
  expect(rule).not.toMatch(/gap/);
});

// AND THE BANDS ARE THE ENGINE'S. `due:bucket` is cut against the COMPANY's
// day start, like the row's own overdue flag and every `due=` filter, so the
// heading a task sits under and the flag beside it cannot disagree. Computed
// here, from the browser's own midnight, they could and did.
test("the queue opens on the engine's due bands, soonest first", async () => {
  const query = serving({ viewer: ada, work_my_work: emptyDay, work_items: noWork });
  mount();
  const asked = await waitFor(() => {
    const call = query.mock.calls.find(
      (c) => c[0] === "work_items" && (c[1] as Record<string, unknown>)?.group_by,
    );
    if (!call) throw new Error("the list has not asked yet");
    return call[1] as Record<string, unknown>;
  });
  expect(asked.group_by).toBe("due:bucket");
  expect(asked.sort).toBe("due");
  expect(asked.status_group).toBe("not_started,active");
});

// THE STRIP COUNTS WHAT THE TAB LISTS: every task on its own. In the
// grammar's default a root this person holds brings its subtree along
// unfiltered — sub-tasks held by somebody else, finished ones — so a strip
// asked that way said more than the list under it ever drew.
test("the assigned count asks the list's own subtask mode", async () => {
  const query = serving({ viewer: ada, work_my_work: emptyDay, work_items: noWork });
  mount();
  const calls = await waitFor(() => {
    const found = query.mock.calls
      .filter((c) => c[0] === "work_items")
      .map((c) => c[1] as Record<string, unknown>);
    if (!found.some((p) => !p.group_by && p.assignee) || !found.some((p) => p.group_by)) {
      throw new Error("the strip and the list have not both asked yet");
    }
    return found;
  });
  const strip = calls.find((p) => !p.group_by && p.assignee)!;
  const list = calls.find((p) => p.group_by)!;
  expect(strip.subtasks).toBe("separate");
  expect(strip.subtasks).toBe(list.subtasks);
  expect(strip.status_group).toBe(list.status_group);
});

// THE LOCK REACHES THE WIRE AND NOTHING ELSE. It is what the tab IS rather
// than a narrowing somebody chose, so there is no chip to take off, no row in
// the Filter menu and no key on the address — where a key would be a second,
// silent answer to the one question the screen has already answered.
test("the assignee is locked on the wire and absent from the URL and the chips", async () => {
  location.hash = "#/me?handle=rui";
  const query = serving({
    viewer: ada,
    work_my_work: { ...emptyDay, handle: "rui" },
    work_items: { ...noWork, items: [task()] },
  });
  mount();
  await waitFor(() => expect(screen.getByText("Ship the thing")).toBeTruthy());
  // EVERY READ OF THE QUEUE — the list and its count; Asked by me's own
  // count is a different question, held to the asker instead, and the frame's
  // count of the VIEWER's own queue is the sidebar's, whatever day is open.
  const frames = JSON.stringify(queueCountParams("ada"));
  const items = query.mock.calls.filter(
    (c) =>
      c[0] === "work_items" &&
      !(c[1] as Record<string, unknown>).asked_by &&
      JSON.stringify(c[1]) !== frames,
  );
  expect(items.length).toBeGreaterThan(0);
  for (const call of items) {
    expect((call[1] as Record<string, unknown>).assignee).toBe("rui");
  }
  expect(location.hash).not.toContain("assignee=");
  // No chip names it — and the Filter menu does not offer the row, which is
  // asserted on the KEY each row prints rather than on its label, because
  // "Status" is also a column head and a Display option one bar over.
  // (The grid's assignee column still names its glyph head for a screen
  // reader; that is the column, not a chip.)
  expect(
    screen.queryAllByText("Assignee").filter((el) => !el.closest(".crewlet-data-grid__head")),
  ).toHaveLength(0);
  fireEvent.click(screen.getByRole("button", { name: "Filter" }));
  await waitFor(() => expect(screen.getByText("priority=")).toBeTruthy());
  expect(screen.queryByText("assignee=")).toBeNull();
});

// AND THE ASSISTANT CANNOT WIDEN IT EITHER: the queue's page action writes
// the list's own filters, and refuses a patch naming the key the page is,
// clearing it included, rather than applying a change the lock would undo.
test("the assistant's page action never widens the queue past its person", async () => {
  location.hash = "#/me?handle=rui";
  serving({
    viewer: ada,
    work_my_work: { ...emptyDay, handle: "rui" },
    work_items: { ...noWork, items: [task()] },
  });
  mount();
  await waitFor(() => expect(screen.getByText("Ship the thing")).toBeTruthy());
  const ask = (payload: unknown) =>
    applyPageAction({
      correlation_id: "c",
      action: "work.filter.apply",
      payload,
      source_revision: "",
    });
  for (const payload of [
    { assignee: null },
    { assignee: "ada" },
    { status: "open", assignee: "" },
  ]) {
    expect(ask(payload)).toMatchObject({ outcome: "blocked" });
  }
  expect(location.hash).not.toContain("assignee=");
  expect(ask({ status: "open" }).outcome).toBe("applied");
  await waitFor(() => expect(location.hash).toContain("status=open"));
  // AND CLEAR TAKES THE FILTER OFF AND LEAVES THE PERSON.
  expect(
    applyPageAction({
      correlation_id: "d",
      action: "work.filters.clear",
      payload: {},
      source_revision: "",
    }).outcome,
  ).toBe("applied");
  await waitFor(() => expect(location.hash).not.toContain("status="));
  expect(location.hash).toContain("handle=rui");
  expect(location.hash).not.toContain("assignee=");
  resetPageContextForTest();
});

// AND THE WORKSPACE'S SAVED VIEWS ARE NOT THIS PERSON'S CLAIMS. The strip's
// first tab reads "All work", which over one person's list is false, and the
// page header's own sections already name the page.
test("the hosted list draws no saved-view strip and asks for none", async () => {
  const query = serving({ viewer: ada, work_my_work: emptyDay, work_items: noWork });
  mount();
  await counts();
  expect(screen.queryByText("All work")).toBeNull();
  expect(screen.queryByRole("link", { name: "All saved views" })).toBeNull();
  expect(query.mock.calls.map((c) => c[0])).not.toContain("work_views");
});

// A SECTION WITH NOTHING IN IT STILL SAYS ITS OWN NAME. A card that vanished
// took its name with it, so a reader could not tell "nothing here" from "this
// product does not have that" — which a section cannot do, because its tab is
// still in the header.
test("an empty claim says what would be in it", async () => {
  location.hash = "#/me/unblocked";
  serving({ viewer: ada, work_my_work: emptyDay, work_items: noWork });
  mount("unblocked");
  await waitFor(() => expect(screen.getByText("Nothing here")).toBeTruthy());
  expect(screen.getByText("Nothing newly unblocked.")).toBeTruthy();
});

// THE QUESTIONS PUT TO SOMEBODY ELSE ARE NOT WRITTEN AS THE READER'S. This is
// the one section where somebody else is blocked on this person rather than
// the other way round, and it is read on a report's day as often as on your own.
test("questions put to somebody else are not addressed to the reader", async () => {
  location.hash = "#/me/asked-of-me?handle=rui";
  serving({
    viewer: ada,
    work_items: noWork,
    work_my_work: { ...emptyDay, handle: "rui" },
  });
  mount("asked-of-me");
  await waitFor(() => expect(screen.getByText("Nothing is waiting on them")).toBeTruthy());
  expect(screen.queryByText("Nothing is waiting on you")).toBeNull();
});

/** A structured ask put to `asked_of`, as `work_my_work.asked_of_me` carries it. */
function askRow(over: Record<string, unknown> = {}) {
  return {
    ...task({ key: "ENG-9", title: "Which reader?" }),
    comment: "c1",
    // THE ASKER'S RECORD NAME (`iam.ActorFor`): a person the directory binds
    // to a seat asks AS that seat, so the row names the person by it.
    asked_by: "rui",
    asked_at: "2031-04-16T09:00:00Z",
    body: "parent or replies?",
    open: true,
    answer_with: 'comment_on_work_item(item: "ENG-9", answers: "c1", choice: "…")',
    decision: {
      question: "Read the parent or the replies?",
      options: [
        { id: "parent", label: "The parent" },
        { id: "replies", label: "The replies" },
      ],
      recommended: "parent",
      role: "approver",
    },
    ...over,
  };
}

/** Everything a bound reader who may act is served. */
const adaActs = {
  ...ada,
  acts: ["comment_on_work_item", "set_priorities", "update_work_item", "place_work_item"],
};

// THE QUESTIONS PUT TO THIS PERSON ARE ANSWERED WHERE THEY STAND: the list
// Home draws, grouped under who asked, with Accept and Change as the answer. The literal tool call this section
// once printed was the gesture a person's assistant makes; the dashboard makes
// it itself now, as the person (ADR-0024).
test("an ask on your own day is answered in place with the option chosen", async () => {
  location.hash = "#/me/asked-of-me";
  serving({
    viewer: adaActs,
    work_items: noWork,
    work_my_work: { ...emptyDay, asked_of_me: [askRow()], totals: emptyDay.totals },
  });
  mount("asked-of-me");
  // THE PERSON WHO ASKED, never the credential that wrote it.
  await waitFor(() =>
    expect(document.querySelector(".decision-group-who")?.textContent).toBe("Rui Santos"),
  );
  expect(screen.getByText("Read the parent or the replies?")).toBeTruthy();
  expect(screen.getByText(/you are the approver/)).toBeTruthy();
  expect(screen.queryByText(/comment_on_work_item\(/)).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Change" }));
  fireEvent.click(screen.getByRole("radio", { name: /The replies/ }));
  fireEvent.click(screen.getByRole("button", { name: "Submit" }));
  await waitFor(() => expect(posted.length).toBe(1));
  expect(posted[0]).toEqual({
    tool: "comment_on_work_item",
    args: { item: "ENG-9", answers: "c1", choice: "replies" },
  });
});

// AND ON SOMEBODY ELSE'S DAY THE SAME ROW IS ABOUT THEM, and its answers are
// held with the sentence that says whose they are. The engine refuses an
// answer from anybody but the person asked, so a pressable option would be a
// refusal waiting to happen — and "you are the approver" a sentence about the
// wrong person.
test("an ask on somebody else's day is written about them and cannot be answered", async () => {
  location.hash = "#/me/asked-of-me?handle=rui";
  serving({
    viewer: adaActs,
    work_items: noWork,
    work_my_work: {
      ...emptyDay,
      handle: "rui",
      asked_of_me: [askRow({ asked_by: "bo" })],
    },
  });
  mount("asked-of-me");
  await waitFor(() => expect(screen.getByText(/Rui Santos is the approver/)).toBeTruthy());
  expect(screen.queryByText(/you are the approver/)).toBeNull();
  // SAID ONCE ON THE PAGE, not only on a hover: a row of dimmed options with
  // nothing said reads as a broken screen.
  expect(screen.getByText("Answering is off. Viewing Rui Santos’s day. Read-only.")).toBeTruthy();
  const accept = screen.getByRole("button", { name: "Accept" });
  expect(accept.getAttribute("aria-disabled")).toBe("true");
  expect(accept.getAttribute("title")).toContain("Viewing Rui Santos’s day. Read-only.");
  expect(screen.getByRole("button", { name: "Change" }).getAttribute("aria-disabled")).toBe("true");
  fireEvent.click(accept);
  await settled(() => posted.length);
  expect(posted).toEqual([]);
});

// AND A PAGE OF ASKS SAYS IT IS ONE: the tab carries the engine's total, and
// twenty rows under a tab saying 35 would read as the tab being wrong.
test("a page of asks under a larger total says which ones it holds", async () => {
  location.hash = "#/me/asked-of-me";
  serving({
    viewer: adaActs,
    work_items: noWork,
    work_my_work: {
      ...emptyDay,
      asked_of_me: [askRow()],
      totals: { ...emptyDay.totals, asked_of_me: { total: 35 } },
    },
  });
  mount("asked-of-me");
  await waitFor(() => expect(screen.getByText(/The newest 1 of 35\./)).toBeTruthy());
  expect(screen.getByRole("link", { name: /All of them in the Inbox/ })).toBeTruthy();
});

// ---------------------------------------------------------------------------
// Asked by me
// ---------------------------------------------------------------------------

// WHAT THIS PERSON IS WAITING ON is the work list held to the ASKER, by the
// one name the engine records them under (`iam.ActorFor`): a person bound to a
// seat asks AS the seat whichever credential they asked through, so there is
// no second name to ask under and no `viewer` to send beside it.
test("asked by me asks for the person's questions by the one name they are recorded under", async () => {
  location.hash = "#/me/asked-by-me?handle=rui";
  const query = serving({
    viewer: ada,
    work_my_work: { ...emptyDay, handle: "rui" },
    work_items: (p: Record<string, unknown>) =>
      p.asked_by === "rui" && !("viewer" in p)
        ? {
            ...noWork,
            items: [task({ key: "ENG-77", title: "Asked through their assistant" })],
            total_hint: 1,
          }
        : noWork,
  });
  mount("asked-by-me");
  await waitFor(() => expect(screen.getByText("Asked through their assistant")).toBeTruthy());
  const asked = query.mock.calls
    .filter((c) => c[0] === "work_items" && (c[1] as Record<string, unknown>).asked_by)
    .map((c) => c[1] as Record<string, unknown>);
  // THE LIST AND ITS COUNT, both held to the person.
  expect(asked.some((p) => p.group_by === undefined && p.limit === 1)).toBe(true);
  expect(asked.some((p) => p.limit !== 1)).toBe(true);
  for (const params of asked) {
    expect(params.asked_by).toBe("rui");
    expect(params).not.toHaveProperty("viewer");
  }
  expect((await counts())["asked-by-me"]).toBe("1");
  // NOT A CHIP AND NOT A KEY: the asker is what the section IS.
  expect(location.hash).not.toContain("asked_by=");
});

// A QUESTION LEFT OPEN ON FINISHED WORK IS STILL ONE SOMEBODY IS WAITING ON, so
// the section opens on every status rather than hiding the one most likely to
// be forgotten.
test("asked by me opens on every status", async () => {
  location.hash = "#/me/asked-by-me";
  const query = serving({ viewer: ada, work_my_work: emptyDay, work_items: noWork });
  mount("asked-by-me");
  const list = await waitFor(() => {
    const call = query.mock.calls.find(
      (c) =>
        c[0] === "work_items" &&
        (c[1] as Record<string, unknown>).asked_by &&
        (c[1] as Record<string, unknown>).limit !== 1,
    );
    if (!call) throw new Error("the list has not asked yet");
    return call[1] as Record<string, unknown>;
  });
  expect(list.status_group).toBeUndefined();
  expect(list.show_closed).toBe("true");
  await waitFor(() => expect(screen.getByText("You are not waiting on an answer")).toBeTruthy());
});

/** One day whose queue somebody else put in order. */
const orderedByRui = {
  viewer: ada,
  work_items: noWork,
  work_my_work: { ...emptyDay, priorities: [task({ key: "ENG-5" })] },
  work_person: {
    handle: "ada",
    priorities_set_by: "rui",
    priorities_set_at: "2031-04-16T09:00:00Z",
    version: 1,
    held: true,
    complete: true,
  },
};

// A QUEUE SOMEBODY ELSE ORDERED IS STAMPED, and the stamp is the one thing on
// this screen that asks for an acknowledgement: a person who starts the day on
// work they did not choose can see who chose it.
//
// ON THE READING THEY LANDED ON, which is the half that was missing. The stamp
// lived inside the Priorities panel, so it reached only a reader who had
// already acknowledged it by opening that panel — while their own next change
// to the queue cleared it for good, which makes the miss unrecoverable rather
// than merely late.
test("a queue a lead ordered is announced on the reading nobody opened", async () => {
  serving(orderedByRui);
  mount();
  await waitFor(() => expect(screen.getByText(/Order set by/)).toBeTruthy());
  expect(screen.getByText(/Rui Santos/)).toBeTruthy();
  // The queue opens ordered by due date, so the banner is what carried it.
  expect(screen.getByRole("radio", { name: /Due/ }).getAttribute("aria-checked")).toBe("true");
  // And the way to the reading it is about is offered from here.
  expect(screen.getByRole("button", { name: "Priorities →" })).toBeTruthy();
});

/** The queue's Priorities reading, in the page bar's order switch. */
function prioritiesOption(): HTMLElement {
  return screen.getByRole("radio", { name: /Priorities/ });
}

// AND THE SWITCH ITSELF IS MARKED, because a banner is read once and a bar is
// scanned: the flag and its title are what a reader who scrolled past the
// banner still sees.
test("the priorities reading carries a mark while somebody else's order stands", async () => {
  serving(orderedByRui);
  mount();
  // THE PERSON'S NAME, as the banner says it — never the bare handle.
  await waitFor(() =>
    expect(prioritiesOption().getAttribute("title")).toBe("Ordered by Rui Santos"),
  );
});

// A QUEUE SOMEBODY ORDERED THEMSELVES IS NOT NEWS. The engine clears the stamp
// on the person's own write, so an absent one is a settled fact rather than a
// missing one, and neither the banner nor the mark is drawn for it.
test("a queue nobody else ordered carries neither banner nor mark", async () => {
  serving({
    viewer: ada,
    work_items: noWork,
    work_my_work: { ...emptyDay, priorities: [task({ key: "ENG-5" })] },
    work_person: { handle: "ada", version: 1, held: true, complete: true },
  });
  mount();
  await counts();
  expect(screen.queryByText(/Order set by/)).toBeNull();
  expect(prioritiesOption().getAttribute("title")).toBeNull();
});

// AND A LINK TO THE PAGE YOU ARE ON IS A LIE. Opened, the Priorities reading
// is where the control would send the reader, so the banner keeps its
// sentence and drops the control.
test("the banner offers no way to the reading that is already open", async () => {
  location.hash = "#/me?order=priorities";
  serving(orderedByRui);
  mount();
  await waitFor(() => expect(screen.getByText(/Order set by/)).toBeTruthy());
  expect(screen.queryByRole("button", { name: "Priorities →" })).toBeNull();
});

// THE COVERAGE OF THE READ THAT IDENTIFIES THIS OBJECT GOES TO THE FRAME, and
// this screen publishes exactly one: the frame holds one slot with one setter,
// so two publishers on one screen is a last-writer-wins race. `#/me`'s object
// is a person, so the person's own record is what the state bar carries; the
// other two reads state their own beside the rows they drew.
test("the page publishes the person's coverage, once", async () => {
  serving({
    viewer: ada,
    work_items: noWork,
    work_my_work: emptyDay,
    work_person: { handle: "ada", version: 1, held: true, complete: true, read_level: "stale" },
  });
  mount();
  const published = await waitFor(() => {
    const answers = vi
      .mocked(usePageCoverage)
      .mock.calls.map((c) => c[0])
      .filter(Boolean);
    if (answers.length === 0) throw new Error("nothing published yet");
    return answers;
  });
  for (const answer of published) {
    const record = answer as { handle?: string; held?: boolean };
    expect(record.handle).toBe("ada");
    // ASSERTED ON A FIELD ONLY THE PERSON'S RECORD CARRIES. Both personal
    // reads answer under the same handle, so a screen that published the
    // WRONG one would satisfy a handle check and nothing else.
    expect(record.held).toBe(true);
  }
});

// ---------------------------------------------------------------------------
// Whose day to read
// ---------------------------------------------------------------------------

/** The picker's rows, as the listbox draws them once it is open. */
async function pickerRows(): Promise<HTMLElement[]> {
  fireEvent.click(await screen.findByRole("combobox", { name: "Whose day" }));
  return await waitFor(() => {
    const rows = screen.getAllByRole("option");
    if (rows.length === 0) throw new Error("the listbox has not opened");
    return rows;
  });
}

// YOURS, THEN YOUR LINE, THEN ANYBODY. Flat and alphabetical the control
// answered "which of the company's seats" with every seat, in an order that
// has nothing to do with the question — and the two days a reader opens are
// their own and one of their reports'.
test("the picker leads with your own day, then the seats you lead", async () => {
  serving(
    {
      viewer: ada,
      work_my_work: emptyDay,
      work_items: noWork,
      work_workload: { rows: [], complete: true },
    },
    ledOrg,
  );
  mount();
  const rows = await pickerRows();
  // THE HEADING THROUGH THE ACCESSIBLE NAME, not through the package's own
  // class: a group is labelled by an element rather than by an attribute, and
  // a test reaching for the class would be asserting the drawing.
  const groupOf = (row: HTMLElement) => {
    const id = row.closest("[role=group]")?.getAttribute("aria-labelledby");
    return (id && document.getElementById(id)?.textContent) || "";
  };
  const labelled = rows.map((r) => [groupOf(r), r.textContent ?? ""] as const);
  expect(labelled.find(([, text]) => text.includes("Ada Okonkwo"))?.[0]).toBe("Yours");
  expect(labelled.find(([, text]) => text.includes("Rui Santos"))?.[0]).toBe("Your line");
  expect(labelled.find(([, text]) => text.includes("Bo Nakamura"))?.[0]).toBe("Anybody");
});

// SIX PEOPLE BEFORE IT SCROLLS. Every option is two lines where the kit sizes
// its panel for six one-line rows, so at 1440×900 the picker showed three and
// cut the "Anybody" heading in half. jsdom lays nothing out, so the cascade
// over the panel this picker opens is what is read.
test("the picker's panel is tall enough for six of its two-line rows", async () => {
  const read = (path: string) => readFileSync(join(process.cwd(), path), "utf8");
  const sheets = [
    read("node_modules/@crewlethq/ui/dist/styles.css"),
    read("src/styles/screens.css"),
  ].map((text) => {
    const style = document.createElement("style");
    style.textContent = text;
    document.head.append(style);
    return style;
  });
  try {
    serving(
      {
        viewer: ada,
        work_my_work: emptyDay,
        work_items: noWork,
        work_workload: { rows: [], complete: true },
      },
      ledOrg,
    );
    mount();
    const rows = await pickerRows();
    const panel = rows[0]!.closest<HTMLElement>(".whose-day-menu");
    expect(panel).not.toBeNull();
    expect(
      getComputedStyle(panel!).getPropertyValue("--crewlet-select-menu-max-height").trim(),
    ).toBe("24rem");
  } finally {
    for (const s of sheets) s.remove();
  }
});

// AND EACH ROW SAYS HOW MUCH IS ON THAT DESK, which is the one fact that makes
// the control worth opening and the one it did not carry.
test("a picker row says how much open work is on that desk", async () => {
  serving(
    {
      viewer: ada,
      work_my_work: emptyDay,
      work_items: noWork,
      work_workload: { rows: [{ handle: "rui", open: 4 }], complete: true },
    },
    ledOrg,
  );
  mount();
  const rows = await pickerRows();
  const text = (name: string) =>
    rows.find((r) => (r.textContent ?? "").includes(name))?.textContent;
  expect(text("Rui Santos")).toContain("4 open items");
  // A HANDLE THE ANSWER DID NOT NAME HOLDS NOTHING, because the read returns a
  // row only for somebody with open work — a real zero rather than a gap.
  expect(text("Bo Nakamura")).toContain("nothing open");
});

// ZERO AND UNKNOWN ARE DIFFERENT. An answer that stopped at its handle cap, or
// one that could not account for every change, has not said that a desk is
// empty — and a row claiming it had would be a lead reassigning work on a
// figure nobody measured.
test("an incomplete workload answer claims no desk is empty", async () => {
  serving(
    {
      viewer: ada,
      work_my_work: emptyDay,
      work_items: noWork,
      work_workload: { rows: [{ handle: "rui", open: 4 }], complete: true, truncated: true },
    },
    ledOrg,
  );
  mount();
  const rows = await pickerRows();
  for (const row of rows) {
    expect(row.textContent).not.toContain("nothing open");
    expect(row.textContent).not.toContain("open item");
  }
});

// A READER WHO LEADS NOBODY HAS NO LINE TO DRAW, and a "Your line" heading
// over nothing would be a group that holds no row.
test("a reader with no reports is offered no line, rather than an empty one", async () => {
  serving({
    viewer: ada,
    work_my_work: emptyDay,
    work_items: noWork,
    work_workload: { rows: [], complete: true },
  });
  mount();
  const rows = await pickerRows();
  const groups = rows.map((r) => {
    const id = r.closest("[role=group]")?.getAttribute("aria-labelledby");
    return (id && document.getElementById(id)?.textContent) || "";
  });
  expect(groups).not.toContain("Your line");
  expect(groups).toContain("Yours");
});

// AND THERE IS NO "PICK SOMEBODY" ROW FOR A READER WHO HAS A DAY. It wrote the
// parameter's own fallback, which the router deletes, so it resolved straight
// back to their own seat and the control re-labelled itself with their name —
// a row that silently refuses. Their way back is the "Yours" row.
test("a bound reader is offered no row that returns them where they are", async () => {
  serving(
    {
      viewer: ada,
      work_my_work: emptyDay,
      work_items: noWork,
      work_workload: { rows: [], complete: true },
    },
    ledOrg,
  );
  mount();
  const rows = await pickerRows();
  expect(rows.map((r) => r.textContent)).not.toContain("Pick somebody");
});

// AND A READER THE ENGINE WILL REFUSE IS OFFERED NO PICKER AT ALL. Naming
// anybody's handle needs a credential, so for an anonymous reader every row is
// a refusal — and the screen's own sentence named that pick as the remedy.
test("an anonymous reader gets the credential sentence, not a menu of refusals", async () => {
  const query = serving({
    viewer: { login: "", grants: [], handle: "", owner: "", name: "", kind: "" },
  });
  mount();
  await waitFor(() => expect(screen.getByText(/Nobody is signed in/)).toBeTruthy());
  expect(screen.queryByRole("combobox", { name: "Whose day" })).toBeNull();
  expect(screen.queryByText(/pick someone above/)).toBeNull();
  // AND THE LOAD STOPS BEING ASKED FOR. It is the picker's own question, and
  // once the viewer answers there is no control for it to fill — the FIRST
  // read is unavoidable, because until then this reader is indistinguishable
  // from one who gets a picker, and delaying it for everybody to spare a read
  // here would be the wrong trade.
  const asked = query.mock.calls.filter((c) => c[0] === "work_workload").length;
  expect(asked).toBe(1);
});

// THE ORDER IS THE CONTENT, so it is DRAWN. Every other panel on this screen is
// a set somebody has a claim on; Priorities is a sequence somebody decided,
// and as an ordinary run of rows it reads exactly like the Watching list
// beside it — the one thing the tab is about, invisible.
test("the priorities are numbered in the order they were stored", async () => {
  location.hash = "#/me?order=priorities";
  serving({
    viewer: ada,
    work_items: noWork,
    work_my_work: {
      ...emptyDay,
      priorities: [
        task({ key: "ENG-5", title: "first" }),
        task({ key: "ENG-9", title: "second" }),
        task({ key: "ENG-2", title: "third" }),
      ],
    },
  });
  const { container } = mount();
  await waitFor(() => expect(screen.getByText("first")).toBeTruthy());
  const places = [...container.querySelectorAll(".work-cell-ord")].map((el) => el.textContent);
  expect(places).toEqual(["1", "2", "3"]);
  // AND IN THE STORED ORDER, never re-sorted: the keys are not ascending, and
  // a list that sorted them would discard the decision it exists to show.
  const rows = [...container.querySelectorAll(".work-row")].map(
    (el) => el.querySelector(".work-key")?.textContent,
  );
  expect(rows).toEqual(["ENG-5", "ENG-9", "ENG-2"]);
});

// THE PRIORITIES SAY WHAT THEY COUNT, before their first row. The list is one
// somebody wrote and can name a colleague's task, so under a Queue tab that
// counts only the work assigned to the person it drew more rows than the tab
// said — "Queue 2" over five rows — and nothing on screen said why.
test("the priorities say how many they hold and how many are the person's own", async () => {
  location.hash = "#/me?order=priorities";
  serving({
    viewer: ada,
    work_items: { ...noWork, total_hint: 2 },
    work_my_work: {
      ...emptyDay,
      priorities: [
        task({ key: "ENG-1", title: "one", assignee: "rui" }),
        task({ key: "ENG-2", title: "two", assignee: "ada" }),
        task({ key: "ENG-3", title: "three", assignee: "rui" }),
        task({ key: "ENG-4", title: "four" }),
        task({ key: "ENG-5", title: "five", assignee: "ada" }),
      ],
      totals: { ...emptyDay.totals, priorities: { total: 5 } },
    },
  });
  mount();
  expect(
    await screen.findByText(
      "5 open tasks on your list, in the order to work them. 2 of them assigned to you. The Queue counts only the work assigned to you.",
    ),
  ).toBeTruthy();
});

// AND NO OTHER CLAIM IS NUMBERED. A place on a list that nobody arranged is a
// rank the engine never stored, read as one somebody did.
test("a claim that is a set rather than a sequence draws no places", async () => {
  location.hash = "#/me/watching";
  serving({
    viewer: ada,
    work_items: noWork,
    work_my_work: { ...emptyDay, watching_recent: [task({ key: "ENG-3" })] },
  });
  const { container } = mount("watching");
  await waitFor(() => expect(screen.getByText("Ship the thing")).toBeTruthy());
  expect(container.querySelector(".work-cell-ord")).toBeNull();
});

// ---------------------------------------------------------------------------
// Reordering the priorities
// ---------------------------------------------------------------------------

/** Three open entries drawn, in the stored order. */
const threeFirst = [
  task({ key: "ENG-5", title: "first" }),
  task({ key: "ENG-9", title: "second" }),
  task({ key: "ENG-2", title: "third" }),
];

/**
 * The person's own record: the drawn three with a finished entry between them
 * and an open one past the page, neither of which a row on screen shows.
 */
const storedOf = (handle: string, version = 7) => ({
  handle,
  priorities: ["ENG-5", "done-1", "ENG-9", "ENG-2", "late-21"],
  version,
  held: true,
  complete: true,
});

/** The row a task key is drawn in. */
function rowOf(key: string): HTMLElement {
  const row = [...document.querySelectorAll<HTMLElement>(".work-row")].find(
    (el) => el.querySelector(".work-key")?.textContent === key,
  );
  if (!row) throw new Error(`no row for ${key}`);
  return row;
}

/** The keys in the order they are drawn. */
function drawnKeys(): string[] {
  return [...document.querySelectorAll(".work-row .work-key")].map((el) => el.textContent ?? "");
}

// A REORDER IS THE WHOLE STORED LIST, CONDITIONAL ON THE RECORD IT WAS MADE
// FROM. The rows are the open entries up to a page; the list the write
// replaces is the person's own, so a row moved up one place moves up one place
// in THAT list — and the finished entry and the one past the page keep theirs.
// Written from the rows, the press would have dropped both.
test("a reorder sends the whole stored list, conditional on the person's version", async () => {
  location.hash = "#/me?order=priorities";
  serving({
    viewer: adaActs,
    work_items: noWork,
    work_my_work: { ...emptyDay, priorities: threeFirst },
    work_person: storedOf("ada", 7),
  });
  mount();
  await waitFor(() => expect(rowOf("ENG-2").getAttribute("draggable")).toBe("true"));
  fireEvent.keyDown(rowOf("ENG-2"), { key: "ArrowUp", altKey: true });
  await waitFor(() => expect(posted.length).toBe(1));
  expect(posted[0]).toEqual({
    tool: "set_priorities",
    args: {
      handle: "ada",
      items: ["ENG-5", "done-1", "ENG-2", "ENG-9", "late-21"],
      if_match: 7,
    },
  });
});

// A DROP IS THE SAME GESTURE AS THE KEYS — and a row dropped at the FOOT of
// the list lands after the last row DRAWN, not at the end of the stored list:
// entries past the page are not drawn, and a row sent behind them would vanish
// from the list the reader just put it in.
test("a row dropped at the foot lands after the last row drawn, not behind the page", async () => {
  location.hash = "#/me?order=priorities";
  serving({
    viewer: adaActs,
    work_items: noWork,
    work_my_work: { ...emptyDay, priorities: threeFirst },
    work_person: storedOf("ada"),
  });
  mount();
  await waitFor(() => expect(rowOf("ENG-5").getAttribute("draggable")).toBe("true"));
  const list = rowOf("ENG-5").parentElement!;
  const data = new Map<string, string>();
  const dataTransfer = {
    setData: (k: string, v: string) => data.set(k, v),
    getData: (k: string) => data.get(k) ?? "",
    effectAllowed: "",
    dropEffect: "",
  };
  // NO POINTER POSITION IS BELOW EVERY ROW'S MIDDLE, which is the foot.
  fireEvent.dragStart(rowOf("ENG-5"), { dataTransfer });
  fireEvent.dragOver(list, { dataTransfer });
  fireEvent.drop(list, { dataTransfer });
  await waitFor(() => expect(posted.length).toBe(1));
  expect(posted[0]!.args.items).toEqual(["done-1", "ENG-9", "ENG-2", "ENG-5", "late-21"]);
});

// WHILE THE WRITE IS OUT, NOTHING BUT THE MOVED ROW CHANGES. The next move is
// held back, but the list is still one the reader rearranges: every grip left
// the rows for the length of the write and the whole list stepped 18px left,
// then back when it landed.
test("a reorder in flight keeps every row's grip and the list's place track", async () => {
  location.hash = "#/me?order=priorities";
  let release: () => void = () => {};
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      const tool = decodeURIComponent(String(url).split("/operator/act/")[1] ?? "");
      posted.push({ tool, args: (JSON.parse(init.body as string) as { args: never }).args });
      await new Promise<void>((resolve) => (release = resolve));
      return new Response(JSON.stringify(reply.body), {
        status: 200,
        headers: { ...ENGINE_HEADERS, "Content-Type": "application/json" },
      });
    }),
  );
  serving({
    viewer: adaActs,
    work_items: noWork,
    work_my_work: { ...emptyDay, priorities: threeFirst },
    work_person: storedOf("ada"),
  });
  mount();
  await waitFor(() => expect(rowOf("ENG-2").getAttribute("draggable")).toBe("true"));
  fireEvent.keyDown(rowOf("ENG-2"), { key: "ArrowUp", altKey: true });
  await waitFor(() => expect(rowOf("ENG-2").getAttribute("data-pending")).toBe("true"));
  const list = rowOf("ENG-2").parentElement!;
  expect(list.getAttribute("data-reorder")).toBe("true");
  for (const key of ["ENG-5", "ENG-9", "ENG-2"]) {
    expect(rowOf(key).querySelector(".work-row-grip")).not.toBeNull();
    // HELD, NOT DROPPED: nothing lifts until the engine answers.
    expect(rowOf(key).getAttribute("draggable")).toBe("false");
  }
  await act(async () => release());
});

// CONFIRMED, NOT OPTIMISTIC — and a refusal PUTS THE ROW BACK. A reorder made
// from a screen that read an older order is refused by the engine, and the
// screen must then draw the order that IS stored, with the engine's sentence,
// rather than the one the reader asked for.
test("a reorder refused as stale puts the rows back and says why", async () => {
  location.hash = "#/me?order=priorities";
  reply = { status: 409, body: { error: "stale_version", tool: "set_priorities", detail: "" } };
  serving({
    viewer: adaActs,
    work_items: noWork,
    work_my_work: { ...emptyDay, priorities: threeFirst },
    work_person: storedOf("ada"),
  });
  mount();
  await waitFor(() => expect(rowOf("ENG-2").getAttribute("draggable")).toBe("true"));
  fireEvent.keyDown(rowOf("ENG-2"), { key: "ArrowUp", altKey: true });
  await waitFor(() => expect(screen.getByText(ACT_ERRORS.stale_version)).toBeTruthy());
  expect(drawnKeys()).toEqual(["ENG-5", "ENG-9", "ENG-2"]);
});

// ON SOMEBODY ELSE'S DAY EVERY CHANGE IS HELD — BUT A LEAD'S REORDER. Setting
// what somebody in your line does next is the one authority the tracker grants
// across people, and the one change this screen makes on another person's day;
// it is released where it is drawn and nowhere else, so the questions on the
// same day stay theirs to answer.
describe("somebody else's day", () => {
  const day = (handle: string) => ({
    viewer: adaActs,
    work_items: noWork,
    work_my_work: { ...emptyDay, handle, priorities: threeFirst, asked_of_me: [askRow()] },
    work_person: storedOf(handle, 3),
  });

  test("a lead reorders a report's queue, as the report's", async () => {
    location.hash = "#/me?order=priorities&handle=rui";
    serving(day("rui"), ledOrg);
    mount();
    await waitFor(() => expect(rowOf("ENG-5").getAttribute("draggable")).toBe("true"));
    // AND WHAT IT DOES IS SAID BEFORE IT IS DONE: the report is told, so it is
    // an instruction to them rather than a private arrangement.
    expect(screen.getByText("Rui Santos is told when you reorder.")).toBeTruthy();
    fireEvent.keyDown(rowOf("ENG-5"), { key: "ArrowDown", altKey: true });
    await waitFor(() => expect(posted.length).toBe(1));
    expect(posted[0]!.args).toEqual({
      handle: "rui",
      items: ["done-1", "ENG-9", "ENG-5", "ENG-2", "late-21"],
      if_match: 3,
    });
  });

  test("and the same lead cannot answer what is asked of the report", async () => {
    location.hash = "#/me/asked-of-me?handle=rui";
    serving(day("rui"), ledOrg);
    mount("asked-of-me");
    const accept = await screen.findByRole("button", { name: "Accept" });
    expect(accept.getAttribute("aria-disabled")).toBe("true");
  });

  test("a reader who does not lead them cannot reorder it, and is told who can", async () => {
    location.hash = "#/me?order=priorities&handle=bo";
    serving(day("bo"), ledOrg);
    mount();
    await waitFor(() =>
      expect(
        screen.getByText(/Reordering is off\. Only Bo Nakamura, or somebody they report to/),
      ).toBeTruthy(),
    );
    expect(rowOf("ENG-5").getAttribute("draggable")).toBeNull();
    fireEvent.keyDown(rowOf("ENG-5"), { key: "ArrowDown", altKey: true });
    await settled(() => posted.length);
    expect(posted).toEqual([]);
  });
});

// A REORDER IS NEVER HIDDEN, and a drag has no button to disable — so a reader
// the engine will not make it for is told why ONCE, above the rows, in the
// sentence every other control uses; the rows do not lift.
test.each([
  [
    "an anonymous reader",
    { login: "", grants: [], handle: "", owner: "", name: "", kind: "" },
    WRITE_REASONS.anonymous,
  ],
  ["a person the engine does not reorder for", { ...ada, acts: [] }, WRITE_REASONS.not_served],
])("%s sees the rows and the reason they do not move", async (_who, viewer, reason) => {
  location.hash = "#/me?order=priorities&handle=ada";
  serving({
    viewer,
    work_items: noWork,
    work_my_work: { ...emptyDay, priorities: threeFirst },
    work_person: storedOf("ada"),
  });
  mount();
  await waitFor(() => expect(screen.getByText(`Reordering is off. ${reason}`)).toBeTruthy());
  expect(rowOf("ENG-5").getAttribute("draggable")).toBeNull();
});

// A PAGE OF PRIORITIES SAYS IT IS ONE, and how much of the list it holds.
test("the entries past the page are named, not dropped", async () => {
  location.hash = "#/me?order=priorities";
  serving({
    viewer: adaActs,
    work_items: noWork,
    work_my_work: {
      ...emptyDay,
      priorities: threeFirst,
      totals: { ...emptyDay.totals, priorities: { total: 5 } },
    },
    work_person: storedOf("ada"),
  });
  mount();
  await waitFor(() => expect(screen.getByText("Showing 3 of 5.")).toBeTruthy());
});

// THE INBOX IS NOT ONE OF THE CLAIMS. What REACHED somebody is a different
// question from what is ON them, and it has a workspace of its own:
// the card that drew it here was the inbox in a narrower column with a smaller
// bound, so a reader met the same notices twice and neither copy was the one
// with the reason facets.
test("what reached somebody is the Inbox, and is not drawn here", async () => {
  const query = serving({ viewer: ada, work_my_work: emptyDay, work_items: noWork });
  mount();
  await counts();
  expect(screen.queryByText("Reached you")).toBeNull();
  expect(query.mock.calls.map((c) => c[0])).not.toContain("inbox");
  // And the way to it is a link rather than a copy.
  expect(screen.getByText("Inbox →")).toBeTruthy();
});
