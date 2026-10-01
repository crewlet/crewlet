/**
 * What the landing screen says, and what a person can do from it.
 *
 * Its failure modes are not crashes: a figure that has not arrived drawn as a
 * zero, a budget meter that is the day's labelled as the week's, a live row
 * naming the work key a prompt mentioned rather than the item the turn is on,
 * a decision whose button sends something other than the choice — and a
 * reader nobody bound shown an empty list that claims to have looked.
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { EMPTY_VALUE, LayerHost, ToastProvider } from "@crewlethq/ui";

import type { ReactNode } from "react";
import { Home, greeting } from "./Home.tsx";
import { Inbox } from "~/routes/inbox/Inbox.tsx";
import { NOBODY_SENTENCE } from "./Decisions.tsx";
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

const JANE = {
  operator_id: "U0FOUNDER",
  operator: true,
  handle: "jane",
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
  store.applyHealth({ status: "healthy", nodes: 3 } as never);
  store.applyOrg(ORG as never);
  store.applyAgents(agents as never);
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
          <FrameReadings>
            <Router>{page}</Router>
          </FrameReadings>
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
    expect(link.getAttribute("href")).toBe("#/spend/budgets");
    // NO BARE DELTA before the budget's line: the tile's second line is the
    // budget's, and "−88% No weekly budget" read as a claim about it.
    expect(card.textContent).not.toMatch(/[+−±]\d/);
  });

  // THE WAY IN IS OFFERED TO WHOEVER CAN SET ONE: a reader who is not an
  // operator gets the fact, not a link to a screen that would refuse them.
  test("a reader who cannot set a budget is told there is none, with no way in", async () => {
    mount({ viewer: { ...JANE, operator: false }, budget: UNCAPPED });
    await settle();
    const card = tile(/^Tokens · /);
    expect(card.textContent).toContain("No weekly budget");
    expect(within(card).queryByRole("link")).toBeNull();
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
        { role: "CTO", handle: "cto", activity: "working" },
        { role: "SWE", handle: "swe", activity: "needs" },
        { role: "DevRel", handle: "devrel", activity: "idle" },
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
  test("an option is answered in place, as the choice", async () => {
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
    expect(card.textContent).toContain(
      "CTO asks: Hold the 2.4 release until the scheduler migration lands?",
    );
    expect(card.textContent).toContain("you are the approver");
    expect(card.textContent).toContain("CTO reports to you");
    expect(card.textContent).toContain("recommends Hold release");
    await act(async () => {
      fireEvent.click(within(card).getByRole("button", { name: "Hold release" }));
    });
    await settle();
    expect(posted).toEqual([
      { tool: "comment_on_work_item", args: { item: "LEAD-12", answers: "c-12", choice: "hold" } },
    ]);
  });

  // AN ASK TAKES ONE ANSWER: while one choice is in flight, the ask's other
  // options refuse a press — with a write per button, "Ship anyway" stayed
  // pressable and sent a second, different choice to the same ask.
  test("while one choice is being sent, the ask's other options cannot send another", async () => {
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
      fireEvent.click(within(card).getByRole("button", { name: "Hold release" }));
    });
    const other = within(card).getByRole("button", { name: /Ship anyway/ });
    expect(other.getAttribute("aria-disabled")).toBe("true");
    await act(async () => {
      fireEvent.click(other);
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
    expect(screen.getByText("CTO continues from your answer")).toBeTruthy();
    expect(screen.queryByText(/posts it to #/)).toBeNull();
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
    expect(
      screen.getByText("CTO is woken with your answer and posts it to #releases"),
    ).toBeTruthy();
  });

  test("a reader nobody bound is told why there is nothing to decide, and asked nothing", async () => {
    mount({ viewer: { ...JANE, handle: "", name: "", acts: [] } });
    await settle();
    expect(screen.getByText(NOBODY_SENTENCE.unbound)).toBeTruthy();
    expect(tile("Waiting on your decision").textContent).toContain("Not bound to a person");
    expect(tile("Waiting on your decision").textContent).toContain(EMPTY_VALUE);
    expect(asked.some((a) => a.kind === "decisions")).toBe(false);
  });

  test("a seat stopped on its budget is a decision with both ways out", async () => {
    mount({
      agents: [
        {
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
    expect(screen.getByText("DevRel stopped — daily token budget exhausted")).toBeTruthy();
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
      viewer: { ...JANE, operator: false, acts: ["comment_on_work_item"] },
      agents: [{ role: "DevRel", handle: "devrel", activity: "stopped", stopped_reason: "budget" }],
    });
    await settle();
    expect(screen.queryByText(/DevRel stopped/)).toBeNull();
    expect(tile("Waiting on your decision").textContent).not.toContain("1");
    const link = document.querySelector<HTMLAnchorElement>(".home-status a");
    expect(link?.textContent).toMatch(/1 condition needs a look/);
  });

  // AN ASK WITH NO OPTIONS IS ANSWERED IN WORDS, ON ITS ROW. The row linked
  // to the Inbox by the ask's comment id, which is not the id the Inbox opens
  // a row by — and the pane it would have opened has nowhere to write.
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
    await act(async () => {
      fireEvent.click(within(card).getByRole("button", { name: "Reply" }));
    });
    const dialog = screen.getByRole("dialog");
    fireEvent.change(within(dialog).getByLabelText(/Your reply/), {
      target: { value: "Use the one we already pay for." },
    });
    await act(async () => {
      fireEvent.click(within(dialog).getByRole("button", { name: "Send reply" }));
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
  // the page it sent the reader to drew "decisions 0" and no ask at all.
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
    // THE DECISIONS CHIP, PRESSED, over the one decision — opened in the
    // pane with its options to answer — and no notice group beside it.
    expect(screen.getByRole("radio", { name: /Decisions/ }).getAttribute("aria-checked")).toBe(
      "true",
    );
    expect(
      screen.getByRole("heading", {
        name: "Hold the 2.4 release until the scheduler migration lands?",
      }),
    ).toBeTruthy();
    expect(screen.getByRole("button", { name: /Hold release/ })).toBeTruthy();
    expect(screen.queryByText("Today")).toBeNull();
  });
});

describe("live now", () => {
  test("names the item the turn is charged to, never the work key", async () => {
    mount({
      agents: [
        {
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
            running_call: {
              round: 7,
              name: "sandbox.run",
              arguments: '{"cmd":"go test ./..."}',
              started_at: "",
            },
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

  test("a condition that needs a look links to where it is listed", async () => {
    mount({
      agents: [{ role: "DevRel", handle: "devrel", activity: "stopped", stopped_reason: "paused" }],
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
