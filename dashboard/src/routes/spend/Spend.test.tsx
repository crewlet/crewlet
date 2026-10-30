/**
 * Settings › Token spend and Settings › Expensive tasks, rendered over wire-shaped
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

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { ExpensiveTasks } from "./ExpensiveTasks.tsx";
import { Spend } from "./Spend.tsx";
import { FrameReadings } from "~/app/Shell.tsx";
import { LayerHost } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import { GROUPS } from "~/contract/spend.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, QueryError, Store } from "~/protocol/index.ts";
import type {
  Bucket,
  BudgetWindow,
  Rollup,
  TokenSeries,
  WorkItemsAnswer,
  WorkSummary,
} from "~/protocol/index.ts";
import { healthFrame } from "~/test/health.ts";
import { withDerived } from "~/test/org.ts";

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
  store.applyHealth(healthFrame());
  store.applyOrg(
    withDerived({
      timezone: "UTC",
      roles: [
        { name: "CEO", handle: "ceo" },
        { name: "SWE", handle: "swe" },
      ],
    }),
  );
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

const VIEWER = {
  login: "op-1",
  grants: ["admin", "worklet", "doclet"],
  handle: "jane",
  owner: "jane",
  name: "Jane",
  kind: "human",
};

// THE CONTROL: a window that answered is what the hero is of, and says so in
// the ANSWER's words — thirty days, from the answer's own `days`.
test("the hero is the window that answered, labelled from the answer", async () => {
  mount("#/settings/spend?window=30d", <Spend />, {
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
  mount("#/settings/spend?window=7d", <Spend />, { tokens: rollup(10_000), viewer: VIEWER });
  await settle();
  expect(screen.getByText("Tokens · last 30 days")).toBeTruthy();
  expect(screen.queryByText("Tokens · last 7 days")).toBeNull();
});

// A NAMED WINDOW IS COMPANY DAYS, asked as `days`; A NAMED PAIR OF DATES IS
// ASKED AS THE DATES. Never two instants this browser subtracted on its own
// clock, which names a different week from the one the engine cuts.
test("the window is asked as company days, or as two company dates", async () => {
  mount("#/settings/spend?window=30d", <Spend />, { tokens: rollup(1), viewer: VIEWER });
  await settle();
  expect(params("tokens")).toEqual([{ days: 30 }]);
  expect(params("token_series")).toContainEqual({ days: 30, group: "phase", bucket: "day" });
  expect(params("token_series")).toContainEqual({ days: 30, bucket: "day", previous: true });
  cleanup();

  mount("#/settings/spend?window=2026-09-01T00:00:00.000Z/2026-09-09T00:00:00.000Z", <Spend />, {
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
      "#/settings/spend?window=30d",
      <LayerHost>
        <Spend />
      </LayerHost>,
      { tokens: rollup(1), viewer: VIEWER },
      (store) =>
        store.applyOrg(
          withDerived({
            timezone: "Europe/Berlin",
            roles: [{ name: "SWE", handle: "swe" }],
          }),
        ),
    );
    await settle();
    fireEvent.click(screen.getByTitle("Pick two dates"));
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
    "2026-06-01 to 2026-08-30 is 91 days, and a spend window is at most 90. Bring since and until closer together";
  mount("#/settings/spend?window=2026-06-01T00:00:00.000Z/2026-08-31T00:00:00.000Z", <Spend />, {
    tokens: new QueryError("bad_params", null, sentence),
    viewer: VIEWER,
  });
  await settle();
  expect(screen.getByText(new RegExp("is 91 days, and a spend window is at most 90"))).toBeTruthy();
  expect(screen.queryByText("The engine refused this request.")).toBeNull();
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
    "2026-05-01 to 2026-09-29 is 152 days, and a spend window is at most 90. Bring since and until closer together";
  const refusal = () => new QueryError("bad_params", null, sentence);
  mount("#/settings/spend?window=30d", <Spend />, {
    tokens: (p: Record<string, unknown>) => (p.since ? refusal() : rollup(48_600_000)),
    token_series: (p: Record<string, unknown>) => (p.since ? refusal() : series(48_600_000)),
    viewer: VIEWER,
  });
  await settle();
  expect(screen.getByText("48.6M")).toBeTruthy();
  await act(async () => {
    location.hash = "#/settings/spend?window=2026-05-01T00:00:00.000Z/2026-09-30T00:00:00.000Z";
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
  mount("#/settings/spend?window=30d", <Spend />, { tokens: rollup(1), viewer: VIEWER }, (store) =>
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
  expect(screen.getByText("Full until Oct 1.")).toBeTruthy();
});

const UNCAPPED = { meter_id: "n:1", seq: 1, timezone: "UTC", org: { windows: [] } };

// NOT REPORTED IS NOT "NO BUDGET". Before the engine's first report the tile
// waits: "No monthly budget" and "Set one" there told the operator of a capped
// company, for the first seconds after every engine start, to set a ceiling it
// already had.
test("before the first budget report the tile waits, and offers nothing", async () => {
  mount("#/settings/spend?window=30d", <Spend />, { tokens: rollup(1), viewer: VIEWER });
  await settle();
  expect(screen.getByRole("status", { name: /not reported yet/ })).toBeTruthy();
  expect(screen.queryByText("No monthly budget")).toBeNull();
  expect(screen.queryByRole("link", { name: "Set one" })).toBeNull();
});

// NO CEILING IS SAID, AND ONLY THE READER WHO CAN SET ONE IS OFFERED TO.
test("no monthly budget says so, and offers to set one only to an operator", async () => {
  mount("#/settings/spend?window=30d", <Spend />, { tokens: rollup(1), viewer: VIEWER }, (store) =>
    store.applyBudget(UNCAPPED),
  );
  await settle();
  expect(screen.getByText("No monthly budget")).toBeTruthy();
  expect(screen.getByRole("link", { name: "Set one" }).getAttribute("href")).toBe(
    "#/company/budgets",
  );
  cleanup();

  mount(
    "#/settings/spend?window=30d",
    <Spend />,
    {
      tokens: rollup(1),
      viewer: { login: "", grants: [], handle: "", owner: "", name: "", kind: "" },
    },
    (store) => store.applyBudget(UNCAPPED),
  );
  await settle();
  expect(screen.getByText("No monthly budget")).toBeTruthy();
  expect(screen.queryByRole("link", { name: "Set one" })).toBeNull();
});

// A MANAGED DOCUMENT (ADR-0030) IS SET WHERE IT IS MANAGED: an operator whose
// token is not one of its writers is told there is no ceiling and offered no
// way to set one, which Company › Budgets would only hold.
test("on a managed document only a writer is offered to set a budget", async () => {
  mount(
    "#/settings/spend?window=30d",
    <Spend />,
    {
      tokens: rollup(1),
      viewer: { ...VIEWER, config_writer: false, config_managed_by: ["gitops"] },
    },
    (store) => store.applyBudget(UNCAPPED),
  );
  await settle();
  expect(screen.getByText("No monthly budget")).toBeTruthy();
  expect(screen.queryByRole("link", { name: "Set one" })).toBeNull();
  cleanup();

  mount(
    "#/settings/spend?window=30d",
    <Spend />,
    {
      tokens: rollup(1),
      viewer: { ...VIEWER, config_writer: true, config_managed_by: ["gitops"] },
    },
    (store) => store.applyBudget(UNCAPPED),
  );
  await settle();
  expect(screen.getByRole("link", { name: "Set one" })).toBeTruthy();
});

// THE CACHE'S SHARE IS READ ÷ INPUT, and a window where nothing reported a
// cache has no figure rather than "0%".
test("the cache tile divides the engine's counts, and is absent when none were reported", async () => {
  mount("#/settings/spend?window=30d", <Spend />, {
    tokens: rollup(1000, { totals: bucket(1000, { input_tokens: 800, cache_read_tokens: 352 }) }),
    viewer: VIEWER,
  });
  await settle();
  expect(screen.getByText("44%")).toBeTruthy();
  cleanup();

  mount("#/settings/spend?window=30d", <Spend />, { tokens: rollup(1000), viewer: VIEWER });
  await settle();
  expect(screen.queryByText("of input served from prompt cache")).toBeNull();
  expect(screen.queryByText("0%")).toBeNull();
});

// THE MEDIAN TASK IS THE TRACKER'S, over the tasks FINISHED in the window, and
// on a company whose tracker is not the engine's there is no tile at all.
test("the median task tile asks the tracker, and is absent on a tracker other than Worklet", async () => {
  mount("#/settings/spend?window=30d", <Spend />, {
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

  mount("#/settings/spend?window=30d", <Spend />, {
    tokens: rollup(1),
    viewer: VIEWER,
    work_items: new QueryError("unknown_query"),
  });
  await settle();
  expect(screen.queryByText(/median tokens per finished task/)).toBeNull();
  expect(screen.queryByText("Most expensive tasks")).toBeNull();
});

// THE OVERVIEW IS EVERYBODY'S AND A TASK IS THE TRACKER'S: a reader without
// Worklet is asked nothing about tasks, and both task readings say what would
// open them rather than drawing the engine's refusal — whose callout offered
// "Sign in" to somebody already signed in. Mutation: enable the card's
// question on `ready` alone and it is asked, and the lock is never drawn.
test("a reader without Worklet is asked no task question on the overview", async () => {
  mount("#/settings/spend?window=30d", <Spend />, {
    tokens: rollup(1),
    viewer: { ...VIEWER, grants: [] },
    work_items: new QueryError("unauthorized"),
  });
  await settle();
  expect(params("work_items")).toEqual([]);
  const card = screen.getByText("Most expensive tasks").closest(".spend-card") as HTMLElement;
  expect(within(card).getByText("Needs Worklet access")).toBeTruthy();
  expect(within(card).queryByText("All")).toBeNull();
  expect(within(card).queryByText(/This answer needs|This needs access/)).toBeNull();
  expect(screen.queryByRole("button", { name: /sign in/i })).toBeNull();
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
    "#/settings/spend?window=30d",
    <Spend />,
    { tokens: rollup(4_000, { by_agent: [ceo, swe] }), viewer: VIEWER },
    (store) =>
      store.applySeats([
        {
          id: "swe",
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
      ]),
  );
  await settle();
  const rows = [...document.querySelectorAll<HTMLElement>("[data-row-index]")];
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
  mount("#/settings/spend?window=30d", <Spend />, {
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
  mount("#/settings/spend?window=30d", <Spend />, {
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

// THE CHART TAKES ITS ROW'S HEIGHT. Drawn at a fixed 12rem beside a taller
// hero, the plot stopped short and the split control stood 34 to 68px under
// the dates. The kit's chart is asked to fill its card instead: the head and
// the dates keep their heights, the plot's row takes the rest, and the plot
// is never shorter than the 12rem it was drawn at. jsdom lays nothing out, so
// what is held is what the chart was asked to draw.
test("the daily chart fills its card, and its plot is never under 12rem", async () => {
  mount("#/settings/spend?window=30d", <Spend />, {
    tokens: rollup(100),
    token_series: {
      ...series(100),
      series: [
        {
          ...bucket(100),
          at: "2026-09-01T00:00:00Z",
          window: "2026-09-01",
          days: 1,
          groups: { execute: bucket(100) },
          other: bucket(0),
        },
      ],
    },
    viewer: VIEWER,
  });
  await settle();
  const figure = document.querySelector<HTMLElement>(".spend-chart > .crewlet-stacked-columns")!;
  expect(figure.classList.contains("crewlet-stacked-columns--fill")).toBe(true);
  expect(figure.style.gridTemplateRows).toBe("auto minmax(12rem, 1fr) auto");
  const plot = figure.querySelector<HTMLElement>(".crewlet-stacked-columns__plot")!;
  expect(plot.style.height).toBe("100%");
  // The split control follows the figure in the same column, so it stands
  // right under the dates once the plot has taken the spare height.
  expect(figure.nextElementSibling?.classList.contains("spend-chart-foot")).toBe(true);
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
  mount("#/settings/spend?window=30d", <Spend />, {
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
// on the seat's agent id and carries the name it read off the chart as its
// `label`, so the legend draws the name and never the id.
test("splitting by seat labels each band with the seat's name, not its key", async () => {
  mount("#/settings/spend?window=30d&group=seat", <Spend />, {
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
          ? [{ ...bucket(100), group: "a-swe", label: "SWE", other: false, folded: 0 }]
          : [],
    }),
    viewer: VIEWER,
  });
  await settle();
  const chart = screen
    .getByRole("heading", { name: "Daily tokens by agent" })
    .closest(".spend-chart");
  expect(within(chart as HTMLElement).getAllByText("SWE").length).toBeGreaterThan(0);
  expect(within(chart as HTMLElement).queryByText("a-swe")).toBeNull();
});

// THE DIMENSIONS ARE THE CONTRACT'S SIX, which the engine gate holds against
// `tokens.Groups` — and a choice re-asks the series by that dimension.
test("the chart offers the contract's six dimensions and asks by the one chosen", async () => {
  mount("#/settings/spend?window=30d", <Spend />, {
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
  mount("#/settings/spend?window=90d", <Spend />, { tokens: rollup(1), viewer: VIEWER });
  await settle();
  const link = screen.getByRole("link", { name: "Recent turns by tokens →" });
  expect(link.getAttribute("href")).toBe("#/live/turns?sort=-tokens&window=30d");
  expect(screen.getByText("the last 30 days")).toBeTruthy();
});

// A REFUSED WINDOW HAS NO LIST TO WAIT FOR. The list's question starts where
// the window's answer does, so a refused window never asks it — and the grid
// it would have filled was a loading skeleton under the refusal for good.
test("the expensive tasks under a refused window say the refusal and settle", async () => {
  const sentence =
    "tokens: since=2026-06-01 until=2026-08-30 is 91 days, and a spend window is at most 90";
  mount(
    "#/settings/spend/tasks?window=2026-06-01T00:00:00.000Z/2026-08-31T00:00:00.000Z",
    <ExpensiveTasks />,
    {
      tokens: new QueryError("bad_params", null, sentence),
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
  mount("#/settings/spend/tasks?window=30d", <ExpensiveTasks />, {
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

// A PERSON IS A ROW OF THEIR OWN, and says why it has no turns. Two people of
// one role are two rows — keyed on the role they were one — and where a seat
// counts its turns a person's row says a person takes none, rather than a
// blank that reads as a seat that ended nothing.
test("people are their own rows, and say they take no turns", async () => {
  const person = (handle: string, total: number) => ({
    ...bucket(total),
    role: "Founder",
    handle,
    agent_id: "",
    person: true,
    by_phase: {},
  });
  mount("#/settings/spend?window=30d", <Spend />, {
    tokens: rollup(900, { by_agent: [person("maya", 600), person("ana", 300)] }),
    viewer: VIEWER,
  });
  await settle();
  const grid = screen.getByText("By agent").closest(".spend-card") as HTMLElement;
  expect(within(grid).getByText(/maya/i)).toBeTruthy();
  expect(within(grid).getByText(/ana/i)).toBeTruthy();
  expect(
    within(grid).getAllByLabelText(/Auxiliary model spend for this person/).length,
  ).toBeGreaterThan(0);
});

// THE AUXILIARY MODEL'S SPEND IS NAMED FOR WHAT IT WAS FOR, in the words the
// turn screen uses for the same records — never as background work outside
// the seats, which it is not: every call is inside its seat's row already.
test("the auxiliary card names each purpose and its provider calls", async () => {
  mount("#/settings/spend?window=30d", <Spend />, {
    tokens: rollup(500, {
      by_worker: [
        { worker: "condense_thread", ...bucket(300, { calls: 7 }) },
        { worker: "memory_filter", ...bucket(200, { calls: 1 }) },
      ],
    }),
    viewer: VIEWER,
  });
  await settle();
  const card = screen.getByText("Auxiliary model").closest(".spend-card") as HTMLElement;
  expect(within(card).getByText("condense thread")).toBeTruthy();
  expect(within(card).getByText("7 calls")).toBeTruthy();
  expect(within(card).getByText("memory filter")).toBeTruthy();
  expect(screen.queryByText("Background workers")).toBeNull();
});

// WHO SPENT IT, THEN WHAT ON. The seats' table shares its row with the model
// and team lists, stacked in the narrow track where every ranked list stands,
// and the tasks share theirs with the auxiliary model: a team list in the wide
// track ran its bar a thousand pixels from the team's name.
test("the ranked lists stand in the narrow track, beside the seats and the tasks", async () => {
  mount("#/settings/spend?window=30d", <Spend />, { tokens: rollup(1_000), viewer: VIEWER });
  await settle();
  const card = (title: string) =>
    screen.getByRole("heading", { name: title }).closest(".spend-card") as HTMLElement;
  const row = (title: string) => card(title).closest(".spend-pair");
  expect(card("By model").parentElement!.className).toBe("spend-side");
  expect(card("By team").parentElement!.className).toBe("spend-side");
  expect(row("By model")).toBe(row("By agent"));
  expect(row("Auxiliary model")).toBe(row("Most expensive tasks"));
  expect(row("Auxiliary model")).not.toBe(row("By agent"));
  // The wide card leads its row: first in the track, and first to be read.
  expect(row("By agent")!.firstElementChild).toBe(card("By agent"));
  expect(row("Most expensive tasks")!.firstElementChild).toBe(card("Most expensive tasks"));
});

// WITHOUT WORKLET THERE ARE NO TASKS TO RANK, and the auxiliary model's
// card joins the side column rather than standing alone in a row of its own.
test("where the company runs no Worklet, the auxiliary model joins the side column", async () => {
  mount(
    "#/settings/spend?window=30d",
    <Spend />,
    { tokens: rollup(1_000), viewer: VIEWER },
    (store) =>
      store.applyOrg(
        withDerived({
          timezone: "UTC",
          roles: [{ name: "CEO", handle: "ceo" }],
          services: { tracker: "jira", knowledge: "doclet", chat: "chatlet" },
        }),
      ),
  );
  await settle();
  const card = (title: string) =>
    screen.getByRole("heading", { name: title }).closest(".spend-card") as HTMLElement;
  expect(card("Auxiliary model").parentElement!.className).toBe("spend-side");
  expect(screen.queryByRole("heading", { name: "Most expensive tasks" })).toBeNull();
  expect(document.querySelectorAll(".spend-pair").length).toBe(1);
});

// ONE LABEL COLUMN FOR THE THREE RANKED LISTS. They stand in one track, and By
// model's half-row words beside the others' 128px drew two bar columns down
// the page, 37px apart at 1280 and 115px at 1920 (`.spend-bars`, screens.css).
test("every ranked list takes the one label width, so the bars start at one x", async () => {
  mount("#/settings/spend?window=30d", <Spend />, {
    tokens: rollup(1_000, {
      by_provider: [
        {
          ...bucket(1_000),
          provider_key: "default",
          models: ["claude-sonnet"],
          seats: ["swe"],
          seats_total: 1,
        },
      ],
      by_worker: [{ worker: "memory_filter", ...bucket(200, { calls: 1 }) }],
    }),
    token_series: (p: Record<string, unknown>) =>
      p.group === "unit"
        ? {
            ...series(1_000),
            group: "unit",
            by_group: [
              { ...bucket(1_000), group: "Engineering", seats: 2, other: false, folded: 0 },
            ],
          }
        : series(1_000),
    viewer: VIEWER,
  });
  await settle();
  for (const title of ["By model", "By team", "Auxiliary model"]) {
    const card = screen.getByRole("heading", { name: title }).closest(".spend-card")!;
    expect(card.querySelector("ul")?.classList.contains("spend-bars"), title).toBe(true);
  }
});

// THE TILES' ROW HOLDS ONLY THE TILES THAT SPEAK, and with neither it is empty,
// which the sheet takes out of the hero (`.spend-tiles:empty`): drawn, an
// empty row added a gap under the budget at the hero's foot.
test("the hero's tiles row holds only the tiles that have something to say", async () => {
  mount("#/settings/spend?window=30d", <Spend />, {
    tokens: rollup(1_000),
    work_items: new QueryError("unknown_query"),
    viewer: VIEWER,
  });
  await settle();
  // No cache was reported and the tracker is not the engine's: neither tile.
  expect(document.querySelector(".spend-tiles")!.childNodes).toHaveLength(0);
  cleanup();

  mount("#/settings/spend?window=30d", <Spend />, {
    tokens: rollup(1_000),
    work_items: (p: Record<string, unknown>) =>
      items([], {
        totals: p.totals
          ? [
              { key: "spend_tokens:median", column: "spend_tokens", op: "median", value: 96_000 },
              { key: "spend_tokens:count", column: "spend_tokens", op: "count", value: 14 },
            ]
          : [],
      }),
    viewer: VIEWER,
  });
  await settle();
  expect(document.querySelectorAll(".spend-tiles > .spend-tile")).toHaveLength(1);
});

// EVERY EMPTY CARD ON THE PAGE SAYS SO ONE WAY: the kit's compact empty
// state, as the agent table and the chart already did. The ranked lists and
// the task card said it as a bare sentence beside them, two treatments for
// one answer. Mutation: draw a ranked list's empty sentence again and the
// second query finds no empty state round it.
test("every empty Spend card says so with the one compact empty state", async () => {
  mount("#/settings/spend?window=30d", <Spend />, {
    tokens: rollup(1),
    work_items: items([]),
    viewer: VIEWER,
  });
  await settle();
  const task = screen.getByText("No task last changed in this window has spent tokens");
  expect(task.closest(".crewlet-empty-state")).not.toBeNull();
  const aux = screen.getByText("The auxiliary model spent no tokens in this window");
  expect(aux.closest(".crewlet-empty-state")).not.toBeNull();
});

// ONE HEAD HEIGHT ACROSS A ROW: the wide card and the side cards beside it
// stack their subtitle under the title at every width, so a row does not
// draw one head inline and its neighbour's stacked.
test("every Spend card with a subtitle stacks it under its title", async () => {
  const { readFileSync } = await import("node:fs");
  const { join } = await import("node:path");
  for (const file of [
    "ByAgent.tsx",
    "ByModel.tsx",
    "ByUnit.tsx",
    "Workers.tsx",
    "ExpensiveTasks.tsx",
  ]) {
    const source = readFileSync(join(process.cwd(), "src/routes/spend", file), "utf8");
    const subtitles = source.match(/\bsubtitle=/g)?.length ?? 0;
    expect(subtitles, file).toBeGreaterThan(0);
    expect(source.match(/subtitlePlacement="below"/g)?.length ?? 0, file).toBe(subtitles);
  }
});

// THE COMPANY'S MODEL ENTRIES ARE AN ADMIN'S. A reader who is not one is never
// offered the split by provider entry (the engine refuses it to them), a
// hand-edited `group=provider` draws the default split instead, and the by
// model card says the breakdown is shown to admins rather than that nothing
// was spent.
test("a reader who is not an admin is offered no split or list by model entry", async () => {
  mount("#/settings/spend?window=30d&group=provider", <Spend />, {
    tokens: rollup(1_000, { by_provider: [], entries_withheld: true }),
    viewer: { ...VIEWER, grants: ["worklet", "doclet"] },
  });
  await settle();
  expect(screen.queryByRole("radio", { name: "Provider" })).toBeNull();
  expect(screen.getByRole("radio", { name: "Phase" }).getAttribute("aria-checked")).toBe("true");
  expect(params("token_series").every((p) => p.group !== "provider")).toBe(true);
  expect(screen.getByText("Shown to admins")).toBeTruthy();
});
