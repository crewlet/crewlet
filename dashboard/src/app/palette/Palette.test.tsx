/**
 * What the palette PROMISES, held against what it does.
 *
 * Every case is a claim the palette makes about itself — in a tab, a hint, a
 * footer or a row — and none of them could fail a type check: a hit that opens
 * the wrong screen, an answer drawn under the wrong question, a suggestion
 * that is really a guess and a row that writes as nobody are all well-typed.
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { CommandPalette } from "./Palette.tsx";
import { forgetAnswersForTest } from "./answer.ts";
import { ANSWER_IDLE_MS } from "./hits.ts";
import { COLLEAGUE_QUERY_MAX } from "~/contract/wire.ts";
import { Router } from "../router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { useViewer, type ViewerState } from "~/lib/viewer.ts";
import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";

vi.mock("~/lib/viewer.ts", () => ({ useViewer: vi.fn() }));

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

const EVERY_ACT = ["update_work_item", "create_work_item", "answer_knowledge"];

const JANE: ViewerState = {
  login: "founder",
  grants: [
    "config:read",
    "config:write",
    "secrets:write",
    "fleet:operate",
    "people:manage",
    "audit:read",
    "state:read",
    "work:write",
    "knowledge:write",
  ],
  operatesFleet: true,
  handle: "jane",
  owner: "jane",
  name: "Jane Founder",
  acts: EVERY_ACT,
  project: "ENG",
  kind: "human",
  unbound: false,
  anonymous: false,
  loading: false,
  asking: false,
};

const ANONYMOUS: ViewerState = {
  ...JANE,
  login: "",
  grants: [],
  operatesFleet: false,
  handle: "",
  owner: "",
  name: "",
  acts: [],
  project: "",
  kind: "",
  anonymous: true,
};

/**
 * A principal no seat binds — a pipeline, an operator outside the chart. It
 * ACTS, under its own login (ADR-0024); what it lacks is a seat.
 */
const UNBOUND: ViewerState = {
  ...ANONYMOUS,
  login: "ci",
  owner: "ci",
  grants: ["state:read", "work:write"],
  acts: EVERY_ACT,
  anonymous: false,
  unbound: true,
};

const ORG = {
  name: "Acme",
  roles: [
    { name: "SWE", handle: "swe", goal: "Ship the engine" },
    { name: "Jane Founder", handle: "jane", kind: "human", goal: "Run it" },
  ],
  units: [
    {
      name: "Platform",
      type: "team",
      roles: [{ name: "SRE", handle: "sre", goal: "Keep it up" }],
    },
  ],
};

const HIT = {
  id: "t1",
  key: "ENG-420",
  title: "Flaky e2e: cluster join under packet loss",
  project: "ENG",
  type: "bug",
  status: "todo",
  priority: "high",
  rank: 1,
};

const PROJECTS = [
  { key: "ENG", name: "Core platform", unit: { name: "Engineering" }, task_counts: {}, version: 1 },
  { key: "OPS", name: "Operations", unit: { name: "Platform" }, task_counts: {}, version: 1 },
];

const OUTCOME = {
  mode: "hybrid",
  served_mode: "hybrid",
  modes: ["hybrid", "keyword", "semantic"],
  degraded: "",
  coverage: { nodes: [], complete: true, buckets_missing: 0 },
};

/** What each question answers, per params. Absent answers never come back. */
type Answers = Partial<Record<string, (params: Record<string, unknown>) => Promise<unknown>>>;

let asked: { kind: string; params: Record<string, unknown> }[];
let posted: { tool: string; body: { request_id: string; args: Record<string, unknown> } }[];

function json(payload: unknown, status = 200): Response {
  return new Response(JSON.stringify(payload), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

/** The act route, answering each tool with `reply`. */
function engine(reply: (tool: string, args: Record<string, unknown>) => Response) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      const tool = decodeURIComponent(String(url).split("/operator/act/")[1] ?? "");
      const body = JSON.parse(init.body as string) as {
        request_id: string;
        args: Record<string, unknown>;
      };
      posted.push({ tool, body });
      return reply(tool, body.args);
    }),
  );
}

