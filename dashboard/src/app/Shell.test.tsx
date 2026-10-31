/**
 * The frame outlives every screen in it, so what a screen published has to go
 * when the screen does.
 *
 * `Shell` is mounted once for the life of the tab — `App` renders
 * `<Shell><Screen/></Shell>`, and only `Screen`'s children remount per route —
 * so a value a screen writes into the frame is a value the frame will keep
 * showing over the NEXT screen unless something takes it back. The coverage
 * facts are the ones where that is worst: they are the state bar's answer to
 * "can I trust what I am looking at", and only three screens publish them.
 */

import { toggleStar } from "~/lib/starred.ts";
import { noteReader } from "~/lib/reader.ts";
import { useRef, type ReactNode } from "react";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useSearchTarget } from "./searchTarget.ts";
import { afterEach, beforeAll, beforeEach, describe, expect, test, vi } from "vitest";
import { Shell, usePageCoverage, usePublishFleet, useSectionCounts } from "./Shell.tsx";
import { CHUNKS, loadChunk } from "./lazyScreen.ts";
import { Router, buildHash } from "./router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store, type FleetAnswer } from "~/protocol/index.ts";
import type { CoverageFacts } from "~/components/work.tsx";
import { setDensity } from "~/lib/prefs.ts";
import { Home } from "~/routes/home/Home.tsx";
import { Inbox } from "~/routes/inbox/Inbox.tsx";
import { installWindow } from "~/testing.tsx";
import { PAGE_ACTIONS_SLOT, PAGE_LENSES_SLOT } from "./frame/PageActions.tsx";
import { Project } from "~/routes/work/Project.tsx";
import { healthFrame } from "~/test/health.ts";
import { emptyDay } from "~/test/myWork.ts";
import { withDerived } from "~/test/org.ts";
import { ZERO_VERSIONS } from "~/test/liveCall.ts";
import { settled } from "~/test/settle.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

/** A screen that publishes coverage, as the Inbox does. */
function Covered({ coverage }: { coverage: CoverageFacts }) {
  usePageCoverage(coverage);
  return <div>the covered screen</div>;
}

/** A screen that publishes none — most of Settings and every detail page. */
function Bare() {
  return <div>the bare screen</div>;
}

/** A DIFFERENT component that also publishes, so React really remounts: two
 *  renders of the same function reconcile, which exercises the deps-changed
 *  path rather than the unmount/mount ordering this has to get right. */
function AlsoCovered({ coverage }: { coverage: CoverageFacts }) {
  usePageCoverage(coverage);
  return <div>the other covered screen</div>;
}

const INCOMPLETE: CoverageFacts = {
  read_level: "linearizable",
  complete: false,
  log_seq: 88,
  applied_through: 41,
  incomplete: {
    records: 3,
    version: 4,
    from: { stream: "CREWLET_WORK_LOG", generation: 1, seq: 42 },
    scope: [],
  },
};

// EVERY CHUNK THE FRAME DRAWS FROM IS IN BEFORE A CASE STARTS, as in
// `App.test.tsx`. The frame draws lazy parts of its own — the New task sheet
// (the Work chunk's), the palette (a chunk of its own), and each peek out of
// its workspace's chunk — and what these cases assert is what the frame draws
// and asks, not how long a cold `import()` takes under a test transformer,
// which a `waitFor` would otherwise be measuring. Loading the Work chunk alone
// left a peek of another workspace to suspend inside an act the case never
// awaited, which React says in the console. The loading path is
// `lazy.test.tsx`'s.
beforeAll(async () => {
  await Promise.all(CHUNKS.map((chunk) => loadChunk(chunk)));
});

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  // jsdom implements no scrolling at all, and the palette keeps its cursor row
  // in view — same stub as `palette/Palette.test.tsx`, for the same reason.
  Element.prototype.scrollIntoView = () => {};
  location.hash = "#/";
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  location.hash = "#/";
});

function frame(child: ReactNode) {
  const store = new Store();
  const view = render(
    <ClientContext.Provider value={{ store, socket: new LiveSocket(store) }}>
      <Router>
        <Shell>{child}</Shell>
      </Router>
    </ClientContext.Provider>,
  );
  return {
    store,
    view,
    show: (next: ReactNode) =>
      view.rerender(
        <ClientContext.Provider value={{ store, socket: new LiveSocket(store) }}>
          <Router>
            <Shell>{next}</Shell>
          </Router>
        </ClientContext.Provider>,
      ),
  };
}

describe("the state bar's coverage", () => {
  test("it is drawn for the screen that published it", () => {
    frame(<Covered coverage={INCOMPLETE} />);
    expect(screen.getByText(/applied through 41 of 88/)).toBeDefined();
    expect(
      screen.getByText(
        "Some records need a newer engine version to read, so this may be incomplete.",
      ),
    ).toBeDefined();
    // AND THE LEVEL IS NOT A WORD ON THE SCREEN. `linearizable` is stronger
    // than this surface's own default; a chip naming it is a label nobody
    // reads, and the one level that matters then arrives as a changed word
    // inside it. See [CoverageTags] in components/work.tsx.
    expect(screen.queryByText("linearizable")).toBeNull();
  });

  // AND A LEVEL THIS SURFACE DID NOT EXPECT IS SAID IN WORDS.
  //
  // `stale`, `session` and `linearizable` are what a dashboard answer is
  // served at. Anything else means the node could not measure its own distance
  // from the log — including a level a later engine invents, which is why the
  // list is the ordinary ones rather than the odd ones: an unknown value is
  // SHOWN.
  test("a level this surface did not expect says what it means", () => {
    frame(
      <Covered
        coverage={{
          read_level: "consistent_prefix",
          complete: true,
          log_seq: 9,
          applied_through: 9,
        }}
      />,
    );
    expect(screen.getByText("age unknown")).toBeDefined();
    expect(screen.queryByText("consistent_prefix")).toBeNull();
  });

  // THE ONE THAT SHIPPED. Land on the Inbox, which publishes; click through to
  // a screen that does not (Settings › Secrets, say), and the inbox's
  // freshness badge and its "this may be incomplete" banner stayed in the
  // bar as claims about data the new screen never read.
  test("it goes when the screen that published it does", () => {
    const { show } = frame(<Covered coverage={INCOMPLETE} />);
    show(<Bare />);
    expect(screen.getByText("the bare screen")).toBeDefined();
    expect(screen.queryByText(/applied through/)).toBeNull();
    expect(
      screen.queryByText(
        "Some records need a newer engine version to read, so this may be incomplete.",
      ),
    ).toBeNull();
  });

  // …and the reset must not eat the INCOMING screen's own facts, which is why
  // it is a cleanup on the hook rather than an effect in the Shell keyed on
  // the route: React flushes a child's effects before its parent's, so a
  // Shell-level reset would run after the new screen had already published.
  test("a screen that publishes its own replaces it rather than losing it", async () => {
    // ON A FAKED CLOCK, so a reset deferred by any timer at all can be run.
    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout", "setInterval", "clearInterval"] });
    const { show } = frame(<Covered coverage={INCOMPLETE} />);
    show(
      <AlsoCovered
        coverage={{ read_level: "stale", complete: true, log_seq: 9, applied_through: 4 }}
      />,
    );
    // FLUSHED, because a reset that lands LATE is the failure being excluded:
    // a deferred one — a microtask, a timeout, a parent effect — runs after
    // the incoming screen has already published and blanks it, and a
    // synchronous assertion cannot see that happen. Every timer the swap
    // armed runs here, and every microtask around them, so whatever the
    // outgoing screen left to happen later has happened.
    await act(() => vi.runOnlyPendingTimersAsync());
    expect(screen.getByText(/applied through 4 of 9/)).toBeDefined();
    expect(screen.queryByText(/applied through 41 of 88/)).toBeNull();
    expect(
      screen.queryByText(
        "Some records need a newer engine version to read, so this may be incomplete.",
      ),
    ).toBeNull();
  });
});

