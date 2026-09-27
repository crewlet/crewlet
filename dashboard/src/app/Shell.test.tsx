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
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { useSearchTarget } from "./searchTarget.ts";
import { afterEach, beforeEach, describe, expect, test } from "vitest";
import { Shell, usePageCoverage, useSectionCounts } from "./Shell.tsx";
import { Router } from "./router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { CoverageFacts } from "~/components/work.tsx";
import { setDensity } from "~/lib/prefs.ts";
import { Home } from "~/routes/home/Home.tsx";
import { Inbox } from "~/routes/inbox/Inbox.tsx";
import { installWindow } from "~/testing.tsx";
import { PAGE_ACTIONS_SLOT } from "./frame/PageActions.tsx";

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
  work_projects: { projects: [] },
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
          operator_id: "U0FOUNDER",
          operator: true,
          handle: "ada",
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

describe("the sidebar's figures", () => {
  // THE ENGINE'S WORD, and only it: two seats working, one idle, one parked on
  // a question — the count is two, whatever a live call says.
  test("the Agents count is the seats the engine calls working", async () => {
    const { store, socket } = answering({});
    mountShell(store, socket);
    act(() =>
      store.applyAgents([
        { role: "A", activity: "working" },
        { role: "B", activity: "working" },
        { role: "C", activity: "idle", live_call: { in_progress: true } },
        { role: "D", activity: "needs" },
      ] as never),
    );
    expect(screen.getByRole("link", { name: /^Agents/ }).textContent).toContain("2");
  });

  // A PIN IS A PERSON'S, so the strip is asked for WITH the viewer — the
  // shared strip has no pins, which is why the sidebar this replaced never
  // drew one — and with the engine's own counts.
  test("pinned views are asked for as the viewer, and draw the view's own total", async () => {
    const asked: { what: string; params?: Record<string, unknown> }[] = [];
    const { store, socket } = answering(
      {
        work_views: {
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
    const views = asked.find((a) => a.what === "work_views");
    expect(views?.params).toMatchObject({ viewer: "ada", counts: true });
    const pin = screen.getByRole("link", { name: /^Blocked, org-wide/ });
    expect(pin.getAttribute("href")).toBe("#/work/views/v-1");
    expect(pin.textContent).toContain("7");
    expect(screen.queryByText("Not pinned")).toBeNull();
  });

  // MY WORK'S FIGURE IS WHAT IS ASKED OF YOU, counted IN FULL by the engine —
  // never the length of the twenty-row page it drew — and a count that stopped
  // at the engine's ceiling is written as the floor it is.
  function myWork(total: number, capped?: boolean) {
    const block = { total };
    return {
      handle: "ada",
      priorities: [],
      assigned: [],
      asked_of_me: [],
      checklist_items: [],
      collaborating: [],
      watching_recent: [],
      unblocked_recent: [],
      complete: true,
      totals: {
        priorities: block,
        assigned: block,
        asked_of_me: capped ? { total, capped: true } : { total },
        checklist_items: block,
        collaborating: block,
        watching_recent: block,
        unblocked_recent: block,
      },
    };
  }

  test("My work shows the engine's total of what is asked of the viewer", async () => {
    const asked: { what: string; params?: Record<string, unknown> }[] = [];
    // THE PAGE IS EMPTY AND THE TOTAL IS NOT: the figure is the total.
    const { store, socket } = answering({ work_my_work: myWork(3) }, asked);
    mountShell(store, socket);
    await settle();
    expect(asked.find((a) => a.what === "work_my_work")?.params).toMatchObject({ handle: "ada" });
    const row = screen.getByRole("link", { name: /^My work/ });
    expect(row.textContent).toContain("3 asked of you");
  });

  test("a count that stopped at the engine's ceiling is drawn as a floor", async () => {
    const { store, socket } = answering({ work_my_work: myWork(200, true) });
    mountShell(store, socket);
    await settle();
    expect(screen.getByRole("link", { name: /^My work/ }).textContent).toContain(
      "200+ asked of you",
    );
  });

  test("nothing asked, or no answer yet, draws no figure", async () => {
    const { store, socket } = answering({ work_my_work: myWork(0) });
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

  // SETTINGS IS NEVER HIDDEN; the lock is what changes.
  test("Settings is a row for an operator without the lock", async () => {
    const { store, socket } = answering({});
    mountShell(store, socket);
    await settle();
    const foot = screen.getByRole("navigation", { name: "Settings" });
    expect(foot.textContent).toContain("Settings");
    expect(foot.textContent).not.toContain("operator credential");
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

  // SETTINGS DRAWS A COLUMN, with a lock on a guarded section for a reader
  // without the credential, and the section the reader is on marked.
  test("Settings draws its sections as a column, locked where guarded", async () => {
    location.hash = "#/settings/secrets";
    const { store, socket } = answering({
      viewer: { operator_id: "", operator: false, handle: "", name: "", kind: "" },
    });
    mountShell(store, socket);
    await settle();
    const column = screen.getByRole("navigation", { name: "Settings sections" });
    const current = column.querySelector('[aria-current="page"]');
    expect(current?.textContent).toContain("Secrets");
    expect(current?.textContent).toContain("needs an operator credential");
    const general = Array.from(column.querySelectorAll("a")).find((a) =>
      a.textContent?.startsWith("General"),
    );
    expect(general?.textContent).not.toContain("operator credential");
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

// A SCREEN'S CONTROLS SIT AS FAR APART AS THE FRAME'S. The slot a screen
// portals its controls into was `gap-1` (4px) beside the frame's own at the
// kit's control spacing, so My work's "Whose day" select touched "Inbox →".
test("the page header's controls slot spaces a screen's controls at the kit's control gap", () => {
  const { store, socket } = answering({});
  mountShell(store, socket);
  const slot = document.getElementById(PAGE_ACTIONS_SLOT);
  expect(slot?.classList.contains("gap-2")).toBe(true);
  expect(slot?.classList.contains("gap-1")).toBe(false);
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
      const rail = document.querySelector(".crewlet-app-shell__rail");
      expect(rail?.getAttribute("data-open")).not.toBe("true");
      act(() => {
        window.dispatchEvent(new KeyboardEvent("keydown", { key: "\\", ctrlKey: true }));
      });
      expect(rail?.getAttribute("data-open")).toBe("true");
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
      const rail = document.querySelector(".crewlet-app-shell__rail");
      expect(rail?.getAttribute("data-open")).not.toBe("true");
      press();
      expect(rail?.getAttribute("data-open")).toBe("true");
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
