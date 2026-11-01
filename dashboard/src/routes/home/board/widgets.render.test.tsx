/**
 * Every widget a section offers draws on Home's board, whatever the engine
 * answers.
 *
 * A widget is a section's component on a page that section does not own, so
 * its failure mode is a crash nobody opening the section would see: a query
 * that answered nothing, a choice that was never made, a reader whose access
 * no longer opens the page. Each one draws inside the board's own frame
 * (`LeafView`), and that frame's error boundary must never be what the reader
 * sees.
 */

import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import { useEffect, useState, type ReactNode } from "react";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { reloadForTest } from "~/lib/prefs.ts";
import { LiveSocket, QueryError, Store } from "~/protocol/index.ts";
import { FrameReadings } from "~/app/Shell.tsx";
import { SessionReading } from "~/lib/frameSession.ts";
import { healthFrame } from "~/test/health.ts";
import { emptyDay } from "~/test/myWork.ts";
import { noteReader } from "~/lib/reader.ts";
import { resetForTest as resetStars, starsKey } from "~/lib/starred.ts";
import { HomeDataProvider, useHomeData } from "../useHomeData.ts";
import { WIDGETS, canUse } from "./registry.tsx";
import { NOTHING_TO_LIST, destinationOf, type WidgetDef } from "./kit.tsx";
import { LeafView } from "./widgets.tsx";
import type { ContribWidget } from "./model.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

const ADMIN = {
  login: "U0FOUNDER",
  grants: ["admin", "worklet", "doclet"],
  handle: "jane",
  owner: "jane",
  name: "Jane Founder",
  kind: "human",
  acts: ["comment_on_work_item", "answer_executor", "update_work_item", "create_work_item"],
};

/** A reader holding the tracker and the knowledge base, and not Admin. */
const MEMBER = { ...ADMIN, grants: ["worklet", "doclet"] };

const seat = (name: string, handle: string, kind: string, managers: string[] = []) => ({
  handle,
  name,
  kind,
  placed_by_ref: false,
  manager: managers[0] ?? "",
  managers,
  reports: [],
  auto_reports: [],
  onboarding_chain: [],
});

const ORG = {
  name: "Nimbus",
  timezone: "UTC",
  roles: [
    { name: "Jane Founder", handle: "jane", kind: "human" },
    { name: "CTO", handle: "cto", kind: "agent" },
  ],
  units: [],
  derived: {
    seats: [seat("Jane Founder", "jane", "human"), seat("CTO", "cto", "agent", ["jane"])],
    units: [],
  },
};

/**
 * The questions Home's own cards ask, answered quietly. Every other question
 * a widget asks is REFUSED as one this engine does not know, the answer an
 * older node gives a newer dashboard: each widget draws the refusal its
 * section's screen draws, never a crash.
 */
const QUIET: Record<string, unknown> = {
  decisions: { handle: "jane", items: [], total: 0, capped: false },
  work_flow: {
    bucket: "day",
    points: [],
    now: { not_started: 0, active: 0, blocked: 0, overdue: 0 },
    blocked_history: false,
    complete: true,
  },
  token_series: {
    group: "unit",
    bucket: "day",
    series: [],
    by_group: [],
    totals: { input_tokens: 0, output_tokens: 0, total_tokens: 0, calls: 0 },
    grouped: { input_tokens: 0, output_tokens: 0, total_tokens: 0, calls: 0 },
  },
  company_feed: { rows: [], complete: true },
  work_projects: {
    projects: [
      {
        key: "LEAD",
        name: "Launch",
        archived: false,
        lead: { handle: "cto" },
        task_counts: { todo: 2, active: 1, done: 1 },
      },
    ],
    total: 1,
    census: {},
    complete: true,
  },
  executors: { runs: [] },
  work_inbox: { handle: "jane", notices: [], primary_reasons: [] },
};

/**
 * Answer REST reads from `served`, a path's own answer, and refuse every
 * other one, as the default does. A path is matched as the read names it,
 * query and all.
 */
function serveRest(served: Record<string, unknown> = {}) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input instanceof Request ? input.url : input);
      const path = Object.keys(served).find((p) => url.endsWith(p));
      return path !== undefined
        ? new Response(JSON.stringify(served[path]), {
            status: 200,
            headers: { "Content-Type": "application/json" },
          })
        : new Response(JSON.stringify({ code: "not_found", message: "Not here." }), {
            status: 404,
            headers: { "Content-Type": "application/json" },
          });
    }),
  );
}

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  location.hash = "#/home";
  // EVERY REST READ IS REFUSED: a widget that reads over REST draws the
  // refusal, never a crash.
  serveRest();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  localStorage.clear();
  reloadForTest();
  location.hash = "";
});

