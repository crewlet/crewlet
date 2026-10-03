/**
 * Spend › Budgets, rendered over wire-shaped answers, with the configuration
 * surface behind the ceiling editor stubbed at `fetch`.
 *
 * What these hold, one case each: every scope states all three windows; a
 * bar is the engine's `state` as served, never a fraction judged here; a
 * ceiling change is CHECKED before it is saved, and each scope is written the
 * way its document holds it (a merge patch for the company, the seat itself
 * for a seat); a change that introduces a warning waits for a second press; a
 * conflict offers a reload; a 0 is refused before any request; a reader who
 * may not change the configuration sees every ceiling with the reason, and no
 * screen offers a reset.
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
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

const OPERATOR = {
  operator_id: "U0FOUNDER",
  operator: true,
  handle: "jane",
  name: "Jane",
  kind: "human",
};
const READER = { operator_id: "", operator: false, handle: "", name: "", kind: "" };

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
}

/** Stub the configuration surface; `respond` answers each request in order. */
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
      };
      calls.push(call);
      return respond(call);
    }),
  );
  return calls;
}

const PM_SEAT = { name: "PM", handle: "pm", llm: "zulu", token_budget: { month: 1000 } };
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

// A CHECK, THEN THE SAVE — and a seat is written as the seat, read at the
// moment of the save and sent back whole with its ceiling changed, on the
// revision it was read from.
test("a seat's ceiling is checked before it is saved, as the seat itself", async () => {
  const calls = stubConfig((call) => {
    if (call.method === "GET") return json(PM_SEAT, 200, { ETag: '"r1"' });
    if (call.url.includes("dry_run=true")) return valid();
    return saved();
  });
  mount(answer());
  await settle();
  await edit(/Change PM's daily token ceiling/, "2.5M");

  const writes = calls.map((c) => `${c.method} ${c.url.replace(/^.*\/config/, "/config")}`);
  expect(writes).toEqual([
    "GET /config/roles/pm",
    "PUT /config/roles/pm?dry_run=true",
    "PUT /config/roles/pm?dry_run=true",
    "PUT /config/roles/pm",
  ]);
  const save = calls[3]!;
  expect(save.ifMatch).toBe('"r1"');
  expect(save.body).toMatchObject({
    name: "PM",
    llm: "zulu",
    token_budget: { month: 1000, day: 2_500_000 },
    _summary: "Set PM's daily token ceiling to 2.5M",
  });
  // The check that preceded it carried the same seat and no summary.
  expect(calls[2]!.body).toEqual({ ...PM_SEAT, token_budget: { month: 1000, day: 2_500_000 } });
  // Saved, and applying until this node reports the epoch.
  expect(screen.getByText("of 2.5M")).toBeTruthy();
  expect(screen.getByText("applying…")).toBeTruthy();
});

// THE COMPANY'S CEILINGS ARE A MERGE PATCH naming only the window changed —
// and an emptied field removes the key, which is the only "no ceiling".
test("the company's ceiling is a merge patch of the one window, and empty removes it", async () => {
  const calls = stubConfig((call) => {
    if (call.method === "GET")
      return json({ name: "Acme", token_budget: { day: 40_000_000 } }, 200, { ETag: '"r1"' });
    if (call.url.includes("dry_run=true")) return valid();
    return saved();
  });
  mount(answer());
  await settle();
  await edit(/Change the company's daily token ceiling/, "");
  const save = calls.at(-1)!;
  expect(save.method).toBe("PATCH");
  expect(save.url).not.toContain("dry_run");
  expect(save.body).toEqual({
    token_budget: { day: null },
    _summary: "Remove the company's daily token ceiling (was 40M)",
  });
});

// A WARNING THE CHANGE INTRODUCES STOPS IT. The check answers every warning
// the company raises; only the new one is shown, and nothing is saved until
// the person presses again.
test("a change that introduces a warning waits for Save anyway", async () => {
  const idle = {
    kind: "advisory",
    path: "roles[0].token_budget.day",
    seat: "pm",
    message: "seat \"pm\"'s token_budget.day is at or above the company's",
  };
  const old = {
    kind: "dangling_reference",
    path: "roles[3].manages[0]",
    seat: "x",
    message: "an old one",
  };
  let checks = 0;
  const calls = stubConfig((call) => {
    if (call.method === "GET") return json(PM_SEAT, 200, { ETag: '"r1"' });
    if (call.url.includes("dry_run=true")) {
      checks += 1;
      return valid(checks === 1 ? [old] : [old, idle]);
    }
    return saved();
  });
  mount(answer());
  await settle();
  await edit(/Change PM's daily token ceiling/, "90M");
  expect(screen.getByText(/at or above the company's/)).toBeTruthy();
  expect(screen.queryByText("an old one")).toBeNull();
  expect(calls.some((c) => c.method === "PUT" && !c.url.includes("dry_run"))).toBe(false);

  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Save anyway" }));
  });
  await settle();
  expect(calls.filter((c) => c.method === "PUT" && !c.url.includes("dry_run"))).toHaveLength(1);
});

// A CONFLICT IS NOT AN OVERWRITE: nothing was saved, and Reload re-reads.
test("a conflict saves nothing and offers a reload", async () => {
  stubConfig((call) => {
    if (call.method === "GET") return json(PM_SEAT, 200, { ETag: '"r1"' });
    if (call.url.includes("dry_run=true")) return valid();
    return json({ error: "revision_advanced", current_revision_id: "r9" }, 409);
  });
  mount(answer());
  await settle();
  await edit(/Change PM's daily token ceiling/, "5M");
  expect(screen.getByText(/changed since this was read, so nothing was saved/)).toBeTruthy();
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

// NEVER HIDDEN: a reader without an operator credential sees every ceiling
// and every pencil, disabled with the reason, and the reason once above.
test("a reader who cannot change the configuration sees why, on every ceiling", async () => {
  mount(answer(), READER);
  await settle();
  const pencils = screen.getAllByRole("button", { name: /token ceiling/ });
  expect(pencils.length).toBe(6);
  for (const pencil of pencils) expect(pencil.getAttribute("aria-disabled")).toBe("true");
  expect(screen.getAllByText(/operator's API token/).length).toBeGreaterThan(0);
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
