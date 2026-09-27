/**
 * The application mounts, routes, and survives an empty engine.
 *
 * A smoke test rather than a screenshot: what it catches is a broken import, a
 * hook-order violation, and — the one that actually happens — a screen that
 * throws on the state a FRESHLY STARTED company is in, where every list is
 * empty and no query has answered yet. That state is the first thing anybody
 * sees, and it is the one least likely to be exercised by hand.
 */

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeAll, beforeEach, describe, expect, test, vi } from "vitest";
import { App } from "./App.tsx";
import { Router } from "./router.tsx";
import { screenScroller } from "~/lib/scroller.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, QueryError, Store } from "~/protocol/index.ts";
import { DESTINATIONS, WORKSPACES } from "./nav.ts";
import { PAGE_ACTIONS_SLOT } from "./frame/PageActions.tsx";
import { buildHash } from "./router.tsx";
import { resetForTest as resetStarsForTest } from "~/lib/starred.ts";
import { CHUNKS, loadChunk } from "./lazyScreen.ts";

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

/** The frame, over a socket that answers the viewer and refuses the rest. */
function mountAs(viewer: Promise<unknown>) {
  const store = new Store();
  store.applyHealth({ status: "ok", nodes: 1, applied_epoch: 2 });
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) => {
    return what === "viewer" ? viewer : Promise.reject(new QueryError("unauthorized"));
  };
  const view = render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <App />
      </Router>
    </ClientContext.Provider>,
  );
  return { view };
}

