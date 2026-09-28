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
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { SeatScreen } from "./Seat.tsx";
import { contactLabel } from "./seat/Settings.tsx";
import { Shell } from "~/app/Shell.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { InboxCountsProvider } from "~/lib/useInboxCounts.ts";
import { LiveSocket, Store, clearToken, storeToken } from "~/protocol/index.ts";
import type {
  AgentRow,
  CompanyDocument,
  DerivedSeat,
  OrgProjection,
  ScheduleRow,
} from "~/protocol/index.ts";

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
      name: "Engineering",
      type: "department",
      children: [
        {
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
        name: "Engineering",
        type: "department",
        lead: "",
        lead_inherited: false,
        channel: "",
        channel_inherited: false,
        seats: [],
      },
      {
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
  units: [
    {
      name: "Engineering",
      children: [
        {
          name: "Core",
          // INHERITED BY THE UNIT'S DIRECT AGENT MEMBERS, SWE among them.
          mcp_env: { gitlab: { GITLAB_HOST: "${ENGINEERING_GITLAB_HOST}" } },
          roles: [
            {
              name: "SWE",
              handle: "swe",
              email: "swe@example.com",
              workers: ["test-writer", "reviewer-lite"],
              sandbox: { enabled: true, run_in: "e2b", coding_agent: "claude-code" },
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

const OPERATOR = { operator_id: "ops", operator: true, handle: "jane", name: "Jane Founder" };
const EVERY_ACT = [
  "create_work_item",
  "update_work_item",
  "pause_seat",
  "resume_seat",
  "answer_run",
];

/** An answer, or how to answer from the question's own parameters. */
type Answer = unknown | ((params: Record<string, unknown>) => unknown);

/**
 * What the engine answers a question no case names — each in the shape the
 * wire carries, so a case about one card is not a case about another card
 * reading a malformed answer.
 */
const DEFAULTS: Record<string, unknown> = {
  viewer: { operator_id: "", operator: false, handle: "", acts: [] },
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
  conversations: {
    handle: "swe",
    conversations: [],
    conversations_total: 0,
    entries: [],
    held_by: "node-1",
  },
  turns: { turns: [], next: null },
  agent: { llm_history: [], next: "" },
  work_inbox: { handle: "", notices: [], primary_reasons: [], unread: 0, primary: 0 },
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
  asked = [];
  posted = [];
  reply = { status: 200, body: { tool: "pause_seat", outcome: "applied" } };
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      const tool = decodeURIComponent(String(url).split("/operator/act/")[1] ?? "");
      const body = JSON.parse(init.body as string) as { args: Record<string, unknown> };
      posted.push({ tool, args: body.args });
      return new Response(JSON.stringify(reply.body), {
        status: reply.status,
        headers: { "Content-Type": "application/json" },
      });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  clearToken();
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
  if (agents.length) store.applyAgents(agents);
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
  const handle = decodeURIComponent(hash.split("/")[3]!.split("?")[0]!);
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

/** Let every settled promise land, and the renders they cause. */
async function settle() {
  for (let i = 0; i < 6; i++) await act(async () => Promise.resolve());
}

const tabNames = () => screen.getAllByRole("tab").map((t) => t.textContent);
const askedFor = (what: string) => asked.filter((a) => a.what === what);

/** A stat tile by its label: the tile's own box. */
function tile(label: string): HTMLElement {
  const el = screen.getByText(label).closest(".crewlet-statcard");
  expect(el, `no tile is labelled ${label}`).not.toBeNull();
  return el as HTMLElement;
}

const WORKING: Partial<AgentRow> = {
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
        started_at: "2026-09-21T09:48:02Z",
        duration_ms: 300,
      },
      {
        name: "sandbox.run",
        round: 5,
        arguments: { cmd: "go test ./provisioner/..." },
        started_at: "2026-09-21T09:49:30Z",
        duration_ms: 85_000,
        failed: true,
      },
    ],
    running_call: {
      round: 7,
      name: "sandbox.run",
      arguments: '{"cmd":"go test ./provisioner/... -run Timeout"}',
      started_at: "2026-09-21T09:51:31Z",
    },
  } as AgentRow["live_call"],
};

// ---------------------------------------------------------------------------
// The tabs a kind has
// ---------------------------------------------------------------------------

// A PERSON RUNS NO TURN: the engine never spawns one, so their profile asks
// nothing a runtime answers — every one of those reads would be a question
// about a thing that cannot exist, and the tabs that draw them are absent.
test("a person's profile has three tabs and asks nothing a runtime answers", async () => {
  // A BOOKMARK NAMING AN AGENT'S TAB lands on Overview rather than a blank.
  mount("#/agents/seats/jane?tab=turns", { answers: { viewer: OPERATOR, config: DOCUMENT } });
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
    "conversations",
    "work_item_turns",
  ]) {
    expect(askedFor(what), `a person's profile asked ${what}`).toEqual([]);
  }
  // Messaged and given work like anybody — and never paused.
  expect(screen.getByRole("button", { name: "Message" })).toBeTruthy();
  expect(screen.getByRole("button", { name: "Assign task" })).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Pause" })).toBeNull();
});

test("an agent's profile has six tabs, and Work counts the engine's total rather than a page", async () => {
  mount("#/agents/seats/swe", {
    answers: {
      work_items: {
        items: [
          { key: "ENG-412", title: "Retry PXE boot", status: "in_progress" },
          { key: "ENG-401", title: "Migrate scheduler state", status: "todo" },
        ],
        total_hint: 7,
      },
    },
  });
  await settle();
  expect(tabNames().map((n) => n?.replace(/\d+$/, ""))).toEqual([
    "Overview",
    "Work",
    "Turns",
    "Memory",
    "Schedules",
    "Settings",
  ]);
  const work = screen.getAllByRole("tab").find((t) => t.textContent?.startsWith("Work"));
  await waitFor(() => expect(work?.textContent).toBe("Work7"));
  // AND THE OVERVIEW'S CARD OFFERS ALL SEVEN, from the same answer.
  expect(screen.getByRole("link", { name: "All 7" }).getAttribute("href")).toBe(
    "#/work?assignee=swe",
  );
});

// ---------------------------------------------------------------------------
// The actions
// ---------------------------------------------------------------------------

test("Events opens the event log narrowed to this seat", async () => {
  mount("#/agents/seats/swe");
  await settle();
  fireEvent.click(screen.getByRole("button", { name: "More on SWE" }));
  fireEvent.click(screen.getByRole("menuitem", { name: /^Events/ }));
  expect(location.hash).toBe("#/live/events?seat=swe");
});

test("Edit in org opens the org editor on this seat", async () => {
  mount("#/agents/seats/swe");
  await settle();
  fireEvent.click(screen.getByRole("button", { name: "More on SWE" }));
  fireEvent.click(screen.getByRole("menuitem", { name: /^Edit in org/ }));
  expect(location.hash).toBe("#/agents/edit?seat=swe");
});

test("a reader who may not act sees every action held, with the reason", async () => {
  mount("#/agents/seats/swe", {
    answers: { viewer: { operator_id: "", operator: false, handle: "", acts: [] } },
    agents: [WORKING],
  });
  await settle();
  for (const name of ["Message", "Assign task", "Pause"]) {
    const button = screen.getByRole("button", { name });
    expect(button.getAttribute("aria-disabled"), `${name} is pressable`).toBe("true");
    expect(button.getAttribute("title")).toMatch(/Set an API token to act/);
  }
});

// FIND, CHOOSE, THEN SAY WHY: the matches are the dialog's content while it
// is finding (in its flow, so all eight are readable), and the reason is
// asked once there is a hand-off for it to be the reason for. The hand-off is
// conditional on the version a read of the chosen task returned.
test("Assign task finds a task, then asks why, and hands it over against the version read", async () => {
  storeToken("t");
  const hits = Array.from({ length: 8 }, (_, i) => ({
    key: `ENG-${20 + i}`,
    title: `PXE task ${i}`,
    assignee: i === 0 ? "cto" : "",
  }));
  mount("#/agents/seats/swe", {
    answers: {
      viewer: { ...OPERATOR, acts: EVERY_ACT },
      work_search: { hits, available: true, mode: "hybrid" },
      work_item: (p: Record<string, unknown>) => ({
        task: { key: p.id, version: 4, title: "PXE task 0", assignee: "cto" },
        complete: true,
      }),
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
  storeToken("t");
  const hits = [
    ...Array.from({ length: 7 }, (_, i) => ({
      key: `ENG-${10 + i}`,
      title: `retry ${i}`,
      assignee: "swe",
    })),
    { key: "ENG-30", title: "retry elsewhere", assignee: "cto" },
    { key: "ENG-31", title: "retry unowned", assignee: "" },
  ];
  mount("#/agents/seats/swe", {
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
  storeToken("t");
  const assignee: Record<string, string> = { "ENG-20": "cto", "ENG-21": "swe" };
  mount("#/agents/seats/swe", {
    answers: {
      viewer: { ...OPERATOR, acts: EVERY_ACT },
      work_search: {
        hits: [
          { key: "ENG-20", title: "Moved away", assignee: "swe" },
          { key: "ENG-21", title: "Moved here", assignee: "cto" },
        ],
        available: true,
        mode: "hybrid",
      },
      work_item: (p: Record<string, unknown>) => ({
        task: { key: p.id, version: 7, title: "t", assignee: assignee[p.id as string] },
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
  await waitFor(() =>
    expect(dialog.getByText(/ENG-21 is already SWE’s — find another/)).toBeTruthy(),
  );
  expect(assign().getAttribute("aria-disabled")).toBe("true");
});

/** The pause dialog, opened. */
async function openPause() {
  fireEvent.click(screen.getByRole("button", { name: "Pause" }));
  return within(await screen.findByRole("dialog", { name: "Pause SWE" }));
}

// A TURN STOPPED AT ITS NEXT ROUND LOSES WHAT IT HAD NOT DONE, so stopping it
// is a separate, explicit choice — and the dialog says what happens without.
test("pausing sends stop_running only when the box is ticked", async () => {
  storeToken("t");
  mount("#/agents/seats/swe", {
    answers: { viewer: { ...OPERATOR, acts: EVERY_ACT } },
    agents: [WORKING],
  });
  await settle();

  let dialog = await openPause();
  expect(dialog.getByText(/the turn it is on finishes first/)).toBeTruthy();
  fireEvent.change(dialog.getByLabelText(/Why/), { target: { value: "rolling the fleet" } });
  fireEvent.click(dialog.getByRole("button", { name: "Pause" }));
  await waitFor(() => expect(posted).toHaveLength(1));
  expect(posted[0]).toEqual({
    tool: "pause_seat",
    args: { handle: "swe", reason: "rolling the fleet" },
  });
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());

  dialog = await openPause();
  fireEvent.click(dialog.getByRole("checkbox", { name: /Also stop the current turn/ }));
  fireEvent.click(dialog.getByRole("button", { name: "Pause" }));
  await waitFor(() => expect(posted).toHaveLength(2));
  expect(posted[1]).toEqual({ tool: "pause_seat", args: { handle: "swe", stop_running: true } });
});

// THREE OUTCOMES, and only one of them keeps the dialog: applied and pending
// are both the change in hand, and a refusal is the engine's sentence, said
// where the reader can fix what it names.
test("a pause the engine refuses keeps the dialog and says why; a pending one closes it", async () => {
  storeToken("t");
  mount("#/agents/seats/swe", {
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
  storeToken("t");
  mount("#/agents/seats/swe", {
    answers: { viewer: { ...OPERATOR, acts: EVERY_ACT } },
    agents: [
      {
        role: "SWE",
        handle: "swe",
        activity: "stopped",
        stopped_reason: "paused",
        paused: {
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
  fireEvent.click(screen.getByRole("button", { name: "Resume" }));
  await waitFor(() => expect(posted).toEqual([{ tool: "resume_seat", args: { handle: "swe" } }]));
});

const PAUSED_SWE: Partial<AgentRow> = {
  role: "SWE",
  handle: "swe",
  activity: "stopped",
  stopped_reason: "paused",
  paused: {
    by: "jane",
    at: "2026-09-21T09:00:00Z",
    reason: "rolling the fleet",
    stop_running: false,
  },
};

// ONE FACT, ONE TONE: a stopped seat is the red state in the seat vocabulary,
// and its notice was amber under a red pill.
test("a pause is said in the stopped state's own tone", async () => {
  mount("#/agents/seats/swe", { agents: [PAUSED_SWE] });
  await settle();
  const notice = document.querySelector(".prof-notices .crewlet-callout") as HTMLElement;
  expect(notice.className).toContain("crewlet-callout--danger");
});

/** The page bar's own "More" — what a phone's bar folds every other action into. */
function pageMore(): string[] {
  fireEvent.click(screen.getByRole("button", { name: "More on this page" }));
  return screen.getAllByRole("menuitem").map((el) => el.textContent ?? "");
}

// ON A PHONE THE BAR KEEPS ONE ACTION IN VIEW: three buttons and the working
// chip were wider than the line, and the "More" holding Events sat off
// screen. Everything folded is still one press away, opening what the inline
// control opens.
test("the phone's More holds every action but Message, and each opens what its button does", async () => {
  storeToken("t");
  mount("#/agents/seats/swe", {
    shell: true,
    answers: { viewer: { ...OPERATOR, acts: EVERY_ACT } },
    agents: [WORKING],
  });
  await settle();
  // FOLDED: hidden on a phone's bar by the frame's own rule.
  expect(
    screen.getByRole("button", { name: "Assign task" }).closest(".page-action-folds"),
  ).not.toBeNull();
  expect(
    screen.getByRole("button", { name: "Pause" }).closest(".page-action-folds"),
  ).not.toBeNull();
  expect(screen.getByRole("button", { name: "Message" }).closest(".page-action-folds")).toBeNull();
  const names = pageMore();
  expect(names.slice(0, 4)).toEqual(["Assign task", "Pause", "Events", "Edit in org"]);
  fireEvent.click(screen.getByRole("menuitem", { name: "Pause" }));
  expect(await screen.findByRole("dialog", { name: "Pause SWE" })).toBeTruthy();
});

// A PAUSED SEAT'S ONE ACTION IS RESUME, so that is the one kept in view.
test("on a paused seat the phone's bar keeps Resume and folds Message", async () => {
  storeToken("t");
  mount("#/agents/seats/swe", {
    shell: true,
    answers: { viewer: { ...OPERATOR, acts: EVERY_ACT } },
    agents: [PAUSED_SWE],
  });
  await settle();
  expect(screen.getByRole("button", { name: "Resume" }).closest(".page-action-folds")).toBeNull();
  expect(
    screen.getByRole("button", { name: "Message" }).closest(".page-action-folds"),
  ).not.toBeNull();
  const names = pageMore();
  expect(names).toContain("Message");
  expect(names).not.toContain("Pause");
});

// HELD THERE AS IT IS INLINE: disabled, with the sentence.
test("a folded action is held in the menu with the reason", async () => {
  mount("#/agents/seats/swe", {
    shell: true,
    answers: { viewer: { operator_id: "", operator: false, handle: "", acts: [] } },
    agents: [WORKING],
  });
  await settle();
  pageMore();
  const assign = screen.getByRole("menuitem", { name: /^Assign task/ });
  expect(assign.getAttribute("aria-disabled")).toBe("true");
  expect(assign.textContent).toMatch(/Set an API token to act/);
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
  mount("#/agents/seats/swe", { answers: { seat_activity: week() } });
  await waitFor(() => expect(tile("Turns · 7 days").textContent).toContain("64"));
  expect(tile("Turns · 7 days").textContent).toContain("+9");
  expect(tile("Turns · 7 days").textContent).toContain("vs last week");
  // ONE ANSWER, both weeks: the chart and the delta are the same days.
  const params = askedFor("seat_activity")[0]?.params ?? {};
  expect(params).toMatchObject({ seat: "swe", days: 7, previous: true });
});

test("the first-pass rate is the engine's, over the turns it reviewed", async () => {
  mount("#/agents/seats/swe", { answers: { seat_activity: week() } });
  await waitFor(() => expect(tile("Approved first pass").textContent).toContain("81%"));
  expect(tile("Approved first pass").textContent).not.toContain("20%");
  expect(tile("Approved first pass").textContent).toContain("12 sent back");
  expect(tile("Median turn").textContent).toContain("3m 20s");
  expect(tile("Median turn").textContent).toContain("p90 11m 40s");
});

test("a week nobody reviewed has no rate, rather than a rate of nought", async () => {
  mount("#/agents/seats/swe", {
    answers: { seat_activity: week({ reviewed: 0, first_pass_pct: undefined, sent_back: 0 }) },
  });
  await waitFor(() => expect(tile("Approved first pass").textContent).toContain("none reviewed"));
  expect(tile("Approved first pass").textContent).not.toContain("0%");
});

// THE FIGURE AND ITS CEILING ARE ONE WINDOW'S: a week's spend over a day's
// limit, or the seven-day sum under a week's cap, would be a ratio of two
// different quantities.
test("the tokens tile is the capped window nearest its ceiling, with that ceiling", async () => {
  mount("#/agents/seats/swe", {
    answers: { seat_activity: week() },
    agents: [
      {
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
  mount("#/agents/seats/swe", { answers: { seat_activity: week() } });
  await waitFor(() => expect(tile("Tokens · 7 days").textContent).toContain("2.1M"));
  expect(tile("Tokens · 7 days").textContent).toContain("no budget caps it");
});

// THE COMPANY'S CEILINGS BIND EVERY SEAT: "no budget caps it" over a company
// with a day and a month set was a claim the gate would contradict.
test("a seat with no budget of its own under a capped company says the company's applies", async () => {
  const org = structuredClone(ORG);
  org.token_budget = { day: 60_000_000, month: 900_000_000 };
  mount("#/agents/seats/swe", { org, answers: { seat_activity: week() } });
  await waitFor(() => expect(tile("Tokens · 7 days").textContent).toContain("2.1M"));
  // NAMED, as the Settings tab names the same ceilings.
  // ONE LINE IN THE TILE — the whole sentence three lines tall stretched the
  // KPI row — and the whole sentence on its title.
  const sub = within(tile("Tokens · 7 days")).getByText(/company cap/);
  expect(sub.textContent).toBe("company cap 60M/day +1");
  expect(sub.getAttribute("title")).toBe(
    "No seat cap — the company's 60M/day and 900M/month apply",
  );
  expect(tile("Tokens · 7 days").textContent).not.toContain("no budget caps it");
});

// THE PAGE IS ONE ROW; THE TOTALS ARE THE HOLDER'S COUNT.
test("the memory card counts what the seat holds, never the page it was sent", async () => {
  mount("#/agents/seats/swe", {
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

test("the current turn names its round against the granted cap, its calls and the one running", async () => {
  mount("#/agents/seats/swe", {
    agents: [WORKING],
    answers: {
      // THE TASK IS NOT IN THE ASSIGNED-WORK TOP FIVE — a turn on a task just
      // asked of the seat, with no priority. Its title comes from reading the
      // turn's own item, never from that list.
      work_items: {
        items: [{ key: "ENG-7", title: "Something else entirely", status: "todo" }],
        total_hint: 1,
      },
      work_item: (p: Record<string, unknown>) =>
        p.id === "ENG-412"
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
  expect(askedFor("work_item").map((q) => q.params)).toContainEqual({ id: "ENG-412" });
  // THE ENGINE'S ROUND, one-based, against what the phase was GRANTED.
  expect(card.textContent).toContain("Execute · round 7 of 25");
  const rows = [...card.querySelectorAll(".prof-feed-row")];
  expect(rows.map((r) => r.querySelector(".prof-feed-name")?.textContent)).toEqual([
    "knowledge.search",
    "sandbox.run",
    "sandbox.run",
  ]);
  expect(rows[0]?.textContent).toContain("PXE retry backoff");
  expect(rows[1]?.querySelector(".prof-feed-took")?.textContent).toMatch(/^failed/);
  expect(rows[2]?.getAttribute("data-running")).toBe("true");
  expect(rows[2]?.textContent).toContain("go test ./provisioner/... -run Timeout");
  expect(rows[2]?.textContent).toContain("running");
  // The recorded phases are none yet, so the turn's tokens are the running phase's.
  expect(card.textContent).toContain("2,500 tokens");
  expect(within(card).getByRole("link", { name: "Watch live" }).getAttribute("href")).toBe(
    "#/live/turns/t-7",
  );
});

test("the setup card reads the chart's model and tools, and the document's own rows", async () => {
  storeToken("t");
  mount("#/agents/seats/swe", { answers: { viewer: OPERATOR, config: DOCUMENT } });
  await waitFor(() => expect(screen.getByText(/e2b · claude-code/)).toBeTruthy());
  const setup = screen.getByText("Setup").closest(".crewlet-card") as HTMLElement;
  expect(setup.textContent).toContain("anthropic-main");
  expect(setup.textContent).toContain("openai-fallback");
  expect(setup.textContent).toContain("gitlab");
  expect(setup.textContent).toContain("nodes labelled pool=build");
  expect(setup.textContent).toContain("test-writer, reviewer-lite");
  expect(within(setup).getByRole("link", { name: "Edit" }).getAttribute("href")).toBe(
    "#/agents/edit?seat=swe",
  );
});

test("without the document the setup card says whose read it is, not that nothing is set", async () => {
  mount("#/agents/seats/swe");
  await settle();
  const setup = screen.getByText("Setup").closest(".crewlet-card") as HTMLElement;
  // THE PUBLIC HALF STILL DRAWS.
  expect(setup.textContent).toContain("anthropic-main");
  expect(setup.textContent).toContain("which an operator token reads");
  expect(setup.textContent).not.toContain("not offered");
});

// ---------------------------------------------------------------------------
// A person's day
// ---------------------------------------------------------------------------

test("a colleague's day is withheld with the reason, and never asked for", async () => {
  mount("#/agents/seats/jane", {
    answers: { viewer: { operator_id: "t-cto", operator: false, handle: "cto", acts: [] } },
  });
  await settle();
  expect(screen.getByText("Their day")).toBeTruthy();
  expect(screen.getByText(/needs an operator credential/)).toBeTruthy();
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
  mount("#/agents/seats/jane", {
    answers: {
      viewer: { operator_id: "jane-token", handle: "jane", acts: [] },
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

  // THE WORK TAB'S BLOCK SAYS THE SAME NUMBER, from the same answer.
  fireEvent.click(screen.getByRole("tab", { name: /Work/ }));
  const block = (await screen.findByText("What they mean to do first")).closest(
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
  storeToken("t");
  mount("#/agents/seats/jane", {
    shell: true,
    answers: {
      viewer: { operator_id: "jane-token", handle: "jane", name: "Jane Founder", acts: [] },
      work_inbox: inboxPage(50, true),
      work_person: { handle: "jane", unread: [], version: 1, held: true, complete: true },
    },
  });
  await waitFor(() => expect(tile("Unread").textContent).toContain("50+"));
  // THE SIDEBAR'S INBOX ROW, which carries the same figure as its badge.
  const nav = document.querySelector(".crewlet-sidebar-nav") as HTMLElement;
  const row = within(nav).getByRole("link", { name: /Inbox/ });
  expect(row.textContent).toContain("50+");
  // ONE READING: the frame's, never a second read of the same inbox.
  expect(askedFor("work_inbox")).toHaveLength(1);
  expect(screen.getByText("Your day")).toBeTruthy();
  const day = screen.getByText("Your day").closest(".prof-day-head") as HTMLElement;
  expect(within(day).getByRole("link", { name: "Inbox" }).getAttribute("href")).toBe("#/inbox");
  expect(within(day).getByRole("link", { name: "My work" }).getAttribute("href")).toBe("#/me");
  expect(screen.queryByText(/Read-only here/)).toBeNull();
});

test("an operator reading a colleague's day asks that colleague's inbox", async () => {
  mount("#/agents/seats/jane", {
    answers: {
      viewer: { operator_id: "ops", operator: true, handle: "cto", acts: [] },
      work_inbox: (p: Record<string, unknown>) =>
        p.handle === "jane" ? inboxPage(7, false) : inboxPage(0, false),
      work_person: { handle: "jane", unread: [], version: 1, held: true, complete: true },
    },
  });
  await waitFor(() => expect(tile("Unread").textContent).toContain("7"));
  expect(askedFor("work_inbox").map((a) => a.params.handle)).toContain("jane");
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
  mount("#/agents/seats/swe", { org: withCharter(five) });
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
  mount("#/agents/seats/swe", { org: withCharter(["One", "Two"]) });
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
  mount("#/agents/seats/cto");
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
  mount("#/agents/seats/swe");
  await settle();
  expect(screen.queryByText("Direct reports")).toBeNull();
});

// ---------------------------------------------------------------------------
// The other tabs
// ---------------------------------------------------------------------------

test("the work tab asks for every row filtered on its own", async () => {
  mount("#/agents/seats/swe?tab=work", { answers: { work_items: { items: [], total_hint: 0 } } });
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
  mount("#/agents/seats/swe?tab=turns", {
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

// A TURN STILL RUNNING HAS NOTHING SETTLED: no summary, no iteration count and
// no final tokens. The list said "no summary recorded · 0 · 0 · running" for a
// turn the Overview's card named, in the same second, as round 7 on ENG-412 —
// so the row asks the push what the seat is doing and what it is on, and the
// figures it does not have are marked as not settled rather than zero.
test("a running turn's row says what it is doing, and has no figures yet", async () => {
  mount("#/agents/seats/swe?tab=turns", {
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
  const doing = await screen.findByText("running — executing · round 7 of 25");
  const row = doing.closest(".grid-row") as HTMLElement;
  expect(within(row).getByText("ENG-412")).toBeTruthy();
  expect(within(row).queryByText("no summary recorded")).toBeNull();
  expect(within(row).getAllByText("Not settled — the turn has not ended")).toHaveLength(2);
  // A SETTLED TURN'S STATE CELL HOLDS NOTHING AT ALL, which is what a phone's
  // card drops: an empty wrapper left a "STATE" label alone on its line.
  const settled = screen.getByText("did thing 1").closest(".grid-row") as HTMLElement;
  const cells = [...settled.querySelectorAll(".grid-cell")];
  const state = cells.find((c) => c.getAttribute("data-label") === "State")!;
  expect(state.childNodes).toHaveLength(0);
});

// A LIVE TURN IS ON SCREEN WHEN THE TAB OPENS: "Running now" leads the tab,
// where under the fifty-row table it was 2,000px down. The settled cards
// after the table are titled, or they read as the table repeating itself, and
// the table's last column is named like every other.
test("the turns tab leads with the running turn and titles the transcripts", async () => {
  mount("#/agents/seats/swe?tab=turns", {
    agents: [WORKING],
    answers: { turns: { turns: [turnRow(1)], next: null } },
  });
  const live = await screen.findByRole("heading", { name: "Running now" });
  const table = screen.getByRole("heading", { name: "Turns" });
  const transcripts = screen.getByRole("heading", { name: "Transcripts · newest first" });
  const order = (a: Node, b: Node) =>
    Boolean(a.compareDocumentPosition(b) & Node.DOCUMENT_POSITION_FOLLOWING);
  expect(order(live, table), "Running now comes before the table").toBe(true);
  expect(order(table, transcripts), "the transcripts follow it, titled").toBe(true);
  await screen.findByText("did thing 1");
  const heads = [...document.querySelectorAll(".grid-th")].map((h) => h.textContent?.trim());
  expect(heads).toContain("State");
  expect(heads).not.toContain("");
});

// THE HEADER COUNTS WHAT IS LOADED, never the history: `.length` of the first
// page read "50" under "the newest 50 this seat took" whether the seat had
// fifty-one turns or four hundred, and nothing reached the fifty-first.
test("the turns list says it holds a page, and loads the one before it", async () => {
  mount("#/agents/seats/swe?tab=turns", {
    answers: {
      turns: (p: Record<string, unknown>) =>
        p.before
          ? { turns: [turnRow(50), turnRow(51)], next: null }
          : {
              turns: Array.from({ length: 50 }, (_, i) => turnRow(i)),
              next: "2026-09-20T00:00:00Z",
            },
    },
  });
  await settle();
  const first = askedFor("turns")[0]?.params ?? {};
  // EVERY DAY THE STORE KEEPS, named: the read's default is a week.
  expect(first).toMatchObject({ seat: "swe", days: 30, limit: 50 });
  const card = screen
    .getByRole("heading", { name: "Turns" })
    .closest(".crewlet-card") as HTMLElement;
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
    before: "2026-09-20T00:00:00Z",
  });
  expect(card.querySelector(".crewlet-card__header")?.textContent).toContain("52");
  expect(card.querySelector(".crewlet-card__header")?.textContent).not.toContain("52+");
  expect(within(card).queryByRole("button", { name: "Load older turns" })).toBeNull();
});

// A KEYLESS ITEM IS NAMED SHORT, whole on its title: `native:<uuid>` took the
// whole cell on a phone and laid the summary out one letter per line.
test("a turn's keyless work item is named by its kind and the head of its id", async () => {
  const id = "b375beaf-1d2e-4c6a-9f0b-2a1c3d4e5f60";
  mount("#/agents/seats/swe?tab=turns", {
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
  expect(keyless.getAttribute("title")).toContain(`native:${id}`);
  expect(keyless.className).toContain("turn-what-key");
  expect(screen.getByText("ENG-29").className).toContain("turn-what-key");
  expect(document.body.textContent).not.toContain(`native:${id}`);
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
  mount("#/agents/seats/swe?tab=memory", {
    answers: {
      agent_memory: memoryOf(),
      conversations: {
        handle: "swe",
        conversations: [],
        conversations_total: 0,
        entries: [],
        held_by: "node-2",
      },
    },
  });
  await waitFor(() => expect(screen.getByText(/Latest 50 of 142/)).toBeTruthy());
  expect(screen.getByText(/Read from node-2, the node holding this seat/)).toBeTruthy();
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
  mount("#/agents/seats/swe?tab=memory", {
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
  mount("#/agents/seats/swe?tab=memory", {
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

// THE CONVERSATION COLUMN IS CAPPED AND A UUID IS CUT TO ITS HEAD: printed
// whole, a native task's `work:task:<uuid>` took the grid's width and left
// "What it did" a third of it.
test("an episode's conversation prints a uuid's head, and the whole key on its title", async () => {
  const key = "work:task:4d631f6d-5107-46f0-8043-a1f9d7a74abc";
  mount("#/agents/seats/swe?tab=memory", {
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
  mount("#/agents/seats/swe?tab=memory", {
    answers: {
      agent_memory: memoryOf({ diary: [], diary_total: 0, held_by: "none" }),
      conversations: {
        handle: "swe",
        conversations: [],
        conversations_total: 0,
        entries: [],
        held_by: "none",
      },
    },
  });
  await waitFor(() =>
    expect(screen.getAllByText(/No node holds this seat, so no copy/).length).toBe(2),
  );
});

const THREAD = "slack:C0TEAM/1709280000.000100";

const threads = (entries: unknown[]) => (p: Record<string, unknown>) => ({
  handle: "swe",
  held_by: "node-2",
  conversations: [{ key: THREAD, turns: 2, last_at: "2026-03-01T09:00:00Z" }],
  conversations_total: 1,
  entries: p.conversation ? entries : [],
});

/** A card's header by its title. */
function header(title: string): HTMLElement {
  const el = screen.getByText(title).closest(".crewlet-card__header");
  expect(el, `no card header carries the title ${title}`).not.toBeNull();
  return el as HTMLElement;
}

// A THREAD'S KEY IS ITS ONLY IDENTITY, SO IT HAS THE ROW: its own line, whole,
// with what is known about the thread as the caption under it. Beside a tag
// and a full timestamp it kept 78px of the list and ENG-28 to ENG-32 all read
// `work:ENG-…`. A native task's uuid is cut to its head, the key whole on the
// row's title.
test("a thread's key has its row, a uuid cut to its head and the key on the title", async () => {
  const native = "work:task:b375beaf-52ee-4db1-a1e3-06d450fbd9f1";
  mount("#/agents/seats/swe?tab=memory", {
    answers: {
      agent_memory: memoryOf(),
      conversations: {
        handle: "swe",
        held_by: "node-2",
        conversations: [
          { key: "work:ENG-9999", turns: 3, last_at: "2026-09-21T07:00:00Z" },
          { key: native, turns: 1, last_at: "2026-09-21T06:00:00Z" },
        ],
        conversations_total: 2,
        entries: [],
      },
    },
  });
  const whole = await screen.findByText("work:ENG-9999");
  const row = whole.closest("button")!;
  // ON A LINE OF ITS OWN: the key is a direct child of the row, and nothing
  // else shares its line.
  expect(whole.parentElement).toBe(row);
  expect(row.getAttribute("title")).toBe("work:ENG-9999");
  expect(row.querySelector(".crewlet-tag"), "no tag competes with the key").toBeNull();
  expect(row.textContent).toContain("3 turns");
  const cut = screen.getByText("work:task:b375beaf");
  expect(cut.closest("button")!.getAttribute("title")).toBe(native);
  expect(screen.queryByText(native)).toBeNull();
});

test("with no thread open the pane counts nothing and instructs only from its empty state", async () => {
  mount("#/agents/seats/swe?tab=memory", {
    answers: { agent_memory: memoryOf(), conversations: threads([]) },
  });
  await screen.findByText("Thread turns");
  expect(
    header("Thread turns").querySelector(".crewlet-count"),
    "nothing was asked for, so a 0 here states a quantity about a thread nobody named",
  ).toBeNull();
  expect(
    screen.getByText(/Choose a thread to see the turns this seat recorded in it/),
  ).toBeTruthy();
});

test("an open thread's turns are counted, and opening one names it on the question", async () => {
  mount(`#/agents/seats/swe?tab=memory&conversation=${encodeURIComponent(THREAD)}`, {
    answers: {
      agent_memory: memoryOf(),
      conversations: threads([
        { turn_id: "t1", at: "2026-03-01T09:00:00Z", intent: "answered the question" },
        { turn_id: "t2", at: "2026-03-01T10:00:00Z", intent: "followed up" },
      ]),
    },
  });
  await screen.findByText("Thread turns");
  await waitFor(() =>
    expect(header("Thread turns").querySelector(".crewlet-count")?.textContent).toBe("2"),
  );
  expect(askedFor("conversations").at(-1)?.params).toMatchObject({
    handle: "swe",
    conversation: THREAD,
  });
});

/** Record every `scrollIntoView` (jsdom lays nothing out and may not have
 *  one), and put back whatever was there. */
function spyScroll() {
  const had = Object.getOwnPropertyDescriptor(Element.prototype, "scrollIntoView");
  const fn = vi.fn();
  Object.defineProperty(Element.prototype, "scrollIntoView", {
    configurable: true,
    writable: true,
    value: fn,
  });
  return {
    mock: fn.mock,
    restore: () => {
      if (had) Object.defineProperty(Element.prototype, "scrollIntoView", had);
      else delete (Element.prototype as unknown as Record<string, unknown>).scrollIntoView;
    },
  };
}

// CHOOSING A THREAD SHOWS IT. Stacked on a phone the detail is below the
// whole list — 1,900px under a row near the top — and at 1440 it sat a screen
// above a row near the end, so the row's highlight was the only feedback the
// section's one interaction gave. The detail's heading is scrolled to and
// focused when it is off screen; when it is already in view (the sticky
// column beside the list) nothing moves, focus included.
test("choosing a thread brings its turns on screen and focuses their heading", async () => {
  const scrolled = spyScroll();
  const rect = vi.spyOn(Element.prototype, "getBoundingClientRect");
  // THE DETAIL IS FAR BELOW the scroller's view, as it is on a phone.
  rect.mockImplementation(function (this: Element) {
    const top = this.id === "prof-thread-turns" ? 1900 : 0;
    return { top, bottom: top + 20, left: 0, right: 300, width: 300, height: 20 } as DOMRect;
  });
  try {
    mount("#/agents/seats/swe?tab=memory", {
      answers: { agent_memory: memoryOf(), conversations: threads([]) },
    });
    const row = (await screen.findByText(THREAD)).closest("button")!;
    fireEvent.click(row);
    const heading = await screen.findByRole("heading", { name: "Thread turns" });
    await waitFor(() => expect(document.activeElement).toBe(heading));
    expect(scrolled.mock.contexts).toContain(heading);
    // AND THE SUBTITLE NAMES WHICH THREAD, the list that says so being a
    // screen away.
    expect(header("Thread turns").textContent).toContain(THREAD);
  } finally {
    scrolled.restore();
    rect.mockRestore();
  }
});

test("a thread whose turns are already on screen moves nothing", async () => {
  // jsdom's every rect is the origin's, so the heading reads as in view.
  const scrolled = spyScroll();
  try {
    mount("#/agents/seats/swe?tab=memory", {
      answers: { agent_memory: memoryOf(), conversations: threads([]) },
    });
    const row = (await screen.findByText(THREAD)).closest("button")!;
    row.focus();
    fireEvent.click(row);
    await waitFor(() => expect(row.getAttribute("aria-pressed")).toBe("true"));
    const heading = screen.getByRole("heading", { name: "Thread turns" });
    expect(scrolled.mock.contexts).not.toContain(heading);
    expect(document.activeElement).toBe(row);
  } finally {
    scrolled.restore();
  }
});

// A THREAD NAMED IN THE ADDRESS the page was opened on is not a gesture: the
// page opens at its top rather than jumping to it.
test("a thread opened from the address does not scroll the page to it", async () => {
  const scrolled = spyScroll();
  const rect = vi.spyOn(Element.prototype, "getBoundingClientRect");
  rect.mockImplementation(function (this: Element) {
    const top = this.id === "prof-thread-turns" ? 1900 : 0;
    return { top, bottom: top + 20, left: 0, right: 300, width: 300, height: 20 } as DOMRect;
  });
  try {
    mount(`#/agents/seats/swe?tab=memory&conversation=${encodeURIComponent(THREAD)}`, {
      answers: { agent_memory: memoryOf(), conversations: threads([]) },
    });
    const heading = await screen.findByRole("heading", { name: "Thread turns" });
    expect(scrolled.mock.contexts).not.toContain(heading);
    expect(document.activeElement).not.toBe(heading);
  } finally {
    scrolled.restore();
    rect.mockRestore();
  }
});

const schedule = (over: Partial<ScheduleRow>): ScheduleRow => ({
  scope_type: "role",
  scope_id: "swe",
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
  mount("#/agents/seats/swe?tab=schedules", {
    schedules: [
      schedule({ scope_type: "unit", scope_id: "Core", name: "weekly-review", runners: ["swe"] }),
      schedule({
        name: "broken",
        timezone: "Mars/Olympus",
        next_run: undefined,
        problem: "unknown time zone Mars/Olympus",
      }),
      // Another seat's: not this one's day.
      schedule({ scope_id: "cto", name: "board-prep", runners: ["cto"] }),
    ],
  });
  await settle();
  expect(screen.getByText("weekly-review")).toBeTruthy();
  expect(screen.getByText("cannot fire")).toBeTruthy();
  expect(screen.queryByText("board-prep")).toBeNull();
});

// A CREDENTIAL IS NAMED, NEVER SHOWN — not its reference, not the mask, and
// not a literal a redaction bug let through.
test("the settings tab names a seat's credentials and prints none of their values", async () => {
  storeToken("t");
  mount("#/agents/seats/swe?tab=settings", { answers: { viewer: OPERATOR, config: DOCUMENT } });
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
    "#/agents/edit?seat=swe",
  );
});

// THE GUARDED DOCUMENT IS READ ONCE FOR THE WHOLE PROFILE. The shell reads it
// for every tab that draws it, and `useQuery` shares no request between two
// callers — so the Settings tab reading it again fetched the whole company
// document twice on every open.
test("the settings tab asks for the company document once", async () => {
  storeToken("t");
  mount("#/agents/seats/swe?tab=settings", { answers: { viewer: OPERATOR, config: DOCUMENT } });
  await waitFor(() => expect(screen.getByText("API_TOKEN")).toBeTruthy());
  await settle();
  expect(askedFor("config")).toHaveLength(1);
});

test("a person's settings carry their contacts and no model, budget or credential card", async () => {
  storeToken("t");
  mount("#/agents/seats/jane?tab=settings", { answers: { viewer: OPERATOR, config: DOCUMENT } });
  await waitFor(() => expect(screen.getByText("U0FOUNDER")).toBeTruthy());
  expect(screen.queryByText("Tool credentials")).toBeNull();
  expect(screen.queryByText("Model and budget")).toBeNull();
  // A CONTACT IS NAMED AS A PERSON NAMES IT, never its config key with the
  // underscores swapped for spaces.
  expect(screen.getByText("Slack member id")).toBeTruthy();
  expect(screen.queryByText("slack user id")).toBeNull();
});

test("a contact key is labelled by the table, and one a newer engine added in sentence case", () => {
  expect(contactLabel("crewlet_operator_id")).toBe("Operator id");
  expect(contactLabel("github_login")).toBe("GitHub login");
  expect(contactLabel("teams_user_id")).toBe("Teams user id");
});

// A WINDOW THE SEAT LEAVES UNCAPPED IS THE COMPANY'S, where the company caps it.
test("the budget rows say where the company's ceiling applies to an uncapped window", async () => {
  const org = structuredClone(ORG);
  org.token_budget = { day: 60_000_000 };
  storeToken("t");
  mount("#/agents/seats/swe?tab=settings", {
    org,
    answers: { viewer: OPERATOR, config: DOCUMENT },
  });
  await waitFor(() => expect(screen.getByText("Model and budget")).toBeTruthy());
  const card = screen.getByText("Model and budget").closest(".crewlet-card") as HTMLElement;
  await waitFor(() => expect(card.textContent).toContain("3M tokens"));
  expect(card.textContent).toContain("the company’s 60M applies");
  // SWE's own week is its own; the month is capped by nobody.
  expect(card.textContent).toContain("3M tokens");
  expect(card.textContent).toContain("uncapped");
});

// ---------------------------------------------------------------------------
// The trail
// ---------------------------------------------------------------------------

test("the trail names the seat and the unit it sits in, and titles the tab with the name", async () => {
  mount("#/agents/seats/swe", { shell: true });
  await settle();
  const trail = screen.getByRole("navigation", { name: "Breadcrumb" });
  const here = trail.querySelector("[aria-current='page']");
  expect(here?.querySelector(".crumb-text")?.textContent).toBe("SWE");
  // ITS BADGE, the outline every other seat badge wears.
  expect(here?.querySelector(".crumb-seat")).not.toBeNull();
  const unit = within(trail).getByRole("link", { name: "Engineering · Core" });
  expect(unit.getAttribute("href")).toBe("#/agents/teams/Core");
  expect(document.title).toBe("SWE · Crewlet");
});