/**
 * The engine's own empty answers for the reads every mounted shell makes, so a
 * case that is about something else is not answered a shape the engine never
 * sends.
 */
const EMPTY: Record<string, unknown> = {
  inbox: { handle: "ada", notices: [], primary_reasons: [], sources: [] },
  work_views: { complete: true, views: [] },
  work_saved_views: { complete: true, views: [] },
  work_projects: { projects: [] },
  work_my_work: emptyDay("ada"),
  fleet: { nodes: [], seats: [], duties: [], target_epoch: 0 },
  retention: { domains: [], nodes: [], snapshots: [], alarms: [], register_readable: true },
  integrations: { integrations: [], tools: [], traffic_known: true, traffic_since: null },
};

/** How many reads every [answering] socket of the file has been asked. */
let queried = 0;

/** A socket answering a bound viewer and whatever else a case supplies. */
function answering(
  answers: Record<string, unknown>,
  asked: { what: string; params?: Record<string, unknown> }[] = [],
) {
  const store = new Store();
  // THE SOCKET'S FIRST FRAME CARRIES THE ORG, as a served frame's always
  // does: what the company runs is known before a read is asked.
  // NAMELESS, so a case that names the company applies its own.
  store.applyOrg(withDerived({ name: "", roles: [] }));
  const socket = new LiveSocket(store);
  (
    socket as unknown as {
      query: (what: string, params?: Record<string, unknown>) => Promise<unknown>;
    }
  ).query = (what: string, params?: Record<string, unknown>) => {
    asked.push({ what, params });
    queried += 1;
    if (what === "viewer") {
      return Promise.resolve(
        answers.viewer ?? {
          login: "U0FOUNDER",
          grants: ["admin", "tracker", "knowledge"],
          handle: "ada",
          owner: "ada",
          name: "Ada Lovelace",
          kind: "human",
        },
      );
    }
    return Promise.resolve(answers[what] ?? EMPTY[what] ?? {});
  };
  return { store, socket };
}

function mountShell(store: Store, socket: LiveSocket, child: ReactNode = <Bare />) {
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Shell>{child}</Shell>
      </Router>
    </ClientContext.Provider>,
  );
}

/**
 * Let the queries the first render made answer, and the renders they cause
 * land, including the reads those renders asked in turn (see `settled`).
 */
async function settle(): Promise<void> {
  await settled(() => queried);
}

function notice(reason: string, n: number) {
  return {
    record_id: `r-${n}`,
    log_seq: n,
    log_stream: "CREWLET_WORK_LOG",
    log_generation: 1,
    at: new Date().toISOString(),
    reason,
    primary: true,
    addressed: false,
    kind: "task_updated",
    subject_id: `s-${n}`,
    read: false,
  };
}

describe("the Inbox badge", () => {
  // A BADGE NOBODY CAN DRIVE DOWN IS A BROKEN COUNTER. Every unread notice
  // counted, most of a busy company's notices are things it merely told you,
  // and nobody answers those — so the ENGINE is asked for the unread primary
  // ones only, and the badge is the page it answers.
  test("asks the engine for unread primary notices, and counts what it answers", async () => {
    const asked: { what: string; params?: Record<string, unknown> }[] = [];
    const { store, socket } = answering(
      {
        inbox: {
          handle: "ada",
          notices: [notice("assignee", 1)],
          primary_reasons: [],
          sources: [],
        },
      },
      asked,
    );
    mountShell(store, socket);
    await settle();
    const inbox = asked.find((a) => a.what === "inbox");
    expect(inbox?.params).toMatchObject({ handle: "ada", unread: true, primary_only: true });
    const row = screen.getByRole("link", { name: /^Inbox/ });
    expect(row.textContent).toContain("1");
  });

  // A PAGE THE ENGINE HAD MORE THAN is a floor, and says so.
  test("a page with more behind it is drawn as a floor", async () => {
    const { store, socket } = answering({
      inbox: {
        handle: "ada",
        notices: [notice("assignee", 1), notice("mention", 2)],
        primary_reasons: [],
        sources: [],
        next_cursor: "c-2",
      },
    });
    mountShell(store, socket);
    await settle();
    expect(screen.getByRole("link", { name: /^Inbox/ }).textContent).toContain("2+");
  });

  // NOTHING TO ANSWER IS NO BADGE AT ALL. A zero in the accent is a mark a
  // reader checks, and it would be there permanently on a quiet company.
  test("nothing waiting draws no figure", async () => {
    const { store, socket } = answering({
      inbox: { handle: "ada", notices: [], primary_reasons: [], sources: [] },
    });
    mountShell(store, socket);
    await settle();
    expect(screen.getByRole("link", { name: /^Inbox/ }).textContent).toBe("Inbox");
  });
});

