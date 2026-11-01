/**
 * A dashboard as a reader sees it, over a fake engine that answers the board
 * and each dataset as the datasets route would.
 *
 * THE LIVE EXIT TEST: an agent writes a row into the board's store collection,
 * the room pushes the change, and the store dataset and the calculation that
 * stands on it are read again (once each, and nothing else is), so the stat
 * drawn from the calculation shows the new total without the reader doing
 * anything.
 */

import type { ReactNode } from "react";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { ToastProvider } from "@crewlethq/ui";

import { DashboardView } from "./DashboardView.tsx";
import { ArtifactDataStore } from "~/lib/artifacts/datastore.ts";
import { ROWS_UNREAD } from "~/lib/artifacts/dashboard.ts";
import type {
  ArtifactDetail,
  DashboardBoardAnswer,
  DashboardDatasetAnswer,
  LiveSocket,
} from "~/protocol/index.ts";
import { settled } from "~/test/settle.ts";

// THE BOARD'S BRAND REGION reads the company's brand over the socket, which
// these tests, mounted with no client, do not serve: the board is drawn bare.
vi.mock("~/components/brand.tsx", () => ({
  AdoptedBrandScope: ({ children }: { children: ReactNode }) => children,
}));
vi.mock("~/lib/store-hooks.ts", async () => {
  const actual =
    await vi.importActual<typeof import("~/lib/store-hooks.ts")>("~/lib/store-hooks.ts");
  return { ...actual, useAgents: () => [], useTokens: () => null };
});

const ID = "0192f2c4-2222-7000-8000-000000000002";

const detail = {
  id: ID,
  kind: "dashboard",
  title: "Delivery",
  head_number: 1,
  role: "viewer",
} as unknown as ArtifactDetail;

const board: DashboardBoardAnswer = {
  artifact: ID,
  version: 1,
  board: {
    v: 1,
    title: "Delivery",
    datasets: [
      { id: "deals", title: "Pipeline", source: "store", store: { collection: "deals" } },
      { id: "total", title: "Pipeline total", source: "calc", calc: { from: "deals", steps: [] } },
      { id: "work", title: "My open work", source: "engine", engine: { name: "tracker.items" } },
      { id: "spend", title: "Spend by seat", source: "engine", engine: { name: "spend.seats" } },
    ],
    widgets: [
      { id: "pipeline", type: "stat", title: "Pipeline", dataset: "total", value: "amount", w: 4 },
      { id: "mine", type: "table", title: "My work", dataset: "work", columns: ["key"], w: 8 },
      {
        id: "seats",
        type: "bars",
        title: "Tokens by seat",
        dataset: "spend",
        label: "handle",
        value: "total_tokens",
        w: 6,
      },
    ],
  },
  datasets: [
    {
      id: "deals",
      title: "Pipeline",
      source: "store",
      shape: { columns: [{ key: "amount", type: "number" }] },
      freshness: { mode: "live", collection: "deals" },
    },
    {
      id: "total",
      title: "Pipeline total",
      source: "calc",
      shape: { columns: [{ key: "amount", type: "number", label: "Amount" }] },
      freshness: { mode: "derived", inputs: ["deals"] },
    },
    {
      id: "work",
      title: "My open work",
      source: "engine",
      shape: { columns: [{ key: "key", type: "text" }] },
      freshness: { mode: "manual" },
    },
    {
      id: "spend",
      title: "Spend by seat",
      source: "engine",
      shape: {
        columns: [
          { key: "handle", type: "text" },
          { key: "total_tokens", type: "number" },
        ],
      },
      freshness: { mode: "manual" },
    },
  ],
  outline: { widgets: [], stats: [], datasets: [] },
};

/** The board's store collection, as an agent writes it. */
let amounts: number[];
/** How many times each dataset was read. */
let reads: Record<string, number>;
/** Whether the next spend read is refused by the read budget. */
let budgetSpent: boolean;
/** Whether the spend read fails as a node that could not reach its domain. */
let spendUnreadable: boolean;

const ready = (dataset: string, rows: DashboardDatasetAnswer["table"]): DashboardDatasetAnswer => ({
  artifact: ID,
  version: 1,
  dataset,
  status: "ready",
  as_of: "2026-10-18T10:00:00Z",
  table: rows,
});

