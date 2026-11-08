/**
 * A seat's profile, end to end: the tabs its kind has, the three actions and
 * what each sends, and what every figure on it is read from.
 *
 * ONE MOUNT FOR EVERY CASE, over one company: an agent (SWE) in a unit under a
 * lead (CTO), and a person (Jane). Every question the screen asks is recorded,
 * so a case can say what was NOT asked — which is the whole of the rule for a
 * person, whose profile must ask nothing a runtime answers.
 */

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { SCREEN_SCROLL_ID } from "~/lib/scroller.ts";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { SeatScreen } from "./Seat.tsx";
import { contactLabel } from "./seat/Settings.tsx";
import { Shell } from "~/app/Shell.tsx";
import { Router } from "~/app/router.tsx";
import { PAGE_ACTIONS_SLOT } from "~/app/frame/PageActions.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { InboxCountsProvider } from "~/lib/useInboxCounts.ts";
import { OWN_SEAT_MESSAGE, WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type {
  AgentRow,
  CompanyDocument,
  DerivedSeat,
  OrgProjection,
  ScheduleRow,
} from "~/protocol/index.ts";
import { EVERY_QUESTION, session as floors } from "~/protocol/session.ts";
import { emptyDay } from "~/test/myWork.ts";
import { ZERO_VERSIONS } from "~/test/liveCall.ts";
import { settled } from "~/test/settle.ts";
import { ENGINE_HEADERS } from "~/test/engineAnswer.ts";

// EVERY ROW A LIST KEEPS IN ITS VIEW, recorded: jsdom lays nothing out, so
// where a list scrolled to is nothing a case can read, and the call is. The
// real one still runs.
const keptInView = vi.hoisted(() => [] as HTMLElement[]);
vi.mock("~/routes/agents/seat/keepInView.ts", async (importOriginal) => {
  const real = await importOriginal<typeof import("~/routes/agents/seat/keepInView.ts")>();
  return {
    ...real,
    keepInView: (box: HTMLElement, row: HTMLElement) => {
      keptInView.push(row);
      real.keepInView(box, row);
    },
  };
});

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

const derived = (over: Partial<DerivedSeat> & { handle: string; name: string }): DerivedSeat => ({
  kind: "agent",
  placed_by_ref: false,
  manager: "",
  managers: null,
  reports: null,
  auto_reports: null,
  onboarding_chain: null,
  ...over,
});

const GOAL = "Keep the bare-metal provisioner and the scheduler reliable, tested and shipping.";

/** The anonymous projection, as `internal/api` writes it. */
const ORG: OrgProjection = {
  name: "Nimbus",
  roles: [
    { name: "CTO", handle: "cto", manages: ["SWE"], goal: "Own the platform" },
    { name: "Jane Founder", handle: "jane", kind: "human", availability: "CET business hours" },
  ],
  units: [
    {
      id: "Engineering",
      name: "Engineering",
      type: "department",
      children: [
        {
          id: "core",
          name: "Core",
          type: "team",
          roles: [
            {
              name: "SWE",
              handle: "swe",
              goal: GOAL,
              responsibilities: ["Owns provisioner/ end to end"],
              llm: { execute: ["anthropic-main", "openai-fallback"] },
              tool_sources: ["builtin", "mcp:gitlab"],
            },
          ],
        },
      ],
    },
  ],
  derived: {
    seats: [
      derived({ handle: "cto", name: "CTO", reports: ["swe"] }),
      derived({ handle: "jane", name: "Jane Founder", kind: "human" }),
      derived({ handle: "swe", name: "SWE", manager: "cto", managers: ["cto"] }),
    ],
    units: [
      {
        id: "Engineering",
        name: "Engineering",
        type: "department",
        lead: "",
        lead_inherited: false,
        channel: "",
        channel_inherited: false,
        seats: [],
      },
      {
        id: "core",
        name: "Core",
        type: "team",
        lead: "",
        lead_inherited: false,
        channel: "",
        channel_inherited: false,
        seats: ["swe"],
      },
    ],
  },
};

/** The company document as the guarded read serves it: redacted. */
const DOCUMENT: CompanyDocument = {
  name: "Nimbus",
  roles: [
    { name: "CTO", handle: "cto" },
    {
      name: "Jane Founder",
      handle: "jane",
      kind: "human",
      email: "jane@example.com",
      contact: { slack_user_id: "U0FOUNDER" },
    },
  ],
  // EVERY STORED UNIT CARRIES ITS KEY — the engine mints one from the name for
  // a unit that declares none — and a seat's home unit is found by it.
  units: [
    {
      id: "Engineering",
      name: "Engineering",
      children: [
        {
          id: "core",
          name: "Core",
          // INHERITED BY THE UNIT'S DIRECT AGENT MEMBERS, SWE among them.
          mcp_env: { gitlab: { GITLAB_HOST: "${ENGINEERING_GITLAB_HOST}" } },
          roles: [
            {
              name: "SWE",
              handle: "swe",
              email: "swe@example.com",
              workers: ["test-writer", "reviewer-lite"],
              sandbox: { setup: [{ name: "deps" }], max_turns: 40 },
              placement: { labels: { pool: "build" } },
              token_budget: { week: 3_000_000 },
              // What an engine whose redaction took any value CONTAINING `${`
              // for a reference sent: its literal half intact.
              mcp_env: {
                tracker: { API_TOKEN: "__redacted__", AUTH_HEADER: "Bearer sk-live-${SUFFIX}" },
              },
            },
          ],
        },
      ],
    },
  ],
};

const OPERATOR = {
  login: "ops",
  grants: ["admin", "worklet", "doclet"],
  handle: "jane",
  owner: "jane",
  name: "Jane Founder",
};
const EVERY_ACT = [
  "create_work_item",
  "update_work_item",
  "pause_seat",
  "resume_seat",
  "steer_turn",
  "answer_executor",
  "open_dm",
];

/** An answer, or how to answer from the question's own parameters. */
type Answer = unknown | ((params: Record<string, unknown>) => unknown);

/** The turn the sessions below are of, a run id as the engine spells one. */
const SESSION_TURN = "0193f0c2-6a1e-7c55-9d1a-3b6f0e2a4c11";

/** Where on the executor log the sessions below were read. */
const LOG_AT = { stream: "CREWLET_EXECUTOR_LOG", generation: 1, seq: 9 };

/** One turn's executor session as `executor_sessions` lists it: a completed
 *  turn whose session is kept and its conversation's head, which the
 *  conversation's next turn would continue, with `over` laid on top. */
function sessionRow(over: Record<string, unknown> = {}) {
  return {
    turn_id: SESSION_TURN,
    conversation_key: "chatlet:room:eng",
    work_key: "",
    harness: "claude-code",
    harness_version: "2.1.310",
    captured_at: "2026-09-21T09:50:00Z",
    ended_at: "2026-09-21T09:51:00Z",
    ending: { outcome: "completed" },
    ask_excerpt: "Fix the flaky provisioner test.",
    kept: true,
    object_bytes: 41234,
    in_progress: false,
    continued_by: "next_turn",
    continuable: true,
    continues_in: "any_box",
    ...over,
  };
}

/** A page of a seat's executor sessions, with `over` laid on top. */
function sessionsList(sessions: unknown[], over: Record<string, unknown> = {}) {
  return {
    handle: "swe",
    sessions,
    read_level: "stale",
    position: LOG_AT,
    complete: true,
    ...over,
  };
}

/** A page of one session's transcript as `executor_session` answers it, with
 *  `over` laid on top. */
function transcriptPage(over: Record<string, unknown> = {}) {
  return {
    handle: "swe",
    session: sessionRow(),
    state: "read",
    entries: [],
    from: 0,
    bounds: {
      entries: 200,
      bytes: 1 << 20,
      entry_bytes: 16 << 10,
      line_bytes: 32 << 20,
      read_bytes: 256 << 20,
    },
    read_level: "stale",
    position: LOG_AT,
    complete: true,
    ...over,
  };
}

/** One entry of a transcript, `kind` the model's unless named. */
function said(text: string, kind = "model") {
  return { kind, text, bytes: text.length };
}

/**
 * What the engine answers a question no case names — each in the shape the
 * wire carries, so a case about one card is not a case about another card
 * reading a malformed answer.
 */
const DEFAULTS: Record<string, unknown> = {
  // AN ADMIN BOUND TO NO SEAT — a deployment's own token — so a case about a
  // card reads the card whole; a case about a lock names the reader it means.
  viewer: {
    login: "token:ops",
    grants: ["admin", "worklet", "doclet"],
    handle: "",
    owner: "",
    acts: [],
  },
  work_items: { items: [], total_hint: 0 },
  seat_activity: { since: "", until: "", days: 7, seats: [], quantile_resolution: 0.06 },
  agent_memory: {
    id: "swe",
    diary: [],
    diary_total: 0,
    episodes: [],
    episodes_total: 0,
    skills: [],
    skills_total: 0,
    counterparties: [],
    counterparties_total: 0,
    latest_reflection: null,
    onboarded_at: "",
    held_by: "node-1",
  },
  // THE MEMORY TAB LISTS THE SEAT'S EXECUTOR SESSIONS under its memory, so
  // every case that opens it is answered a list — none, here — and a session
  // the address names reads as one whose turn captured nothing.
  executor_sessions: sessionsList([]),
  executor_session: (params: Record<string, unknown>) =>
    transcriptPage({
      session: sessionRow({ turn_id: params.turn_id, kept: false, object_bytes: 0 }),
      state: "none",
      why: "nothing of this turn's session was captured",
    }),
  turns: { turns: [], next: null },
  agent: { llm_history: [], next: "" },
  inbox: { handle: "", notices: [], primary_reasons: [], unread: 0, primary: 0 },
  work_my_work: emptyDay("swe"),
  tokens: {
    since: "2026-09-15T00:00:00Z",
    until: "2026-09-22T00:00:00Z",
    totals: { input_tokens: 0, output_tokens: 0, total_tokens: 0, calls: 0 },
    by_phase: [],
    by_model: [],
    by_worker: [],
    by_agent: [],
  },
};

let asked: { what: string; params: Record<string, unknown> }[];
let posted: { tool: string; args: Record<string, unknown> }[];
let reply: { status: number; body: unknown };

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  keptInView.length = 0;
  asked = [];
  posted = [];
  reply = { status: 200, body: { tool: "pause_seat", outcome: "applied" } };
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init?: RequestInit) => {
      // A READ IS ANSWERED, never parsed as an act: a person's seat reads
      // `/iam/seats` for a directory reader, and a body-less GET handed to
      // `JSON.parse` threw inside the stub, which the page swallowed as a
      // failed read while every case here passed by accident.
      if ((init?.method ?? "GET").toUpperCase() === "GET") {
        const path = new URL(String(url), "http://engine.test").pathname;
        return new Response(JSON.stringify(path === "/iam/seats" ? { seats: [] } : {}), {
          status: path === "/iam/seats" ? 200 : 404,
          headers: { ...ENGINE_HEADERS, "Content-Type": "application/json" },
        });
      }
      const tool = decodeURIComponent(String(url).split("/operator/act/")[1] ?? "");
      const body = JSON.parse(init?.body as string) as { args: Record<string, unknown> };
      posted.push({ tool, args: body.args });
      return new Response(JSON.stringify(reply.body), {
        status: reply.status,
        headers: { ...ENGINE_HEADERS, "Content-Type": "application/json" },
      });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
  document.title = "";
});

function mount(
  hash: string,
  {
    answers = {},
    agents = [],
    schedules = [],
    shell = false,
    org = ORG,
  }: {
    answers?: Partial<Record<string, Answer>>;
    agents?: Partial<AgentRow>[];
    schedules?: ScheduleRow[];
    shell?: boolean;
    org?: OrgProjection;
  } = {},
) {
  location.hash = hash;
  const store = new Store();
  store.applyOrg(org);
  if (agents.length) store.applySeats(agents);
  if (schedules.length) store.applySchedules({ schedules });
  store.setConnected(true);
  const socket = new LiveSocket(store);
  (
    socket as unknown as {
      query: (what: string, params?: Record<string, unknown>) => Promise<unknown>;
    }
  ).query = (what, params = {}) => {
    asked.push({ what, params });
    const answer = what in answers ? answers[what] : DEFAULTS[what];
    return Promise.resolve(
      typeof answer === "function"
        ? (answer as (p: Record<string, unknown>) => unknown)(params)
        : (answer ?? {}),
    );
  };
  // `#/company/org/seats/{handle}`: the handle is the fifth segment.
  const handle = decodeURIComponent(hash.split("/")[4]!.split("?")[0]!);
  const screenEl = <SeatScreen handle={handle} />;
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        {shell ? (
          <Shell>{screenEl}</Shell>
        ) : (
          <ViewerProvider>
            <InboxCountsProvider>{screenEl}</InboxCountsProvider>
          </ViewerProvider>
        )}
      </Router>
    </ClientContext.Provider>,
  );
  return { store };
}

/**
 * Lets every read the screen asked land, and the renders they cause, until a
 * pass asks nothing more: every answer is a stub, so a read asked again would
 * move the count across a boundary. What a case says was NOT asked is read
 * here.
 */
async function settle() {
  await settled(() => asked.length);
}

const tabNames = () => screen.getAllByRole("tab").map((t) => t.textContent);

/**
 * The Turns card, under the Activity tab's Turns section: the section's own
 * heading says "Turns" too, so the card is the one heading named so that heads
 * a card.
 */
function turnsCard(): HTMLElement {
  const card = screen
    .getAllByRole("heading", { name: "Turns" })
    .map((h) => h.closest<HTMLElement>(".crewlet-card"))
    .find((c) => c !== null);
  expect(card, "no card is headed Turns").toBeDefined();
  return card!;
}
const askedFor = (what: string) => asked.filter((a) => a.what === what);

/** A stat tile by its label: the tile's own box. */
function tile(label: string): HTMLElement {
  const el = screen.getByText(label).closest(".crewlet-statcard");
  expect(el, `no tile is labelled ${label}`).not.toBeNull();
  return el as HTMLElement;
}

const WORKING: Partial<AgentRow> = {
  id: "swe",
  role: "SWE",
  handle: "swe",
  agent_id: "a-swe",
  activity: "working",
  stopped_reason: null,
  paused: null,
  turn: {
    turn_id: "t-7",
    work_item: { backend: "native", id: "i-412", key: "ENG-412", project: "ENG" },
    started_at: "2026-09-21T09:45:50Z",
    stage: "phase",
  },
  live_call: {
    versions: ZERO_VERSIONS,
    turn_id: "t-7",
    phase: "execute",
    iteration: 1,
    model: "anthropic-main",
    in_progress: true,
    round_num: 6,
    rounds_used: 7,
    max_rounds: 25,
    started_at: "2026-09-21T09:46:00Z",
    updated_at: "2026-09-21T09:51:31Z",
    input_tokens: 0,
    output_tokens: 0,
    total_tokens: 2_500,
    tool_executions: [
      {
        name: "knowledge.search",
        round: 1,
        arguments: { q: "PXE retry backoff" },
        result: "",
        success: true,
        started_at: "2026-09-21T09:48:02Z",
        duration_ms: 300,
      },
      {
        name: "sandbox.run",
        round: 5,
        arguments: { cmd: "go test ./provisioner/..." },
        result: "exit status 1",
        success: false,
        error: "exit status 1",
        started_at: "2026-09-21T09:49:30Z",
        duration_ms: 85_000,
      },
    ],
    running_calls: [
      {
        id: "call-7",
        round: 7,
        name: "sandbox.run",
        arguments: '{"cmd":"go test ./provisioner/... -run Timeout"}',
        started_at: "2026-09-21T09:51:31Z",
      },
    ],
  } as AgentRow["live_call"],
};

// ---------------------------------------------------------------------------
// The tabs a kind has
// ---------------------------------------------------------------------------

// A PERSON RUNS NO TURN: the engine never spawns one, so their profile asks
// nothing a runtime answers — every one of those reads would be a question
// about a thing that cannot exist, and the tab that draws them is absent.
test("a person's profile has three tabs and asks nothing a runtime answers", async () => {
  // A BOOKMARK NAMING AN AGENT'S SECTION lands on Overview rather than a blank.
  mount("#/company/org/seats/jane?tab=turns", { answers: { viewer: OPERATOR, config: DOCUMENT } });
  await settle();
  expect(tabNames().map((n) => n?.replace(/\d+$/, ""))).toEqual(["Overview", "Work", "Settings"]);
  const selected = screen
    .getAllByRole("tab")
    .find((t) => t.getAttribute("aria-selected") === "true");
  expect(selected?.textContent).toBe("Overview");
  for (const what of [
    "seat_activity",
    "agent_memory",
    "agent",
    "turns",
    "tokens",
    "executor_sessions",
    "work_item_turns",
  ]) {
    expect(askedFor(what), `a person's profile asked ${what}`).toEqual([]);
  }
  // Messaged and given work like anybody — and never paused.
  expect(screen.getByRole("button", { name: "Message" })).toBeTruthy();
  expect(screen.getByRole("button", { name: "Assign task" })).toBeTruthy();
  expect(screen.queryByRole("button", { name: /^Pause/ })).toBeNull();
});

// FOUR TABS, EACH A GROUP OF SECTIONS: Work holds Code, Activity its Turns
// then Memory, Settings its setup then Schedules and Access.
test("an agent's profile has four tabs, and Work counts the engine's total rather than a page", async () => {
  mount("#/company/org/seats/swe", {
    answers: {
      work_items: {
        // EACH WITH ITS ID, as the engine sends every task: the list is keyed
        // by it, and rows without one drew React's missing-key warning.
        items: [
          { id: "t-412", key: "ENG-412", title: "Retry PXE boot", status: "in_progress" },
          { id: "t-401", key: "ENG-401", title: "Migrate scheduler state", status: "todo" },
        ],
        total_hint: 7,
      },
    },
  });
  await settle();
  expect(tabNames().map((n) => n?.replace(/\d+$/, ""))).toEqual([
    "Overview",
    "Work",
    "Activity",
    "Settings",
  ]);
  const work = screen.getAllByRole("tab").find((t) => t.textContent?.startsWith("Work"));
  await waitFor(() => expect(work?.textContent).toBe("Work7"));
  // AND THE OVERVIEW'S CARD OFFERS ALL SEVEN, from the same answer.
  expect(screen.getByRole("link", { name: "All 7" }).getAttribute("href")).toBe(
    "#/worklet/tasks?assignee=swe",
  );
});

