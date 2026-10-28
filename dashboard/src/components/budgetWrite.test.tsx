/**
 * Raise budget, where a stopped seat is reported: which scope it raises, and
 * that the dialog writes only the windows a person changed.
 */

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { RaiseBudgetButton } from "./budgetWrite.tsx";
import { FrameReadings } from "~/app/Shell.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { BudgetWindow, BudgetsAnswer, OrgProjection } from "~/protocol/index.ts";
import { managedSentence } from "~/protocol/configAnswer.ts";
import { CHART_ORG } from "~/test/orgchart.ts";
import { healthFrame } from "~/test/health.ts";
import { ADMIN_RAISES } from "~/lib/remedy.ts";

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
});

const day = (extra: Partial<BudgetWindow> = {}): BudgetWindow => ({
  period: "day",
  window: "2026-09-29",
  starts_at: "2026-09-29T00:00:00Z",
  resets_at: "2026-09-30T00:00:00Z",
  used: 100,
  limit: 100,
  state: "refusing",
  ...extra,
});

const BUDGETS: BudgetsAnswer = {
  timezone: "UTC",
  durable: true,
  near_fraction: 0.9,
  org: { windows: [day({ limit: 5_000_000, used: 5_000_000 })] },
  seats: [{ agent_id: "a", role: "DevRel", handle: "devrel", windows: [day()] }],
  assistant: { counted: false, windows: [day({ used: 0, limit: 100_000, state: "ok" })] },
};

/** An administrator, who holds every grant. */
const ADMIN = {
  login: "U0",
  grants: ["admin", "worklet", "doclet"],
  handle: "jane",
  owner: "jane",
  name: "Jane",
  kind: "human",
  config_writer: true,
  config_managed_by: [],
};

function mount(
  own: BudgetWindow | undefined,
  orgRefusing: boolean,
  viewer: unknown = ADMIN,
  budgets = BUDGETS,
) {
  const store = new Store();
  store.applyHealth(healthFrame());
  store.setConnected(true);
  store.applyOrg(CHART_ORG as OrgProjection);
  store.applyBudget({
    meter_id: "n:1",
    seq: 1,
    timezone: "UTC",
    org: { windows: orgRefusing ? [day({ limit: 5_000_000, used: 5_000_000 })] : [] },
  });
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    what === "budgets"
      ? Promise.resolve(budgets)
      : what === "viewer"
        ? Promise.resolve(viewer)
        : new Promise(() => {});
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <ToastProvider>
        <LayerHost>
          <FrameReadings>
            <Router>
              <RaiseBudgetButton handle="devrel" name="DevRel" window={own} />
            </Router>
          </FrameReadings>
        </LayerHost>
      </ToastProvider>
    </ClientContext.Provider>,
  );
}

async function settle() {
  await act(async () => {
    for (let i = 0; i < 12; i++) await Promise.resolve();
  });
}

async function open() {
  await settle();
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Raise budget" }));
  });
  await settle();
}

// WHOSE CEILING STOPPED IT: raising the seat's while the company's is the one
// refusing changes nothing the gate reads.
test("it raises the seat's own ceiling when the seat's window is the one refusing", async () => {
  mount(day(), true);
  await open();
  expect(screen.getByRole("dialog", { name: "Raise DevRel's budget" })).toBeTruthy();
  expect(document.activeElement).toBe(screen.getByLabelText(/Daily ceiling/));
});

// A LEAD RAISES THE SEATS THEY LEAD, and never the company's ceiling: a
// seat's ceiling is no credential, and the engine admits a lead's write of a
// seat inside the units they lead. Where the ceiling is not theirs they are
// told why, and offered no button that is locked for them.
//
// Mutation: draw the button for a reader the gate refuses, and the second
// half finds one.
test("a lead raises a seat they lead, and is told the company's ceiling is not theirs", async () => {
  const pm = { ...ADMIN, login: "pm.person", grants: [], handle: "pm", owner: "pm" };
  mount(day(), true, pm);
  await settle();
  const seat = screen.getByRole("button", { name: "Raise budget" });
  expect(seat.getAttribute("title")).toBeNull();
  cleanup();

  mount(undefined, true, pm);
  await settle();
  expect(screen.queryByRole("button", { name: "Raise budget" })).toBeNull();
  expect(
    screen.getByText(
      "You lead Management and Developer Relations, and this is outside them: changing it takes Admin access.",
    ),
  ).toBeTruthy();
});

// A READER WHO CAN RAISE NO CEILING IS TOLD WHO CAN, never handed a button
// locked for them: the stopped seat's row reads as a fact with a person to ask.
test("a reader without Admin is told an admin can raise it, with no button", async () => {
  mount(day(), true, { ...ADMIN, login: "dee", grants: [], handle: "", owner: "dee" });
  await settle();
  expect(screen.queryByRole("button", { name: "Raise budget" })).toBeNull();
  expect(screen.getByText(ADMIN_RAISES)).toBeTruthy();
});

// A MANAGED DOCUMENT IS WRITTEN SOMEWHERE ELSE (ADR-0030): every credential
// but its writers is refused a raise (an administrator holding every grant,
// and a lead inside what they lead), so the row says who manages it rather
// than offering a save the engine refuses, or a button locked for them.
//
// Mutation: drop the managed check on the lead path, and the lead's raise is
// offered.
test("on a managed document a raise is held for everybody but its writers", async () => {
  const managed = { config_writer: false, config_managed_by: ["gitops"] };
  const pm = { ...ADMIN, login: "pm.person", grants: [], handle: "pm", owner: "pm" };
  for (const viewer of [
    { ...ADMIN, ...managed },
    { ...pm, ...managed },
  ]) {
    mount(day(), true, viewer);
    await settle();
    expect(screen.queryByRole("button", { name: "Raise budget" })).toBeNull();
    expect(screen.getByText(managedSentence(["gitops"]))).toBeTruthy();
    cleanup();
  }
});