function answer(dataset: string): {
  status: number;
  body: unknown;
  headers?: Record<string, string>;
} {
  switch (dataset) {
    case "deals":
      return {
        status: 200,
        body: ready("deals", {
          columns: board.datasets[0]!.shape.columns,
          rows: amounts.map((amount) => ({ amount })),
        }),
      };
    case "total":
      return {
        status: 200,
        body: ready("total", {
          columns: board.datasets[1]!.shape.columns,
          rows: [{ amount: amounts.reduce((a, b) => a + b, 0) }],
        }),
      };
    case "work":
      return {
        status: 200,
        body: {
          artifact: ID,
          version: 1,
          dataset: "work",
          status: "refused",
          refusal: { dataset: "work", reason: "no_grant", grants: ["worklet"] },
          detail: "You cannot read tracker: people and agents holding the tracker.",
        },
      };
    case "spend":
      if (spendUnreadable) {
        return {
          status: 503,
          body: { error: "unavailable", detail: "this node could not read spend right now" },
        };
      }
      if (budgetSpent) {
        budgetSpent = false;
        return {
          status: 429,
          body: { error: "read_budget", detail: "slow down" },
          headers: { "Retry-After": "1" },
        };
      }
      return {
        status: 200,
        body: ready("spend", {
          columns: board.datasets[3]!.shape.columns,
          rows: [{ handle: "ana", total_tokens: 4200 }],
        }),
      };
  }
  return { status: 404, body: { error: "not_found" } };
}

