/**
 * Agent skills: every count the engine's total, and "loaded by" from both
 * ways a skill reaches a seat.
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { Skills } from "./Skills.tsx";
import { Router } from "~/app/router.tsx";
import { FrameReadings } from "~/app/Shell.tsx";
import { FRAME_ANSWERS } from "~/test/viewer.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { reloadForTest } from "~/lib/prefs.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
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

const skill = (id: string, title: string, updated_at = "2026-09-20T10:00:00Z") => ({
  id,
  container: "TS",
  title,
  status: "published",
  version: 1,
  skill: true,
  updated_at,
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

/** An answer, or one drawn from the params a question was asked with. */
type Answer = unknown | ((params: Record<string, unknown>) => unknown);

function mount(answers: Record<string, Answer>) {
  const store = new Store();
  store.applyHealth(healthFrame({ nodes: 1 }));
  store.applyOrg(withDerived(ORG) as never);
  const socket = new LiveSocket(store);
  const asked: { kind: string; params: Record<string, unknown> }[] = [];
  socket.query = ((what: string, params?: Record<string, unknown>) => {
    asked.push({ kind: what, params: params ?? {} });
    const answer = answers[what];
    return Promise.resolve(
      (typeof answer === "function" ? answer(params ?? {}) : answer) ?? FRAME_ANSWERS[what] ?? {},
    );
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

// THE ORDER IS THE ENGINE'S, never a sort of the windows loaded. By name is
// the knowledge base's own order and Newest is the engine's newest-first one,
// each drawn exactly as answered: here the newest skill's title sorts last, so
// a client sort of either kind would put it somewhere else. And no head is a
// button, since a head would sort the rows one window holds.
//
// Mutation: sort the rows here by title again, and Newest draws "Zeta" last;
// ask without the order, and Newest is answered in the title order.
test("the tool skills are drawn in the order the engine read them in", async () => {
  const byTitle = [
    skill("s-1", "Alpha", "2026-09-20T10:00:00Z"),
    skill("s-2", "Beta", "2026-09-22T10:00:00Z"),
    skill("s-3", "Zeta", "2026-09-24T10:00:00Z"),
  ];
  const newest = [byTitle[2], byTitle[1], byTitle[0]];
  const { asked } = mount({
    pages: (params: Record<string, unknown>) => ({
      pages: params.order === "newest" ? newest : byTitle,
      limit: 500,
      total: 3,
    }),
  });
  await settle();
  expect(asked.findLast((a) => a.kind === "pages")?.params).toMatchObject({ order: "title" });
  const titles = () =>
    [...document.querySelectorAll("[data-row-index]")].map((row) =>
      ["Alpha", "Beta", "Zeta"].find((t) => row.textContent?.includes(t)),
    );
  expect(titles()).toEqual(["Alpha", "Beta", "Zeta"]);
  expect(screen.queryByRole("button", { name: /^Updated/ })).toBeNull();
  expect(screen.queryByRole("button", { name: /^Skill/ })).toBeNull();

  fireEvent.click(screen.getByRole("radio", { name: "Newest" }));
  await settle();
  expect(asked.findLast((a) => a.kind === "pages")?.params).toMatchObject({
    skills: true,
    status: "published",
    order: "newest",
  });
  expect(location.hash).toContain("sort=-updated");
  expect(titles()).toEqual(["Zeta", "Beta", "Alpha"]);
});

// A LINK TO THE NEWEST SKILLS OPENS ON THEM: `sort=` is the URL's, so a
// reader sent `sort=-updated` lands on the engine's newest order, and a
// `sort=` no order answers to reads as the knowledge base's own.
test("sort= in the address picks the engine's order", async () => {
  location.hash = "#/doclet/skills?sort=-updated";
  const { asked } = mount({ pages: { pages: [skill("s-1", "Alpha")], limit: 500, total: 1 } });
  await settle();
  expect(asked.findLast((a) => a.kind === "pages")?.params).toMatchObject({ order: "newest" });
  expect(screen.getByRole("radio", { name: "Newest" }).getAttribute("aria-checked")).toBe("true");
  cleanup();
  location.hash = "#/doclet/skills?sort=loads";
  const again = mount({ pages: { pages: [skill("s-1", "Alpha")], limit: 500, total: 1 } });
  await settle();
  expect(again.asked.findLast((a) => a.kind === "pages")?.params).toMatchObject({ order: "title" });
});

test("a seat's learned skills say how many of its total are listed", async () => {
  location.hash = "#/doclet/skills?kind=learned&seat=swe";
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
  expect(asked.find((a) => a.kind === "agent_memory")?.params).toEqual({
    id: "swe",
    skills_order: "uses",
  });
  expect(screen.getByText("Triage an alert")).toBeTruthy();
  expect(screen.getByText("1 of 40 learned skills.")).toBeTruthy();
});

// THE HOLDER ORDERS A SEAT'S SKILLS, because it cuts the page: sorted here, the
// most used were the most used of the newest fifty. The order in `sort=` is
// asked for as `skills_order`, and the heads are not buttons, because a head
// that re-sorted the page would answer another question under the same arrow.
// Mutation: drop `skills_order` from the question and the holder is asked for
// the newest.
test("a seat's learned skills are ordered by the holder, in the order sort= names", async () => {
  location.hash = "#/doclet/skills?kind=learned&seat=swe&sort=-updated";
  const { asked } = mount({
    agent_memory: {
      id: "swe",
      skills: [],
      skills_total: 0,
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
  expect(asked.find((a) => a.kind === "agent_memory")?.params).toEqual({
    id: "swe",
    skills_order: "updated",
  });
  expect(screen.getByRole("radio", { name: "Recently changed" }).getAttribute("aria-checked")).toBe(
    "true",
  );
});

// WHAT A SEAT LEARNED IS ITS TRAIL, which the engine answers to an admin and
// to whoever leads the seat: anybody else is shown the lock, naming both ways
// in, and the seat's memory is never asked. The CONTROL is the case above,
// read by an admin. Mutation: ask regardless and `agent_memory` is asked.
test("a seat's learned skills are locked to a reader who may not read its trail", async () => {
  location.hash = "#/doclet/skills?kind=learned&seat=swe";
  const { asked } = mount({
    viewer: { login: "erin", owner: "erin", handle: "", grants: ["doclet"], acts: [] },
  });
  await settle();
  expect(screen.getByText("You need Admin access")).toBeTruthy();
  expect(screen.getByText(/This seat’s lead can see it too\./)).toBeTruthy();
  expect(asked.some((a) => a.kind === "agent_memory")).toBe(false);
});
