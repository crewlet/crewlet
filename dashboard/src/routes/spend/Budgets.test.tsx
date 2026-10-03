/**
 * Spend › Budgets, rendered over wire-shaped answers, with the two surfaces
 * behind the ceiling editor — the configuration and the org chart — stubbed
 * at `fetch`.
 *
 * What these hold, one case each: every scope states all three windows; a
 * bar is the engine's `state` as served, never a fraction judged here; each
 * scope is written where it lives (a checked merge patch of the settings for
 * the company, the seat's chart content write of its runtime half for a
 * seat); a company change that introduces a warning waits for a second press;
 * a seat write that lost a race offers a reload; a 0 is refused before any
 * request; a reader who may not change the configuration sees every ceiling
 * with the reason, and no screen offers a reset.
 */

import { act, cleanup, fireEvent, render, screen, within } from "~/test/inCase.ts";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { Budgets } from "./Budgets.tsx";
import { FrameReadings } from "~/app/Shell.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { BudgetWindow, BudgetsAnswer } from "~/protocol/index.ts";

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
  vi.unstubAllGlobals();
  location.hash = "";
});

function win(period: BudgetWindow["period"], extra: Partial<BudgetWindow> = {}): BudgetWindow {
  const spans = {
    day: ["2026-09-29", "2026-09-29T00:00:00Z", "2026-09-30T00:00:00Z"],
    week: ["2026-W40", "2026-09-28T00:00:00Z", "2026-10-05T00:00:00Z"],
    month: ["2026-09", "2026-09-01T00:00:00Z", "2026-10-01T00:00:00Z"],
  } as const;
  const [window, starts_at, resets_at] = spans[period];
  return { period, window, starts_at, resets_at, used: 0, state: "ok", ...extra };
}

function answer(extra: Partial<BudgetsAnswer> = {}): BudgetsAnswer {
  return {
    timezone: "UTC",
    durable: true,
    near_fraction: 0.9,
    org: {
      windows: [
        win("day", { used: 2_000_000, limit: 40_000_000 }),
        win("week", { used: 9_000_000 }),
        win("month", { used: 30_000_000, limit: 80_000_000 }),
      ],
    },
    seats: [
      {
        agent_id: "a-pm",
        role: "PM",
        handle: "pm",
        windows: [
          // 95% of the ceiling and served `ok`: drawn ok, because the engine
          // said so. 50% and served `near`: drawn near.
          win("day", { used: 950, limit: 1000, state: "ok" }),
          win("week", { used: 500, limit: 1000, state: "near" }),
          win("month", {
            used: 900,
            limit: 1000,
            state: "refusing",
            refused_at: "2026-09-29T09:00:00Z",
          }),
        ],
      },
    ],
    ...extra,
  };
}

// The viewer answer as the engine sends it. The operator holds the company's
// grants; the reader holds the state grant alone.
const OPERATOR = {
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

let budgetAsks = 0;

function mount(budgets: BudgetsAnswer, viewer: Record<string, unknown> = OPERATOR) {
  location.hash = "#/spend/budgets";
  budgetAsks = 0;
  const store = new Store();
  store.applyHealth({ status: "healthy", applied_epoch: 4 });
  store.setConnected(true);
  store.applyOrg({ timezone: "UTC", roles: [{ name: "PM", handle: "pm" }] });
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) => {
    if (what === "budgets") {
      budgetAsks += 1;
      return Promise.resolve(budgets);
    }
    if (what === "viewer") return Promise.resolve(viewer);
    return new Promise(() => {});
  };
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <ToastProvider>
        <LayerHost>
          <FrameReadings>
            <Router>
              <Budgets />
            </Router>
          </FrameReadings>
        </LayerHost>
      </ToastProvider>
    </ClientContext.Provider>,
  );
  return store;
}

async function settle() {
  await act(async () => {
    for (let i = 0; i < 12; i++) await Promise.resolve();
  });
}

const json = (payload: unknown, status: number, headers: Record<string, string> = {}) =>
  new Response(JSON.stringify(payload), {
    status,
    headers: { "Content-Type": "application/json", ...headers },
  });

interface Call {
  method: string;
  url: string;
  body: unknown;
  ifMatch: string | null;
  key: string | null;
}

/** Stub the engine's two surfaces; `respond` answers each request in order. */
function stubConfig(respond: (call: Call) => Response): Call[] {
  const calls: Call[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit = {}) => {
      const headers = new Headers(init.headers);
      const call: Call = {
        method: init.method ?? "GET",
        url: String(url),
        body: init.body ? JSON.parse(init.body as string) : undefined,
        ifMatch: headers.get("If-Match"),
        key: headers.get("Idempotency-Key"),
      };
      calls.push(call);
      return respond(call);
    }),
  );
  return calls;
}