// A SECTION'S WORD IS AN ADDRESS: `tab=memory`, pasted from before the tabs
// were grouped, opens Activity, scrolled to Memory under the Turns, and the
// address is said again as the tab it is. The scroll is HELD while the tab
// fills, since one assignment landed short of Memory on a page whose cards
// below it had not drawn yet. Mutation: drop Memory from `SECTION_TABS` and
// the page lands on Overview; scroll once, and the page stays where it was
// clamped.
test("a tab= naming a section lands on the tab holding it, scrolled to the section", async () => {
  const PLACE = 640;
  let top = 0;
  /** The page's height so far: short until the cards under Memory have drawn. */
  let room = 120;
  const rect = vi.spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element,
  ) {
    const y = this.id === "seat-section-memory" ? PLACE - top : 0;
    return { top: y, bottom: y, left: 0, right: 0, width: 0, height: 0, x: 0, y } as DOMRect;
  });
  try {
    mount("#/company/org/seats/swe?tab=memory", { shell: true });
    const scroller = document.getElementById(SCREEN_SCROLL_ID)!;
    Object.defineProperty(scroller, "scrollTop", {
      configurable: true,
      get: () => top,
      set: (v: number) => {
        top = Math.min(v, room);
      },
    });
    await settle();
    await waitFor(() => expect(location.hash).toBe("#/company/org/seats/swe?tab=activity"));
    const selected = screen
      .getAllByRole("tab")
      .find((t) => t.getAttribute("aria-selected") === "true");
    expect(selected?.textContent).toBe("Activity");
    // THE TAB FILLS: the page grows tall enough to scroll Memory to its top.
    await waitFor(() => expect(top).toBe(room));
    room = 5000;
    await waitFor(() => expect(top).toBe(PLACE));
    const turns = document.getElementById("seat-section-turns")!;
    const memory = document.getElementById("seat-section-memory")!;
    expect(
      Boolean(turns.compareDocumentPosition(memory) & Node.DOCUMENT_POSITION_FOLLOWING),
      "Memory follows the Turns",
    ).toBe(true);
  } finally {
    rect.mockRestore();
  }
});

// ---------------------------------------------------------------------------
// The actions
// ---------------------------------------------------------------------------

test("Events opens the event log narrowed to this seat", async () => {
  mount("#/company/org/seats/swe");
  await settle();
  fireEvent.click(screen.getByRole("button", { name: "More on SWE" }));
  fireEvent.click(screen.getByRole("menuitem", { name: /^Events/ }));
  expect(location.hash).toBe("#/live/events?seat=swe");
});

// ON THE ORG CHART, the seat selected and its editor open: a selection alone
// never opens an editor, so the address says so (`editor=open`).
test("Edit in org opens the org editor on this seat", async () => {
  mount("#/company/org/seats/swe");
  await settle();
  fireEvent.click(screen.getByRole("button", { name: "More on SWE" }));
  fireEvent.click(screen.getByRole("menuitem", { name: /^Edit in org/ }));
  expect(location.hash).toBe("#/company/org?seat=swe&editor=open");
});

/** The sentence a held control is described by, for a reader who meets it. */
function heldBy(button: HTMLElement): string {
  return (button.getAttribute("aria-describedby") ?? "")
    .split(/\s+/)
    .map((id) => document.getElementById(id)?.textContent ?? "")
    .join(" ");
}

// THE SEAT'S OWN CONTROLS INCLUDED: Pause, and Pause and stop while a turn
// runs, are drawn held with the same sentence as Message and Assign task.
test("a reader who may not act sees every action held, with the reason", async () => {
  mount("#/company/org/seats/swe", {
    answers: { viewer: { login: "", grants: [], handle: "", owner: "", acts: [] } },
    agents: [WORKING],
  });
  await settle();
  for (const name of ["Message", "Assign task", "Pause SWE"]) {
    const button = screen.getByRole("button", { name });
    expect(button.getAttribute("aria-disabled"), `${name} is pressable`).toBe("true");
    expect(heldBy(button), name).toContain(WRITE_REASONS.anonymous);
  }
  // PAUSE AND STOP IS IN THE BAR'S "MORE", held there with the same sentence.
  const stop = headerEntry("Pause and stop");
  expect(stop.getAttribute("aria-disabled")).toBe("true");
  expect(`${stop.textContent} ${heldBy(stop)}`).toContain(WRITE_REASONS.anonymous);
});

// FIND, CHOOSE, THEN SAY WHY: the matches are the dialog's content while it
// is finding (in its flow, so all eight are readable), and the reason is
// asked once there is a hand-off for it to be the reason for. The hand-off is
// conditional on the version a read of the chosen task returned.
test("Assign task finds a task, then asks why, and hands it over against the version read", async () => {
  // Each hit with its id, as the engine sends one: the picker's options are
  // keyed by it.
  const hits = Array.from({ length: 8 }, (_, i) => ({
    id: `t-${20 + i}`,
    key: `ENG-${20 + i}`,
    title: `PXE task ${i}`,
    assignee: i === 0 ? "cto" : "",
  }));
  mount("#/company/org/seats/swe", {
    answers: {
      viewer: { ...OPERATOR, acts: EVERY_ACT },
      work_search: { hits, available: true, mode: "hybrid" },
      // THE TASK ASKED FOR, by the id a Worklet task is asked by, answered as
      // the engine answers it: its id and key both.
      work_item: (p: Record<string, unknown>) => {
        const hit = hits.find((h) => h.id === p.id || h.key === p.id)!;
        return {
          task: { id: hit.id, key: hit.key, version: 4, title: hit.title, assignee: "cto" },
          complete: true,
        };
      },
    },
    agents: [WORKING],
  });
  await settle();
  fireEvent.click(screen.getByRole("button", { name: "Assign task" }));
  const dialog = within(await screen.findByRole("dialog", { name: "Assign a task to SWE" }));
  expect(dialog.queryByLabelText(/Why it is theirs now/)).toBeNull();
  const find = dialog.getByRole("combobox", { name: /Find a task/ });
  fireEvent.change(find, { target: { value: "PXE" } });
  await waitFor(() => expect(dialog.getAllByRole("option")).toHaveLength(8));
  const list = dialog.getByRole("listbox");
  expect(
    list.closest(".assign-seat-find"),
    "the matches are laid out in the dialog's flow",
  ).not.toBeNull();
  // THE KEYBOARD TAKES A MATCH: the first is highlighted as the list opens.
  fireEvent.keyDown(find, { key: "Enter" });
  const why = await dialog.findByLabelText(/Why it is theirs now/);
  expect(dialog.getByText("ENG-20 is with CTO now.")).toBeTruthy();
  fireEvent.change(why, { target: { value: "you know the loop" } });
  await waitFor(() =>
    expect(dialog.getByRole("button", { name: "Assign" }).getAttribute("aria-disabled")).not.toBe(
      "true",
    ),
  );
  fireEvent.click(dialog.getByRole("button", { name: "Assign" }));
  await waitFor(() => expect(posted).toHaveLength(1));
  expect(posted[0]).toEqual({
    tool: "update_work_item",
    args: { item: "ENG-20", assignee: "swe", if_match: 4, reason: "you know the loop" },
  });
});

// THE TASKS THAT CAN MOVE COME FIRST. For "retry", seven of eight matches were
// already the seat's and ranked first, and choosing one only said so — so the
// seat's own are listed after every other match, drawn but disabled and saying
// why, and the search asks for a page wide enough that eight movable tasks
// still fill the list.
test("Assign task lists the tasks it can hand over first, and the seat's own as disabled", async () => {
  // Each with its id, as the engine sends one: the options are keyed by it.
  const hits = [
    ...Array.from({ length: 7 }, (_, i) => ({
      id: `t-${10 + i}`,
      key: `ENG-${10 + i}`,
      title: `retry ${i}`,
      assignee: "swe",
    })),
    { id: "t-30", key: "ENG-30", title: "retry elsewhere", assignee: "cto" },
    { id: "t-31", key: "ENG-31", title: "retry unowned", assignee: "" },
  ];
  mount("#/company/org/seats/swe", {
    answers: {
      viewer: { ...OPERATOR, acts: EVERY_ACT },
      work_search: { hits, available: true, mode: "hybrid" },
    },
    agents: [WORKING],
  });
  await settle();
  fireEvent.click(screen.getByRole("button", { name: "Assign task" }));
  const dialog = within(await screen.findByRole("dialog", { name: "Assign a task to SWE" }));
  fireEvent.change(dialog.getByRole("combobox", { name: /Find a task/ }), {
    target: { value: "retry" },
  });
  await waitFor(() => expect(dialog.getAllByRole("option")).toHaveLength(8));
  const options = dialog.getAllByRole("option");
  expect(options[0]?.textContent).toContain("ENG-30");
  expect(options[1]?.textContent).toContain("ENG-31");
  expect(options[0]?.getAttribute("aria-disabled")).toBeNull();
  for (const own of options.slice(2)) {
    expect(own.getAttribute("aria-disabled")).toBe("true");
    expect(own.textContent).toContain("already theirs");
  }
  // CHOOSING ONE DOES NOTHING: it is not a choice.
  fireEvent.mouseDown(options[2]!);
  expect(dialog.queryByLabelText(/Why it is theirs now/)).toBeNull();
  expect(askedFor("work_search").at(-1)?.params).toMatchObject({ q: "retry", limit: 25 });
});

// WHOSE A TASK IS, ONCE THE READ LANDS, IS THE READ'S: the list was drawn from
// the search's answer, and the task can have moved since. A hit naming CTO for
// a task this seat has since been handed is held, from the read, with the
// sentence that says why.
test("Assign task decides whose the task is from the read, not the search's answer", async () => {
  const assignee: Record<string, string> = { "ENG-20": "cto", "ENG-21": "swe" };
  mount("#/company/org/seats/swe", {
    answers: {
      viewer: { ...OPERATOR, acts: EVERY_ACT },
      work_search: {
        hits: [
          { id: "t-20", key: "ENG-20", title: "Moved away", assignee: "swe" },
          { id: "t-21", key: "ENG-21", title: "Moved here", assignee: "cto" },
        ],
        available: true,
        mode: "hybrid",
      },
      work_item: (p: Record<string, unknown>) => ({
        task: {
          id: `t-${String(p.id).slice(4)}`,
          key: p.id,
          version: 7,
          title: "t",
          assignee: assignee[p.id as string],
        },
        complete: true,
      }),
    },
    agents: [WORKING],
  });
  await settle();
  fireEvent.click(screen.getByRole("button", { name: "Assign task" }));
  const dialog = within(await screen.findByRole("dialog", { name: "Assign a task to SWE" }));
  const find = dialog.getByRole("combobox", { name: /Find a task/ });
  const pick = async (key: string) => {
    fireEvent.change(find, { target: { value: "moved" } });
    await waitFor(() => expect(dialog.getAllByRole("option")).toHaveLength(2));
    // THE LIST TAKES A ROW ON THE PRESS, as the kit's combobox does.
    fireEvent.mouseDown(dialog.getByRole("option", { name: new RegExp(key) }));
  };
  const assign = () => dialog.getByRole("button", { name: "Assign" });

  // A HIT THE SEARCH ANSWERED AS THE SEAT'S is not offered: it is disabled.
  fireEvent.change(find, { target: { value: "moved" } });
  await waitFor(() => expect(dialog.getAllByRole("option")).toHaveLength(2));
  expect(dialog.getByRole("option", { name: /ENG-20/ }).getAttribute("aria-disabled")).toBe("true");

  await pick("ENG-21");
  await waitFor(() => expect(dialog.getByText(/ENG-21 is already assigned to SWE\./)).toBeTruthy());
  expect(assign().getAttribute("aria-disabled")).toBe("true");
});

/** The pause dialog, opened from the page bar's Pause. */
async function openPause() {
  fireEvent.click(screen.getByRole("button", { name: "Pause SWE" }));
  return within(await screen.findByRole("dialog", { name: "Pause SWE" }));
}

// A TURN STOPPED AT ITS NEXT ROUND LOSES WHAT IT HAD NOT DONE, so stopping it
// is a separate, explicit choice — and the dialog says what happens without.
// Pause and stop is that choice made up front, and it asks first too.
test("pausing sends stop_running only when the box is ticked", async () => {
  mount("#/company/org/seats/swe", {
    answers: { viewer: { ...OPERATOR, acts: EVERY_ACT } },
    agents: [WORKING],
  });
  await settle();

  let dialog = await openPause();
  expect(dialog.getByText(/Otherwise a running turn finishes first\./)).toBeTruthy();
  fireEvent.change(dialog.getByLabelText(/Why/), { target: { value: "rolling the fleet" } });
  fireEvent.click(dialog.getByRole("button", { name: "Pause" }));
  await waitFor(() => expect(posted).toHaveLength(1));
  expect(posted[0]).toEqual({
    tool: "pause_seat",
    args: { handle: "swe", reason: "rolling the fleet" },
  });
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());

  dialog = await openPause();
  fireEvent.click(dialog.getByRole("checkbox", { name: /Also stop the running turn/ }));
  fireEvent.click(dialog.getByRole("button", { name: "Pause" }));
  await waitFor(() => expect(posted).toHaveLength(2));
  expect(posted[1]).toEqual({ tool: "pause_seat", args: { handle: "swe", stop_running: true } });
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());

  fireEvent.click(headerEntry("Pause and stop"));
  dialog = within(await screen.findByRole("dialog", { name: "Pause SWE and stop its turn" }));
  expect(posted).toHaveLength(2);
  fireEvent.click(dialog.getByRole("button", { name: "Pause and stop" }));
  await waitFor(() => expect(posted).toHaveLength(3));
  expect(posted[2]).toEqual({ tool: "pause_seat", args: { handle: "swe", stop_running: true } });
});

