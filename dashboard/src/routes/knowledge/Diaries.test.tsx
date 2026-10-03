/**
 * Agent diaries: EVERY agent, each counted by its holder, and one agent's
 * diary as its holder answered it.
 *
 *  - MORE THAN A DOZEN AGENTS ARE LISTED — the Knowledge home once stopped at
 *    twelve, and the thirteenth agent's diary was reachable from nowhere.
 *  - A HOLDER THAT DID NOT ANSWER IS NAMED in the partial callout, and its
 *    agents say so instead of drawing zeros as counts.
 *  - AN AGENT NO NODE HOLDS SAYS SO, on the list and on its own page, rather
 *    than reading as an agent that remembers nothing.
 */

import { act, cleanup, render, screen, within } from "~/test/inCase.ts";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { Diaries, Diary } from "./Diaries.tsx";
import { Router } from "~/app/router.tsx";
import { FrameReadings } from "~/app/Shell.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { reloadForTest } from "~/lib/prefs.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { MemoryOverviewSeat } from "~/contract/memory.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

const AGENTS = Array.from({ length: 15 }, (_, i) => ({
  name: `Agent ${String(i).padStart(2, "0")}`,
  handle: `agent-${i}`,
  kind: "agent",
}));

const ORG = {
  name: "Acme",
  timezone: "UTC",
  roles: [...AGENTS, { name: "Jane Founder", handle: "jane", kind: "human" }],
  units: [],
};

const entry = (content: string) => ({
  id: `d-${content}`,
  content,
  retention: "diary_long",
  source: "tool:reflect_and_persist",
  turn_id: "turn-1",
  created_at: "2026-09-28T10:00:00Z",
  ttl_until: "",
  retrievals: 0,
});

function row(handle: string, over: Partial<MemoryOverviewSeat> = {}): MemoryOverviewSeat {
  return {
    agent_id: `agent-${handle}`,
    handle,
    diary_total: 7,
    episodes_total: 3,
    skills_total: 1,
    last_reflection_at: "2026-09-28T10:00:00Z",
    latest_reflection: entry(`${handle} noted the deploy window`),
    held_by: "node-a",
    unavailable: "",
    ...over,
  };
}

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  localStorage.clear();
  reloadForTest();
  location.hash = "#/";
});

function mount(ui: React.ReactNode, answers: Record<string, unknown>) {
  const store = new Store();
  store.applyHealth({ status: "healthy", nodes: 1 } as never);
  store.applyOrg(ORG as never);
  const socket = new LiveSocket(store);
  const asked: { kind: string; params: Record<string, unknown> }[] = [];
  socket.query = ((what: string, params?: Record<string, unknown>) => {
    asked.push({ kind: what, params: params ?? {} });
    return Promise.resolve(answers[what] ?? {});
  }) as typeof socket.query;
  render(
    <ToastProvider>
      <LayerHost>
        <ClientContext.Provider value={{ store, socket }}>
          <FrameReadings>
            <Router>{ui}</Router>
          </FrameReadings>
        </ClientContext.Provider>
      </LayerHost>
    </ToastProvider>,
  );
  return { asked };
}

async function settle() {
  await act(async () => {
    for (let i = 0; i < 10; i++) await Promise.resolve();
  });
}

const rowOf = (text: string) => screen.getByText(text).closest(".grid-row") as HTMLElement;

test("every agent is listed, past a dozen, each linking to its diary", async () => {
  mount(<Diaries />, {
    memory_overview: {
      seats: AGENTS.map((a) => row(a.handle)),
      coverage: { nodes: [{ id: "node-a", answered: true, error: "" }], complete: true },
    },
  });
  await settle();
  for (const agent of AGENTS) expect(screen.getByText(agent.name)).toBeTruthy();
  const last = rowOf("Agent 14");
  expect(within(last).getByText("agent-14 noted the deploy window")).toBeTruthy();
  expect(last.querySelector("a.row-link")?.getAttribute("href")).toBe(
    "#/knowledge/diaries/agent-14",
  );
  // THE HEADER COUNTS THE AGENTS, and no partial callout on a complete answer.
  expect(screen.getByText("15")).toBeTruthy();
  expect(screen.queryByText(/is missing/)).toBeNull();
});