describe("the sidebar's figures", () => {
  // THE ENGINE'S WORD, and only it: two seats working, one idle, one parked on
  // a question — the count is two, whatever a live call says. AND IT IS ON
  // LIVE, which holds the running turns: on the Org chart it sent a reader
  // looking for one to the chart.
  test("the Live count is the seats the engine calls working, and the Org chart carries none", async () => {
    const { store, socket } = answering({});
    mountShell(store, socket);
    act(() =>
      store.applySeats([
        { id: "a", agent_id: "a", handle: "a", role: "A", activity: "working" },
        { id: "b", agent_id: "b", handle: "b", role: "B", activity: "working" },
        {
          id: "c",
          agent_id: "c",
          handle: "c",
          role: "C",
          activity: "idle",
          live_call: { in_progress: true, versions: ZERO_VERSIONS },
        },
        { id: "d", agent_id: "d", handle: "d", role: "D", activity: "needs" },
      ] as never),
    );
    expect(screen.getByRole("link", { name: /^Live/ }).textContent).toContain("2");
    expect(screen.getByRole("link", { name: /^Org chart/ }).textContent).toBe("Org chart");
  });

  // ---------------------------------------------------------------------------
  // The running turns, from every screen
  // ---------------------------------------------------------------------------

  /** A roster row for the seat named `role`, its handle and agent id derived
   *  from the name the way these fixtures write them. */
  function seat(role: string, over: Record<string, unknown> = {}) {
    const handle = role.toLowerCase();
    return { id: handle, agent_id: `a-${handle}`, handle, role, ...over };
  }

  /** A seat the engine calls working, on `turn`, started at minute `at`. */
  function workingSeat(role: string, turn: string, at: number, item = "") {
    return {
      ...seat(role),
      activity: "working",
      turn: {
        turn_id: turn,
        started_at: `2026-09-21T10:${String(at).padStart(2, "0")}:00Z`,
        stage: "phase",
        ...(item ? { work_item: { backend: "native", id: item, key: item, project: "ENG" } } : {}),
      },
    };
  }

  /** The sidebar's Running group: its rows, as each one reads and where it goes. */
  function runningRows() {
    const nav = screen.getByRole("navigation", { name: "Navigation" });
    const heading = within(nav).queryByText("Running", { exact: true });
    if (!heading) return [];
    const group = heading.closest(".crewlet-nav-group") as HTMLElement;
    return [...group.querySelectorAll("a")].map((a) => ({
      text: a.textContent ?? "",
      href: a.getAttribute("href") ?? "",
    }));
  }

  const watching = (turn: string) => buildHash(["live", "turns", turn], { tab: "transcript" });

  // EACH RUNNING TURN IS A ROW THAT WATCHES IT, from any screen: the reader
  // looking for one went Agents, the chart, a card, the profile and a tab.
  // OLDEST FIRST, so the turn that starts is appended and nothing a reader is
  // reaching for moves; a seat waiting on a person runs nothing and is not here.
  test("the Running group is a row per working seat, oldest first, each watching its turn", async () => {
    const { store, socket } = answering({});
    mountShell(store, socket);
    expect(runningRows(), "no group while nothing runs").toEqual([]);
    const roster = [
      workingSeat("CTO", "t-late", 9),
      workingSeat("SWE", "t-early", 1, "ENG-412"),
      seat("PM", { activity: "needs" }),
      seat("QA", { activity: "idle" }),
    ];
    act(() => store.applySeats(roster as never));
    const rows = runningRows();
    expect(rows.map((r) => r.href)).toEqual([watching("t-early"), watching("t-late")]);
    // THE SEAT'S NAME, AND THE ITEM ITS TURN IS CHARGED TO as the row's lead.
    expect(rows[0]!.text).toContain("SWE");
    expect(rows[0]!.text).toContain("ENG-412");
    expect(rows[1]!.text).toContain("CTO");
    // A TURN THAT STARTS IS APPENDED; the rows above it hold their places —
    // here on a seat the roster gains, which is how a new seat arrives.
    act(() => store.applySeats([...roster, workingSeat("Ops", "t-new", 20)] as never));
    expect(runningRows().map((r) => r.href)).toEqual([
      watching("t-early"),
      watching("t-late"),
      watching("t-new"),
    ]);
  });

  // FOUR, HOME'S OWN NUMBER, then ONE row to Now running for the rest — which
  // also counts a working seat whose turn has published no id yet, rather than
  // linking it to nothing.
  test("past four the rest are one row to Now running, a seat with no turn id among them", async () => {
    const { store, socket } = answering({});
    mountShell(store, socket);
    act(() =>
      store.applySeats([
        workingSeat("A", "t-a", 1),
        seat("Fresh", { activity: "working", turn: null, live_call: null }),
        workingSeat("B", "t-b", 2),
        workingSeat("C", "t-c", 3),
        workingSeat("D", "t-d", 4),
        workingSeat("E", "t-e", 5),
      ] as never),
    );
    const rows = runningRows();
    expect(rows.map((r) => r.href)).toEqual([
      watching("t-a"),
      watching("t-b"),
      watching("t-c"),
      watching("t-d"),
      "#/live",
    ]);
    expect(rows[4]!.text).toContain("2 more running");
    // AND THE COUNT ON LIVE IS THE WHOLE SET, the group's five rows' worth.
    expect(screen.getByRole("link", { name: /^Live/ }).textContent).toContain("6");
  });

  // HOME'S LIVE NOW AND THE RUNNING GROUP NAME THE SAME SEATS IN THE SAME
  // ORDER, on one screen, over one roster. They were two selections under one
  // cap: the seat with no turn id sorted FIRST (its start read as 0) and Home
  // drew it while the sidebar skipped it, so the two lists differed by two
  // seats beside each other.
  test("Home's Live now names the seats the Running group names, in its order", async () => {
    location.hash = "#/home";
    const { store, socket } = answering({
      work_workload: { rows: [] },
      sandbox_runs: { runs: [] },
      work_projects: { projects: [], complete: true },
    });
    mountShell(store, socket, <Home key="home" />);
    act(() =>
      store.applySeats([
        workingSeat("A", "t-a", 1),
        seat("Fresh", { activity: "working", turn: null, live_call: null }),
        workingSeat("B", "t-b", 2),
        workingSeat("C", "t-c", 3),
        workingSeat("D", "t-d", 4),
        workingSeat("E", "t-e", 5),
      ] as never),
    );
    await settle();
    const turnOf = (hash: string) => /#\/live\/turns\/([^?]+)/.exec(hash)?.[1] ?? "";
    const sidebar = runningRows()
      .map((r) => turnOf(r.href))
      .filter(Boolean);
    const home = [...document.querySelectorAll(".live-list .live-row")].map((row) =>
      turnOf(row.getAttribute("href") ?? ""),
    );
    expect(sidebar).toEqual(["t-a", "t-b", "t-c", "t-d"]);
    expect(home).toEqual(sidebar);
    // AND UNDER THE CAP, where the id-less seat is not crowded out by four
    // others: it is counted on both and drawn on neither. (A push carries the
    // rows that changed, so the three that stopped are pushed idle.)
    act(() =>
      store.applyAgents(
        ["C", "D", "E"].map((role) => ({
          agent_id: `a-${role.toLowerCase()}`,
          activity: "idle",
          turn: null,
          live_call: null,
        })) as never,
      ),
    );
    const homeRows = () =>
      [...document.querySelectorAll(".live-list .live-row")].map((row) =>
        turnOf(row.getAttribute("href") ?? ""),
      );
    expect(runningRows().map((r) => turnOf(r.href))).toEqual(["t-a", "t-b", ""]);
    expect(homeRows()).toEqual(["t-a", "t-b"]);
  });

  // "MORE" THAN NOTHING READS AS ROWS THAT FAILED TO DRAW: with no working
  // seat's turn named yet, the group's one row says how many run, plainly.
  test("with no running turn named yet, the one row says how many run, not how many more", async () => {
    const { store, socket } = answering({});
    mountShell(store, socket);
    act(() =>
      store.applySeats([
        seat("P", { activity: "working", turn: null, live_call: null }),
        seat("Q", { activity: "working", turn: null, live_call: null }),
      ] as never),
    );
    const rows = runningRows();
    expect(rows.map((r) => r.href)).toEqual(["#/live"]);
    expect(rows[0]!.text).toContain("2 running");
    expect(rows[0]!.text).not.toContain("more");
  });

  // THE ONE ROW THE READER IS WATCHING is the current one, as every sidebar row
  // is the screen the reader is on.
  test("the running turn the reader is watching is the current row", async () => {
    location.hash = watching("t-b");
    const { store, socket } = answering({});
    mountShell(store, socket);
    act(() => store.applySeats([workingSeat("A", "t-a", 1), workingSeat("B", "t-b", 2)] as never));
    const nav = screen.getByRole("navigation", { name: "Navigation" });
    const current = [...nav.querySelectorAll('a[aria-current="page"]')].map((a) =>
      a.getAttribute("href"),
    );
    expect(current).toContain(watching("t-b"));
    expect(current).not.toContain(watching("t-a"));
  });

  // ONE CURRENT ROW: a starred page's Starred row is the one the reader
  // followed, and the workspace row containing it gives the mark up. Both
  // carried the current fill and `aria-current`. Mutation: mark the workspace
  // row current whatever a shortcut says and the list holds two.
  test("a starred page is current in Starred alone, not in its workspace too", async () => {
    location.hash = "#/live/events";
    noteReader("p-star");
    const { store, socket } = answering({});
    mountShell(store, socket);
    await settle();
    act(() => void toggleStar({ path: ["live", "events"], label: "Event log", workspace: "live" }));
    await settle();
    const nav = screen.getByRole("navigation", { name: "Navigation" });
    const current = [...nav.querySelectorAll('a[aria-current="page"]')].map((a) =>
      a.getAttribute("href"),
    );
    expect(current).toEqual(["#/live/events"]);
    act(() => void toggleStar({ path: ["live", "events"], label: "Event log", workspace: "live" }));
  });

  // `g r` IS "TAKE ME TO THE RUNNING TURN": the one turn's watch link when one
  // seat is working, and Now running — every one of them — when several are,
  // or none.
  test("g then r watches the one running turn, and opens Now running otherwise", async () => {
    const { store, socket } = answering({});
    mountShell(store, socket);
    const chord = () =>
      act(() => {
        window.dispatchEvent(new KeyboardEvent("keydown", { key: "g", bubbles: true }));
        window.dispatchEvent(new KeyboardEvent("keydown", { key: "r", bubbles: true }));
      });
    chord();
    expect(location.hash).toBe("#/live");
    location.hash = "#/";
    act(() => store.applySeats([workingSeat("A", "t-a", 1)] as never));
    chord();
    expect(location.hash).toBe(watching("t-a"));
    location.hash = "#/";
    act(() => store.applySeats([workingSeat("A", "t-a", 1), workingSeat("B", "t-b", 2)] as never));
    chord();
    expect(location.hash).toBe("#/live");
  });

  // A PIN IS A PERSON'S, and the engine reads the caller's own off the request
  // that asks — there is no `viewer=` to name somebody else's — across every
  // container, and with the engine's own counts.
  test("pinned views are asked for as the caller, and draw the view's own total", async () => {
    const asked: { what: string; params?: Record<string, unknown> }[] = [];
    const { store, socket } = answering(
      {
        work_saved_views: {
          complete: true,
          views: [
            {
              id: "v-1",
              key: "blocked",
              name: "Blocked, org-wide",
              type: "list",
              container: { kind: "workspace", id: "" },
              builtin: false,
              pinned: true,
              count: 7,
            },
            {
              id: "v-2",
              key: "other",
              name: "Not pinned",
              type: "list",
              container: { kind: "workspace", id: "" },
              builtin: false,
            },
          ],
        },
      },
      asked,
    );
    mountShell(store, socket);
    await settle();
    const views = asked.find((a) => a.what === "work_saved_views");
    expect(views?.params).toMatchObject({ counts: true });
    expect(views?.params).not.toHaveProperty("viewer");
    // NOT ONE CONTAINER'S STRIP: a pin made on a project board lives there.
    expect(views?.params).not.toHaveProperty("container");
    // A PIN RUNS THE VIEW: the list with the view's key as `view=`, not the
    // inventory page that describes the view and offers to run it.
    const pin = screen.getByRole("link", { name: /^Blocked, org-wide/ });
    expect(pin.getAttribute("href")).toBe("#/work/tasks?view=blocked");
    expect(pin.textContent).toContain("7");
    expect(screen.queryByText("Not pinned")).toBeNull();
  });

  // A VIEW SAVED ON A PROJECT RUNS ON THAT PROJECT: its filters without its
  // scope would be a different question. The fixture is the engine's own
  // `work_saved_views` row for it — the only read that can return a project
  // view at all, since the workspace strip holds none.
  test("a pinned project view runs on its project, and is current while it runs", async () => {
    location.hash = "#/work/ENG?view=mine";
    const { store, socket } = answering({
      work_saved_views: {
        complete: true,
        views: [
          {
            id: "v-3",
            key: "mine",
            name: "My ENG bugs",
            type: "board",
            container: { kind: "project", id: "ENG" },
            builtin: false,
            pinned: true,
          },
        ],
      },
    });
    mountShell(store, socket);
    await settle();
    // THE PROJECT IS ITS LEAD, so two pins of one name in two projects differ.
    const pin = screen.getByRole("link", { name: /^ENG\s*My ENG bugs/ });
    expect(pin.getAttribute("href")).toBe("#/work/ENG?view=mine");
    expect(pin.getAttribute("aria-current")).toBe("page");
  });

  // MY WORK'S FIGURE IS THE QUEUE'S: the open work assigned to the viewer,
  // counted IN FULL by the engine — `total_hint` over the whole matching set,
  // never the length of a page — and a count that stopped at the engine's
  // ceiling is written as the floor it is. It is the figure the Queue tab
  // carries on the viewer's own day, read once by the frame for both; it used
  // to count the questions asked of the viewer, so the row said 1 over a tab
  // that said 2.
  function queue(total: number, capped?: boolean) {
    return { items: [], groups: [], total_hint: total, total_capped: capped, complete: true };
  }

  test("My work shows the engine's count of the open work assigned to the viewer", async () => {
    const asked: { what: string; params?: Record<string, unknown> }[] = [];
    // THE PAGE IS EMPTY AND THE TOTAL IS NOT: the figure is the total.
    const { store, socket } = answering({ work_items: queue(3) }, asked);
    mountShell(store, socket);
    await settle();
    const read = asked.find((a) => a.what === "work_items" && a.params?.assignee === "ada");
    expect(read?.params).toMatchObject({
      assignee: "ada",
      status_group: "not_started,active",
      subtasks: "separate",
      limit: 1,
    });
    // AND ONLY THAT: the seven-block day is the screen's to read, not the row's.
    expect(asked.some((a) => a.what === "work_my_work")).toBe(false);
    const row = screen.getByRole("link", { name: /^My work/ });
    expect(row.textContent).toContain("3 open, assigned to you");
  });

  test("a count that stopped at the engine's ceiling is drawn as a floor", async () => {
    const { store, socket } = answering({ work_items: queue(10000, true) });
    mountShell(store, socket);
    await settle();
    expect(screen.getByRole("link", { name: /^My work/ }).textContent).toContain(
      `${(10000).toLocaleString()}+ open, assigned to you`,
    );
  });

  test("nothing on you, or no answer yet, draws no figure", async () => {
    const { store, socket } = answering({ work_items: queue(0) });
    mountShell(store, socket);
    await settle();
    expect(screen.getByRole("link", { name: /^My work/ }).textContent).toBe("My work");
  });

  // WHO THIS BROWSER IS IS ONE FACT, READ ONCE. Every `useViewer` is its own
  // standing query, and the shell, the sidebar and the Inbox count each asked
  // for their own — three reads of one answer, all of them holding a slot of
  // the socket's four before a screen's first read could run.
  test("the frame asks who the viewer is once", async () => {
    const asked: { what: string; params?: Record<string, unknown> }[] = [];
    const { store, socket } = answering({}, asked);
    mountShell(store, socket);
    await settle();
    expect(asked.filter((a) => a.what === "viewer")).toHaveLength(1);
  });

  // AND WHAT IS WAITING ON THEM IS ONE READING, whichever surfaces say it.
  // The badge, Home's status line and the Inbox's own band each asked for
  // their own count on their own minute, so on Home and the Inbox the badge
  // and the sentence beside it came from two reads and could disagree; and
  // Home asked who the viewer is a second time besides.
  test.each([
    ["Home", "#/home", <Home key="home" />],
    ["the Inbox", "#/inbox", <Inbox key="inbox" />],
  ])("%s in the frame reads the frame's count and the frame's viewer", async (_, hash, child) => {
    const asked: { what: string; params?: Record<string, unknown> }[] = [];
    location.hash = hash;
    const { store, socket } = answering(
      { work_workload: { rows: [] }, sandbox_runs: { runs: [] } },
      asked,
    );
    mountShell(store, socket, child);
    await settle();
    expect(asked.filter((a) => a.what === "viewer")).toHaveLength(1);
    expect(asked.filter((a) => a.what === "inbox" && a.params?.primary_only === true)).toHaveLength(
      1,
    );
  });

  // EVERY "NEW TASK" OPENS THE FRAME'S ONE SHEET: Home's in its page bar, not
  // a second form, and not the palette it stood in for until the sheet landed.
  // AND IT IS THE ONLY ONE ON HOME: the sidebar's head carries no `+`, so a
  // second New task there is a door nobody decided to put back. Its one
  // control is the engine's status.
  test("Home's New task opens the frame's New task sheet, the only one on the page", async () => {
    location.hash = "#/home";
    const { store, socket } = answering({
      work_workload: { rows: [] },
      sandbox_runs: { runs: [] },
      work_projects: { projects: [], complete: true },
    });
    mountShell(store, socket, <Home key="home" />);
    await settle();
    const presses = screen.getAllByRole("button", { name: /^New task$/ });
    expect(presses).toHaveLength(1);
    const headControls = [...document.querySelectorAll(".crewlet-app-shell__rail-head button")];
    expect(headControls.every((press) => press.closest(".crewlet-app-shell__rail-status"))).toBe(
      true,
    );
    await act(async () => {
      fireEvent.click(presses[0]!);
    });
    await waitFor(() => expect(screen.getByRole("dialog", { name: "New task" })).toBeTruthy());
  });

  // A PROJECT IS NOT A SIDEBAR ROW: it is a scope of Work, switched where
  // Work's trail names it, so the rail carries no group of them.
  test("the sidebar draws no Projects group", async () => {
    location.hash = "#/work/ENG";
    const eng = {
      key: "ENG",
      name: "Core platform",
      unit: { resolved: true },
      lead: {},
      task_counts: { todo: 2, active: 1, done: 0, closed: 0 },
    };
    const { store, socket } = answering({
      work_projects: { projects: [eng], complete: true },
      work_project: { ...eng, statuses: [], types: [], fields: [], complete: true },
    });
    mountShell(store, socket);
    await settle();
    const nav = screen.getByRole("navigation", { name: "Navigation" });
    expect(within(nav).queryByText("Projects")).toBeNull();
    expect(nav.querySelector('a[href="#/work/ENG"]')).toBeNull();
    expect(within(nav).queryByRole("button", { name: /^New task in/ })).toBeNull();
  });

  // SETTINGS IS NEVER HIDDEN; the lock is what changes.
  test("Settings is at the foot for an operator, without the lock, and not current off Settings", async () => {
    const { store, socket } = answering({});
    mountShell(store, socket);
    await settle();
    const foot = document.querySelector<HTMLElement>(".crewlet-app-shell__rail-foot")!;
    const settings = within(foot).getByRole("link", { name: "Settings" });
    expect(settings.getAttribute("title")).toBe("Settings");
    expect(settings.hasAttribute("aria-current")).toBe(false);
    // BEFORE PREFERENCES, in the account row's controls.
    const controls = [
      ...foot.querySelectorAll(
        ".crewlet-account-row__actions a, .crewlet-account-row__actions button",
      ),
    ];
    expect(controls.map((control) => control.getAttribute("aria-label"))).toEqual([
      "Settings",
      "Preferences",
    ]);
  });
});