// THREE OUTCOMES, and only one of them keeps the dialog: applied and pending
// are both the change in hand, and a refusal is the engine's sentence, said
// where the reader can fix what it names.
test("a pause the engine refuses keeps the dialog and says why; a pending one closes it", async () => {
  mount("#/company/org/seats/swe", {
    answers: { viewer: { ...OPERATOR, acts: EVERY_ACT } },
    agents: [WORKING],
  });
  await settle();

  reply = {
    status: 409,
    body: { error: "conflict", tool: "pause_seat", detail: "SWE is already paused." },
  };
  let dialog = await openPause();
  fireEvent.click(dialog.getByRole("button", { name: "Pause" }));
  await waitFor(() => expect(dialog.getByRole("alert")).toBeTruthy());
  expect(screen.getByRole("dialog", { name: "Pause SWE" })).toBeTruthy();
  fireEvent.click(dialog.getByRole("button", { name: "Cancel" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());

  reply = { status: 202, body: { tool: "pause_seat", outcome: "pending" } };
  dialog = await openPause();
  fireEvent.click(dialog.getByRole("button", { name: "Pause" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(posted).toHaveLength(2);
});

test("a paused seat says who paused it and why, and offers Resume", async () => {
  mount("#/company/org/seats/swe", {
    answers: { viewer: { ...OPERATOR, acts: EVERY_ACT } },
    agents: [
      {
        id: "swe",
        agent_id: "swe",
        role: "SWE",
        handle: "swe",
        activity: "stopped",
        stopped_reason: "paused",
        paused: {
          by_kind: "human",
          by: "jane",
          at: "2026-09-21T09:00:00Z",
          reason: "rolling the fleet",
          stop_running: false,
        },
      },
    ],
  });
  await settle();
  // THE PERSON, by name — `paused.by` is an address.
  const notice = document.querySelector(".prof-notices") as HTMLElement;
  expect(notice.textContent).toContain("Paused by Jane Founder");
  expect(notice.textContent).toContain("rolling the fleet");
  expect(screen.getAllByText("Paused").length).toBeGreaterThan(0);
  // RESUMING ONE SEAT IS ONE PRESS: it changes nothing a person has to weigh.
  fireEvent.click(screen.getByRole("button", { name: "Resume SWE" }));
  await waitFor(() => expect(posted).toEqual([{ tool: "resume_seat", args: { handle: "swe" } }]));
});

// A PAUSE THE RUNNING TURN HAS NOT REACHED YET: the seat is still working, so
// its page says the pause waits for the turn, offers Resume, and offers Pause
// and stop, which a second pause makes of the first. Mutation: read the
// pause alone and Pause and stop is gone; read the activity alone and Resume
// is Pause again; drop the note from the pill and it reads "Working" alone.
test("a paused seat whose turn still runs offers Resume and Pause and stop, and says it pauses after this turn", async () => {
  mount("#/company/org/seats/swe", {
    answers: { viewer: { ...OPERATOR, acts: EVERY_ACT } },
    agents: [
      {
        ...WORKING,
        paused: {
          by_kind: "human",
          by: "jane",
          at: "2026-09-21T09:00:00Z",
          reason: "rolling the fleet",
          stop_running: false,
        },
      },
    ],
  });
  await settle();
  // THE PILL SAYS IT TOO: "Working" alone beside Resume read as nobody's pause.
  expect(document.querySelector(".prof-title")?.textContent).toContain("Working · pausing");
  const notice = document.querySelector(".prof-notices") as HTMLElement;
  expect(notice.textContent).toContain("Pauses after this turn");
  expect(notice.textContent).toContain("Paused by Jane Founder");
  expect(notice.textContent).toContain("Its running turn finishes first");
  // THE QUOTED REASON ENDS ITS OWN SENTENCE: no full stop after the quote.
  expect(notice.textContent).toContain("“rolling the fleet.” Its running turn");
  expect(screen.queryByRole("button", { name: "Pause SWE" })).toBeNull();
  const resume = screen.getByRole("button", { name: "Resume SWE" });
  expect(resume.getAttribute("aria-disabled")).not.toBe("true");
  const stop = headerEntry("Pause and stop");
  expect(stop.getAttribute("aria-disabled")).not.toBe("true");
  fireEvent.click(stop);
  const dialog = within(await screen.findByRole("dialog", { name: "Pause SWE and stop its turn" }));
  fireEvent.click(dialog.getByRole("button", { name: "Pause and stop" }));
  await waitFor(() => expect(posted).toHaveLength(1));
  expect(posted[0]).toEqual({ tool: "pause_seat", args: { handle: "swe", stop_running: true } });
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  fireEvent.click(screen.getByRole("button", { name: "Resume SWE" }));
  await waitFor(() => expect(posted).toHaveLength(2));
  expect(posted[1]).toEqual({ tool: "resume_seat", args: { handle: "swe" } });
});

const PAUSED_SWE: Partial<AgentRow> = {
  id: "swe",
  agent_id: "swe",
  role: "SWE",
  handle: "swe",
  activity: "stopped",
  stopped_reason: "paused",
  paused: {
    by_kind: "human",
    by: "jane",
    at: "2026-09-21T09:00:00Z",
    reason: "rolling the fleet",
    stop_running: false,
  },
};

// ONE FACT, ONE TONE: a stopped seat is the red state in the seat vocabulary,
// and its notice was amber under a red pill.
test("a pause is said in the stopped state's own tone", async () => {
  mount("#/company/org/seats/swe", { agents: [PAUSED_SWE] });
  await settle();
  const notice = document.querySelector(".prof-notices .crewlet-callout") as HTMLElement;
  expect(notice.className).toContain("crewlet-callout--danger");
});

// ONE BANNER PER CAUSE, IN PLAIN WORDS. A provider stop and the failed turn
// that came with it were two banners, the first headed by the machine code
// ("server: llm chain exhausted ..."). The raw message is one press away.
test("a fault stop is one banner in plain words, the engine's message behind a disclosure", async () => {
  mount("#/company/org/seats/swe", {
    agents: [
      {
        id: "swe",
        agent_id: "swe",
        role: "SWE",
        handle: "swe",
        activity: "stopped",
        stopped_reason: "provider",
        last_error: {
          kind: "server",
          detail: "llm chain exhausted after 1 provider (default)",
          phase: "execute",
          turn_id: "t1",
          at: "2026-09-21T09:00:00Z",
          event_id: "e1",
        },
      },
    ],
  });
  await settle();
  const callouts = document.querySelectorAll(".prof-notices .crewlet-callout");
  expect(callouts).toHaveLength(1);
  const text = callouts[0]!.textContent ?? "";
  expect(text).toContain("This seat is stopped: its models are at their usage limit.");
  expect(text).toContain("Its model provider ran but gave no usable answer.");
  expect(text).not.toContain("server:");
  expect(text).not.toContain("llm chain exhausted");
  fireEvent.click(screen.getByRole("button", { name: /Technical details/ }));
  await waitFor(() => expect(screen.getByText(/llm chain exhausted/)).toBeTruthy());
});

/** An entry of the seat's own "More" in the page bar, which holds the turn's controls. */
function headerEntry(name: string): HTMLElement {
  fireEvent.click(screen.getByRole("button", { name: "More on SWE" }));
  return screen.getByRole("menuitem", { name: new RegExp(`^${name}`) });
}

/** The page bar's own "More" — what a phone's bar folds every other action into. */
function pageMore(): string[] {
  fireEvent.click(screen.getByRole("button", { name: "More on this page" }));
  // Each entry's label, without the sentence a described entry carries under it.
  return screen
    .getAllByRole("menuitem")
    .map((el) => el.querySelector(".crewlet-menu__label")?.textContent ?? el.textContent ?? "");
}

// THE BAR KEEPS PAUSE OR RESUME, its "More" the rest of the seat's controls:
// with Pause and stop, Steer and Direct message beside them the bar cut the
// trail's own name and wrapped onto two rows. Mutation: draw the turn's
// controls in the bar again.
test("the bar holds Pause, and Pause and stop, Steer and Direct message are in its More", async () => {
  mount("#/company/org/seats/swe", {
    shell: true,
    answers: { viewer: { ...OPERATOR, acts: EVERY_ACT } },
    agents: [WORKING],
  });
  await settle();
  const bar = document.getElementById(PAGE_ACTIONS_SLOT) as HTMLElement;
  expect(within(bar).getByRole("button", { name: "Pause SWE" })).toBeDefined();
  for (const name of [/^Pause and stop/, /^Steer/, /^Direct message/]) {
    expect(within(bar).queryByRole("button", { name }), String(name)).toBeNull();
  }
  fireEvent.click(within(bar).getByRole("button", { name: "More on SWE" }));
  const entries = screen
    .getAllByRole("menuitem")
    .map((el) => el.querySelector(".crewlet-menu__label")?.textContent ?? "");
  expect(entries).toEqual(
    expect.arrayContaining(["Direct message", "Pause and stop", "Steer its turn"]),
  );
});

// ON A PHONE THE BAR KEEPS ONE ACTION IN VIEW: three buttons and the working
// chip were wider than the line, and the "More" holding Events sat off
// screen. Everything folded is still one press away, opening what the inline
// control opens.
test("the phone's More holds every action but Message, and each opens what its button does", async () => {
  mount("#/company/org/seats/swe", {
    shell: true,
    answers: { viewer: { ...OPERATOR, acts: EVERY_ACT } },
    agents: [WORKING],
  });
  await settle();
  // FOLDED: hidden on a phone's bar by the frame's own rule.
  expect(
    screen.getByRole("button", { name: "Assign task" }).closest(".page-action-folds"),
  ).not.toBeNull();
  for (const name of ["Pause SWE", "More on SWE"]) {
    expect(
      screen.getByRole("button", { name }).closest(".page-action-folds"),
      `${name} is folded`,
    ).not.toBeNull();
  }
  expect(screen.getByRole("button", { name: "Message" }).closest(".page-action-folds")).toBeNull();
  const names = pageMore();
  // WATCH LIVE FIRST while it works, as it leads the bar. DIRECT MESSAGE is
  // folded with the rest: the company runs Chatlet until the engine says
  // otherwise, and the reader holds Chatlet through admin.
  // PAUSE AND STOP, folded with the rest, is one press away as well.
  expect(names.slice(0, 8)).toEqual([
    "Watch live",
    "Direct message",
    "Assign task",
    "Pause",
    "Pause and stop",
    "Steer its turn",
    "Events",
    "Edit in org",
  ]);
  fireEvent.click(screen.getByRole("menuitem", { name: "Pause" }));
  expect(await screen.findByRole("dialog", { name: "Pause SWE" })).toBeTruthy();
});

// WHILE IT WORKS THE BAR WATCHES ITS TURN — the turn's Transcript, the phase it
// is on open — folded on a phone into the same More, which goes to the same
// place. A seat that is not working has nothing to watch and draws no such
// control.
test("a working seat's page bar watches its turn, and an idle one's offers nothing to watch", async () => {
  mount("#/company/org/seats/swe", { shell: true, agents: [WORKING] });
  await settle();
  const bar = document.getElementById(PAGE_ACTIONS_SLOT) as HTMLElement;
  const watch = within(bar).getByRole("link", { name: "Watch live" });
  expect(watch.getAttribute("href")).toBe("#/live/turns/t-7?tab=transcript");
  expect(watch.closest(".page-action-folds")).not.toBeNull();
  pageMore();
  fireEvent.click(screen.getByRole("menuitem", { name: "Watch live" }));
  expect(location.hash).toBe("#/live/turns/t-7?tab=transcript");

  cleanup();
  mount("#/company/org/seats/swe", {
    shell: true,
    agents: [{ ...WORKING, activity: "idle", turn: null, live_call: null }],
  });
  await settle();
  const idle = document.getElementById(PAGE_ACTIONS_SLOT) as HTMLElement;
  expect(within(idle).queryByRole("link", { name: "Watch live" })).toBeNull();
  expect(pageMore()).not.toContain("Watch live");
});

// THE ACTIVITY TAB, WHICH HOLDS THE TURNS, SAYS A TURN IS RUNNING BEHIND IT:
// the pulsing dot, and the words, since a dot is never the only carrier,
// while the engine says the seat is working, and only then.
test("the Activity tab carries a running mark, in words too, only while the seat works", async () => {
  mount("#/company/org/seats/swe", { agents: [WORKING] });
  await settle();
  const turns = screen.getByRole("tab", { name: /^Activity/ });
  expect(turns.textContent).toBe("Activity, running now");
  expect(turns.querySelector(".crewlet-status-dot")).not.toBeNull();
  // NO OTHER TAB IS MARKED.
  const marked = screen.getAllByRole("tab").filter((t) => t.querySelector(".crewlet-status-dot"));
  expect(marked).toEqual([turns]);

  cleanup();
  mount("#/company/org/seats/swe", {
    agents: [{ ...WORKING, activity: "needs", live_call: null }],
  });
  await settle();
  const quiet = screen.getByRole("tab", { name: /^Activity/ });
  expect(quiet.textContent).toBe("Activity");
  expect(quiet.querySelector(".crewlet-status-dot")).toBeNull();
});

// A PAUSED SEAT'S ONE ACTION IS RESUME, so that is the one kept in view.
test("on a paused seat the phone's bar keeps Resume and folds Message", async () => {
  mount("#/company/org/seats/swe", {
    shell: true,
    answers: { viewer: { ...OPERATOR, acts: EVERY_ACT } },
    agents: [PAUSED_SWE],
  });
  await settle();
  expect(
    screen.getByRole("button", { name: "Resume SWE" }).closest(".page-action-folds"),
  ).toBeNull();
  expect(
    screen.getByRole("button", { name: "Message" }).closest(".page-action-folds"),
  ).not.toBeNull();
  const names = pageMore();
  expect(names).toContain("Message");
  expect(names).not.toContain("Pause");
});

// HELD THERE AS IT IS INLINE: disabled, with the sentence.
test("a folded action is held in the menu with the reason", async () => {
  mount("#/company/org/seats/swe", {
    shell: true,
    answers: { viewer: { login: "", grants: [], handle: "", owner: "", acts: [] } },
    agents: [WORKING],
  });
  await settle();
  pageMore();
  const assign = screen.getByRole("menuitem", { name: /^Assign task/ });
  expect(assign.getAttribute("aria-disabled")).toBe("true");
  expect(assign.textContent).toContain(WRITE_REASONS.anonymous);
});

// MESSAGE'S OWN-SEAT HOLD IS THE BUTTON'S RULE, and the folded entry asks the
// same rule rather than the bare act: on the reader's own seat both are held
// in one sentence, and on anybody else's both are free. Mutation: fold the
// entry on `useWriteAccess` alone and the first half goes red.
test("Message folded on a phone is held on your own seat as the button is", async () => {
  mount("#/company/org/seats/swe", {
    shell: true,
    answers: { viewer: { ...OPERATOR, handle: "swe", owner: "swe", acts: EVERY_ACT } },
    agents: [PAUSED_SWE],
  });
  await settle();
  const inline = screen.getByRole("button", { name: "Message" });
  expect(inline.getAttribute("aria-disabled")).toBe("true");
  expect(inline.getAttribute("title")).toBe(OWN_SEAT_MESSAGE);
  pageMore();
  const folded = screen.getByRole("menuitem", { name: /^Message/ });
  expect(folded.getAttribute("aria-disabled")).toBe("true");
  expect(folded.textContent).toContain(OWN_SEAT_MESSAGE);
  cleanup();

  mount("#/company/org/seats/swe", {
    shell: true,
    answers: { viewer: { ...OPERATOR, acts: EVERY_ACT } },
    agents: [PAUSED_SWE],
  });
  await settle();
  pageMore();
  expect(screen.getByRole("menuitem", { name: /^Message/ }).getAttribute("aria-disabled")).not.toBe(
    "true",
  );
});

// ---------------------------------------------------------------------------
// The Overview's figures
// ---------------------------------------------------------------------------

const week = (over: Record<string, unknown> = {}) => ({
  since: "2026-09-15",
  until: "2026-09-21",
  days: 7,
  previous_since: "2026-09-08",
  previous_until: "2026-09-14",
  quantile_resolution: 0.06,
  seats: [
    {
      handle: "swe",
      in_chart: true,
      turns: 64,
      failed: 2,
      reviewed: 30,
      // THE TWO DENOMINATORS DISAGREE ON PURPOSE: 13 first passes over 64
      // turns is 20%, and the engine's rate over the thirty it REVIEWED is
      // what the tile has to show.
      first_pass: 13,
      first_pass_pct: 81,
      sent_back: 12,
      p50_ms: 200_000,
      p90_ms: 700_000,
      tokens: 2_100_000,
      per_day: [{ day: "2026-09-21", turns: 6, failed: 0, tokens: 1 }],
      previous: {
        turns: 55,
        failed: 0,
        reviewed: 20,
        first_pass: 10,
        sent_back: 3,
        tokens: 1,
        per_day: [{ day: "2026-09-14", turns: 5, failed: 0, tokens: 1 }],
      },
      ...over,
    },
  ],
});

test("the turns tile is the week's total from every node, against the week before", async () => {
  mount("#/company/org/seats/swe", { answers: { seat_activity: week() } });
  await waitFor(() => expect(tile("Turns · 7 days").textContent).toContain("64"));
  expect(tile("Turns · 7 days").textContent).toContain("+9");
  expect(tile("Turns · 7 days").textContent).toContain("vs last week");
  // ONE ANSWER, both weeks: the chart and the delta are the same days.
  const params = askedFor("seat_activity")[0]?.params ?? {};
  expect(params).toMatchObject({ seat: "swe", days: 7, previous: true });
});

test("the first-pass rate is the engine's, over the turns it reviewed", async () => {
  mount("#/company/org/seats/swe", { answers: { seat_activity: week() } });
  await waitFor(() => expect(tile("Approved first pass").textContent).toContain("81%"));
  expect(tile("Approved first pass").textContent).not.toContain("20%");
  // WHAT THE RATE IS OF, then the reviews that sent work back.
  expect(tile("Approved first pass").textContent).toContain("13 of 30 · 12 sent back");
  expect(tile("Median turn").textContent).toContain("3m 20s");
  expect(tile("Median turn").textContent).toContain("p90 11m 40s");
});

test("a week nobody reviewed has no rate, rather than a rate of nought", async () => {
  mount("#/company/org/seats/swe", {
    answers: { seat_activity: week({ reviewed: 0, first_pass_pct: undefined, sent_back: 0 }) },
  });
  await waitFor(() => expect(tile("Approved first pass").textContent).toContain("none reviewed"));
  expect(tile("Approved first pass").textContent).not.toContain("0%");
});

// THE FIGURE AND ITS CEILING ARE ONE WINDOW'S: a week's spend over a day's
// limit, or the seven-day sum under a week's cap, would be a ratio of two
// different quantities.
test("the tokens tile is the capped window nearest its ceiling, with that ceiling", async () => {
  mount("#/company/org/seats/swe", {
    answers: { seat_activity: week() },
    agents: [
      {
        id: "swe",
        agent_id: "swe",
        role: "SWE",
        handle: "swe",
        activity: "idle",
        budget: {
          windows: [
            {
              period: "day",
              window: "2026-09-21",
              starts_at: "2026-09-21T00:00:00Z",
              resets_at: "2026-09-22T00:00:00Z",
              used: 10_000,
              limit: 1_000_000,
              state: "ok",
            },
            {
              period: "week",
              window: "2026-W38",
              starts_at: "2026-09-15T00:00:00Z",
              resets_at: "2026-09-22T00:00:00Z",
              used: 2_400_000,
              limit: 3_000_000,
              state: "near",
            },
          ],
        },
      },
    ],
  });
  await settle();
  const t = tile("Tokens · this week");
  expect(t.textContent).toContain("2.4M");
  expect(t.textContent).toContain("of 3M budget");
});

test("an uncapped seat's tokens tile is the week the others count", async () => {
  mount("#/company/org/seats/swe", { answers: { seat_activity: week() } });
  await waitFor(() => expect(tile("Tokens · 7 days").textContent).toContain("2.1M"));
  expect(tile("Tokens · 7 days").textContent).toContain("no budget caps it");
});

// THE COMPANY'S CEILINGS BIND EVERY SEAT: "no budget caps it" over a company
// with a day and a month set was a claim the gate would contradict.
test("a seat with no budget of its own under a capped company says the company's applies", async () => {
  const org = structuredClone(ORG);
  org.token_budget = { day: 60_000_000, month: 900_000_000 };
  mount("#/company/org/seats/swe", { org, answers: { seat_activity: week() } });
  await waitFor(() => expect(tile("Tokens · 7 days").textContent).toContain("2.1M"));
  // NAMED, as the Settings tab names the same ceilings.
  // ONE LINE IN THE TILE — the whole sentence three lines tall stretched the
  // KPI row — and the whole sentence on its title.
  const sub = within(tile("Tokens · 7 days")).getByText(/company cap/);
  expect(sub.textContent).toBe("company cap 60M/day +1");
  expect(sub.getAttribute("title")).toBe("No seat cap; the company's 60M/day and 900M/month apply");
  expect(tile("Tokens · 7 days").textContent).not.toContain("no budget caps it");
});

// THE PAGE IS ONE ROW; THE TOTALS ARE THE HOLDER'S COUNT.
test("the memory card counts what the seat holds, never the page it was sent", async () => {
  mount("#/company/org/seats/swe", {
    answers: {
      agent_memory: {
        id: "swe",
        diary: [
          {
            id: "d1",
            content: "Kernel matrix changes need the CI image bumped first.",
            retention: "diary_long",
            source: "tool:reflect_and_persist",
            turn_id: "t-6",
            created_at: "2026-09-21T07:00:00Z",
            ttl_until: "",
            retrievals: 0,
          },
        ],
        diary_total: 142,
        episodes: [],
        episodes_total: 38,
        skills: [],
        skills_total: 4,
        counterparties: [],
        counterparties_total: 0,
        latest_reflection: {
          id: "d1",
          content: "Kernel matrix changes need the CI image bumped first.",
          retention: "diary_long",
          source: "tool:reflect_and_persist",
          turn_id: "t-6",
          created_at: "2026-09-21T07:00:00Z",
          ttl_until: "",
          retrievals: 0,
        },
        onboarded_at: "2026-09-01T08:00:00Z",
        held_by: "node-2",
      },
    },
  });
  const totals = await waitFor(() => {
    const el = document.querySelector(".prof-memory-totals");
    expect(el).not.toBeNull();
    return el as HTMLElement;
  });
  expect([...totals.querySelectorAll("dd")].map((d) => d.textContent)).toEqual(["142", "38", "4"]);
  expect(screen.getByText(/Kernel matrix changes need the CI image bumped first/)).toBeTruthy();
  // ONE ROW IS ALL THE CARD DRAWS, so one row is all it asks for.
  expect(askedFor("agent_memory")[0]?.params).toMatchObject({ id: "swe", limit: 1 });
});

// A TURN ON THE TASK THAT DID NOT CLAIM ITS KEY. Two tasks hold `ENG-412`, and
// the turn is charged to the second: asked for and opened by the key, its card
// titled and linked the claimant. It is asked for by its id, and opened by the
// address the answer gives — its id, since its key is the claimant's.
test("a turn on a task whose key another claimed names and opens that task, not the claimant", async () => {
  const onDuplicate = structuredClone(WORKING);
  const ref = { backend: "native", id: "t-dup", key: "ENG-412", project: "ENG" };
  onDuplicate.turn!.work_item = ref;
  onDuplicate.live_call!.work_item = ref;
  mount("#/company/org/seats/swe", {
    agents: [onDuplicate],
    answers: {
      work_item: (p: Record<string, unknown>) =>
        p.id === "t-dup"
          ? { task: { id: "t-dup", key: "ENG-412", title: "The duplicate" }, key_collision: true }
          : { task: { id: "t-claimant", key: "ENG-412", title: "The claimant" } },
      work_item_turns: (p: Record<string, unknown>) => ({
        item: p.id === "t-dup" ? "t-dup" : "t-claimant",
        key: "ENG-412",
        turns: [],
        complete: true,
      }),
      turns: { turns: [], next: null },
    },
  });
  await settle();
  const card = document.querySelector(".prof-turn") as HTMLElement;
  await waitFor(() => expect(card.textContent).toContain("The duplicate"));
  expect(card.textContent).not.toContain("The claimant");
  expect(card.querySelector("a.work-key")?.getAttribute("href")).toBe("#/worklet/t-dup");
  expect(askedFor("work_item_turns").map((q) => q.params)).toEqual([{ id: "t-dup", limit: 1 }]);
});

test("the current turn names its round against the granted cap, its calls and the one running", async () => {
  mount("#/company/org/seats/swe", {
    agents: [WORKING],
    answers: {
      // THE TASK IS NOT IN THE ASSIGNED-WORK TOP FIVE — a turn on a task just
      // asked of the seat, with no priority. Its title comes from reading the
      // turn's own item, never from that list.
      work_items: {
        items: [{ id: "t-7", key: "ENG-7", title: "Something else entirely", status: "todo" }],
        total_hint: 1,
      },
      // A WORKLET TASK IS ASKED FOR BY ITS ID: its key may be one another task
      // claimed first, which would answer about the claimant.
      work_item: (p: Record<string, unknown>) =>
        p.id === "i-412"
          ? { task: { id: "i-412", key: "ENG-412", title: "Retry PXE boot on DHCP timeout" } }
          : { task: { id: "x", key: String(p.id), title: "the wrong task" } },
      work_item_turns: { item: "i-412", key: "ENG-412", turns: [], complete: true },
      turns: { turns: [], next: null },
    },
  });
  await settle();
  const card = document.querySelector(".prof-turn") as HTMLElement;
  expect(card.textContent).toContain("Turn 1 on");
  expect(card.textContent).toContain("ENG-412");
  await waitFor(() => expect(card.textContent).toContain("Retry PXE boot on DHCP timeout"));
  expect(askedFor("work_item").map((q) => q.params)).toContainEqual({ id: "i-412" });
  expect(askedFor("work_item_turns").map((q) => q.params)).toContainEqual({
    id: "i-412",
    limit: 1,
  });
  // And opened by the address the answer gives: the key, which it claimed.
  expect(card.querySelector("a.work-key")?.getAttribute("href")).toBe("#/worklet/ENG-412");
  // The head cuts this line beside the title, so its whole words are on hover.
  expect(card.querySelector(".prof-turn-about")?.getAttribute("title")).toBe(
    "Turn 1 on ENG-412 · Retry PXE boot on DHCP timeout",
  );
  // THE ENGINE'S ROUND, one-based, against what the phase was GRANTED.
  expect(card.textContent).toContain("Execute · round 7 of 25");
  // THE KIT'S GRID, headed, in live order: the calls as they ran, the running
  // one last, each row's state on its edge and in its words.
  const feed = within(card).getByRole("table", { name: "Calls this phase, round 7 of 25" });
  expect(
    within(feed)
      .getAllByRole("columnheader")
      .map((h) => h.textContent),
  ).toEqual(["At", "Call", "Asked", "Took"]);
  expect(feed.querySelector("button")).toBeNull();
  const rows = [...feed.querySelectorAll<HTMLElement>(".crewlet-data-grid__row")];
  expect(rows.map((r) => r.querySelectorAll('[role="cell"]')[1]?.textContent)).toEqual([
    "knowledge.search",
    "sandbox.run",
    "sandbox.run",
  ]);
  expect(rows[0]?.textContent).toContain("PXE retry backoff");
  expect(rows[0]?.getAttribute("data-tone")).toBeNull();
  expect(rows[1]?.getAttribute("data-tone")).toBe("danger");
  expect(rows[1]?.querySelectorAll('[role="cell"]')[3]?.textContent).toMatch(/^failed/);
  expect(rows[2]?.getAttribute("data-tone")).toBe("info");
  expect(rows[2]?.textContent).toContain("go test ./provisioner/... -run Timeout");
  expect(rows[2]?.textContent).toContain("running");
  // The recorded phases are none yet, so the turn's tokens are the running phase's.
  expect(card.textContent).toContain("2,500 tokens");
  // A WATCH LINK: the turn's Transcript, where the phase it is on is open.
  expect(within(card).getByRole("link", { name: "Watch live" }).getAttribute("href")).toBe(
    "#/live/turns/t-7?tab=transcript",
  );
});

test("the setup card reads the chart's model and tools, and the document's own rows", async () => {
  mount("#/company/org/seats/swe", { answers: { viewer: OPERATOR, config: DOCUMENT } });
  await waitFor(() => expect(screen.getByText(/1 setup step · up to 40 turns/)).toBeTruthy());
  const setup = screen.getByText("Setup").closest(".crewlet-card") as HTMLElement;
  expect(setup.textContent).toContain("anthropic-main");
  expect(setup.textContent).toContain("openai-fallback");
  expect(setup.textContent).toContain("gitlab");
  expect(setup.textContent).toContain("Nodes labelled pool=build");
  expect(setup.textContent).toContain("test-writer, reviewer-lite");
  expect(within(setup).getByRole("link", { name: "Edit" }).getAttribute("href")).toBe(
    "#/company/org?seat=swe&editor=open",
  );
});

test("without the document the setup card says whose read it is, not that nothing is set", async () => {
  mount("#/company/org/seats/swe", {
    answers: {
      viewer: { login: "jane.doe", grants: ["worklet"], handle: "jane", owner: "jane", acts: [] },
    },
  });
  await settle();
  const setup = screen.getByText("Setup").closest(".crewlet-card") as HTMLElement;
  // THE PUBLIC HALF STILL DRAWS.
  expect(setup.textContent).toContain("anthropic-main");
  expect(setup.textContent).toContain("Some setup is only visible to admins.");
  expect(setup.textContent).not.toContain("The catalogue's defaults");
});

// ---------------------------------------------------------------------------
// A person's day
// ---------------------------------------------------------------------------

// A COLLEAGUE'S DAY IS THEIRS, and the reason a reader is not shown it depends
// on what they hold: with Worklet it is whose the record is (their lead and an
// admin read it too); without, it is the app their whole day is kept in.
test("a colleague's day is withheld with the reason, and never asked for", async () => {
  mount("#/company/org/seats/jane", {
    answers: {
      viewer: { login: "t-cto", grants: ["worklet"], handle: "cto", owner: "cto", acts: [] },
    },
  });
  await settle();
  expect(screen.getByText("Their day")).toBeTruthy();
  expect(
    screen.getByText("Their day is theirs: their lead and an admin read it too."),
  ).toBeTruthy();
  expect(askedFor("work_person")).toEqual([]);
  cleanup();
  asked = [];
  mount("#/company/org/seats/jane", {
    answers: { viewer: { login: "t-cto", grants: [], handle: "cto", owner: "cto", acts: [] } },
  });
  await settle();
  expect(
    screen.getByText("Their day needs Worklet access. An admin can give it to you."),
  ).toBeTruthy();
  expect(askedFor("work_person")).toEqual([]);
});

/** Jane's queue as the engine resolves it: two of her three priorities are
 *  still open — the third, PROD-2, is done and still on the stored list. */
const JANE_QUEUE = {
  handle: "jane",
  priorities: [
    { id: "t1", key: "ENG-1", title: "One", status: "todo" },
    { id: "t2", key: "ENG-2", title: "Two", status: "in_progress" },
  ],
  assigned: [],
  asked_of_me: [],
  checklist_items: [],
  collaborating: [],
  watching_recent: [],
  unblocked_recent: [],
  totals: {
    priorities: { total: 2 },
    assigned: { total: 0 },
    asked_of_me: { total: 0 },
    checklist_items: { total: 0 },
    collaborating: { total: 0 },
    watching_recent: { total: 0 },
    unblocked_recent: { total: 0 },
  },
  complete: true,
};

// THE ENGINE'S COUNT OF WHAT IS STILL OPEN, never the stored list's length: a
// person's list is their own object and a read never rewrites it, so it still
// names a task they finished. Counted off the record (the mutation), Maya's
// tile said 3 beside a Work tab listing 2.
test("a person's Priorities tile counts what is still open, as the Work tab does", async () => {
  mount("#/company/org/seats/jane", {
    answers: {
      viewer: { login: "jane-token", grants: ["worklet"], handle: "jane", owner: "jane", acts: [] },
      work_person: {
        handle: "jane",
        priorities: ["t1", "t2", "t-done"],
        priorities_set_by: "cto",
        unread: [],
        pinned_views: [],
        version: 1,
      },
      work_my_work: JANE_QUEUE,
    },
  });
  await waitFor(() =>
    expect(tile("Priorities").querySelector(".crewlet-statcard__value")?.textContent).toBe("2"),
  );
  // WHO SET IT, by name.
  expect(tile("Priorities").textContent).toContain("set by CTO");
  expect(screen.queryByText("Queue")).toBeNull();

  // THE WORK TAB'S BLOCK SAYS THE SAME NUMBER, from the same answer — and,
  // this being Jane's own seat, says so to her.
  fireEvent.click(screen.getByRole("tab", { name: /Work/ }));
  const block = (await screen.findByText("What you mean to do first")).closest(
    ".crewlet-card__header",
  ) as HTMLElement;
  expect(block.querySelector(".crewlet-count")?.textContent).toBe("2");
});

/** A page of `n` unread notices, and whether an older page exists. */
const inboxPage = (n: number, more: boolean) => ({
  handle: "jane",
  notices: Array.from({ length: n }, (_, i) => ({ record_id: `r${i}` })),
  primary_reasons: ["mention"],
  unread: n,
  primary: n,
  ...(more ? { next_cursor: "c1" } : {}),
});

// THE INBOX'S OWN COUNT, NEVER THE RECORD'S `unread`. That list holds the
// EXCEPTIONS below the read mark — notices marked unread again — so a person
// with fifty waiting read "Unread 0" beside a sidebar badge saying 50+.
test("your own day's Unread is the sidebar badge's count, and the day is yours", async () => {
  mount("#/company/org/seats/jane", {
    shell: true,
    answers: {
      viewer: {
        login: "jane-token",
        grants: ["worklet"],
        handle: "jane",
        owner: "jane",
        name: "Jane Founder",
        acts: [],
      },
      inbox: inboxPage(50, true),
      work_person: { handle: "jane", unread: [], version: 1, held: true, complete: true },
    },
  });
  await waitFor(() => expect(tile("Unread").textContent).toContain("50+"));
  // THE SIDEBAR'S INBOX ROW, which carries the same figure as its badge.
  const nav = document.querySelector(".crewlet-sidebar-nav") as HTMLElement;
  const row = within(nav).getByRole("link", { name: /Inbox/ });
  expect(row.textContent).toContain("50+");
  // ONE READING: the frame's, never a second read of the same inbox.
  expect(askedFor("inbox")).toHaveLength(1);
  expect(screen.getByText("Your day")).toBeTruthy();
  const day = screen.getByText("Your day").closest(".prof-day-head") as HTMLElement;
  expect(within(day).getByRole("link", { name: "Inbox" }).getAttribute("href")).toBe("#/inbox");
  expect(within(day).getByRole("link", { name: "My work" }).getAttribute("href")).toBe("#/me");
  expect(screen.queryByText(/Read-only here/)).toBeNull();
});

// YOUR OWN SEAT POINTS AT YOUR OWN ACCOUNT, where the password and the second
// factor are: a person opening their own page looked for them here and found a
// profile with neither. A colleague's seat says nothing of the kind.
test("your own seat points to your Account, and a colleague's does not", async () => {
  mount("#/company/org/seats/jane", {
    answers: {
      viewer: { login: "jane.doe", handle: "jane", owner: "jane", name: "Jane", acts: [] },
    },
  });
  await settle();
  const card = screen.getByRole("heading", { name: "Your account" }).closest(".prof-card");
  expect(card).not.toBeNull();
  expect(within(card as HTMLElement).getByText(/two-step verification/)).toBeTruthy();
  expect(
    within(card as HTMLElement)
      .getByRole("link", { name: "Account" })
      .getAttribute("href"),
  ).toBe("#/account");
  cleanup();
  mount("#/company/org/seats/jane", {
    answers: { viewer: { login: "t-cto", grants: [], handle: "cto", owner: "cto", acts: [] } },
  });
  await settle();
  expect(screen.getByText("Their day")).toBeTruthy();
  expect(screen.queryByRole("heading", { name: "Your account" })).toBeNull();
});

test("an operator reading a colleague's day asks that colleague's inbox", async () => {
  mount("#/company/org/seats/jane", {
    answers: {
      viewer: {
        login: "ops",
        grants: ["admin", "worklet", "doclet"],
        handle: "cto",
        owner: "cto",
        acts: [],
      },
      inbox: (p: Record<string, unknown>) =>
        p.handle === "jane" ? inboxPage(7, false) : inboxPage(0, false),
      work_person: { handle: "jane", unread: [], version: 1, held: true, complete: true },
    },
  });
  await waitFor(() => expect(tile("Unread").textContent).toContain("7"));
  expect(askedFor("inbox").map((a) => a.params.handle)).toContain("jane");
  expect(screen.getByText("Their day")).toBeTruthy();
  expect(tile("Unread").textContent).toContain("waiting on them");
});

/** The chart with SWE's charter replaced. */
function withCharter(responsibilities: string[], goal = GOAL): OrgProjection {
  const org = structuredClone(ORG);
  const swe = org.units![0]!.children![0]!.roles![0]!;
  swe.responsibilities = responsibilities;
  swe.goal = goal;
  return org;
}

// THE ARTBOARD'S ABOUT, and the rest on request: drawn whole, a dozen
// four-line responsibilities stood the card 1,700px tall and pushed Setup and
// Memory off the side of the page.
test("About lists three responsibilities and offers the rest", async () => {
  const five = ["Owns ``nimbuscore`` end to end", "Two", "Three", "Four", "Five"];
  mount("#/company/org/seats/swe", { org: withCharter(five) });
  await settle();
  const about = screen
    .getByRole("heading", { name: "About" })
    .closest(".crewlet-card") as HTMLElement;
  expect(within(about).getAllByRole("listitem")).toHaveLength(3);
  const more = within(about).getByRole("button", { name: "Show all 5 responsibilities" });
  expect(more.getAttribute("aria-expanded")).toBe("false");
  expect(document.getElementById(more.getAttribute("aria-controls") ?? "")).toBeTruthy();
  // PROMPT TEXT IS MARKDOWN: ``nimbuscore`` is a code span, not two stray pairs.
  expect(within(about).getByText("nimbuscore").tagName).toBe("CODE");
  expect(about.textContent).not.toContain("``");
  fireEvent.click(more);
  expect(within(about).getAllByRole("listitem")).toHaveLength(5);
  const less = within(about).getByRole("button", { name: "Show less" });
  expect(less.getAttribute("aria-expanded")).toBe("true");
  // WHOLE, nothing is clamped.
  expect(about.querySelector(".clamp")).toBeNull();
});

test("a charter that fits offers nothing more", async () => {
  mount("#/company/org/seats/swe", { org: withCharter(["One", "Two"]) });
  await settle();
  const about = screen
    .getByRole("heading", { name: "About" })
    .closest(".crewlet-card") as HTMLElement;
  expect(within(about).getAllByRole("listitem")).toHaveLength(2);
  expect(within(about).queryByRole("button")).toBeNull();
});

// ---------------------------------------------------------------------------
// Direct reports
// ---------------------------------------------------------------------------

test("a lead's reports are counted with where they came from, each goal clamped prose", async () => {
  mount("#/company/org/seats/cto");
  await settle();
  const card = screen.getByText("Direct reports").closest(".crewlet-card") as HTMLElement;
  expect(card.textContent).toContain("all from its manages list");
  // Never the relation the other way round.
  expect(card.textContent).not.toContain("reports to");
  const goal = within(card).getByTitle(GOAL);
  expect(goal.textContent).toBe(GOAL);
  expect(goal.className).toContain("clamp");
  expect(goal.className, "`.truncate` is one line, ended at a pixel").not.toContain("truncate");
  expect(goal.getAttribute("title")).toBe(GOAL);
});

test("a seat nobody reports to draws no reports card at all", async () => {
  mount("#/company/org/seats/swe");
  await settle();
  expect(screen.queryByText("Direct reports")).toBeNull();
});

// ---------------------------------------------------------------------------
// The other tabs
// ---------------------------------------------------------------------------

test("the work tab asks for every row filtered on its own", async () => {
  mount("#/company/org/seats/swe?tab=work", {
    answers: { work_items: { items: [], total_hint: 0 } },
  });
  await settle();
  const params = askedFor("work_items").at(-1)?.params ?? {};
  expect(params.assignee).toBe("swe");
  expect(
    params.subtasks,
    "without it the filter is a predicate on the root and a subtree rides along unfiltered",
  ).toBe("separate");
  expect(screen.getByText("Nothing open is assigned to them")).toBeTruthy();
});

// THE TURN LIST IS THIS SEAT'S. It asked with `role`, which the engine does not
// read, so the tab listed every seat's turns under one seat's name.
test("the turns tab asks for this seat's turns by its handle", async () => {
  mount("#/company/org/seats/swe?tab=turns", {
    answers: {
      turns: { turns: [], next: null },
      tokens: {
        since: "2026-09-15T00:00:00Z",
        until: "2026-09-22T00:00:00Z",
        totals: { input_tokens: 0, output_tokens: 0, total_tokens: 4_321, calls: 3 },
        by_phase: [
          { phase: "execute", input_tokens: 0, output_tokens: 0, total_tokens: 4_321, calls: 3 },
        ],
        by_model: [],
        by_worker: [],
        by_agent: [],
      },
    },
  });
  await settle();
  const turns = askedFor("turns")[0]?.params ?? {};
  expect(turns.seat).toBe("swe");
  expect(turns).not.toHaveProperty("role");
  expect(askedFor("tokens")[0]?.params).toMatchObject({ seat: "swe", days: 7 });
  expect(screen.getByText("Where its tokens go")).toBeTruthy();
  expect(screen.getAllByText("4,321").length).toBeGreaterThan(0);
  // LIVE NOW READS `seat=`, so the seat's model activity is its running turn
  // and its recent phases there — not the turn list, which it pointed at
  // while Live read no seat.
  expect(screen.getByRole("link", { name: "All its model activity" }).getAttribute("href")).toBe(
    "#/live?seat=swe",
  );
});

/** One settled turn row, as `turns` answers it. */
const turnRow = (i: number, over: Record<string, unknown> = {}) => ({
  turn_id: `turn-${i}`,
  started_at: "2026-09-21T09:00:00Z",
  ended_at: "2026-09-21T09:01:00Z",
  duration_ms: 60_000,
  complete: true,
  parked: false,
  failed: false,
  iterations: 1,
  total_tokens: 1_000,
  summary: `did thing ${i}`,
  ...over,
});

// A PAGED LIST IS THE ENGINE'S ORDER. The record is walked newest first, a
// page at a time, so a head that sorted the rows loaded would order a few
// pages and read as an order over the whole record: no head is a button, and
// an address naming a sort leaves the rows as the engine sent them.
test("the turns list keeps the engine's order, with no head that sorts the pages loaded", async () => {
  mount("#/company/org/seats/swe?tab=turns&sort.prof-turns=tokens", {
    answers: {
      turns: {
        turns: [
          turnRow(1, { total_tokens: 9_000 }),
          turnRow(2, { total_tokens: 10 }),
          turnRow(3, { total_tokens: 500 }),
        ],
        next: "older",
      },
    },
  });
  await screen.findByText("did thing 1");
  const card = turnsCard();
  const table = within(card).getByRole("table");
  const heads = within(table).getAllByRole("columnheader");
  expect(heads.length).toBeGreaterThan(0);
  for (const head of heads) {
    expect(within(head).queryByRole("button")).toBeNull();
    expect(head.getAttribute("aria-sort")).toBeNull();
  }
  const rows = [...table.querySelectorAll<HTMLElement>(".crewlet-data-grid__row")];
  expect(rows.map((r) => (r.textContent?.match(/did thing \d/) ?? [""])[0])).toEqual([
    "did thing 1",
    "did thing 2",
    "did thing 3",
  ]);
  // Each row still opens its turn, and the older pages are still a press away.
  expect(rows[0]?.querySelector("a.crewlet-data-grid__row-link")?.getAttribute("href")).toContain(
    "turn-1",
  );
  expect(within(card).getByRole("button", { name: "Load older turns" })).toBeTruthy();
});

// A TURN STILL RUNNING HAS NOTHING SETTLED: no summary, no iteration count and
// no final tokens. The list said "no summary recorded · 0 · 0 · running" for a
// turn the Overview's card named, in the same second, as round 7 on ENG-412 —
// so the row asks the push what the seat is doing and what it is on, and the
// figures it does not have are marked as not settled rather than zero.
test("a running turn's row says what it is doing, and has no figures yet", async () => {
  mount("#/company/org/seats/swe?tab=turns", {
    agents: [WORKING],
    answers: {
      turns: {
        turns: [
          turnRow(7, {
            turn_id: "t-7",
            complete: false,
            ended_at: "",
            duration_ms: 0,
            iterations: 0,
            total_tokens: 0,
            summary: "",
          }),
          turnRow(1),
        ],
        next: null,
      },
    },
  });
  const doing = await screen.findByText("running · executing · round 7 of 25");
  const row = doing.closest(".crewlet-data-grid__row") as HTMLElement;
  expect(within(row).getByText("ENG-412")).toBeTruthy();
  expect(within(row).queryByText("no summary recorded")).toBeNull();
  expect(within(row).getAllByText("Not settled")).toHaveLength(2);
  // A SETTLED TURN'S STATE CELL HOLDS NOTHING AT ALL, which is what a phone's
  // card drops: an empty wrapper left a "STATE" label alone on its line.
  const settled = screen.getByText("did thing 1").closest(".crewlet-data-grid__row") as HTMLElement;
  const cells = [...settled.querySelectorAll(".crewlet-data-grid__cell")];
  const state = cells.find((c) => c.getAttribute("data-label") === "State")!;
  expect(state.childNodes).toHaveLength(0);
});

// A LIVE TURN IS ON SCREEN WHEN THE TAB OPENS: "Running now" leads the tab,
// where under the fifty-row table it was 2,000px down. The settled cards
// after the table are titled, or they read as the table repeating itself, and
// the table's last column is named like every other.
test("the turns tab leads with the running turn and titles the transcripts", async () => {
  mount("#/company/org/seats/swe?tab=turns", {
    agents: [WORKING],
    answers: { turns: { turns: [turnRow(1)], next: null } },
  });
  const live = await screen.findByRole("heading", { name: "Running now" });
  const table = within(turnsCard()).getByRole("heading", { name: "Turns" });
  const transcripts = screen.getByRole("heading", { name: "Transcripts · newest first" });
  const order = (a: Node, b: Node) =>
    Boolean(a.compareDocumentPosition(b) & Node.DOCUMENT_POSITION_FOLLOWING);
  expect(order(live, table), "Running now comes before the table").toBe(true);
  expect(order(table, transcripts), "the transcripts follow it, titled").toBe(true);
  await screen.findByText("did thing 1");
  const heads = [...document.querySelectorAll(".crewlet-data-grid__th")].map((h) =>
    h.textContent?.trim(),
  );
  expect(heads).toContain("State");
  expect(heads).not.toContain("");
});

// A PHASE STREAMED AFTER THE TAB OPENED IS THIS SEAT'S BY ITS ID, never by its
// role name: two unit seats stamped from one template share "SWE", and matched
// on that, a sibling's finished turn landed among this seat's transcripts.
test("the turns tab takes a streamed phase by the seat's id, not a sibling's by its role", async () => {
  const { store } = mount("#/company/org/seats/swe?tab=turns", {
    agents: [{ ...WORKING, activity: "idle", turn: null, live_call: null }],
  });
  await settle();
  const streamed = (id: string, turn: string, agentId: string) => ({
    id,
    type: "agent_phase_completed",
    source: "engine",
    actor: "SWE",
    summary: "",
    category: "lifecycle",
    trace_id: "",
    span_id: "",
    parent_span_id: "",
    topic: "",
    failed: false,
    timestamp: "2026-09-21T09:50:00Z",
    payload: { turn_id: turn, phase: "execute", iteration: 1, role: "SWE", agent_id: agentId },
  });
  act(() => {
    store.applyEvent(streamed("ev-mine", "t-mine", "a-swe") as never);
    store.applyEvent(streamed("ev-sibling", "t-sibling", "a-sibling") as never);
  });
  const transcripts = screen
    .getByRole("heading", { name: "Transcripts · newest first" })
    .closest("section") as HTMLElement;
  expect(transcripts.querySelectorAll(".turn-card")).toHaveLength(1);
});

/** A phase of the running turn `t-7` completing, as the socket streams it. */
const completed = (phase: string, at: string) => ({
  id: `ev-${phase}`,
  type: "agent_phase_completed",
  source: "engine",
  actor: "SWE",
  summary: "",
  category: "lifecycle",
  trace_id: "",
  span_id: "",
  parent_span_id: "",
  topic: "",
  failed: false,
  timestamp: at,
  payload: {
    turn_id: "t-7",
    phase,
    iteration: 1,
    role: "SWE",
    agent_id: "a-swe",
    duration_ms: 30_000,
  },
});

/** The turn card in a section of the tab, and each of its phases: name, open. */
function turnCardIn(heading: string) {
  const section = screen.getByRole("heading", { name: heading }).closest("section")!;
  const card = section.querySelector<HTMLElement>(".turn-card");
  const phases = card
    ? [...card.querySelectorAll(".phase-card")].map((c) => ({
        phase: c.querySelector(".phase-head")?.textContent ?? "",
        open: c.querySelector(".phase-body") !== null,
      }))
    : [];
  return { card, phases };
}

// A TURN BETWEEN PHASES IS STILL RUNNING. Its phase completes, the push clears
// the call, and only then does the next phase start — and split on "has a live
// phase", the card left Running now for the settled list in that gap and came
// back, remounted, with the transcript the reader was following shut. The
// seat's own turn record decides, as on the turn's page.
test("a running turn's card stays put, its phases open, across a phase boundary", async () => {
  const { store } = mount("#/company/org/seats/swe?tab=turns", { agents: [WORKING] });
  await screen.findByRole("heading", { name: "Running now" });
  const before = turnCardIn("Running now");
  expect(
    before.phases.map((p) => p.open),
    "the execute it is on is open",
  ).toEqual([true]);
  act(() => {
    store.applyEvent(completed("execute", "2026-09-21T09:52:00Z") as never);
    store.applyAgents([{ ...WORKING, live_call: null }] as never);
  });
  const between = turnCardIn("Running now");
  expect(between.card, "the same card, in Running now, not remounted").toBe(before.card);
  expect(
    between.phases.map((p) => p.open),
    "and its execute still open",
  ).toEqual([true]);
  act(() =>
    store.applyAgents([
      {
        ...WORKING,
        live_call: {
          ...WORKING.live_call!,
          phase: "review",
          tool_executions: [],
          running_calls: null,
          started_at: "2026-09-21T09:52:05Z",
        },
      },
    ] as never),
  );
  const after = turnCardIn("Running now");
  expect(after.card).toBe(before.card);
  expect(
    after.phases.map((p) => p.open),
    "execute stays open, review opens",
  ).toEqual([true, true]);
});

// AND WHEN THE TURN ENDS, the card moves to the settled transcripts — the one
// remount left — with the phases the reader watched still open.
test("a turn that ends keeps the phases its reader watched open in the transcripts", async () => {
  const { store } = mount("#/company/org/seats/swe?tab=turns", { agents: [WORKING] });
  await screen.findByRole("heading", { name: "Running now" });
  act(() => {
    store.applyEvent(completed("execute", "2026-09-21T09:52:00Z") as never);
    store.applyAgents([
      {
        ...WORKING,
        live_call: { ...WORKING.live_call!, phase: "review", tool_executions: [] },
      },
    ] as never);
  });
  act(() => {
    store.applyEvent(completed("review", "2026-09-21T09:53:00Z") as never);
    store.applyAgents([{ ...WORKING, activity: "idle", turn: null, live_call: null }] as never);
  });
  expect(screen.queryByRole("heading", { name: "Running now" })).toBeNull();
  const ended = turnCardIn("Transcripts · newest first");
  expect(ended.phases.map((p) => p.open)).toEqual([true, true]);
});

// THE HEADER COUNTS WHAT IS LOADED, never the history: `.length` of the first
// page read "50" under "the newest 50 this seat took" whether the seat had
// fifty-one turns or four hundred, and nothing reached the fifty-first.
test("the turns list says it holds a page, and loads the one before it", async () => {
  mount("#/company/org/seats/swe?tab=turns", {
    answers: {
      turns: (p: Record<string, unknown>) =>
        p.before
          ? { turns: [turnRow(50), turnRow(51)], next: null }
          : {
              turns: Array.from({ length: 50 }, (_, i) => turnRow(i)),
              // OPAQUE: whatever the engine hands out is handed back as is.
              next: "MjAyNi0wOS0yMFQwMDowMDowMFogdC00OQ",
            },
    },
  });
  await settle();
  const first = askedFor("turns")[0]?.params ?? {};
  // EVERY DAY THE STORE KEEPS, named: the read's default is a week.
  expect(first).toMatchObject({ seat: "swe", days: 30, limit: 50 });
  const card = turnsCard();
  expect(card.querySelector(".crewlet-card__header")?.textContent).toContain("50+");
  fireEvent.click(within(card).getByRole("button", { name: "Load older turns" }));
  // THE ROW ITSELF, by its whole summary. A substring of the card's text was
  // already true before the older page arrived — "did thing 5" and the "1"
  // of its round count run together in `textContent` — so under load the
  // wait passed at once and the header was read before the page landed.
  await within(card).findByText("did thing 51");
  expect(askedFor("turns").find((a) => a.params.before)?.params).toMatchObject({
    seat: "swe",
    days: 30,
    before: "MjAyNi0wOS0yMFQwMDowMDowMFogdC00OQ",
  });
  expect(card.querySelector(".crewlet-card__header")?.textContent).toContain("52");
  expect(card.querySelector(".crewlet-card__header")?.textContent).not.toContain("52+");
  expect(within(card).queryByRole("button", { name: "Load older turns" })).toBeNull();
});

// AN OLDER PAGE OF TURNS THAT FAILS LEAVES THE CONTROL THAT ASKS IT AGAIN,
// its failure said beside it, never in its place. Mutation: draw the failure
// in the control's place again, and there is no Load older turns to press.
test("an older page of turns that fails keeps Load older turns, which asks it again", async () => {
  let failures = 1;
  mount("#/company/org/seats/swe?tab=turns", {
    answers: {
      turns: (p: Record<string, unknown>) =>
        p.before
          ? failures-- > 0
            ? Promise.reject(new Error("unavailable"))
            : { turns: [turnRow(50)], next: null }
          : { turns: Array.from({ length: 50 }, (_, i) => turnRow(i)), next: "t-49" },
    },
  });
  await settle();
  const card = turnsCard();
  fireEvent.click(within(card).getByRole("button", { name: "Load older turns" }));
  expect(
    await within(card).findByText("Could not read the next page (unavailable). Try again."),
  ).toBeTruthy();
  fireEvent.click(within(card).getByRole("button", { name: "Load older turns" }));
  await within(card).findByText("did thing 50");
  expect(within(card).queryByText(/Could not read the next page/)).toBeNull();
});

// A KEYLESS ITEM IS NAMED SHORT, whole on its title: `<backend>:<uuid>` took
// the whole cell on a phone and laid the summary out one letter per line.
test("a turn's keyless work item is named by its kind and the head of its id", async () => {
  const id = "b375beaf-1d2e-4c6a-9f0b-2a1c3d4e5f60";
  mount("#/company/org/seats/swe?tab=turns", {
    answers: {
      turns: {
        turns: [
          turnRow(1, { work_item: { backend: "native", id, key: "", project: "ENG" } }),
          turnRow(2, { work_item: { backend: "native", id, key: "ENG-29", project: "ENG" } }),
        ],
        next: null,
      },
    },
  });
  await settle();
  const keyless = screen.getByText("task b375beaf");
  expect(keyless.getAttribute("title")).toContain(`Worklet task ${id}`);
  expect(keyless.className).toContain("turn-what-key");
  expect(screen.getByText("ENG-29").className).toContain("turn-what-key");
  expect(document.body.textContent).not.toContain(`Worklet task ${id}`);
});

const memoryOf = (over: Record<string, unknown> = {}) => ({
  id: "swe",
  diary: Array.from({ length: 50 }, (_, i) => ({
    id: `d${i}`,
    content: `note ${i}`,
    retention: "diary_long",
    source: "tool:reflect_and_persist",
    turn_id: "",
    created_at: "2026-09-21T07:00:00Z",
    ttl_until: "",
    retrievals: 0,
  })),
  diary_total: 142,
  episodes: [],
  episodes_total: 0,
  skills: [],
  skills_total: 0,
  counterparties: [],
  counterparties_total: 0,
  latest_reflection: null,
  onboarded_at: "",
  held_by: "node-2",
  ...over,
});

test("the memory tab's lists say they are the latest page of the whole", async () => {
  mount("#/company/org/seats/swe?tab=memory", {
    answers: {
      agent_memory: memoryOf(),
    },
  });
  await waitFor(() => expect(screen.getByText(/Latest 50 of 142/)).toBeTruthy());
  expect(screen.getByText(/Runs on node-2\./)).toBeTruthy();
});

// ONE RULE FOR ALL FOUR HEADERS: the skills card said its cut in a footer,
// "(50 of 63 shown)", where every other card says it in its header.
test("the learned skills say their cut in the header, as the other three lists do", async () => {
  const skill = (i: number) => ({
    id: `s${i}`,
    key: `k${i}`,
    title: `Skill ${i}`,
    summary: "",
    version: 1,
    uses: 0,
    updated_at: "",
  });
  mount("#/company/org/seats/swe?tab=memory", {
    answers: {
      agent_memory: memoryOf({
        skills: Array.from({ length: 50 }, (_, i) => skill(i)),
        skills_total: 63,
      }),
    },
  });
  await waitFor(() => expect(screen.getByText("Skills it taught itself")).toBeTruthy());
  const card = screen.getByText("Skills it taught itself").closest(".crewlet-card") as HTMLElement;
  expect(card.querySelector(".crewlet-card__header")?.textContent).toContain("Latest 50 of 63");
  expect(card.textContent).not.toContain("shown)");
});

// THE SAME RECORDS AS DOCLET'S LEARNED GRID, in its columns: a hand-built list
// of them read differently on the two screens. The holder ordered the page, so
// no head sorts it.
test("a seat's learned skills are drawn in the Learned grid's columns, in the holder's order", async () => {
  mount("#/company/org/seats/swe?tab=memory", {
    answers: {
      agent_memory: memoryOf({
        skills: [
          {
            id: "s1",
            key: "triage",
            title: "Triage a failed deploy",
            summary: "Read the log first.",
            version: 3,
            uses: 12,
            updated_at: "2026-09-20T07:00:00Z",
          },
          {
            id: "s2",
            key: "unnamed-skill",
            title: "",
            summary: "",
            version: 1,
            uses: 40,
            updated_at: "2026-09-19T07:00:00Z",
          },
        ],
        skills_total: 2,
      }),
    },
  });
  const title = await screen.findByText("Triage a failed deploy");
  const card = screen.getByText("Skills it taught itself").closest(".crewlet-card") as HTMLElement;
  const table = within(card).getByRole("table");
  const heads = within(table).getAllByRole("columnheader");
  expect(heads.map((h) => h.textContent)).toEqual(["Skill", "Uses", "Version", "Updated"]);
  for (const head of heads) expect(within(head).queryByRole("button")).toBeNull();
  const rows = [...table.querySelectorAll<HTMLElement>(".crewlet-data-grid__row")];
  expect(rows[0]?.contains(title)).toBe(true);
  expect(rows[0]?.textContent).toContain("Read the log first.");
  expect(rows[0]?.querySelector('[data-label="Uses"]')?.textContent).toBe("12");
  // A SKILL WITH NO TITLE SAYS ITS KEY, as Doclet's does.
  expect(rows[1]?.textContent).toContain("unnamed-skill");
});

test("a seat with no learned skills says so in the grid's empty state", async () => {
  mount("#/company/org/seats/swe?tab=memory", { answers: { agent_memory: memoryOf() } });
  await screen.findByText("No learned skills");
  expect(screen.getByText("Skills learned from repeated work appear here.")).toBeTruthy();
});

// THE REVIEWER'S DECISION IN THE ONE TABLE THAT TONES IT: `failed` is red on
// the turn it came from and was amber here.
test("an episode's outcome takes the review decision's own tone", async () => {
  const episode = (id: string, review_outcome: string) => ({
    id,
    turn_id: id,
    task_summary: `episode ${id}`,
    review_outcome,
    created_at: "2026-09-21T07:00:00Z",
  });
  mount("#/company/org/seats/swe?tab=memory", {
    answers: {
      agent_memory: memoryOf({
        episodes: [episode("e1", "failed"), episode("e2", "done"), episode("e3", "self_iterate")],
        episodes_total: 3,
      }),
    },
  });
  await waitFor(() => expect(screen.getByText("episode e1")).toBeTruthy());
  const tag = (word: string) =>
    screen.getByText(word, { selector: ".crewlet-tag, .crewlet-tag *" });
  expect(tag("failed").closest(".crewlet-tag")?.className).toContain("danger");
  expect(tag("done").closest(".crewlet-tag")?.className).toContain("success");
  expect(tag("self_iterate").closest(".crewlet-tag")?.className).toContain("warning");
});

// WHAT WOKE A TURN IS NOT WHAT IT DID. The label of the waking event sat under
// "What it did", so every chat turn read as having done "Message from Ana";
// and a compacted row, which has no label, read "The episode recorded no
// summary" in place of the pattern it was folded into.
test("an episode says what woke it apart from what it did, and a compacted row its pattern", async () => {
  mount("#/company/org/seats/swe?tab=memory", {
    answers: {
      agent_memory: memoryOf({
        episodes: [
          {
            id: "e1",
            turn_id: "e1",
            task_summary: "Message from Ana: Slack message",
            ask: "The staging deploy keeps failing.",
            plan_summary: "Rolled staging back to v41.",
            review_outcome: "done",
            created_at: "2026-09-21T07:00:00Z",
          },
          {
            id: "c1",
            turn_id: "",
            compacted: true,
            count: 12,
            compaction: {
              common_task_pattern: "Triaging a failed staging deploy",
              done: 9,
              notable_patterns: "Two went to the SRE lead.",
            },
            review_outcome: "done",
            created_at: "2026-08-01T07:00:00Z",
          },
        ],
        episodes_total: 2,
      }),
    },
  });
  await waitFor(() => expect(screen.getByText("Message from Ana: Slack message")).toBeTruthy());
  const headers = screen.getAllByRole("columnheader").map((h) => h.textContent?.trim());
  expect(headers).toContain("Woken by");
  expect(headers).toContain("What it did");
  const row = (text: string) =>
    screen.getByText(text).closest(".crewlet-data-grid__row") as HTMLElement;
  const cellUnder = (tr: HTMLElement, header: string) =>
    tr.querySelector(`[data-label="${header}"]`) as HTMLElement;
  // The turn: its label and its ask under Woken by, its account under What it did.
  const turn = row("Message from Ana: Slack message");
  expect(cellUnder(turn, "Woken by").textContent).toContain(
    "Asked: The staging deploy keeps failing.",
  );
  expect(cellUnder(turn, "What it did").textContent).toBe("Rolled staging back to v41.");
  // The compacted row: how many turns, its pattern and what varied, and its tally.
  const folded = row("Triaging a failed staging deploy");
  expect(cellUnder(folded, "Woken by").textContent).toBe("12 turns like this");
  expect(cellUnder(folded, "What it did").textContent).toContain(
    "What varied: Two went to the SRE lead.",
  );
  expect(cellUnder(folded, "Outcome").textContent).toContain("9 of 12 done");
  expect(screen.queryByText("The episode recorded no summary")).toBeNull();
});

// AN EPISODE IS A TURN, AND OPENS IT: each turn's row links to the turn it
// summarises, and the list orders by what woke each turn. A compacted row
// folds several turns, is none of them, and has no link. What it did is
// prose, never an order.
test("an episode opens its turn, and the list sorts by what woke it", async () => {
  const turn = (id: string, task_summary: string, created_at: string) => ({
    id,
    turn_id: `turn-${id}`,
    task_summary,
    plan_summary: `did ${id}`,
    review_outcome: "done",
    created_at,
  });
  mount("#/company/org/seats/swe?tab=memory", {
    answers: {
      agent_memory: memoryOf({
        episodes: [
          turn("e1", "Schedule: weekly-review", "2026-09-21T09:00:00Z"),
          turn("e2", "Message from a colleague", "2026-09-21T08:00:00Z"),
          {
            id: "c1",
            turn_id: "",
            compacted: true,
            count: 3,
            compaction: { common_task_pattern: "Triage", done: 3, notable_patterns: "" },
            review_outcome: "done",
            created_at: "2026-09-20T07:00:00Z",
          },
        ],
        episodes_total: 3,
      }),
    },
  });
  await waitFor(() => expect(screen.getByText("Schedule: weekly-review")).toBeTruthy());
  const card = screen.getByText("Episodes").closest(".crewlet-card") as HTMLElement;
  const rows = () => [...card.querySelectorAll<HTMLElement>(".crewlet-data-grid__row")];
  const link = (r: HTMLElement) =>
    r.querySelector("a.crewlet-data-grid__row-link")?.getAttribute("href") ?? null;
  expect(rows().map(link)).toEqual(["#/live/turns/turn-e1", "#/live/turns/turn-e2", null]);
  const head = (name: string) =>
    within(card)
      .getAllByRole("columnheader")
      .find((h) => h.textContent?.trim() === name)!;
  expect(within(head("What it did")).queryByRole("button")).toBeNull();
  fireEvent.click(within(head("Woken by")).getByRole("button"));
  await waitFor(() =>
    expect(rows().map((r) => r.querySelector('[data-label="Woken by"]')?.textContent)).toEqual([
      "Message from a colleague",
      "Schedule: weekly-review",
      "3 turns like this",
    ]),
  );
});

// A LONG ASK IS LISTED AS ITS OPENING, MARKED, AND READ WHOLE ON DEMAND. The
// listing carries an episode's ask and its account as their openings — fifty
// whole asks could be more than the transport takes, and the holder refused the
// whole memory read — with each whole text's size beside it; the row says it is
// an opening and opens the turn whole, from the seat's holder.
test("a long ask is listed as its marked opening and read whole on demand", async () => {
  const whole = `${"the deploy log says ".repeat(200)}and then it stopped.`;
  const opening = whole.slice(0, 600);
  mount("#/company/org/seats/swe?tab=memory", {
    answers: {
      agent_memory: memoryOf({
        episodes: [
          {
            id: "e1",
            turn_id: "e1",
            task_summary: "Message from Ana: Slack message",
            ask: opening,
            ask_bytes: whole.length,
            plan_summary: "Read the log and filed the regression.",
            plan_summary_bytes: 38,
            review_outcome: "done",
            created_at: "2026-09-21T07:00:00Z",
          },
        ],
        episodes_total: 1,
      }),
      agent_episode: (params: Record<string, unknown>) => ({
        handle: params.id,
        held_by: "node-2",
        episode: {
          id: params.episode,
          turn_id: "e1",
          task_summary: "Message from Ana: Slack message",
          ask: whole,
          ask_bytes: whole.length,
          plan_summary: "Read the log and filed the regression.",
          plan_summary_bytes: 38,
          review_outcome: "done",
          created_at: "2026-09-21T07:00:00Z",
        },
      }),
    },
  });
  await waitFor(() => expect(screen.getByText("Message from Ana: Slack message")).toBeTruthy());
  const row = screen
    .getByText("Message from Ana: Slack message")
    .closest(".crewlet-data-grid__row") as HTMLElement;
  const woken = row.querySelector('[data-label="Woken by"]') as HTMLElement;
  expect(woken.textContent).toContain(`Asked: ${opening}…`);
  // A WHOLE ACCOUNT IS NOT MARKED and offers nothing to open.
  const did = row.querySelector('[data-label="What it did"]') as HTMLElement;
  expect(did.textContent).toBe("Read the log and filed the regression.");
  expect(askedFor("agent_episode")).toHaveLength(0);

  fireEvent.click(screen.getByRole("button", { name: /^Read all/ }));
  await waitFor(() => expect(screen.getByText(/and then it stopped\./)).toBeTruthy());
  expect(askedFor("agent_episode")[0]?.params).toEqual({ id: "swe", episode: "e1" });
});

// THE CONVERSATION COLUMN IS CAPPED AND A UUID IS CUT TO ITS HEAD: printed
// whole, a Worklet task's `work:task:<uuid>` took the grid's width and left
// "What it did" a third of it.
test("an episode's conversation prints a uuid's head, and the whole key on its title", async () => {
  const key = "work:task:4d631f6d-5107-46f0-8043-a1f9d7a74abc";
  mount("#/company/org/seats/swe?tab=memory", {
    answers: {
      agent_memory: memoryOf({
        episodes: [
          {
            id: "e1",
            turn_id: "e1",
            task_summary: "episode e1",
            conversation_key: key,
            created_at: "2026-09-21T07:00:00Z",
          },
        ],
        episodes_total: 1,
      }),
    },
  });
  const cell = await screen.findByText("work:task:4d631f6d");
  expect(cell.getAttribute("title")).toBe(key);
  expect(screen.queryByText(key)).toBeNull();
});

test("a seat no node holds says why its memory is empty", async () => {
  mount("#/company/org/seats/swe?tab=memory", {
    answers: {
      agent_memory: memoryOf({ diary: [], diary_total: 0, held_by: "none" }),
    },
  });
  await waitFor(() => {
    expect(
      screen.getByText(/No node is running this seat, so its memory can’t be shown\./),
    ).toBeTruthy();
  });
});

// ---------------------------------------------------------------------------
// The Memory tab's executor sessions
// ---------------------------------------------------------------------------

// A SEAT WHOSE TURNS CAPTURED NOTHING SAYS SO, and when a session appears —
// rather than drawing an empty list a reader cannot tell from one that failed.
test("a seat with no executor sessions says so under its memory", async () => {
  mount("#/company/org/seats/swe?tab=memory", { answers: { agent_memory: memoryOf() } });
  expect(await screen.findByText("No sessions recorded")).toBeTruthy();
  expect(
    screen.getByText(
      "A turn appears here from its first launch where it read its conversation, and otherwise once its session is first kept or it ends.",
    ),
  ).toBeTruthy();
  expect(askedFor("executor_sessions")[0]?.params).toMatchObject({ handle: "swe" });
});

// CHOOSING A SESSION OPENS ITS TRANSCRIPT, asked of the engine by its turn as
// every question about a turn names it, under the account of how the turn
// ended — the brief, the model's words, a note a person sent the run, each
// labelled by who it is from.
test("choosing a session opens its transcript, asked by its turn", async () => {
  mount("#/company/org/seats/swe?tab=memory", {
    answers: {
      agent_memory: memoryOf(),
      executor_sessions: sessionsList([sessionRow()]),
      executor_session: transcriptPage({
        entries: [
          said("Fix the flaky provisioner test.", "brief"),
          said("## A note from ada, sent while this turn was running\n\nTarget main.", "note"),
          said("Running the tests first."),
        ],
      }),
    },
  });
  const row = await screen.findByRole("button", { name: /Fix the flaky provisioner test\./ });
  expect(askedFor("executor_session")).toHaveLength(0);
  fireEvent.click(row);
  expect(await screen.findByText("Running the tests first.")).toBeTruthy();
  expect(row.getAttribute("aria-pressed")).toBe("true");
  expect(askedFor("executor_session").at(-1)?.params).toMatchObject({
    handle: "swe",
    turn_id: SESSION_TURN,
  });
  expect(screen.getByText("Note")).toBeTruthy();
  expect(screen.getByText(/40 KiB kept/)).toBeTruthy();
  // WHICH LAUNCH WOULD CONTINUE IT leads what the account says of it: an
  // ended turn's newest session is its conversation's next turn's.
  expect(
    screen.getByText(
      "The conversation's next turn would continue this session in whichever box it gets, " +
        "where that box can take it up and the seat's budget can carry it.",
    ),
  ).toBeTruthy();
});

// A SESSION WHOSE OBJECT IS GONE IS A WARNING SAYING SO, in the engine's own
// sentence — never an empty transcript, which would read as a run that did
// nothing.
test("a session whose object is gone is said, not drawn as an empty transcript", async () => {
  const why = "the object store no longer holds this session's object";
  mount(`#/company/org/seats/swe?tab=memory&session=${SESSION_TURN}`, {
    answers: {
      agent_memory: memoryOf(),
      executor_sessions: sessionsList([sessionRow()]),
      executor_session: transcriptPage({ state: "missing", why }),
    },
  });
  expect(await screen.findByText(why)).toBeTruthy();
  expect(screen.getByText("The session is gone.")).toBeTruthy();
  expect(screen.queryByText("No entries")).toBeNull();
});

// THE LIST AND THE TRANSCRIPT EACH PAGE ON, by the cursor each answer hands
// out, and a page after the first is drawn after it.
test("the sessions and a session's transcript each page on", async () => {
  const older = "0193f0c2-6a1e-7c55-9d1a-3b6f0e2a4c12";
  mount(`#/company/org/seats/swe?tab=memory&session=${SESSION_TURN}`, {
    answers: {
      agent_memory: memoryOf(),
      executor_sessions: (p: Record<string, unknown>) =>
        p.cursor === "c1"
          ? sessionsList([sessionRow({ turn_id: older, ask_excerpt: "An older turn's ask." })])
          : sessionsList([sessionRow()], { next_cursor: "c1" }),
      executor_session: (p: Record<string, unknown>) =>
        p.cursor === "k.1.1"
          ? transcriptPage({ from: 1, entries: [said("The second page.")] })
          : transcriptPage({
              entries: [said("The first page.")],
              cut: "entries",
              next_cursor: "k.1.1",
            }),
    },
  });
  expect(await screen.findByText("The first page.")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Load more of the transcript" }));
  expect(await screen.findByText("The second page.")).toBeTruthy();
  expect(screen.getByText("The first page.")).toBeTruthy();

  fireEvent.click(screen.getByRole("button", { name: "Load older sessions" }));
  expect(await screen.findByText("An older turn's ask.")).toBeTruthy();
});

// CHOOSING ANOTHER SESSION SHOWS THAT SESSION, from its loading state: the one
// before it — its account and its entries — is never drawn under the next
// one's heading while the next is being read.
test("choosing another session never shows the last one's transcript under it", async () => {
  const other = "0193f0c2-6a1e-7c55-9d1a-3b6f0e2a4c13";
  let answer: (value: unknown) => void = () => undefined;
  mount(`#/company/org/seats/swe?tab=memory&session=${SESSION_TURN}`, {
    answers: {
      agent_memory: memoryOf(),
      executor_sessions: sessionsList([
        sessionRow(),
        sessionRow({ turn_id: other, ask_excerpt: "The other turn's ask." }),
      ]),
      executor_session: (p: Record<string, unknown>) =>
        p.turn_id === other
          ? new Promise((resolve) => {
              answer = resolve;
            })
          : transcriptPage({ entries: [said("The first session's words.")] }),
    },
  });
  expect(await screen.findByText("The first session's words.")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: /The other turn's ask\./ }));
  await waitFor(() => expect(screen.queryByText("The first session's words.")).toBeNull());
  act(() =>
    answer(
      transcriptPage({
        session: sessionRow({ turn_id: other }),
        entries: [said("The other session's words.")],
      }),
    ),
  );
  expect(await screen.findByText("The other session's words.")).toBeTruthy();
});

// A PAGE THAT NO LONGER FOLLOWS ON IS REFUSED, and the reader is told so — in
// words naming no cause the screen cannot know, the session captured again or
// a node of another release answering — and offered the transcript from its
// start, the one way on, rather than a refusal with no way past it. Read again,
// focus is on the transcript's heading: the button pressed is gone with the
// pages it stood under.
test("a page of an older capture offers the transcript again from its start", async () => {
  mount(`#/company/org/seats/swe?tab=memory&session=${SESSION_TURN}`, {
    answers: {
      agent_memory: memoryOf(),
      executor_sessions: sessionsList([sessionRow()]),
      executor_session: (p: Record<string, unknown>) => {
        if (p.cursor) throw new Error("bad_params");
        return transcriptPage({
          entries: [said("The first page.")],
          cut: "entries",
          next_cursor: "k.1.1",
        });
      },
    },
  });
  expect(await screen.findByText("The first page.")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Load more of the transcript" }));
  expect(await screen.findByText(/later pages no longer follow on/)).toBeTruthy();
  const before = askedFor("executor_session").length;
  fireEvent.click(screen.getByRole("button", { name: "Read it again from the start" }));
  await waitFor(() => expect(askedFor("executor_session").length).toBeGreaterThan(before));
  expect(askedFor("executor_session").at(-1)?.params).not.toHaveProperty("cursor");
  expect(await screen.findByText("The first page.")).toBeTruthy();
  expect(document.activeElement).toBe(screen.getByRole("heading", { name: "Transcript" }));
});

// A NEWER CAPTURE THAN THE TRANSCRIPT ON SCREEN IS OFFERED, never swapped in
// under the reader: the list says the session was captured since, and the
// reader reads the newer capture from its start when they choose to — focus
// then on the transcript's heading, the button pressed being gone.
test("a newer capture of the open session is offered from its start", async () => {
  const later = "2026-09-21T09:55:00Z";
  let reads = 0;
  mount(`#/company/org/seats/swe?tab=memory&session=${SESSION_TURN}`, {
    answers: {
      agent_memory: memoryOf(),
      executor_sessions: sessionsList([sessionRow({ captured_at: later })]),
      executor_session: () =>
        ++reads === 1
          ? transcriptPage({ entries: [said("What the first capture held.")] })
          : transcriptPage({
              session: sessionRow({ captured_at: later }),
              entries: [said("What the newer capture holds.")],
            }),
    },
  });
  expect(await screen.findByText("What the first capture held.")).toBeTruthy();
  fireEvent.click(await screen.findByRole("button", { name: "Read the newer capture" }));
  expect(await screen.findByText("What the newer capture holds.")).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Read the newer capture" })).toBeNull();
  expect(document.activeElement).toBe(screen.getByRole("heading", { name: "Transcript" }));
});

/** The transcript's own card: the account, the entries and what is said
 *  under them, apart from the list beside it. */
function transcriptCard(): HTMLElement {
  const card = screen.getByRole("heading", { name: "Transcript" }).closest(".thread-detail-card");
  expect(card, "the transcript's card is not drawn").not.toBeNull();
  return card as HTMLElement;
}

// A FIRST PAGE ASKED AGAIN FOR A NEWER CAPTURE IS OFFERED, NEVER DRAWN. A write
// from this tab asks every question again, the transcript's first page among
// them, and its answer may be of a capture since: drawn, it replaced the
// entries under the reader with nothing to say so; with a page held, it set
// the pages bridging to a capture they can never meet. The entries read stay,
// and the newer capture is offered from its start.
test("a first page answered again for a newer capture is offered, never drawn", async () => {
  const later = "2026-09-21T09:55:00Z";
  let captured = sessionRow().captured_at;
  mount(`#/company/org/seats/swe?tab=memory&session=${SESSION_TURN}`, {
    answers: {
      agent_memory: memoryOf(),
      executor_sessions: () => sessionsList([sessionRow({ captured_at: captured })]),
      executor_session: () =>
        captured === later
          ? transcriptPage({
              session: sessionRow({ captured_at: later }),
              entries: [said("What the newer capture holds.")],
            })
          : transcriptPage({ entries: [said("What the first capture held.")] }),
    },
  });
  expect(await screen.findByText("What the first capture held.")).toBeTruthy();
  const before = askedFor("executor_session").length;
  captured = later;
  act(() => {
    floors.written(null, null, [EVERY_QUESTION]);
  });
  await waitFor(() => expect(askedFor("executor_session").length).toBeGreaterThan(before));
  expect(await screen.findByRole("button", { name: "Read the newer capture" })).toBeTruthy();
  expect(screen.getByText("What the first capture held.")).toBeTruthy();
  expect(screen.queryByText("What the newer capture holds.")).toBeNull();
});

// HOW THE TURN ENDED FOLLOWS THE LIST'S ROW. A turn's end is recorded with no
// new capture, so it reaches the list's poll and never the transcript's first
// page, read once — which went on saying the turn had not ended beside a list
// saying how it did. And a reconnect asks the list again, never the
// transcript.
test("how a turn ended follows the list's row once its transcript is read", async () => {
  let ending: Record<string, unknown> | null = null;
  const { store } = mount(`#/company/org/seats/swe?tab=memory&session=${SESSION_TURN}`, {
    answers: {
      agent_memory: memoryOf(),
      executor_sessions: () =>
        sessionsList([sessionRow({ ending, ended_at: ending ? "2026-09-21T10:30:00Z" : null })]),
      executor_session: transcriptPage({
        session: sessionRow({ ending: null, ended_at: null }),
        entries: [said("Running the tests first.")],
      }),
    },
  });
  expect(await screen.findByText("Running the tests first.")).toBeTruthy();
  expect(within(transcriptCard()).getByText("not ended")).toBeTruthy();
  const listed = askedFor("executor_sessions").length;
  const read = askedFor("executor_session").length;
  ending = { outcome: "completed", decision: "done" };
  act(() => store.setConnected(false));
  act(() => store.setConnected(true));
  await waitFor(() => expect(askedFor("executor_sessions").length).toBeGreaterThan(listed));
  await waitFor(() => expect(within(transcriptCard()).queryByText("not ended")).toBeNull());
  expect(within(transcriptCard()).getByText("completed")).toBeTruthy();
  expect(askedFor("executor_session")).toHaveLength(read);
});

// A FAILED POLL OF THE LIST LEAVES THE TRANSCRIPT, AND ITS PAGES, WHERE THEY
// ARE. The transcript was drawn inside the list's answer, so a poll that timed
// out or was refused replaced it with the list's failure, and the pages the
// reader had loaded were read again from the first once a poll came back.
test("a failed poll of the list leaves the open transcript and its pages", async () => {
  let failing = false;
  const { store } = mount(`#/company/org/seats/swe?tab=memory&session=${SESSION_TURN}`, {
    answers: {
      agent_memory: memoryOf(),
      executor_sessions: () => {
        if (failing) throw new Error("unavailable");
        return sessionsList([sessionRow()]);
      },
      executor_session: (p: Record<string, unknown>) =>
        p.cursor === "k.1.1"
          ? transcriptPage({ from: 1, entries: [said("The second page.")] })
          : transcriptPage({
              entries: [said("The first page.")],
              cut: "entries",
              next_cursor: "k.1.1",
            }),
    },
  });
  expect(await screen.findByText("The first page.")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Load more of the transcript" }));
  expect(await screen.findByText("The second page.")).toBeTruthy();
  const listed = askedFor("executor_sessions").length;
  failing = true;
  act(() => store.setConnected(false));
  act(() => store.setConnected(true));
  await waitFor(() => expect(askedFor("executor_sessions").length).toBeGreaterThan(listed));
  await settle();
  expect(screen.getByText("The first page.")).toBeTruthy();
  expect(screen.getByText("The second page.")).toBeTruthy();
  expect(screen.queryByText(/Fix the flaky provisioner test\./)).toBeNull();
});

// A LATER PAGE THAT CANNOT BE READ SAYS SO AFTER THE ENTRIES. Every page is
// read from the archive's start, so a first page can read whole and a later
// one meet the damage — an archive cut short past the entries shown, an object
// gone since — and come back with no entries and no cursor: read only from the
// first page's state, the transcript looked as if the run had ended there.
test("a later page that cannot be read says so after the entries", async () => {
  const why =
    "this session opened, and is not an archive of its harness's format: " +
    "it ends part way through";
  mount(`#/company/org/seats/swe?tab=memory&session=${SESSION_TURN}`, {
    answers: {
      agent_memory: memoryOf(),
      executor_sessions: sessionsList([sessionRow()]),
      executor_session: (p: Record<string, unknown>) =>
        p.cursor === "k.1.1"
          ? transcriptPage({ from: 1, state: "malformed", why })
          : transcriptPage({
              entries: [said("The first page.")],
              cut: "entries",
              next_cursor: "k.1.1",
            }),
    },
  });
  expect(await screen.findByText("The first page.")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Load more of the transcript" }));
  expect(await screen.findByText(/The rest of this transcript could not be read/)).toBeTruthy();
  expect(within(transcriptCard()).getByText(new RegExp(why))).toBeTruthy();
  expect(screen.getByText("The first page.")).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Load more of the transcript" })).toBeNull();
});

// A PAGE CUT WHERE NO LATER PAGE CAN GO FURTHER SAYS SO, a first page as much
// as a later one; and one that read nothing says nothing of it could be shown —
// never "No entries", which says the harness recorded nothing.
test("a page cut at the read's own bound says the rest cannot be shown", async () => {
  const sessions = { agent_memory: memoryOf(), executor_sessions: sessionsList([sessionRow()]) };
  const address = `#/company/org/seats/swe?tab=memory&session=${SESSION_TURN}`;
  mount(address, {
    answers: {
      ...sessions,
      executor_session: transcriptPage({ entries: [said("All a read reached.")], cut: "read" }),
    },
  });
  expect(await screen.findByText("All a read reached.")).toBeTruthy();
  expect(screen.getByText(/The rest of this session cannot be shown.*256 MiB/)).toBeTruthy();
  cleanup();

  mount(address, {
    answers: {
      ...sessions,
      executor_session: (p: Record<string, unknown>) =>
        p.cursor === "k.1.1"
          ? transcriptPage({ from: 1, entries: [said("All a read reached.")], cut: "read" })
          : transcriptPage({
              entries: [said("The first page.")],
              cut: "entries",
              next_cursor: "k.1.1",
            }),
    },
  });
  expect(await screen.findByText("The first page.")).toBeTruthy();
  expect(screen.queryByText(/cannot be shown/)).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Load more of the transcript" }));
  expect(await screen.findByText("All a read reached.")).toBeTruthy();
  expect(screen.getByText(/The rest of this session cannot be shown.*256 MiB/)).toBeTruthy();
  cleanup();

  mount(address, {
    answers: { ...sessions, executor_session: transcriptPage({ entries: [], cut: "read" }) },
  });
  expect(await screen.findByText(/None of this session can be shown.*256 MiB/)).toBeTruthy();
  expect(screen.queryByText("No entries")).toBeNull();
});

// A SESSION THE ADDRESS NAMES IS KEPT IN ITS LIST'S VIEW once the rows are
// drawn: on opening, the list has no rows until it is answered, which is after
// the first render, and a list that kept the row in view only then opened at
// its top with the pressed row out of sight.
test("a session the address names is kept in its list's view", async () => {
  mount(`#/company/org/seats/swe?tab=memory&session=${SESSION_TURN}`, {
    answers: {
      agent_memory: memoryOf(),
      executor_sessions: sessionsList([
        sessionRow({
          turn_id: "0193f0c2-6a1e-7c55-9d1a-3b6f0e2a4c19",
          ask_excerpt: "Another ask.",
        }),
        sessionRow(),
      ]),
    },
  });
  await waitFor(() =>
    expect(
      keptInView.some((row) => row.textContent?.includes("Fix the flaky provisioner test.")),
    ).toBe(true),
  );
});

const schedule = (over: Partial<ScheduleRow>): ScheduleRow => ({
  scope_type: "role",
  scope_id: "swe",
  scope_name: "swe",
  name: "daily-standup",
  cron: "0 9 * * 1-5",
  timezone: "UTC",
  task: "Post the standup",
  target: "",
  enabled: true,
  timeout_seconds: 0,
  catchup: false,
  runners: ["swe"],
  next_run: new Date(Date.now() + 3_600_000).toISOString(),
  ...over,
});

test("the schedules tab lists a unit schedule this seat runs, and marks one that cannot fire", async () => {
  mount("#/company/org/seats/swe?tab=schedules", {
    schedules: [
      schedule({
        scope_type: "unit",
        scope_id: "Core",
        scope_name: "Core",
        name: "weekly-review",
        runners: ["swe"],
      }),
      schedule({
        name: "broken",
        timezone: "Mars/Olympus",
        next_run: undefined,
        problem: "unknown time zone Mars/Olympus",
      }),
      // Another seat's: not this one's day.
      schedule({ scope_id: "cto", scope_name: "cto", name: "board-prep", runners: ["cto"] }),
    ],
  });
  await settle();
  expect(screen.getByText("weekly-review")).toBeTruthy();
  // THE REASON IN ITS NEXT CELL, as the company's grid says it.
  expect(screen.getByText("unknown time zone Mars/Olympus")).toBeTruthy();
  expect(screen.queryByText("board-prep")).toBeNull();
});

// ONE COLUMN SET FOR THE SAME ROWS: a seat's recurring work is drawn with the
// company Schedules grid's columns, ordered by the next fire, its expression
// sortable, and its last fire read from the same history.
test("a seat's recurring work has the company grid's columns, ordered by its next fire", async () => {
  const daily = schedule({});
  mount("#/company/org/seats/swe?tab=schedules", {
    schedules: [daily],
    answers: {
      schedules: {
        schedules: [daily],
        recent_runs: [
          {
            scope_type: "role",
            scope_id: "swe",
            scope_name: "swe",
            schedule_name: "daily-standup",
            fire_label: "2026-01-05T09:00:00Z",
            target_handle: "swe",
            scheduled_at: "2026-01-05T09:00:00Z",
            fired_at: new Date(Date.now() - 3_600_000).toISOString(),
            outcome: "fired",
            trace_id: "",
          },
        ],
      },
    },
  });
  await settle();
  const card = screen.getByText("Recurring work").closest(".crewlet-card") as HTMLElement;
  const table = within(card).getByRole("table");
  const heads = within(table).getAllByRole("columnheader");
  expect(heads.map((h) => h.textContent)).toEqual([
    "Name",
    "Scope",
    "Cron",
    "Task",
    "Wakes",
    "Next",
    "Last",
  ]);
  const next = heads.find((h) => h.textContent === "Next");
  expect(next?.getAttribute("aria-sort")).toBe("ascending");
  expect(within(heads.find((h) => h.textContent === "Cron")!).queryByRole("button")).toBeTruthy();
  await waitFor(() => expect(table.textContent).toContain("fired"));
});

// AN ASSIGNED ROW'S TITLE IS CUT AT THE CARD'S EDGE, AND READ WHOLE ON HOVER.
// Mutation: a plain span, and a long title has no way to be read in place.
test("an assigned row's cut title gives its whole title on hover", async () => {
  const title = "Let learners star a phrase from search results and find it again";
  mount("#/company/org/seats/swe", {
    answers: {
      work_items: { items: [{ id: "t-1", key: "ENG-1", title, status: "todo" }], total_hint: 1 },
    },
  });
  await settle();
  const cut = screen.getByText(title);
  Object.defineProperty(cut, "scrollWidth", { configurable: true, value: 300 });
  Object.defineProperty(cut, "clientWidth", { configurable: true, value: 120 });
  fireEvent.mouseEnter(cut);
  expect(cut.getAttribute("title")).toBe(title);
});

// A CUT SCHEDULE NAME IS STILL READ WHOLE: the name is the kit's text cell,
// which puts its words on its title when the pointer finds them cut.
test("a schedule's cut name gives its whole name on hover", async () => {
  mount("#/company/org/seats/swe?tab=schedules", {
    schedules: [schedule({ name: "weekly-backlog-grooming-and-triage" })],
  });
  await settle();
  const name = screen.getByText("weekly-backlog-grooming-and-triage");
  Object.defineProperty(name, "scrollWidth", { configurable: true, value: 300 });
  Object.defineProperty(name, "clientWidth", { configurable: true, value: 120 });
  fireEvent.mouseEnter(name);
  expect(name.getAttribute("title")).toBe("weekly-backlog-grooming-and-triage");
});

// A CREDENTIAL IS NAMED, NEVER SHOWN — not its reference, not the mask, and
// not a literal a redaction bug let through.
test("the settings tab names a seat's credentials and prints none of their values", async () => {
  mount("#/company/org/seats/swe?tab=settings", {
    answers: { viewer: OPERATOR, config: DOCUMENT },
  });
  await waitFor(() => expect(screen.getByText("API_TOKEN")).toBeTruthy());
  expect(screen.getByText("AUTH_HEADER")).toBeTruthy();
  expect(screen.getByText("GITLAB_HOST")).toBeTruthy();
  const page = document.body.textContent ?? "";
  expect(page).not.toContain("sk-live");
  expect(page).not.toContain("__redacted__");
  expect(page).not.toContain("ENGINEERING_GITLAB_HOST");
  expect(page).toContain("from the secret store");
  expect(screen.getByText("swe@example.com")).toBeTruthy();
  expect(screen.getByRole("link", { name: "Edit in org" }).getAttribute("href")).toBe(
    "#/company/org?seat=swe&editor=open",
  );
});

// THE GUARDED DOCUMENT IS READ ONCE FOR THE WHOLE PROFILE. The shell reads it
// for every tab that draws it, and `useQuery` shares no request between two
// callers — so the Settings tab reading it again fetched the whole company
// document twice on every open.
test("the settings tab asks for the company document once", async () => {
  mount("#/company/org/seats/swe?tab=settings", {
    answers: { viewer: OPERATOR, config: DOCUMENT },
  });
  await waitFor(() => expect(screen.getByText("API_TOKEN")).toBeTruthy());
  await settle();
  expect(askedFor("config")).toHaveLength(1);
});

// A READ NOBODY SENT IS NOT A COMPANY WITH NO CONFIGURATION. The document is
// never asked of a reader without Admin who leads no unit over the seat,
// and the tab read "no document, not loading" as an answer — telling a person
// on their own seat "No company configuration is active" while a company ran.
// It names the grant instead, and where the seat sits in a unit, that its lead
// reads it too. The CONTROL is a read that did come back with no document,
// which is the one place that sentence is true. Mutation: drop the withheld
// arm and the first two go red; draw the grant wherever no document is in
// hand and the control does.
test("a reader the document is not asked for is told the grant, never that none is active", async () => {
  const reader = {
    login: "jane.doe",
    grants: ["worklet"],
    handle: "jane",
    owner: "jane",
    acts: [],
  };
  mount("#/company/org/seats/jane?tab=settings", { answers: { viewer: reader } });
  const identity = (await screen.findByText("You need Admin access")).closest(
    ".crewlet-card",
  ) as HTMLElement;
  expect(identity.textContent).toContain("Admin");
  expect(identity.textContent).toContain("jane.doe");
  // A PANEL'S LOCK IS THE LINE ALONE: no button, so a small card stays small.
  expect(within(identity).queryByRole("button")).toBeNull();
  expect(within(identity).queryByRole("link", { name: "Go to Home" })).toBeNull();
  // ABOVE EVERY UNIT, so no lead reads it either.
  expect(identity.textContent).not.toContain("leads");
  expect(document.body.textContent).not.toContain("No company configuration is active");
  expect(askedFor("config")).toEqual([]);
  cleanup();

  mount("#/company/org/seats/swe?tab=settings", { answers: { viewer: { ...reader, handle: "" } } });
  await screen.findAllByText("You need Admin access");
  expect(document.body.textContent).toContain("Leads of Core can see these too.");
  expect(document.body.textContent).not.toContain("No company configuration is active");
  cleanup();

  mount("#/company/org/seats/jane?tab=settings", {
    answers: { viewer: OPERATOR, config: () => null },
  });
  expect(await screen.findByText("No company configuration is active")).toBeTruthy();
  expect(screen.queryByText("You need Admin access")).toBeNull();
});

test("a person's settings carry their contacts and no model, budget or credential card", async () => {
  mount("#/company/org/seats/jane?tab=settings", {
    answers: { viewer: OPERATOR, config: DOCUMENT },
  });
  await waitFor(() => expect(screen.getByText("U0FOUNDER")).toBeTruthy());
  expect(screen.queryByText("Tool credentials")).toBeNull();
  expect(screen.queryByText("Model and budget")).toBeNull();
  // A CONTACT IS NAMED AS A PERSON NAMES IT, never its config key with the
  // underscores swapped for spaces.
  expect(screen.getByText("Slack member id")).toBeTruthy();
  expect(screen.queryByText("slack user id")).toBeNull();
});

// A CONTACT IS OPTIONAL: a person with none is reached through the dashboard,
// and the tab says that rather than that one is required.
test("a person with no contact identity is said to be reached through the dashboard only", async () => {
  const document = structuredClone(DOCUMENT);
  delete document.roles![1]!.contact;
  mount("#/company/org/seats/jane?tab=settings", {
    answers: { viewer: OPERATOR, config: document },
  });
  expect(
    await screen.findByText(
      "Reached only through the dashboard: their Slack, GitHub or tracker activity is not attributed to them. Add a contact identity in Structure.",
    ),
  ).toBeTruthy();
});

test("a contact key is labelled by the table, and one a newer engine added in sentence case", () => {
  expect(contactLabel("github_login")).toBe("GitHub login");
  expect(contactLabel("teams_user_id")).toBe("Teams user id");
});

// A WINDOW THE SEAT LEAVES UNCAPPED IS THE COMPANY'S, where the company caps it.
test("the budget rows say where the company's ceiling applies to an uncapped window", async () => {
  const org = structuredClone(ORG);
  org.token_budget = { day: 60_000_000 };
  mount("#/company/org/seats/swe?tab=settings", {
    org,
    answers: { viewer: OPERATOR, config: DOCUMENT },
  });
  await waitFor(() => expect(screen.getByText("Model and budget")).toBeTruthy());
  const card = screen.getByText("Model and budget").closest(".crewlet-card") as HTMLElement;
  await waitFor(() => expect(card.textContent).toContain("3M tokens"));
  expect(card.textContent).toContain("company limit, 60M");
  // SWE's own week is its own; the month is capped by nobody.
  expect(card.textContent).toContain("3M tokens");
  expect(card.textContent).toContain("uncapped");
});

// ---------------------------------------------------------------------------
// The trail
// ---------------------------------------------------------------------------

test("the trail names the seat and the unit it sits in, and titles the tab with the name", async () => {
  mount("#/company/org/seats/swe", { shell: true });
  await settle();
  const trail = screen.getByRole("navigation", { name: "Breadcrumb" });
  const here = trail.querySelector("[aria-current='page']");
  expect(here?.querySelector(".crewlet-breadcrumb__text")?.textContent).toBe("SWE");
  // ITS BADGE, the outline every other seat badge wears.
  expect(here?.querySelector(".crewlet-breadcrumb__badge")).not.toBeNull();
  // BY THE UNIT'S KEY, never its name, which two units may share: its panel
  // over Structure's Table, the one place a unit is read.
  const unit = within(trail).getByRole("link", { name: "Engineering · Core" });
  expect(unit.getAttribute("href")).toBe("#/company/org/table?peek=unit%3Acore");
  expect(document.title).toBe("SWE · Crewlet");
});

// WITHOUT WORKLET, MESSAGE AND ASSIGN TAKE THEIR WRAPPERS WITH THEM:
// the buttons drew nothing and their empty spans still took the bar's gap.
// Mutation: keep the spans whatever the services, and the bar holds two
// empty children.
test("a company running no Worklet leaves no empty slot in the seat's bar", async () => {
  mount("#/company/org/seats/swe", {
    shell: true,
    agents: [WORKING],
    org: {
      ...ORG,
      services: { tracker: "jira", knowledge: "doclet", chat: "chatlet" },
    } as OrgProjection,
  });
  await settle();
  const bar = document.getElementById(PAGE_ACTIONS_SLOT) as HTMLElement;
  expect(
    [...bar.querySelectorAll(":scope > span")].filter((s) => s.childElementCount === 0),
  ).toEqual([]);
  expect(within(bar).queryByRole("button", { name: /^Message/ })).toBeNull();
});

// A PARKED RUN IS ANSWERED FROM ITS SEAT WITH WHAT IT ASKED WHOLE. The live
// entry the seat's page draws from carries no launch, which an answer names,
// so the answer dialog reads the run's durable row for the question, its
// context and its options, and answers the launch that row names.
//
// Mutation: draw the dialog off the live entry alone, and the context and the
// options are missing and nothing can be sent; send the option as `answer`,
// or drop the launch, and the write is wrong.
test("a parked run is answered from its seat with its context and options", async () => {
  const { store } = mount("#/company/org/seats/swe", {
    answers: {
      viewer: { ...OPERATOR, acts: EVERY_ACT },
      executors: {
        runs: [
          {
            turn_id: "t-7",
            launch_id: "q-7",
            agent_handle: "swe",
            status: "awaiting_clarification",
            question: "Which retry ceiling should the client use?",
            question_context: "The API allows **five** retries.",
            decision: {
              question: "Which retry ceiling should the client use?",
              options: [
                { id: "three", label: "Three" },
                { id: "five", label: "Five" },
              ],
              recommended: "three",
              role: "contributor",
            },
          },
        ],
      },
    },
    agents: [WORKING],
  });
  act(() =>
    store.applyExecutors([
      {
        turn_id: "t-7",
        role: "SWE",
        agent_handle: "swe",
        agent_id: "a-swe",
        harness: "claude",
        sandbox_id: "box-1",
        status: "awaiting_clarification",
        started_at: "2026-09-21T09:45:50Z",
        question: "Which retry ceiling should the client use?",
        question_context: "",
      },
    ]),
  );
  await settle();
  expect(screen.getByText(/This seat's executor is waiting on a question:/).textContent).toContain(
    "Which retry ceiling should the client use?",
  );
  fireEvent.click(screen.getByRole("button", { name: "Answer" }));
  const dialog = await screen.findByRole("dialog");
  await settle();
  expect(dialog.querySelector(".prose strong")?.textContent).toBe("five");
  fireEvent.click(within(dialog).getByRole("button", { name: /Three/ }));
  await waitFor(() => expect(posted).toHaveLength(1));
  expect(posted[0]).toEqual({
    tool: "answer_executor",
    args: { turn_id: "t-7", launch_id: "q-7", choice: "three" },
  });
});

// THE NOTICE READS AS THE READING DOES, so the only way to the rest of a long
// question is not its Answer, which is a write.
test("a seat's notice offers the whole of a long question before its Answer", async () => {
  const { store } = mount("#/company/org/seats/swe", { agents: [WORKING] });
  act(() =>
    store.applyExecutors([
      {
        turn_id: "t-7",
        role: "SWE",
        agent_handle: "swe",
        agent_id: "a-swe",
        harness: "claude",
        sandbox_id: "box-1",
        status: "awaiting_clarification",
        started_at: "2026-09-21T09:45:50Z",
        question: `${"Which retry ceiling should the client use here? ".repeat(8)}Say which.`,
        question_context: "",
      },
    ]),
  );
  await settle();
  const notice = screen.getByText(/This seat's executor is waiting on a question:/);
  expect(notice.textContent).toContain("Say which.");
  expect(within(notice).getByRole("button", { name: "Show all" })).toBeTruthy();
});

/** A read still in flight: it never answers while the case looks. */
const inFlight = () => new Promise<never>(() => {});

// ARRIVING ON A SEAT DOES NOT MOVE THE PAGE AS EACH QUERY ANSWERS: each card
// stands its answer's shape while it is asked. Setup drew three rows and a note,
// then five, and pushed Memory and Schedules down the side.
test("the setup card stands its five rows while the document is asked", async () => {
  mount("#/company/org/seats/swe", { answers: { viewer: OPERATOR, config: inFlight } });
  const setup = (await screen.findByText("Setup")).closest(".crewlet-card") as HTMLElement;
  await waitFor(() => expect(askedFor("config")).toHaveLength(1));
  const terms = [...setup.querySelectorAll("dt")].map((t) => t.textContent);
  expect(terms).toEqual(["Model", "Sandbox", "Runs on", "Workers", "Tools"]);
  expect(setup.textContent).not.toContain("Loading setup…");
});

test("the chart and the memory card stand their shapes while they are asked", async () => {
  mount("#/company/org/seats/swe", {
    answers: { viewer: OPERATOR, seat_activity: inFlight, agent_memory: inFlight },
  });
  const chart = (await screen.findByText("Turns per day")).closest(".crewlet-card") as HTMLElement;
  expect(chart.querySelector(".crewlet-card__subtitle")).toBeTruthy();
  expect(chart.querySelector(".prof-chart-body [class*='skeleton']")).toBeTruthy();
  const memory = screen.getByText("Memory").closest(".crewlet-card") as HTMLElement;
  await waitFor(() => expect(memory.querySelector("[class*='skeleton']")).toBeTruthy());
});

// THE PUSH KNOWS ONLY THE PHASE RUNNING, so a running turn's card grows as the
// phases before it land, and pushed everything under it down the screen. It
// holds a card's room while it does, and the live phase stays in view.
test("a running turn's card holds its room while its history is asked, in view", async () => {
  mount("#/company/org/seats/swe?tab=turns", {
    agents: [WORKING],
    answers: { agent: inFlight, turns: inFlight },
  });
  const live = await screen.findByRole("heading", { name: "Running now" });
  const section = live.closest("section") as HTMLElement;
  expect(section.querySelector(".turn-card")).toBeTruthy();
  expect(section.querySelector(".prof-live-turn-reading")).toBeTruthy();
  const table = within(turnsCard()).getByRole("table");
  expect(table.getAttribute("aria-busy")).toBe("true");
  expect(within(table).getAllByRole("columnheader").length).toBeGreaterThan(0);
});

test("a running turn's card lets go of its room when its history answers", async () => {
  mount("#/company/org/seats/swe?tab=turns", { agents: [WORKING] });
  const live = await screen.findByRole("heading", { name: "Running now" });
  const section = live.closest("section") as HTMLElement;
  await waitFor(() => expect(section.querySelector(".prof-live-turn-reading")).toBeNull());
  expect(section.querySelector(".turn-card")).toBeTruthy();
});

// A BUSY SEAT'S ACTIVITY TAB RAN TO 28,000PX with Sessions at the very bottom:
// the tab's head names its sections, and a press moves to one and focuses it.
test("the Activity tab's head jumps to each of its sections", async () => {
  mount("#/company/org/seats/swe?tab=activity", { answers: { agent_memory: memoryOf() } });
  const nav = await screen.findByRole("navigation", { name: "Sections of this tab" });
  const labels = within(nav)
    .getAllByRole("button")
    .map((b) => b.textContent);
  expect(labels).toEqual(["Tokens", "Turns", "Transcripts", "Memory", "Sessions"]);
  for (const section of ["tokens", "turns", "transcripts", "memory", "sessions"]) {
    expect(document.getElementById(`seat-section-${section}`), section).toBeTruthy();
  }
  const sessions = document.getElementById("seat-section-sessions") as HTMLElement;
  const moved = vi.fn();
  sessions.scrollIntoView = moved;
  fireEvent.click(within(nav).getByRole("button", { name: "Sessions" }));
  expect(moved).toHaveBeenCalledTimes(1);
  expect(document.activeElement).toBe(sessions);
  // SESSIONS IS ITS OWN SECTION, not the tail of Memory's.
  const memory = document.getElementById("seat-section-memory") as HTMLElement;
  expect(memory.contains(sessions)).toBe(false);
});

// A SEAT'S PAGE OF FIFTY IS NOT DRAWN WHOLE: the diary, the skills and the
// colleagues start at the newest five with the way to the rest, which says the
// latest where the holder cut the page, so it never claims the whole record.
test("a seat's long memory lists start capped, with the way to the rest", async () => {
  const skill = (i: number) => ({
    id: `s${i}`,
    key: `k${i}`,
    title: `Skill ${i}`,
    summary: "",
    version: 1,
    uses: 0,
    updated_at: "",
  });
  const colleague = (i: number) => ({
    subject: { handle: `h${i}`, name: `Colleague ${i}` },
    resolved: true,
    traits: {},
    interactions: 1,
    first_seen_at: "2026-09-01T07:00:00Z",
    last_updated_at: "2026-09-01T07:00:00Z",
    last_corroborated_at: "2026-09-01T07:00:00Z",
  });
  mount("#/company/org/seats/swe?tab=memory", {
    answers: {
      agent_memory: memoryOf({
        skills: Array.from({ length: 7 }, (_, i) => skill(i)),
        skills_total: 7,
        counterparties: Array.from({ length: 6 }, (_, i) => colleague(i)),
        counterparties_total: 6,
      }),
    },
  });
  const diary = (await screen.findByText("Diary")).closest(".crewlet-card") as HTMLElement;
  expect(diary.querySelectorAll(".thread-entry")).toHaveLength(5);
  const more = within(diary).getByRole("button", { name: "Show the latest 50" });
  expect(more.getAttribute("aria-expanded")).toBe("false");
  fireEvent.click(more);
  expect(diary.querySelectorAll(".thread-entry")).toHaveLength(50);
  const less = within(diary).getByRole("button", { name: "Show fewer" });
  expect(less.getAttribute("aria-expanded")).toBe("true");
  expect(document.getElementById(less.getAttribute("aria-controls")!)).toBeTruthy();

  const skills = screen
    .getByText("Skills it taught itself")
    .closest(".crewlet-card") as HTMLElement;
  expect(skills.querySelectorAll(".crewlet-data-grid__row")).toHaveLength(5);
  fireEvent.click(within(skills).getByRole("button", { name: "Show all 7" }));
  expect(skills.querySelectorAll(".crewlet-data-grid__row")).toHaveLength(7);

  const people = screen.getByText("Who it has worked with").closest(".crewlet-card") as HTMLElement;
  expect(people.querySelectorAll(".thread-entry")).toHaveLength(5);
  expect(within(people).getByRole("button", { name: "Show all 6" })).toBeTruthy();
});
