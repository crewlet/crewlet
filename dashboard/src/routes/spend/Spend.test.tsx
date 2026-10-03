/**
 * Spend › Overview and Spend › Expensive tasks, rendered over wire-shaped
 * answers.
 *
 * What these hold, one case each: every figure is the window's that ANSWERED
 * (and says which, from the answer, never from the control); a refusal is the
 * engine's own sentence and no figures; the window is asked as the engine
 * takes it — company days, or two company dates; the monthly tile and the
 * per-seat "today" column are the engine's state and the budget's own
 * calendar window; a tile with no answer says so rather than drawing a zero;
 * the expensive tasks are the engine's order with their reopens; and the
 * dimensions the chart offers are the contract's six.
 */

import { act, cleanup, fireEvent, render, screen, within } from "~/test/inCase.ts";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { ExpensiveTasks } from "./ExpensiveTasks.tsx";
import { Spend } from "./Spend.tsx";
import { FrameReadings } from "~/app/Shell.tsx";
import { LayerHost } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import { GROUPS } from "~/contract/spend.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, QueryRefusedError, Store } from "~/protocol/index.ts";
import type {
  Bucket,
  BudgetWindow,
  Rollup,
  TokenSeries,
  WorkItemsAnswer,
  WorkSummary,
} from "~/protocol/index.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
});

afterEach(() => {
  cleanup();
  location.hash = "";
});

function bucket(total: number, extra: Partial<Bucket> = {}): Bucket {
  return { input_tokens: total, output_tokens: 0, total_tokens: total, calls: 3, ...extra };
}

/** A named 30-day window's rollup, as `tokens{days:30}` answers it. */
function rollup(total: number, extra: Partial<Rollup> = {}): Rollup {
  return {
    since: "2026-08-31T00:00:00Z",
    until: "2026-09-30T00:00:00Z",
    from: "2026-08-31",
    to: "2026-09-29",
    days: 30,
    horizon: { days: 181, floor: "2026-04-01" },
    totals: bucket(total),
    by_phase: [],
    by_model: [],
    by_provider: [],
    by_worker: [],
    by_agent: [],
    ...extra,
  };
}

function series(total: number): TokenSeries {
  return {
    group: "phase",
    bucket: "day",
    since: "2026-08-31T00:00:00Z",
    until: "2026-09-30T00:00:00Z",
    from: "2026-08-31",
    to: "2026-09-29",
    days: 30,
    horizon: { days: 181, floor: "2026-04-01" },
    series: [],
    by_group: [],
    totals: bucket(total),
    grouped: bucket(total),
  };
}

function month(extra: Partial<BudgetWindow> = {}): BudgetWindow {
  return {
    period: "month",
    window: "2026-09",
    starts_at: "2026-09-01T00:00:00Z",
    resets_at: "2026-10-01T00:00:00Z",
    used: 48_600_000,
    limit: 80_000_000,
    state: "ok",
    ...extra,
  };
}

function task(key: string, tokens: number, spend: Partial<WorkSummary["spend"]> = {}): WorkSummary {
  return {
    id: `id-${key}`,
    key,
    project: "ENG",
    title: `Task ${key}`,
    type: "task",
    status: "in_progress",
    updated: "2026-09-28T00:00:00Z",
    version: 1,
    spend: { tokens, turns: 22, workers: 0, sent_back: 0, reopens: 0, ...spend },
  } as WorkSummary;
}

function items(rows: WorkSummary[], extra: Partial<WorkItemsAnswer> = {}): WorkItemsAnswer {
  return { items: rows, total_hint: rows.length, complete: true, ...extra };
}

type Answer = unknown | ((params: Record<string, unknown>) => unknown);

/** Every question the screen asked, with its parameters. */
let asks: { what: string; params: Record<string, unknown> }[] = [];

/**
 * Mount a screen with a stubbed answer per question. An answer that is an
 * Error rejects; a function is handed the parameters; a missing one never
 * settles, which is "still loading".
 */