function mount({
  viewer = JANE,
  answers = {},
  hash = "#/",
  agents = [{ id: "swe", agent_id: "a-swe", role: "SWE", handle: "swe", activity: "working" }],
}: {
  viewer?: ViewerState;
  answers?: Answers;
  hash?: string;
  agents?: Record<string, unknown>[];
} = {}) {
  location.hash = hash;
  vi.mocked(useViewer).mockReturnValue(viewer);
  const store = new Store();
  store.setConnected(true);
  store.applyOrg(ORG as never);
  // THE ROSTER, which is how a seat reaches the list: an overlay for a seat
  // the roster does not carry is dropped (`Store.applyAgents`).
  store.applySeats(agents as never);
  const socket = new LiveSocket(store);
  socket.query = ((kind: string, params?: Record<string, unknown>) => {
    asked.push({ kind, params: params ?? {} });
    const answer = answers[kind];
    return answer ? answer(params ?? {}) : new Promise(() => {});
  }) as typeof socket.query;
  const closed = vi.fn();
  const keys = vi.fn();
  render(
    <ToastProvider>
      <LayerHost>
        <ClientContext.Provider value={{ store, socket }}>
          <Router>
            <CommandPalette onClose={closed} onShowKeys={keys} />
          </Router>
        </ClientContext.Provider>
      </LayerHost>
    </ToastProvider>,
  );
  const input = screen.getByRole("combobox");
  return {
    closed,
    keys,
    input,
    type: async (value: string) => {
      await act(async () => {
        fireEvent.change(input, { target: { value } });
      });
    },
    press: async (key: string, init: KeyboardEventInit = {}) => {
      await act(async () => {
        fireEvent.keyDown(input, { key, ...init });
      });
    },
    /** The option carrying this text. */
    row: (text: string | RegExp) =>
      screen.getByText(text).closest('[role="option"]') as HTMLElement,
    tab: () => screen.getByRole("tab", { selected: true }).textContent,
  };
}

beforeEach(() => {
  asked = [];
  posted = [];
  forgetAnswersForTest();
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  Element.prototype.scrollIntoView = () => {};
  localStorage.clear();
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  location.hash = "#/";
});

describe("scopes and sigils", () => {
  test("the five scopes are tabs, All first", () => {
    mount();
    expect(screen.getAllByRole("tab").map((t) => t.textContent)).toEqual([
      "All",
      "Tasks",
      "Pages",
      "Agents",
      "Actions",
    ]);
  });

  // A SIGIL SELECTS ITS TAB AND LEAVES THE BOX, so the tab row says where the
  // reader is and the query is what they are looking for.
  test.each([
    ["#", "Tasks"],
    ["@", "Agents"],
    [">", "Actions"],
  ])("%s opens %s", async (sigil, tab) => {
    const p = mount();
    await p.type(`${sigil}abc`);
    expect(p.tab()).toBe(tab);
    expect((p.input as HTMLInputElement).value).toBe("abc");
  });

  test("Tab walks the scopes from the field", async () => {
    const p = mount();
    await p.press("Tab");
    expect(p.tab()).toBe("Tasks");
    await p.press("Tab", { shiftKey: true });
    await p.press("Tab", { shiftKey: true });
    expect(p.tab()).toBe("Actions");
  });

  // THE FOOTER SAYS "seats and units" — so a unit is listed before anything
  // is typed, as the old `@` scope once failed to.
  test("@ lists teams beside agents before anything is typed", async () => {
    const p = mount();
    await p.type("@");
    expect(screen.getByText("Teams")).toBeDefined();
    expect(screen.getByText("Platform")).toBeDefined();
    expect(screen.getByText("SWE")).toBeDefined();
  });
});

