/**
 * Company › Budgets, rendered over wire-shaped answers, with the configuration
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
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { Budgets } from "./Budgets.tsx";
import { FrameReadings } from "~/app/Shell.tsx";
import { Router } from "~/app/router.tsx";
import { arriveAgain } from "~/lib/arrival.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { CONFIG_WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { BudgetWindow, BudgetsAnswer } from "~/protocol/index.ts";
import { healthFrame } from "~/test/health.ts";
import { withDerived } from "~/test/org.ts";
import {
  applyPageAction,
  pageSnapshot,
  resetPageContextForTest,
  setFramePage,
} from "~/app/assistant/context.ts";
import { ENGINE_HEADERS } from "~/test/engineAnswer.ts";

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
  resetPageContextForTest();
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
    assistant: {
      counted: true,
      windows: [
        win("day", { used: 180_000, limit: 200_000 }),
        win("week", { used: 400_000 }),
        win("month", { used: 900_000 }),
      ],
    },
    ...extra,
  };
}

const OPERATOR = {
  login: "U0FOUNDER",
  grants: ["admin", "worklet", "doclet"],
  handle: "jane",
  owner: "jane",
  name: "Jane",
  kind: "human",
};
const READER = { login: "", grants: [], handle: "", owner: "", name: "", kind: "" };

let budgetAsks = 0;

function mount(
  budgets: BudgetsAnswer,
  viewer: Record<string, unknown> = OPERATOR,
  hash = "#/company/budgets",
) {
  location.hash = hash;
  budgetAsks = 0;
  const store = new Store();
  store.applyHealth(healthFrame({ applied_epoch: 4 }));
  store.setConnected(true);
  store.applyOrg(withDerived({ timezone: "UTC", roles: [{ name: "PM", handle: "pm" }] }));
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
    headers: { ...ENGINE_HEADERS, "Content-Type": "application/json", ...headers },
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
  expect(screen.getByText(/last refused a call/i)).toBeTruthy();
});

// THE SEATS' TABLE IS ITS CARD'S BODY: a table in a bordered box of its own
// inside the card drew two edges a pixel apart (styles/frame.test.ts holds
// the flush form and the one rule under the card's header).
test("the seats' table runs to its card's edges", async () => {
  mount(answer());
  await settle();
  const seats = screen.getByText("Agent seats").closest(".crewlet-card")!;
  expect(seats.querySelector(".crewlet-data-grid")!.hasAttribute("data-flush")).toBe(true);
});

// ONE SET OF COLUMNS: the company's windows stand over the seats' columns,
// because the row and the table are handed the same tracks. Three tiles of
// their own started at x positions no seat column shared.
test("the company's row and the seats' table share their tracks", async () => {
  mount(answer());
  await settle();
  const row = document.querySelector<HTMLElement>(".budget-row")!;
  const table = screen
    .getByText("Agent seats")
    .closest(".crewlet-card")!
    .querySelector<HTMLElement>(".crewlet-data-grid")!;
  expect(row.style.getPropertyValue("--budget-tracks")).toBe(table.style.gridTemplateColumns);
  expect(row.children).toHaveLength(4);
  // The resets are said once, in the company row, and not again over the seats.
  expect(screen.getAllByText(/Resets today at midnight/)).toHaveLength(1);
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
  // And the zone the page states is the one the engine counts the windows in,
  // said once, in the company row's first track, with when each turns over.
  expect(
    screen.getByText(
      /^Every seat together, on the company clock \(UTC\)\. Resets today at midnight/,
    ),
  ).toBeTruthy();
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

describe("a saved ceiling, followed by this node", () => {
  const PM_DAY = /Change PM's daily token ceiling/;
  /** The PM's daily ceiling, its figure and its mark. */
  const cell = () => screen.getByRole("button", { name: PM_DAY }).closest<HTMLElement>(".ceiling")!;

  /** Saves 2.5M as PM's daily ceiling: revision r2, activated at epoch 5. */
  async function saveDay() {
    stubConfig((call) => {
      if (call.method === "GET") return json(PM_SEAT, 200, { ETag: '"r1"' });
      if (call.url.includes("dry_run=true")) return valid();
      return saved();
    });
    const store = mount(answer());
    await settle();
    await edit(PM_DAY, "2.5M");
    expect(within(cell()).getByText("of 2.5M")).toBeTruthy();
    expect(within(cell()).getByText("applying…")).toBeTruthy();
    return store;
  }

  // A REFUSAL LEAVES THE APPLIED EPOCH WHERE IT WAS, so read alone it said
  // "applying…" beside the saved ceiling for the life of the page while the
  // gate went on enforcing the one before. Mutation: follow the save by the
  // applied epoch alone and the cell keeps "of 2.5M" and "applying…".
  test("refused by this node, it draws the ceiling still enforced and says so", async () => {
    const store = await saveDay();
    const asks = budgetAsks;
    act(() =>
      store.applyHealth(
        healthFrame({
          applied_epoch: 4,
          config_attempt: { epoch: 5, revision_id: "r2", status: "error" },
        }),
      ),
    );
    await settle();
    expect(within(cell()).getByText("of 1,000")).toBeTruthy();
    expect(within(cell()).getByText("refused by this node")).toBeTruthy();
    expect(within(cell()).queryByText("applying…")).toBeNull();
    // Nothing moved here, so nothing is read again.
    expect(budgetAsks).toBe(asks);
  });

  // A DROPPED SOCKET CLEARS THE HEALTH SLICE, and says nothing about the
  // save: read as this node's standing, the missing push was "applying", and
  // the refused ceiling went back to the saved figure marked applying until
  // the socket came back. Mutation: follow the save by the live slice
  // (`useEngineHealth`) and read its absence as applying, as
  // `nodeStanding(null)` did, and this reads "of 2.5M" and "applying…".
  test("refused by this node, it stays so through a dropped socket", async () => {
    const store = await saveDay();
    act(() =>
      store.applyHealth(
        healthFrame({
          applied_epoch: 4,
          config_attempt: { epoch: 5, revision_id: "r2", status: "error" },
        }),
      ),
    );
    await settle();
    expect(within(cell()).getByText("refused by this node")).toBeTruthy();
    act(() => store.setConnected(false));
    await settle();
    expect(within(cell()).getByText("of 1,000")).toBeTruthy();
    expect(within(cell()).getByText("refused by this node")).toBeTruthy();
    expect(within(cell()).queryByText("applying…")).toBeNull();
  });

  test("applied by this node, it says so and reads the budgets again", async () => {
    const store = await saveDay();
    const asks = budgetAsks;
    act(() =>
      store.applyHealth(
        healthFrame({
          applied_epoch: 5,
          config_attempt: { epoch: 5, revision_id: "r2", status: "ok" },
        }),
      ),
    );
    await settle();
    expect(budgetAsks).toBeGreaterThan(asks);
    expect(within(cell()).getByText("applied")).toBeTruthy();
  });

  // A LATER ACTIVATION THIS NODE TRIED FIRST, and refused: it will not come
  // back to this save, so the cell settles on the answer read again rather
  // than applying for ever. Mutation: drop `passed` from the settling
  // standings and the cell says "applying…" for good.
  test("passed by a later activation, it settles on the ceiling read again", async () => {
    const store = await saveDay();
    const asks = budgetAsks;
    act(() =>
      store.applyHealth(
        healthFrame({
          applied_epoch: 4,
          config_attempt: { epoch: 6, revision_id: "r3", status: "error" },
        }),
      ),
    );
    await settle();
    expect(budgetAsks).toBeGreaterThan(asks);
    expect(within(cell()).getByText("of 1,000")).toBeTruthy();
    expect(within(cell()).queryByRole("status")).toBeNull();
  });
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

