/**
 * What the landing screen says, and what a person can do from it.
 *
 * Its failure modes are not crashes: a figure that has not arrived drawn as a
 * zero, a budget meter that is the day's labelled as the week's, a live row
 * naming the work key a prompt mentioned rather than the item the turn is on,
 * a decision whose button sends something other than the choice — and a
 * reader nobody bound shown an empty list that claims to have looked.
 */

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { EMPTY_VALUE, LayerHost, ToastProvider } from "@crewlethq/ui";

import type { ReactNode } from "react";
import { Home } from "./Home.tsx";
import {
  applyPageAction,
  pageSnapshot,
  resetPageContextForTest,
  setFramePage,
} from "~/app/assistant/context.ts";
import { greeting } from "~/lib/format.ts";
import { Inbox } from "~/routes/inbox/Inbox.tsx";
import { NOBODY_SENTENCE } from "./Decisions.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { reloadForTest, pinZoneForTest } from "~/lib/prefs.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { FrameReadings } from "~/app/Shell.tsx";
import { SessionReading } from "~/lib/frameSession.ts";
import { healthFrame } from "~/test/health.ts";
import { ZERO_VERSIONS } from "~/test/liveCall.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

const JANE = {
  login: "U0FOUNDER",
  grants: ["admin", "worklet", "doclet"],
  handle: "jane",
  owner: "jane",
  name: "Jane Founder",
  kind: "human",
  acts: ["comment_on_work_item", "answer_run", "update_work_item", "create_work_item"],
};

const seat = (name: string, handle: string, kind: string, managers: string[] = []) => ({
  handle,
  name,
  kind,
  placed_by_ref: false,
  manager: managers[0] ?? "",
  managers,
  reports: handle === "jane" ? ["cto"] : [],
  auto_reports: [],
  onboarding_chain: [],
});

const ORG = {
  name: "Nimbus",
  timezone: "UTC",
  roles: [
    { name: "Jane Founder", handle: "jane", kind: "human" },
    { name: "CTO", handle: "cto", kind: "agent" },
    { name: "SWE", handle: "swe", kind: "agent" },
    { name: "DevRel", handle: "devrel", kind: "agent" },
  ],
  units: [],
  // THE ENGINE'S HIERARCHY, which is where "reports to you" is read from.
  derived: {
    seats: [
      seat("Jane Founder", "jane", "human"),
      seat("CTO", "cto", "agent", ["jane"]),
      seat("SWE", "swe", "agent"),
      seat("DevRel", "devrel", "agent"),
    ],
    units: [],
  },
};

const DECISION = {
  question: "Hold the 2.4 release until the scheduler migration lands?",
  options: [
    { id: "hold", label: "Hold release" },
    { id: "ship", label: "Ship anyway" },
  ],
  recommended: "hold",
  role: "approver" as const,
};

function ask(extra: Record<string, unknown> = {}) {
  return {
    kind: "ask",
    at: "2026-09-22T09:48:00Z",
    ask: {
      id: "t-12",
      key: "LEAD-12",
      project: "LEAD",
      title: "2.4 release",
      type: "task",
      status: "todo",
      comment: "c-12",
      asked_by: "cto",
      asked_at: "2026-09-22T09:48:00Z",
      body: "Hold the release?",
      decision: DECISION,
      answer_with: "",
      ...extra,
    },
  };
}

const QUIET = {
  decisions: { handle: "jane", items: [], total: 0, capped: false },
  work_flow: {
    bucket: "day",
    points: Array.from({ length: 14 }, (_, i) => ({
      window: `2026-09-${String(9 + i).padStart(2, "0")}`,
      start: `2026-09-${String(9 + i).padStart(2, "0")}T00:00:00Z`,
      end: "",
      not_started: 3,
      active: i,
      done: 0,
      closed: 0,
      completed: i % 3,
    })),
    now: { not_started: 3, active: 13, blocked: 2, overdue: 0 },
    blocked_history: false,
    complete: true,
  },
  token_series: {
    group: "unit",
    bucket: "day",
    series: [],
    by_group: [],
    totals: { input_tokens: 0, output_tokens: 0, total_tokens: 1_234_000, calls: 9 },
    grouped: { input_tokens: 0, output_tokens: 0, total_tokens: 0, calls: 0 },
  },
  company_feed: { rows: [], complete: true },
  work_projects: { projects: [], total: 0, census: {}, complete: true },
  sandbox_runs: { runs: [] },
  work_inbox: { handle: "jane", notices: [], primary_reasons: [] },
};

type Answer = unknown | ((params: Record<string, unknown>) => Promise<unknown>);

