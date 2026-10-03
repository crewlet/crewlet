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
import { act, cleanup, fireEvent, render, screen, waitFor } from "~/test/inCase.ts";
import { useSearchTarget } from "./searchTarget.ts";
import { afterEach, beforeAll, beforeEach, describe, expect, test } from "vitest";
import { Shell, usePageCoverage, usePublishFleet, useSectionCounts } from "./Shell.tsx";
import { Router } from "./router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store, type FleetAnswer } from "~/protocol/index.ts";
import type { CoverageFacts } from "~/components/work.tsx";
import { setDensity } from "~/lib/prefs.ts";
import { Home } from "~/routes/home/Home.tsx";
import { Inbox } from "~/routes/inbox/Inbox.tsx";
import { installWindow } from "~/testing.tsx";
import { PAGE_ACTIONS_SLOT, PAGE_LENSES_SLOT } from "./frame/PageActions.tsx";
import { Project } from "~/routes/work/Project.tsx";
import { CHUNKS, loadChunk } from "./lazyScreen.ts";

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

// EVERY CHUNK THE FRAME DRAWS FROM IS IN BEFORE A CASE STARTS — the Knowledge
// column, the New task sheet and the peeks are lazy (`lazyScreen.ts`), and
// what this suite asserts is what the frame does, not how long a cold
// `import()` takes. A mount that suspended on one would also do it inside the
// synchronous `act` `render` opens, which React refuses to wait on.
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
  // a screen that does not — Settings › Secrets, say — and the inbox's
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
  fleet: { nodes: [], seats: [], duties: [], target_epoch: 0 },
  retention: { domains: [], nodes: [], snapshots: [], alarms: [], register_readable: true },
  integrations: { integrations: [], tools: [], traffic_known: true, traffic_since: null },
  // The Knowledge column's count of agent diaries (`routes/knowledge/KnowledgeTree.tsx`).
  memory_overview: { seats: [], coverage: { nodes: [], complete: true } },
};

/** What the engine answers for a person bound to the seat `ada`, holding every grant. */
const ADA = {
  login: "ada.lovelace",
  grants: [
    "state:read",
    "audit:read",
    "config:read",
    "secrets:read",
    "work:write",
    "knowledge:write",
    "config:write",
    "secrets:write",
    "fleet:operate",
    "people:manage",
    "sandbox:run",
  ],
  handle: "ada",
  owner: "ada",
  name: "Ada Lovelace",
  kind: "human",
};

/** A person signed in holding only the grant every screen reads under, and no seat. */
const READER = {
  login: "jane.doe",
  grants: ["state:read"],
  handle: "",
  owner: "jane.doe",
  name: "",
  kind: "",
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
      return Promise.resolve(answers.viewer ?? ADA);
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
  // AN UNBOUND PRINCIPAL HAS A RECORD TOO, kept under their login — where
  // their assistant writes their notices — so the badge asks for it there
  // rather than for nobody's.
  test("an unbound reader's badge counts the record kept under their login", async () => {
    const asked: { what: string; params?: Record<string, unknown> }[] = [];
    const { store, socket } = answering(
      {
        viewer: READER,
        work_inbox: { handle: "jane.doe", notices: [notice("mention", 1)], primary_reasons: [] },
      },
      asked,
    );
    mountShell(store, socket);
    await settle();
    const inbox = asked.find((a) => a.what === "work_inbox");
    expect(inbox?.params).toMatchObject({ handle: "jane.doe", unread: true, primary_only: true });
    expect(screen.getByRole("link", { name: /^Inbox/ }).textContent).toContain("1");
  });

  test("nothing waiting draws no figure", async () => {
    const { store, socket } = answering({
      work_inbox: { handle: "ada", notices: [], primary_reasons: [] },
    });
    mountShell(store, socket);
    await settle();
    expect(screen.getByRole("link", { name: /^Inbox/ }).textContent).toBe("Inbox");
  });
});