// A LEAD'S RAISE IS COMPARED WITH THE COMPANY AS IT STANDS. The engine answers
// a lead's check of their seat unchanged, so a warning the company already
// carries — here a seat with no contact, which no ceiling caused — is the
// baseline's too, and the raise saves without asking the lead to confirm it.
test("a lead's raise saves without confirming a warning the company already had", async () => {
  const pm = { ...ADMIN, login: "pm.person", grants: [], handle: "pm", owner: "pm" };
  const seat = { name: "DevRel", handle: "devrel", token_budget: { day: 100 } };
  const unreachable = {
    kind: "advisory",
    path: "roles[0].contact",
    seat: "ceo",
    message: "ceo is a human seat with no contact identity",
  };
  const calls: { method: string; url: string; body: unknown }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit = {}) => {
      calls.push({
        method: init.method ?? "GET",
        url,
        body: init.body ? JSON.parse(init.body as string) : undefined,
      });
      const reply = (payload: unknown, status: number, headers: Record<string, string> = {}) =>
        new Response(JSON.stringify(payload), {
          status,
          headers: { "Content-Type": "application/json", ...headers },
        });
      if ((init.method ?? "GET") === "GET") return reply(seat, 200, { ETag: '"r1"' });
      if (url.includes("dry_run=true"))
        return reply({ valid: true, base_revision_id: "r1", warnings: [unreachable] }, 200);
      return reply({ revision_id: "r2", epoch: 2, warnings: [] }, 201);
    }),
  );
  mount(day(), true, pm);
  await open();
  fireEvent.change(screen.getByLabelText(/Daily ceiling/), { target: { value: "200" } });
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
  });
  await settle();
  expect(calls.map((c) => `${c.method} ${c.url.includes("dry_run") ? "check" : "write"}`)).toEqual([
    "GET write",
    "PUT check",
    "PUT check",
    "PUT write",
  ]);
  // The baseline was the seat as read, the second check the raise.
  expect(calls[1]!.body).toEqual(seat);
  expect(calls.at(-1)!.body).toMatchObject({ token_budget: { day: 200 } });
  expect(screen.queryByText(/no contact identity/)).toBeNull();
  expect(screen.queryByRole("dialog")).toBeNull();
});

test("it raises the company's when only the company's window is refusing", async () => {
  mount(undefined, true);
  await open();
  expect(screen.getByRole("dialog", { name: "Raise the company's budget" })).toBeTruthy();
});

// "LAST REFUSED" ONLY WHERE THE WINDOW IS STILL REFUSING. A refused round is
// counted, so a window that refused one reads past its ceiling and refusing; a
// ceiling raised since gives it room again while its stamp stays until the
// next admitted charge clears it, and the dialog must not call that window
// refusing in the same breath as its state says ok.
test("a refusal stamp is said to be refusing only while the window still refuses", async () => {
  const stamped = (extra: Partial<BudgetWindow>) =>
    day({ refused_at: "2026-09-29T09:00:00Z", ...extra });
  for (const [window, refusing] of [
    [stamped({ used: 120, limit: 100, state: "refusing" }), true],
    [stamped({ used: 120, limit: 1000, state: "ok" }), false],
  ] as const) {
    mount(day(), true, ADMIN, {
      ...BUDGETS,
      seats: [{ agent_id: "a", role: "DevRel", handle: "devrel", windows: [window] }],
    });
    await open();
    const helper = screen.getByText(/tokens spent today/);
    expect(/last refused/.test(helper.textContent ?? "")).toBe(refusing);
    cleanup();
  }
});

// ONLY WHAT WAS CHANGED: the dialog shows all three windows, and a save
// names the one a person typed in — a window left as it was is not rewritten.
test("the dialog checks, then saves only the windows that changed", async () => {
  const calls: { method: string; url: string; body: unknown }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit = {}) => {
      calls.push({
        method: init.method ?? "GET",
        url,
        body: init.body ? JSON.parse(init.body as string) : undefined,
      });
      const reply = (payload: unknown, status: number, headers: Record<string, string> = {}) =>
        new Response(JSON.stringify(payload), {
          status,
          headers: { "Content-Type": "application/json", ...headers },
        });
      if ((init.method ?? "GET") === "GET")
        return reply({ name: "Acme", token_budget: { day: 5_000_000 } }, 200, { ETag: '"r1"' });
      if (url.includes("dry_run=true"))
        return reply({ valid: true, base_revision_id: "r1", warnings: [] }, 200);
      return reply({ revision_id: "r2", epoch: 2, warnings: [] }, 201);
    }),
  );
  mount(undefined, true);
  await open();
  const field = screen.getByLabelText(/Daily ceiling/) as HTMLInputElement;
  expect(field.value).toBe("5M");
  fireEvent.change(field, { target: { value: "8M" } });
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
  });
  await settle();
  expect(calls.map((c) => `${c.method} ${c.url.includes("dry_run") ? "check" : "write"}`)).toEqual([
    "GET write",
    "PATCH check",
    "PATCH check",
    "PATCH write",
  ]);
  expect(calls.at(-1)!.body).toEqual({
    token_budget: { day: 8_000_000 },
    _summary: "Raise the company's daily token ceiling from 5M to 8M",
  });
  // A save ends the dialog.
  expect(screen.queryByRole("dialog")).toBeNull();
});