/** Home's providers, as `Home` mounts them, around `children`. */
function Page({ children }: { children: ReactNode }) {
  return <HomeDataProvider value={useHomeData()}>{children}</HomeDataProvider>;
}

function mount(
  children: ReactNode,
  viewer: Record<string, unknown> = ADMIN,
  answers: Record<string, unknown> = {},
  base: Record<string, unknown> = QUIET,
) {
  const store = new Store();
  // A SHORT ANSWER'S HEALTH IS SHORT TOO: the live push this node seeded at
  // boot missed the node every other short answer missed.
  store.applyHealth(
    healthFrame({ nodes: 1, ...(base === PARTIAL ? { seeded_from: SHORT_A_NODE } : {}) }),
  );
  store.applyOrg(ORG as never);
  store.applySeats([] as never);
  const socket = new LiveSocket(store);
  // AN ANSWER MAY BE A FUNCTION OF THE QUESTION'S PARAMETERS, for the case
  // where two surfaces ask one question at different limits.
  socket.query = ((what: string, params?: Record<string, unknown>) => {
    const all: Record<string, unknown> = { ...base, viewer, ...answers };
    const answer = all[what];
    return what in all
      ? Promise.resolve(typeof answer === "function" ? answer(params ?? {}) : answer)
      : Promise.reject(new QueryError("unknown_query"));
  }) as typeof socket.query;
  return render(
    <ToastProvider>
      <LayerHost>
        <ClientContext.Provider value={{ store, socket }}>
          <SessionReading>
            <FrameReadings>
              <Router>
                <Page>{children}</Page>
              </Router>
            </FrameReadings>
          </SessionReading>
        </ClientContext.Provider>
      </LayerHost>
    </ToastProvider>,
  );
}

async function settle() {
  await act(async () => {
    for (let i = 0; i < 8; i++) await Promise.resolve();
  });
}

const leaf = (def: WidgetDef, params: Record<string, string>): ContribWidget => ({
  id: "w-under-test",
  kind: "widget",
  widget: def.key,
  params,
  w: def.size.w,
  h: null,
});

/**
 * A widget with its choices made from its own offer: each param takes the
 * first option its `useChoices` gives, as a reader picking the first entry of
 * the settings sheet would. A param with nothing to offer stays unchosen.
 */
function Chosen({ def }: { def: WidgetDef }) {
  const params: Record<string, string> = {};
  for (const p of def.params ?? []) {
    // One hook per param, in the def's fixed order.
    // eslint-disable-next-line react-hooks/rules-of-hooks
    const first = p.useChoices().options[0];
    if (first) params[p.key] = first.value;
  }
  // The choice settles before the widget is drawn, as Add to Home does.
  const [shown, setShown] = useState<Record<string, string> | null>(null);
  const wanted = JSON.stringify(params);
  useEffect(() => setShown(JSON.parse(wanted) as Record<string, string>), [wanted]);
  return shown ? <LeafView widget={leaf(def, shown)} /> : null;
}

/** The sentence the board's frame draws in place of a widget that could not draw. */
const BOUNDARY = /could not be drawn/;

const defs = [...WIDGETS.values()];

describe("every widget a section offers", () => {
  test("is offered at all: the registry found Home's and every section's", () => {
    const workspaces = new Set(defs.map((d) => destinationOf(d.from)?.workspace));
    for (const w of [
      "home",
      "inbox",
      "me",
      "org",
      "schedules",
      "integrations",
      "budgets",
      "worklet",
      "chatlet",
      "live",
      "doclet",
      "settings",
    ])
      expect(workspaces.has(w as never), w).toBe(true);
  });

  test.each(defs.map((d) => [d.key, d] as const))(
    "%s draws for an admin with its first choices, whatever the engine answers",
    async (_key, def) => {
      const { container } = mount(<Chosen def={def} />);
      await settle();
      expect(container.textContent ?? "").not.toMatch(BOUNDARY);
      expect(container.textContent ?? "").not.toMatch(/no longer/);
      expect((container.textContent ?? "").trim().length).toBeGreaterThan(0);
    },
  );
});