// A SEAT'S STATE IN THE CHART'S WORDS: the pauser is a person, and a hint
// reading "Paused by jane" names an address where a person is meant.
test("an agent's state names its pauser by the chart's name", async () => {
  const p = mount({
    agents: [
      {
        id: "sre",
        agent_id: "a-sre",
        role: "SRE",
        handle: "sre",
        activity: "stopped",
        stopped_reason: "paused",
        paused: { by: "jane", by_kind: "human", at: new Date().toISOString(), reason: "" },
      },
    ],
  });
  await p.type("@");
  expect(p.row("SRE").textContent).toContain("Paused by Jane Founder");
});

describe("an id pasted out of a log", () => {
  test("the trace hit opens the trace, not the whole event log", async () => {
    const id = "22222222-2222-4222-8222-222222222222";
    const p = mount();
    await p.type(id);
    fireEvent.click(p.row("as a trace — every event that carries it"));
    expect(location.hash).toBe(`#/live/traces/${id}`);
  });
});

describe("the task search has four answers, kept apart", () => {
  const answering = (reply: (q: string) => Promise<unknown>): Answers => ({
    work_search: (params) => reply(String(params.q)),
  });

  test("too short is not a search", async () => {
    const p = mount();
    await p.type("#a");
    expect(screen.getByText(/at least two characters/)).toBeDefined();
    expect(asked.filter((a) => a.kind === "work_search")).toEqual([]);
  });

  test("hybrid, eight at most, and a hit names its status in words", async () => {
    const p = mount({
      answers: answering(async () => ({ ...OUTCOME, hits: [HIT], available: true })),
    });
    await p.type("#cluster join");
    expect(asked.find((a) => a.kind === "work_search")!.params).toMatchObject({
      q: "cluster join",
      mode: "hybrid",
      limit: 8,
    });
    expect(p.row(/Flaky e2e/).textContent).toContain("To do · unassigned");
    expect(screen.getByText(/See every match for “cluster join”/)).toBeDefined();
  });

  test("a refused read says so rather than claiming nothing matched", async () => {
    const p = mount({ answers: answering(() => Promise.reject(new Error("unknown_query"))) });
    await p.type("#auth");
    expect(screen.getByText(/does not serve this answer/)).toBeDefined();
    expect(screen.queryByText(/No task matches/)).toBeNull();
  });

  test("a building index is not an empty company", async () => {
    const p = mount({
      answers: answering(async () => ({ ...OUTCOME, hits: [], available: false })),
    });
    await p.type("#auth");
    expect(screen.getByText(/still building/)).toBeDefined();
  });

  test("an answered term with nothing does say nothing matched", async () => {
    const p = mount({
      answers: answering(async () => ({ ...OUTCOME, hits: [], available: true })),
    });
    await p.type("#zzz");
    expect(screen.getByText("No task matches “zzz”.")).toBeDefined();
  });

  test("the hits for one term are never shown under the next", async () => {
    const p = mount({
      answers: answering((q) =>
        q === "auth"
          ? Promise.resolve({ ...OUTCOME, hits: [HIT], available: true })
          : new Promise(() => {}),
      ),
    });
    await p.type("#auth");
    expect(screen.getByText(/Flaky e2e/)).toBeDefined();
    await p.type("authz");
    expect(screen.queryByText(/Flaky e2e/)).toBeNull();
    expect(screen.getByText("Searching…")).toBeDefined();
  });
});

