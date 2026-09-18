/**
 * The application mounts, routes, and survives an empty engine.
 *
 * A smoke test rather than a screenshot: what it catches is a broken import, a
 * hook-order violation, and — the one that actually happens — a screen that
 * throws on the state a FRESHLY STARTED company is in, where every list is
 * empty and no query has answered yet. That state is the first thing anybody
 * sees, and it is the one least likely to be exercised by hand.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test } from "vitest";
import { App } from "./App.tsx";
import { Router } from "./router.tsx";
import { screenScroller } from "~/lib/scroller.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { DESTINATIONS } from "./nav.ts";
import { buildHash } from "./router.tsx";
import { resetForTest as resetStarsForTest } from "~/lib/starred.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

function mount() {
  const store = new Store();
  const socket = new LiveSocket(store);
  const view = render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <App />
      </Router>
    </ClientContext.Provider>,
  );
  return { store, socket, view };
}

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  location.hash = "#/";
});

afterEach(() => {
  // Explicit, because the suite runs with `globals: false` — testing-library
  // only registers its own auto-cleanup when a global afterEach exists, so
  // without this every render stacks up in one document and a getByText that
  // should find one node finds five.
  cleanup();
  location.hash = "#/";
});

describe("the shell", () => {
  test("mounts against an engine that has answered nothing", () => {
    mount();
    // The chrome is present and honest: not connected, and saying so — in the
    // rail's own engine pill and in the state bar, which is the one place a
    // degraded connection is reported now.
    expect(screen.getAllByText("unreachable").length).toBeGreaterThan(0);
    // AND THE LANDING SCREEN IS THE INBOX, which is what a person opening
    // this wants to know: is anything waiting on me.
    expect(screen.getAllByText("Inbox").length).toBeGreaterThan(0);
  });

  test("a disconnected engine is itself the first thing needing a person", () => {
    // It is not a footnote in a popover: the page cannot tell the truth about
    // anything else while the socket is down, so it leads.
    mount();
    expect(screen.getByText("No connection to the engine")).toBeDefined();
  });

  test("every workspace is in the rail, locked ones included", () => {
    // A SECTION THAT VANISHES without a credential is indistinguishable from
    // one that does not exist, so Admin is always a row and carries a lock.
    mount();
    for (const label of ["Inbox", "Work", "Company", "Knowledge", "Activity", "Cost", "Admin"]) {
      expect(screen.getAllByText(label).length, label).toBeGreaterThan(0);
    }
  });
});

describe("routing", () => {
  // DERIVED FROM THE DESTINATIONS TABLE, not a hand-written list. The list
  // was one, and a hand-written one covers exactly the screens somebody
  // remembered to add to it — so a new destination that renders a blank ships
  // green, which is the one failure this test exists to catch.
  test("every destination renders a screen rather than a blank", () => {
    const visited = DESTINATIONS.map((item) => buildHash(item.path));
    // The two-level routes are the reason the list is derived: a hand-written
    // one would not have them.
    expect(visited).toContain(buildHash(["work", "views"]));
    expect(visited).toContain(buildHash(["activity", "turns"]));
    for (const hash of visited) {
      location.hash = hash;
      const { view } = mount();
      expect(view.container.querySelector(".screen-inner")?.children.length, hash).toBeGreaterThan(
        0,
      );
      view.unmount();
    }
  });

  // THE OTHER HALF OF `source.test.ts`'s link gate. That one proves every
  // link names a segment a workspace OWNS; this one proves the object routes
  // those links actually build resolve to a screen. A workspace can own
  // `activity` while `#/activity/traces/{id}` still falls through to Not
  // Found, which is exactly what shipped: `traces` had a screen, four buttons
  // pointing at it, and no case in the switch.
  test("every object route a link builds resolves to a screen", () => {
    const routes = [
      ["activity", "turns", "11111111-1111-4111-8111-111111111111"],
      ["activity", "traces", "22222222-2222-4222-8222-222222222222"],
      ["activity", "runs", "33333333-3333-4333-8333-333333333333"],
      ["activity", "events", "44444444-4444-4444-8444-444444444444"],
      ["activity", "a2a", "55555555-5555-4555-8555-555555555555"],
      ["activity", "schedules", "role", "ceo", "standup"],
      ["company", "people", "ada"],
      ["company", "units", "platform"],
      ["work", "ENG-42"],
      ["goals", "66666666-6666-4666-8666-666666666666"],
      ["knowledge", "ENG", "Deploy runbook"],
      ["admin", "fleet", "node-a"],
      ["admin", "integrations", "slack"],
      ["admin", "credentials", "SLACK_SIGNING_SECRET"],
      ["admin", "config", "revisions", "77777777-7777-4777-8777-777777777777"],
    ];
    for (const path of routes) {
      const hash = buildHash(path);
      location.hash = hash;
      const { view } = mount();
      expect(view.container.textContent, hash).not.toMatch(
        /there is no such screen|under (Activity|Company|Admin)/,
      );
      view.unmount();
    }
  });

  test("an unknown screen says so instead of rendering nothing", () => {
    location.hash = "#/nonsense";
    mount();
    expect(screen.getByText(/there is no such screen/)).toBeDefined();
  });

  // A TAIL NOTHING ROUTES IS NOT THE WORKSPACE'S LANDING SCREEN. `cost` had a
  // two-valued test — `budgets` or the spend screen — so `#/cost/budget`, the
  // obvious typo and the shape of a stale bookmark, drew the spend tables
  // under a trail reading "Cost / budget": the address, the trail and the
  // screen each naming something different. Every sibling workspace in this
  // file ends its switch with Not Found; this one now does too.
  test("a tail under Cost that names no screen says so", () => {
    location.hash = "#/cost/budget";
    mount();
    expect(screen.getByText(/there is no such screen/)).toBeDefined();
    // …and the two addresses that DO exist are untouched.
    cleanup();
    location.hash = "#/cost";
    mount();
    expect(screen.queryByText(/there is no such screen/)).toBeNull();
    cleanup();
    location.hash = "#/cost/budgets";
    mount();
    expect(screen.queryByText(/there is no such screen/)).toBeNull();
  });

  // THE SAME SHAPE ONE CASE OVER. `revisions/{id}` is the only tail Config
  // has, and reading it as `tail[0] === "revisions"` alone rendered the config
  // screen with the segment dropped for everything else.
  test("a tail under Config that names no screen says so", () => {
    location.hash = "#/admin/config/nonsense";
    mount();
    expect(screen.getByText(/there is no such screen/)).toBeDefined();
  });

  /**
   * A TOOL NAME IS A THIRD PARTY'S STRING, and `servers` is a legal one.
   *
   * `#/admin/tools/servers/{name}` filters the catalogue by origin and
   * `#/admin/tools/{name}` is one tool's page — the address `objects.ts`
   * builds and where a tool row's `Open ↗` goes. Discriminating on the WORD
   * took the page away from a tool actually called `servers` and dropped the
   * reader on the unfiltered catalogue, which is the regression the switch's
   * own comment says it fixed. The tail's LENGTH is what tells them apart:
   * the router encodes each segment whole, so a tool page is always exactly
   * one tail segment and the filter always two.
   */
  test("a tool named `servers` keeps its page, and the origin filter keeps its", () => {
    const annotations = {
      read_only: "yes",
      destructive: "no",
      idempotent: "yes",
      open_world: "no",
    } as const;
    const catalogue = [
      { name: "servers", description: "", source: "mcp:acme", annotations, delivers: "" },
      { name: "create_issue", description: "", source: "mcp:github", annotations, delivers: "" },
    ];

    location.hash = "#/admin/tools/servers";
    const one = mount();
    one.store.applyTools(catalogue);
    one.view.rerender(
      <ClientContext.Provider value={{ store: one.store, socket: new LiveSocket(one.store) }}>
        <Router>
          <App />
        </Router>
      </ClientContext.Provider>,
    );
    // The addressed tool is drawn above the catalogue in its own header.
    expect(one.view.container.querySelector(".object-head")).not.toBeNull();
    one.view.unmount();
    cleanup();

    location.hash = "#/admin/tools/servers/github";
    const two = mount();
    two.store.applyTools(catalogue);
    two.view.rerender(
      <ClientContext.Provider value={{ store: two.store, socket: new LiveSocket(two.store) }}>
        <Router>
          <App />
        </Router>
      </ClientContext.Provider>,
    );
    // Two segments is the FILTER, so no tool is addressed and the catalogue
    // stands alone.
    expect(two.view.container.querySelector(".object-head")).toBeNull();
  });

  test("a seat that does not exist explains itself", () => {
    location.hash = "#/company/people/ghost";
    mount();
    expect(screen.getByText(/No seat called/)).toBeDefined();
  });
});