test("a holder that did not answer is named, and its agent draws no zeros", async () => {
  mount(<Diaries />, {
    memory_overview: {
      seats: [
        row("agent-0"),
        row("agent-1", {
          held_by: "node-c",
          unavailable: "the node holding it did not answer",
          diary_total: 0,
          episodes_total: 0,
          skills_total: 0,
          last_reflection_at: "",
          latest_reflection: null,
        }),
      ],
      coverage: {
        nodes: [
          { id: "node-a", answered: true, error: "" },
          { id: "node-c", answered: false, error: "no answer within the 2s read budget" },
        ],
        complete: false,
      },
    },
  });
  await settle();
  const callout = screen.getByText(/is missing one node/).closest(".coverage-note") as HTMLElement;
  expect(within(callout).getByText("node-c")).toBeTruthy();
  expect(callout.textContent).toContain("listed without their counts");
  expect(callout.textContent).toContain("2s read budget");
  const silent = rowOf("Agent 01");
  expect(within(silent).getByText("Its holder did not answer")).toBeTruthy();
  expect(within(silent).queryByText("0")).toBeNull();
  // The answered agent keeps its counts beside it.
  expect(within(rowOf("Agent 00")).getByText("7")).toBeTruthy();
});

test("an agent no node holds says so on the list", async () => {
  mount(<Diaries />, {
    memory_overview: {
      seats: [
        row("agent-2", {
          held_by: "none",
          diary_total: 0,
          episodes_total: 0,
          skills_total: 0,
          last_reflection_at: "",
          latest_reflection: null,
        }),
      ],
      coverage: { nodes: [{ id: "node-a", answered: true, error: "" }], complete: true },
    },
  });
  await settle();
  const r = rowOf("Agent 02");
  expect(within(r).getByText("Held by no node")).toBeTruthy();
  expect(within(r).getByText("No node")).toBeTruthy();
  expect(within(r).queryByText("0")).toBeNull();
});

const memory = (over: Record<string, unknown> = {}) => ({
  id: "agent-3",
  diary: [entry("the release window moved to Thursday")],
  diary_total: 60,
  episodes: [],
  episodes_total: 0,
  skills: [],
  skills_total: 2,
  counterparties: [],
  counterparties_total: 5,
  latest_reflection: entry("the release window moved to Thursday"),
  onboarded_at: "",
  held_by: "node-b",
  ...over,
});

test("one agent's diary is its holder's answer, and names that node", async () => {
  const { asked } = mount(<Diary handle="agent-3" />, { agent_memory: memory() });
  await settle();
  expect(asked.find((a) => a.kind === "agent_memory")?.params).toEqual({ id: "agent-3" });
  expect(screen.getByText("the release window moved to Thursday")).toBeTruthy();
  expect(screen.getByText(/Read from node-b, the node holding this seat/)).toBeTruthy();
  // THE HEADER IS THE TOTAL, with the page it drew said beside it.
  expect(screen.getByText(/Latest 1 of 60/)).toBeTruthy();
  expect(screen.getByRole("link", { name: /Agent 03.s whole memory/ }).getAttribute("href")).toBe(
    "#/agents/seats/agent-3?tab=memory",
  );
});

test("one agent no node holds says why its diary is not shown", async () => {
  mount(<Diary handle="agent-4" />, {
    agent_memory: memory({
      id: "agent-4",
      diary: [],
      diary_total: 0,
      latest_reflection: null,
      held_by: "none",
    }),
  });
  await settle();
  expect(screen.getByText("No node holds this agent")).toBeTruthy();
  expect(screen.queryByRole("heading", { name: "Diary" })).toBeNull();
});
