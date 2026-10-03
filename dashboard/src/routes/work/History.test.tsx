/**
 * The tracker's own log, and the claims a log screen must not overstate.
 *
 * A twenty-row "Recent activity" card sat under every board and every list. It
 * had a time axis nobody could set, no filters at all, and a page size that WAS
 * its count — so on any company busier than twenty rows the number a reader saw
 * was a constant, and the row said `project updated` where the fact somebody
 * came for is which field moved from what to what.
 *
 * The cases here are about the four halves that are easy to overstate: what the
 * bars cover (the pages this client holds, not a window the engine counted),
 * what a facet counts (the same), whether the rows on screen are all there are
 * (they are not, and the page fetches the rest rather than telling a reader to
 * narrow the window at them), and who "you" is in a sentence the engine wrote
 * for somebody else.
 */

import { cleanup, fireEvent, render, screen, waitFor } from "~/test/inCase.ts";
import { afterEach, expect, test, vi } from "vitest";

import { History, HistoryView } from "./History.tsx";
import { pick } from "~/testing.tsx";
import { Router } from "~/app/router.tsx";
import { usePageCoverage } from "~/app/Shell.tsx";
import { useClient, useConnection, useOrg } from "~/lib/store-hooks.ts";
import type { QueryName, WorkActivityRecord } from "~/protocol/index.ts";
import {
  CLAIMANT,
  CLAIMANT_HREF,
  DUPLICATE,
  DUPLICATE_HREF,
  SHARED_KEY,
} from "~/test/keyCollision.ts";
import { ViewerProvider } from "~/lib/viewer.ts";

vi.mock("~/lib/store-hooks.ts", async () => {
  const actual =
    await vi.importActual<typeof import("~/lib/store-hooks.ts")>("~/lib/store-hooks.ts");
  return { ...actual, useClient: vi.fn(), useConnection: vi.fn(), useOrg: vi.fn() };
});

// THE FRAME'S ONE COVERAGE SLOT, as a spy. Mocked WITHOUT `importActual`,
// because the point of these two cases is which component CALLS this hook and
// pulling the real Shell in would drag the whole route table behind a question
// about one function.
vi.mock("~/app/Shell.tsx", () => ({ usePageCoverage: vi.fn() }));

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.mocked(usePageCoverage).mockClear();
  location.hash = "#/";
});

function serving(answers: Partial<Record<QueryName, unknown>>) {
  return servingWith((what) => answers[what as QueryName] ?? {});
}

/** Answers that depend on what was ASKED — a cursored page, say. */
function servingWith(answer: (what: string, params?: Record<string, unknown>) => unknown) {
  const query = vi.fn(async (what: string, params?: Record<string, unknown>) =>
    answer(what, params),
  );
  vi.mocked(useClient).mockReturnValue({ socket: { query } } as never);
  vi.mocked(useConnection).mockReturnValue({ connected: true } as never);
  vi.mocked(useOrg).mockReturnValue({
    name: "Acme",
    roles: [{ name: "Ada Okonkwo", handle: "ada", kind: "agent" }],
  } as never);
  return query;
}

/** One change, with everything a row draws. */
function record(over: Partial<WorkActivityRecord> = {}): WorkActivityRecord {
  return {
    id: over.id ?? "r1",
    log_seq: 1,
    log_stream: "CREWLET_WORK_LOG",
    log_generation: 1,
    at: new Date(Date.now() - 60 * 60_000).toISOString(),
    effective_at: new Date(Date.now() - 60 * 60_000).toISOString(),
    kind: "status",
    actor: "ada",
    actor_kind: "agent",
    subject_kind: "task",
    subject_id: "t1",
    subject_key: "ENG-1",
    notified: true,
    ...over,
  };
}

const mount = () =>
  render(
    <ViewerProvider>
      <Router>
        <History />
      </Router>
    </ViewerProvider>,
  );

/** What `work_activity` was actually asked. */
function asked(query: ReturnType<typeof serving>): Record<string, unknown> {
  const calls = query.mock.calls as unknown as [string, Record<string, unknown>?][];
  return calls.findLast(([what]) => what === "work_activity")?.[1] ?? {};
}