/** The PM seat as `GET /chart/seats/pm?runtime=true` serves it. */
const PM_SEAT = {
  seat: {
    handle: "pm",
    kind: "agent",
    unit: "product",
    name: "PM",
    goal: "ship the roadmap",
    runtime: { llm: "zulu", token_budget: { month: 1000 } },
  },
  manages: null,
  answer: { level: "consistent_prefix", position: "CREWLET_CHART_LOG@1:4" },
  runtime: true,
};
/** The company's settings as `GET /config` serves them. */
const COMPANY_DOC = { name: "Acme", token_budget: { day: 40_000_000 } };
const valid = (warnings: unknown[] = []) =>
  json({ valid: true, base_revision_id: "r1", warnings, derived: null }, 200);
const saved = () => json({ revision_id: "r2", epoch: 5, warnings: [], derived: null }, 201);

/** Open the editor for one ceiling and type a value into it. */
async function edit(name: RegExp, value: string) {
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name }));
  });
  const field = screen.getByRole("textbox");
  fireEvent.change(field, { target: { value } });
  await act(async () => {
    fireEvent.submit(field.closest("form")!);
  });
  await settle();
}

test("every scope states its three windows, the company's first", async () => {
  mount(answer());
  await settle();
  const company = screen.getByRole("region", { name: "The company today" });
  expect(within(company).getByText("2M")).toBeTruthy();
  expect(within(company).getByText("of 40M")).toBeTruthy();
  // The week has no ceiling, and says so rather than drawing an empty bar.
  const week = screen.getByRole("region", { name: "The company this week" });
  expect(within(week).getByText("No ceiling")).toBeTruthy();
  expect(within(week).queryByRole("meter")).toBeNull();
  expect(screen.getByRole("region", { name: "The company this month" })).toBeTruthy();
  for (const heading of ["Today", "This week", "This month"]) {
    expect(screen.getAllByText(heading).length).toBeGreaterThan(0);
  }
  expect(screen.getByText(/refusing charges since/i)).toBeTruthy();
});

// THE ENGINE'S STATE, AS SERVED. 95% served `ok` is drawn ok and 50% served
// `near` is drawn near: a bar that re-derived its tone from the fill would
// get both of these the other way round.
test("each bar is the state the engine served, whatever its fill", async () => {
  mount(answer());
  await settle();
  const tone = (label: string) =>
    screen.getByRole("meter", { name: label }).closest("[data-tone]")?.getAttribute("data-tone") ??
    screen
      .getByRole("meter", { name: label })
      .querySelector("[data-tone]")
      ?.getAttribute("data-tone");
  expect(tone("PM's daily token budget")).not.toBe("warning");
  expect(tone("PM's daily token budget")).not.toBe("danger");
  expect(tone("PM's weekly token budget")).toBe("warning");
  expect(tone("PM's monthly token budget")).toBe("danger");
  // And the threshold the page states is the one the engine served.
  expect(screen.getByText(/near at 90% of its ceiling/)).toBeTruthy();
});

