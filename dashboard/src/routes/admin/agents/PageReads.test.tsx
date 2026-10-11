/**
 * A knowledge page as the agents read it, in the console: HOW each read it —
 * the turn and the search — and the engine's own count of today, which a
 * person's page does not carry.
 */

import { act, cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { PageReads } from "./PageReads.tsx";
import { Router } from "~/app/router.tsx";
import { FrameReadings } from "~/app/Shell.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { reloadForTest } from "~/lib/prefs.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { PageReadsAnswer } from "~/contract/pages.ts";
import type { PageDetail } from "~/protocol/index.ts";
import { healthFrame } from "~/test/health.ts";
import { withDerived } from "~/test/org.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

const NOW = Date.parse("2026-09-25T00:00:00Z");
const ID = "0b6f5a4e-6a41-4b6e-9d8c-3f1e2d4c5b6a";

const ADMIN = {
  token_id: "U0FOUNDER",
  role: "admin",
  reach: "admin",
  linked: true,
  handle: "jane",
  name: "Jane Founder",
  kind: "human",
  acts: [],
};

const ORG = {
  name: "Acme",
  timezone: "Asia/Tokyo",
  roles: [
    { name: "Jane Founder", handle: "jane", kind: "human" },
    { name: "SWE", handle: "swe", kind: "agent" },
    { name: "CTO", handle: "cto", kind: "agent" },
  ],
  units: [],
};

const detail = (over: Partial<PageDetail> = {}): PageDetail => ({
  page: {
    id: ID,
    container: "ENG",
    title: "Provisioner runbook",
    status: "published",
    version: 14,
    author: "cto",
    updated_at: "2026-09-24T22:00:00Z",
    revision: 40,
    body: "Check the switch.",
  } as PageDetail["page"],
  revision: 40,
  skill: false,
  onboarding: false,
  ...over,
});

const reads = (over: Partial<PageReadsAnswer> = {}): PageReadsAnswer => ({
  page: ID,
  since: "2026-08-27",
  until: "2026-09-25",
  days: 30,
  readers: [
    {
      handle: "swe",
      via: "get_page",
      count: 1,
      last_at: "2026-09-24T16:00:00Z",
      last_turn_id: "run-2",
      last_work_item: { id: "t-1", key: "ENG-412", title: "Retry PXE boot", ordinal: 2 },
    },
    {
      handle: "cto",
      via: "search",
      count: 3,
      last_at: "2026-09-24T14:00:00Z",
      last_query: "DHCP lease",
    },
  ],
  readers_total: 2,
  // THE ENGINE'S COUNT, which a list cut by its cap cannot be counted from.
  distinct_seats_today: 3,
  elided: 0,
  ...over,
});

type Answer = unknown | ((params: Record<string, unknown>) => Promise<unknown>);

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  vi.spyOn(Date, "now").mockReturnValue(NOW);
  location.hash = `#/admin/agents/pages/${ID}`;
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  localStorage.clear();
  reloadForTest();
  location.hash = "#/";
});