// EVERY SCREEN'S CODE IS IN BEFORE A CASE STARTS. The screens are lazy
// chunks, and what this suite asserts is what each screen draws on the state it
// is given — not how long a cold `import()` takes under a test transformer,
// which a `findBy` timeout would otherwise be measuring. The loading path has
// its own suite, `lazy.test.tsx`.
beforeAll(async () => {
  await Promise.all(CHUNKS.map((chunk) => loadChunk(chunk)));
});

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
    // sidebar's health card and in the landing screen's own sentence.
    expect(screen.getAllByText("Reconnecting").length).toBeGreaterThan(0);
    // AND THE LANDING SCREEN IS HOME, whose one sentence an engine condition
    // takes over rather than sitting beside a claim that all is well.
    expect(screen.getByText(/Not connected to the engine/)).toBeDefined();
  });

  test("a disconnected engine is itself the first thing needing a person", () => {
    // It is not a footnote in a popover: the page cannot tell the truth about
    // anything else while the socket is down, so it leads.
    mount();
    expect(
      screen.getByText("Not connected to the engine. What is shown is the last state it sent."),
    ).toBeDefined();
  });

  test("every workspace is in the sidebar, Settings included", () => {
    // A SECTION THAT VANISHES without a credential is indistinguishable from
    // one that does not exist, so Settings is always a row and carries a lock.
    mount();
    const nav = screen.getByRole("navigation", { name: "Navigation" });
    for (const label of [
      "Home",
      "Inbox",
      "My work",
      "Work",
      "Agents",
      "Live",
      "Knowledge",
      "Spend",
    ]) {
      expect(nav.textContent, label).toContain(label);
    }
    const foot = screen.getByRole("navigation", { name: "Settings" });
    expect(foot.textContent).toContain("Settings");
    // THE LOCK IS SAID, not only drawn: this reader holds no credential.
    expect(foot.textContent).toContain("most sections need an operator credential");
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
    expect(visited).toContain(buildHash(["live", "turns"]));
    for (const hash of visited) {
      location.hash = hash;
      const { view } = mount();
      expect(
        view.container.querySelector(".crewlet-app-shell__content")?.children.length,
        hash,
      ).toBeGreaterThan(0);
      expect(view.container.textContent, hash).not.toMatch(/there is no such screen/);
      // AND NOT A BOUNDARY'S FALLBACK. A screen that throws is caught now,
      // and a caught failure is not a blank — so without this line a screen
      // that threw on an empty engine would pass the check above.
      expect(view.container.textContent, hash).not.toMatch(/could not be (drawn|loaded)/);
      view.unmount();
    }
    // A MOUNT OF THE WHOLE APPLICATION PER DESTINATION, some forty of them in
    // one case: about 1.5 s alone and past vitest's 5 s default when the full
    // suite shares the machine, which failed it on load rather than on a
    // blank screen. The budget is the case's own, sized to its loop.
  }, 30_000);

  // THE OTHER HALF OF `source.test.ts`'s link gate. That one proves every
  // link names a segment a workspace OWNS; this one proves the object routes
  // those links actually build are DRAWN. A workspace can own `live` while a
  // trace still falls through to Not Found, which is exactly what shipped once:
  // `traces` had a screen, four buttons pointing at it, and no case in the
  // switch.
  test("every object route a link builds resolves to a screen", () => {
    const routes = [
      ["live", "turns", "11111111-1111-4111-8111-111111111111"],
      ["live", "traces", "22222222-2222-4222-8222-222222222222"],
      ["live", "runs", "33333333-3333-4333-8333-333333333333"],
      ["live", "events", "44444444-4444-4444-8444-444444444444"],
      ["live", "a2a", "55555555-5555-4555-8555-555555555555"],
      ["agents", "schedules", "role", "ceo", "standup"],
      ["agents", "seats", "ada"],
      ["agents", "teams", "platform"],
      ["work", "ENG-42"],
      ["knowledge", "pages", "66666666-6666-4666-8666-666666666666"],
      ["settings", "nodes", "node-a"],
      ["settings", "integrations", "slack"],
      ["settings", "secrets", "SLACK_SIGNING_SECRET"],
      ["settings", "config", "revisions", "77777777-7777-4777-8777-777777777777"],
    ];
    for (const path of routes) {
      const hash = buildHash(path);
      location.hash = hash;
      const { view } = mount();
      expect(view.container.textContent, hash).not.toMatch(/there is no such screen/);
      // AND NOT A BOUNDARY'S FALLBACK. A screen that throws is caught now,
      // and a caught failure is not a blank — so without this line a screen
      // that threw on an empty engine would pass the check above.
      expect(view.container.textContent, hash).not.toMatch(/could not be (drawn|loaded)/);
      view.unmount();
    }
  });

  test("an unknown screen says so instead of rendering nothing", () => {
    location.hash = "#/nonsense";
    mount();
    expect(screen.getByText(/there is no such screen/)).toBeDefined();
  });

  // A TAIL NOTHING ROUTES IS NOT THE WORKSPACE'S LANDING SCREEN. Spend had a
  // two-valued test once — `budgets` or the spend screen — so the obvious
  // typo, the shape of a stale bookmark, drew the spend tables under a trail
  // naming something else: the address, the trail and the screen each naming
  // a different page.
  test("a tail under Spend that names no screen says so", () => {
    location.hash = "#/spend/budget";
    mount();
    expect(screen.getByText(/there is no such screen/)).toBeDefined();
    // …and the two addresses that DO exist are untouched.
    cleanup();
    location.hash = "#/spend";
    mount();
    expect(screen.queryByText(/there is no such screen/)).toBeNull();
    cleanup();
    location.hash = "#/spend/budgets";
    mount();
    expect(screen.queryByText(/there is no such screen/)).toBeNull();
  });

  // THE SAME SHAPE ONE CASE OVER. `revisions/{id}` is the only tail Config
  // has, and reading it as `tail[0] === "revisions"` alone rendered the config
  // screen with the segment dropped for everything else.
  test("a tail under Config that names no screen says so", () => {
    location.hash = "#/settings/config/nonsense";
    mount();
    expect(screen.getByText(/there is no such screen/)).toBeDefined();
  });

  /**
   * A TOOL NAME IS A THIRD PARTY'S STRING, and `servers` is a legal one.
   *
   * `#/settings/tools/servers/{name}` filters the catalogue by origin and
   * `#/settings/tools/{name}` is one tool's page — the address `objects.ts`
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

    location.hash = "#/settings/tools/servers";
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

    location.hash = "#/settings/tools/servers/github";
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
    location.hash = "#/agents/seats/ghost";
    mount();
    expect(screen.getByText(/No seat called/)).toBeDefined();
  });
});

// A GUARDED SECTION FOR A READER THE ENGINE SAYS HOLDS NO CREDENTIAL IS ITS
// REFUSAL AND NOTHING ELSE. Each guarded screen used to mount, ask, be refused
// and draw the refusal as data: Nodes read "0 nodes", four tiles of 0 and "No
// nodes are reporting", under a banner calling the refusal the last reading
// that succeeded. Derived from the table, so a section marked guarded later is
// held to this without anybody remembering to add it here.
describe("a guarded section, for a reader without an operator credential", () => {
  const settings = WORKSPACES.find((row) => row.key === "settings")!;
  const guarded = settings.sections.filter((s) => s.guarded && !s.answersRefusal);

  const anonymous = { operator_id: "", operator: false };

  test("the table still guards the sections it did", () => {
    expect(guarded.map((s) => s.key)).toEqual(
      expect.arrayContaining(["integrations", "secrets", "nodes", "config", "backups", "audit"]),
    );
  });

  test("every guarded section draws the one refusal, and not a figure", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response(JSON.stringify({ error: "unauthorized" }), { status: 401 })),
    );
    for (const section of guarded) {
      location.hash = buildHash(section.path);
      const { view } = mountAs(Promise.resolve(anonymous));
      const content = () => view.container.querySelector(".crewlet-app-shell__content")!;
      expect(
        await screen.findByText(`${section.label} needs an operator credential`),
        section.key,
      ).toBeDefined();
      expect(screen.getByRole("button", { name: "Set token" })).toBeDefined();
      // NOTHING THE SCREEN WOULD HAVE COUNTED: no tile, no chip in the header
      // slot, no empty state claiming the section holds nothing.
      expect(content().querySelector(".crewlet-stat-card"), section.key).toBeNull();
      expect(document.getElementById(PAGE_ACTIONS_SLOT)?.textContent, section.key).toBe("");
      expect(content().textContent, section.key).not.toMatch(/\b0 (nodes?|credentials?)\b/);
      expect(content().textContent, section.key).not.toMatch(/No nodes are reporting/);
      view.unmount();
      cleanup();
    }
    vi.unstubAllGlobals();
  });

  // THE FIRST ANSWER IS WAITED FOR. A cold load of a guarded section sent its
  // guarded reads before the frame knew who was asking, was refused, and drew
  // the refusal a frame before the frame's own — so the section asks nothing
  // until the viewer has answered.
  test("while the first viewer read is out, a guarded section asks nothing", async () => {
    location.hash = "#/settings/nodes";
    const asked: string[] = [];
    const store = new Store();
    const socket = new LiveSocket(store);
    (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) => {
      asked.push(what);
      return what === "viewer" ? new Promise(() => {}) : Promise.resolve({});
    };
    render(
      <ClientContext.Provider value={{ store, socket }}>
        <Router>
          <App />
        </Router>
      </ClientContext.Provider>,
    );
    for (let i = 0; i < 6; i++) await Promise.resolve();
    expect(screen.getByText("Checking your access to Nodes")).toBeDefined();
    expect(asked).not.toContain("fleet");
  });

  // A FAILED VIEWER READ IS NOT REFUSED either — it would lock an operator out
  // until the next poll — so the screen mounts, and ITS refused reads are its
  // to say. They say it without a figure: a refused read is not "0 nodes",
  // "0 credentials held" or "0 of 6 configured".
  test("a viewer read that failed mounts the screen, which counts nothing it was refused", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response(JSON.stringify({ error: "unauthorized" }), { status: 401 })),
    );
    for (const section of guarded) {
      location.hash = buildHash(section.path);
      const { view } = mountAs(Promise.reject(new QueryError("timeout")));
      const content = () => view.container.querySelector(".crewlet-app-shell__content")!;
      for (let i = 0; i < 8; i++) await Promise.resolve();
      await new Promise((r) => setTimeout(r, 0));
      expect(
        screen.queryByText(`${section.label} needs an operator credential`),
        section.key,
      ).toBeNull();
      expect(
        screen.queryByText(`Checking your access to ${section.label}`),
        section.key,
      ).toBeNull();
      // A COUNT IN THE HEADER IS A TAG ("0 nodes", "0 credentials held",
      // "0 of 6 configured"); a time-range control there is not a figure.
      expect(
        document.getElementById(PAGE_ACTIONS_SLOT)?.querySelector(".crewlet-tag"),
        section.key,
      ).toBeNull();
      expect(content().querySelector(".crewlet-stat-card"), section.key).toBeNull();
      view.unmount();
      cleanup();
    }
    vi.unstubAllGlobals();
  });

  test("an operator is given the screen", async () => {
    location.hash = "#/settings/nodes";
    mountAs(Promise.resolve({ operator_id: "ops", operator: true }));
    expect(await screen.findByText(/Seat ownership is a lease/)).toBeDefined();
    expect(screen.queryByText("Nodes needs an operator credential")).toBeNull();
  });

  // THE CHARTER IS PUBLIC, and so is the tool registry: neither is guarded.
  test("General and Tools are open to anybody", async () => {
    for (const path of [["settings"], ["settings", "tools"]]) {
      location.hash = buildHash(path);
      const { view } = mountAs(Promise.resolve(anonymous));
      await Promise.resolve();
      await Promise.resolve();
      expect(view.container.textContent, path.join("/")).not.toMatch(
        /(General|Tools) needs an operator credential/,
      );
      view.unmount();
      cleanup();
    }
  });
});

describe("live state reaches the screen", () => {
  test("a pushed roster renders its seats", () => {
    // THE HASH BEFORE THE MOUNT. The router reads `location.hash` at mount
    // and then listens for `hashchange`, which jsdom dispatches
    // asynchronously — so assigning it after mounting and re-rendering
    // synchronously renders the screen the reader was on before.
    location.hash = "#/agents/roster";
    const { store, view } = mount();
    store.applyOrg({
      name: "Acme",
      roles: [{ name: "CEO", handle: "ceo", goal: "Set direction" }],
    });
    store.applyAgents([{ role: "CEO", activity: "working" }]);
    view.rerender(
      <ClientContext.Provider value={{ store, socket: new LiveSocket(store) }}>
        <Router>
          <App />
        </Router>
      </ClientContext.Provider>,
    );
    expect(screen.getAllByText("CEO").length).toBeGreaterThan(0);
  });

  // THE STAR'S WHOLE ROUND TRIP: the page header's star makes one, and the
  // sidebar's Starred section — drawn only when there is one — reads it back.
  test("keeping a page puts it in the sidebar's Starred", async () => {
    localStorage.clear();
    resetStarsForTest();
    location.hash = "#/agents/seats/ceo";
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

  // HANDLES LIVE ONLY UNDER `seats/`, which is what lets a person be called
  // `roster`: the resolver reads the segment at the handle's position and
  // never compares it with a reserved word. Rendered end to end, because the
  // failure this guards is a screen rather than a route — the roster drawn
  // under a trail naming a person, or a person's page that never opens.
  test("a seat whose handle is a reserved word opens its own page", () => {
    location.hash = "#/agents/seats/roster";
    const { store, view } = mount();
    store.applyOrg({
      name: "Acme",
      roles: [{ name: "Rota Keeper", handle: "roster", goal: "Keep the rota" }],
    });
    view.rerender(
      <ClientContext.Provider value={{ store, socket: new LiveSocket(store) }}>
        <Router>
          <App />
        </Router>
      </ClientContext.Provider>,
    );
    expect(view.container.querySelector(".object-head")?.textContent).toContain("Rota Keeper");
    expect(screen.queryByText(/No seat called/)).toBeNull();
    expect(screen.queryByText(/there is no such screen/)).toBeNull();
    // AND NOT THE ROSTER: the page is named for the person — ONCE. The last
    // crumb is the page's `h1`, and the object header under it names the
    // same seat at level two; two `h1`s reading the same words were two page
    // titles to a reader navigating by heading.
    const titles = screen.getAllByRole("heading", { level: 1 });
    expect(titles.map((h) => h.textContent)).toEqual(["Rota Keeper"]);
  });

  test("a seat opened on a tab it does not have still has a page under it", () => {
    // THE BLANK PAGE, end to end. `tab=` is a string off a URL and the tab
    // set belongs to the seat's kind, so `?tab=zzz` — a bookmark, a typed
    // URL, a link made before the kind changed — used to select no tab and
    // match no branch: the header and the strip, and then nothing.
    location.hash = "#/agents/seats/ceo?tab=zzz";
    const { store, view } = mount();
    store.applyOrg({
      name: "Acme",
      roles: [{ name: "CEO", handle: "ceo", goal: "Set direction" }],
    });
    store.applyAgents([{ role: "CEO", activity: "working" }]);
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
    location.hash = "#/agents/seats/ceo?tab=turns";
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
        activity: "working",
        live_call: {
          turn_id: "t1",
          phase: "review",
          iteration: 1,
          model: "claude-sonnet-5",
          in_progress: true,
          round_num: 0,
          rounds_used: 1,
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
    store.applyAgents([{ role: "CEO", activity: "idle", live_call: null }] as never);
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
        activity: "working",
        live_call: {
          turn_id: "t1",
          phase: "execute",
          iteration: 1,
          model: "claude-sonnet-5",
          in_progress: true,
          round_num: 0,
          rounds_used: 1,
          started_at: "2026-01-01T00:00:01Z",
          updated_at: "2026-01-01T00:00:02Z",
        },
      },
    ] as never);
    redraw();
    store.applyEvent(phaseEnvelope("p1", "execute", "2026-01-01T00:00:05Z") as never);
    store.applyAgents([{ role: "CEO", activity: "idle", live_call: null }] as never);
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

/**
 * A NODE ID IS AN OPERATOR'S STRING, and a domain's name is the engine's.
 *
 * The fleet page once held both under one segment and told them apart by the
 * tail's length, so every address under it that was not a node rendered the
 * NODE screen for a node named after its first segment. Domains live only
 * under `backups/` now: a node called `backups` is a node, and a domain is
 * never read as one.
 */
