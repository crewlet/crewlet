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
};

function mount(own: BudgetWindow | undefined, orgRefusing: boolean) {
  const store = new Store();
  store.applyHealth({ status: "healthy" });
  store.setConnected(true);
  store.applyBudget({
    meter_id: "n:1",
    seq: 1,
    timezone: "UTC",
    org: { windows: orgRefusing ? [day({ limit: 5_000_000, used: 5_000_000 })] : [] },
  });
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    what === "budgets"
      ? Promise.resolve(BUDGETS)
      : what === "viewer"
        ? Promise.resolve({
            operator_id: "U0",
            operator: true,
            handle: "jane",
            name: "Jane",
            kind: "human",
          })
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

test("it raises the company's when only the company's window is refusing", async () => {
  mount(undefined, true);
  await open();
  expect(screen.getByRole("dialog", { name: "Raise the company's budget" })).toBeTruthy();
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