/** The bar's pickers, by the dimension each one narrows. */
const picker = (name: string) => screen.getByRole("combobox", { name });

// THE WINDOW IS A WALL-CLOCK RANGE, which is what a reader choosing one means:
// the engine's own `from`/`to` over the AUTHORED instants, never a log
// position, which is a different order and not a time at all.
test("the window reaches the engine as the instants it names", async () => {
  const query = serving({ work_activity: { records: [], complete: true } });
  mount();
  await waitFor(() => expect(typeof asked(query).from).toBe("string"));
  expect(typeof asked(query).to).toBe("string");
  expect(asked(query).container).toBe("workspace");
  // A WEEK IS THE DEFAULT, because this is read after the fact rather than
  // watched: a tracker's pace is a person typing a comment. And a QUARTER OF
  // AN HOUR is not offered at all — of a tracker it is almost always empty,
  // and an empty axis reads as a log that stopped.
  expect(screen.getByText("7d")).toBeTruthy();
  expect(screen.queryByText("15m")).toBeNull();
});

// A ROW IS THE DELTA, not the kind. `project updated` over a zero is what the
// card drew; which field moved from what to what is the fact a reader came
// for, and it is worded by the same helper the item's own history uses.
test("a row says which field moved and where it went", async () => {
  serving({
    work_activity: {
      records: [record({ fields: { status: { from: "todo", to: "in_progress" } } })],
      complete: true,
    },
  });
  const { container } = mount();
  await waitFor(() => expect(screen.getByText("ENG-1")).toBeTruthy());
  const what = container.querySelector(".work-log-what");
  expect(what?.textContent).toContain("To do");
  expect(what?.textContent).toContain("In progress");
  // AND IT WRAPS RATHER THAN ELLIPSING. `.truncate` is `white-space: nowrap`,
  // so carrying it here cancelled the wrapping `.work-log-what` is written for
  // and a multi-field commit lost everything after its first clause. jsdom
  // computes no layout, so the class list is what can be asserted — which is
  // also the only thing that was wrong.
  expect(what?.className).not.toContain("truncate");
});

// AN OPERATOR IS NOT A SEAT. A write made with an API token carries the
// token's own name and the author kind `operator`, which is the whole point of
// the audit trail — and an agent's is the ordinary case, so only the others
// are marked.
test("a write an operator made is marked as one", async () => {
  serving({
    work_activity: {
      records: [
        record({ id: "r1", actor: "founder", actor_kind: "operator" }),
        record({ id: "r2", actor: "ada", actor_kind: "agent" }),
        // A PERSON THE DIRECTORY BINDS TO A SEAT writes AS the seat, so the
        // row names them as every other screen does, and keeps the
        // credential the write came through on the title.
        record({ id: "r3", actor: "ada", actor_kind: "human", operator_id: "pat:0193a8" }),
      ],
      complete: true,
    },
  });
  const { container } = mount();
  await waitFor(() => expect(container.querySelectorAll(".work-log-row").length).toBe(3));
  // READ OFF THE ROWS, because the facet rail lists the same authors one band
  // up: a bare text query would match either and prove neither.
  const rows = [...container.querySelectorAll(".work-log-who")];
  const who = rows.map((el) => el.textContent ?? "");
  expect(who[0]).toContain("founder");
  expect(who[0]).toContain("operator");
  // AN AGENT'S WRITE IS THE ORDINARY CASE, so only the others are marked.
  expect(who[1]).not.toContain("agent");
  expect(who[2]).toContain("Ada Okonkwo");
  expect(who[2]).toContain("human");
  expect(rows[2]!.querySelector("[title]")?.getAttribute("title")).toBe(
    "Written through pat:0193a8",
  );
});

// A HANDLE IS THE DATABASE'S WORD FOR A PERSON, resolved through the chart
// like everywhere else in this product.
test("an author is named the way the company names them", async () => {
  serving({ work_activity: { records: [record()], complete: true } });
  const { container } = mount();
  await waitFor(() => expect(container.querySelector(".work-log-who")).toBeTruthy());
  expect(container.querySelector(".work-log-who")?.textContent).toContain("Ada Okonkwo");
});