// AS AN OPERATOR: both are guarded sections, and the frame draws a guarded
// section only once it knows the reader may read it.
test("a node named `backups` keeps its page, and a domain keeps its own", async () => {
  const operator = () => mountAs(Promise.resolve({ operator_id: "ops", operator: true }));
  const settle = async () => {
    for (let i = 0; i < 6; i++) await act(async () => Promise.resolve());
  };
  location.hash = "#/settings/nodes/backups";
  const one = operator();
  await settle();
  // ONE SEGMENT UNDER NODES IS A NODE, whatever it is called.
  expect(one.view.container.textContent).toContain("All nodes");
  expect(one.view.container.textContent).not.toContain("there is no such screen");
  one.view.unmount();
  cleanup();

  // A DOMAIN IS UNDER BACKUPS, and it is not a node read.
  location.hash = "#/settings/backups/tracker";
  const two = operator();
  await settle();
  expect(two.view.container.textContent).not.toContain("All nodes");
  expect(two.view.container.textContent).not.toContain("holds a lease");
  two.view.unmount();
  cleanup();

  // AND A TAIL THAT NAMES NEITHER SAYS SO, rather than dropping segments.
  location.hash = "#/settings/backups/tracker/extra";
  operator();
  await settle();
  expect(screen.getByText(/there is no such screen/)).toBeDefined();
});