describe("the page header", () => {
  // A SECTION'S FIGURE IS THE SCREEN'S, and it lands on that section's tab.
  test("a section count a screen publishes is drawn on its tab", async () => {
    function Counting() {
      useSectionCounts({ "asked-of-me": "3", watching: "20+" });
      return <div>my work</div>;
    }
    location.hash = "#/me/watching";
    const { store, socket } = answering({});
    mountShell(store, socket, <Counting />);
    const tabs = screen.getByRole("navigation", { name: "My work sections" });
    const asked = Array.from(tabs.querySelectorAll("a")).find((a) =>
      a.textContent?.startsWith("Asked of me"),
    );
    expect(asked?.textContent).toBe("Asked of me3");
    const current = tabs.querySelector('[aria-current="page"]');
    expect(current?.textContent).toBe("Watching20+");
  });

  // WHOSE DAY IT IS travels between My work's sections; a filter does not.
  test("a section tab carries the workspace's own query and nothing else", async () => {
    location.hash = "#/me?handle=bo&order=priorities";
    const { store, socket } = answering({});
    mountShell(store, socket);
    const tabs = screen.getByRole("navigation", { name: "My work sections" });
    const unblocked = Array.from(tabs.querySelectorAll("a")).find(
      (a) => a.textContent === "Unblocked",
    );
    expect(unblocked?.getAttribute("href")).toBe("#/me/unblocked?handle=bo");
  });

  // THE WINDOW travels between Spend's two rows in the Settings column, and
  // the chart's split does not, because it is the overview's alone.
  test("a Spend row carries the window, and Expensive tasks carries it back", async () => {
    location.hash = "#/settings/spend?window=90d&group=model";
    const { store, socket } = answering({});
    mountShell(store, socket);
    await settle();
    const row = (path: string) =>
      Array.from(
        screen.getByRole("navigation", { name: "Settings sections" }).querySelectorAll("a"),
      ).find((a) => a.getAttribute("href")?.startsWith(path));
    expect(row("#/settings/spend/tasks")?.getAttribute("href")).toBe(
      "#/settings/spend/tasks?window=90d",
    );
    cleanup();
    location.hash = "#/settings/spend/tasks?window=90d";
    const again = answering({});
    mountShell(again.store, again.socket);
    await settle();
    expect(row("#/settings/spend?")?.getAttribute("href")).toBe("#/settings/spend?window=90d");
  });

  // ON AN OBJECT PAGE the crumbs are the way back; a strip of tabs over a
  // task would say it is a fourth kind of list.
  test("no section tabs are drawn on an object's own page", async () => {
    location.hash = "#/work/ENG-42";
    const { store, socket } = answering({});
    mountShell(store, socket);
    expect(document.querySelector(".section-tabs")).toBeNull();
    cleanup();
    location.hash = "#/live/turns/5f0c2a4e-8b1d-4c3a-9e2f-7a6b5c4d3e2f";
    const turn = answering({});
    mountShell(turn.store, turn.socket);
    expect(screen.queryByRole("navigation", { name: "Live sections" })).toBeNull();
    cleanup();
    location.hash = "#/live/turns";
    const again = answering({});
    mountShell(again.store, again.socket);
    expect(screen.getByRole("navigation", { name: "Live sections" })).toBeDefined();
  });

  // SETTINGS DRAWS A COLUMN, with a lock on a section for a reader holding
  // none of its grants — naming the grant — and the section the reader is on
  // marked.
  test("Settings draws its sections as a column, locked where the reader lacks the grant", async () => {
    location.hash = "#/settings/secrets";
    // A READER THE SOCKET ANSWERS — every signed-in principal is one, holding
    // nothing at all: what they lack is the section's own (`sectionOpen`).
    const { store, socket } = answering({
      viewer: {
        login: "reader",
        grants: [],
        handle: "",
        owner: "reader",
        name: "",
        kind: "",
      },
    });
    mountShell(store, socket);
    await settle();
    const column = screen.getByRole("navigation", { name: "Settings sections" });
    const current = column.querySelector('[aria-current="page"]');
    expect(current?.textContent).toContain("Secrets");
    expect(current?.textContent).toContain("Needs Admin");
    const spend = Array.from(column.querySelectorAll("a")).find(
      (a) => a.getAttribute("href") === "#/settings/spend",
    );
    expect(spend?.textContent).not.toContain("Needs");
  });

  // ONE KEY FOR A GROUP THE READER HOLDS NONE OF, on the group's label, the
  // sidebar's rule too: a Tracker-only reader met a key on 13 of 15 rows. A
  // group with an open row keeps the key on each locked row, and every locked
  // row still says why, as its title and after its name.
  test("a Settings group the reader holds none of draws its key once, on its label", async () => {
    location.hash = "#/settings/spend";
    const { store, socket } = answering({
      viewer: { login: "t", grants: ["tracker"], handle: "", owner: "t", name: "", kind: "" },
    });
    mountShell(store, socket);
    await settle();
    const column = screen.getByRole("navigation", { name: "Settings sections" });
    const access = within(column).getByRole("group", { name: "Access, Needs Admin access" });
    expect(access.querySelectorAll("svg.crewlet-locked-label__key")).toHaveLength(1);
    const people = within(access).getByRole("link", { name: /^People, Needs Admin access/ });
    expect(people.querySelector("[title='Needs Admin access']")).not.toBeNull();
    // SPEND HOLDS AN OPEN ROW, so its group is not locked and names no reason.
    const spend = within(column).getByRole("group", { name: "Spend" });
    expect(spend.closest(".crewlet-nav-group--locked")).toBeNull();
  });

  // ON A PHONE THE COLUMN IS ONE ROW NAMING THE SECTION. Stacked whole it put
  // every section's own content ~520px down. The toggle names where the
  // reader is and says whether the list is open, and picking a section (the
  // path moving) folds it again. The folding itself is the phone block's
  // (frame.test.ts holds it); this holds the control a reader operates.
  test("the Settings column folds behind a toggle naming the section", async () => {
    location.hash = "#/settings/secrets";
    const { store, socket } = answering({});
    mountShell(store, socket);
    await settle();
    // BY ITS CLASS: jsdom applies the wide half of the stylesheet, where the
    // toggle is not drawn — the fold is the phone block's, held by frame.test.
    const toggle = document.querySelector<HTMLButtonElement>(".crewlet-section-frame__toggle")!;
    // WHAT IT IS CALLED, its decoration left out — what it SHOWS ("Section ·
    // Secrets") is the picker's own suite's (`PageHeader.test.tsx`).
    const spoken = (el: HTMLElement) => {
      const copy = el.cloneNode(true) as HTMLElement;
      copy.querySelectorAll("[aria-hidden='true']").forEach((n) => n.remove());
      return copy.textContent;
    };
    expect(spoken(toggle)).toBe("Settings Section: Secrets");
    const list = document.getElementById(toggle.getAttribute("aria-controls") ?? "");
    expect(list?.querySelector("nav[aria-label='Settings sections']")).not.toBeNull();
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    expect(toggle.closest(".crewlet-section-frame__nav")?.hasAttribute("data-open")).toBe(false);

    fireEvent.click(toggle);
    expect(toggle.getAttribute("aria-expanded")).toBe("true");
    expect(toggle.closest(".crewlet-section-frame__nav")?.hasAttribute("data-open")).toBe(true);

    await act(async () => {
      location.hash = "#/settings/nodes";
      window.dispatchEvent(new HashChangeEvent("hashchange"));
    });
    await settle();
    const moved = document.querySelector<HTMLButtonElement>(".crewlet-section-frame__toggle")!;
    expect(spoken(moved)).toBe("Settings Section: Nodes");
    expect(moved.getAttribute("aria-expanded"), "a picked section left the list open").toBe(
      "false",
    );
  });

  // AN ADDRESS THAT NAMES NO SCREEN IS IN NO SECTION. Settings owns every
  // address under it, so a mistyped one (or one that moved out, as General
  // did) draws "Not found" beside a column, and a phone picker, marking none.
  test("a Settings address that names no screen marks no section", async () => {
    location.hash = "#/settings/general";
    const { store, socket } = answering({});
    mountShell(store, socket);
    await settle();
    const toggle = document.querySelector<HTMLButtonElement>(".crewlet-section-frame__toggle")!;
    expect(toggle.textContent).toBe("Settings sections");
    expect(document.querySelector(".crewlet-section-frame__list [aria-current]")).toBeNull();
  });

  // NO OPERATOR POLL OUTSIDE SETTINGS. The frame is mounted for the life of
  // the tab, so a figure hook that asked unconditionally would put two
  // operator questions on a timer behind every screen of every tab.
  const OPERATOR_ANSWERS = ["fleet", "retention"];
  test("the operator answers are asked only while the Settings column is drawn", async () => {
    for (const where of ["#/home", "#/work", "#/company/budgets", "#/knowledge"]) {
      location.hash = where;
      const asked: { what: string }[] = [];
      const { store, socket } = answering({}, asked);
      mountShell(store, socket);
      await settle();
      expect(
        asked.map((a) => a.what).filter((w) => OPERATOR_ANSWERS.includes(w)),
        `${where} asked an operator answer`,
      ).toEqual([]);
      cleanup();
    }
    location.hash = "#/settings/models";
    const asked: { what: string }[] = [];
    const { store, socket } = answering({}, asked);
    mountShell(store, socket);
    await settle();
    expect(new Set(asked.map((a) => a.what).filter((w) => OPERATOR_ANSWERS.includes(w)))).toEqual(
      new Set(OPERATOR_ANSWERS),
    );
  });

  test("a reader without the grants is asked nothing, even in Settings", async () => {
    location.hash = "#/settings/models";
    const asked: { what: string }[] = [];
    const { store, socket } = answering(
      { viewer: { login: "", grants: [], handle: "", owner: "", name: "", kind: "" } },
      asked,
    );
    mountShell(store, socket);
    await settle();
    expect(asked.map((a) => a.what).filter((w) => OPERATOR_ANSWERS.includes(w))).toEqual([]);
  });

  // A LOCKED ROW DRAWS NO FIGURE: Nodes and Configuration read the health
  // push every reader holds, and drew "1" and "epoch 1" beside their locks
  // while the equally locked Backups row drew nothing. Mutation: drop the
  // gate and the locked rows count.
  test("a Settings row the reader cannot open draws no figure", async () => {
    location.hash = "#/settings/spend";
    const { store, socket } = answering({
      viewer: { login: "lee", grants: ["tracker"], handle: "", owner: "lee", name: "", kind: "" },
    });
    mountShell(store, socket);
    act(() => store.applyHealth(healthFrame({ applied_epoch: 2, nodes: 1 })));
    await settle();
    const column = screen.getByRole("navigation", { name: "Settings sections" });
    const row = (start: string) =>
      Array.from(column.querySelectorAll("a")).find((a) => a.textContent?.startsWith(start));
    expect(row("Nodes")?.textContent).toMatch(/Needs Admin/);
    expect(row("Nodes")?.textContent).not.toMatch(/node live/);
    expect(row("Configuration")?.textContent).not.toMatch(/epoch/);
  });

  // A FIGURE SAYS WHAT IT COUNTS: the applied epoch read "e2", a code.
  test("the Configuration row names the epoch this node applied in words", async () => {
    location.hash = "#/settings/models";
    const { store, socket } = answering({});
    mountShell(store, socket);
    act(() => store.applyHealth(healthFrame({ applied_epoch: 2, nodes: 1 })));
    await settle();
    const column = screen.getByRole("navigation", { name: "Settings sections" });
    const config = Array.from(column.querySelectorAll("a")).find((a) =>
      a.textContent?.startsWith("Configuration"),
    );
    expect(config?.textContent).toContain("epoch 2");
    expect(config?.textContent).not.toMatch(/\be2\b/);
  });

  // ONE READING OF THE FLEET, NOT TWO. On Nodes the column's "n behind on
  // config" mirrors the screen's own "Behind on config" tile; polling beside
  // the screen on another clock let the two disagree for up to a poll and
  // asked the engine the same question twice a tick.
  test("while a screen publishes the fleet answer the column draws it and asks none", async () => {
    const answer: FleetAnswer = {
      nodes: [
        { id: "n1", roles: [], broker: "member", seats: 1, config_epoch: 3 },
        { id: "n2", roles: [], broker: "member", seats: 1, config_epoch: 2 },
      ],
      seats: [],
      duties: [],
      unplaceable: [],
      unmanned_roles: [],
      this_node: "n1",
      target_epoch: 3,
    };
    function Publishing() {
      usePublishFleet(answer);
      return null;
    }
    location.hash = "#/settings/nodes";
    const asked: { what: string }[] = [];
    const { store, socket } = answering({}, asked);
    mountShell(store, socket, <Publishing />);
    act(() => store.applyHealth(healthFrame({ applied_epoch: 3, nodes: 2 })));
    await settle();
    expect(asked.filter((a) => a.what === "fleet")).toEqual([]);
    const column = screen.getByRole("navigation", { name: "Settings sections" });
    const nodes = Array.from(column.querySelectorAll("a")).find((a) =>
      a.textContent?.startsWith("Nodes"),
    );
    expect(nodes?.textContent).toContain("1 behind on config");
  });
});