describe("live state reaches the screen", () => {
  test("a pushed roster renders its seats", () => {
    // THE HASH BEFORE THE MOUNT. The router reads `location.hash` at mount
    // and then listens for `hashchange`, which jsdom dispatches
    // asynchronously — so assigning it after mounting and re-rendering
    // synchronously renders the screen the reader was on before.
    location.hash = "#/company/people";
    const { store, view } = mount();
    store.applyOrg({
      name: "Acme",
      roles: [{ name: "CEO", handle: "ceo", goal: "Set direction" }],
    });
    store.applyAgents([{ role: "CEO", state: "working" }]);
    view.rerender(
      <ClientContext.Provider value={{ store, socket: new LiveSocket(store) }}>
        <Router>
          <App />
        </Router>
      </ClientContext.Provider>,
    );
    expect(screen.getAllByText("CEO").length).toBeGreaterThan(0);
  });

  // THE STAR'S WHOLE ROUND TRIP. `lib/starred.ts` was written complete — the
  // cap, the refusal at it, the storage guards, its own suite — and nothing in
  // the product could make a star or read one back: the page bar's own doc
  // comment promised "Copy link, star, open" and only Copy link existed, so
  // every workspace sidebar's Starred section was empty by construction.
  test("keeping a page puts it in this workspace's sidebar", async () => {
    localStorage.clear();
    resetStarsForTest();
    location.hash = "#/company/people/ceo";
    const { store, view } = mount();
    store.applyOrg({
      name: "Acme",
      roles: [{ name: "CEO", handle: "ceo", goal: "Set direction" }],
    });
    view.rerender(
      <ClientContext.Provider value={{ store, socket: new LiveSocket(store) }}>
        <Router>
          <App />
        </Router>
      </ClientContext.Provider>,
    );
    expect(screen.queryByText("Starred")).toBeNull();
    fireEvent.click(screen.getByTitle("Keep in Starred"));
    expect(screen.getByText("Starred")).toBeDefined();
    // AND IT IS THE SAME BUTTON that takes it back out: a filled star means
    // "not this one".
    fireEvent.click(screen.getByTitle("Remove from Starred"));
    expect(screen.queryByText("Starred")).toBeNull();
  });

  test("a seat opened on a tab it does not have still has a page under it", () => {
    // THE BLANK PAGE, end to end. `tab=` is a string off a URL and the tab
    // set belongs to the seat's kind, so `?tab=zzz` — a bookmark, a typed
    // URL, a link made before the kind changed — used to select no tab and
    // match no branch: the header and the strip, and then nothing.
    location.hash = "#/company/people/ceo?tab=zzz";
    const { store, view } = mount();
    store.applyOrg({
      name: "Acme",
      roles: [{ name: "CEO", handle: "ceo", goal: "Set direction" }],
    });
    store.applyAgents([{ role: "CEO", state: "working" }]);
    view.rerender(
      <ClientContext.Provider value={{ store, socket: new LiveSocket(store) }}>
        <Router>
          <App />
        </Router>
      </ClientContext.Provider>,
    );
    // THE STRIP'S OWN ANSWER, which is the thing being asserted: a `tab=`
    // naming no tab must resolve to one that exists, and the reader must be
    // able to see WHICH. Reading the page's content instead was ambiguous
    // the moment the seat grew an ObjectHeader whose facts repeat a
    // property's label.
    const selected = screen
      .getAllByRole("tab")
      .find((t) => t.getAttribute("aria-selected") === "true");
    expect(selected?.textContent).toContain("Overview");
  });
});

