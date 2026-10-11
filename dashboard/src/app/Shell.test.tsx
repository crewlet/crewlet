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

import { useRef, type ReactNode } from "react";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useSearchTarget } from "./searchTarget.ts";
import { afterEach, beforeAll, beforeEach, describe, expect, test } from "vitest";
import { Shell } from "./Shell.tsx";
import { usePageCoverage, useSectionCounts } from "./page.tsx";
import { Router, buildHash } from "./router.tsx";
import { CHUNKS, loadChunk } from "./lazyScreen.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { CoverageFacts } from "~/components/work.tsx";
import { setDensity } from "~/lib/prefs.ts";
import { Overview } from "~/routes/admin/overview/Overview.tsx";
import { Inbox } from "~/routes/inbox/Inbox.tsx";
import { installWindow } from "~/testing.tsx";
import { PAGE_ACTIONS_SLOT, PAGE_LENSES_SLOT } from "./frame/PageActions.tsx";
import { Project } from "~/routes/work/Project.tsx";
import { healthFrame } from "~/test/health.ts";
import { emptyDay } from "~/test/myWork.ts";
import { withDerived } from "~/test/org.ts";
import { ZERO_VERSIONS } from "~/test/liveCall.ts";
import { rosterOf } from "~/test/seats.ts";

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

// EVERY CHUNK'S CODE IS IN BEFORE A CASE STARTS — the console's sidebar and
// door, the New task sheet, the screens — for the reason `App.test.tsx` gives:
// what these cases assert is what the frame draws, not how long a cold
// `import()` takes under a test transformer. The New task case used to wait a
// second for the sheet's chunk to arrive cold, and on a busy machine the
// transform alone took longer — a flake that measured the runner, not the
// frame.
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
    expect(screen.getByText("This answer is incomplete")).toBeDefined();
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
  // a screen that does not — Connect › Secrets, say — and the inbox's
  // freshness badge and its "this answer is incomplete" banner stayed in the
  // bar as claims about data the new screen never read.
  test("it goes when the screen that published it does", () => {
    const { show } = frame(<Covered coverage={INCOMPLETE} />);
    show(<Bare />);
    expect(screen.getByText("the bare screen")).toBeDefined();
    expect(screen.queryByText(/applied through/)).toBeNull();
    expect(screen.queryByText("This answer is incomplete")).toBeNull();
  });

  // …and the reset must not eat the INCOMING screen's own facts, which is why
  // it is a cleanup on the hook rather than an effect in the Shell keyed on
  // the route: React flushes a child's effects before its parent's, so a
  // Shell-level reset would run after the new screen had already published.
  test("a screen that publishes its own replaces it rather than losing it", async () => {
    const { show } = frame(<Covered coverage={INCOMPLETE} />);
    show(
      <AlsoCovered
        coverage={{ read_level: "stale", complete: true, log_seq: 9, applied_through: 4 }}
      />,
    );
    // FLUSHED, because a reset that lands LATE is the failure being excluded:
    // a deferred one — a microtask, a timeout, a parent effect — runs after
    // the incoming screen has already published and blanks it, and a
    // synchronous assertion cannot see that happen.
    await act(async () => {});
    expect(screen.getByText(/applied through 4 of 9/)).toBeDefined();
    expect(screen.queryByText(/applied through 41 of 88/)).toBeNull();
    expect(screen.queryByText("This answer is incomplete")).toBeNull();
  });
});

/**
 * The engine's own empty answers for the reads every mounted shell makes, so a
 * case that is about something else is not answered a shape the engine never
 * sends.
 */
const EMPTY: Record<string, unknown> = {
  work_inbox: { handle: "ada", notices: [], primary_reasons: [] },
  work_views: { complete: true, views: [] },
  work_saved_views: { complete: true, views: [] },
  work_projects: { projects: [] },
  work_my_work: emptyDay("ada"),
  fleet: { nodes: [], seats: [], duties: [], target_epoch: 0 },
  retention: { domains: [], nodes: [], snapshots: [], alarms: [], register_readable: true },
  integrations: { integrations: [], tools: [], traffic_known: true, traffic_since: null },
};