function mount(
  hash: string,
  view: React.ReactElement,
  answers: Record<string, Answer>,
  setup: (store: Store) => void = () => {},
) {
  location.hash = hash;
  asks = [];
  const store = new Store();
  store.applyHealth({ status: "healthy" });
  store.applyOrg({
    timezone: "UTC",
    roles: [
      { name: "CEO", handle: "ceo" },
      { name: "SWE", handle: "swe" },
    ],
  });
  setup(store);
  const socket = new LiveSocket(store);
  (
    socket as unknown as {
      query: (what: string, params: Record<string, unknown>) => Promise<unknown>;
    }
  ).query = (what, params) => {
    asks.push({ what, params });
    const answer = answers[what];
    if (answer === undefined) return new Promise(() => {});
    const value =
      typeof answer === "function" ? (answer as (p: unknown) => unknown)(params) : answer;
    return value instanceof Error ? Promise.reject(value) : Promise.resolve(value);
  };
  return render(
    <ClientContext.Provider value={{ store, socket }}>
      <FrameReadings>
        <Router>{view}</Router>
      </FrameReadings>
    </ClientContext.Provider>,
  );
}

async function settle() {
  await act(async () => {
    for (let i = 0; i < 6; i++) await Promise.resolve();
  });
}

const params = (what: string) => asks.filter((a) => a.what === what).map((a) => a.params);

// The viewer answer as the engine sends it: a founder holding the grant every
// ceiling write takes (`config:write`), and a reader holding the state grant
// alone.
const VIEWER = {
  login: "jane.doe",
  grants: ["state:read", "config:read", "config:write"],
  handle: "jane",
  owner: "jane",
  name: "Jane",
  kind: "human",
  acts: [],
};
const READER = {
  login: "reader",
  grants: ["state:read"],
  handle: "",
  owner: "reader",
  name: "",
  kind: "",
  acts: [],
};

// THE CONTROL: a window that answered is what the hero is of, and says so in
// the ANSWER's words — thirty days, from the answer's own `days`.
test("the hero is the window that answered, labelled from the answer", async () => {
  mount("#/spend?window=30d", <Spend />, {
    tokens: rollup(48_600_000),
    token_series: (p: Record<string, unknown>) =>
      p.previous ? series(41_186_441) : series(48_600_000),
    viewer: VIEWER,
  });
  await settle();
  expect(screen.getByText("Tokens · last 30 days")).toBeTruthy();
  expect(screen.getByText("48.6M")).toBeTruthy();
  // +18%: 48.6M against 41.19M, cut by the ENGINE (`previous=true`).
  expect(screen.getByText(/\+18%/)).toBeTruthy();
  expect(screen.getByText(/vs previous 30 days/)).toBeTruthy();
  // The history's floor, stated.
  expect(screen.getByText(/History from Apr 1 · 181 days/)).toBeTruthy();
});

// THE LABEL IS THE DATA'S, NOT THE CONTROL'S. The URL asks for seven days; the
// answer on screen covers thirty (an older answer, a reconnect) — the heading
// names the thirty the figures are of.
test("the window label is the answer's window, never the control's", async () => {
  mount("#/spend?window=7d", <Spend />, { tokens: rollup(10_000), viewer: VIEWER });
  await settle();
  expect(screen.getByText("Tokens · last 30 days")).toBeTruthy();
  expect(screen.queryByText("Tokens · last 7 days")).toBeNull();
});

// A NAMED WINDOW IS COMPANY DAYS, asked as `days`; A NAMED PAIR OF DATES IS
// ASKED AS THE DATES. Never two instants this browser subtracted on its own
// clock, which names a different week from the one the engine cuts.
test("the window is asked as company days, or as two company dates", async () => {
  mount("#/spend?window=30d", <Spend />, { tokens: rollup(1), viewer: VIEWER });
  await settle();
  expect(params("tokens")).toEqual([{ days: 30 }]);
  expect(params("token_series")).toContainEqual({ days: 30, group: "phase", bucket: "day" });
  expect(params("token_series")).toContainEqual({ days: 30, bucket: "day", previous: true });
  cleanup();

  mount("#/spend?window=2026-09-01T00:00:00.000Z/2026-09-09T00:00:00.000Z", <Spend />, {
    tokens: rollup(1),
    viewer: VIEWER,
  });
  await settle();
  // Both inclusive: the window ends at the first instant of the 9th, so its
  // last day is the 8th.
  expect(params("tokens")).toEqual([{ since: "2026-09-01", until: "2026-09-08" }]);
});