// A MODAL DOES NOT OUTLIVE THE SCREEN IT WAS OPENED ON.
//
// `Shell` is mounted once for the life of the tab, so what it holds open stays
// open across every route change unless something takes it back. The drawer
// already did; the palette did not — press `Ctrl-K`, then Back, and it was
// still there over a different screen, still offering the objects it had
// ranked for the one the reader had just left.
//
// Picking a palette row closes it on the way out, so what this covers is every
// OTHER way the route moves while it is open: Back, Forward, a phone's back
// gesture, a restored history entry.
describe("what the frame holds open", () => {
  test("a route change closes the palette, the way the kit closes the drawer", async () => {
    frame(<Bare />);
    await act(async () => {
      window.dispatchEvent(new KeyboardEvent("keydown", { key: "k", ctrlKey: true }));
    });
    // FOUND, NOT QUERIED: the palette's code is a chunk of its own, so the
    // dialog draws once that chunk is in.
    expect(
      await screen.findByRole("dialog"),
      "ctrl-k did not open the palette, so this test proves nothing",
    ).not.toBeNull();

    await act(async () => {
      location.hash = "#/work";
      window.dispatchEvent(new HashChangeEvent("hashchange"));
    });
    expect(
      screen.queryByRole("dialog"),
      "the palette survived a route change and is now over a screen it knows nothing about",
    ).toBeNull();
  });
});

