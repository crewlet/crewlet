/**
 * The seat peek: what it reads, what it withholds, and what it lets a reader
 * do — without a refused request and within three reads.
 */

import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test } from "vitest";

import { NODE_WITHHELD, PERIOD_WORDS, SeatPeek, budgetLine, turnOrdinal } from "./SeatPeek.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { CHART_ORG } from "~/test/orgchart.ts";
import { LiveSocket, Store, type AgentRow, type OrgProjection } from "~/protocol/index.ts";
import type { BudgetWindow } from "~/protocol/types.ts";

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
    turn_id: "turn-2",
    phase: "execute",
    model: "claude-sonnet-5",
    round_num: 7,
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
  }: { projection?: OrgProjection; agents?: AgentRow[]; handle?: string } = {},
) {
  const store = new Store();
  store.applyHealth({ status: "healthy" });
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

// AN ANONYMOUS READER IS TOLD WHAT IS WITHHELD, AND ASKED FOR NOTHING IT
// WOULD BE REFUSED. `fleet` is operator-only; a read sent anyway would come
// back refused and draw a refusal where a sentence belongs.
test("an anonymous reader sees the node withheld and sends no guarded read", async () => {
  const { asked } = await mount(ANONYMOUS);
  // IN WORDS ON THE ROW, where a sighted reader reads them: the dash that
  // stood here carried the sentence only as its accessible label.
  const running = screen.getByText("Running on").nextElementSibling;
  expect(running?.textContent).toBe(NODE_WITHHELD);
  expect(running?.textContent).toBe("Shown to operators");
  expect(asked).not.toContain("fleet");
  expect(asked).not.toContain("config");
  // THE CHAIN IS PUBLIC, so it is drawn for everybody.
  expect(screen.getByText("anthropic-main")).toBeTruthy();
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

// A BUDGET IS LABELLED BY ITS OWN WINDOW, never by a period the screen
// assumed: a month's ceiling under "Budget today" is a different claim.
test("each capped window is labelled with its own period", async () => {
  await mount(ANONYMOUS);
  expect(screen.getByText("Budget today")).toBeTruthy();
  expect(screen.getByText("Budget this month")).toBeTruthy();
  expect(screen.queryByText("Budget this week")).toBeNull();
  for (const period of ["day", "week", "month"] as const) {
    expect(budgetLine(window_(period, 1, 2)).label).toBe(`Budget ${PERIOD_WORDS[period]}`);
  }
});

test("a seat nothing caps says so", async () => {
  await mount(ANONYMOUS, { agents: [{ ...SWE, budget: null } as AgentRow] });
  expect(screen.getByText(/No budget/)).toBeTruthy();
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

// NOT REPORTED IS NOT NOBODY: without the engine's derived block the peek
// cannot say who a seat reports to, and says that.
test("with no derived hierarchy the peek does not claim the seat reports to nobody", async () => {
  const { derived: _drop, ...flat } = org() as OrgProjection & { derived: unknown };
  // The founder declares her handle, which is what a document-only index can
  // address a seat by.
  await mount(ANONYMOUS, { projection: flat as OrgProjection, handle: "jane" });
  expect(screen.getByText("Not reported by this engine")).toBeTruthy();
  expect(screen.queryByText(/Nobody/)).toBeNull();
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