/** A socket answering a bound viewer and whatever else a case supplies. */
function answering(
  answers: Record<string, unknown>,
  asked: { what: string; params?: Record<string, unknown> }[] = [],
) {
  const store = new Store();
  const socket = new LiveSocket(store);
  (
    socket as unknown as {
      query: (what: string, params?: Record<string, unknown>) => Promise<unknown>;
    }
  ).query = (what: string, params?: Record<string, unknown>) => {
    asked.push({ what, params });
    if (what === "viewer") {
      return Promise.resolve(
        answers.viewer ?? {
          token_id: "U0FOUNDER",
          role: "admin",
          reach: "admin",
          linked: true,
          handle: "ada",
          name: "Ada Lovelace",
          kind: "human",
        },
      );
    }
    const answer = answers[what] ?? EMPTY[what] ?? {};
    // A FUNCTION ANSWERS BY THE PARAMETERS, for a case whose screen and frame
    // put two different questions to one query.
    return Promise.resolve(
      typeof answer === "function"
        ? (answer as (params?: Record<string, unknown>) => unknown)(params)
        : answer,
    );
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

/** Let the queries the first render made answer, and the renders they cause land. */
async function settle(): Promise<void> {
  await act(async () => {
    for (let i = 0; i < 5; i++) await Promise.resolve();
  });
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
      { work_inbox: { handle: "ada", notices: [notice("assignee", 1)], primary_reasons: [] } },
      asked,
    );
    mountShell(store, socket);
    await settle();
    const inbox = asked.find((a) => a.what === "work_inbox");
    expect(inbox?.params).toMatchObject({ handle: "ada", unread: true, primary_only: true });
    const row = screen.getByRole("link", { name: /^Inbox/ });
    expect(row.textContent).toContain("1");
  });

  // A PAGE THE ENGINE HAD MORE THAN is a floor, and says so.
  test("a page with more behind it is drawn as a floor", async () => {
    const { store, socket } = answering({
      work_inbox: {
        handle: "ada",
        notices: [notice("assignee", 1), notice("mention", 2)],
        primary_reasons: [],
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
      work_inbox: { handle: "ada", notices: [], primary_reasons: [] },
    });
    mountShell(store, socket);
    await settle();
    expect(screen.getByRole("link", { name: /^Inbox/ }).textContent).toBe("Inbox");
  });
});