// THE CUSTOM DIALOG OPENS ON THE WINDOW THE CHART IS OF. At 13:00 UTC it is
// 15:00 in Berlin on the 29th, and "30d" is the thirty company days ending
// today — so the dialog shows the 31st of August to the 29th of September,
// and Apply without an edit asks for exactly that. Aligned to UTC days, as it
// was, the named range ended at 02:00 on the 30th in Berlin and the dialog
// offered thirty-one days ending tomorrow.
test("the custom window opens on the company days a named range covers", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-09-29T13:00:00Z"));
  try {
    mount(
      "#/spend?window=30d",
      <LayerHost>
        <Spend />
      </LayerHost>,
      { tokens: rollup(1), viewer: VIEWER },
      (store) =>
        store.applyOrg({
          timezone: "Europe/Berlin",
          roles: [{ name: "SWE", handle: "swe" }],
        }),
    );
    await settle();
    fireEvent.click(screen.getByTitle("Name two company days of your own"));
    expect((screen.getByLabelText("First day") as HTMLInputElement).value).toBe("2026-08-31");
    expect((screen.getByLabelText("Last day") as HTMLInputElement).value).toBe("2026-09-29");
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    await settle();
    expect(params("tokens")).toContainEqual({ since: "2026-08-31", until: "2026-09-29" });
  } finally {
    vi.useRealTimers();
  }
});

