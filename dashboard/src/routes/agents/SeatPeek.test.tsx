/**
 * The seat peek: what it reads, what it withholds, and what it lets a reader
 * do — without a refused request and within three reads.
 */

import { act, cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test } from "vitest";

import { SeatPeek, budgetLine, turnOrdinal } from "./SeatPeek.tsx";
import { PERIOD_WORDS } from "~/lib/budget.ts";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { CHART_ORG } from "~/test/orgchart.ts";
import { LiveSocket, Store, type AgentRow, type OrgProjection } from "~/protocol/index.ts";
import type { BudgetWindow } from "~/protocol/types.ts";
import type { EngineHealth } from "~/contract/health.ts";
import { healthFrame } from "~/test/health.ts";
import { ZERO_VERSIONS } from "~/test/liveCall.ts";

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
  location.hash = "#/agents?peek=seat%3Aswe";
});

afterEach(() => {
  cleanup();
  location.hash = "";
});

const window_ = (period: BudgetWindow["period"], used: number, limit: number): BudgetWindow => ({
  period,
  window: period === "day" ? "2031-05-01" : period === "week" ? "2031-W18" : "2031-05",
  starts_at: "2031-05-01T00:00:00Z",
  resets_at: "2031-05-02T00:00:00Z",
  used,
  limit,
  state: "ok",
});

/** SWE, working ENG-412 on its seventh round of twenty-five, with a model
 *  chain and three tool sources on the public projection. */
function org(): OrgProjection {
  const copy = structuredClone(CHART_ORG) as OrgProjection & {
    units: { children?: { roles?: Record<string, unknown>[] }[] }[];
  };
  const swe = copy.units[1]!.children![0]!.roles![0]!;
  swe.goal = "Keep the scheduler reliable.";
  swe.llm = { execute: ["anthropic-main", "backup"] };
  swe.tool_sources = ["builtin", "mcp:gitlab", "mcp:slack", "mcp:sentry"];
  return copy;
}

const SWE: AgentRow = {
  id: "SWE",
  role: "SWE",
  handle: "swe",
  activity: "working",
  turn: {
    turn_id: "turn-2",
    started_at: "2031-05-01T08:00:00Z",
    stage: "running",
    work_item: { key: "ENG-412" },
  },
  live_call: {
    versions: ZERO_VERSIONS,
    turn_id: "turn-2",
    phase: "execute",
    model: "claude-sonnet-5",
    // THE ENGINE'S TWO COUNTERS: the round in flight, zero-based, and the
    // rounds that have come back, one-based — the seventh round is running.
    round_num: 6,
    rounds_used: 6,
    max_rounds: 25,
    work_item: { key: "ENG-412" },
    in_progress: true,
    updated_at: "2031-05-01T08:06:00Z",
  },
  budget: { windows: [window_("day", 630_000, 1_000_000), window_("month", 2_000_000, 9_000_000)] },
} as unknown as AgentRow;

async function mount(
  viewer: Record<string, unknown>,
  {
    projection = org(),
    agents = [SWE],
    handle = "swe",
    health = healthFrame(),
  }: {
    projection?: OrgProjection;
    agents?: AgentRow[];
    handle?: string;
    health?: EngineHealth;
  } = {},
) {
  const store = new Store();
  store.applyHealth(health);
  store.applyOrg(projection);
  store.applyAgents(agents);
  const socket = new LiveSocket(store);
  const asked: string[] = [];
  (socket as unknown as { query: (what: string, p?: unknown) => Promise<unknown> }).query = (
    what,
  ) => {
    asked.push(what);
    if (what === "viewer") return Promise.resolve(viewer);
    if (what === "fleet") {
      return Promise.resolve({
        nodes: [],
        seats: [{ handle: "swe", node: "node-2", acquired_at: "2031-05-01T08:02:00Z" }],
        duties: [],
      });
    }
    if (what === "work_workload") {
      return Promise.resolve({ rows: [{ handle: "swe", open: 4, blocked: 1, overdue: 0 }] });
    }
    if (what === "work_item_turns") {
      return Promise.resolve({ key: "ENG-412", turns: [{ turn_id: "turn-1", ordinal: 1 }] });
    }
    return Promise.reject(new Error(`the peek asked for ${what}`));
  };
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <ViewerProvider>
        <Router>
          <SeatPeek handle={handle} />
        </Router>
      </ViewerProvider>
    </ClientContext.Provider>,
  );
  await act(async () => {
    for (let i = 0; i < 8; i++) await Promise.resolve();
  });
  return { asked: asked.filter((w) => w !== "viewer") };
}