// A COMMIT CAN NAME NOBODY, and the wire says so by leaving the actor out.
test("a change the engine made names the engine", async () => {
  serving({ work_activity: { records: [record({ actor: "" })], complete: true } });
  mount();
  await waitFor(() => expect(screen.getByText("the engine")).toBeTruthy());
});

// THE PICKERS COUNT THE ROWS LOADED, and the bar says so ONCE: the engine
// answers this question with rows rather than with counts, so an option
// claiming a total would be a number this screen invented about somebody's
// company — and three rails each ending in the same caption said it three
// times.
test("a picker's counts say what they are a count of, once", async () => {
  serving({
    work_activity: {
      records: [
        record({ id: "r1", project: "ENG" }),
        record({ id: "r2", project: "ENG" }),
        record({ id: "r3", kind: "comment", project: "PROD" }),
      ],
      complete: true,
    },
  });
  mount();
  // ONE PICKER PER DIMENSION — the kinds, the authors and the projects — in
  // the one bar, and one sentence for all three.
  await waitFor(() =>
    expect(screen.getAllByText("Counts are over the changes loaded")).toHaveLength(1),
  );
  expect(picker("Kind")).toBeTruthy();
  expect(picker("By")).toBeTruthy();
  expect(picker("Project")).toBeTruthy();
  // AND THE SET IS WHAT THE PAGE HOLDS rather than a closed set of thirty-two
  // kinds, most of them zero, each with how many of the loaded rows carry it.
  fireEvent.click(picker("Kind"));
  expect(screen.getByRole("option", { name: /^status.*2 changes/ })).toBeTruthy();
  expect(screen.getByRole("option", { name: /^comment.*1 change/ })).toBeTruthy();
});

// PICKING A FACET ASKS THE ENGINE AGAIN rather than hiding rows this client
// holds: the answer is a page, so a client-side narrowing would be a filter
// over whatever happened to be loaded.
test("choosing a kind narrows the question rather than the page", async () => {
  location.hash = "#/work/history?kind=comment";
  const query = serving({ work_activity: { records: [], complete: true } });
  mount();
  await waitFor(() => expect(asked(query).kinds).toBe("comment"));
});

// THE PROJECT IS A DIMENSION THE RECORD CARRIES and the engine already filters
// on, and until the rail existed the only way to reach it was a link from a
// project's own page — the page read the key, drew its chip and had no control
// that set it.
test("the project rail narrows the log to one project", async () => {
  const query = serving({
    work_activity: {
      records: [record({ id: "r1", project: "ENG" }), record({ id: "r2", project: "PROD" })],
      complete: true,
    },
  });
  mount();
  await waitFor(() => expect(screen.getAllByText("ENG-1")).toHaveLength(2));
  pick(picker("Project"), /^ENG/);
  // THE CONTAINER, which is how `work_activity` takes a project: the engine
  // maps `project:KEY` onto its own `h.project_key` predicate.
  await waitFor(() => expect(asked(query).container).toBe("project:ENG"));
  expect(picker("Project").textContent).toContain("ENG");
});

// A PROJECT NARROWING IS VISIBLE, because the page is reached from a project's
// own feed and a reader who cannot see the narrowing reads one project's
// changes as the company's — so the picker shows it even when no loaded row
// carries it, and choosing "Every project" takes it off.
test("a project narrowing arriving in the address is visible and removable", async () => {
  location.hash = "#/work/history?project=ENG";
  const query = serving({ work_activity: { records: [], complete: true } });
  mount();
  await waitFor(() => expect(asked(query).container).toBe("project:ENG"));
  expect(picker("Project").textContent).toContain("ENG");
  pick(picker("Project"), "Every project");
  await waitFor(() => expect(asked(query).container).toBe("workspace"));
});