// A SEAT'S CEILING IS ITS CHART RUNTIME: read with the runtime half at the
// moment of the save, sent back as the seat's content with its ceiling
// changed, under an operation a retry can resend. There is no check: the
// chart has no dry run, and refuses a ceiling it will not hold outright.
test("a seat's ceiling is written as the seat's chart content, and applies on the org push", async () => {
  const calls = stubConfig((call) => {
    if (call.method === "GET") return json(PM_SEAT, 200);
    return json({ outcome: "applied", position: "CREWLET_CHART_LOG@1:5", op_id: "k" }, 200);
  });
  const store = mount(answer());
  await settle();
  await edit(/Change PM's daily token ceiling/, "2.5M");

  const writes = calls.map((c) => `${c.method} ${c.url.replace(/^https?:\/\/[^/]+/, "")}`);
  expect(writes).toEqual(["GET /chart/seats/pm?runtime=true", "PATCH /chart/seats/pm"]);
  const save = calls[1]!;
  expect(save.key).toBeTruthy();
  expect(save.body).toEqual({
    unit: "product",
    name: "PM",
    email: "",
    backstory: "",
    goal: "ship the roadmap",
    responsibilities: [],
    behavioral_guidelines: [],
    project: "",
    space: "",
    runtime: { llm: "zulu", token_budget: { month: 1000, day: 2_500_000 } },
  });
  // Saved, and applying until this node pushes its org again.
  expect(screen.getByText("of 2.5M")).toBeTruthy();
  expect(screen.getByText("applying…")).toBeTruthy();
  const before = budgetAsks;
  await act(async () => {
    store.applyOrg({ timezone: "UTC", roles: [{ name: "PM", handle: "pm" }] });
  });
  await settle();
  expect(budgetAsks).toBeGreaterThan(before);
});

// THE COMPANY'S CEILINGS ARE A MERGE PATCH naming only the window changed —
// and an emptied field removes the key, which is the only "no ceiling".
test("the company's ceiling is a merge patch of the one window, and empty removes it", async () => {
  const calls = stubConfig((call) => {
    if (call.method === "GET") return json(COMPANY_DOC, 200, { ETag: '"r1"' });
    if (call.url.includes("dry_run=true")) return valid();
    return saved();
  });
  mount(answer());
  await settle();
  await edit(/Change the company's daily token ceiling/, "");
  const save = calls.at(-1)!;
  expect(save.method).toBe("PATCH");
  expect(save.url).not.toContain("dry_run");
  expect(save.ifMatch).toBe('"r1"');
  expect(save.body).toEqual({
    token_budget: { day: null },
    _summary: "Remove the company's daily token ceiling (was 40M)",
  });
});

// A WARNING THE CHANGE INTRODUCES STOPS IT. The check answers every warning
// the company raises; only the new one is shown, and nothing is saved until
// the person presses again.
test("a company change that introduces a warning waits for Save anyway", async () => {
  const fresh = {
    kind: "advisory",
    path: "token_budget.week",
    message: "the company's token_budget.week is below its token_budget.day",
  };
  const old = { kind: "advisory", path: "scheduler.default", message: "an old one" };
  let checks = 0;
  const calls = stubConfig((call) => {
    if (call.method === "GET") return json(COMPANY_DOC, 200, { ETag: '"r1"' });
    if (call.url.includes("dry_run=true")) {
      checks += 1;
      return valid(checks === 1 ? [old] : [old, fresh]);
    }
    return saved();
  });
  mount(answer());
  await settle();
  await edit(/Change the company's daily token ceiling/, "90M");
  expect(screen.getByText(/below its token_budget.day/)).toBeTruthy();
  expect(screen.queryByText("an old one")).toBeNull();
  const stored = () => calls.filter((c) => c.method === "PATCH" && !c.url.includes("dry_run"));
  expect(stored()).toHaveLength(0);

  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Save anyway" }));
  });
  await settle();
  expect(stored()).toHaveLength(1);
});

// A SEAT WRITE THAT LOST A RACE IS NOT AN OVERWRITE: a colleague's write to
// the same seat landed first, nothing was saved, and Reload re-reads.
test("a seat write that lost a race saves nothing and offers a reload", async () => {
  stubConfig((call) => {
    if (call.method === "GET") return json(PM_SEAT, 200);
    return json({ error: "stale", detail: "the seat moved under this write" }, 409);
  });
  mount(answer());
  await settle();
  await edit(/Change PM's daily token ceiling/, "5M");
  expect(
    screen.getByText(/changed this seat at the same moment, so nothing was saved/),
  ).toBeTruthy();
  const before = budgetAsks;
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Reload" }));
  });
  await settle();
  expect(budgetAsks).toBeGreaterThan(before);
});

// A 0 IS REFUSED BEFORE ANY REQUEST, in the engine's terms: empty is the only
// way a window has no ceiling.
test("a ceiling of 0 is refused before anything is sent", async () => {
  const calls = stubConfig(() => json({}, 500));
  mount(answer());
  await settle();
  await edit(/Change PM's daily token ceiling/, "0");
  expect(
    screen.getByText("A ceiling of 0 is refused: leave it empty for no daily ceiling."),
  ).toBeTruthy();
  expect(calls).toHaveLength(0);
});

// NEVER HIDDEN: a reader without `config:write` sees every ceiling and every
// pencil, disabled with the reason, and the reason once above.
test("a reader who cannot change the configuration sees why, on every ceiling", async () => {
  mount(answer(), READER);
  await settle();
  const pencils = screen.getAllByRole("button", { name: /token ceiling/ });
  expect(pencils.length).toBe(6);
  for (const pencil of pencils) expect(pencil.getAttribute("aria-disabled")).toBe("true");
  expect(screen.getAllByText(/takes the config:write grant/).length).toBeGreaterThan(0);
});

// UNREADABLE IS NOT ZERO.
test("a counter nobody could read says so, and draws no figures", async () => {
  mount(answer({ durable: false, org: { windows: [] }, seats: [] }));
  await settle();
  expect(screen.getByText(/could not be READ/)).toBeTruthy();
  expect(screen.queryByRole("meter")).toBeNull();
});

// THERE IS NO RESET, on the screen or behind it: a window's `used` is what it
// spent, and room is made by raising the ceiling or by the window turning over.
test("no control resets a budget", async () => {
  mount(answer());
  await settle();
  expect(screen.queryByRole("button", { name: /reset/i })).toBeNull();
  for (const file of [
    "./Budgets.tsx",
    "../../components/budgetWrite.tsx",
    "../../lib/useCeilingWrite.ts",
  ]) {
    const source = readFileSync(resolve(__dirname, file), "utf8");
    expect(source, file).not.toMatch(/budgets\/reset|resetBudget|Reset budget/i);
  }
});