describe("a widget whose choice is not made", () => {
  // `live.events` has a choice with a meaning when unmade: every event.
  const choosers = defs.filter((d) => (d.params?.length ?? 0) > 0 && d.key !== "live.events");

  test("there are some to hold to it", () => {
    expect(choosers.length).toBeGreaterThan(0);
  });

  // Nothing saved yet to choose from: the lists a choice is made from answer empty.
  const NOTHING_SAVED = { work_saved_views: { views: [] }, containers: { containers: [] } };

  test.each(choosers.map((d) => [d.key, d] as const))(
    "%s says where the choice is made",
    async (_key, def) => {
      const { container } = mount(<LeafView widget={leaf(def, {})} />, ADMIN, NOTHING_SAVED);
      await settle();
      expect(container.textContent ?? "").not.toMatch(BOUNDARY);
      expect(screen.getByText(/^Choose an? .+ in Edit Home\.$/)).toBeTruthy();
    },
  );

  test.each(["worklet.view", "doclet.space"])(
    "%s says its list of choices was refused rather than waiting on it",
    async (key) => {
      const { container } = mount(<LeafView widget={leaf(WIDGETS.get(key)!, {})} />);
      await settle();
      expect(container.textContent ?? "").not.toMatch(BOUNDARY);
      expect(screen.queryByText(/in Edit Home\.$/)).toBeNull();
      expect(container.querySelector(".crewlet-skeleton")).toBeNull();
    },
  );

  test("a choice that names nothing the widget offers is treated as unmade", async () => {
    const shortcut = WIDGETS.get("home.shortcut")!;
    mount(<LeafView widget={leaf(shortcut, { page: "no-such-page" })} />);
    await settle();
    expect(screen.getByText("Choose a page in Edit Home.")).toBeTruthy();
  });
});

describe("a reader whose access no longer opens a widget's page", () => {
  // WHAT A MEMBER (Worklet and Doclet, no Admin) CANNOT OPEN, read off the
  // registry's own rule: Spend's readings and the models are every reader's,
  // so a Settings widget is not by its key alone a closed one.
  const closed = defs.filter((d) => !canUse(d, MEMBER.grants));

  test("there are some to hold to it", () => {
    expect(closed.map((d) => d.key)).toContain("settings.secrets");
  });

  test.each(closed.map((d) => [d.key, d] as const))(
    "%s says so, and asks nothing of its section",
    async (_key, def) => {
      const { container } = mount(<LeafView widget={leaf(def, {})} />, MEMBER);
      await settle();
      const page = destinationOf(def.from)!.label;
      expect(screen.getByText(`You no longer have access to ${page}.`)).toBeTruthy();
      expect(container.textContent ?? "").not.toMatch(BOUNDARY);
    },
  );

  test("a widget its section no longer offers says so rather than vanishing", async () => {
    mount(
      <LeafView
        widget={{ id: "w-gone", kind: "widget", widget: "gone.away", params: {}, w: 4, h: null }}
      />,
    );
    await settle();
    expect(
      screen.getByText("This widget is no longer available. Remove it in Edit Home."),
    ).toBeTruthy();
  });
});

// ---------------------------------------------------------------------------
// An answer that could not account for everything
// ---------------------------------------------------------------------------

/** One project, as `work_project` answers it. */
const PROJECT = {
  key: "LEAD",
  name: "Launch",
  unit: { key: "", name: "" },
  lead: { handle: "cto" },
  task_counts: { todo: 2, active: 1, done: 1 },
  version: 1,
  statuses: [],
  types: [],
  fields: [],
  policy_stamp: 1,
  complete: true,
};

/** A fleet read one node did not answer. */
const SHORT_A_NODE = {
  complete: false,
  nodes: [
    { id: "node-1", answered: true, error: "" },
    { id: "node-2", answered: false, error: "timeout" },
  ],
};

/**
 * Every question a widget reads whose answer can say it is short, answered
 * EMPTY and saying so: a record this build cannot decode met the read
 * (`complete: false`), or a node did not answer it (`coverage`). Each widget's
 * choice is still offered (a saved view, a project, a space), so a widget is
 * held to what it draws over its answer rather than to its picker.
 */
