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
import { ToastProvider } from "@crewlethq/ui";

import { CommandPalette } from "./Palette.tsx";
import { AssistantControlContext, type AssistantControl } from "../assistant/control.ts";
import { ProjectSwitcherProvider } from "../header/ProjectSwitcher.tsx";
import { forgetAnswersForTest } from "./answer.ts";
import { ANSWER_IDLE_MS, destinationHits } from "./hits.ts";
import { ACT_ERRORS } from "~/contract/errors.ts";
import { COLLEAGUE_QUERY_MAX, SEARCH_QUERY_MAX } from "~/contract/wire.ts";
import { Router, href } from "../router.tsx";
import { DESTINATIONS } from "../nav.ts";
import { TOUCH_MEDIA } from "../layout.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { useViewer, type ViewerState } from "~/lib/viewer.ts";
import { remember } from "~/lib/recents.ts";
import { noteReader } from "~/lib/reader.ts";
import { reloadForTest } from "~/lib/prefs.ts";
import { STORAGE_KEYS } from "~/lib/storage.ts";
import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { withDerived } from "~/test/org.ts";
import { ZERO_VERSIONS } from "~/test/liveCall.ts";

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
  grants: ["admin", "worklet", "doclet"],
  admin: true,
  worklet: true,
  doclet: true,
  chatlet: true,
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
  configWriter: true,
  configManagedBy: [],
  assistant: "disabled",
  assistantModel: null,
  chatVendors: [],
};