// THE PAGE BOUNDARY IS CROSSED, not announced. The screen used to print that
// older changes existed and tell the reader to narrow the window — which moves
// the window's newest edge and reaches FEWER, NEWER rows, the opposite of what
// they were reaching for.
test("older changes are fetched, and the ones already loaded stay", async () => {
  const query = servingWith((what, params) => {
    if (what !== "work_activity") return {};
    return params?.cursor
      ? { records: [record({ id: "r2", subject_key: "ENG-2", kind: "comment" })], complete: true }
      : { records: [record({ id: "r1" })], next_cursor: "c1", complete: true };
  });
  mount();
  await waitFor(() => expect(screen.getByText("ENG-1")).toBeTruthy());
  fireEvent.click(screen.getByRole("button", { name: "Load older changes" }));
  await waitFor(() => expect(screen.getByText("ENG-2")).toBeTruthy());
  // THE CURSOR THE ENGINE MINTED, sent back verbatim: it is a log POSITION
  // (`stream@generation:seq`), not a row count this client could compute.
  expect(asked(query).cursor).toBe("c1");
  // AND THE FIRST PAGE IS STILL THERE. A "load older" that replaced the list
  // would be a pager, which is not what a log is read with.
  expect(screen.getByText("ENG-1")).toBeTruthy();
  // THE EXHAUSTED END SAYS SO, rather than leaving a button that answers
  // nothing: the second page came back with no cursor.
  await waitFor(() =>
    expect(screen.queryByRole("button", { name: "Load older changes" })).toBeNull(),
  );
  expect(screen.getByText(/oldest change in this window/)).toBeTruthy();
});

// AND A NEW QUESTION THROWS THEM AWAY. Pages fetched under the previous window
// are rows the server would never have answered for this one, and the cursor
// walks the old query's history — the event log states both at length.
test("changing the window discards the pages fetched under the old one", async () => {
  const query = servingWith((what, params) => {
    if (what !== "work_activity") return {};
    return params?.cursor
      ? { records: [record({ id: "r2", subject_key: "ENG-2", kind: "comment" })], complete: true }
      : { records: [record({ id: "r1" })], next_cursor: "c1", complete: true };
  });
  mount();
  await waitFor(() => expect(screen.getByText("ENG-1")).toBeTruthy());
  fireEvent.click(screen.getByRole("button", { name: "Load older changes" }));
  await waitFor(() => expect(screen.getByText("ENG-2")).toBeTruthy());

  fireEvent.click(screen.getByText("30d"));
  await waitFor(() => expect(screen.queryByText("ENG-2")).toBeNull());
  // THE NEXT ASK CARRIES NO CURSOR: the first page of the new window is the
  // first page, and resuming the old one there would page into a different
  // question's history.
  expect(asked(query).cursor).toBeUndefined();
});

// AND AN EMPTY WINDOW SAYS WHAT WOULD BE IN IT. "No rows" over a log is six
// different facts, and the remedy for this one is the window or the filters.
test("a window with nothing in it says so and offers the way out", async () => {
  serving({ work_activity: { records: [], complete: true } });
  mount();
  await waitFor(() => expect(screen.getByText("Nothing changed in this window")).toBeTruthy());
  expect(screen.getByText(/Widen the window/)).toBeTruthy();
});

// NOTHING HAPPENED AND NOTHING COULD BE READ ARE OPPOSITE FACTS. A node
// holding records this build cannot decode answers an empty window too, and
// "Nothing changed" over that told a reader their company had been quiet when
// the changes were there and unread.
test("an empty window this node could not read is not a quiet one", async () => {
  serving({
    work_activity: {
      records: [],
      complete: false,
      incomplete: {
        records: 3,
        version: 14,
        from: { seq: 41, generation: 1, stream: "CREWLET_WORK_LOG" },
        scope: ["project:ENG"],
      },
    },
  });
  mount();
  await waitFor(() => expect(screen.getByText("No change here could be read")).toBeTruthy());
  expect(screen.queryByText("Nothing changed in this window")).toBeNull();
  expect(screen.getByText(/3 record\(s\) this build cannot read/)).toBeTruthy();
  expect(screen.getByText(/not a refresh/)).toBeTruthy();
});

