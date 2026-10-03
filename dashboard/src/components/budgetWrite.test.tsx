/**
 * Raise budget, where a stopped seat is reported: which scope it raises, and
 * that the dialog writes only the windows a person changed.
 */

import { act, cleanup, fireEvent, render, screen } from "~/test/inCase.ts";
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
            login: "jane",
            grants: ["config:read", "config:write"],
            handle: "jane",
            owner: "jane",
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

/** A seat's chart row as `GET /chart/seats/{handle}?runtime=true` serves it. */
const DEVREL_SEAT = {
  seat: {
    handle: "devrel",
    kind: "agent",
    unit: "growth",
    name: "DevRel",
    email: "${CHART_SEAT_DEVREL_EMAIL}",
    goal: "grow the community",
    runtime: { llm: "zulu", token_budget: { day: 100 } },
  },
  manages: null,
  answer: { level: "consistent_prefix", position: "CREWLET_CHART_LOG@1:4" },
  runtime: true,
};

interface Sent {
  method: string;
  url: string;
  body: unknown;
  key: string | null;
}

/** The chart surface behind a seat's ceiling, answering each write as told. */
function stubChart(write: (call: number) => Response): Sent[] {
  const sent: Sent[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit = {}) => {
      const method = init.method ?? "GET";
      sent.push({
        method,
        url,
        body: init.body ? JSON.parse(init.body as string) : undefined,
        key: new Headers(init.headers).get("Idempotency-Key"),
      });
      if (method === "GET") {
        return new Response(JSON.stringify(DEVREL_SEAT), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
      }
      return write(sent.filter((s) => s.method === "PATCH").length);
    }),
  );
  return sent;
}

const answer = (payload: unknown, status: number) =>
  new Response(JSON.stringify(payload), {
    status,
    headers: { "Content-Type": "application/json" },
  });

async function raiseTo(value: string) {
  const field = screen.getByLabelText(/Daily ceiling/) as HTMLInputElement;
  fireEvent.change(field, { target: { value } });
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
  });
  await settle();
}

// A SEAT'S CEILING IS ITS CHART RUNTIME, written through the seat's content
// write: read with the runtime half, sent back whole with the ceiling changed,
// under an operation the engine can answer a retry of. The company document
// holds no seats, so the `/config` write this used to send reached nothing.
test("a seat's ceiling is a chart content write of its runtime half", async () => {
  const sent = stubChart(() =>
    answer({ outcome: "pending", position: "CREWLET_CHART_LOG@1:5", op_id: "k" }, 202),
  );
  mount(day(), false);
  await open();
  await raiseTo("2.5M");
  expect(sent.map((s) => `${s.method} ${new URL(s.url).pathname}${new URL(s.url).search}`)).toEqual(
    ["GET /chart/seats/devrel?runtime=true", "PATCH /chart/seats/devrel"],
  );
  const write = sent[1]!;
  expect(write.key).toBeTruthy();
  expect(write.body).toEqual({
    unit: "growth",
    name: "DevRel",
    email: "${CHART_SEAT_DEVREL_EMAIL}",
    backstory: "",
    goal: "grow the community",
    responsibilities: [],
    behavioral_guidelines: [],
    project: "",
    space: "",
    runtime: { llm: "zulu", token_budget: { day: 2_500_000 } },
  });
  // DURABLE, so the dialog is done; this node applies it in its own time.
  expect(screen.queryByRole("dialog")).toBeNull();
});

// AN OUTCOME NOBODY COULD ESTABLISH IS RESENT AS ITSELF: the same operation,
// which the chart's ledger answers with whatever became of the first. A fresh
// key would be a second write beside one that may have landed.
test("an unknown outcome is said, and sending again resends the same operation", async () => {
  const sent = stubChart((call) =>
    call === 1
      ? answer({ error: "unavailable", outcome: "unknown", op_id: "k" }, 503)
      : answer({ outcome: "applied", position: "CREWLET_CHART_LOG@1:5", op_id: "k" }, 200),
  );
  mount(day(), false);
  await open();
  await raiseTo("2.5M");
  expect(screen.getByText(/could not say whether the ceiling was saved/)).toBeTruthy();
  expect(screen.getByRole("dialog")).toBeTruthy();

  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Send again" }));
  });
  await settle();
  const writes = sent.filter((s) => s.method === "PATCH");
  expect(writes).toHaveLength(2);
  expect(writes[1]!.key).toBe(writes[0]!.key);
  // Resent, not re-read: the second write is the first one's body.
  expect(sent.filter((s) => s.method === "GET")).toHaveLength(1);
  expect(writes[1]!.body).toEqual(writes[0]!.body);
  expect(screen.queryByRole("dialog")).toBeNull();
});

// A RUNTIME HALF THE CHART WITHHELD IS NOT AN EMPTY ONE: written back, it
// would state the seat had no model chain and no ceilings at all.
test("a seat read without its runtime half saves nothing", async () => {
  const sent: string[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit = {}) => {
      sent.push(init.method ?? "GET");
      return answer({ ...DEVREL_SEAT, seat: { handle: "devrel" }, runtime: false }, 200);
    }),
  );
  mount(day(), false);
  await open();
  await raiseTo("2.5M");
  expect(screen.getByText(/needs config:read/)).toBeTruthy();
  expect(sent).toEqual(["GET"]);
});