const ANONYMOUS: ViewerState = {
  ...JANE,
  login: "",
  grants: [],
  admin: false,
  worklet: false,
  doclet: false,
  chatlet: false,
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
  grants: ["worklet"],
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
      id: "platform",
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
  assistant,
}: {
  viewer?: ViewerState;
  answers?: Answers;
  hash?: string;
  agents?: Record<string, unknown>[];
  /** The frame's hold on the assistant; absent, the frame offers none. */
  assistant?: AssistantControl;
} = {}) {
  location.hash = hash;
  vi.mocked(useViewer).mockReturnValue(viewer);
  const store = new Store();
  store.setConnected(true);
  store.applyOrg(withDerived(ORG) as never);
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
  // NO `LayerHost`, as the application mounts none: every overlay is
  // portalled to the body, in the one stacking context the layer stack orders
  // (`app/App.tsx`).
  render(
    <ToastProvider>
      <ClientContext.Provider value={{ store, socket }}>
        <Router>
          {/* THE FRAME'S SWITCHER, which "Switch project…" opens. */}
          <ProjectSwitcherProvider>
            {assistant ? (
              <AssistantControlContext.Provider value={assistant}>
                <CommandPalette onClose={closed} onShowKeys={keys} />
              </AssistantControlContext.Provider>
            ) : (
              <CommandPalette onClose={closed} onShowKeys={keys} />
            )}
          </ProjectSwitcherProvider>
        </Router>
      </ClientContext.Provider>
    </ToastProvider>,
  );
  const input = screen.getByRole("combobox");
  return {
    store,
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

// THE PALETTE IS ON THE PAGE'S ONE LAYER STACK, portalled to the body rather
// than into a host of its own: a stacking context around it would cap it
// beneath the narrow layout's open drawer, which paints in the root context,
// and the palette opened from the drawer's search drew under the drawer.
test("the palette is portalled to the body, under no layer host", () => {
  mount();
  const dialog = screen.getByRole("dialog", { name: "Search" });
  expect(dialog.closest(".crewlet-layer-host")).toBeNull();
  expect(dialog.closest(".crewlet-modal-overlay")?.parentElement).toBe(document.body);
});

// THE PALETTE'S KEYCAPS ARE FOR A READER WITH KEYS: on a phone its footer's
// legend and its rows' chords named keys nobody there has. Mutation: draw them
// whatever the input and the touch case goes red; drop them everywhere and the
// keyboard case does.
test.each([
  [true, 0],
  [false, 1],
])("on a touch device (%s) the palette's key legend is drawn %d times", async (on, legends) => {
  const had = Object.getOwnPropertyDescriptor(globalThis, "matchMedia");
  Object.defineProperty(globalThis, "matchMedia", {
    configurable: true,
    writable: true,
    value: (query: string) => ({
      matches: on && query === TOUCH_MEDIA,
      media: query,
      onchange: null,
      addEventListener: () => {},
      removeEventListener: () => {},
      addListener: () => {},
      removeListener: () => {},
      dispatchEvent: () => false,
    }),
  });
  try {
    const p = mount();
    await p.type("budget");
    expect(document.querySelectorAll(".palette-keys")).toHaveLength(legends);
    const create = p.row(/Create task/);
    expect(create.querySelectorAll(".crewlet-kbd-combo")).toHaveLength(legends);
  } finally {
    if (had) Object.defineProperty(globalThis, "matchMedia", had);
  }
});

describe("scopes and sigils", () => {
  test("the six scopes are tabs, All first", () => {
    mount();
    expect(screen.getAllByRole("tab").map((t) => t.textContent)).toEqual([
      "All",
      "Tasks",
      "Pages",
      "Artifacts",
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

// A DESTINATION SAYS WHAT OPENS IT BEFORE A PRESS, read from the navigation's
// own table: Chatlet is the Chatlet app's, so a reader holding Worklet and
// Doclet but not Chatlet is told which access to ask for — and a reader
// holding it is told nothing. Mutation: drop the row's `grants` and the first
// case goes red; lock on the table's grants whatever the reader holds and the
// second does.
describe("a destination's lock", () => {
  const WITHOUT_CHATLET: ViewerState = {
    ...JANE,
    grants: ["worklet", "doclet"],
    admin: false,
    chatlet: false,
  };

  test.each([
    ["without Chatlet", WITHOUT_CHATLET, true],
    [
      "holding Chatlet",
      { ...WITHOUT_CHATLET, grants: ["worklet", "doclet", "chatlet"], chatlet: true },
      false,
    ],
  ])("Chatlet's lock, to a reader %s", async (_, viewer, locked) => {
    const p = mount({ viewer });
    await p.type("chat");
    expect(/· needs Chatlet access$/.test(p.row(/^Chatlet$/).textContent ?? "")).toBe(locked);
  });

  // A SECTION IS LOCKED BY ITS WORKSPACE FIRST, as the frame refuses it: a
  // reader who cannot open Chatlet cannot open its Mentions, and one who cannot
  // open Doclet is told Doclet before Agent diaries' own Admin. Read
  // off a section's own list, every section of a workspace that names the
  // app — Worklet's, My work's, Chatlet's — was offered with no lock at all, and
  // the press opened onto the refusal. Mutation: lock by the section's own
  // grants alone and the Mentions and Asked of me rows go red.
  test.each([
    { term: "mentions", label: /^Mentions$/, viewer: WITHOUT_CHATLET, needs: "Chatlet" },
    { term: "search chat", label: /^Search chat$/, viewer: WITHOUT_CHATLET, needs: "Chatlet" },
    {
      term: "asked of me",
      label: /^Asked of me$/,
      viewer: { ...WITHOUT_CHATLET, grants: ["doclet"] },
      needs: "Worklet",
    },
    {
      term: "diaries",
      label: /^Agent diaries$/,
      viewer: { ...WITHOUT_CHATLET, grants: ["worklet"] },
      needs: "Doclet",
    },
    { term: "diaries", label: /^Agent diaries$/, viewer: WITHOUT_CHATLET, needs: "Admin" },
  ])("$term is locked naming $needs access where the reader lacks it", async (c) => {
    const p = mount({ viewer: c.viewer });
    await p.type(c.term);
    expect(p.row(c.label).textContent).toMatch(new RegExp(`· needs ${c.needs} access$`));
  });

  test("a section whose workspace and own access the reader holds is not locked", async () => {
    const p = mount({ viewer: { ...WITHOUT_CHATLET, grants: ["chatlet"], chatlet: true } });
    await p.type("mentions");
    expect(p.row(/^Mentions$/).textContent).not.toMatch(/needs/);
  });
});

// THE TURNS RUNNING AS IT OPENED SIT UNDER RECENT IN AN EMPTY PALETTE, each
// going to the turn's watch link — its Transcript, the phase it is on open.
// Oldest first, as every list of running turns is; a seat waiting on a person,
// or one whose turn has no id yet, is not offered as a way to a running turn.
describe("running now", () => {
  // RECENTS ARE A READER'S (`lib/recents.ts`): a place visited before the tab
  // knows who reads it lands in no list, so the reader is named first.
  beforeEach(() => noteReader("p-1"));

  const working = (role: string, handle: string, turn: string, minute: number, item = "") => ({
    id: handle,
    agent_id: `a-${handle}`,
    role,
    handle,
    activity: "working",
    turn: {
      turn_id: turn,
      started_at: `2026-09-21T10:0${minute}:00Z`,
      stage: "phase",
      ...(item ? { work_item: { backend: "native", id: item, key: item, project: "ENG" } } : {}),
    },
    live_call: {
      turn_id: turn,
      phase: "execute",
      round_num: 0,
      rounds_used: 1,
      max_rounds: 25,
      versions: ZERO_VERSIONS,
    },
  });
  const agents = [
    working("SRE", "sre", "t-late", 9),
    working("SWE", "swe", "t-early", 1, "ENG-412"),
    { id: "jane", agent_id: "a-jane", role: "Jane Founder", handle: "jane", activity: "needs" },
    { ...working("SWE 2", "swe-2", "", 3), turn: null, live_call: null },
  ];

  /** The group a heading names, by its rendered label. */
  const group = (label: string) => screen.getByText(label).closest('[role="group"]') as HTMLElement;
  /** The option the listbox holds active — the one Enter commits. */
  const highlighted = () => {
    const id = screen.getByRole("combobox").getAttribute("aria-activedescendant") ?? "";
    return document.getElementById(id)?.textContent ?? "";
  };

  test("sits under Recent in the empty All scope, oldest first, saying what each is doing", () => {
    remember({ path: ["inbox"], label: "Inbox", workspace: "Inbox" }, true);
    mount({ agents });
    // UNDER RECENT, ahead of Go to: the first row is still where the reader was.
    expect(screen.getAllByRole("option")[0]?.textContent).toContain("Inbox");
    const rows = within(group("Running now")).getAllByRole("option");
    expect(rows).toHaveLength(2);
    expect(rows[0]?.textContent).toContain("SWE");
    expect(rows[0]?.textContent).toContain("Executing ENG-412");
    expect(rows[1]?.textContent).toContain("SRE");
    const order = (a: Node, b: Node) =>
      Boolean(a.compareDocumentPosition(b) & Node.DOCUMENT_POSITION_FOLLOWING);
    expect(order(group("Recent"), group("Running now")), "Recent comes first").toBe(true);
    expect(order(group("Running now"), group("Go to")), "Go to follows it").toBe(true);
  });

  // "⌘K, ENTER" REOPENS WHERE THE READER WAS, whether or not the company is
  // working — a group above Recent made that habit open a running turn.
  test("Enter on an empty palette still reopens the most recent place while turns run", async () => {
    remember({ path: ["inbox"], label: "Inbox", workspace: "Inbox" }, true);
    const p = mount({ agents });
    await p.press("Enter");
    expect(location.hash).toBe("#/inbox");
  });

  // THE SET IS READ AS THE PALETTE OPENS. The kit holds the highlight by
  // index, so a turn that ended while the palette was open took its row out
  // from under every row below it, and Enter opened the row after the one
  // highlighted. The finished turn keeps its row and says so.
  test("a turn ending while it is open moves no row, and Enter opens the one highlighted", async () => {
    const p = mount({ agents });
    // PAST BOTH RUNNING ROWS, onto a Go to row whose index the group's size decides.
    for (let i = 0; i < 3; i++) await p.press("ArrowDown");
    const before = highlighted();
    expect(before, "the highlight is on a row under the group").not.toContain("SRE");
    act(() =>
      p.store.applyAgents([
        { agent_id: "a-swe", activity: "idle", turn: null, live_call: null },
        agents[0]!,
      ] as never),
    );
    expect(highlighted()).toBe(before);
    const rows = within(group("Running now")).getAllByRole("option");
    expect(rows).toHaveLength(2);
    expect(rows[0]?.textContent).toContain("Finished");
    // THE SCREEN THE HIGHLIGHTED ROW NAMES — the longest label it starts
    // with, so a label that prefixes another cannot answer for it.
    const dest = DESTINATIONS.filter((d) => before.startsWith(d.label)).sort(
      (a, b) => b.label.length - a.label.length,
    )[0];
    expect(dest, `no destination is labelled like "${before}"`).toBeTruthy();
    await p.press("Enter");
    expect(location.hash).toBe(href(dest!.path));
  });

  // AND A TURN THAT STARTS while it is open is not offered until the next ⌘K —
  // on a seat the roster gains meanwhile, which is how a new seat arrives.
  test("a turn starting while it is open adds no row", () => {
    const p = mount({ agents });
    act(() => p.store.applySeats([...agents, working("CTO", "cto", "t-new", 5)] as never));
    expect(within(group("Running now")).getAllByRole("option")).toHaveLength(2);
  });

  test("a row goes to the turn's watch link", () => {
    const p = mount({ agents });
    const running = screen.getByText("Running now").closest('[role="group"]') as HTMLElement;
    fireEvent.click(within(running).getAllByRole("option")[0]!);
    expect(location.hash).toBe("#/live/turns/t-early?tab=transcript");
    expect(p.closed).toHaveBeenCalled();
  });

  test("is not drawn once something is typed, or while nothing runs", async () => {
    const p = mount({ agents });
    await p.type("deploy");
    expect(screen.queryByText("Running now")).toBeNull();
    cleanup();
    mount({
      agents: [{ id: "swe", agent_id: "a-swe", role: "SWE", handle: "swe", activity: "idle" }],
    });
    expect(screen.queryByText("Running now")).toBeNull();
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
    fireEvent.click(p.row("as a trace"));
    expect(location.hash).toBe(`#/live/traces/${id}`);
  });
});

test("a term typed fast keeps every keystroke, and the searches ask its last spelling", async () => {
  const p = mount({
    answers: {
      work_search: async () => ({ ...OUTCOME, hits: [HIT], available: true }),
      knowledge: async () => ({ ...OUTCOME, hits: [], available: true }),
      colleague: async () => ({ match: null, candidates: [] }),
    },
  });
  const input = p.input as HTMLInputElement;
  // KEYSTROKES AS A BROWSER DELIVERS THEM WHEN THEY QUEUE UP: one native
  // input event after another, each flushed by React on its own, outside
  // `act`. A search asked with the term as typed set state from its effect
  // after every keystroke, and React counted those towards its nested-update
  // limit until it threw a keystroke away (error #185). `act` drains every
  // update between keystrokes and would hide that.
  const environment = globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean | undefined };
  const wasActEnvironment = environment.IS_REACT_ACT_ENVIRONMENT;
  const errors: unknown[] = [];
  const onError = (event: ErrorEvent) => errors.push(event.error ?? event.message);
  const logged = vi.spyOn(console, "error").mockImplementation((...args) => errors.push(args));
  const setValue = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!.set!;
  const typed = "cluster join under packet loss and the flaky sign in test ".repeat(4).trim();
  window.addEventListener("error", onError);
  environment.IS_REACT_ACT_ENVIRONMENT = false;
  try {
    for (const key of typed) {
      setValue.call(input, input.value + key);
      input.dispatchEvent(new Event("input", { bubbles: true }));
    }
  } finally {
    environment.IS_REACT_ACT_ENVIRONMENT = wasActEnvironment;
    window.removeEventListener("error", onError);
    logged.mockRestore();
  }
  await act(async () => {});

  expect(errors).toEqual([]);
  expect(input.value).toBe(typed);
  expect(asked.filter((a) => a.kind === "work_search").at(-1)?.params).toMatchObject({ q: typed });
});

describe("the task search has four answers, kept apart", () => {
  const answering = (reply: (q: string) => Promise<unknown>): Answers => ({
    work_search: (params) => reply(String(params.q)),
  });

  test("too short is not a search", async () => {
    const p = mount();
    await p.type("#a");
    expect(screen.getByText("Type two or more characters.")).toBeDefined();
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
    expect(screen.getByText("Not available on this node.")).toBeDefined();
    expect(screen.queryByText(/No task matches/)).toBeNull();
  });

  test("a building index is not an empty company", async () => {
    const p = mount({
      answers: answering(async () => ({ ...OUTCOME, hits: [], available: false })),
    });
    await p.type("#auth");
    expect(screen.getByText("Still indexing. Try again shortly.")).toBeDefined();
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

  // A REFUSAL IS TOLD IN THE ENGINE'S OWN WORDS where it has them: the act
  // route answers a refusal's `detail` only in a person's words (ADR-0060),
  // and a spent budget's says whose ceiling it was and that nothing was sent,
  // which the dashboard's one sentence for the class cannot.
  test("a refused question is told in the engine's words", async () => {
    vi.useFakeTimers();
    const said =
      "The company's token budget for this window is spent, so nothing was sent. It comes back when the window resets, or as soon as the ceiling is raised.";
    engine((tool) =>
      tool === "answer_knowledge"
        ? json(
            {
              error: "budget_exhausted",
              message:
                "The token budget for this window is spent, so nothing was spent on this. Raise it, or wait for the window to turn over.",
              detail: said,
              remedy: { setting: "company_token_budget" },
              tool,
              op_id: "op-1",
            },
            409,
          )
        : json({ error: "not_found", detail: "no" }, 404),
    );
    const p = mount();
    await p.type(QUESTION);
    await settle(ANSWER_IDLE_MS);
    await settle(0);
    expect(screen.getByText(said)).toBeDefined();
    expect(screen.queryByText(ACT_ERRORS.budget_exhausted)).toBeNull();
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
    expect(
      screen.getByText(
        "Asking needs a seat. Ask whoever manages people here to bind your login to one.",
      ),
    ).toBeDefined();
    cleanup();
    const q = mount({ viewer: ANONYMOUS });
    await q.type(QUESTION);
    expect(screen.getByText("Sign in to ask.")).toBeDefined();
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
        receipt: { item: "ENG-431", key: "ENG-431" },
      }),
    );
    const p = mount({ answers: TASKS });
    await p.type("rotate the join timeout");
    // THE PROJECT BY NAME, as the design says it; why this one is its title.
    const where = within(p.row(/Create task “rotate the join timeout”/)).getByText(
      "in ENG · Core platform",
    );
    expect(where.getAttribute("title")).toBe("ENG: where your own work lands");
    await act(async () => {
      fireEvent.click(p.row(/Create task/));
    });
    await act(async () => {});
    expect(posted.map((x) => [x.tool, x.body.args])).toEqual([
      ["create_work_item", { title: "rotate the join timeout", project: "ENG" }],
    ]);
    expect(location.hash).toBe("#/worklet/ENG-431");
  });

  test("create on a task page files into that task's project", async () => {
    const p = mount({ answers: TASKS, hash: "#/worklet/OPS-7" });
    await p.type("rotate the join timeout");
    const where = within(p.row(/Create task/)).getByText("in OPS · Operations");
    expect(where.getAttribute("title")).toBe("OPS: the project you are on");
  });

  // A NAME THE TERM NAMES EXACTLY ONCE is a suggestion; the person still
  // presses the row, and the ask goes to that seat as the asker.
  test("ask sends the question to the one agent the term names", async () => {
    engine((tool) =>
      json({
        tool,
        outcome: "applied",
        position: "CREWLET_TRACKER_LOG@1:6",
        receipt: { item: "ENG-432", key: "ENG-432" },
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
        receipt: { item: "ENG-433", key: "ENG-433" },
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
    expect(assign.textContent).toContain("suggested: SWE, part of the name matches");
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
  // press on it says so again instead of doing anything. (A reader holding no
  // Worklet is sent no task search, so no task is at hand to assign.)
  test.each([
    ["an anonymous reader", ANONYMOUS, WRITE_REASONS.anonymous, [/Ask an agent/, /Create task/]],
    [
      "a person the engine does not make it for",
      { ...JANE, acts: ["answer_knowledge"] },
      WRITE_REASONS.not_served,
      [/Assign ENG-420/, /Ask an agent/, /Create task/],
    ],
  ])("for %s each is drawn, disabled with the reason", async (_, viewer, reason, labels) => {
    engine((tool) => json({ tool, outcome: "applied", position: "", receipt: {} }));
    const p = mount({ viewer, answers: TASKS });
    await p.type("cluster join");
    for (const label of labels) {
      expect(p.row(label).textContent).toContain(reason);
    }
    await act(async () => {
      fireEvent.click(p.row(/Create task/));
    });
    expect(posted).toEqual([]);
    expect(p.closed).not.toHaveBeenCalled();
  });

  // A BROWSER THE ENGINE REFUSED IS OFFERED NONE OF THE COMPANY. A person
  // whose seat was taken out of the chart kept a palette listing the agents
  // and tools of the snapshot they held, and was offered Ask and Create held
  // as "Offline" — online, and refused. The CONTROL is the same
  // palette before the refusal, which lists the seat and asks its questions.
  // Mutation: keep the snapshot on a refusal, ask regardless, or rank offline
  // first in the gate, and a line here goes red.
  test("a refused browser asks nothing, lists no seat or tool, and says why it cannot write", async () => {
    const p = mount({ answers: TASKS });
    act(() => p.store.applyTools([{ name: "task_activity", source: "mcp:tracker" }] as never));
    await p.type("swe");
    expect(screen.getAllByText("SWE").length).toBeGreaterThan(0);
    await p.type("task");
    expect(screen.getAllByText("task_activity").length).toBeGreaterThan(0);
    expect(asked.some((q) => q.kind === "work_search")).toBe(true);

    act(() => p.store.setAccessRefused("your seat is no longer in the chart"));
    asked = [];
    await p.type("swe");
    expect(screen.queryByText("SWE")).toBeNull();
    await p.type("task");
    expect(screen.queryByText("task_activity")).toBeNull();
    expect(asked).toEqual([]);
    for (const label of [/Ask an agent/, /Create task/]) {
      expect(p.row(label).textContent).toContain(WRITE_REASONS.refused);
      expect(p.row(label).textContent).not.toContain(WRITE_REASONS.offline);
    }
  });

  // NO DEFAULT PROJECT IS NOT A REASON TO REFUSE: the person chooses where,
  // and the project the top task hit is filed in is offered first — offered,
  // never taken for them. This was a disabled row on every screen that is not
  // a project's, for the founder the seeded company is built around.
  test("a person with no default project chooses where, the top hit's first", async () => {
    engine((tool) =>
      json({ tool, outcome: "applied", position: "", receipt: { item: "OPS-12", key: "OPS-12" } }),
    );
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
    engine((tool) =>
      json({ tool, outcome: "applied", position: "", receipt: { item: "ENG-9", key: "ENG-9" } }),
    );
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
    expect(p.row(/Create task/).textContent).toContain("No projects yet. Add one in Worklet.");
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

describe("the search bound", () => {
  // THE ENGINE REFUSES A PHRASE PAST ITS SEARCH BOUND rather than cutting it,
  // so a pasted passage asks neither search nor the answer — the list would
  // lose its tasks and pages to two refusals and the answer card to a third,
  // with nothing saying why — and the lead says why instead.
  test("a term past it asks nothing and says why", async () => {
    vi.useFakeTimers();
    engine(() => json({ error: "not_found", detail: "no" }, 404));
    const p = mount();
    const searches = () => asked.filter((a) => a.kind === "work_search" || a.kind === "knowledge");
    const pasted = "why does the deploy keep failing ".repeat(15).trim();
    await p.type(pasted);
    await act(async () => {
      vi.advanceTimersByTime(ANSWER_IDLE_MS * 2);
    });
    await act(async () => {});
    expect(searches()).toEqual([]);
    expect(posted).toEqual([]);
    expect(
      screen.getByText(
        `This is ${pasted.length} bytes, and a search takes at most ${SEARCH_QUERY_MAX}. Search on a few keywords or a phrase, not a pasted passage.`,
      ),
    ).toBeDefined();
    // BYTES, as the engine counts, not characters.
    await p.type("語".repeat(134));
    expect(searches()).toEqual([]);
    // AT the bound it is a search, of both.
    await p.type("x".repeat(SEARCH_QUERY_MAX));
    expect(searches().map((a) => a.kind)).toEqual(["work_search", "knowledge"]);
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

/*
 * CHANGE THEME IS A STEP OF THE PALETTE, the kit's own: a list of the themes
 * in place of the results, whose highlight previews behind the palette and
 * whose Enter applies. A preview is never written, and leaving the step puts
 * back the theme in effect.
 */
describe("the theme step", () => {
  const root = () => document.documentElement;

  beforeEach(() => reloadForTest());
  afterEach(() => {
    root().removeAttribute("data-palette");
    localStorage.clear();
    reloadForTest();
  });

  async function intoStep() {
    const p = mount();
    await p.type(">");
    await act(async () => {
      fireEvent.click(p.row("Change theme…"));
    });
    return p;
  }

  const labels = () =>
    screen
      .getAllByRole("option")
      .map((o) => o.querySelector(".crewlet-palette__label")?.textContent);

  test("Actions offers Change theme with the theme in effect, and the modes not in use", async () => {
    const p = mount();
    await p.type(">");
    expect(p.row("Change theme…").textContent).toContain("Crewlet is on");
    // The mode in effect is the system's, so it is not offered again.
    expect(screen.queryByText("Mode: match system")).toBeNull();
    expect(screen.getByText("Mode: dark")).toBeTruthy();
    expect(screen.queryByText(/^Theme: /)).toBeNull();
  });

  test("it opens on the theme in effect, keeps the palette open and draws no scopes", async () => {
    const p = await intoStep();
    expect(p.closed).not.toHaveBeenCalled();
    expect(screen.getByRole("combobox", { name: "Change theme" })).toBe(p.input);
    expect(screen.getByRole("button", { name: "Back from Change theme" })).toBeTruthy();
    expect(screen.queryAllByRole("tab")).toEqual([]);
    const active = document.getElementById(p.input.getAttribute("aria-activedescendant")!);
    expect(active?.textContent).toContain("Crewlet");
    expect(root().hasAttribute("data-palette")).toBe(false);
    // Every theme, under its group.
    expect(screen.getByRole("group", { name: "High contrast" })).toBeTruthy();
    expect(labels()).toHaveLength(10);
  });

  test("the highlight previews, a query narrows by group, and Enter applies and closes with Undo", async () => {
    const p = await intoStep();
    await p.press("ArrowDown");
    expect(root().getAttribute("data-palette")).toBe("violet");
    expect(localStorage.getItem(STORAGE_KEYS.palette)).toBeNull();
    // A group's name finds its themes, and a tag finds its own: Origin's is
    // "Purple on charcoal". The best match leads, and is previewed.
    await p.type("purple");
    expect(labels()).toEqual(["Origin", "Abyss", "Iris", "Dusk"]);
    expect(root().getAttribute("data-palette")).toBe("origin");
    await p.type("abyss");
    expect(labels()).toEqual(["Abyss"]);
    expect(root().getAttribute("data-palette")).toBe("abyss");
    await p.press("Enter");
    expect(localStorage.getItem(STORAGE_KEYS.palette)).toBe("abyss");
    expect(root().getAttribute("data-palette")).toBe("abyss");
    expect(p.closed).toHaveBeenCalledOnce();
    expect(screen.getAllByText("Theme set to Abyss").length).toBeGreaterThan(0);
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Undo" }));
    });
    expect(root().hasAttribute("data-palette")).toBe(false);
    expect(localStorage.getItem(STORAGE_KEYS.palette)).toBeNull();
  });

  test("Escape goes back to Actions and puts back the theme in effect", async () => {
    const p = await intoStep();
    await p.press("ArrowDown");
    expect(root().getAttribute("data-palette")).toBe("violet");
    await p.press("Escape");
    expect(p.closed).not.toHaveBeenCalled();
    expect(root().hasAttribute("data-palette")).toBe(false);
    expect(p.tab()).toBe("Actions");
    expect(screen.getByText("Change theme…")).toBeTruthy();
  });

  test("a query that matches no theme says so", async () => {
    const p = await intoStep();
    await p.type("zzz");
    expect(screen.getByText("No theme matches.")).toBeTruthy();
    expect(root().hasAttribute("data-palette")).toBe(false);
  });
});

// THE COMPANY'S PROJECTS ARE A GROUP IN ALL, ranked as the switcher ranks
// them, and the switcher itself is a row, Switch project…, which opens the
// panel over wherever the palette was. And the company's work has its full
// ranked search one row away.
describe("projects, the switcher and the full search", () => {
  const listing = async () => ({
    projects: [
      {
        key: "ENG",
        name: "Core platform",
        unit: { name: "Engineering" },
        task_counts: { todo: 2, active: 1, done: 0, closed: 0 },
        version: 1,
      },
      {
        key: "OPS",
        name: "Operations",
        unit: { name: "Platform" },
        task_counts: { todo: 0, active: 0, done: 4, closed: 0 },
        version: 1,
      },
    ],
    total: 2,
    census: { active: 2, archived: 0 },
    complete: true,
  });

  test("a term ranks the projects as the switcher does, each going to its page", async () => {
    const p = mount({ answers: { work_projects: listing } });
    await act(async () => Promise.resolve());
    await p.type("o");
    const group = screen.getByRole("group", { name: "Projects" });
    const rows = within(group).getAllByRole("option");
    // A KEY THAT STARTS WITH IT before a name that merely contains it.
    expect(rows.map((r) => r.querySelector(".crewlet-key-chip")?.textContent)).toEqual([
      "OPS",
      "ENG",
    ]);
    expect(rows[1]!.textContent).toContain("3 open");
    fireEvent.click(rows[0]!);
    expect(location.hash).toBe("#/worklet/OPS");
    expect(p.closed).toHaveBeenCalled();
  });

  test("Switch project… is a place to go, and opens the switcher as the palette closes", async () => {
    const p = mount({ answers: { work_projects: listing } });
    // WITH WORK'S PLACES, under the sidebar's own Workspace heading.
    const go = screen.getByRole("group", { name: "Workspace" });
    const row = within(go).getByRole("option", { name: /Switch project…/ });
    expect(row.textContent).toContain("or G then P, from anywhere");
    await act(async () => {
      fireEvent.click(row);
    });
    expect(p.closed).toHaveBeenCalled();
    expect(await screen.findByRole("dialog", { name: "Switch project" })).toBeTruthy();
  });

  test("it is a command too, found by its name", async () => {
    const p = mount();
    await p.type(">switch");
    expect(p.row("Switch project…")).toBeTruthy();
  });

  test("Search all work for a term goes to the ranked search", async () => {
    const p = mount();
    await p.type("invoice");
    fireEvent.click(p.row("Search all work for “invoice”"));
    expect(location.hash).toBe("#/worklet/search?q=invoice");
  });
});

// A PERSON LOOKS FOR THEIR ACCOUNT BY WHAT IS ON IT. Only the word "account"
// found it, so a reader typing "password" or "2fa" was offered nothing but a
// search — while the row's own hint named both. The words are the table's
// (`Destination.keywords`), matched after every label and word by word.
describe("a screen found by what it holds", () => {
  test.each([
    "password",
    "2fa",
    "two-step",
    "two step",
    "security",
    "profile",
    "sessions",
    "token",
    "my account",
  ])("%s finds Account, and Enter on it opens it", async (q) => {
    const p = mount();
    await p.type(q);
    const go = screen.getByRole("group", { name: "Go to" });
    const row = within(go).getByRole("option", { name: /^Account/ });
    fireEvent.click(row);
    expect(location.hash).toBe("#/account");
  });

  // THE CHARTER IS FOUND BY WHAT IT HOLDS, and opens where it is READ: the
  // palette is a way to get somewhere, so it lands on the page rather than
  // in an editor, and never in the org builder that used to write it.
  test.each(["mission", "vision", "policies", "charter"])(
    "%s finds Principles, and Enter on it opens the page as it reads",
    async (q) => {
      const p = mount();
      await p.type(q);
      const go = screen.getByRole("group", { name: "Go to" });
      fireEvent.click(within(go).getByRole("option", { name: /^Principles/ }));
      expect(location.hash).toBe("#/company/principles");
    },
  );

  // THE EMPTY PALETTE DRAWS THE SIDEBAR'S TREE: one flat "Go to" of every
  // place hid it, and set look-alikes side by side. Mutation: push every
  // destination as one group and the headings are gone.
  test("the empty palette lists every place under the sidebar's own headings", () => {
    mount();
    const headings = ["Go to", "Company", "Workspace", "Settings", "Account"];
    for (const name of headings) expect(screen.getByRole("group", { name })).toBeDefined();
    expect(
      within(screen.getByRole("group", { name: "Company" })).getByRole("option", {
        name: /^Budgets/,
      }),
    ).toBeDefined();
    expect(
      within(screen.getByRole("group", { name: "Settings" })).getByRole("option", {
        name: /^Token spend/,
      }),
    ).toBeDefined();
  });

  // A LABEL STILL WINS: a screen called what was typed is never pushed under
  // one that merely holds the word. "s" starts two of Account's words
  // ("sessions", "security"), which a flat ranking scored ahead of every label
  // with an "s" in its middle — Tasks, Projects, Agents. Chatlet holds it only
  // in its words too ("messages", "rooms"), so every screen found that way is
  // ranked after every label that has one.
  test("every label match ranks above a keyword match, however good", () => {
    const nav = { to: vi.fn() } as unknown as Parameters<typeof destinationHits>[1];
    const hits = destinationHits("s", nav, Infinity, null, {
      tracker: true,
      knowledge: true,
      chat: true,
    }).map((h) => h.label);
    // THE LABEL MATCHES COME FIRST, then the screens only a keyword found
    // ("Account", "Chatlet"), and never one after the other.
    const firstKeyword = hits.findIndex((label) => !label.toLowerCase().includes("s"));
    expect(firstKeyword).toBeGreaterThan(0);
    expect(hits.slice(firstKeyword)).toEqual(expect.arrayContaining(["Account", "Chatlet"]));
    for (const label of hits.slice(firstKeyword)) expect(label.toLowerCase()).not.toContain("s");
    for (const label of hits.slice(0, firstKeyword)) expect(label.toLowerCase()).toContain("s");
    expect(hits).toContain("Tasks");
  });

  // SETTINGS OPENS WHERE THE READER MAY READ: a reader holding only Worklet
  // was sent to Models & keys, which is Admin's, and its entry read "needs
  // Admin access" although Token spend and Expensive tasks are theirs.
  // Mutation: go to the workspace's own path and the press lands on the lock.
  test("a reader holding only Worklet opens Settings on Token spend, unlocked", () => {
    const to = vi.fn();
    const nav = { to } as unknown as Parameters<typeof destinationHits>[1];
    const runs = { tracker: true, knowledge: true, chat: true };
    const settings = destinationHits(
      "settings",
      nav,
      Infinity,
      {
        anonymous: false,
        grants: ["worklet"],
      },
      runs,
    ).find((h) => h.id === "nav-settings")!;
    expect(settings.hint).not.toMatch(/needs/);
    settings.go();
    expect(to).toHaveBeenCalledWith(["settings", "spend"]);
    const admin = destinationHits(
      "settings",
      nav,
      Infinity,
      {
        anonymous: false,
        grants: ["admin"],
      },
      runs,
    ).find((h) => h.id === "nav-settings")!;
    admin.go();
    expect(to).toHaveBeenLastCalledWith(["settings", "models"]);
  });

  // THE HINT IS PROSE AND IS NEVER SEARCHED: Service accounts' mentions
  // "machine principals", and it is no answer to "principals".
  test("a word only a hint's prose holds finds nothing", async () => {
    const p = mount();
    await p.type("principals");
    expect(screen.queryByRole("option", { name: /^Service accounts/ })).toBeNull();
  });
});

// AN ARTIFACT IS FOUND BY ITS TITLE. "Q4 launch" found nothing, and "artif"
// listed the engine's own agent tools instead. All carries the term into the
// Artifacts scope rather than asking a fourth question of its own.
describe("artifacts", () => {
  const listing = (params: Record<string, unknown>) =>
    Promise.resolve({
      contents:
        params["q"] === "Q4 launch"
          ? [{ id: "a-1", kind: "sheet", title: "Q4 launch review", owner: "jane-founder" }]
          : [],
      total: 1,
      read_level: "",
      position: "",
    });

  test("All offers the Artifacts scope for a term, and asks nothing of the listing itself", async () => {
    const p = mount({ answers: { artifacts: listing } });
    await p.type("Q4 launch");
    expect(asked.some((q) => q.kind === "artifacts")).toBe(false);
    await act(async () => {
      fireEvent.click(p.row("Find “Q4 launch” in artifacts →"));
    });
    expect(p.tab()).toBe("Artifacts");
    expect(asked.find((q) => q.kind === "artifacts")?.params).toMatchObject({
      q: "Q4 launch",
      scope: "all",
    });
    const row = await screen.findByText("Q4 launch review");
    // THE KIND AT THE ROW'S END, never cut away with a long title.
    expect(
      row.closest('[role="option"]')?.querySelector(".crewlet-palette__meta")?.textContent,
    ).toBe("Sheet");
  });

  // A RECENT ARTIFACT SAYS ITS WORKSPACE BY NAME: the stored key read
  // "Q4 roadmap artifacts", as if it were one phrase.
  test("a recent artifact names its workspace as the sidebar does", () => {
    remember({ path: ["artifacts", "a-1"], label: "Q4 roadmap", workspace: "artifacts" }, true);
    mount();
    const row = within(screen.getByRole("group", { name: "Recent" })).getAllByRole("option")[0];
    expect(row?.textContent).toContain("Artifacts");
    expect(row?.textContent).not.toContain("artifacts");
  });

  test("the engine's own agent tools are not offered, and a connected tool is", async () => {
    const p = mount();
    act(() => {
      p.store.applyTools([
        { name: "get_artifact", description: "", source: "builtin", annotations: {} },
        { name: "artifact_lint", description: "", source: "mcp:linter", annotations: {} },
      ] as never);
    });
    await p.type("artif");
    expect(screen.queryByText("get_artifact")).toBeNull();
    expect(screen.getByText("artifact_lint")).toBeTruthy();
  });
});

// THE QUESTION, ASKED OF THE ASSISTANT: offered only where this reader may
// ask, and handed to the frame's own control, which opens the panel with it.
test("a typed question can be asked of the assistant, where the reader may ask", async () => {
  const ask = vi.fn();
  const p = mount({ assistant: { open: false, unavailable: null, toggle: () => {}, ask } });
  await p.type("why did spend go up");
  const row = p.row("Ask the assistant: “why did spend go up”");
  await act(async () => {
    fireEvent.click(row);
  });
  expect(ask).toHaveBeenCalledWith("why did spend go up");
  expect(p.closed).toHaveBeenCalled();
  cleanup();
  const q = mount();
  await q.type("why did spend go up");
  expect(screen.queryByText("Ask the assistant: “why did spend go up”")).toBeNull();
});

// THE LAST ANSWER, NEVER ENTER'S BY DEFAULT: the assistant's row closes the
// list (05-ux-design, "Keyboard"), and with nothing found the row Enter takes
// is not the artifacts scope either, which only moves the reader to another
// scope.
test("the assistant's row is last, and the artifacts scope is not what Enter takes", async () => {
  const ask = vi.fn();
  mount({ assistant: { open: false, unavailable: null, toggle: () => {}, ask } });
  const box = screen.getByRole("combobox");
  await act(async () => {
    fireEvent.change(box, { target: { value: "why did spend go up" } });
  });
  const options = screen.getAllByRole("option");
  expect(options.at(-1)?.textContent).toContain("Ask the assistant: “why did spend go up”");
  const id = box.getAttribute("aria-activedescendant") ?? "";
  expect(document.getElementById(id)?.textContent ?? "").not.toContain("in artifacts");
});