// AND A NODE BEHIND ITS LOG says how far it applied, in the position words
// every coverage surface uses, rather than that nothing happened.
test("an empty window on a node behind its log says how far it has applied", async () => {
  serving({
    work_activity: { records: [], complete: true, applied_through: 41, log_seq: 88 },
  });
  mount();
  await waitFor(() =>
    expect(screen.getByText("Nothing in this window has been applied here")).toBeTruthy(),
  );
  expect(screen.getByText(/applied through 41 of 88/)).toBeTruthy();
});

/** An answer from a node that is behind, which is the only state coverage draws in. */
const behind = (records: WorkActivityRecord[]) => ({
  records,
  complete: true,
  log_seq: 1_240,
  applied_through: 1_204,
});

// ONE FACT, ONE PLACE. The screen hands its answer to the frame and draws none
// of it; drawn in both, "applied through 1,204 of 1,240" appeared twice on one
// screen — which is the duplication the shared component was extracted to end.
test("the screen publishes its coverage and draws none of it", async () => {
  serving({ work_activity: behind([record()]) });
  const { container } = mount();
  await waitFor(() => expect(screen.getByText("ENG-1")).toBeTruthy());
  expect(vi.mocked(usePageCoverage)).toHaveBeenCalled();
  expect(container.querySelector(".work-coverage")).toBeNull();
});

// AND THE LENS IS THE EXACT REVERSE. Inside a project's page the frame's one
// coverage slot belongs to the project's own read, so this publishes nothing —
// and states its rows' coverage where the rows are instead.
test("the lens draws its own coverage and publishes none", async () => {
  // THE ROWS CARRY A PROJECT, so the missing rail below is the absence of a
  // SETTER and not the absence of anything to count — without it the assertion
  // would pass on a screen that had simply loaded no project.
  serving({ work_activity: behind([record({ project: "ENG" })]) });
  const { container } = render(
    <ViewerProvider>
      <Router>
        <HistoryView container="project:ENG" embedded />
      </Router>
    </ViewerProvider>,
  );
  await waitFor(() => expect(screen.getByText("ENG-1")).toBeTruthy());
  expect(vi.mocked(usePageCoverage)).not.toHaveBeenCalled();
  expect(container.querySelector(".work-coverage")).toBeTruthy();
  // AND NO PROJECT PICKER: every row under a project's header carries the
  // same key, so a picker there is one option that narrows nothing.
  expect(screen.queryByRole("combobox", { name: "Project" })).toBeNull();
  expect(picker("Kind")).toBeTruthy();
});

// THE ANSWER'S OWN RESOLUTION REACHES THE SENTENCE. The delta carries the other
// task's id; `keys` is what the answering node resolved it to, and the wiring
// between the two is the part that can silently not happen.
test("a relation delta names the other task by the key the answer resolved", async () => {
  serving({
    work_activity: {
      records: [record({ kind: "relations", fields: { waiting_on: { from: "", to: "t-2" } } })],
      keys: { "t-2": "ENG-2" },
      complete: true,
    },
  });
  const { container } = mount();
  await waitFor(() => expect(container.querySelector(".work-log-what")).toBeTruthy());
  const what = container.querySelector(".work-log-what")?.textContent ?? "";
  expect(what).toContain("ENG-2");
  expect(what).not.toContain("t-2");
});

/** The one record the engine writes in the second person. */
const prioritised = () =>
  record({
    kind: "prioritised",
    actor: "founder",
    actor_kind: "operator",
    subject_kind: "person",
    subject_id: "agent-swe",
    subject_key: undefined,
    excerpt: "founder put ENG-1 at position 1 of your priorities",
  });