describe("the sidebars' figures", () => {
  // THE ENGINE'S WORD, and only it: two seats working, one idle, one parked on
  // a question — the count is two, whatever a live call says. AND IT IS ON
  // LIVE, in the console, which holds the running turns.
  test("the Live count is the seats the engine calls working", async () => {
    location.hash = "#/admin";
    const { store, socket } = answering({});
    mountShell(store, socket);
    act(() =>
      rosterOf(store, [
        { role: "A", activity: "working" },
        { role: "B", activity: "working" },
        {
          role: "C",
          activity: "idle",
          live_call: { in_progress: true, versions: ZERO_VERSIONS } as never,
        },
        { role: "D", activity: "needs" },
      ]),
    );
    expect(screen.getByRole("link", { name: /^Live/ }).textContent).toContain("2");
  });

  // ---------------------------------------------------------------------------
  // The running turns, from every screen
  // ---------------------------------------------------------------------------

  /** A seat the engine calls working, on `turn`, started at minute `at`. */
  function workingSeat(role: string, turn: string, at: number, item = "") {
    return {
      role,
      handle: role.toLowerCase(),
      activity: "working",
      turn: {
        turn_id: turn,
        started_at: `2026-09-21T10:${String(at).padStart(2, "0")}:00Z`,
        stage: "phase",
        ...(item ? { work_item: { backend: "native", id: item, key: item, project: "ENG" } } : {}),
      },
    };
  }

  /** The console sidebar's Running group: its rows, as each one reads and where it goes. */
  function runningRows() {
    const nav = screen.getByRole("navigation", { name: "Admin console" });
    const heading = within(nav).queryByText("Running", { exact: true });
    if (!heading) return [];
    const group = heading.closest(".crewlet-nav-group") as HTMLElement;
    return [...group.querySelectorAll("a")].map((a) => ({
      text: a.textContent ?? "",
      href: a.getAttribute("href") ?? "",
    }));
  }

  const watching = (turn: string) =>
    buildHash(["admin", "live", "turns", turn], { tab: "transcript" });

  // EACH RUNNING TURN IS A ROW THAT WATCHES IT, from any console screen: the
  // reader looking for one went Agents, the chart, a card, the profile and a
  // tab. OLDEST FIRST, so the turn that starts is appended and nothing a reader
  // is reaching for moves; a seat waiting on a person runs nothing and is not
  // here.
  test("the Running group is a row per working seat, oldest first, each watching its turn", async () => {
    location.hash = "#/admin";
    const { store, socket } = answering({});
    mountShell(store, socket);
    expect(runningRows(), "no group while nothing runs").toEqual([]);
    act(() =>
      rosterOf(store, [
        workingSeat("CTO", "t-late", 9),
        workingSeat("SWE", "t-early", 1, "ENG-412"),
        { role: "PM", handle: "pm", activity: "needs" },
        { role: "QA", handle: "qa", activity: "idle" },
        { role: "Ops", handle: "ops", activity: "idle" },
      ] as never),
    );
    const rows = runningRows();
    expect(rows.map((r) => r.href)).toEqual([watching("t-early"), watching("t-late")]);
    // THE SEAT'S NAME, AND THE ITEM ITS TURN IS CHARGED TO as the row's lead.
    expect(rows[0]!.text).toContain("SWE");
    expect(rows[0]!.text).toContain("ENG-412");
    expect(rows[1]!.text).toContain("CTO");
    // A TURN THAT STARTS IS APPENDED; the rows above it hold their places.
    act(() => store.applyAgents([workingSeat("Ops", "t-new", 20)] as never));
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
    location.hash = "#/admin";
    const { store, socket } = answering({});
    mountShell(store, socket);
    act(() =>
      rosterOf(store, [
        workingSeat("A", "t-a", 1),
        { role: "Fresh", handle: "fresh", activity: "working", turn: null, live_call: null },
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
      "#/admin/live",
    ]);
    expect(rows[4]!.text).toContain("2 more running");
    // AND THE COUNT ON LIVE IS THE WHOLE SET, the group's five rows' worth.
    expect(screen.getByRole("link", { name: /^Live/ }).textContent).toContain("6");
  });

  // THE OVERVIEW'S LIVE NOW AND THE RUNNING GROUP NAME THE SAME SEATS IN THE
  // SAME ORDER, on one screen, over one roster. They were two selections under
  // one cap: the seat with no turn id sorted FIRST (its start read as 0) and
  // the landing screen drew it while the sidebar skipped it, so the two lists
  // differed by two seats beside each other.
  test("the Overview's Live now names the seats the Running group names, in its order", async () => {
    location.hash = "#/admin";
    const { store, socket } = answering({
      work_workload: { rows: [] },
      sandbox_runs: { runs: [] },
      work_projects: { projects: [], complete: true },
    });
    mountShell(store, socket, <Overview key="overview" />);
    act(() =>
      rosterOf(store, [
        workingSeat("A", "t-a", 1),
        { role: "Fresh", handle: "fresh", activity: "working", turn: null, live_call: null },
        workingSeat("B", "t-b", 2),
        workingSeat("C", "t-c", 3),
        workingSeat("D", "t-d", 4),
        workingSeat("E", "t-e", 5),
      ] as never),
    );
    await settle();
    const turnOf = (hash: string) => /#\/admin\/live\/turns\/([^?]+)/.exec(hash)?.[1] ?? "";
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
          role,
          handle: role.toLowerCase(),
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
    location.hash = "#/admin";
    const { store, socket } = answering({});
    mountShell(store, socket);
    act(() =>
      rosterOf(store, [
        { role: "P", handle: "p", activity: "working", turn: null, live_call: null },
        { role: "Q", handle: "q", activity: "working", turn: null, live_call: null },
      ] as never),
    );
    const rows = runningRows();
    expect(rows.map((r) => r.href)).toEqual(["#/admin/live"]);
    expect(rows[0]!.text).toContain("2 running");
    expect(rows[0]!.text).not.toContain("more");
  });

  // THE ONE ROW THE READER IS WATCHING is the current one, as every sidebar row
  // is the screen the reader is on.
  test("the running turn the reader is watching is the current row", async () => {
    location.hash = watching("t-b");
    const { store, socket } = answering({});
    mountShell(store, socket);
    act(() => rosterOf(store, [workingSeat("A", "t-a", 1), workingSeat("B", "t-b", 2)] as never));
    const nav = screen.getByRole("navigation", { name: "Admin console" });
    const current = [...nav.querySelectorAll('a[aria-current="page"]')].map((a) =>
      a.getAttribute("href"),
    );
    expect(current).toContain(watching("t-b"));
    expect(current).not.toContain(watching("t-a"));
  });

  // `g r` IS "TAKE ME TO THE RUNNING TURN": the one turn's watch link when one
  // seat is working, and Now running — every one of them — when several are,
  // or none. An admin's key, so it waits for the viewer to say so.
  test("g then r watches the one running turn, and opens Now running otherwise", async () => {
    const { store, socket } = answering({});
    mountShell(store, socket);
    await settle();
    const chord = () =>
      act(() => {
        window.dispatchEvent(new KeyboardEvent("keydown", { key: "g", bubbles: true }));
        window.dispatchEvent(new KeyboardEvent("keydown", { key: "r", bubbles: true }));
      });
    act(() =>
      rosterOf(store, [
        { role: "A", activity: "idle" },
        { role: "B", activity: "idle" },
      ]),
    );
    chord();
    expect(location.hash).toBe("#/admin/live");
    location.hash = "#/";
    act(() => store.applyAgents([workingSeat("A", "t-a", 1)] as never));
    chord();
    expect(location.hash).toBe(watching("t-a"));
    location.hash = "#/";
    act(() => store.applyAgents([workingSeat("B", "t-b", 2)] as never));
    chord();
    expect(location.hash).toBe("#/admin/live");
  });

  // A PIN IS A PERSON'S, so every saved view is asked for WITH the viewer —
  // the shared views carry no pins, which is why the sidebar this replaced
  // never drew one — across every container, and with the engine's own counts.
  test("pinned views are asked for as the viewer, and draw the view's own total", async () => {
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
    expect(views?.params).toMatchObject({ viewer: "ada", counts: true });
    // NOT ONE CONTAINER'S STRIP: a pin made on a project board lives there.
    expect(views?.params).not.toHaveProperty("container");
    // A PIN RUNS THE VIEW: the list with the view's key as `view=`, not the
    // inventory page that describes the view and offers to run it.
    const pin = screen.getByRole("link", { name: /^Blocked, org-wide/ });
    expect(pin.getAttribute("href")).toBe("#/work?view=blocked");
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

  // MY WORK AND WORK CARRY NO FIGURE. What is on a person is their Queue's
  // tab, counted where it is drawn; a count beside a row on every screen is a
  // mark a reader learns to skip — and a reading the frame held for it was a
  // poll on every screen for a number only My work draws.
  test("My work and Work carry no figure, and the frame counts no queue", async () => {
    const asked: { what: string; params?: Record<string, unknown> }[] = [];
    const { store, socket } = answering(
      { work_items: { items: [], groups: [], total_hint: 3, complete: true } },
      asked,
    );
    mountShell(store, socket);
    await settle();
    expect(screen.getByRole("link", { name: /^My work/ }).textContent).toBe("My work");
    expect(
      asked.some((a) => a.what === "work_items" && a.params?.limit === 1),
      "the frame asked for a queue count nothing draws",
    ).toBe(false);
    expect(asked.some((a) => a.what === "work_my_work")).toBe(false);
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
  // Home asked who the viewer is a second time, and a screen that asks the
  // badge's question for itself polls it on its own minute, so the badge and
  // the figure beside it come from two reads and disagree for most of one.
  // The Inbox asks a question of its own — its Unread list, every unread
  // notice — which is not this one, and is not counted here.
  test.each([
    ["the Overview", "#/admin", <Overview key="overview" />],
    ["the Inbox", "#/inbox", <Inbox key="inbox" />],
  ])(
    "%s in the frame asks again neither who the viewer is nor what waits on them",
    async (_, hash, child) => {
      const asked: { what: string; params?: Record<string, unknown> }[] = [];
      location.hash = hash;
      const { store, socket } = answering(
        { work_workload: { rows: [] }, sandbox_runs: { runs: [] } },
        asked,
      );
      mountShell(store, socket, child);
      await settle();
      expect(asked.filter((a) => a.what === "viewer")).toHaveLength(1);
      expect(
        asked.filter((a) => a.what === "work_inbox" && a.params?.primary_only === true),
      ).toHaveLength(1);
    },
  );

  // TWO FIGURES ON THE INBOX, AND NEVER ONE NAME FOR BOTH. Unread lists every
  // unread notice and the badge counts only those waiting on the reader, so
  // the screen the badge opens said "Unread 3+" under a badge saying 2 with
  // nothing to tell the two apart. The screen says the badge's figure as well,
  // from the frame's one reading and in the badge's own words — and the page
  // here is full of notices that merely informed, which is exactly where a
  // count of the page's own primary notices would have said nothing at all.
  test("on the Inbox the badge's figure is said again, in the badge's words", async () => {
    const asked: { what: string; params?: Record<string, unknown> }[] = [];
    location.hash = "#/inbox";
    const informed = (n: number) => ({ ...notice("watcher", n), primary: false });
    const { store, socket } = answering(
      {
        work_workload: { rows: [] },
        sandbox_runs: { runs: [] },
        work_inbox: (params?: Record<string, unknown>) =>
          params?.primary_only
            ? {
                handle: "ada",
                notices: [notice("mention", 7), notice("assignee", 8)],
                primary_reasons: [],
              }
            : {
                handle: "ada",
                notices: [informed(1), informed(2), informed(3)],
                primary_reasons: [],
                next_cursor: "c-3",
              },
      },
      asked,
    );
    mountShell(store, socket, <Inbox key="inbox" />);
    await settle();
    const badge = screen.getByRole("link", { name: /^Inbox.*2 notices waiting on you/ });
    expect(badge.textContent).toContain("2");
    expect(screen.getByRole("radio", { name: /Unread/ }).textContent).toContain("3+");
    expect(screen.getByText("2 notices waiting on you")).toBeTruthy();
    expect(
      asked.filter((a) => a.what === "work_inbox" && a.params?.primary_only === true),
    ).toHaveLength(1);
  });

  // EVERY "NEW TASK" OPENS THE FRAME'S ONE SHEET: the Overview's in its page
  // bar, the person sidebar's Hand off and its key — not a second form each,
  // and not the palette it stood in for until the sheet landed. The sheet's
  // chunk is already in (see `beforeAll`), so what is waited for here is the
  // render, never a cold import.
  test.each([
    [
      "the Overview's New task",
      "#/admin",
      () => fireEvent.click(screen.getByRole("button", { name: /^New task$/ })),
    ],
    [
      "the sidebar's Hand off",
      "#/today",
      () => fireEvent.click(screen.getByRole("button", { name: /^Hand off/ })),
    ],
    [
      "the hand-off key",
      "#/today",
      () => window.dispatchEvent(new KeyboardEvent("keydown", { key: "c", bubbles: true })),
    ],
  ] as const)("%s opens the frame's New task sheet", async (_who, hash, press) => {
    location.hash = hash;
    const { store, socket } = answering({
      work_workload: { rows: [] },
      sandbox_runs: { runs: [] },
      work_projects: { projects: [], complete: true },
    });
    mountShell(store, socket, hash === "#/admin" ? <Overview key="overview" /> : <Bare />);
    await settle();
    await act(async () => {
      press();
    });
    expect(screen.getByRole("dialog", { name: "New task" })).toBeTruthy();
  });

  // HAND OFF FILES INTO THE PROJECT THE READER IS IN, which the sheet shows as
  // its chosen project before anything is sent.
  test("Hand off opens the sheet on the project the reader is in", async () => {
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
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: /^Hand off/ }));
    });
    const sheet = screen.getByRole("dialog", { name: "New task" });
    expect(sheet.textContent).toContain("ENG · Core platform");
  });

  // THE PROJECT A READER IS IN IS THE ROUTE'S OWN ANSWER, the one Hand off
  // asks — never the path's second segment. Read off the path, every other
  // Work page was a project: `#/work/views` opened the sheet asking for a
  // project called `views`, and a task opened by its uuid named the uuid. And
  // the project row under Work that is marked current is the same answer.
  test.each([
    ["#/work/views", ""],
    ["#/work/projects", ""],
    ["#/work/search", ""],
    ["#/work/5f0c2a4e-8b1d-4c3a-9e2f-7a6b5c4d3e2f", ""],
    ["#/work/ENG-12", "ENG"],
    ["#/work/ENG", "ENG"],
  ])("on %s Hand off files into %j", async (hash, project) => {
    location.hash = hash;
    // ADA LEADS IT, so it is one of hers and nests under Work.
    const eng = {
      key: "ENG",
      name: "Core platform",
      unit: { resolved: true },
      lead: { handle: "ada" },
      task_counts: { todo: 2, active: 1, done: 0, closed: 0 },
    };
    const asked: { what: string; params?: Record<string, unknown> }[] = [];
    const { store, socket } = answering(
      {
        work_projects: { projects: [eng], complete: true },
        work_project: { ...eng, statuses: [], types: [], fields: [], complete: true },
      },
      asked,
    );
    mountShell(store, socket);
    await settle();
    // INSIDE WORK THE PROJECTS ARE OPEN under it, and the row marked current
    // is the same answer as Hand off's project.
    const row = screen.getByRole("link", { name: /^Core platform/ });
    expect(row.getAttribute("aria-current")).toBe(project === "ENG" ? "page" : null);
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: /^Hand off/ }));
    });
    screen.getByRole("dialog", { name: "New task" });
    await settle();
    // NO SEGMENT OF THE PATH IS ASKED FOR AS A PROJECT.
    const projectsAsked = asked
      .filter((a) => a.what === "work_project")
      .map((a) => String(a.params?.key));
    expect(projectsAsked.every((key) => key === "ENG")).toBe(true);
  });

  // THE DOOR TO THE CONSOLE IS AN ADMIN'S. A member has no console to open,
  // and a locked door on every screen of theirs is a row about somebody
  // else's job — so it is drawn for an admin, and for nobody else.
  test("the admin door is drawn for an admin, and keeps the reader's place", async () => {
    location.hash = "#/work/ENG-42";
    const { store, socket } = answering({});
    mountShell(store, socket);
    await settle();
    const door = screen.getByRole("navigation", { name: "Admin console" });
    expect(door.querySelector("a")?.getAttribute("href")).toBe("#/admin/live/turns?item=ENG-42");
  });

  test("a member is drawn no door", async () => {
    const { store, socket } = answering({
      viewer: { token_id: "ada", role: "member", reach: "member", linked: true, handle: "ada" },
    });
    mountShell(store, socket);
    await settle();
    expect(screen.queryByRole("navigation", { name: "Admin console" })).toBeNull();
  });

  // ITS FIGURE IS NEUTRAL, NEVER THE ACCENT, with a dot in the worst state:
  // what waits on an admin is not what waits on the reader as a person.
  test("the door counts what needs an admin, in the worst state's colour", async () => {
    const { store, socket } = answering({ sandbox_runs: { runs: [] } });
    mountShell(store, socket);
    await settle();
    act(() =>
      rosterOf(store, [
        { role: "A", activity: "stopped", stopped_reason: "error" },
        { role: "B", activity: "idle" },
      ] as never),
    );
    const door = screen.getByRole("navigation", { name: "Admin console" });
    const link = door.querySelector("a")!;
    expect(link.textContent).toMatch(/1/);
    expect(link.getAttribute("title")).toMatch(/^1 thing needs an admin: /);
    expect(link.querySelector(".crewlet-nav-item__badge")).toBeNull();
  });
});