const ANONYMOUS = { operator_id: "", acts: [] };
const OPERATOR = {
  operator_id: "ops",
  operator: true,
  handle: "jane",
  name: "Jane Founder",
  acts: ["create_work_item"],
};

// AN ANONYMOUS READER IS TOLD WHAT THE PUBLIC PUSH SAYS, AND ASKED FOR
// NOTHING IT WOULD BE REFUSED. `fleet` is operator-only; a read sent anyway
// would come back refused and draw a refusal where a sentence belongs. But
// `/health` is public, so the node's own name and the seats it holds are
// already on this reader's screen — and the profile's Setup card says them.
// The peek said "Shown to operators" over the same fact.
test("an anonymous reader sees what the health push says of the node, and sends no guarded read", async () => {
  const { asked } = await mount(ANONYMOUS, {
    health: healthFrame({ node: "node-1", seats: ["swe"] }),
  });
  // IN WORDS ON THE ROW, where a sighted reader reads them.
  const running = screen.getByText("Running on").nextElementSibling;
  expect(running?.textContent).toBe("this node · node-1");
  expect(asked).not.toContain("fleet");
  expect(asked).not.toContain("config");
  // THE CHAIN IS PUBLIC, so it is drawn for everybody.
  expect(screen.getByText("anthropic-main")).toBeTruthy();
});

// NEVER WHICH PEER: that is the fleet read's, and the push does not say it.
test("an anonymous reader is told a seat another node holds is another node's", async () => {
  await mount(ANONYMOUS, { health: healthFrame({ node: "node-1", seats: ["cto"] }) });
  expect(screen.getByText("Running on").nextElementSibling?.textContent).toBe("another node");
});

test("an operator sees the node that holds the seat and since when", async () => {
  const { asked } = await mount(OPERATOR);
  expect(asked).toContain("fleet");
  expect(screen.getByText(/node-2/)).toBeTruthy();
});

// AT MOST THREE READS PER OPEN, in the heaviest case: an operator, a seat
// working on a task.
test("the peek asks at most three questions", async () => {
  const { asked } = await mount(OPERATOR);
  expect(new Set(asked).size).toBeLessThanOrEqual(3);
  expect(asked).not.toContain("config");
});

// THE STATE CARD: the engine's line, and which turn on the task and which
// round of how many.
test("the state card names the turn on the task and the round", async () => {
  await mount(OPERATOR);
  expect(screen.getByText(/Executing ENG-412/)).toBeTruthy();
  expect(screen.getByText("Turn 2 · round 7 of 25")).toBeTruthy();
  expect(screen.getByText(/serving now: claude-sonnet-5/)).toBeTruthy();
});

// A PEEK IS ADDRESSED AS THE SEAT'S PAGE IS, BY THE HANDLE ALONE. The role
// name is only the first seat called that, and the handle in another case is
// another string: the peek names what it was given rather than drawing the
// seat it resembles.
test("the peek resolves a handle and nothing else", async () => {
  for (const handle of ["SWE", "Swe"]) {
    await mount(ANONYMOUS, { handle });
    expect(screen.getByText(`No seat called “${handle}”`), handle).toBeTruthy();
    cleanup();
  }
  await mount(ANONYMOUS);
  expect(screen.queryByText(/No seat called/)).toBeNull();
  expect(screen.getByText("Keep the scheduler reliable.")).toBeTruthy();
});

// THE CHART TO THE TURN IN TWO PRESSES: the card opens this peek, and its state
// card watches the running turn — its Transcript, the phase it is on open. A
// seat that is not working has no such link, and neither has one whose turn
// has published no id yet.
test("the state card watches the running turn, and only a running one", async () => {
  await mount(OPERATOR);
  const state = screen.getByRole("region", { name: "Doing now" });
  expect(within(state).getByRole("link", { name: "Watch live" }).getAttribute("href")).toBe(
    "#/live/turns/turn-2?tab=transcript",
  );
  cleanup();
  await mount(OPERATOR, {
    agents: [{ ...SWE, activity: "idle", turn: null, live_call: null } as AgentRow],
  });
  expect(screen.queryByRole("link", { name: "Watch live" })).toBeNull();
  cleanup();
  await mount(OPERATOR, { agents: [{ ...SWE, turn: null, live_call: null } as AgentRow] });
  expect(screen.queryByRole("link", { name: "Watch live" })).toBeNull();
});