// A NOTIFICATION IS ADDRESSED; A LOG IS NOT. `tracker.prioritisedWake` writes
// the card the woken seat reads, so its "your priorities" is correct for that
// seat and second person to every other reader of the company-wide log.
test("a queue somebody else's is named as theirs", async () => {
  serving({
    work_activity: { records: [prioritised()], complete: true },
    viewer: {
      login: "ada.okonkwo",
      grants: ["state:read"],
      handle: "ada",
      owner: "ada",
      name: "Ada Okonkwo",
      kind: "human",
    },
  });
  const { container } = mount();
  await waitFor(() => expect(container.querySelector(".work-log-what")).toBeTruthy());
  const what = container.querySelector(".work-log-what")?.textContent ?? "";
  expect(what).toContain("agent-swe's priorities");
  expect(what).not.toContain("your");
});

// AND SECOND PERSON SURVIVES FOR THE ONE READER IT IS TRUE OF.
test("a reader looking at their own queue is still addressed as themselves", async () => {
  serving({
    work_activity: { records: [prioritised()], complete: true },
    viewer: {
      login: "sam.swe",
      grants: ["state:read"],
      handle: "agent-swe",
      owner: "agent-swe",
      name: "SWE",
      kind: "human",
    },
  });
  const { container } = mount();
  await waitFor(() =>
    expect(container.querySelector(".work-log-what")?.textContent).toContain("your priorities"),
  );
});

// THE BAR SAYS HOW MUCH IS SHOWN IN PLAIN WORDS, with the way to more beside it:
// "100 changes loaded, and older ones beyond them" described a paging state
// where a reader wanted a number and a button.
test("the bar says how many are shown and offers the older ones", async () => {
  const query = servingWith((what, params) => {
    if (what !== "work_activity") return {};
    return params?.cursor
      ? { records: [record({ id: "r2", subject_key: "ENG-2" })], complete: true }
      : { records: [record({ id: "r1" })], next_cursor: "c1", complete: true };
  });
  mount();
  await waitFor(() => expect(screen.getByText(/Showing the latest 1/)).toBeTruthy());
  fireEvent.click(screen.getByRole("button", { name: "Load older" }));
  await waitFor(() => expect(asked(query).cursor).toBe("c1"));
  await waitFor(() => expect(screen.getByText(/Showing the latest 2/)).toBeTruthy());
});

// AND AN UNBOUND READER'S QUEUE IS KEPT UNDER THEIR LOGIN, which is the name a
// person subject carries for them — compared against the seat handle alone,
// a reader the directory binds to no seat was told about their own queue in
// the third person.
test("an unbound reader's own queue, kept under their login, is addressed to them", async () => {
  serving({
    work_activity: {
      records: [{ ...prioritised(), subject_id: "jane.doe" }],
      complete: true,
    },
    viewer: {
      login: "jane.doe",
      grants: ["state:read"],
      handle: "",
      owner: "jane.doe",
      name: "",
      kind: "",
    },
  });
  const { container } = mount();
  await waitFor(() =>
    expect(container.querySelector(".work-log-what")?.textContent).toContain("your priorities"),
  );
});

// A DUPLICATE'S HISTORY LEADS TO THE DUPLICATE.
//
// A change is drawn under its task's key, and a key two tasks hold opens the
// one that claimed it first — so every row of the duplicate's history linked
// to its claimant, and the log is the one place a reader goes to find out what
// happened to it. The engine says which key opens another task beside the key,
// and the flagged one's row goes by the task's id.
test("two tasks under one key each link their own history rows", async () => {
  serving({
    work_activity: {
      records: [
        record({
          id: "r2",
          subject_id: DUPLICATE,
          subject_key: SHARED_KEY,
          subject_key_collision: true,
        }),
        record({ id: "r1", subject_id: CLAIMANT, subject_key: SHARED_KEY }),
      ],
      complete: true,
    },
  });
  const { container } = mount();
  await waitFor(() => expect(container.querySelectorAll(".work-log-key a").length).toBe(2));
  const links = [...container.querySelectorAll(".work-log-key a")].map((a) => [
    a.textContent,
    a.getAttribute("href"),
  ]);
  expect(links).toEqual([
    [SHARED_KEY, DUPLICATE_HREF],
    [SHARED_KEY, CLAIMANT_HREF],
  ]);
});