const PARTIAL: Record<string, unknown> = {
  ...QUIET,
  decisions: { handle: "jane", items: [], total: 0, capped: false, complete: false },
  work_flow: { ...(QUIET.work_flow as object), complete: false },
  company_feed: { rows: [], complete: false },
  work_projects: { ...(QUIET.work_projects as object), complete: false },
  work_project: { ...PROJECT, complete: false },
  work_inbox: { handle: "jane", notices: [], primary_reasons: [], complete: false },
  work_my_work: emptyDay("jane", { complete: false }),
  work_items: { items: [], total_hint: 0, total_capped: false, complete: false },
  work_saved_views: {
    complete: true,
    views: [
      {
        id: "v-1",
        key: "mine",
        name: "Mine",
        type: "list",
        container: { kind: "workspace", id: "" },
        builtin: false,
      },
    ],
  },
  page_activity: { changes: [], complete: false },
  pages: { pages: [], total: 0, complete: false },
  containers: { containers: [{ key: "ENG", name: "Engineering" }] },
  memory_overview: { seats: [], coverage: SHORT_A_NODE },
  turns: { turns: [], coverage: SHORT_A_NODE },
  events: { events: [], coverage: SHORT_A_NODE },
  config_audit: [],
  backups: { policy: {}, points: [], history: [], more: false, coverage: SHORT_A_NODE },
  chat_channels: {
    read_level: "local",
    complete: false,
    position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 1 },
    channels: [],
    read_state: true,
    member: true,
  },
  chat_activity: {
    read_level: "local",
    complete: false,
    position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 1 },
    items: [],
    unread: { mention: 0, reply: 0 },
    read_state: true,
  },
};

/**
 * The widgets whose answers carry NO flag saying what they could not account
 * for, each with why. Every widget not named here is held to the gap rule
 * below, so a widget added tomorrow is held to it until somebody writes down
 * why it is not — and an entry naming a widget nobody offers fails.
 */
const NO_GAP: Record<string, string> = {
  "org.seat": "a seat's state is the live push",
  "org.roster": "the roster is the live push",
  "schedules.upcoming": "the schedules' answer carries no flag",
  "home.pinned": "the reader's own stars, kept in this browser; a pinned project says its own gap",
  "home.live": "who is working is the live push",
  "home.tokens": "a window's spend carries no flag",
  "home.figure":
    "its first choice is the agents working now, the live push; the tracker's figures are held in Kpis.test.tsx",
  "home.shortcut": "a link, which reads nothing",
  "live.executors": "the executors' answer and the push carry no flag",
  "settings.nodes": "the fleet's answer carries no flag",
  "integrations.connected": "its coverage is the traffic counts', which the card does not draw",
  "settings.mcp-servers": "the servers' answer carries no flag",
  "settings.models": "the company document carries no flag",
  "settings.secrets": "the secret store's listing carries no flag",
  "settings.config": "the revisions and the fleet carry no flag",
  "settings.spend": "a window's spend carries no flag",
  "budgets.month": "the budget counter carries no flag",
  "budgets.near-limit": "an unread counter is the answer's own `durable`, said as its empty state",
  "settings.spend-by-model": "a window's spend carries no flag",
  "settings.expensive-tasks": "a window's spend carries no flag",
};

/** Every sentence a gap is said in ends by saying the answer may be wrong. */
const GAP = /may be (incomplete|short)/i;

/** The REST reads a widget makes, answered short as [PARTIAL] answers the socket's. */
const PARTIAL_REST: Record<string, unknown> = {
  "/iam/people?limit=200": { people: [] },
  // A BINDING THIS NODE COULD NOT CHECK is the directory check's gap.
  "/iam/check": { findings: [], people_with_admin: 1, bindings_unchecked: 2 },
};

describe("a widget over an answer that could not account for everything", () => {
  test("every widget the list names is offered", () => {
    expect(Object.keys(NO_GAP).filter((key) => !WIDGETS.has(key))).toEqual([]);
  });

  const held = defs.filter((d) => !(d.key in NO_GAP));

  test("holds most of them to it", () => {
    expect(held.length).toBeGreaterThan(20);
  });

  test.each(held.map((d) => [d.key, d] as const))(
    "%s says what its answer missed, and never its own claim of nothing",
    async (_key, def) => {
      serveRest(PARTIAL_REST);
      const { container } = mount(<Chosen def={def} />, ADMIN, {}, PARTIAL);
      await settle();
      const text = container.textContent ?? "";
      expect(text).not.toMatch(BOUNDARY);
      expect(text).toMatch(GAP);
      // AN EMPTY STATE OVER A GAP IS THE KIT'S, never the widget's claim.
      const titles = [...container.querySelectorAll(".crewlet-empty-state__title")].map(
        (el) => el.textContent,
      );
      expect(titles.filter((title) => title !== NOTHING_TO_LIST)).toEqual([]);
    },
  );
});