// NEVER HIDDEN: a reader who may not write the configuration sees every
// ceiling and every pencil, disabled with the reason, and the reason once
// above — the grant a ceiling is written under, for a signed-in reader
// without it, and signing in for nobody at all.
test("a reader who cannot change the configuration sees why, on every ceiling", async () => {
  mount(answer(), { login: "dee", owner: "dee", grants: [] });
  await settle();
  // The company's three, each person's assistant's three, and the one seat's.
  const pencils = screen.getAllByRole("button", { name: /token ceiling/ });
  expect(pencils.length).toBe(9);
  for (const pencil of pencils) expect(pencil.getAttribute("aria-disabled")).toBe("true");
  expect(screen.getAllByText(CONFIG_WRITE_REASONS.no_grant).length).toBeGreaterThan(0);
  cleanup();
  mount(answer(), READER);
  await settle();
  expect(screen.getAllByText(CONFIG_WRITE_REASONS.anonymous).length).toBeGreaterThan(0);
});

// UNREADABLE IS NOT ZERO.
test("a counter nobody could read says so, and draws no figures", async () => {
  mount(answer({ durable: false, org: { windows: [] }, seats: [] }));
  await settle();
  expect(screen.getByText("Couldn’t read current spend. Try again shortly.")).toBeTruthy();
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

// THE ASSISTANT FILLS THE FIELD AND NOTHING MORE: the editor of that scope's
// window opens with the value in it, nothing is sent, and the save is the
// person's own press.
test("a ceiling the assistant prepared opens its editor filled in, and saves nothing", async () => {
  const calls = stubConfig(() => valid());
  mount(answer());
  await settle();
  const ask = (payload: unknown) =>
    applyPageAction({
      correlation_id: "c",
      action: "budgets.ceiling.prefill",
      payload,
      source_revision: "",
    });
  expect(ask({ seat: "nobody", period: "day", ceiling: "1M" }).outcome).toBe("invalid");
  expect(ask({ period: "day", ceiling: "0" }).outcome).toBe("invalid");
  await act(async () => {
    expect(ask({ seat: "pm", period: "week", ceiling: "2M" }).outcome).toBe("applied");
  });
  await settle();
  const field = screen.getByRole("textbox") as HTMLInputElement;
  expect(field.value).toBe("2M");
  expect(calls).toEqual([]);
  resetPageContextForTest();
});

test("a ceiling open in its editor is the assistant's to read and fill in, and saves nothing", async () => {
  const calls = stubConfig(() => valid());
  setFramePage({
    screen: "spend-budgets",
    title: "Spend › Budgets",
    path: ["spend", "budgets"],
    params: {},
    query: {},
  });
  mount(answer());
  await settle();
  expect(pageSnapshot()!.context.draft).toBeUndefined();
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /Change PM's daily token ceiling/ }));
  });
  await settle();
  const open = () => pageSnapshot()!.context;
  expect(open().draft).toMatchObject({ kind: "client", id: "pm", step: "day" });
  expect(open().draft!.fields[0]).toMatchObject({ path: "ceiling", editable: true });
  const set = (ceiling: string) =>
    applyPageAction({
      correlation_id: ceiling,
      action: "draft.set",
      payload: { values: { ceiling } },
      source_revision: open().source_revision,
    });
  // A VALUE THE FIELD REFUSES is refused in its own words.
  expect(set("lots")).toMatchObject({
    outcome: "invalid",
    reason: expect.stringMatching(/^Give a whole number of tokens/),
  });
  await act(async () => {
    expect(set("3M").outcome).toBe("applied");
  });
  expect((screen.getByRole("textbox") as HTMLInputElement).value).toBe("3M");
  expect(calls).toEqual([]);
});