describe("a turn watched to its end", () => {
  /**
   * THE REPORTED BUG, end to end.
   *
   * A reader opens a seat's Model activity tab and watches a turn run. The
   * engine pushes the phase's rounds; the turn renders live. Then the review
   * lands: the projection clears `live_call` and pushes the overlay, and the
   * durable `agent_phase_completed` arrives on the same socket a beat earlier.
   *
   * Before this, the tab read only the overlay and a query answered once at
   * mount — so the turn vanished. On a seat's FIRST turn, the one where
   * onboarding runs, the mount-time history is empty and the page was left
   * claiming the seat had never taken a turn at all.
   */
  const seat = { name: "Acme", roles: [{ name: "CEO", handle: "ceo", goal: "Set direction" }] };

  const phaseEnvelope = (id: string, phase: string, ts: string) => ({
    id,
    type: "agent_phase_completed",
    timestamp: ts,
    source: "engine",
    actor: "CEO",
    summary: `${phase} finished`,
    category: "lifecycle",
    trace_id: "",
    span_id: "",
    parent_span_id: "",
    topic: "",
    failed: false,
    payload: {
      turn_id: "t1",
      phase,
      iteration: phase === "onboarding" ? 0 : 1,
      role: "CEO",
      model: "claude-sonnet-5",
      total_tokens: 100,
    },
  });

  function seatView() {
    location.hash = "#/company/people/ceo?tab=turns";
    const { store, view } = mount();
    store.applyOrg(seat);
    const redraw = () =>
      view.rerender(
        <ClientContext.Provider value={{ store, socket: new LiveSocket(store) }}>
          <Router>
            <App />
          </Router>
        </ClientContext.Provider>,
      );
    return { store, view, redraw };
  }

  test("the turn is still there once its review completes", () => {
    const { store, redraw } = seatView();

    // Onboarding and execute have landed; the review is live.
    store.applyEvent(phaseEnvelope("p1", "onboarding", "2026-01-01T00:00:01Z") as never);
    store.applyEvent(phaseEnvelope("p2", "execute", "2026-01-01T00:00:05Z") as never);
    store.applyAgents([
      {
        role: "CEO",
        state: "working",
        live_call: {
          turn_id: "t1",
          phase: "review",
          iteration: 1,
          model: "claude-sonnet-5",
          in_progress: true,
          round_num: 0,
          rounds: 1,
          started_at: "2026-01-01T00:00:07Z",
          updated_at: "2026-01-01T00:00:08Z",
        },
      },
    ] as never);
    redraw();
    expect(screen.getByText("Running now")).toBeDefined();
    expect(screen.getAllByText("review").length).toBeGreaterThan(0);

    // The review lands. The event goes out first, then the overlay that clears
    // the live call — the order internal/api/stream.Ingest publishes them in.
    store.applyEvent(phaseEnvelope("p3", "review", "2026-01-01T00:00:09Z") as never);
    store.applyAgents([{ role: "CEO", state: "idle", live_call: null }] as never);
    redraw();

    // STILL THERE, all three phases of it, and no longer running.
    expect(screen.queryByText("Running now")).toBeNull();
    expect(screen.getAllByText("onboarding").length).toBeGreaterThan(0);
    expect(screen.getAllByText("execute").length).toBeGreaterThan(0);
    expect(screen.getAllByText("review").length).toBeGreaterThan(0);
    // And the page does not claim the seat has never run.
    expect(screen.queryByText("No phases in the record for this seat")).toBeNull();
  });

  test("it is not held behind a 'new turns' button either", () => {
    // The reader was watching it: it is not a new row, whichever list it is in.
    // SCROLLED DOWN, which is the whole condition — at the top of the scroller
    // the settled list admits everything anyway, so an assertion taken there
    // passes whether the rule holds or not.
    const { store, redraw } = seatView();
    const scroller = screenScroller();
    if (!scroller) throw new Error("no scroller to scroll: the shell's layout moved");
    Object.defineProperty(scroller, "scrollTop", { value: 400, configurable: true });
    store.applyAgents([
      {
        role: "CEO",
        state: "working",
        live_call: {
          turn_id: "t1",
          phase: "execute",
          iteration: 1,
          model: "claude-sonnet-5",
          in_progress: true,
          round_num: 0,
          rounds: 1,
          started_at: "2026-01-01T00:00:01Z",
          updated_at: "2026-01-01T00:00:02Z",
        },
      },
    ] as never);
    redraw();
    store.applyEvent(phaseEnvelope("p1", "execute", "2026-01-01T00:00:05Z") as never);
    store.applyAgents([{ role: "CEO", state: "idle", live_call: null }] as never);
    redraw();
    expect(screen.queryByText(/finished while you were reading/)).toBeNull();
    expect(screen.getAllByText("execute").length).toBeGreaterThan(0);
    // And the transcript the reader had open is still open: the card is
    // remounted when it crosses lists, so its latched state does not travel.
    // Probed on the open card's own footer control, whose title still names
    // THIS turn — so the assertion stays about the turn that crossed rather
    // than about any card that happens to be open.
    expect(screen.getAllByTitle(/^turn t1/).length).toBeGreaterThan(0);
  });
});