describe("what a widget counts over a page", () => {
  test("Projects says the busiest were chosen from a page the engine cut", async () => {
    mount(<LeafView widget={leaf(WIDGETS.get("home.projects")!, {})} />, ADMIN, {
      work_projects: { ...(QUIET.work_projects as object), truncated: true },
    });
    await settle();
    expect(screen.getByText(/^Only the first 200 projects were read/)).toBeTruthy();
  });

  test("Backups counts failed runs over the newest runs when the history was cut", async () => {
    const run = (outcome: string, i: number) => ({
      id: `run-${i}`,
      at: `2026-10-0${i + 1}T09:00:00Z`,
      outcome,
      summary: "",
    });
    const history = [run("failed", 0), run("succeeded", 1)];
    const backups = { policy: {}, points: [], history, coverage: { complete: true, nodes: [] } };
    mount(<LeafView widget={leaf(WIDGETS.get("settings.backups")!, {})} />, ADMIN, {
      backups: { ...backups, more: true },
    });
    await settle();
    expect(screen.getByText("in the newest 2")).toBeTruthy();
    cleanup();
    mount(<LeafView widget={leaf(WIDGETS.get("settings.backups")!, {})} />, ADMIN, {
      backups: { ...backups, more: false },
    });
    await settle();
    expect(screen.getByText("in the recent history")).toBeTruthy();
  });
});

describe("the Inbox widget", () => {
  /** One unread primary notice, as the engine sends it. */
  const notice = (n: number) => ({
    record_id: `r-${n}`,
    log_seq: n,
    log_stream: "CREWLET_TRACKER_LOG",
    log_generation: 1,
    at: "2026-10-20T09:00:00Z",
    reason: "assignee",
    primary: true,
    addressed: true,
    kind: "assigned",
    subject_id: `t-${n}`,
    subject_key: `ENG-${n}`,
    excerpt: "",
    actor: "cto",
    actor_kind: "agent",
    read: false,
  });

  // THE CARD AND THE SIDEBAR SAY ONE COUNT: the card's page holds the few rows
  // it draws, and the sidebar's holds a page of fifty with a cursor past it.
  test("counts what the sidebar counts, as the floor the sidebar draws", async () => {
    const inbox = (params: Record<string, unknown>) => {
      const limit = Number(params.limit);
      const all = Array.from({ length: 60 }, (_, i) => notice(i + 1));
      return {
        handle: "jane",
        notices: all.slice(0, limit),
        primary_reasons: ["assignee"],
        primary: Math.min(limit, all.length),
        next_cursor: limit < all.length ? "more" : "",
        complete: true,
      };
    };
    mount(<LeafView widget={leaf(WIDGETS.get("inbox.updates")!, {})} />, ADMIN, {
      work_inbox: inbox,
    });
    await settle();
    expect(screen.getByText("Inbox · 50+ unread")).toBeTruthy();
    expect(screen.getByText("All 50+ unread")).toBeTruthy();
  });
});

describe("the Agent skills widget", () => {
  const def = () => WIDGETS.get("doclet.skills")!;
  const skill = (id: string, title: string, updated_at: string) => ({
    id,
    container: "SKILLS",
    title,
    status: "published",
    version: 1,
    skill: true,
    updated_at,
    revision: 1,
  });

  // THE NEWEST ARE THE ENGINE'S, read newest first across every skill the
  // company keeps, and drawn in the order answered. It asked for fifty in the
  // knowledge base's own order and sorted them here, so past fifty skills the
  // newest was the newest of the alphabetically first fifty. Here the
  // title-ordered answer holds none of the newest, and the newest skill's
  // title sorts last.
  //
  // Mutation: ask without `order`, and the widget lists the title window;
  // sort the answer here by title, and "Zeta" is drawn last.
  test("lists the newest skills the engine read, in its order", async () => {
    const asked: Record<string, unknown>[] = [];
    mount(<LeafView widget={leaf(def(), {})} />, ADMIN, {
      pages: (params: Record<string, unknown>) => {
        asked.push(params);
        return params.order === "newest"
          ? {
              pages: [
                skill("s-9", "Zeta", "2026-10-19T10:00:00Z"),
                skill("s-8", "Kappa", "2026-10-18T10:00:00Z"),
              ],
              total: 64,
              limit: params.limit,
            }
          : {
              pages: [skill("s-1", "Alpha", "2026-09-01T10:00:00Z")],
              total: 64,
              limit: params.limit,
            };
      },
    });
    await settle();
    expect(asked.at(-1)).toMatchObject({ skills: true, status: "published", order: "newest" });
    expect(screen.getByText("Agent skills · 64")).toBeTruthy();
    expect(screen.queryByText("Alpha")).toBeNull();
    const zeta = screen.getByText("Zeta");
    const kappa = screen.getByText("Kappa");
    expect(zeta.compareDocumentPosition(kappa)).toBe(Node.DOCUMENT_POSITION_FOLLOWING);
  });
});