beforeEach(() => {
  amounts = [100, 250];
  reads = {};
  budgetSpent = false;
  spendUnreadable = false;
  Object.defineProperty(globalThis, "fetch", {
    writable: true,
    value: vi.fn(async (input: RequestInfo | URL) => {
      const path = new URL(String(input), "http://engine.test").pathname;
      if (path === `/artifacts/${ID}/versions/1/dashboard`) {
        return new Response(JSON.stringify(board), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
      }
      const dataset = /\/sources\/([a-z_]+)$/.exec(path)?.[1] ?? "";
      reads[dataset] = (reads[dataset] ?? 0) + 1;
      const got = answer(dataset);
      return new Response(JSON.stringify(got.body), {
        status: got.status,
        headers: { "Content-Type": "application/json", ...(got.headers ?? {}) },
      });
    }),
  });
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

function dataStore(): ArtifactDataStore {
  return new ArtifactDataStore({
    artifact: ID,
    socket: { query: vi.fn(async () => ({})) } as unknown as LiveSocket,
    via: { via: "dashboard" },
    clientID: "client-1",
    newKey: () => "key-1",
    write: async () => {
      throw new Error("a board writes nothing");
    },
  });
}

const mount = (store: ArtifactDataStore) =>
  render(
    <ToastProvider>
      <DashboardView detail={detail} version={1} store={store} options={{ gatherMs: 10 }} />
    </ToastProvider>,
  );

/** The figure a widget draws, by its id. */
const widget = (id: string) => document.querySelector<HTMLElement>(`[data-widget="${id}"]`)!;

test("an agent's write to the board's store dataset reaches the stat drawn from it, live", async () => {
  const store = dataStore();
  mount(store);
  await waitFor(() =>
    expect(within(widget("pipeline")).getByText((350).toLocaleString())).toBeTruthy(),
  );
  const before = { ...reads };

  // THE AGENT'S WRITE, as the room pushes it.
  amounts.push(650);
  store.applyPush({
    artifact: ID,
    deltas: [
      {
        collection: "deals",
        doc: "d3",
        version: 3,
        body: { amount: 650 },
        read: "all",
        created_by: "closer",
        updated_by: { name: "closer", kind: "agent" },
        updated_at: "2026-10-18T10:01:00Z",
      },
    ],
  });

  await waitFor(() =>
    expect(within(widget("pipeline")).getByText((1000).toLocaleString())).toBeTruthy(),
  );
  expect(reads.deals).toBe(before.deals! + 1);
  expect(reads.total).toBe(before.total! + 1);
  expect(reads.work).toBe(before.work);
  expect(reads.spend).toBe(before.spend);
});

test("a push naming another collection reads nothing again", async () => {
  const store = dataStore();
  mount(store);
  await waitFor(() => expect(reads.total).toBe(1));
  // THE GATHER IS ON THE CASE'S CLOCK: a push naming a live dataset's
  // collection is read again once the gather window closes, so the window is
  // run out, and every read it caused has landed, before nothing is counted.
  vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
  store.applyPush({
    artifact: ID,
    keys: [{ collection: "notes", doc: "n1", version: 2 }],
    reread: true,
  });
  await act(() => vi.runOnlyPendingTimersAsync());
  await settled(() => reads.total ?? 0);
  expect(reads.deals).toBe(1);
  expect(reads.total).toBe(1);
});

test("a dataset the reader may not read draws the refusal and no row", async () => {
  mount(dataStore());
  await waitFor(() =>
    expect(within(widget("mine")).getByText(/You cannot read tracker/)).toBeTruthy(),
  );
  expect(within(widget("mine")).queryByRole("table")).toBeNull();
});

test("a read the budget refused is asked again once the wait it named is over", async () => {
  // THE WAIT IS ON THE CASE'S CLOCK: the refusal names one second, which is
  // run out rather than slept, so "once it is over" is held exactly.
  // Waited for by settling, not by `eventually`, whose `vi.waitFor` moves a
  // faked clock on at every look and would spend part of the wait itself.
  vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
  budgetSpent = true;
  mount(dataStore());
  await settled(
    () => reads.spend ?? 0,
    () => reads.total ?? 0,
  );
  expect(reads.spend).toBe(1);
  expect(within(widget("seats")).queryByText("ana")).toBeNull();
  await act(() => vi.advanceTimersByTimeAsync(999));
  expect(reads.spend).toBe(1);
  await act(() => vi.advanceTimersByTimeAsync(1));
  await settled(() => reads.spend ?? 0);
  expect(reads.spend).toBe(2);
  expect(within(widget("seats")).getByText("ana")).toBeTruthy();
});

// A READ THAT FAILED OUTSIDE THE ANSWER'S STATUSES is said in a reader's
// words: the route's detail names a node and a domain, not anything a person
// can act on.
test("a dataset that could not be read says so in a reader's words, never the route's", async () => {
  spendUnreadable = true;
  mount(dataStore());
  await waitFor(() => expect(within(widget("seats")).getByText(ROWS_UNREAD)).toBeTruthy());
  expect(document.body.textContent).not.toContain("this node could not read");
});

test("the Sources rail lists every dataset with how it is kept current", async () => {
  mount(dataStore());
  const rail = await screen.findByRole("complementary", { name: "Sources" });
  // WAITED FOR WHOLE: a dataset's title is drawn before what keeps it
  // current, which its own answer says.
  await waitFor(() => {
    expect(within(rail).getByText("Pipeline total")).toBeTruthy();
    expect(within(rail).getAllByText("Live").length).toBeGreaterThan(0);
    expect(within(rail).getByText("As its rows are written")).toBeTruthy();
    expect(within(rail).getByText("When its inputs change")).toBeTruthy();
    expect(within(rail).getByText("The deals collection of this dashboard's data")).toBeTruthy();
  });
  // One card, headed by its own title, as a panel beside a board is.
  expect(within(rail).getByRole("heading", { level: 2, name: "Sources" })).toBeTruthy();
  expect(rail.querySelector(".crewlet-card")).not.toBeNull();
});

// EVERY WIDGET STANDS ON THE BOARD'S TWELVE-COLUMN GRID, the one Home's board
// uses: a frame says how many columns it spans, and only a grid parent reads
// that, so a frame placed straight in the board stacks at full width.
test("each widget is a cell of the board's twelve-column grid", async () => {
  mount(dataStore());
  await waitFor(() => expect(widget("pipeline")).toBeTruthy());
  const grid = document.querySelector<HTMLElement>(".dashboard-board .crewlet-arrange-grid");
  expect(grid).toBeTruthy();
  expect(grid?.style.getPropertyValue("--crewlet-arrange-columns")).toBe("12");
  const cells = [...document.querySelectorAll(".dashboard-cell")].filter(
    (c) => !c.parentElement?.closest(".dashboard-cell"),
  );
  expect(cells.length).toBe(board.board.widgets.length);
  for (const cell of cells) expect(cell.parentElement).toBe(grid);
});

// THE RAIL'S PREVIEW IS A WIDGET'S TABLE: a dataset's first rows drawn by the
// rule a widget's table is, never sorted and never broken mid-word, where the
// design system's default table offered a sort on every column and wrapped
// "DEP-1040" over two lines in the rail's width.
test("the Sources rail previews a dataset's rows as a widget's table draws them", async () => {
  mount(dataStore());
  const rail = await screen.findByRole("complementary", { name: "Sources" });
  await waitFor(() => expect(within(rail).getByText("Pipeline total")).toBeTruthy());
  within(rail).getByText("Pipeline total").click();
  await waitFor(() => expect(rail.querySelector("table")).not.toBeNull());
  expect(rail.querySelector(".crewlet-data-table--wrap-lines")).toBeNull();
  expect(rail.querySelector(".crewlet-data-table__sort-button, th[aria-sort]")).toBeNull();
});