function mount(answers: Record<string, Answer>) {
  const store = new Store();
  store.applyHealth(healthFrame({ nodes: 1 }));
  store.applyOrg(withDerived(ORG) as never);
  const socket = new LiveSocket(store);
  const asked: { kind: string; params: Record<string, unknown> }[] = [];
  socket.query = ((what: string, params?: Record<string, unknown>) => {
    asked.push({ kind: what, params: params ?? {} });
    const all: Record<string, Answer> = {
      viewer: ADMIN,
      // THE FRAME'S OWN READINGS, which every screen is drawn inside.
      work_inbox: { handle: "jane", notices: [], primary_reasons: [], unread: 0, primary: 0 },
      decisions: { handle: "jane", items: [], total: 0, capped: false },
      ...answers,
    };
    if (what in all) {
      const answer = all[what];
      return typeof answer === "function"
        ? (answer as (p: Record<string, unknown>) => Promise<unknown>)(params ?? {})
        : Promise.resolve(answer);
    }
    return Promise.resolve({});
  }) as typeof socket.query;
  render(
    <ToastProvider>
      <LayerHost>
        <ClientContext.Provider value={{ store, socket }}>
          <FrameReadings>
            <Router>
              <PageReads id={ID} />
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

const readsCard = () =>
  screen.getByRole("heading", { name: /^Read by agents/ }).closest(".crewlet-card") as HTMLElement;

test("the page and how it was read are asked by the page's id", async () => {
  const { asked } = mount({ page: detail(), page_reads: reads() });
  await settle();
  expect(asked.find((a) => a.kind === "page")?.params).toEqual({ id: ID });
  expect(asked.find((a) => a.kind === "page_reads")?.params).toEqual({ page: ID });
});

// HOW, which the person's page never draws: the turn a read was in, as a way
// into Live, and the words a search used.
test("each reader says how it read the page and in which turn", async () => {
  mount({ page: detail(), page_reads: reads() });
  await settle();
  const card = readsCard();
  expect(within(card).getByRole("link", { name: "turn 2 on ENG-412" }).getAttribute("href")).toBe(
    "#/admin/live/turns/run-2",
  );
  expect(within(card).getByText(/search: “DHCP lease”/)).toBeTruthy();
  expect(within(card).getByText("3 reads")).toBeTruthy();
  // AN AGENT IS OPENED IN THE CONSOLE, where this page is.
  expect(within(card).getByRole("link", { name: "SWE" }).getAttribute("href")).toBe(
    "#/admin/agents/seats/swe",
  );
});

// THE COUNT IS THE ENGINE'S, cut on the company's day: two readers listed, one
// of them yesterday on the company's clock, and the figure is still three.
test("today's count is the engine's, beside the window the list covers", async () => {
  mount({ page: detail(), page_reads: reads() });
  await settle();
  const card = readsCard();
  expect(within(card).getByText("read by 3 agents today")).toBeTruthy();
  expect(within(card).getByRole("heading").textContent).toBe("Read by agents · last 30 days");
});

// A NODE WITHOUT THE USAGE DOMAIN DOES NOT SAY "NOBODY READ IT".
test("a node that keeps no record of reads says so, never that nobody read it", async () => {
  mount({ page: detail(), page_reads: () => Promise.reject(new Error("unknown_query")) });
  await settle();
  const card = readsCard();
  expect(within(card).getByText("This node keeps no record of who read what.")).toBeTruthy();
  expect(within(card).queryByText(/No agent has read it/)).toBeNull();
});

// THE CAP NOTE IS COMPANY-WIDE, and worded so: the engine's `elided` counts
// what the cap dropped on every page, so it says the list MAY be short.
test("dropped reads are named as possibly missing, never as this page's", async () => {
  mount({ page: detail(), page_reads: reads({ elided: 4 }) });
  await settle();
  expect(within(readsCard()).getByText(/this list may be missing some/)).toBeTruthy();
});

test("a tool skill lists who it reached, loaded and offered; any other page has no such card", async () => {
  const loads = [
    { handle: "swe", last_at: "2026-09-24T16:00:00Z", count: 7, loaded: 2, offered: 5 },
  ];
  mount({ page: detail({ skill: true, skill_loaded_by: loads }), page_reads: reads() });
  await settle();
  const card = screen
    .getByRole("heading", { name: /^Reached as a skill/ })
    .closest(".crewlet-card") as HTMLElement;
  expect(within(card).getByText("loaded 2 times · offered 5 times")).toBeTruthy();
  cleanup();

  mount({ page: detail({ skill: false, skill_loaded_by: loads }), page_reads: reads() });
  await settle();
  expect(screen.queryByRole("heading", { name: /^Reached as a skill/ })).toBeNull();
});

test("a page that does not exist says so", async () => {
  mount({ page: () => Promise.reject(new Error("not_found")), page_reads: reads() });
  await settle();
  expect(screen.getByText("There is no such page")).toBeTruthy();
});