describe("the Project widget", () => {
  const def = () => WIDGETS.get("worklet.project")!;

  // BY ITS KEY, never found in the picker's page: a project past the page the
  // picker read is still the project the reader chose.
  test("draws its project from the project's own answer, past the picker's page", async () => {
    mount(<LeafView widget={leaf(def(), { project: "ENG" })} />, ADMIN, {
      work_projects: { ...(QUIET.work_projects as object), truncated: true },
      work_project: { ...PROJECT, key: "ENG", name: "Engineering" },
      work_items: { items: [], total_hint: 0, total_capped: false, complete: true },
    });
    await settle();
    expect(screen.getByText("ENG · Engineering")).toBeTruthy();
    expect(screen.queryByText(/^Choose a .+ in Edit Home\.$/)).toBeNull();
  });

  test("a key the engine no longer answers to is said with its key", async () => {
    mount(<LeafView widget={leaf(def(), { project: "GONE" })} />, ADMIN, {
      work_project: () => Promise.reject(new QueryError("not_found")),
    });
    await settle();
    expect(screen.getByText(/^There’s no project GONE any more\./)).toBeTruthy();
  });

  test("its picker says when the engine cut the list it offers", async () => {
    function Choices() {
      const choices = def().params![0]!.useChoices();
      return <p>{choices.loading ? "…" : (choices.partial ?? "whole")}</p>;
    }
    mount(<Choices />, ADMIN, {
      work_projects: { ...(QUIET.work_projects as object), truncated: true },
    });
    await settle();
    expect(screen.getByText("Only the first 200 projects are offered.")).toBeTruthy();
  });
});

describe("a pinned project", () => {
  afterEach(() => resetStars());

  test("draws its census from its own answer, however many projects the company has", async () => {
    noteReader("p-1");
    localStorage.setItem(
      starsKey("p-1"),
      JSON.stringify([
        { path: ["worklet", "ENG"], label: "Engineering", workspace: "worklet", at: 1 },
      ]),
    );
    resetStars();
    mount(<LeafView widget={leaf(WIDGETS.get("home.pinned")!, {})} />, ADMIN, {
      // THE FIRST PAGE OF PROJECTS DOES NOT HOLD IT.
      work_projects: { ...(QUIET.work_projects as object), truncated: true },
      work_project: { ...PROJECT, key: "ENG", name: "Engineering" },
    });
    await settle();
    expect(screen.getByText("Engineering")).toBeTruthy();
    expect(screen.getByText("25%")).toBeTruthy();
  });
});

describe("the People & access widget", () => {
  const check = (unchecked: number) => ({
    "/iam/people?limit=200": { people: [] },
    "/iam/check": { findings: [], people_with_admin: 1, bindings_unchecked: unchecked },
  });

  // "NO DANGLING BINDING" AND "THIS NODE COULD NOT SAY" ARE DIFFERENT ANSWERS,
  // as the directory's own page draws them.
  test("never says no access problems were found over bindings it could not check", async () => {
    serveRest(check(2));
    mount(<LeafView widget={leaf(WIDGETS.get("settings.people")!, {})} />);
    await settle();
    expect(await screen.findByText(/^2 bindings couldn’t be checked just now/)).toBeTruthy();
    expect(screen.queryByText("No access problems found.")).toBeNull();
  });

  test("says no access problems were found when every binding was checked", async () => {
    serveRest(check(0));
    mount(<LeafView widget={leaf(WIDGETS.get("settings.people")!, {})} />);
    await settle();
    expect(await screen.findByText("No access problems found.")).toBeTruthy();
  });
});