describe("the sidebar's figures", () => {
  // THE ENGINE'S WORD, and only it: two seats working, one idle, one parked on
  // a question — the count is two, whatever a live call says.
  test("the Agents count is the seats the engine calls working", async () => {
    const { store, socket } = answering({});
    mountShell(store, socket);
    act(() =>
      store.applySeats([
        { id: "a", agent_id: "id-a", role: "A", activity: "working" },
        { id: "b", agent_id: "id-b", role: "B", activity: "working" },
        {
          id: "c",
          agent_id: "id-c",
          role: "C",
          activity: "idle",
          live_call: { in_progress: true },
        },
        { id: "d", agent_id: "id-d", role: "D", activity: "needs" },
      ] as never),
    );
    expect(screen.getByRole("link", { name: /^Agents/ }).textContent).toContain("2");
  });

  // A PIN IS A PERSON'S, and the engine answers the CALLER'S — a `viewer=`
  // naming anybody else was a free choice of whose personal views to read, and
  // the engine takes none — across every container, with the engine's own
  // counts. The shared views carry no pins, which is why the sidebar this replaced
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
    expect(views?.params).toMatchObject({ counts: true });
    expect(views?.params).not.toHaveProperty("viewer");
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
    expect(
      asked.filter((a) => a.what === "work_inbox" && a.params?.primary_only === true),
    ).toHaveLength(1);
  });

  // EVERY "NEW TASK" OPENS THE FRAME'S ONE SHEET: Home's in its page bar, the
  // sidebar's head `+` and the Projects group's `+` — not a second form each, and
  // not the palette it stood in for until the sheet landed.
  test.each([
    ["Home's New task", "button", /^New task$/],
    ["the sidebar's head +", "button", /^New task$/],
  ] as const)("%s opens the frame's New task sheet", async (who, role, name) => {
    location.hash = "#/home";
    const { store, socket } = answering({
      work_workload: { rows: [] },
      sandbox_runs: { runs: [] },
      work_projects: { projects: [], complete: true },
    });
    mountShell(store, socket, <Home key="home" />);
    await settle();
    const presses = screen.getAllByRole(role, { name });
    // The page bar's button comes after the sidebar's in document order.
    const press = who === "Home's New task" ? presses[presses.length - 1]! : presses[0]!;
    await act(async () => {
      fireEvent.click(press);
    });
    await waitFor(() => expect(screen.getByRole("dialog", { name: "New task" })).toBeTruthy());
  });

  // THE PROJECTS GROUP'S `+` FILES INTO THE PROJECT THE READER IS IN, which
  // the sheet shows as its chosen project before anything is sent.
  test("the Projects group's + opens the sheet on the project the reader is in", async () => {
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
      fireEvent.click(screen.getByRole("button", { name: "New task in ENG" }));
    });
    const sheet = await waitFor(() => screen.getByRole("dialog", { name: "New task" }));
    expect(sheet.textContent).toContain("ENG · Core platform");
  });

  // THE PROJECT A READER IS IN IS THE ROUTE'S OWN ANSWER, the one the sidebar's
  // head `+` asks — never the path's second segment. Read off the path, every
  // other Work page was a project: `#/work/views` offered "New task in views"
  // and opened the sheet asking for a project called `views`, and a task
  // opened by its uuid named the uuid.
  test.each([
    ["#/work/views", ""],
    ["#/work/projects", ""],
    ["#/work/history", ""],
    ["#/work/search", ""],
    ["#/work/5f0c2a4e-8b1d-4c3a-9e2f-7a6b5c4d3e2f", ""],
    ["#/work/ENG-12", "ENG"],
    ["#/work/ENG", "ENG"],
  ])("on %s the Projects group's + files into %j", async (hash, project) => {
    location.hash = hash;
    const eng = {
      key: "ENG",
      name: "Core platform",
      unit: { resolved: true },
      lead: {},
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
    const row = screen.getByRole("link", { name: /^ENG Core platform/ });
    // THE ROW MARKED CURRENT IS THE SAME ANSWER as the `+`'s project.
    expect(row.getAttribute("aria-current")).toBe(project === "ENG" ? "page" : null);
    const press = screen.getByRole("button", {
      name: project ? `New task in ${project}` : "New task in a project",
    });
    await act(async () => {
      fireEvent.click(press);
    });
    await waitFor(() => screen.getByRole("dialog", { name: "New task" }));
    await settle();
    // NO SEGMENT OF THE PATH IS ASKED FOR AS A PROJECT.
    const projectsAsked = asked
      .filter((a) => a.what === "work_project")
      .map((a) => String(a.params?.key));
    expect(projectsAsked.every((key) => key === "ENG")).toBe(true);
  });

  // SETTINGS IS NEVER HIDDEN; the lock is what changes.
  test("Settings is a row without the lock for a reader every section opens for", async () => {
    const { store, socket } = answering({});
    mountShell(store, socket);
    await settle();
    const foot = screen.getByRole("navigation", { name: "Settings" });
    expect(foot.textContent).toContain("Settings");
    expect(foot.textContent).not.toContain("need a grant");
  });

  // AND FOR A READER IT CLOSES ON, THE LOCK SAYS HOW MUCH AND NAMES WHAT
  // WOULD OPEN IT — what they would ask somebody for.
  test("Settings carries a lock naming the grants a reader lacks", async () => {
    const { store, socket } = answering({ viewer: READER });
    mountShell(store, socket);
    await settle();
    const foot = screen.getByRole("navigation", { name: "Settings" });
    expect(foot.textContent).toMatch(/\d+ of its sections need a grant you do not hold/);
    const lock = foot.querySelector(".side-locked");
    expect(lock?.getAttribute("title")).toContain("config:read");
    expect(lock?.getAttribute("title")).toContain("fleet:operate");
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

  // SETTINGS DRAWS A COLUMN, with a lock on a section for a reader holding
  // none of its grants — naming the grant — and the section the reader is on
  // marked.
  test("Settings draws its sections as a column, locked where the reader lacks the grant", async () => {
    location.hash = "#/settings/secrets";
    const { store, socket } = answering({ viewer: READER });
    mountShell(store, socket);
    await settle();
    const column = screen.getByRole("navigation", { name: "Settings sections" });
    const current = column.querySelector('[aria-current="page"]');
    expect(current?.textContent).toContain("Secrets");
    expect(current?.textContent).toContain("needs config:read");
    const general = Array.from(column.querySelectorAll("a")).find((a) =>
      a.textContent?.startsWith("General"),
    );
    expect(general?.textContent).not.toContain("needs");
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
    const toggle = document.querySelector<HTMLButtonElement>(".section-picker-toggle")!;
    expect(toggle.textContent).toBe("Settings section: Secrets");
    const list = document.getElementById(toggle.getAttribute("aria-controls") ?? "");
    expect(list?.querySelector("nav[aria-label='Settings sections']")).not.toBeNull();
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    expect(toggle.closest(".section-picker")?.hasAttribute("data-open")).toBe(false);

    fireEvent.click(toggle);
    expect(toggle.getAttribute("aria-expanded")).toBe("true");
    expect(toggle.closest(".section-picker")?.hasAttribute("data-open")).toBe(true);

    await act(async () => {
      location.hash = "#/settings/nodes";
      window.dispatchEvent(new HashChangeEvent("hashchange"));
    });
    await settle();
    const moved = document.querySelector<HTMLButtonElement>(".section-picker-toggle")!;
    expect(moved.textContent).toBe("Settings section: Nodes");
    expect(moved.getAttribute("aria-expanded"), "a picked section left the list open").toBe(
      "false",
    );
  });

  // AN ADDRESS THAT NAMES NO SCREEN IS IN NO SECTION. General's path is the
  // workspace root and so a prefix of every Settings address: a mistyped one
  // drew "Not found" beside a column, and a phone picker, marking General.
  test("a Settings address that names no screen marks no section", async () => {
    location.hash = "#/settings/general";
    const { store, socket } = answering({});
    mountShell(store, socket);
    await settle();
    const toggle = document.querySelector<HTMLButtonElement>(".section-picker-toggle")!;
    expect(toggle.textContent).toBe("Settings sections");
    expect(document.querySelector(".section-picker-list [aria-current]")).toBeNull();
  });

  // NO GUARDED POLL OUTSIDE SETTINGS. The frame is mounted for the life of
  // the tab, so a figure hook that asked unconditionally would put three
  // guarded questions on a timer behind every screen of every tab.
  const OPERATOR_ANSWERS = ["fleet", "retention", "integrations"];
  test("the guarded answers are asked only while the Settings column is drawn", async () => {
    for (const where of ["#/home", "#/work", "#/spend/budgets", "#/knowledge"]) {
      location.hash = where;
      const asked: { what: string }[] = [];
      const { store, socket } = answering({}, asked);
      mountShell(store, socket);
      await settle();
      expect(
        asked.map((a) => a.what).filter((w) => OPERATOR_ANSWERS.includes(w)),
        `${where} asked a guarded answer`,
      ).toEqual([]);
      cleanup();
    }
    location.hash = "#/settings";
    const asked: { what: string }[] = [];
    const { store, socket } = answering({}, asked);
    mountShell(store, socket);
    await settle();
    expect(new Set(asked.map((a) => a.what).filter((w) => OPERATOR_ANSWERS.includes(w)))).toEqual(
      new Set(OPERATOR_ANSWERS),
    );
  });

  test("a reader without the grants is asked nothing, even in Settings", async () => {
    location.hash = "#/settings";
    const asked: { what: string }[] = [];
    const { store, socket } = answering({ viewer: READER }, asked);
    mountShell(store, socket);
    await settle();
    expect(asked.map((a) => a.what).filter((w) => OPERATOR_ANSWERS.includes(w))).toEqual([]);
  });

  // EACH ANSWER BY ITS OWN SECTION'S GRANT: a reader who may read the
  // configuration and not operate the fleet is asked the roll-up and nothing
  // the fleet's grant guards.
  test("each guarded answer is asked of a reader its own section opens for", async () => {
    location.hash = "#/settings";
    const asked: { what: string }[] = [];
    const { store, socket } = answering(
      { viewer: { ...READER, grants: ["state:read", "config:read"] } },
      asked,
    );
    mountShell(store, socket);
    await settle();
    expect(asked.map((a) => a.what).filter((w) => OPERATOR_ANSWERS.includes(w))).toEqual([
      "integrations",
    ]);
  });

  // THE PILL IS THE ENGINE'S ROLL-UP, read in the row's name — and painted
  // as the state it is, not as the accent's unread badge.
  test("the Integrations row carries the roll-up's attention count as its pill", async () => {
    location.hash = "#/settings";
    const { store, socket } = answering({
      integrations: {
        integrations: [],
        traffic_known: true,
        traffic_since: null,
        tools: [
          { key: "slack", surfaces: [], state: "attention", label: "Needs attention", reason: "" },
          { key: "gitlab", surfaces: [], state: "connected", label: "Connected", reason: "" },
        ],
      },
    });
    mountShell(store, socket);
    await settle();
    const column = screen.getByRole("navigation", { name: "Settings sections" });
    const row = Array.from(column.querySelectorAll("a")).find((a) =>
      a.textContent?.startsWith("Integrations"),
    );
    expect(row?.textContent).toContain("1");
    expect(row?.textContent).toContain("1 needs attention");
    expect(row?.closest(".section-row-attention")).not.toBeNull();
    expect(row?.querySelector(".crewlet-nav-item__badge")?.textContent).toBe("1");
  });

  // A FIGURE SAYS WHAT IT COUNTS: the applied epoch read "e2", a code.
  test("the Configuration row names the epoch this node applied in words", async () => {
    location.hash = "#/settings";
    const { store, socket } = answering({});
    mountShell(store, socket);
    act(() => store.applyHealth({ status: "ok", applied_epoch: 2, nodes: 1 }));
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
        { id: "n1", roles: [], seats: 1, config_epoch: 3 },
        { id: "n2", roles: [], seats: 1, config_epoch: 2 },
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
    act(() => store.applyHealth({ status: "ok", applied_epoch: 3, nodes: 2 }));
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

// THE ENGINE'S STATE IS THE HEALTH CARD'S NAME, and the card is the way to the
// nodes. Every word is the health push's: `HealthCard.test.tsx` holds the
// precedence, this holds the wiring.
// THE COMPANY IS THE LOCKUP'S TITLE, beside the product's mark — the approved
// design's "Nimbus" — and the lockup is the way home.
test("the lockup is the company's name beside the product's mark, and goes home", async () => {
  const { store, socket } = answering({});
  mountShell(store, socket);
  const home = () =>
    document.querySelector<HTMLAnchorElement>(".crewlet-app-shell a[href='#/home']");
  // Before the company is known the product stands in.
  expect(home()?.textContent).toContain("Crewlet");
  act(() => store.applyOrg({ name: "Nimbus", roles: [], units: [] }));
  expect(home()?.textContent).toContain("Nimbus");
  expect(home()?.textContent).not.toContain("Crewlet");
  expect(home()?.querySelector("img")?.getAttribute("src")).toBe(
    "/static/dashboard/crewlet-icon.svg",
  );
});

test("the health card reads the push and links to the nodes", async () => {
  const { store, socket } = answering({});
  mountShell(store, socket);
  act(() => {
    store.setConnected(true);
    store.applyHealth({
      status: "healthy",
      configured: true,
      nodes: 3,
      applied_epoch: 42,
      alarms: { count: 0 },
    });
  });
  const card = document.querySelector<HTMLAnchorElement>("a.health-card");
  expect(card?.getAttribute("href")).toBe("#/settings/nodes");
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
});