// THE REFUSAL IS THE ENGINE'S SENTENCE. Ninety-one days is past the engine's
// ninety, and the reader chose it — so the screen says what the engine said
// about it, not that the screen asked wrong, and draws no figures.
test("a window the engine refuses shows the engine's own sentence and no figures", async () => {
  const sentence =
    "2026-06-01 to 2026-08-30 is 91 days, and a spend window is at most 90 — bring since and until closer together";
  mount("#/spend?window=2026-06-01T00:00:00.000Z/2026-08-31T00:00:00.000Z", <Spend />, {
    tokens: new QueryRefusedError("bad_params", null, sentence),
    viewer: VIEWER,
  });
  await settle();
  expect(screen.getByText(new RegExp("is 91 days, and a spend window is at most 90"))).toBeTruthy();
  expect(screen.queryByText(/screen&rsquo;s bug|screen's bug/)).toBeNull();
  expect(screen.queryByText(/Tokens · /)).toBeNull();
});

// A REFUSAL REPLACES THE FIGURES, AND IS SAID ONCE. The window before it
// answered, so the hook still holds that answer; drawn under the refused
// control it was last week's numbers beneath a "Custom" that answered nothing,
// and each of the three questions asked of the refused window put the same
// sentence on screen again — the banner, the chart card and the hero's
// comparison line.
test("a refused window draws none of the previous window's figures, and says so once", async () => {
  const sentence =
    "2026-05-01 to 2026-09-29 is 152 days, and a spend window is at most 90 — bring since and until closer together";
  const refusal = () => new QueryRefusedError("bad_params", null, sentence);
  mount("#/spend?window=30d", <Spend />, {
    tokens: (p: Record<string, unknown>) => (p.since ? refusal() : rollup(48_600_000)),
    token_series: (p: Record<string, unknown>) => (p.since ? refusal() : series(48_600_000)),
    viewer: VIEWER,
  });
  await settle();
  expect(screen.getByText("48.6M")).toBeTruthy();
  await act(async () => {
    location.hash = "#/spend?window=2026-05-01T00:00:00.000Z/2026-09-30T00:00:00.000Z";
    window.dispatchEvent(new HashChangeEvent("hashchange"));
  });
  await settle();
  expect(params("tokens")).toContainEqual({ since: "2026-05-01", until: "2026-09-29" });
  expect(screen.getAllByText(new RegExp("is 152 days")).length).toBe(1);
  expect(screen.queryByText("48.6M")).toBeNull();
  expect(screen.queryByText(/Tokens · /)).toBeNull();
  expect(screen.queryByRole("heading", { name: "By agent" })).toBeNull();
  expect(screen.queryByRole("radiogroup", { name: "Split by" })).toBeNull();
  expect(screen.getByRole("button", { name: "Export" }).hasAttribute("disabled")).toBe(true);
});

// THE MONTHLY TILE IS THE ENGINE'S STATE, and its window is this month.
test("the monthly budget is drawn in the engine's state, with its reset", async () => {
  mount("#/spend?window=30d", <Spend />, { tokens: rollup(1), viewer: VIEWER }, (store) =>
    store.applyBudget({
      meter_id: "n:1",
      seq: 1,
      timezone: "UTC",
      org: { windows: [month({ used: 70_000_000, state: "refusing" })] },
    }),
  );
  await settle();
  const meter = screen.getByRole("meter", { name: "This month" });
  expect(meter.getAttribute("aria-valuetext")).toMatch(/refusing charges, resets Oct 1/);
  expect(screen.getByText(/70M of 80M · resets Oct 1/)).toBeTruthy();
  expect(screen.getByText(/Refusing charges/)).toBeTruthy();
});

const UNCAPPED = { meter_id: "n:1", seq: 1, timezone: "UTC", org: { windows: [] } };

// NOT REPORTED IS NOT "NO BUDGET". Before the engine's first report the tile
// waits: "No monthly budget" and "Set one" there told the operator of a capped
// company, for the first seconds after every engine start, to set a ceiling it
// already had.
test("before the first budget report the tile waits, and offers nothing", async () => {
  mount("#/spend?window=30d", <Spend />, { tokens: rollup(1), viewer: VIEWER });
  await settle();
  expect(screen.getByRole("status", { name: /not reported yet/ })).toBeTruthy();
  expect(screen.queryByText("No monthly budget")).toBeNull();
  expect(screen.queryByRole("link", { name: "Set one" })).toBeNull();
});

// NO CEILING IS SAID, AND ONLY THE READER WHO CAN SET ONE IS OFFERED TO.
test("no monthly budget says so, and offers to set one only to an operator", async () => {
  mount("#/spend?window=30d", <Spend />, { tokens: rollup(1), viewer: VIEWER }, (store) =>
    store.applyBudget(UNCAPPED),
  );
  await settle();
  expect(screen.getByText("No monthly budget")).toBeTruthy();
  expect(screen.getByRole("link", { name: "Set one" }).getAttribute("href")).toBe(
    "#/spend/budgets",
  );
  cleanup();

  mount(
    "#/spend?window=30d",
    <Spend />,
    {
      tokens: rollup(1),
      viewer: READER,
    },
    (store) => store.applyBudget(UNCAPPED),
  );
  await settle();
  expect(screen.getByText("No monthly budget")).toBeTruthy();
  expect(screen.queryByRole("link", { name: "Set one" })).toBeNull();
});

// THE CACHE'S SHARE IS READ ÷ INPUT, and a window where nothing reported a
// cache has no figure rather than "0%".
test("the cache tile divides the engine's counts, and is absent when none were reported", async () => {
  mount("#/spend?window=30d", <Spend />, {
    tokens: rollup(1000, { totals: bucket(1000, { input_tokens: 800, cache_read_tokens: 352 }) }),
    viewer: VIEWER,
  });
  await settle();
  expect(screen.getByText("44%")).toBeTruthy();
  cleanup();

  mount("#/spend?window=30d", <Spend />, { tokens: rollup(1000), viewer: VIEWER });
  await settle();
  expect(screen.queryByText("of input served from prompt cache")).toBeNull();
  expect(screen.queryByText("0%")).toBeNull();
});

// THE MEDIAN TASK IS THE TRACKER'S, over the tasks FINISHED in the window, and
// on a company whose tracker is not the engine's there is no tile at all.
test("the median task tile asks the tracker, and is absent on a non-native tracker", async () => {
  mount("#/spend?window=30d", <Spend />, {
    tokens: rollup(1),
    viewer: VIEWER,
    work_items: (p: Record<string, unknown>) =>
      p.totals
        ? items([], {
            totals: [
              { key: "spend_tokens:median", column: "spend_tokens", op: "median", value: 96_000 },
              { key: "spend_tokens:count", column: "spend_tokens", op: "count", value: 14 },
            ],
          })
        : items([]),
  });
  await settle();
  expect(screen.getByText("96k")).toBeTruthy();
  expect(screen.getByText(/median tokens per finished task · 14 tasks/)).toBeTruthy();
  const median = params("work_items").find((p) => p.totals);
  expect(median).toEqual({
    finished: "range:2026-08-31T00:00:00Z..2026-09-30T00:00:00Z",
    show_closed: "true",
    spend_tokens: "gt:0",
    totals: "spend_tokens:median,spend_tokens:count",
    limit: 1,
  });
  cleanup();

  mount("#/spend?window=30d", <Spend />, {
    tokens: rollup(1),
    viewer: VIEWER,
    work_items: new QueryRefusedError("unknown_query", null),
  });
  await settle();
  expect(screen.queryByText(/median tokens per finished task/)).toBeNull();
  expect(screen.queryByText("Most expensive tasks")).toBeNull();
});

// "EXHAUSTED" IS THE ENGINE'S `refusing` FOR THE SEAT'S DAY, and the column is
// TODAY's whatever the window — never a thirty-day spend over a day's ceiling.
test("a seat whose day is refusing reads exhausted, and share and per-turn are the window's", async () => {
  const swe = {
    ...bucket(3_000),
    role: "SWE",
    handle: "swe",
    agent_id: "a-swe",
    by_phase: {},
    turns: 10,
  };
  const ceo = {
    ...bucket(1_000),
    role: "CEO",
    handle: "ceo",
    agent_id: "a-ceo",
    by_phase: {},
    turns: 4,
  };
  mount(
    "#/spend?window=30d",
    <Spend />,
    { tokens: rollup(4_000, { by_agent: [ceo, swe] }), viewer: VIEWER },
    (store) => {
      store.applySeats([{ id: "swe", agent_id: "a-swe", role: "SWE", handle: "swe" }]);
      store.applyAgents([
        {
          // PAIRED BY AGENT ID with the spend row, never by handle.
          agent_id: "a-swe",
          role: "SWE",
          handle: "swe",
          activity: "stopped",
          stopped_reason: "budget",
          budget: {
            windows: [
              {
                period: "day",
                window: "2026-09-29",
                starts_at: "2026-09-29T00:00:00Z",
                resets_at: "2026-09-30T00:00:00Z",
                used: 100,
                limit: 100,
                state: "refusing",
              },
            ],
          },
        },
      ]);
    },
  );
  await settle();
  const rows = [...document.querySelectorAll<HTMLElement>(".grid-row")];
  const sweRow = rows.find((r) => within(r).queryByText("SWE"))!;
  expect(within(sweRow).getByText("exhausted")).toBeTruthy();
  // The meter is named for the seat the row DRAWS — its name, never the
  // handle a screen reader would otherwise hear beside it.
  expect(within(sweRow).getByRole("meter", { name: "SWE's daily token budget" })).toBeTruthy();
  expect(within(sweRow).getByText("75%")).toBeTruthy();
  expect(within(sweRow).getByText("300")).toBeTruthy();
  const ceoRow = rows.find((r) => within(r).queryByText("CEO"))!;
  // Nothing caps the CEO's day: said in words, not an empty bar or a dash.
  expect(within(ceoRow).getByText("No ceiling")).toBeTruthy();
  expect(screen.getByText("Budget today")).toBeTruthy();
  // The biggest first, the table's opening order.
  expect(rows.indexOf(sweRow)).toBeLessThan(rows.indexOf(ceoRow));
});

// A COLUMN OF ABSENCES IS NOT DRAWN. Where no seat's day is capped the
// "Budget today" column would be a dash on every row, taking the width the
// names were cut for; the caption says it once instead.
test("with no seat's day capped the budget column is not drawn, and the caption says why", async () => {
  const ceo = {
    ...bucket(1_000),
    role: "CEO",
    handle: "ceo",
    agent_id: "a-ceo",
    by_phase: {},
    turns: 4,
  };
  mount("#/spend?window=30d", <Spend />, {
    tokens: rollup(1_000, { by_agent: [ceo] }),
    viewer: VIEWER,
  });
  await settle();
  expect(screen.queryByText("Budget today")).toBeNull();
  expect(screen.queryByText("No ceiling")).toBeNull();
  expect(screen.getByText(/no seat has a daily token ceiling/)).toBeTruthy();
});

// "AND N MORE" IS ITS OWN ELEMENT, apart from the names: the column is one
// line, and cut as one sentence the count went first — three names then read
// as everybody who used the entry.
test("by model names the top seats and keeps the count of the rest apart", async () => {
  mount("#/spend?window=30d", <Spend />, {
    tokens: rollup(1_000, {
      by_provider: [
        {
          ...bucket(1_000),
          provider_key: "default",
          models: ["claude-sonnet"],
          seats: ["ceo", "swe"],
          seats_total: 6,
        },
      ],
    }),
    viewer: VIEWER,
  });
  await settle();
  const more = screen.getByText("and 4 more");
  expect(more.className).toBe("spend-used-more");
  expect(more.parentElement!.getAttribute("title")).toBe("Used by CEO, SWE and 4 more");
  expect(more.previousElementSibling!.textContent).toBe("CEO, SWE");
});

// THE FOUR PHASE BANDS ARE ALWAYS THE LEGEND. The engine answers all four by
// phase, a band the window never spent in at zero, and the chart keeps it: the
// sentence under the title names review, workers and auxiliary, and a legend
// that lost Workers in a month without one disagreed with it.
test("the phase legend is all four bands, a band nothing spent in included", async () => {
  const band = (group: string, total: number) => ({
    ...bucket(total),
    group,
    other: false,
    folded: 0,
  });
  mount("#/spend?window=30d", <Spend />, {
    tokens: rollup(100),
    token_series: {
      ...series(100),
      series: [
        {
          ...bucket(100),
          at: "2026-09-01T00:00:00Z",
          window: "2026-09-01",
          days: 1,
          groups: { execute: bucket(80), review: bucket(15), auxiliary: bucket(5) },
          other: bucket(0),
        },
      ],
      by_group: [band("execute", 80), band("review", 15), band("workers", 0), band("auxiliary", 5)],
    },
    viewer: VIEWER,
  });
  await settle();
  const chart = screen
    .getByRole("heading", { name: "Daily tokens by phase" })
    .closest(".spend-chart");
  for (const name of ["Execute", "Review", "Workers", "Auxiliary"]) {
    expect(within(chart as HTMLElement).getAllByText(name).length).toBeGreaterThan(0);
  }
});

// A SEAT SERIES IS NAMED AS THE TABLE BELOW NAMES IT: the engine keys the band
// on the seat's agent id — the identity a rename does not move — and sends the
// seat's name beside it, which the legend reads.
test("splitting by seat labels each band with the seat's name, not its handle", async () => {
  mount("#/spend?window=30d&group=seat", <Spend />, {
    tokens: rollup(100),
    token_series: (p: Record<string, unknown>) => ({
      ...series(100),
      group: p.group === "seat" ? "seat" : "phase",
      series: [
        {
          ...bucket(100),
          at: "2026-09-01T00:00:00Z",
          window: "2026-09-01",
          days: 1,
          groups: { "a-swe": bucket(100) },
          other: bucket(0),
        },
      ],
      by_group:
        p.group === "seat"
          ? [
              {
                ...bucket(100),
                group: "a-swe",
                label: "SWE",
                handle: "swe",
                other: false,
                folded: 0,
              },
            ]
          : [],
    }),
    viewer: VIEWER,
  });
  await settle();
  const chart = screen
    .getByRole("heading", { name: "Daily tokens by agent" })
    .closest(".spend-chart");
  expect(within(chart as HTMLElement).getAllByText("SWE").length).toBeGreaterThan(0);
  expect(within(chart as HTMLElement).queryByText("swe")).toBeNull();
  expect(within(chart as HTMLElement).queryByText("a-swe")).toBeNull();
});

// THE DIMENSIONS ARE THE CONTRACT'S SIX, which the engine gate holds against
// `tokens.Groups` — and a choice re-asks the series by that dimension.
test("the chart offers the contract's six dimensions and asks by the one chosen", async () => {
  mount("#/spend?window=30d", <Spend />, {
    tokens: rollup(1),
    token_series: series(1),
    viewer: VIEWER,
  });
  await settle();
  const group = screen.getByRole("radiogroup", { name: "Split by" });
  const options = within(group)
    .getAllByRole("radio")
    .map((r) => r.textContent);
  expect(options).toEqual(GROUPS.map((g) => g.label));
  fireEvent.click(within(group).getByRole("radio", { name: "Unit" }));
  await settle();
  expect(location.hash).toContain("group=unit");
  expect(params("token_series")).toContainEqual({ days: 30, group: "unit", bucket: "day" });
});

// THE TURN VIEW IS THE TURN LIST'S. Per-turn spend is not on this screen; the
// link carries the window where the turn list has one, and a quarter goes to
// the thirty days the event log holds — saying so.
test("recent turns link to the turn list by tokens, over the same window where it has one", async () => {
  mount("#/spend?window=90d", <Spend />, { tokens: rollup(1), viewer: VIEWER });
  await settle();
  const link = screen.getByRole("link", { name: "Recent turns by tokens →" });
  expect(link.getAttribute("href")).toBe("#/live/turns?sort=-tokens&window=30d");
  expect(screen.getByText(/the last 30 days — the turn list keeps no older/)).toBeTruthy();
});

// A REFUSED WINDOW HAS NO LIST TO WAIT FOR. The list's question starts where
// the window's answer does, so a refused window never asks it — and the grid
// it would have filled was a loading skeleton under the refusal for good.
test("the expensive tasks under a refused window say the refusal and settle", async () => {
  const sentence =
    "tokens: since=2026-06-01 until=2026-08-30 is 91 days, and a spend window is at most 90";
  mount(
    "#/spend/tasks?window=2026-06-01T00:00:00.000Z/2026-08-31T00:00:00.000Z",
    <ExpensiveTasks />,
    {
      tokens: new QueryRefusedError("bad_params", null, sentence),
      viewer: VIEWER,
    },
  );
  await settle();
  expect(screen.getByText(/is 91 days, and a spend window is at most 90/)).toBeTruthy();
  expect(document.querySelector('[aria-busy="true"]')).toBeNull();
  expect(screen.queryByRole("heading", { name: "Most expensive tasks" })).toBeNull();
  expect(params("work_items").filter((p) => p.sort === "-spend_tokens")).toEqual([]);
});

// THE MOST EXPENSIVE TASKS ARE THE TRACKER'S ORDER, with what drove each —
// the reopens included — and no price anywhere.
test("the expensive tasks are the engine's order, with their reopens", async () => {
  mount("#/spend/tasks?window=30d", <ExpensiveTasks />, {
    tokens: rollup(1),
    viewer: VIEWER,
    work_items: items([
      task("ENG-401", 3_400_000, { reopens: 4 }),
      task("ENG-405", 2_100_000, { workers: 3, turns: 14 }),
      task("PROD-91", 1_200_000, { sent_back: 3, turns: 9 }),
    ]),
  });
  await settle();
  const keys = screen.getAllByText(/^(ENG|PROD)-\d+$/).map((el) => el.textContent);
  expect(keys).toEqual(["ENG-401", "ENG-405", "PROD-91"]);
  expect(screen.getByText("22 turns · reopened 4 times")).toBeTruthy();
  expect(screen.getByText("14 turns · 3 workers")).toBeTruthy();
  expect(screen.getByText("9 turns · sent back 3 times")).toBeTruthy();
  expect(params("work_items")[0]).toEqual({
    sort: "-spend_tokens",
    closed_since: "2026-08-31T00:00:00Z",
    updated: "range:2026-08-31T00:00:00Z..2026-09-30T00:00:00Z",
    spend_tokens: "gt:0",
    fields: "spend",
    limit: 50,
  });
  expect(document.body.innerHTML).not.toMatch(/[$€£]\s?\d|\bUSD\b/);
});