describe("the knowledge answer", () => {
  const QUESTION = "why does cluster join flake";
  const RECEIPT = {
    answer_md: "**ENG-420** tracks it [1].",
    sources: [{ kind: "task", ref: "ENG-420", title: "Flaky e2e" }],
    tokens: { input: 1200, output: 240 },
    model: "small-model",
    cached: false,
  };

  function answering() {
    engine((tool) =>
      tool === "answer_knowledge"
        ? json({ tool, outcome: "applied", position: "", receipt: RECEIPT })
        : json({ error: "not_found", detail: "no" }, 404),
    );
  }

  const settle = async (ms: number) => {
    await act(async () => {
      vi.advanceTimersByTime(ms);
    });
    await act(async () => {});
  };

  test("is asked once per settled question, and drawn with its sources and tokens", async () => {
    vi.useFakeTimers();
    answering();
    const p = mount();
    await p.type(QUESTION);
    await settle(ANSWER_IDLE_MS - 1);
    expect(posted).toEqual([]);
    await settle(1);
    expect(posted.map((x) => [x.tool, x.body.args])).toEqual([
      ["answer_knowledge", { q: QUESTION }],
    ]);
    await settle(0);
    expect(screen.getByText("Answer from your company’s knowledge")).toBeDefined();
    expect(screen.getByText("1,440 tokens, charged to the company · small-model")).toBeDefined();
    // Every source is also a row, so the keyboard reaches it.
    expect(screen.getAllByText("ENG-420").length).toBeGreaterThan(0);

    // THE SAME QUESTION AGAIN spends nothing: kept for the session.
    await p.type("x");
    await p.type(`  ${QUESTION.toUpperCase()} `);
    await settle(ANSWER_IDLE_MS);
    expect(posted).toHaveLength(1);
    expect(screen.getByText("Answer from your company’s knowledge")).toBeDefined();
  });

  // TOKENS, NEVER MONEY — the unit the company's budget counts.
  test("its footnote is tokens and never a price", async () => {
    vi.useFakeTimers();
    answering();
    const p = mount();
    await p.type(QUESTION);
    await settle(ANSWER_IDLE_MS);
    await settle(0);
    const lead = screen
      .getByText("Answer from your company’s knowledge")
      .closest(".palette-answer")!;
    expect(lead.textContent).toMatch(/tokens/);
    expect(lead.textContent).not.toMatch(/[$€£¥]|USD|cost|price/i);
  });

  test("a new term abandons the question before it, which is never drawn", async () => {
    vi.useFakeTimers();
    let release: (r: Response) => void = () => {};
    const signals: AbortSignal[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn((_url: string, init: RequestInit) => {
        signals.push(init.signal as AbortSignal);
        posted.push({ tool: "answer_knowledge", body: JSON.parse(init.body as string) });
        return new Promise<Response>((resolve) => {
          release = resolve;
        });
      }),
    );
    const p = mount();
    await p.type(QUESTION);
    await settle(ANSWER_IDLE_MS);
    expect(signals).toHaveLength(1);
    await p.type("who owns the billing runbook");
    expect(signals[0]!.aborted).toBe(true);
    release(json({ tool: "answer_knowledge", outcome: "applied", receipt: RECEIPT }));
    await settle(0);
    expect(screen.queryByText(/ENG-420 \*\*|tracks it/)).toBeNull();
  });

  test.each([
    ["a short term", JANE, "cluster"],
    ["two words", JANE, "cluster joinflakes"],
    ["an anonymous reader", ANONYMOUS, QUESTION],
    // THE ANSWER RUNS ON THE SEAT'S OWN MODEL, so the engine refuses it to a
    // principal no seat binds — the one change an unbound principal may not
    // make — and nothing is sent for one.
    ["an unbound principal", UNBOUND, QUESTION],
  ])("is never asked for %s", async (_, viewer, term) => {
    vi.useFakeTimers();
    answering();
    const p = mount({ viewer });
    await p.type(term);
    await settle(ANSWER_IDLE_MS * 2);
    expect(posted).toEqual([]);
  });

  test("an anonymous or unbound reader is told what would let them ask", async () => {
    const p = mount({ viewer: UNBOUND });
    await p.type(QUESTION);
    expect(screen.getByText(/bind your login to a seat/)).toBeDefined();
    cleanup();
    const q = mount({ viewer: ANONYMOUS });
    await q.type(QUESTION);
    expect(
      screen.getByText(/Sign in to get an answer from your company’s knowledge/),
    ).toBeDefined();
  });

  // "ASKING" IS DRAWN FOR A QUESTION ACTUALLY SENT. A reader whose access is
  // still being read cannot ask, so no timer is armed for them — and when the
  // viewer answers, the pause starts then. Armed anyway, the timer marked the
  // question sent while the press was refused, and the moment access arrived
  // the card claimed to be reading pages nobody had been asked to read.
  test("a reader who could not ask yet waits for a pause of their own", async () => {
    vi.useFakeTimers();
    answering();
    const p = mount({ viewer: { ...JANE, loading: true } });
    await p.type(QUESTION);
    await settle(ANSWER_IDLE_MS * 2);
    expect(posted).toEqual([]);
    vi.mocked(useViewer).mockReturnValue(JANE);
    await p.type(`${QUESTION} `);
    expect(screen.queryByText("Reading your company’s pages and tasks…")).toBeNull();
    await settle(ANSWER_IDLE_MS);
    expect(posted.map((x) => x.tool)).toEqual(["answer_knowledge"]);
  });

  // THE THREE ACTIONS STAY IN THE FIRST SCREEN under an answer: two hits of
  // each kind, one line of chips, and every source a row UNDER the actions.
  test("under an answer, All keeps the actions ahead of the sources", async () => {
    vi.useFakeTimers();
    const sources = [
      { kind: "page", ref: "p1", title: "Runbook: node drain" },
      { kind: "page", ref: "p2", title: "ADR-0009" },
      { kind: "page", ref: "p3", title: "Q4 OKRs" },
      { kind: "page", ref: "p4", title: "Decision log" },
      { kind: "page", ref: "p5", title: "Incident 2026-09-14" },
      { kind: "task", ref: "ENG-420", title: "Flaky e2e" },
    ];
    engine((tool) =>
      json({ tool, outcome: "applied", position: "", receipt: { ...RECEIPT, sources } }),
    );
    const page = (id: string, title: string) => ({ id, title, container: "ENG", url: "" });
    const p = mount({
      answers: {
        work_search: async () => ({
          ...OUTCOME,
          hits: [HIT, { ...HIT, key: "ENG-421" }, { ...HIT, key: "ENG-422" }],
          available: true,
        }),
        knowledge: async () => ({
          ...OUTCOME,
          backend: "native",
          available: true,
          hits: [page("p1", "Runbook: node drain"), page("p2", "ADR-0009"), page("p3", "Q4 OKRs")],
        }),
      },
    });
    await p.type(QUESTION);
    await settle(ANSWER_IDLE_MS);
    await settle(0);
    const actions = screen.getByRole("group", { name: "Actions" });
    const sourceRows = screen.getByRole("group", { name: "Sources of the answer" });
    expect(
      actions.compareDocumentPosition(sourceRows) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
    expect(
      within(screen.getByRole("group", { name: "Tasks" })).getAllByRole("option"),
    ).toHaveLength(2);
    expect(
      within(screen.getByRole("group", { name: "Pages" })).getAllByRole("option"),
    ).toHaveLength(2);
    // What the hits already drew — two pages and ENG-420 — is not listed
    // again; the rest is, numbered as the answer cites it.
    const cited = within(sourceRows);
    expect(cited.getAllByRole("option").map((o) => o.textContent)).toEqual([
      "Q4 OKRs source 3 of the answer",
      "Decision log source 4 of the answer",
      "Incident 2026-09-14 source 5 of the answer",
    ]);
    expect(document.querySelectorAll(".palette-source")).toHaveLength(4);
    expect(document.querySelector(".palette-sources-more")!.textContent).toMatch(/^\+2/);
  });

  test("is not asked in the Tasks, Agents or Actions scopes", async () => {
    vi.useFakeTimers();
    answering();
    const p = mount();
    for (const sigil of ["#", "@", ">"]) {
      await p.type(`${sigil}${QUESTION}`);
      await settle(ANSWER_IDLE_MS);
    }
    expect(posted).toEqual([]);
  });
});

describe("the actions, made as the person", () => {
  const TASKS: Answers = {
    work_search: async () => ({ ...OUTCOME, hits: [HIT], available: true }),
    colleague: async (params) =>
      String(params.q).includes("swe")
        ? {
            match: { handle: "swe", why: "part of the name matches" },
            candidates: [{ handle: "swe", why: "part of the name matches" }],
          }
        : { match: null, candidates: [] },
    work_item: async () => ({ task: { ...HIT, version: 7 } }),
    work_projects: async () => ({
      projects: PROJECTS,
      total: 2,
      census: { active: 2, archived: 0 },
    }),
  };

  test("create files the term where the person's work lands, and opens it", async () => {
    engine((tool) =>
      json({
        tool,
        outcome: "applied",
        position: "CREWLET_TRACKER_LOG@1:5",
        receipt: { key: "ENG-431" },
      }),
    );
    const p = mount({ answers: TASKS });
    await p.type("rotate the join timeout");
    // THE PROJECT BY NAME, as the design says it; why this one is its title.
    const where = within(p.row(/Create task “rotate the join timeout”/)).getByText(
      "in ENG · Core platform",
    );
    expect(where.getAttribute("title")).toBe("ENG — where your own work lands");
    await act(async () => {
      fireEvent.click(p.row(/Create task/));
    });
    await act(async () => {});
    expect(posted.map((x) => [x.tool, x.body.args])).toEqual([
      ["create_work_item", { title: "rotate the join timeout", project: "ENG" }],
    ]);
    expect(location.hash).toBe("#/work/ENG-431");
  });

  test("create on a task page files into that task's project", async () => {
    const p = mount({ answers: TASKS, hash: "#/work/OPS-7" });
    await p.type("rotate the join timeout");
    const where = within(p.row(/Create task/)).getByText("in OPS · Operations");
    expect(where.getAttribute("title")).toBe("OPS — the project you are on");
  });

  // A NAME THE TERM NAMES EXACTLY ONCE is a suggestion; the person still
  // presses the row, and the ask goes to that seat as the asker.
  test("ask sends the question to the one agent the term names", async () => {
    engine((tool) =>
      json({
        tool,
        outcome: "applied",
        position: "CREWLET_TRACKER_LOG@1:6",
        receipt: { key: "ENG-432" },
      }),
    );
    const p = mount({ answers: TASKS });
    await p.type("swe cluster join");
    const ask = p.row(/Ask SWE about “swe cluster join”/);
    await act(async () => {
      fireEvent.click(ask);
    });
    await act(async () => {});
    expect(posted.map((x) => [x.tool, x.body.args])).toEqual([
      [
        "create_work_item",
        { title: "swe cluster join", assignee: "swe", ask: "swe", project: "ENG" },
      ],
    ]);
  });

  test("with no one named, ask opens a picker rather than guessing", async () => {
    engine((tool) =>
      json({
        tool,
        outcome: "applied",
        position: "CREWLET_TRACKER_LOG@1:6",
        receipt: { key: "ENG-433" },
      }),
    );
    const p = mount({ answers: TASKS });
    await p.type("cluster join");
    await act(async () => {
      fireEvent.click(p.row(/Ask an agent about “cluster join”…/));
    });
    expect(posted).toEqual([]);
    expect((p.input as HTMLInputElement).placeholder).toBe("Ask which agent?");
    await act(async () => {
      fireEvent.click(p.row("SWE"));
    });
    await act(async () => {});
    expect(posted[0]!.body.args).toMatchObject({ title: "cluster join", ask: "swe" });
  });

  test("assign picks an agent and is conditioned on the version the picker read", async () => {
    engine((tool) =>
      json({ tool, outcome: "applied", position: "CREWLET_TRACKER_LOG@1:8", receipt: {} }),
    );
    const p = mount({ answers: TASKS });
    await p.type("swe cluster join");
    const assign = p.row(/Assign ENG-420 to an agent…/);
    expect(assign.textContent).toContain("suggested: SWE — part of the name matches");
    await act(async () => {
      fireEvent.click(assign);
    });
    await act(async () => {});
    expect(screen.getByText("Suggested")).toBeDefined();
    await act(async () => {
      fireEvent.click(within(screen.getByRole("group", { name: "Suggested" })).getByText("SWE"));
    });
    await act(async () => {});
    expect(posted.map((x) => [x.tool, x.body.args])).toEqual([
      ["update_work_item", { item: "ENG-420", assignee: "swe", if_match: 7 }],
    ]);
  });

  // Alt+A with the term typed opens the assign picker — the row's own key.
  test("the rows' keys reach them from the field", async () => {
    const p = mount({ answers: TASKS });
    await p.type("cluster join");
    await p.press("a", { code: "KeyA", altKey: true });
    expect((p.input as HTMLInputElement).placeholder).toBe("Assign ENG-420 to…");
    // Backspace on the empty picker goes back to the results.
    await p.press("Backspace");
    expect((p.input as HTMLInputElement).placeholder).toMatch(/^Search/);
  });

  test("a refusal stays in the palette, in the engine's words", async () => {
    engine((tool) => json({ error: "budget_exhausted", tool, detail: "no room" }, 409));
    const p = mount({ answers: TASKS });
    await p.type("rotate the join timeout");
    await act(async () => {
      fireEvent.click(p.row(/Create task/));
    });
    await act(async () => {});
    expect(p.closed).not.toHaveBeenCalled();
    expect(document.querySelector(".is-refused")).not.toBeNull();
  });

  // A WRITE ROW IS NEVER HIDDEN: it is drawn for every reader, and where
  // this browser cannot act its hint is the sentence that says why — and a
  // press on it says so again instead of doing anything.
  test.each([
    ["an anonymous reader", ANONYMOUS, WRITE_REASONS.anonymous],
    [
      "a person the engine does not make it for",
      { ...JANE, acts: ["answer_knowledge"] },
      WRITE_REASONS.not_served,
    ],
  ])("for %s each is drawn, disabled with the reason", async (_, viewer, reason) => {
    engine((tool) => json({ tool, outcome: "applied", position: "", receipt: {} }));
    const p = mount({ viewer, answers: TASKS });
    await p.type("cluster join");
    for (const label of [/Assign ENG-420/, /Ask an agent/, /Create task/]) {
      expect(p.row(label).textContent).toContain(reason);
    }
    await act(async () => {
      fireEvent.click(p.row(/Create task/));
    });
    expect(posted).toEqual([]);
    expect(p.closed).not.toHaveBeenCalled();
  });

  // NO DEFAULT PROJECT IS NOT A REASON TO REFUSE: the person chooses where,
  // and the project the top task hit is filed in is offered first — offered,
  // never taken for them. This was a disabled row on every screen that is not
  // a project's, for the founder the seeded company is built around.
  test("a person with no default project chooses where, the top hit's first", async () => {
    engine((tool) => json({ tool, outcome: "applied", position: "", receipt: { key: "OPS-12" } }));
    const p = mount({ viewer: { ...JANE, project: "" }, answers: TASKS });
    await p.type("cluster join");
    const create = p.row(/Create task/);
    expect(create.textContent).toContain("choose a project");
    await act(async () => {
      fireEvent.click(create);
    });
    expect(posted).toEqual([]);
    expect((p.input as HTMLInputElement).placeholder).toBe("File it in which project?");
    const suggested = screen.getByRole("group", { name: "Suggested" });
    expect(suggested.textContent).toContain("where ENG-420 is filed");
    await act(async () => {
      fireEvent.click(
        within(screen.getByRole("group", { name: "Projects" })).getByText("Operations"),
      );
    });
    await act(async () => {});
    expect(posted.map((x) => [x.tool, x.body.args])).toEqual([
      ["create_work_item", { title: "cluster join", project: "OPS" }],
    ]);
  });

  test("an ask with no default project steps through the agent, then the project", async () => {
    engine((tool) => json({ tool, outcome: "applied", position: "", receipt: { key: "ENG-9" } }));
    const p = mount({ viewer: { ...JANE, project: "" }, answers: TASKS });
    await p.type("cluster join");
    await act(async () => {
      fireEvent.click(p.row(/Ask an agent about/));
    });
    await act(async () => {
      fireEvent.click(p.row("SWE"));
    });
    expect(posted).toEqual([]);
    expect((p.input as HTMLInputElement).placeholder).toBe("File it in which project?");
    await act(async () => {
      fireEvent.click(
        within(screen.getByRole("group", { name: "Suggested" })).getByText("Core platform"),
      );
    });
    await act(async () => {});
    expect(posted.map((x) => x.body.args)).toEqual([
      { title: "cluster join", assignee: "swe", ask: "swe", project: "ENG" },
    ]);
  });

  test("a company with no project at all is the one reason a create is refused", async () => {
    const p = mount({
      viewer: { ...JANE, project: "" },
      answers: {
        ...TASKS,
        work_projects: async () => ({ projects: [], total: 0, census: { active: 0, archived: 0 } }),
      },
    });
    await p.type("cluster join");
    expect(p.row(/Create task/).textContent).toContain("no project yet");
  });

  // WHO HOLDS IT NOW is said on the assign step, from the picker's own read.
  test("the assign step marks the agent that holds the task", async () => {
    const p = mount({
      answers: {
        ...TASKS,
        work_item: async () => ({ task: { ...HIT, assignee: "swe", version: 7 } }),
      },
    });
    await p.type("cluster join");
    await p.press("a", { code: "KeyA", altKey: true });
    await act(async () => {});
    expect(p.row("SWE").textContent).toContain("holds it");
    expect(p.row("SRE").textContent).not.toContain("holds it");
  });
});

// A PRESS THAT COULD NOT ACT is said UNDER the answer, never in its place:
// the answer is paid for, and a row this reader cannot use is no reason to
// take it off the screen.
describe("a notice beside the answer", () => {
  test("a blocked row's reason is drawn under an answer already on screen", async () => {
    vi.useFakeTimers();
    engine((tool) =>
      json({
        tool,
        outcome: "applied",
        position: "",
        receipt: {
          answer_md: "It flakes under packet loss.",
          sources: [],
          tokens: { input: 10, output: 5 },
          model: "m",
          cached: false,
        },
      }),
    );
    const p = mount({ viewer: { ...JANE, acts: ["answer_knowledge"] } });
    await p.type("why does cluster join flake");
    await act(async () => {
      vi.advanceTimersByTime(ANSWER_IDLE_MS);
    });
    await act(async () => {});
    await p.press("Enter", { ctrlKey: true });
    expect(screen.getByText("It flakes under packet loss.")).toBeDefined();
    expect(screen.getByText(WRITE_REASONS.not_served, { selector: "p" })).toBeDefined();
  });
});

describe("the colleague question", () => {
  // THE ENGINE REFUSES A NAME PAST ITS CAP, and that refusal would only drop
  // its name tiers from the list with nothing saying why — so a longer term,
  // a pasted log line, is not sent, and the chart's own matching answers.
  test("a term past the engine's cap is not asked", async () => {
    const p = mount();
    await p.type(`@${"x".repeat(COLLEAGUE_QUERY_MAX + 1)}`);
    await p.type("@swe");
    const names = asked.filter((a) => a.kind === "colleague").map((a) => a.params.q);
    expect(names).toEqual(["swe"]);
    // BYTES, as the engine counts, not characters: 67 three-byte runes are
    // 201 bytes in a string JavaScript calls 67 long.
    await p.type(`@${"語".repeat(67)}`);
    expect(asked.filter((a) => a.kind === "colleague")).toHaveLength(1);
  });
});

describe("closing", () => {
  test("the chord that opened it closes it, from its own field", async () => {
    const p = mount();
    await p.press("k", { ctrlKey: true });
    expect(p.closed).toHaveBeenCalledTimes(1);
  });

  test("a command runs and closes the palette", async () => {
    const p = mount();
    await p.type(">keyboard");
    await p.press("Enter");
    expect(p.keys).toHaveBeenCalledTimes(1);
    expect(p.closed).toHaveBeenCalledTimes(1);
  });
});