describe("the page header", () => {
  // A SECTION'S FIGURE IS THE SCREEN'S, and it lands on that section's tab.
  test("a section count a screen publishes is drawn on its tab", async () => {
    function Counting() {
      useSectionCounts({ "asked-of-me": "3", following: "20+" });
      return <div>my work</div>;
    }
    location.hash = "#/me/following";
    const { store, socket } = answering({});
    mountShell(store, socket, <Counting />);
    const tabs = screen.getByRole("navigation", { name: "My work sections" });
    const asked = Array.from(tabs.querySelectorAll("a")).find((a) =>
      a.textContent?.startsWith("Asked of me"),
    );
    expect(asked?.textContent).toBe("Asked of me3");
    const current = tabs.querySelector('[aria-current="page"]');
    expect(current?.textContent).toBe("Following20+");
  });

  // WHOSE DAY IT IS travels between My work's sections; a filter does not.
  test("a section tab carries the workspace's own query and nothing else", async () => {
    location.hash = "#/me?handle=bo&order=priorities";
    const { store, socket } = answering({});
    mountShell(store, socket);
    const tabs = screen.getByRole("navigation", { name: "My work sections" });
    const ready = Array.from(tabs.querySelectorAll("a")).find(
      (a) => a.textContent === "Ready to start",
    );
    expect(ready?.getAttribute("href")).toBe("#/me/ready?handle=bo");
  });

  // THE WINDOW travels between Spend's sections — the tab and the overview's
  // "All" link reach Expensive tasks on the same one — and the chart's split
  // does not, because it is the overview's alone.
  test("a Spend tab carries the window, and Budgets carries it back", async () => {
    location.hash = "#/admin/spend?window=90d&group=model";
    const { store, socket } = answering({});
    mountShell(store, socket);
    const tab = (label: string) =>
      Array.from(
        screen.getByRole("navigation", { name: "Spend sections" }).querySelectorAll("a"),
      ).find((a) => a.textContent === label);
    expect(tab("Expensive tasks")?.getAttribute("href")).toBe("#/admin/spend/tasks?window=90d");
    expect(tab("Budgets")?.getAttribute("href")).toBe("#/admin/spend/budgets?window=90d");
    cleanup();
    location.hash = "#/admin/spend/budgets?window=90d";
    const again = answering({});
    mountShell(again.store, again.socket);
    expect(tab("Overview")?.getAttribute("href")).toBe("#/admin/spend?window=90d");
  });

  // ON AN OBJECT PAGE the crumbs are the way back; a strip of tabs over a
  // task would say it is a fourth kind of list.
  test("no section tabs are drawn on an object's own page", async () => {
    location.hash = "#/work/ENG-42";
    const { store, socket } = answering({});
    mountShell(store, socket);
    expect(screen.queryByRole("navigation", { name: "Work sections" })).toBeNull();
    cleanup();
    location.hash = "#/work/projects";
    const again = answering({});
    mountShell(again.store, again.socket);
    expect(screen.getByRole("navigation", { name: "Work sections" })).toBeDefined();
  });

  // THE CONSOLE'S SECTIONS ARE TABS like every workspace's: Settings' column
  // is gone with Settings, and its sections are Company's, Connect's and
  // Engine's — each a strip in the page header, the section the reader is on
  // marked.
  test("a console workspace draws its sections as tabs, the reader's own marked", async () => {
    location.hash = "#/admin/connect/secrets";
    const { store, socket } = answering({});
    mountShell(store, socket);
    await settle();
    const tabs = screen.getByRole("navigation", { name: "Connect sections" });
    const current = tabs.querySelector('[aria-current="page"]');
    expect(current?.textContent).toContain("Secrets");
    expect(Array.from(tabs.querySelectorAll("a")).map((a) => a.getAttribute("href"))).toEqual(
      expect.arrayContaining([
        "#/admin/connect",
        "#/admin/connect/tools",
        "#/admin/connect/models",
        "#/admin/connect/secrets",
      ]),
    );
  });

  // NO ADMIN POLL FROM THE FRAME. The frame is mounted for the life of the
  // tab, and the Settings column's figures put three admin questions on a
  // timer behind the column; with the column gone, the frame asks none of them
  // anywhere — a console screen asks for its own.
  test("the frame asks no admin answer, in either shell", async () => {
    for (const where of ["#/today", "#/work", "#/admin", "#/admin/company", "#/knowledge"]) {
      location.hash = where;
      const asked: { what: string }[] = [];
      const { store, socket } = answering({}, asked);
      mountShell(store, socket);
      await settle();
      expect(
        asked.map((a) => a.what).filter((w) => ["fleet", "retention", "integrations"].includes(w)),
        `${where} asked an admin answer`,
      ).toEqual([]);
      cleanup();
    }
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
    expect(
      screen.queryByRole("dialog"),
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
    document.querySelector<HTMLAnchorElement>(".crewlet-app-shell a[href='#/today']");
  // Before the company is known the product stands in.
  expect(home()?.textContent).toContain("Crewlet");
  act(() => store.applyOrg(withDerived({ name: "Nimbus", roles: [], units: [] })));
  expect(home()?.textContent).toContain("Nimbus");
  expect(home()?.textContent).not.toContain("Crewlet");
  expect(home()?.querySelector("img")?.getAttribute("src")).toBe(
    "/static/dashboard/crewlet-icon.svg",
  );
});

test("the console's health card reads the push and links to the nodes", async () => {
  location.hash = "#/admin";
  const { store, socket } = answering({});
  mountShell(store, socket);
  act(() => {
    store.setConnected(true);
    store.applyHealth(healthFrame({ nodes: 3, applied_epoch: 42, alarms: { count: 0 } }));
  });
  const card = document.querySelector<HTMLAnchorElement>("a.health-card");
  expect(card?.getAttribute("href")).toBe("#/admin/engine");
  expect(card?.textContent).toContain("Engine healthy");
  expect(card?.textContent).toContain("3 nodes · config epoch 42");
});

// A PROJECT'S LENSES ARE BESIDE ITS NAME, on the bar: `Work › ENG Core ·
// Items | About | History`. As a row of their own under it they cost the
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
  location.hash = "#/work";
  const bare = answering({});
  mountShell(bare.store, bare.socket);
  expect(document.getElementById(PAGE_LENSES_SLOT)?.childElementCount).toBe(0);
});

// A SCREEN'S CONTROLS SIT AS FAR APART AS THE FRAME'S. The slot a screen
// portals its controls into was `gap-1` (4px) beside the frame's own at the
// kit's control spacing, so My work's "Whose day" select touched "Inbox →".
// THE ENGINE'S STATE IS THE HEALTH CARD'S NAME, in the console, and the card is
// the way to the nodes. Every word is the health push's: `HealthCard.test.tsx`
// holds the precedence, this holds the wiring. The person shell draws no card:
// a person's screen is not a status page, and only trouble is said there.
test("the person sidebar says the connection only when it is in trouble", async () => {
  const { store, socket } = answering({});
  mountShell(store, socket);
  expect(screen.getByText("Reconnecting…")).toBeTruthy();
  act(() => store.setConnected(true));
  expect(screen.queryByText("Reconnecting…")).toBeNull();
  expect(document.querySelector("a.health-card")).toBeNull();
});

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
// a screen that draws a column INSIDE itself — Knowledge's tree — loses that
// column's width from its list. The shell asks for the width that is right for
// the frame on screen now, at the reader's density, and writes ONE attribute.
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

  test("beside Knowledge's tree, the tree's width later", async () => {
    const { win, app } = await peekAt(
      "#/knowledge?peek=page:0f0f0f0f-1111-4222-8333-444455556666",
      1280,
    );
    try {
      expect(win.asked).toContain("(width >= 1416px)");
      // A drawer at the width a plain screen takes a column at, over a list
      // that keeps its floor beside the tree.
      expect(app.getAttribute("data-peek")).toBe("drawer");
      win.set(1416);
      expect(app.getAttribute("data-peek")).toBe("column");
      expect(app.style.getPropertyValue("--peek-reserve")).toBe("750px");
    } finally {
      win.restore();
    }
  });

  // A CONSOLE OBJECT IS NOT A PERSON'S PEEK: in the person shell a `peek=`
  // naming a turn opens no rail, and the frame keeps its one column.
  test("the person shell opens no rail for a console object", async () => {
    const { win, app } = await peekAt("#/work?peek=turn:t-1", 1600);
    try {
      expect(app.hasAttribute("data-peek")).toBe(false);
    } finally {
      win.restore();
    }
  });

  // THE PADDING AND THE INSET SCALE WITH THE DENSITY, which no media query
  // can read — so the width asked moves with the reader's choice.
  test("at the reader's density", async () => {
    setDensity("comfortable");
    const { win, app } = await peekAt("#/work?peek=seat:ceo", 1166);
    try {
      expect(win.asked).toContain("(width >= 1167px)");
      expect(app.getAttribute("data-peek")).toBe("drawer");
      win.set(1167);
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

  // A CONSOLE CHORD IS AN ADMIN'S: an admin reaches the console's workspaces
  // from anywhere, and a member's key binds no letter that leads to a
  // refusal.
  test("g then a console letter goes there for an admin, and nowhere for a member", async () => {
    const admin = answering({});
    mountShell(admin.store, admin.socket);
    await settle();
    key("g");
    key("s");
    expect(location.hash).toBe("#/admin/spend");
    cleanup();
    location.hash = "#/today";
    const member = answering({
      viewer: { token_id: "ada", role: "member", reach: "member", linked: true, handle: "ada" },
    });
    mountShell(member.store, member.socket);
    await settle();
    key("g");
    key("s");
    expect(location.hash).toBe("#/today");
  });

  // G . IS THE DOOR AS A KEY: the other shell's twin of where the reader is,
  // both ways, and nothing at all for a member.
  test("g then . crosses to the other shell keeping the place, for an admin", async () => {
    location.hash = "#/team/seats/swe";
    const admin = answering({});
    mountShell(admin.store, admin.socket);
    await settle();
    key("g");
    key(".");
    expect(location.hash).toBe("#/admin/agents/seats/swe");
    key("g");
    key(".");
    expect(location.hash).toBe("#/team/seats/swe");
    cleanup();
    const member = answering({
      viewer: { token_id: "ada", role: "member", reach: "member", linked: true, handle: "ada" },
    });
    mountShell(member.store, member.socket);
    await settle();
    key("g");
    key(".");
    expect(location.hash).toBe("#/team/seats/swe");
  });
});