// THE COMPANY IS THE LOCKUP'S TITLE, beside the product's mark — the approved
// design's "Nimbus" — and the lockup is the way home.
test("the lockup is the company's name beside the product's mark, and goes home", async () => {
  const { store, socket } = answering({});
  mountShell(store, socket);
  const home = () =>
    document.querySelector<HTMLAnchorElement>(".crewlet-app-shell a[href='#/home']");
  // Before the company is known the product stands in.
  expect(home()?.textContent).toContain("Crewlet");
  act(() => store.applyOrg(withDerived({ name: "Nimbus", roles: [], units: [] })));
  expect(home()?.textContent).toContain("Nimbus");
  expect(home()?.textContent).not.toContain("Crewlet");
  expect(home()?.querySelector("img")?.getAttribute("src")).toBe(
    "/static/dashboard/crewlet-icon.svg",
  );
});

// THE ENGINE'S STATE IS THE ENGINE ROW'S NAME, at the rail's foot, and the row
// opens the engine panel. Every word is the health push's: `EnginePanel.test.tsx`
// holds the precedence and the panel, this holds the wiring.
test("the engine row reads the push and opens the engine panel", async () => {
  const { store, socket } = answering({});
  mountShell(store, socket);
  act(() => {
    store.setConnected(true);
    store.applyHealth(healthFrame({ nodes: 3, applied_epoch: 42, alarms: { count: 0 } }));
  });
  const row = screen.getByRole("button", { name: "Engine healthy" });
  expect(row.closest("aside")).not.toBeNull();
  fireEvent.click(row);
  const panel = screen.getByRole("dialog", { name: "Engine" });
  expect(within(panel).getByText("3 live")).toBeDefined();
  expect(within(panel).getByText("Epoch 42")).toBeDefined();
});

