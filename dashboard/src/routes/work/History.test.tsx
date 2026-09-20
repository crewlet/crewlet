/**
 * The tracker's own log, and the claims a log screen must not overstate.
 *
 * A twenty-row "Recent activity" card sat under every board and every list. It
 * had a time axis nobody could set, no filters at all, and a page size that WAS
 * its count — so on any company busier than twenty rows the number a reader saw
 * was a constant, and the row said `project updated` where the fact somebody
 * came for is which field moved from what to what.
 *
 * The cases here are about the two halves that are easy to overstate: what the
 * bars cover (a page this client holds, not a window the engine counted) and
 * what a facet counts (the rows loaded, for the same reason).
 */

import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { History } from "./History.tsx";
import { Router } from "~/app/router.tsx";
import { useClient, useConnection, useOrg } from "~/lib/store-hooks.ts";
import type { QueryName, WorkActivityRecord } from "~/protocol/index.ts";

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
    <Router>
      <History />
    </Router>,
  );

/** What `work_activity` was actually asked. */
function asked(query: ReturnType<typeof serving>): Record<string, unknown> {
  const calls = query.mock.calls as unknown as [string, Record<string, unknown>?][];
  return calls.findLast(([what]) => what === "work_activity")?.[1] ?? {};
}

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
// for, and it is rendered by the same helper the item's own history uses.
test("a row says which field moved and where it went", async () => {
  serving({
    work_activity: {
      records: [record({ fields: { status: { from: "todo", to: "in_progress" } } })],
      complete: true,
    },
  });
  const { container } = mount();
  await waitFor(() => expect(screen.getByText("ENG-1")).toBeTruthy());
  const what = container.querySelector(".work-log-what")?.textContent ?? "";
  expect(what).toContain("To do");
  expect(what).toContain("In progress");
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
      ],
      complete: true,
    },
  });
  const { container } = mount();
  await waitFor(() => expect(container.querySelectorAll(".work-log-row").length).toBe(2));
  // READ OFF THE ROWS, because the facet rail lists the same authors one band
  // up: a bare text query would match either and prove neither.
  const who = [...container.querySelectorAll(".work-log-who")].map((el) => el.textContent ?? "");
  expect(who[0]).toContain("founder");
  expect(who[0]).toContain("operator");
  // AN AGENT'S WRITE IS THE ORDINARY CASE, so only the others are marked.
  expect(who[1]).not.toContain("agent");
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

// THE FACETS COUNT THE ROWS LOADED, and the rail says so: the engine answers
// this question with rows rather than with counts, so a chip claiming a total
// would be a number this screen invented about somebody's company.
test("a facet says what its count is a count of", async () => {
  serving({
    work_activity: {
      records: [record({ id: "r1" }), record({ id: "r2" }), record({ id: "r3", kind: "comment" })],
      complete: true,
    },
  });
  mount();
  // ONE RAIL PER DIMENSION — the kinds and the authors — and each says what it
  // counted.
  await waitFor(() => expect(screen.getAllByText(/counts over the rows loaded/).length).toBe(2));
  // AND THE SET IS WHAT THE PAGE HOLDS rather than a closed set of thirty-two
  // kinds, most of them zero: a rail of thirty-two chips is not a filter.
  // Read off the rail itself, because a row's own delta names its field with
  // the same word.
  const rail = screen.getAllByText(/counts over the rows loaded/)[0]?.closest(".facet-row");
  expect(within(rail as HTMLElement).getByText("status")).toBeTruthy();
  expect(within(rail as HTMLElement).getByText("comment")).toBeTruthy();
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

// THE PAGE BOUNDARY IS SAID, and it is a different fact from an empty window:
// a reader who cannot see the boundary reads one page as the whole history.
test("a capped page says older changes exist", async () => {
  serving({
    work_activity: { records: [record()], next_cursor: "c1", complete: true },
  });
  mount();
  await waitFor(() =>
    expect(screen.getByText(/Older changes exist beyond this page/)).toBeTruthy(),
  );
});

// AND AN EMPTY WINDOW SAYS WHAT WOULD BE IN IT. "No rows" over a log is six
// different facts, and the remedy for this one is the window or the filters.
test("a window with nothing in it says so and offers the way out", async () => {
  serving({ work_activity: { records: [], complete: true } });
  mount();
  await waitFor(() => expect(screen.getByText("Nothing changed in this window")).toBeTruthy());
  expect(screen.getByText(/Widen the window/)).toBeTruthy();
});

// A PROJECT NARROWING IS A CHIP rather than a hidden parameter: the page is
// reached from a project's own feed, and a reader who cannot see the narrowing
// reads one project's changes as the company's.
test("a project narrowing is visible and removable", async () => {
  location.hash = "#/work/history?project=ENG";
  const query = serving({ work_activity: { records: [], complete: true } });
  mount();
  await waitFor(() => expect(asked(query).container).toBe("project:ENG"));
  expect(screen.getByLabelText("Show every project")).toBeTruthy();
});