// EACH PERSON'S ASSISTANT CEILING IS RAISED HERE, as the company's is: a merge
// patch one level down, checked first and recorded under a summary naming
// whose ceiling moved. It is the ceiling a person's question is refused by,
// and the remedy of that refusal opens this card.
test("each person's assistant ceiling is a merge patch of the one window, recorded as theirs", async () => {
  const calls = stubConfig((call) => {
    if (call.method === "GET")
      return json(
        { name: "Acme", assistant: { llm: ["p"], token_budget: { day: 200_000 } } },
        200,
        { ETag: '"r1"' },
      );
    if (call.url.includes("dry_run=true")) return valid();
    return saved();
  });
  mount(answer());
  await settle();
  await edit(/Change Ask Crewlet's per-person daily token ceiling/, "500k");
  const save = calls.at(-1)!;
  expect(save.method).toBe("PATCH");
  expect(save.url).not.toContain("dry_run");
  expect(save.body).toEqual({
    assistant: { token_budget: { day: 500_000 } },
    _summary: "Raise Ask Crewlet's per-person daily token ceiling from 200k to 500k",
  });
});

// A FIGURE ONLY WHERE ONE IS COUNTED. The viewer's own spend is drawn where
// the engine counts it; a zero nobody measured is not drawn as "0 tokens".
test("the assistant's row draws the viewer's own spend only where it is counted", async () => {
  mount(answer());
  await settle();
  const card = screen.getByRole("region", { name: "Ask Crewlet, per person" });
  expect(within(card).getByText("180k")).toBeDefined();
  expect(within(card).getAllByRole("meter").length).toBe(1);
  cleanup();

  mount(answer({ assistant: { counted: false, windows: [win("day", { limit: 200_000 })] } }));
  await settle();
  const uncounted = screen.getByRole("region", { name: "Ask Crewlet, per person" });
  expect(within(uncounted).queryByText(/^0$/)).toBeNull();
  expect(within(uncounted).queryByRole("meter")).toBeNull();
  expect(
    within(uncounted).getByRole("button", { name: /Ask Crewlet's per-person daily token ceiling/ }),
  ).toBeDefined();
});

// A REMEDY'S ADDRESS OPENS THE CEILING IT NAMES: the card, or the seat's row,
// marked as the one the reader came for and given focus; and a press on that
// same address once the reader has moved on brings them back to it.
//
// Mutation: drop the section's arrival, or ignore a press on the address
// already open, and a case fails.
describe("arriving at a ceiling", () => {
  const card = (name: string) => screen.getByRole("region", { name });

  test("the assistant's ceiling is marked and focused", async () => {
    mount(answer(), OPERATOR, "#/company/budgets?section=assistant");
    await settle();
    const assistant = card("Ask Crewlet, per person");
    expect(assistant.classList.contains("is-selected")).toBe(true);
    expect(card("The company").classList.contains("is-selected")).toBe(false);
    expect(document.activeElement).toBe(assistant.parentElement);
  });

  test("a seat's ceiling is its row, marked as current", async () => {
    mount(answer(), OPERATOR, "#/company/budgets?section=seat:pm");
    await settle();
    const marked = document.querySelector('[aria-current="true"]');
    expect(marked?.closest('[role="row"]')?.textContent).toContain("PM");
  });

  test("a press on the address already open arrives again", async () => {
    mount(answer(), OPERATOR, "#/company/budgets?section=company");
    await settle();
    const company = card("The company");
    expect(document.activeElement).toBe(company.parentElement);
    (document.activeElement as HTMLElement).blur();
    expect(document.activeElement).toBe(document.body);
    await act(async () => {
      arriveAgain();
    });
    expect(document.activeElement).toBe(company.parentElement);
  });

  test("an address arrived at while the screen is open is revealed", async () => {
    mount(answer());
    await settle();
    expect(card("The company").classList.contains("is-selected")).toBe(false);
    await act(async () => {
      location.hash = "#/company/budgets?section=company";
      window.dispatchEvent(new HashChangeEvent("hashchange"));
    });
    await settle();
    expect(card("The company").classList.contains("is-selected")).toBe(true);
    expect(document.activeElement).toBe(card("The company").parentElement);
  });

  test("a section this screen does not draw reveals nothing, and the screen is read as it is", async () => {
    mount(answer(), OPERATOR, "#/company/budgets?section=seat:ghost");
    await settle();
    expect(document.querySelector('[aria-current="true"]')).toBeNull();
    expect(document.querySelector(".is-selected")).toBeNull();
    expect(card("The company")).toBeDefined();
  });
});
