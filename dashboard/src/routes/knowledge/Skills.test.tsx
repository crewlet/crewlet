/**
 * Agent skills: every count the engine's total, and "loaded by" from both
 * ways a skill reaches a seat.
 */

import { act, cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { Skills } from "./Skills.tsx";
import { Router } from "~/app/router.tsx";
import { FrameReadings } from "~/app/Shell.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { reloadForTest } from "~/lib/prefs.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

const ORG = {
  name: "Acme",
  timezone: "UTC",
  roles: [
    { name: "SWE", handle: "swe", kind: "agent" },
    { name: "PM", handle: "pm", kind: "agent" },
    { name: "Jane Founder", handle: "jane", kind: "human" },
  ],
  units: [],
};

const skill = (id: string, title: string) => ({
  id,
  container: "TS",
  title,
  status: "published",
  version: 1,
  skill: true,
  updated_at: "2026-09-20T10:00:00Z",
  revision: 1,
});

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

function mount(answers: Record<string, unknown>) {
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
            <Router>
              <Skills />
            </Router>
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

test("the tool skills count is the listing's total, and loaded-by reads both ways", async () => {
  const at = "2026-09-24T10:00:00Z";
  const { asked } = mount({
    pages: {
      pages: [skill("s-1", "Working the tracker"), skill("s-2", "Reading the wiki")],
      limit: 500,
      // SIX HUNDRED IN ALL, two in this window: the count is the total.
      total: 600,
      after: "cursor",
      skill_loaded_by: {
        "s-1": [
          { handle: "swe", last_at: at, count: 3, loaded: 2, offered: 1 },
          { handle: "pm", last_at: at, count: 4, loaded: 0, offered: 4 },
        ],
        // OFFERED ONLY: nobody asked for the body, and the column says so.
        "s-2": [{ handle: "pm", last_at: at, count: 5, loaded: 0, offered: 5 }],
      },
    },
  });
  await settle();
  expect(asked.find((a) => a.kind === "pages")?.params).toMatchObject({
    skills: true,
    status: "published",
  });
  expect(screen.getByText("600")).toBeTruthy();
  expect(screen.getByText(/2 of 600 skills loaded/)).toBeTruthy();
  const tracker = screen
    .getByText("Working the tracker")
    .closest("[data-row-index]") as HTMLElement;
  expect(within(tracker).getByText("SWE")).toBeTruthy();
  const wiki = screen.getByText("Reading the wiki").closest("[data-row-index]") as HTMLElement;
  expect(within(wiki).getByText(/offered to PM/)).toBeTruthy();
});

test("a seat's learned skills say how many of its total are listed", async () => {
  location.hash = "#/knowledge/skills?kind=learned&seat=swe";
  const { asked } = mount({
    agent_memory: {
      id: "swe",
      skills: [
        {
          id: "k-1",
          key: "triage",
          title: "Triage an alert",
          summary: "read before paging",
          version: 2,
          updated_at: "2026-09-20T10:00:00Z",
          uses: 4,
        },
      ],
      skills_total: 40,
      diary: [],
      diary_total: 0,
      episodes: [],
      episodes_total: 0,
      counterparties: [],
      counterparties_total: 0,
      latest_reflection: null,
      onboarded_at: "",
      held_by: "node-a",
    },
  });
  await settle();
  expect(asked.find((a) => a.kind === "agent_memory")?.params).toEqual({ id: "swe" });
  expect(screen.getByText("Triage an alert")).toBeTruthy();
  expect(screen.getByText("1 of 40 learned skills.")).toBeTruthy();
});