let asked: { kind: string; params: Record<string, unknown> }[];
let posted: { tool: string; args: Record<string, unknown> }[];

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  location.hash = "#/home";
  asked = [];
  posted = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      const tool = decodeURIComponent(String(url).split("/operator/act/")[1] ?? "");
      const body = JSON.parse(init.body as string) as { args: Record<string, unknown> };
      posted.push({ tool, args: body.args });
      return new Response(
        JSON.stringify({
          tool,
          outcome: "applied",
          position: "CREWLET_TRACKER_LOG@0:9",
          receipt: {},
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      );
    }),
  );
});

afterEach(() => {
  cleanup();
  resetPageContextForTest();
  vi.unstubAllGlobals();
  localStorage.clear();
  reloadForTest();
  location.hash = "";
});

/** A report that caps nothing: "no ceiling", as distinct from no report. */
const UNCAPPED = { meter_id: "n:1", seq: 1, timezone: "UTC", org: { windows: [] } };

/** Mount Home over a socket answering a bound viewer and a quiet company;
 *  `answers` overrides one question, and a function answer is a thunk so a
 *  case can answer with silence. */
function mount({
  answers = {},
  viewer = JANE,
  agents = [],
  budget,
  page = <Home />,
}: {
  answers?: Record<string, Answer>;
  viewer?: Record<string, unknown>;
  agents?: Record<string, unknown>[];
  budget?: unknown;
  /** The screen to mount: Home, or where one of its links lands. */
  page?: ReactNode;
} = {}) {
  const store = new Store();
  store.applyHealth(healthFrame({ nodes: 3 }));
  store.applyOrg(ORG as never);
  store.applySeats(agents as never);
  if (budget) store.applyBudget(budget as never);
  const socket = new LiveSocket(store);
  socket.query = ((what: string, params?: Record<string, unknown>) => {
    asked.push({ kind: what, params: params ?? {} });
    const all: Record<string, Answer> = { ...QUIET, viewer, ...answers };
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
          <SessionReading>
            <FrameReadings>
              <Router>{page}</Router>
            </FrameReadings>
          </SessionReading>
        </ClientContext.Provider>
      </LayerHost>
    </ToastProvider>,
  );
  return { store, socket };
}

async function settle() {
  await act(async () => {
    for (let i = 0; i < 6; i++) await Promise.resolve();
  });
}

function tile(label: string | RegExp): HTMLElement {
  const title = screen.getAllByText(label).find((el) => el.closest(".crewlet-statcard"));
  const card = title?.closest(".crewlet-statcard");
  if (!card) throw new Error(`no tile labelled ${String(label)}`);
  return card as HTMLElement;
}

describe("the figures", () => {
  test("a figure whose question has not answered draws no zero", async () => {
    const silent = () => new Promise(() => {});
    mount({ answers: { work_flow: silent, token_series: silent, decisions: silent } });
    await settle();
    for (const label of [
      "Tasks in progress",
      /^Completed · /,
      /^Tokens · /,
      "Waiting on your decision",
    ]) {
      const card = tile(label);
      expect(card.getAttribute("aria-busy")).toBe("true");
      expect(card.textContent).not.toMatch(/\b0\b/);
    }
  });

  test("the budget meter is the week's, with its reset and the engine's state", async () => {
    mount({
      budget: {
        org: {
          windows: [
            {
              period: "day",
              window: "2026-09-22",
              starts_at: "",
              resets_at: "2026-09-23T00:00:00Z",
              used: 90,
              limit: 100,
              state: "near",
            },
            {
              period: "week",
              window: "2026-W39",
              starts_at: "",
              resets_at: "2026-09-28T00:00:00Z",
              used: 630,
              limit: 1000,
              state: "ok",
            },
          ],
        },
      },
    });
    await settle();
    const card = tile(/^Tokens · /);
    expect(card.textContent).toContain("63% of this week's budget · resets Mon");
    const meter = within(card).getByRole("meter");
    expect(meter.getAttribute("aria-valuenow")).toBe("630");
    expect(meter.getAttribute("aria-valuemax")).toBe("1000");
    // THE ENGINE'S VERDICT, not a threshold of ours: the week is `ok`, so
    // nothing on the tile is in the warning ink although the DAY is near.
    expect(card.innerHTML).not.toMatch(/warning/);
  });

  // NOT REPORTED IS NOT "NO BUDGET": before the engine's first report the
  // line says it has no reading, and offers no way to set a ceiling the
  // company may well have.
  test("before the first budget report the tile says so, and offers nothing", async () => {
    mount();
    await settle();
    const card = tile(/^Tokens · /);
    expect(card.textContent).toContain("Weekly budget not reported yet");
    expect(card.textContent).not.toContain("No weekly budget");
    expect(within(card).queryByRole("link")).toBeNull();
  });

  test("a company with no weekly budget says so and points at the budgets", async () => {
    mount({ budget: UNCAPPED });
    await settle();
    const card = tile(/^Tokens · /);
    const link = within(card).getByRole("link", { name: "No weekly budget" });
    // THE COMPANY'S OWN CEILINGS, the section a spent week opens too.
    expect(link.getAttribute("href")).toBe("#/company/budgets?section=company");
    // NO BARE DELTA before the budget's line: the tile's second line is the
    // budget's, and "−88% No weekly budget" read as a claim about it.
    expect(card.textContent).not.toMatch(/[+−±]\d/);
  });

  // THE WAY IN IS OFFERED TO WHOEVER CAN SET ONE: a reader without
  // Admin gets the fact, not a link to a screen that would refuse them.
  test("a reader who cannot set a budget is told there is none, with no way in", async () => {
    mount({ viewer: { ...JANE, grants: ["worklet"] }, budget: UNCAPPED });
    await settle();
    const card = tile(/^Tokens · /);
    expect(card.textContent).toContain("No weekly budget");
    expect(within(card).queryByRole("link")).toBeNull();
  });

  // NOR TO AN OPERATOR WHO IS NOT A WRITER OF A MANAGED DOCUMENT: Spend ›
  // Budgets would hold every ceiling for them with the managed sentence.
  test("on a managed document a non-writer is told there is no budget, with no way in", async () => {
    mount({
      viewer: { ...JANE, config_writer: false, config_managed_by: ["gitops"] },
      budget: UNCAPPED,
    });
    await settle();
    const card = tile(/^Tokens · /);
    expect(card.textContent).toContain("No weekly budget");
    expect(within(card).queryByRole("link")).toBeNull();
    cleanup();

    mount({
      viewer: { ...JANE, config_writer: true, config_managed_by: ["gitops"] },
      budget: UNCAPPED,
    });
    await settle();
    expect(within(tile(/^Tokens · /)).getByRole("link", { name: "No weekly budget" })).toBeTruthy();
  });

  test("today is asked of the engine as one company day", async () => {
    location.hash = "#/home?range=today";
    mount();
    await settle();
    const series = asked.filter((a) => a.kind === "token_series");
    expect(series.map((a) => a.params.days)).toEqual([1]);
    expect(tile("Completed · today").textContent).toContain("vs yesterday");
  });

  test("the crew is the engine's word for every seat", async () => {
    mount({
      agents: [
        { id: "cto", agent_id: "cto", role: "CTO", handle: "cto", activity: "working" },
        { id: "swe", agent_id: "swe", role: "SWE", handle: "swe", activity: "needs" },
        { id: "devrel", agent_id: "devrel", role: "DevRel", handle: "devrel", activity: "idle" },
      ],
    });
    await settle();
    const card = tile("Agents working now");
    expect(card.textContent).toContain("1/ 3");
    expect(card.textContent).toContain("1 waiting · 1 idle");
    expect(card.textContent).not.toContain("0 stopped");
  });
});

describe("the decisions", () => {
  // GROUPED BY WHO IS ASKING: the asker, the item and the role are said once
  // on the head, and each question carries its answer in place.
  test("an ask is answered by accepting its recommendation, and says so", async () => {
    mount({
      answers: {
        decisions: {
          handle: "jane",
          items: [ask()],
          total: 1,
          capped: false,
          oldest_at: "2026-09-22T09:48:00Z",
        },
      },
    });
    await settle();
    const card = screen.getByText("Needs your decision").closest(".crewlet-card") as HTMLElement;
    const group = card.querySelector(".decision-group") as HTMLElement;
    expect(group.querySelector(".decision-group-who")?.textContent).toBe("CTO");
    expect(group.querySelector(".decision-group-head")?.textContent).toContain(
      "on LEAD-12 · you are the approver",
    );
    // THE STATE ON THE ASKER'S BADGE: a question glyph, never a ring round it.
    expect(group.querySelector(".crewlet-avatar__status--needs")).not.toBeNull();
    expect(group.innerHTML).not.toContain("ring-");
    expect(card.textContent).toContain("Hold the 2.4 release until the scheduler migration lands?");
    expect(card.querySelector(".decision-rec")?.textContent).toBe("Recommended: Hold release");
    await act(async () => {
      fireEvent.click(within(card).getByRole("button", { name: "Accept" }));
    });
    await settle();
    expect(posted).toEqual([
      { tool: "comment_on_work_item", args: { item: "LEAD-12", answers: "c-12", choice: "hold" } },
    ]);
    expect(card.querySelector(".decision-answered")?.textContent).toBe(
      "Answered Hold release (Recommended)",
    );
  });

  test("Change opens every option, hides the recommendation line, and sends the one chosen", async () => {
    mount({ answers: { decisions: { handle: "jane", items: [ask()], total: 1, capped: false } } });
    await settle();
    const card = screen.getByText("Needs your decision").closest(".crewlet-card") as HTMLElement;
    fireEvent.click(within(card).getByRole("button", { name: "Change" }));
    expect(card.querySelector(".decision-rec")).toBeNull();
    const radios = within(card).getAllByRole("radio") as HTMLInputElement[];
    expect(radios.map((r) => r.checked)).toEqual([true, false, false]);
    expect(within(card).getByRole("radio", { name: /Something else/ })).toBeTruthy();
    fireEvent.click(within(card).getByRole("radio", { name: /Ship anyway/ }));
    await act(async () => {
      fireEvent.click(within(card).getByRole("button", { name: "Submit" }));
    });
    await settle();
    expect(posted).toEqual([
      { tool: "comment_on_work_item", args: { item: "LEAD-12", answers: "c-12", choice: "ship" } },
    ]);
    expect(card.querySelector(".decision-answered")?.textContent).toBe("Answered Ship anyway");
  });

  test("Something else sends a reply as the answer, and quotes it", async () => {
    mount({ answers: { decisions: { handle: "jane", items: [ask()], total: 1, capped: false } } });
    await settle();
    const card = screen.getByText("Needs your decision").closest(".crewlet-card") as HTMLElement;
    fireEvent.click(within(card).getByRole("button", { name: "Change" }));
    fireEvent.click(within(card).getByRole("radio", { name: /Something else/ }));
    const submit = within(card).getByRole("button", { name: "Submit" });
    expect(submit.getAttribute("aria-disabled")).toBe("true");
    fireEvent.change(within(card).getByRole("textbox", { name: "Something else" }), {
      target: { value: "Hold, and ship the hotfix alone." },
    });
    await act(async () => {
      fireEvent.click(within(card).getByRole("button", { name: "Submit" }));
    });
    await settle();
    expect(posted).toEqual([
      {
        tool: "comment_on_work_item",
        args: { item: "LEAD-12", answers: "c-12", body: "Hold, and ship the hotfix alone." },
      },
    ]);
    expect(card.querySelector(".decision-answered")?.textContent).toBe(
      "Answered “Hold, and ship the hotfix alone.”",
    );
  });

  test("one ask's options are open at a time", async () => {
    mount({
      answers: {
        decisions: {
          handle: "jane",
          items: [ask(), ask({ comment: "c-13", decision: { ...DECISION, question: "Second?" } })],
          total: 2,
          capped: false,
        },
      },
    });
    await settle();
    const card = screen.getByText("Needs your decision").closest(".crewlet-card") as HTMLElement;
    // ONE HEAD FOR BOTH: the same asker, the same item.
    expect(card.querySelectorAll(".decision-group")).toHaveLength(1);
    const [first, second] = within(card).getAllByRole("button", { name: "Change" });
    fireEvent.click(first!);
    expect(within(card).getAllByRole("radiogroup")).toHaveLength(1);
    fireEvent.click(second!);
    expect(within(card).getAllByRole("radiogroup")).toHaveLength(1);
    expect(within(card).getAllByRole("button", { name: "Change" })).toHaveLength(1);
  });

  // AN ASK TAKES ONE ANSWER: while it is in flight nothing else can send one.
  test("while the answer is being sent, the ask cannot send another", async () => {
    let release: (r: Response) => void = () => {};
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string, init: RequestInit) => {
        const tool = decodeURIComponent(String(url).split("/operator/act/")[1] ?? "");
        posted.push({ tool, args: (JSON.parse(init.body as string) as { args: never }).args });
        return new Promise<Response>((resolve) => (release = resolve));
      }),
    );
    mount({ answers: { decisions: { handle: "jane", items: [ask()], total: 1, capped: false } } });
    await settle();
    const card = screen.getByText("Needs your decision").closest(".crewlet-card") as HTMLElement;
    await act(async () => {
      fireEvent.click(within(card).getByRole("button", { name: "Accept" }));
    });
    const change = within(card).getByRole("button", { name: "Change" });
    expect(change.getAttribute("aria-disabled")).toBe("true");
    await act(async () => {
      fireEvent.click(within(card).getByRole("button", { name: "Accept" }));
    });
    expect(posted.map((p) => p.args.choice)).toEqual(["hold"]);
    await act(async () => {
      release(
        new Response(
          JSON.stringify({ tool: "comment_on_work_item", outcome: "applied", receipt: {} }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      );
    });
    await settle();
  });

  test("the inform line appears only where the ask promised a channel", async () => {
    mount({ answers: { decisions: { handle: "jane", items: [ask()], total: 1, capped: false } } });
    await settle();
    expect(screen.queryByText(/answer posted to #/)).toBeNull();
    cleanup();
    mount({
      answers: {
        decisions: {
          handle: "jane",
          items: [
            ask({ decision: { ...DECISION, inform: { surface: "slack", channel: "releases" } } }),
          ],
          total: 1,
          capped: false,
        },
      },
    });
    await settle();
    expect(screen.getByText("answer posted to #releases")).toBeTruthy();
  });

  test("a reader nobody signed in is told why there is nothing to decide, and asked nothing", async () => {
    mount({
      viewer: { login: "", grants: [], handle: "", owner: "", name: "", kind: "", acts: [] },
    });
    await settle();
    expect(screen.getByText(NOBODY_SENTENCE)).toBeTruthy();
    expect(tile("Waiting on your decision").textContent).toContain("Nobody is signed in");
    expect(tile("Waiting on your decision").textContent).toContain(EMPTY_VALUE);
    expect(asked.some((a) => a.kind === "decisions")).toBe(false);
  });

  // AN UNBOUND READER IS SOMEBODY: their record is their login, and a question
  // put to them or a run waiting on them reaches it — so their decisions are
  // asked for and counted like anybody's, never drawn as nobody's.
  test("a reader the directory binds to no seat is asked what waits on them", async () => {
    mount({
      viewer: {
        login: "ci.release",
        grants: [],
        handle: "",
        owner: "ci.release",
        name: "",
        kind: "",
        acts: [],
      },
      answers: {
        decisions: { handle: "ci.release", items: [ask()], total: 1, capped: false },
      },
    });
    await settle();
    expect(asked.some((a) => a.kind === "decisions")).toBe(true);
    expect(screen.queryByText(NOBODY_SENTENCE)).toBeNull();
    expect(tile("Waiting on your decision").textContent).toContain("1");
  });

  // THE READER IS GREETED BY THEIR OWN NAME, which their row holds and the
  // frame's `GET /auth/session` answered — bound or not — and NEVER BY THE
  // SEAT'S: a person with no name of their own was greeted "Good afternoon,
  // Founder", a role. ONE READ, the frame's: Home asked again for itself. The
  // CONTROL is a bound reader whose row holds no name, greeted bare beside a
  // seat named "Jane". Mutation: greet by the viewer's (the seat's) name and
  // the control is greeted "Jane"; read the session in Home and it is asked
  // twice.
  test("a reader is greeted by their own name, never their seat's", async () => {
    let name: string | undefined = "Bob Smith";
    const sessions = vi.fn(
      async (_input: RequestInfo | URL) =>
        new Response(
          JSON.stringify({ person: "p-bob", name, login: "bob.smith", kind: "person" }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
    );
    vi.stubGlobal("fetch", sessions);
    mount({
      viewer: {
        login: "bob.smith",
        grants: [],
        handle: "",
        owner: "bob.smith",
        name: "",
        kind: "",
        acts: [],
      },
    });
    // The salute follows the wall clock ("Hello" from midnight until five), so
    // only the name is asserted.
    expect(await screen.findByRole("heading", { name: /^(?:Good \w+|Hello), Bob$/ })).toBeTruthy();
    const reads = () =>
      sessions.mock.calls.filter(([url]) => String(url).endsWith("/auth/session"));
    expect(reads()).toHaveLength(1);
    cleanup();
    sessions.mockClear();
    name = undefined;
    mount();
    await waitFor(() => expect(reads()).toHaveLength(1));
    expect(await screen.findByRole("heading", { name: /^(?:Good \w+|Hello)$/ })).toBeTruthy();
    expect(screen.queryByRole("heading", { name: /Jane/ })).toBeNull();
  });

  test("a seat stopped on its budget is a decision with both ways out", async () => {
    mount({
      agents: [
        {
          id: "devrel",
          agent_id: "devrel",
          role: "DevRel",
          handle: "devrel",
          activity: "stopped",
          stopped_reason: "budget",
          budget: {
            windows: [
              {
                period: "day",
                window: "2026-09-22",
                starts_at: "",
                resets_at: "2026-09-23T00:00:00Z",
                used: 100,
                limit: 100,
                state: "refusing",
                refused_at: "2026-09-22T09:00:00Z",
              },
            ],
          },
        },
      ],
    });
    await settle();
    expect(screen.getByText("DevRel stopped: daily token budget exhausted")).toBeTruthy();
    const row = screen
      .getByText("DevRel stopped: daily token budget exhausted")
      .closest(".decision-row") as HTMLElement;
    expect(row.querySelector(".crewlet-avatar__status--stopped")).not.toBeNull();
    expect(row.querySelector(".decision-state")?.textContent).toBe("Stopped");
    expect(tile("Waiting on your decision").textContent).toContain("1");
    // RAISED IN PLACE: the button opens the seat's own ceilings — its day is
    // the window refusing — rather than sending the reader to find it.
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Raise budget" }));
    });
    expect(screen.getByRole("dialog", { name: "Raise DevRel's budget" })).toBeTruthy();
  });

  // A DECISION WAITS ON A READER ONLY WHERE THEY CAN MAKE IT. A stopped seat
  // is raised through `/config` or handed on through `update_work_item`; a
  // reader who can do neither is told it is a condition, never that it waits
  // on them.
  test("a stopped seat the reader cannot act on is a condition, not their decision", async () => {
    mount({
      viewer: { ...JANE, grants: ["worklet"], acts: ["comment_on_work_item"] },
      agents: [
        {
          id: "devrel",
          agent_id: "devrel",
          role: "DevRel",
          handle: "devrel",
          activity: "stopped",
          stopped_reason: "budget",
        },
      ],
    });
    await settle();
    expect(screen.queryByText(/DevRel stopped/)).toBeNull();
    expect(tile("Waiting on your decision").textContent).not.toContain("1");
    const link = document.querySelector<HTMLAnchorElement>(".home-status a");
    expect(link?.textContent).toMatch(/1 condition needs a look/);
  });

  // A MANAGED DOCUMENT (ADR-0030) TAKES THE CEILING OUT OF AN OPERATOR'S
  // HANDS unless their token is one of its writers: the raise control is
  // disabled with the managed sentence, so a seat stopped between turns —
  // nothing to hand on — is a condition for them, never a decision.
  test("on a managed document a stopped seat is not a non-writer's decision", async () => {
    mount({
      viewer: { ...JANE, config_writer: false, config_managed_by: ["gitops"] },
      agents: [{ role: "DevRel", handle: "devrel", activity: "stopped", stopped_reason: "budget" }],
    });
    await settle();
    expect(screen.queryByText(/DevRel stopped/)).toBeNull();
    expect(tile("Waiting on your decision").textContent).not.toContain("1");
    const link = document.querySelector<HTMLAnchorElement>(".home-status a");
    expect(link?.textContent).toMatch(/1 condition needs a look/);
  });

  // AN ASK WITH NO OPTIONS IS ANSWERED IN WORDS, IN PLACE: Reply opens the
  // field under the question, and Submit sends it as the answer to that ask.
  test("an ask with no options is replied to in place, as an answer to that ask", async () => {
    mount({
      answers: {
        decisions: {
          handle: "jane",
          items: [ask({ decision: undefined, body: "Which vendor should we use?" })],
          total: 1,
          capped: false,
        },
      },
    });
    await settle();
    const card = screen.getByText("Needs your decision").closest(".crewlet-card") as HTMLElement;
    expect(within(card).queryByRole("link", { name: "Reply" })).toBeNull();
    fireEvent.click(within(card).getByRole("button", { name: "Reply" }));
    fireEvent.change(within(card).getByRole("textbox", { name: "Reply to CTO" }), {
      target: { value: "Use the one we already pay for." },
    });
    await act(async () => {
      fireEvent.click(within(card).getByRole("button", { name: "Submit" }));
    });
    await settle();
    expect(posted).toEqual([
      {
        tool: "comment_on_work_item",
        args: { item: "LEAD-12", answers: "c-12", body: "Use the one we already pay for." },
      },
    ]);
  });

  // "REVIEW" LANDS ON WHAT IT COUNTED. It pointed at the Inbox filtered on a
  // notice reason no notice carries: Home said a decision was waiting, and
  // the page it sent the reader to drew "decisions 0" and no ask at all. The
  // Inbox has no filter now, and its list opens with Needs you.
  test("Review and Open inbox land on an Inbox that draws the waiting decision", async () => {
    const waiting = { handle: "jane", items: [ask()], total: 1, capped: false };
    mount({ answers: { decisions: waiting } });
    await settle();
    const review = screen.getByRole("link", { name: /Review/ }).getAttribute("href") ?? "";
    const open = screen.getByRole("link", { name: "Open inbox" }).getAttribute("href") ?? "";
    expect(open).toBe(review);
    cleanup();

    location.hash = review;
    mount({ answers: { decisions: waiting }, page: <Inbox /> });
    await settle();
    // NEEDS YOU, over the one decision, opened in the pane with its options
    // to answer, and no updates group beside it.
    expect(review).toBe("#/inbox");
    expect(screen.getByRole("region", { name: /^Needs you/ })).toBeTruthy();
    expect(
      screen.getByRole("heading", {
        name: "Hold the 2.4 release until the scheduler migration lands?",
      }),
    ).toBeTruthy();
    expect(screen.getByRole("button", { name: /Hold release/ })).toBeTruthy();
    expect(screen.queryByRole("region", { name: "Updates" })).toBeNull();
  });
});

describe("live now", () => {
  test("names the item the turn is charged to, never the work key", async () => {
    mount({
      agents: [
        {
          id: "swe",
          agent_id: "swe",
          role: "SWE",
          handle: "swe",
          activity: "working",
          turn: {
            turn_id: "run-7",
            started_at: "2026-09-22T09:00:00Z",
            stage: "phase",
            work_item: { backend: "native", id: "t", key: "ENG-412", project: "ENG" },
          },
          live_call: {
            versions: ZERO_VERSIONS,
            turn_id: "run-7",
            work_key: "wk-notakey",
            phase: "execute",
            iteration: 0,
            round_num: 6,
            rounds_used: 7,
            max_rounds: 25,
            in_progress: true,
            updated_at: "2026-09-22T09:05:00Z",
            work_item: { backend: "native", id: "t", key: "ENG-412", project: "ENG" },
            running_calls: [
              {
                id: "call-7",
                round: 7,
                name: "sandbox.run",
                arguments: '{"cmd":"go test ./..."}',
                started_at: "",
              },
            ],
          },
        },
      ],
    });
    await settle();
    const card = screen.getByText("Live now").closest(".crewlet-card") as HTMLElement;
    expect(card.textContent).toContain("Executing ENG-412");
    expect(card.textContent).toContain("Execute · round 7 of 25");
    expect(card.textContent).toContain("sandbox.run go test ./...");
    expect(card.textContent).not.toContain("wk-notakey");
    expect(within(card).getAllByRole("link")[1]?.getAttribute("href")).toBe("#/live/turns/run-7");
  });
});

describe("the feed", () => {
  test("each row says what the engine recorded, in tokens", async () => {
    mount({
      answers: {
        company_feed: {
          complete: true,
          rows: [
            {
              kind: "completed",
              at: "2026-09-22T09:42:00Z",
              work: {
                id: "h1",
                kind: "completed",
                at: "",
                cursor: "",
                actor: "swe",
                task: "t1",
                key: "ENG-398",
                title: "Webhook fan-out",
                spend: { tokens: 38_200, turns: 1 },
                first_pass: true,
              },
            },
            {
              kind: "created",
              at: "2026-09-22T09:31:00Z",
              work: {
                id: "h2",
                kind: "created",
                at: "",
                cursor: "",
                actor: "cto",
                task: "t2",
                key: "PROD-7",
                title: "Pricing",
                origin: { surface: "slack" },
              },
            },
            {
              kind: "handoff",
              at: "2026-09-22T09:15:00Z",
              work: {
                id: "h3",
                kind: "handoff",
                at: "",
                cursor: "",
                actor: "cto",
                task: "t3",
                key: "ENG-415",
                from: "cto",
                to: "swe",
                reassignments: 1,
                reassignment_budget: 8,
              },
            },
          ],
        },
      },
    });
    await settle();
    const card = screen.getByText("Recent activity").closest(".crewlet-card") as HTMLElement;
    expect(card.textContent).toContain("approved on first review");
    expect(card.textContent).toContain("1 turn · 38.2k tokens");
    expect(card.textContent).toContain("from Slack");
    expect(card.textContent).toContain("hand-off 1 of 8");
    expect(card.textContent).not.toMatch(/\$|cost_usd/);
  });

  // A SCHEDULE'S ROW SAYS WHAT THE SCHEDULER DID: a fold of runs is counted
  // and dated, and a tick it skipped never reads "ran".
  test("a schedule row says what ran, and a skipped tick never says ran", async () => {
    const run = (at: string, schedule: Record<string, unknown>) => ({
      kind: "schedule",
      at,
      schedule: { scope_type: "role", scope_id: "pm", name: "backlog-sweep", ...schedule },
    });
    mount({
      answers: {
        company_feed: {
          complete: true,
          rows: [
            run("2026-09-22T09:40:00Z", {
              target: "cto",
              outcome: "fired",
              runs: 12,
              since: "2026-09-22T02:40:00Z",
            }),
            run("2026-09-22T02:30:00Z", { outcome: "skipped_catchup", runs: 1 }),
          ],
        },
      },
    });
    await settle();
    const rows = document.querySelectorAll(".home-feed-row");
    expect(rows[0]?.textContent).toContain("Schedule backlog-sweep ran 12 times for CTO");
    expect(rows[0]?.querySelector(".home-feed-aside")?.textContent).toMatch(/^since /);
    expect(rows[1]?.textContent).toContain("skipped a missed tick");
    expect(rows[1]?.textContent).not.toMatch(/\bran\b/);
  });

  test("a filter asks the engine for that kind", async () => {
    mount();
    await settle();
    await act(async () => {
      fireEvent.click(screen.getByRole("radio", { name: "Hand-offs" }));
    });
    await settle();
    expect(asked.filter((a) => a.kind === "company_feed").at(-1)?.params.kinds).toBe("handoff");
  });
});

describe("the head", () => {
  test("the greeting is in the zone timestamps are drawn in", () => {
    const at = Date.parse("2026-09-22T20:00:00Z");
    pinZoneForTest("Asia/Tokyo");
    reloadForTest();
    // 05:00 in Tokyo; 22:00 in the browser's own Berlin.
    expect(greeting(at, "Ada Lovelace")).toBe("Good morning, Ada");
    pinZoneForTest("");
    reloadForTest();
    expect(greeting(at, "Ada Lovelace")).toBe("Good evening, Ada");
    expect(greeting(at, "")).toBe("Good evening");
  });

  test("the small hours name no part of the day", () => {
    pinZoneForTest("UTC");
    reloadForTest();
    expect(greeting(Date.parse("2026-10-26T01:11:00Z"), "Miles Rae")).toBe("Hello, Miles");
    expect(greeting(Date.parse("2026-10-26T04:59:00Z"), "")).toBe("Hello");
    expect(greeting(Date.parse("2026-10-26T05:00:00Z"), "Miles")).toBe("Good morning, Miles");
    expect(greeting(Date.parse("2026-10-26T12:00:00Z"), "Miles")).toBe("Good afternoon, Miles");
    expect(greeting(Date.parse("2026-10-26T18:00:00Z"), "Miles")).toBe("Good evening, Miles");
    pinZoneForTest("");
    reloadForTest();
  });

  test("a condition that needs a look links to where it is listed", async () => {
    mount({
      agents: [
        {
          id: "devrel",
          agent_id: "devrel",
          role: "DevRel",
          handle: "devrel",
          activity: "stopped",
          stopped_reason: "paused",
        },
      ],
    });
    await settle();
    const link = document.querySelector<HTMLAnchorElement>(".home-status a");
    expect(link?.textContent).toMatch(/condition needs a look/);
    expect(link?.getAttribute("href")).toMatch(/^#\/inbox/);
  });

  test("the sentence weights what waits on the reader", async () => {
    mount({ answers: { decisions: { handle: "jane", items: [ask()], total: 1, capped: false } } });
    await settle();
    const figure = document.querySelector(".home-status-figure");
    expect(figure?.textContent).toBe("1 decision");
    expect(figure?.closest("p")?.textContent).toBe(
      "Nimbus is running on 3 nodes. 1 decision is waiting on you, and no agents are working right now.",
    );
  });
});

describe("the board being arranged, for the assistant", () => {
  test("is declared while Edit Home is open, read only, and gone when it closes", async () => {
    setFramePage({ screen: "home", title: "Home", path: ["home"], params: {}, query: {} });
    mount();
    await settle();
    expect(pageSnapshot()!.context.draft).toBeUndefined();
    fireEvent.click(screen.getByRole("button", { name: "Edit Home" }));
    await settle();
    const draft = pageSnapshot()!.context.draft!;
    expect(draft).toMatchObject({ kind: "client", step: "arrange" });
    expect(draft.help).toMatch(/done on the board itself/);
    const widgets = draft.fields.find((f) => f.path === "widgets")!;
    expect(widgets).toMatchObject({ kind: "list" });
    expect(widgets.value.split("\n").length).toBeGreaterThan(1);
    // NOTHING ON THE BOARD IS THE ASSISTANT'S TO SET, so the page takes no
    // draft.set at all.
    expect(draft.fields.some((f) => f.editable)).toBe(false);
    expect(pageSnapshot()!.context.accepts).not.toContain("draft.set");
    expect(
      applyPageAction({
        correlation_id: "c1",
        action: "draft.set",
        payload: { values: { widgets: ["Note"] } },
        source_revision: pageSnapshot()!.context.source_revision,
      }),
    ).toMatchObject({ outcome: "no_handler" });
  });
});
