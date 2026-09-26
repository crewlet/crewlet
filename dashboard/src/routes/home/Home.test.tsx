/**
 * What the landing screen says, and on the day nothing is wrong.
 *
 * Its failure mode is not a crash: it is a queue-shaped home rendering a
 * healthy company as a blank page, which a reader cannot tell from a
 * dashboard that is broken. So the first fold is the COMPANY — the pulse
 * strip, figures true whatever the queue holds — and the conditions the
 * engine raised sit under it, the first few of them as ways into the Inbox.
 */

import { act, cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test } from "vitest";
import { EMPTY_VALUE } from "@crewlethq/ui";

import { Home, HOME_DECISIONS, greeting, statusLine } from "./Home.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { reloadForTest, setZone } from "~/lib/prefs.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { FrameReadings } from "~/app/Shell.tsx";

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
  location.hash = "#/home";
});

afterEach(() => {
  cleanup();
  localStorage.clear();
  reloadForTest();
  location.hash = "";
});

/**
 * Mount Home over a socket answering a bound viewer and a quiet company;
 * `answers` overrides one question, and a function answer is a thunk so a
 * case can answer with silence.
 */
function mount(answers: Record<string, unknown> = {}) {
  const store = new Store();
  // CONNECTED, because an inert socket is a condition in its own right and
  // these cases are about a company that is FINE.
  store.applyHealth({ status: "healthy" });
  const socket = new LiveSocket(store);
  (
    socket as unknown as {
      query: (what: string, params?: Record<string, unknown>) => Promise<unknown>;
    }
  ).query = (what: string) => {
    if (what in answers) {
      const answer = answers[what];
      return typeof answer === "function"
        ? (answer as () => Promise<unknown>)()
        : Promise.resolve(answer);
    }
    if (what === "viewer") {
      return Promise.resolve({
        operator_id: "U0FOUNDER",
        operator: true,
        handle: "ada",
        name: "Ada Lovelace",
        kind: "human",
      });
    }
    if (what === "sandbox_runs") return Promise.resolve({ runs: [] });
    if (what === "work_projects")
      return Promise.resolve({ projects: [], total: 0, complete: true });
    if (what === "work_workload") return Promise.resolve({ rows: [] });
    if (what === "work_inbox") {
      return Promise.resolve({ handle: "ada", notices: [], primary_reasons: [] });
    }
    return Promise.resolve({});
  };
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <FrameReadings>
        <Router>
          <Home />
        </Router>
      </FrameReadings>
    </ClientContext.Provider>,
  );
  return { store, socket };
}

async function settle() {
  await act(async () => {
    for (let i = 0; i < 4; i++) await Promise.resolve();
  });
}

describe("a healthy company is not a blank page", () => {
  test("the pulse strip and the decisions band both say what they are", async () => {
    mount();
    await settle();
    const strip = screen.getByRole("group", { name: "The company right now" });
    expect(within(strip).getByText("open")).toBeTruthy();
    expect(within(strip).getByText("tokens")).toBeTruthy();
    expect(within(strip).getByText("alarms")).toBeTruthy();
    expect(screen.getByText("Needs a decision")).toBeTruthy();
    expect(screen.getByText("Nothing needs a decision")).toBeTruthy();
  });

  // THE STRIP'S WORD AGREES WITH ITS FIGURE: one alarm is one alarm, not
  // "1 alarms" — and the figure is the ENGINE'S alarm table, the one the
  // sidebar's health card reads, never this screen's own conditions under
  // the same word (the strip said "0 alarms" beside a card saying five).
  test("the alarms are the engine's table, and one is counted as one", async () => {
    const { store } = mount();
    await act(async () => {
      store.applyHealth({ status: "healthy", alarms: { count: 1, worst: "backup_age" } });
    });
    await settle();
    const strip = screen.getByRole("group", { name: "The company right now" });
    const fact = [...strip.querySelectorAll("a")].find((a) => /alarm/.test(a.textContent ?? ""));
    expect(fact?.textContent).toContain("1");
    expect(within(strip).getByText("alarm")).toBeTruthy();
    expect(within(strip).queryByText("alarms")).toBeNull();
    expect(fact?.getAttribute("href")).toBe("#/settings/nodes");
  });

  test("an alarm table the engine has not evaluated is a dash, not zero alarms", async () => {
    mount();
    await settle();
    const strip = screen.getByRole("group", { name: "The company right now" });
    const fact = [...strip.querySelectorAll("a")].find((a) => /alarm/.test(a.textContent ?? ""));
    expect(fact?.textContent).toContain(EMPTY_VALUE);
    expect(fact?.textContent).not.toContain("0");
  });

  test("a figure whose query has not answered draws a dash, never a zero", async () => {
    const silent = () => new Promise(() => {});
    mount({ work_projects: silent, work_workload: silent });
    await settle();
    const strip = screen.getByRole("group", { name: "The company right now" });
    const openFact = [...strip.querySelectorAll("a")].find((a) => a.textContent?.includes("open"));
    expect(openFact?.textContent).toContain(EMPTY_VALUE);
    expect(openFact?.textContent).not.toContain("0");
  });
});