// THE ALARMS' NAMES ARE THE RETENTION REPORT'S, which takes Admin,
// so the panel asks for them only where Settings › Backups & retention, the
// section that lists them, opens for the reader: decided by the rule every
// lock is drawn by, over the frame's one reading of what the reader holds. A
// reader without it is told the longest-standing alarm off the push, and the
// engine is never asked a question it would refuse. Mutation: hand the panel
// `readsReport` unconditionally and the second reader asks.
test("the engine panel asks the alarms' names only of a reader the report's section opens for", async () => {
  const report = { domains: [], alarms: [{ kind: "backup_age", detail: "", remedy: "" }] };
  const holdsNothingButTheCompany = {
    login: "reader",
    grants: [],
    handle: "ada",
    owner: "ada",
    name: "Ada Lovelace",
    kind: "human",
  };
  for (const [viewer, asks] of [
    [undefined, true],
    [holdsNothingButTheCompany, false],
  ] as const) {
    const asked: { what: string }[] = [];
    const { store, socket } = answering(
      viewer ? { viewer, retention: report } : { retention: report },
      asked,
    );
    mountShell(store, socket);
    await settle();
    act(() => {
      store.setConnected(true);
      store.applyHealth(healthFrame({ alarms: { count: 1, worst: "backup_age" } }));
    });
    fireEvent.click(screen.getByRole("button", { name: "Engine serving, 1 alarm" }));
    await settle();
    expect(
      asked.some((a) => a.what === "retention"),
      viewer ? "a reader without Admin" : "a reader holding Admin",
    ).toBe(asks);
    cleanup();
  }
});

// A PROJECT'S LENSES ARE BESIDE ITS NAME, on the bar: `Work › ENG Core ·
// Tasks | About | History | Files`. As a row of their own under it they cost the
// first screenful a line the approved Board does not spend. And the row on
// the bar still controls the region in the page, by id.
test("an object's lenses sit on the page bar beside the breadcrumb and control the page", async () => {
  location.hash = "#/work/ENG?lens=about";
  const eng = {
    key: "ENG",
    name: "Core platform",
    unit: { resolved: true, name: "Core" },
    lead: {},
    task_counts: { todo: 2, active: 1, done: 0, closed: 0 },
  };
  const { store, socket } = answering({
    work_projects: { projects: [eng], complete: true },
    work_project: { ...eng, statuses: [], types: [], fields: [], complete: true },
    work_items: { items: [], groups: [], complete: true },
    work_activity: { records: [], complete: true },
  });
  mountShell(store, socket, <Project projectKey="ENG" />);
  const lenses = await waitFor(() => screen.getByRole("tablist", { name: "Lens" }));
  const slot = document.getElementById(PAGE_LENSES_SLOT);
  expect(slot?.contains(lenses)).toBe(true);
  // IMMEDIATELY AFTER THE TRAIL, before the controls.
  expect(slot?.previousElementSibling?.getAttribute("aria-label")).toBe("Breadcrumb");
  expect(slot?.nextElementSibling?.classList.contains("page-controls")).toBe(true);
  // THE ROW CONTROLS THE PANEL IN THE PAGE.
  const about = screen.getByRole("tab", { name: "About" });
  expect(about.getAttribute("aria-selected")).toBe("true");
  const panel = document.getElementById(about.getAttribute("aria-controls") ?? "");
  expect(panel?.getAttribute("role")).toBe("tabpanel");
  expect(panel?.closest("main")).toBeTruthy();
  // AND A PAGE WITH NO LENSES LEAVES THE SLOT EMPTY, which draws nothing.
  cleanup();
  location.hash = "#/work/views";
  const bare = answering({});
  mountShell(bare.store, bare.socket);
  expect(document.getElementById(PAGE_LENSES_SLOT)?.childElementCount).toBe(0);
});

// A SCREEN'S CONTROLS SIT AS FAR APART AS THE FRAME'S. The slot a screen
// portals its controls into was `gap-1` (4px) beside the frame's own at the
// kit's control spacing, so My work's "Whose day" select touched "Inbox →".
test("the page header's controls slot spaces a screen's controls at the kit's control gap", () => {
  const { store, socket } = answering({});
  mountShell(store, socket);
  const slot = document.getElementById(PAGE_ACTIONS_SLOT);
  expect(slot?.classList.contains("gap-2")).toBe(true);
  expect(slot?.classList.contains("gap-1")).toBe(false);
  // AND THEY END THE BAR: a screen's primary action is the last control, after
  // the frame's star and link, as the approved designs order it.
  expect(slot?.parentElement?.lastElementChild).toBe(slot);
});