// A BUDGET IS LABELLED BY ITS OWN WINDOW, never by a period the screen
// assumed: a month's ceiling under "Budget today" is a different claim.
test("each capped window is labelled with its own period", async () => {
  await mount(ANONYMOUS);
  expect(screen.getByText("Budget today")).toBeTruthy();
  expect(screen.getByText("Budget this month")).toBeTruthy();
  expect(screen.queryByText("Budget this week")).toBeNull();
  for (const period of ["day", "week", "month"] as const) {
    expect(budgetLine(window_(period, 1, 2), "UTC").label).toBe(`Budget ${PERIOD_WORDS[period]}`);
  }
});

// WHEN IT RESETS IS THE COMPANY'S DATE, as the Budgets screen captions it. A
// week cut in Tokyo turns over at Monday's midnight there — Sunday afternoon
// in UTC — and the figure printed that instant on the reader's clock, to the
// minute.
test("a window resets on the company's calendar", () => {
  const week = { ...window_("week", 630_000, 1_000_000), resets_at: "2031-05-04T15:00:00Z" };
  expect(budgetLine(week, "Asia/Tokyo").figure).toBe("630k of 1M · resets May 5");
  expect(budgetLine(window_("day", 1, 2), "Asia/Tokyo").figure).toBe(
    "1 of 2 · resets at midnight (Asia/Tokyo)",
  );
});

test("a seat nothing caps says so", async () => {
  await mount(ANONYMOUS, { agents: [{ ...SWE, budget: null } as AgentRow] });
  expect(screen.getByText("No budget — nothing caps this seat's tokens")).toBeTruthy();
});

// THE COMPANY'S CEILINGS BIND EVERY SEAT: a seat with none of its own under a
// company that caps a window is not one "nothing caps".
test("a seat with no budget of its own under a capped company names the company's ceiling", async () => {
  const projection = org();
  projection.token_budget = { day: 60_000_000 };
  await mount(ANONYMOUS, { projection, agents: [{ ...SWE, budget: null } as AgentRow] });
  expect(screen.getByText("No seat budget — the company's 60M/day applies")).toBeTruthy();
  expect(screen.queryByText(/nothing caps/)).toBeNull();
});

// MESSAGE IS NEVER HIDDEN: it is held with the reason for every reader who
// cannot act, and pressable for one who can.
test.each([
  ["anonymous", ANONYMOUS, WRITE_REASONS.anonymous],
  ["unbound", { operator_id: "ci", acts: [] }, WRITE_REASONS.unbound],
  ["not served", { ...OPERATOR, acts: [] }, WRITE_REASONS.not_served],
])("Message is held with the reason for a reader who is %s", async (_, viewer, reason) => {
  await mount(viewer);
  const message = screen.getByRole("button", { name: /Message/ });
  expect(message.getAttribute("aria-disabled")).toBe("true");
  expect(message.getAttribute("title")).toBe(reason);
});

test("Message is pressable for a reader who can file", async () => {
  await mount(OPERATOR);
  const message = screen.getByRole("button", { name: /Message/ });
  expect(message.getAttribute("aria-disabled")).not.toBe("true");
});

// A SEAT THE ENGINE GAVE NO MANAGER is the top of the chart, and says so
// rather than leaving the line blank.
test("a seat with no manager reports to nobody, at the top of the chart", async () => {
  await mount(ANONYMOUS, { handle: "jane" });
  expect(screen.getByText("Nobody — the top of the chart")).toBeTruthy();
});

test("the tools name their source, and more than three fold into a count", async () => {
  await mount(ANONYMOUS);
  expect(screen.getByText("built-in")).toBeTruthy();
  expect(screen.getByText("gitlab")).toBeTruthy();
  expect(screen.getByText("+1")).toBeTruthy();
});

// WHICH TURN THIS IS: the running turn is written when it ends, so it is the
// one after the newest recorded — unless a segment of it is already recorded.
test("the turn ordinal follows the task's own turn list", () => {
  expect(turnOrdinal("t3", { turn_id: "t2", ordinal: 2 }, true)).toBe(3);
  expect(turnOrdinal("t2", { turn_id: "t2", ordinal: 2 }, true)).toBe(2);
  expect(turnOrdinal("t1", undefined, true)).toBe(1);
  expect(turnOrdinal("t1", undefined, false)).toBeNull();
});