describe("the decisions band", () => {
  // THE FIRST FEW, EACH A WAY INTO THE INBOX ROW THAT HOLDS IT. Acting
  // happens there; this screen only says what is waiting.
  test("shows the first conditions as links to their Inbox rows, and says how many", async () => {
    const { store } = mount();
    await act(async () => {
      // No configuration, and a reconnecting socket: two conditions at once.
      store.applyHealth({ status: "healthy", configured: false });
      store.setConnected(false);
    });
    await settle();
    const band = screen.getByText("Needs a decision").closest(".crewlet-card") as HTMLElement;
    const rows = [...band.querySelectorAll<HTMLAnchorElement>("a.inbox-row")];
    expect(rows.length).toBeGreaterThan(0);
    expect(rows.length).toBeLessThanOrEqual(HOME_DECISIONS);
    for (const row of rows) expect(row.getAttribute("href")).toMatch(/^#\/inbox\?row=/);
    expect(within(band).getByRole("link", { name: "Open inbox" }).getAttribute("href")).toBe(
      "#/inbox",
    );
  });
});

describe("the head", () => {
  test("the greeting is in the zone the reader chose", () => {
    const at = Date.parse("2026-09-22T20:00:00Z");
    setZone("Asia/Tokyo");
    reloadForTest();
    // 05:00 in Tokyo; 22:00 in the browser's own Berlin.
    expect(greeting(at, "Ada Lovelace")).toBe("Good morning, Ada");
    setZone("");
    reloadForTest();
    expect(greeting(at, "Ada Lovelace")).toBe("Good evening, Ada");
    expect(greeting(at, "")).toBe("Good evening");
  });

  test("the status line says the condition that decides, before anything pushed", () => {
    const base = {
      company: "Nimbus",
      connected: true,
      authRejected: false,
      configured: true,
      nodes: 3,
      decisions: 0,
      waiting: { waiting: null, capped: false },
      working: 0,
    };
    expect(statusLine({ ...base, authRejected: true, connected: false })).toMatch(/refused/);
    expect(statusLine({ ...base, connected: false })).toMatch(/Not connected/);
    expect(statusLine({ ...base, configured: false })).toMatch(/No configuration/);
    expect(statusLine(base)).toContain("Nimbus");
  });

  // THE SAME COUNT AS THE BADGE: the Inbox's own reading, said only when
  // something is waiting and only once the engine has answered.
  test("the status line says what is waiting in the Inbox, as the badge counts it", () => {
    const base = {
      company: "Nimbus",
      connected: true,
      authRejected: false,
      configured: true,
      nodes: 3,
      decisions: 0,
      working: 4,
    };
    expect(statusLine({ ...base, waiting: { waiting: 2, capped: false } })).toBe(
      "Nimbus is running on 3 nodes. Nothing needs a decision, 2 notices are waiting on you, and 4 agents are working right now.",
    );
    expect(statusLine({ ...base, waiting: { waiting: 1, capped: false } })).toContain(
      "1 notice is waiting on you",
    );
    expect(statusLine({ ...base, waiting: { waiting: 50, capped: true } })).toContain(
      "50+ notices are waiting on you",
    );
    // Nothing waiting, and nothing answered, say nothing about the Inbox.
    for (const waiting of [
      { waiting: 0, capped: false },
      { waiting: null, capped: false },
    ]) {
      expect(statusLine({ ...base, waiting })).not.toMatch(/notice/);
    }
  });
});