// WHERE THE PEEK IS A COLUMN IS A WIDTH PER FRAME, not one per window.
//
// A media query in the stylesheet had one number for every screen, and it was
// short twice: it left out the screen's padding and the scroller's gutter, so
// the Work list beside a peek was 396px at the width that promised it 444; and
// Settings draws a 236px section column INSIDE the screen, so at 1280 a peek
// there left the nodes grid 328px and cut its columns off. The shell asks for
// the width that is right for the frame on screen now, at the reader's
// density, and writes ONE attribute.
describe("where the peek is a column", () => {
  async function peekAt(hash: string, width: number) {
    const win = installWindow(width);
    location.hash = hash;
    const { store, socket } = answering({ fleet: { nodes: [] } });
    mountShell(store, socket);
    await settle();
    const app = document.querySelector<HTMLElement>(".app");
    expect(app, "the shell drew no root").not.toBeNull();
    return { win, app: app! };
  }

  afterEach(() => {
    setDensity("normal");
  });

  test("beside a screen, from the width that leaves its list the floor", async () => {
    const { win, app } = await peekAt("#/work?peek=seat:ceo", 1159);
    try {
      expect(win.asked).toContain("(width >= 1160px)");
      expect(app.getAttribute("data-peek")).toBe("drawer");
      win.set(1160);
      expect(app.getAttribute("data-peek")).toBe("column");
      // The track's cap: the gutter, the padding and the list's floor.
      expect(app.style.getPropertyValue("--peek-reserve")).toBe("494px");
    } finally {
      win.restore();
    }
  });

  test("beside Settings' section column, a column's width later", async () => {
    const { win, app } = await peekAt("#/settings/nodes?peek=node:n1", 1280);
    try {
      expect(win.asked).toContain("(width >= 1396px)");
      // THE WIDTH THAT SHIPPED BROKEN: a drawer now, over a grid that keeps
      // every one of its columns.
      expect(app.getAttribute("data-peek")).toBe("drawer");
      win.set(1396);
      expect(app.getAttribute("data-peek")).toBe("column");
      expect(app.style.getPropertyValue("--peek-reserve")).toBe("730px");
    } finally {
      win.restore();
    }
  });

  // THE PADDING AND THE INSET SCALE WITH THE DENSITY, which no media query
  // can read — so the width asked moves with the reader's choice.
  test("at the reader's density", async () => {
    setDensity("comfortable");
    const { win, app } = await peekAt("#/work?peek=seat:ceo", 1167);
    try {
      expect(win.asked).toContain("(width >= 1168px)");
      expect(app.getAttribute("data-peek")).toBe("drawer");
      win.set(1168);
      expect(app.getAttribute("data-peek")).toBe("column");
    } finally {
      win.restore();
    }
  });

  // A PHONE IS ONE PANE: the widest phone is a drawer for a peek, as every
  // window under the column's width is, and the sidebar is its drawer too.
  test("on a phone a peek is a drawer, and so is the sidebar", async () => {
    const { win, app } = await peekAt("#/work?peek=seat:ceo", 639);
    try {
      expect(app.getAttribute("data-peek")).toBe("drawer");
      const sidebar = document.querySelector(".crewlet-app-shell__rail");
      expect(sidebar?.getAttribute("data-open")).not.toBe("true");
      act(() => {
        window.dispatchEvent(new KeyboardEvent("keydown", { key: "\\", ctrlKey: true }));
      });
      expect(sidebar?.getAttribute("data-open")).toBe("true");
    } finally {
      win.restore();
    }
  });

  test("with no peek open, the sheet is one column at every width", async () => {
    const { win, app } = await peekAt("#/work", 1600);
    try {
      expect(app.hasAttribute("data-peek")).toBe(false);
      expect(app.style.getPropertyValue("--peek-reserve")).toBe("");
    } finally {
      win.restore();
    }
  });
});

// BELOW THE SHELL BREAKPOINT THE SIDEBAR IS A DRAWER, and the keyboard reaches
// it: Mod+\ opens it, and above the breakpoint — where the sidebar is always
// on screen — the chord does nothing at all.
describe("the drawer's key", () => {
  function press(): void {
    act(() => {
      window.dispatchEvent(new KeyboardEvent("keydown", { key: "\\", ctrlKey: true }));
    });
  }

  // AT THE BREAKPOINT'S OWN EDGE, both sides: 1023 is the widest drawer and
  // 1024 the narrowest window the sidebar is always on screen in.
  test("Mod+\\ opens the sidebar's drawer below the shell breakpoint", async () => {
    const win = installWindow(1023);
    try {
      const { store, socket } = answering({});
      mountShell(store, socket);
      await settle();
      const sidebar = document.querySelector(".crewlet-app-shell__rail");
      expect(sidebar?.getAttribute("data-open")).not.toBe("true");
      press();
      expect(sidebar?.getAttribute("data-open")).toBe("true");
    } finally {
      win.restore();
    }
  });

  test("and does nothing where the sidebar is already on screen", async () => {
    const win = installWindow(1024);
    try {
      const { store, socket } = answering({});
      mountShell(store, socket);
      await settle();
      press();
      expect(
        document.querySelector(".crewlet-app-shell__rail")?.getAttribute("data-open"),
      ).not.toBe("true");
    } finally {
      win.restore();
    }
  });
});

// THE FRAME'S KEYS, read from `keymap.ts`. `/` was bound to the palette on
// every screen, so on a screen with a search box of its own the key a reader
// pressed to search what they were looking at took them away from it.
describe("the frame's keys", () => {
  function key(key: string, init: KeyboardEventInit = {}): void {
    act(() => {
      window.dispatchEvent(
        new KeyboardEvent("keydown", { key, bubbles: true, cancelable: true, ...init }),
      );
    });
  }

  function Searchable() {
    const box = useRef<HTMLInputElement>(null);
    useSearchTarget(box);
    return <input aria-label="the screen's own search" ref={box} />;
  }

  test("/ focuses the screen's own search, and opens nothing over it", () => {
    frame(<Searchable />);
    key("/");
    expect(document.activeElement).toBe(screen.getByLabelText("the screen's own search"));
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  test("/ opens the palette on a screen with no search of its own", () => {
    frame(<Bare />);
    key("/");
    expect(screen.queryByRole("dialog", { name: "Search" })).not.toBeNull();
  });

  test("the chord that opened the palette closes it", async () => {
    frame(<Bare />);
    key("k", { ctrlKey: true });
    const palette = screen.getByRole("dialog", { name: "Search" });
    // From inside its own field, where the reader is when they press it.
    await act(async () => {
      palette.querySelector("input")!.dispatchEvent(
        new KeyboardEvent("keydown", {
          key: "k",
          ctrlKey: true,
          bubbles: true,
          cancelable: true,
        }),
      );
    });
    expect(screen.queryByRole("dialog", { name: "Search" })).toBeNull();
  });

  test("? shows the legend, and so does the palette's row for it", async () => {
    frame(<Bare />);
    key("?", { shiftKey: true });
    expect(screen.getByRole("dialog", { name: "Keyboard shortcuts" })).not.toBeNull();
    act(() => screen.getByRole("button", { name: /close/i }).click());
    expect(screen.queryByRole("dialog", { name: "Keyboard shortcuts" })).toBeNull();

    key("k", { ctrlKey: true });
    const input = screen.getByRole("dialog", { name: "Search" }).querySelector("input")!;
    await act(async () => {
      fireEvent.change(input, { target: { value: ">keyboard" } });
    });
    await act(async () => {
      fireEvent.keyDown(input, { key: "Enter" });
    });
    expect(screen.queryByRole("dialog", { name: "Search" })).toBeNull();
    expect(screen.getByRole("dialog", { name: "Keyboard shortcuts" })).not.toBeNull();
  });

  test("g then a letter goes to that workspace", () => {
    frame(<Bare />);
    key("g");
    key("w");
    expect(location.hash).toBe("#/work");
  });
});
