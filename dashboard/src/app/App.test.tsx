/**
 * The application mounts, routes, and survives an empty engine.
 *
 * A smoke test rather than a screenshot: what it catches is a broken import, a
 * hook-order violation, and — the one that actually happens — a screen that
 * throws on the state a FRESHLY STARTED company is in, where every list is
 * empty and no query has answered yet. That state is the first thing anybody
 * sees, and it is the one least likely to be exercised by hand.
 */

import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  waitForElementToBeRemoved,
  within,
} from "@testing-library/react";
import { afterEach, beforeAll, beforeEach, describe, expect, test, vi } from "vitest";
import { App } from "./App.tsx";
import { Router } from "./router.tsx";
import { screenScroller } from "~/lib/scroller.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, QueryError, Store, sessionRestored } from "~/protocol/index.ts";
import { DESTINATIONS, WORKSPACES, closedBy } from "./nav.ts";
import { accessNames } from "~/lib/access.ts";
import { PAGE_ACTIONS_SLOT } from "./frame/PageActions.tsx";
import { buildHash } from "./router.tsx";
import { noteReader } from "~/lib/reader.ts";
import { resetForTest as resetStarsForTest } from "~/lib/starred.ts";
import { CHUNKS, loadChunk } from "./lazyScreen.ts";
import { TOUCH_MEDIA } from "./layout.ts";
import { healthFrame } from "~/test/health.ts";
import { withDerived } from "~/test/org.ts";
import { ZERO_VERSIONS } from "~/test/liveCall.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

/** An admin's `viewer` answer: every screen is theirs to read. EXPANDED, as
 *  the engine reports an admin's grants: every app with it. */
const ADMIN_VIEWER = {
  login: "founder",
  owner: "jane",
  handle: "jane",
  grants: ["admin", "worklet", "doclet", "chatlet"],
};

/**
 * The app over a socket that never connects — and, where a case reads a
 * screen only an admin reads, one whose `viewer` question answers `viewer`.
 */
function mount(viewer: unknown = null) {
  const store = new Store();
  const socket = new LiveSocket(store);
  if (viewer !== null) {
    const real = socket.query.bind(socket);
    (socket as unknown as { query: (what: string, ...rest: unknown[]) => Promise<unknown> }).query =
      (what, ...rest) =>
        what === "viewer"
          ? Promise.resolve(viewer)
          : (real as (w: string, ...r: unknown[]) => Promise<unknown>)(what, ...rest);
  }
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
  // THE SOCKET'S FIRST FRAME CARRIES THE ORG, as a served frame's always
  // does: what the company runs is known before a read is asked.
  store.applyOrg(withDerived({ name: "Acme", roles: [] }));
  store.applyHealth(healthFrame({ nodes: 1, applied_epoch: 2 }));
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
  sessionStorage.clear();
  // A SESSION NEED IS A STATE THE TRANSPORTS KEEP FOR THE TAB
  // (`protocol/signin.ts`), so a case whose engine answered `401` would send
  // every later case to the sign-in.
  sessionRestored();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

describe("the shell", () => {
  test("mounts against an engine that has answered nothing", () => {
    mount();
    // The chrome is present and honest: not connected yet, and saying so, in
    // the rail head's engine status and in the landing screen's own sentence.
    // A tab still dialling has lost nothing, so the status says it is
    // connecting, as its name and its title.
    expect(screen.getAllByTitle("Connecting").length).toBeGreaterThan(0);
    // AND THE LANDING SCREEN IS HOME, whose one sentence an engine condition
    // takes over rather than sitting beside a claim that all is well.
    expect(screen.getByText(/Not connected\. Showing the last state received/)).toBeDefined();
  });

  test("a disconnected engine is itself the first thing needing a person", () => {
    // It is not a footnote in a popover: the page cannot tell the truth about
    // anything else while the socket is down, so it leads.
    mount();
    expect(screen.getByText("Not connected. Showing the last state received.")).toBeDefined();
  });

  test("every workspace is in the sidebar, Settings included", async () => {
    // A SECTION THAT VANISHES without its grant is indistinguishable from one
    // that does not exist, so Settings is always a row. DERIVED FROM THE
    // TABLE: a list written here was one workspace short the day Chatlet
    // arrived, and passed.
    mountAs(Promise.resolve({ login: "jane.doe", owner: "jane.doe", grants: [] }));
    const nav = screen.getByRole("navigation", { name: "Navigation" });
    const rows = WORKSPACES.filter((ws) => ws.place !== "foot" && ws.place !== "menu");
    expect(rows.map((ws) => ws.key)).toContain("chatlet");
    for (const ws of rows) {
      expect(nav.textContent, ws.label).toContain(ws.label);
    }
    // AT THE FOOT, AS ITS GLYPH beside the reader's own row: a link named in words.
    const foot = document.querySelector<HTMLElement>(".crewlet-app-shell__rail-foot")!;
    expect(within(foot).getByRole("link", { name: "Settings" })).toBeTruthy();
  });

  // A LOCK GOES WHERE THE CLOSED THING IS. This reader holds none of the
  // grants most Settings sections need, and the row said so with a key — so a
  // person looking for their own Account, the first row of its You group,
  // read Settings as closed to them. Settings is open (its You group needs
  // nothing), and its column locks each section that is not.
  test("Settings is not locked as a whole; its column locks what is closed", async () => {
    location.hash = "#/settings";
    mountAs(Promise.resolve({ login: "jane.doe", owner: "jane.doe", grants: [] }));
    const column = await screen.findByRole("navigation", { name: "Settings sections" });
    await waitFor(() =>
      expect(within(column).getByRole("link", { name: /^Nodes/ }).textContent).toMatch(
        /Needs Admin/,
      ),
    );
    // The foot's Settings names no lock: its title is its name alone.
    const foot = document.querySelector<HTMLElement>(".crewlet-app-shell__rail-foot")!;
    const settings = within(foot).getByRole("link", { name: "Settings" });
    expect(settings.getAttribute("title")).toBe("Settings");
  });
});

// ONE STACKING CONTEXT FOR THE WHOLE LAYER BAND. Every overlay is portalled to
// `document.body`, where its z-index is its depth on the kit's one layer stack.
// The narrow layout's drawer is on that stack too, but it is the sidebar itself
// and is painted in place, in the root context — so an application-wide
// `LayerHost` (a stacking context at the bottom of the band) drew every overlay
// opened from the drawer UNDER it: on a phone the Account popover's sign-outs
// and the palette's results could not be tapped. What paints above what is the
// browser's to measure; what this holds is the one structural fact it needs.
describe("the overlays the frame opens", () => {
  const person = {
    login: "jane.doe",
    owner: "jane",
    handle: "jane",
    name: "Jane",
    grants: [],
  };

  test("the reader's menu and the preferences are portalled to the body, under no layer host", async () => {
    mountAs(Promise.resolve(person));
    fireEvent.click(await screen.findByRole("button", { name: /your menu/ }));
    const menu = await screen.findByRole("menu", { name: "Your account" });
    expect(menu.parentElement).toBe(document.body);
    expect(menu.closest(".crewlet-layer-host")).toBeNull();
    fireEvent.keyDown(menu, { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("menu")).toBeNull());
    fireEvent.click(screen.getByRole("button", { name: "Preferences" }));
    const popover = await screen.findByRole("dialog", { name: "Preferences" });
    expect(popover.parentElement).toBe(document.body);
    expect(popover.closest(".crewlet-layer-host")).toBeNull();
    expect(document.querySelectorAll(".crewlet-layer-host")).toHaveLength(0);
  });

  test("the palette opened from the sidebar's search is portalled to the body too", async () => {
    mountAs(Promise.resolve(person));
    fireEvent.click(await screen.findByRole("button", { name: /Ask or jump to/ }));
    const palette = await screen.findByRole("dialog", { name: "Search" });
    expect(palette.closest(".crewlet-layer-host")).toBeNull();
    expect(palette.closest(".crewlet-modal-overlay")?.parentElement).toBe(document.body);
  });
});

// A KEYCAP IS A HINT FOR A KEYBOARD. On a phone the drawer's search drew
// "Ctrl + K", a key nobody there has, and the keycap cut the trigger's own
// label to "Ask or jump t…". Where the reader's primary input is touch
// (`TOUCH_MEDIA`) the frame draws none; everywhere else it does. Mutation:
// draw the keycap whatever the input and the touch case goes red.
describe("the drawer's search", () => {
  const touch = (on: boolean) => {
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
    return () => {
      if (had) Object.defineProperty(globalThis, "matchMedia", had);
    };
  };

  test.each([
    [true, 0],
    [false, 1],
  ])("on a touch device (%s) draws %d keycap", (on, caps) => {
    const restore = touch(on);
    try {
      mount();
      const search = document.querySelector(".crewlet-app-shell__rail-search")!;
      expect(search.textContent).toContain("Ask or jump to…");
      expect(search.querySelectorAll(".crewlet-kbd-combo")).toHaveLength(caps);
    } finally {
      restore();
    }
  });
});

describe("routing", () => {
  // DERIVED FROM THE DESTINATIONS TABLE, not a hand-written list. The list
  // was one, and a hand-written one covers exactly the screens somebody
  // remembered to add to it — so a new destination that renders a blank ships
  // green, which is the one failure this test exists to catch.
  // WHO IS WORKING IS HOME'S, as the Main artboard draws it — the sidebar's
  // Live row carries the count everywhere else. Drawn in every bar it sat
  // between a task's trail and its actions, and pushed a profile's controls
  // past a phone's edge.
  test("the working chip is in Home's page bar and in no other", async () => {
    for (const [hash, drawn] of [
      ["#/", true],
      ["#/worklet", false],
      ["#/company/org", false],
      ["#/worklet/ENG", false],
      ["#/company/org/seats/agent-swe", false],
    ] as const) {
      location.hash = hash;
      const { store, view } = mount();
      // AWAITED: a screen reads its chunk through `use`, and a synchronous
      // act stops flushing at the first render that used a promise, settled
      // or not, so the re-render this push causes would be left unflushed.
      await act(async () => {
        store.applySeats([
          { agent_id: "a1", id: "a1", role: "Agent SWE", activity: "working" },
        ] as never);
      });
      const chip = view.container.querySelector(".page-bar .working-now");
      expect(chip !== null, hash).toBe(drawn);
      view.unmount();
    }
  });

  test("every destination renders a screen rather than a blank", async () => {
    const visited = DESTINATIONS.map((item) => buildHash(item.path));
    // The two-level routes are the reason the list is derived: a hand-written
    // one would not have them.
    expect(visited).toContain(buildHash(["worklet", "views"]));
    expect(visited).toContain(buildHash(["live", "turns"]));
    for (const hash of visited) {
      location.hash = hash;
      const { view } = mount();
      // AWAITED, for the reason the working chip's case gives: what a screen
      // renders after its first pass is flushed only by an awaited act.
      await act(async () => {});
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
  });

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
      ["company", "schedules", "role", "ceo", "standup"],
      ["company", "org", "seats", "ada"],
      ["company", "org", "teams", "platform"],
      ["worklet", "ENG-42"],
      ["doclet", "pages", "66666666-6666-4666-8666-666666666666"],
      ["chatlet", "88888888-8888-4888-8888-888888888888"],
      ["settings", "nodes", "node-a"],
      ["company", "integrations", "slack"],
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
    location.hash = "#/settings/spend/budget";
    mount();
    expect(screen.getByText(/there is no such screen/)).toBeDefined();
    // …and the two addresses that DO exist are untouched.
    cleanup();
    location.hash = "#/settings/spend";
    mount();
    expect(screen.queryByText(/there is no such screen/)).toBeNull();
    cleanup();
    location.hash = "#/company/budgets";
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
   * `#/settings/mcp-tools/servers/{name}` filters the catalogue by origin and
   * `#/settings/mcp-tools/{name}` is one tool's page, the address `objects.ts`
   * builds and where a tool row's `Open ↗` goes. Discriminating on the WORD
   * took the page away from a tool actually called `servers` and dropped the
   * reader on the unfiltered catalogue, which is the regression the switch's
   * own comment says it fixed. The tail's LENGTH is what tells them apart:
   * the router encodes each segment whole, so a tool page is always exactly
   * one tail segment and the filter always two.
   */
  test("a tool named `servers` keeps its page, and the origin filter keeps its", async () => {
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

    location.hash = "#/settings/mcp-tools/servers";
    const one = mount(ADMIN_VIEWER);
    one.store.applyTools(catalogue);
    one.view.rerender(
      <ClientContext.Provider value={{ store: one.store, socket: one.socket }}>
        <Router>
          <App />
        </Router>
      </ClientContext.Provider>,
    );
    // The addressed tool is drawn above the catalogue in its own header —
    // once the viewer has said this reader is an admin, whose screen it is.
    await waitFor(() =>
      expect(one.view.container.querySelector(".crewlet-object-header")).not.toBeNull(),
    );
    one.view.unmount();
    cleanup();

    location.hash = "#/settings/mcp-tools/servers/github";
    const two = mount(ADMIN_VIEWER);
    two.store.applyTools(catalogue);
    two.view.rerender(
      <ClientContext.Provider value={{ store: two.store, socket: two.socket }}>
        <Router>
          <App />
        </Router>
      </ClientContext.Provider>,
    );
    // Two segments is the FILTER, so no tool is addressed and the catalogue
    // stands alone.
    await waitFor(() => expect(screen.queryByText("Checking your access to MCP Tools")).toBeNull());
    expect(two.view.container.querySelector(".prof-head")).toBeNull();
  });

  test("a seat that does not exist explains itself", () => {
    location.hash = "#/company/org/seats/ghost";
    mount();
    expect(screen.getByText(/No seat called/)).toBeDefined();
  });
});

// A SECTION FOR A READER WHO HOLDS NONE OF ITS GRANTS IS ITS REFUSAL AND
// NOTHING ELSE. Each guarded screen used to mount, ask, be refused and draw the
// refusal as data: Nodes read "0 nodes", four tiles of 0 and "No nodes are
// reporting", under a banner calling the refusal the last reading that
// succeeded. Derived from the table, so a section given a grant later is held
// to this without anybody remembering to add it here.
describe("a section, for a reader holding none of its grants", () => {
  const settings = WORKSPACES.find((row) => row.key === "settings")!;
  const guarded = settings.sections.filter((s) => s.grants);
  // EVERY WORKSPACE'S, for the refusal itself: the screen is not mounted, so
  // what it would have drawn is nobody's question.
  const everywhere = WORKSPACES.flatMap((row) => row.sections).filter((s) => s.grants);

  /** Signed in, and holding nothing. */
  const reader = { login: "reader", owner: "reader", grants: [] };

  /** What a screen's banner says of a read refused on no access it can name. */
  const REFUSED_UNNAMED = "You don’t have access to this.";

  /** What only a reader nobody signed in for is told. */
  const SIGN_IN = "Sign in to see this.";

  test("the table still names a grant for the sections it guarded", () => {
    expect(guarded.map((s) => s.key)).toEqual(
      expect.arrayContaining([
        "people",
        "service-accounts",
        "tokens",
        "mcp-tools",
        "limits",
        "storage",
        "secrets",
        "nodes",
        "config",
        "backups",
        "audit",
      ]),
    );
    // AND THE AUDIT TRAIL'S READS ARE ADMIN'S, wherever they sit.
    expect(everywhere.map((s) => s.key)).toEqual(
      expect.arrayContaining(["turns", "a2a", "events", "diaries"]),
    );
  });

  test("every such section draws the one refusal naming its grant, and not a figure", async () => {
    // `403`, NEVER `401`: this reader is signed in and holds too little, and a
    // `401` says nobody is — which sends the tab to the sign-in instead.
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response(JSON.stringify({ error: "unauthorized" }), { status: 403 })),
    );
    for (const section of everywhere) {
      location.hash = buildHash(section.path);
      // HOLDING NO ACCESS AT ALL, which the socket serves the overview: what
      // such a reader lacks is the section's own access, which is what its
      // lock names (`sectionOpen`).
      const { view } = mountAs(Promise.resolve({ ...reader, grants: [] }));
      const content = () => view.container.querySelector(".crewlet-app-shell__content")!;
      // THE CLOSED THING IS NAMED: the section, or — for a section in a
      // workspace the reader cannot open at all — the workspace.
      const closed = closedBy(section.path, [])!;
      expect(closed, section.key).not.toBeNull();
      expect(
        await screen.findByText(`You need ${accessNames(closed.needs)} access`),
        section.key,
      ).toBeDefined();
      // THE COMMON WAY OUT IS HOME, and switching account the other, never a
      // token; the reader is named, from the reading the refusal was decided on.
      expect(screen.getByRole("link", { name: "Go to Home" })).toBeDefined();
      expect(screen.getByRole("button", { name: "Switch account" })).toBeDefined();
      expect(content().textContent, section.key).toMatch(/Signed in as reader/);
      // NOTHING THE SCREEN WOULD HAVE COUNTED: no tile, no chip in the header
      // slot, no empty state claiming the section holds nothing.
      expect(content().querySelector(".crewlet-statcard"), section.key).toBeNull();
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
  //
  // EACH STEP IS WAITED FOR AS THE EVENT IT IS, never as a number of ticks.
  // React commits what a settled read changed on a scheduler task of its own,
  // and its scheduler yields to the event loop once five milliseconds of its
  // clock have passed, so on a loaded runner the frame's commit landed a task
  // behind a fixed `setTimeout(0)` and the case read the frame still
  // checking: "Checking your access to File storage", on a run whose screen
  // mounted a moment later.
  test("a viewer read that failed mounts the screen, which counts nothing it was refused", async () => {
    // `403`: a `401` says nobody is signed in, which sends the tab to the
    // sign-in rather than leaving the screen to say what it was refused.
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response(JSON.stringify({ error: "unauthorized" }), { status: 403 })),
    );
    for (const section of guarded) {
      location.hash = buildHash(section.path);
      const { view } = mountAs(Promise.reject(new QueryError("timeout")));
      const content = () =>
        view.container.querySelector<HTMLElement>(".crewlet-app-shell__content")!;
      // THE FRAME CHECKS FIRST AND THEN GIVES WAY: the read is still out at
      // the first render, so the skeleton is there to be removed, and its
      // removal is the commit that decided what replaces it.
      await waitForElementToBeRemoved(
        () => screen.queryByText(`Checking your access to ${section.label}`),
        { onTimeout: (e) => new Error(`${section.key}: ${e.message}`) },
      );
      // WHAT REPLACED IT IS THE SCREEN, and not the frame's refusal, which
      // that same commit would have drawn.
      expect(
        screen.queryByText(`You need ${accessNames(section.grants!)} access`),
        section.key,
      ).toBeNull();
      // AND THE SCREEN HAS SAID WHAT IT WAS REFUSED before anything below
      // looks for a figure: looked for sooner, nothing had answered that
      // could have drawn one, and the case passed having watched nothing.
      // Every read here is a 403 naming no access, which every screen says
      // as no access, and NEVER AS A SIGN-IN: this reader is signed in, and
      // Audit told them to sign in where every other screen did not.
      await waitFor(() =>
        expect(
          within(content()).queryAllByText(REFUSED_UNNAMED),
          `${section.key} said nothing of what it was refused`,
        ).not.toEqual([]),
      );
      expect(within(content()).queryByText(SIGN_IN), section.key).toBeNull();
      // A COUNT IN THE HEADER IS A TAG ("0 nodes", "0 credentials held",
      // "0 of 6 configured"); a time-range control there is not a figure.
      // Tools' figures are the registry the socket pushes every reader — no
      // read was refused for them — so they are the figures kept.
      if (section.key !== "mcp-tools") {
        expect(
          document.getElementById(PAGE_ACTIONS_SLOT)?.querySelector(".crewlet-tag"),
          section.key,
        ).toBeNull();
        expect(content().querySelector(".crewlet-statcard"), section.key).toBeNull();
      }
      view.unmount();
      cleanup();
    }
    vi.unstubAllGlobals();
  });

  // SETTINGS LISTS ONLY WHAT LIVES IN IT, in its four groups, and for a reader
  // holding no permission it is not nothing but locks: Spend's readings are
  // every reader's. Each row's title is its hint, the words the palette shows.
  test("the Settings column lists only what lives in Settings, Spend open to every reader", async () => {
    location.hash = "#/settings/models";
    mountAs(Promise.resolve({ ...reader, grants: [] }));
    const column = await screen.findByRole("navigation", { name: "Settings sections" });
    const rows = within(column).getAllByRole("link");
    expect(rows[0]?.getAttribute("href")).toBe("#/settings/models");
    for (const row of rows) expect(row.getAttribute("href")).toMatch(/^#\/settings\//);
    for (const group of ["Runtime", "Spend", "Access", "Engine"]) {
      expect(column.querySelector(`.crewlet-nav-group__label`)).not.toBeNull();
      expect(within(column).getAllByText(group).length).toBeGreaterThan(0);
    }
    const row = (href: string) => rows.find((a) => a.getAttribute("href") === href);
    await waitFor(() => expect(row("#/settings/people")?.textContent).toMatch(/Needs Admin/));
    expect(row("#/settings/spend")?.textContent).toBe("Token spend");
    expect(row("#/settings/people")?.getAttribute("title")).toBe(
      settings.sections.find((s) => s.key === "people")!.hint,
    );
  });

  // EVERY SETTINGS SECTION BUT SPEND'S READINGS IS ADMIN'S, and Expensive
  // tasks names the tracker's tasks: a reader holding nothing is told so on
  // each row, and the readings carry no lock.
  test("for a reader holding nothing, only the sections that need a grant are locked", async () => {
    location.hash = "#/settings/models";
    mountAs(Promise.resolve(reader));
    const column = await screen.findByRole("navigation", { name: "Settings sections" });
    const row = (href: string) =>
      within(column)
        .getAllByRole("link")
        .find((a) => a.getAttribute("href") === href);
    await waitFor(() => expect(row("#/settings/people")?.textContent).toMatch(/Needs Admin/));
    expect(row("#/settings/mcp-tools")?.textContent).toMatch(/Needs Admin access/);
    expect(row("#/settings/limits")?.textContent).toMatch(/Needs Admin access/);
    expect(row("#/settings/spend/tasks")?.textContent).toMatch(/Needs Worklet access/);
    expect(row("#/settings/spend")?.textContent).toBe("Token spend");
  });

  test("an operator is given the screen", async () => {
    location.hash = "#/settings/nodes";
    mountAs(
      Promise.resolve({
        login: "ops",
        owner: "ops",
        grants: ["admin", "worklet", "doclet"],
      }),
    );
    expect(await screen.findByRole("navigation", { name: "Related" })).toBeDefined();
    expect(screen.queryByText("Nodes needs Admin")).toBeNull();
  });

  // AN APP'S WORKSPACE IS CLOSED WHOLE, its every page included — a chat
  // ROOM too, which is the page a chat reader lives on and sits under no
  // section's address of its own (`sectionOf` files it under the landing's).
  // The frame draws its one refusal naming the app in place of the screen,
  // and the screen mounts nothing: no rail, no room.
  test("a chat room draws the one refusal naming Chatlet, and not the room", async () => {
    location.hash = buildHash(["chatlet", "88888888-8888-4888-8888-888888888888"]);
    const { view } = mountAs(Promise.resolve({ ...reader, grants: ["worklet", "doclet"] }));
    expect(await screen.findByText("You need Chatlet access")).toBeDefined();
    expect(view.container.querySelector(".chat-frame")).toBeNull();
    expect(view.container.querySelector('nav[aria-label="Your rooms"]')).toBeNull();
  });

  // THE CHARTER IS PUBLIC; the tool registry is Admin's, since what each
  // seat may call is how the company is run.
  //
  // PRINCIPLES ARE JUDGED ON THE ANSWER, so the case waits for the frame to
  // have applied it: the sidebar locking Worklet is that answer drawn. Judged
  // any sooner, the frame has not been told who is reading, and the page is
  // open to them whatever its rule says.
  test("the company's Principles are open to anybody, and MCP Tools is Admin's", async () => {
    location.hash = buildHash(["company"]);
    const principles = mountAs(Promise.resolve({ login: "", owner: "", grants: [] }));
    const sidebar = await screen.findByRole("navigation", { name: "Navigation" });
    await waitFor(() =>
      expect(
        within(sidebar)
          .getByRole("link", { name: /^Worklet/ })
          .querySelector("[title='Needs Worklet access']"),
      ).not.toBeNull(),
    );
    expect(screen.queryByText(/^You need .* access$/)).toBeNull();
    // THE PAGE'S OWN CARDS: `#/company` opens Principles, whose cards hold
    // the company's charter, which every signed-in reader reads.
    expect(screen.getByRole("heading", { name: "Standing policies" })).toBeDefined();
    principles.view.unmount();
    cleanup();
    location.hash = buildHash(["settings", "mcp-tools"]);
    mountAs(Promise.resolve({ login: "reader", owner: "reader", grants: ["worklet"] }));
    expect(await screen.findByText("You need Admin access")).toBeDefined();
  });
});

// CHATLET, AS A PERSON. Each of its places is one screen told which view to
// draw, so a dispatch that swapped two of them draws the wrong view under a
// correct trail and title, and "rendered something" passes that. Its sections
// are drawn by ONE renderer, the screen's Rooms panel: a frame strip beside it
// would be a second set of links to the same places.
describe("chat, for a person", () => {
  const served = {
    read_level: "session",
    complete: true,
    position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 3 },
  };

  /** The frame, as a person bound to `ada` holding Chatlet, recording every question asked. */
  function mountChat(hash: string) {
    location.hash = hash;
    const asked: string[] = [];
    const store = new Store();
    store.applyHealth(healthFrame({ nodes: 1, applied_epoch: 2 }));
    // A COMPANY RUNNING CHATLET, as the projection says before a chat
    // page is drawn at all.
    store.applyOrg(
      withDerived({
        name: "Acme",
        roles: [],
        services: { tracker: "worklet", knowledge: "doclet", chat: "chatlet" },
      }),
    );
    const socket = new LiveSocket(store);
    (socket as unknown as { query: (what: string) => Promise<unknown> }).query = async (what) => {
      asked.push(what);
      switch (what) {
        case "viewer":
          return {
            login: "ada.lovelace",
            owner: "ada",
            handle: "ada",
            name: "Ada",
            kind: "human",
            grants: ["chatlet"],
            acts: [],
            chat_vendors: [],
          };
        case "chat_channels":
          return { ...served, read_state: true, channels: [] };
        case "chat_activity":
          return {
            ...served,
            items: [],
            unread: { mention: 0, reply: 0 },
            read_state: true,
          };
        default:
          // EVERYTHING ELSE IS A QUESTION THIS NODE DOES NOT SERVE: the frame
          // reads what it is asking for, and an empty object is no answer.
          throw new QueryError("unknown_query");
      }
    };
    const view = render(
      <ClientContext.Provider value={{ store, socket }}>
        <Router>
          <App />
        </Router>
      </ClientContext.Provider>,
    );
    return { asked, view };
  }

  /** The links on the page to `path`, wherever they are drawn. */
  function linksTo(view: ReturnType<typeof render>, path: string[]): number {
    return view.container.querySelectorAll(`a[href="${buildHash(path)}"]`).length;
  }

  /** The chat row's views the Rooms panel draws, by the row's own table. */
  const panelViews = () =>
    (WORKSPACES.find((ws) => ws.key === "chatlet")?.sections ?? []).filter(
      (s) => s.tab !== false && s.path.length > 1,
    );

  test.each(["#/chatlet", "#/chatlet/activity", "#/chatlet/search"])(
    "%s draws each chat view once, in the Rooms panel, and no section strip",
    async (hash) => {
      const { view } = mountChat(hash);
      fireEvent.click(await screen.findByRole("button", { name: /^Menu/ }));
      const panel = await screen.findByRole("dialog", { name: "Rooms" });
      // NOT VACUOUS: the row declares the views the panel draws.
      expect(panelViews().length).toBeGreaterThan(0);
      for (const section of panelViews()) {
        expect(linksTo(view, section.path), `links to ${section.label}`).toBe(1);
        expect(within(panel).getByRole("link", { name: section.label })).toBeDefined();
      }
      expect(view.container.querySelector(".section-tabs")).toBeNull();
      expect(view.container.querySelector(".crewlet-section-frame__nav")).toBeNull();
    },
  );

  // EVERY VIEW IS ITS ROW'S SECTION, named in the trail as the row names it:
  // a dispatch that drew another view under it would be named wrong.
  test.each(["activity", "threads", "drafts", "saved", "search", "browse", "new"])(
    "#/chatlet/%s is the row's section of that name",
    async (view) => {
      mountChat(`#/chatlet/${view}`);
      const section = WORKSPACES.find((ws) => ws.key === "chatlet")?.sections.find(
        (s) => s.key === view,
      );
      expect(section, view).toBeDefined();
      const trail = await screen.findByRole("navigation", { name: "Breadcrumb" });
      expect(within(trail).getByText(section!.label)).toBeDefined();
    },
  );
});

describe("live state reaches the screen", () => {
  test("a pushed roster renders its seats", () => {
    // THE HASH BEFORE THE MOUNT. The router reads `location.hash` at mount
    // and then listens for `hashchange`, which jsdom dispatches
    // asynchronously — so assigning it after mounting and re-rendering
    // synchronously renders the screen the reader was on before.
    location.hash = "#/company/org/roster";
    const { store, view } = mount();
    store.applyOrg(
      withDerived({
        name: "Acme",
        roles: [{ name: "CEO", handle: "ceo", goal: "Set direction" }],
      }),
    );
    store.applySeats([
      { id: "ceo", agent_id: "a-ceo", handle: "ceo", role: "CEO", activity: "working" },
    ]);
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
    // A STAR IS KEPT FOR THE TAB'S READER (`lib/starred.ts`), whom the frame
    // learns from the session read this suite does not answer.
    noteReader("p-1");
    resetStarsForTest();
    location.hash = "#/company/org/seats/ceo";
    const { store, view } = mount();
    store.applyOrg(
      withDerived({
        name: "Acme",
        roles: [{ name: "CEO", handle: "ceo", goal: "Set direction" }],
      }),
    );
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
    location.hash = "#/company/org/seats/roster";
    const { store, view } = mount();
    store.applyOrg(
      withDerived({
        name: "Acme",
        roles: [{ name: "Rota Keeper", handle: "roster", goal: "Keep the rota" }],
      }),
    );
    view.rerender(
      <ClientContext.Provider value={{ store, socket: new LiveSocket(store) }}>
        <Router>
          <App />
        </Router>
      </ClientContext.Provider>,
    );
    expect(view.container.querySelector(".prof-head")?.textContent).toContain("Rota Keeper");
    expect(screen.queryByText(/No seat called/)).toBeNull();
    expect(screen.queryByText(/there is no such screen/)).toBeNull();
    // AND NOT THE ROSTER: the page is named for the person — ONCE. The last
    // crumb is the page's `h1`, and the object header under it names the
    // same seat at level two; two `h1`s reading the same words were two page
    // titles to a reader navigating by heading.
    //
    // BY ITS ACCESSIBLE NAME: the crumb wears the seat's badge, which is
    // decorative and hidden from the name a reader hears.
    expect(screen.getAllByRole("heading", { level: 1 })).toHaveLength(1);
    expect(screen.getByRole("heading", { level: 1, name: "Rota Keeper" })).toBeTruthy();
  });

  test("a seat opened on a tab it does not have still has a page under it", () => {
    // THE BLANK PAGE, end to end. `tab=` is a string off a URL and the tab
    // set belongs to the seat's kind, so `?tab=zzz` — a bookmark, a typed
    // URL, a link made before the kind changed — used to select no tab and
    // match no branch: the header and the strip, and then nothing.
    location.hash = "#/company/org/seats/ceo?tab=zzz";
    const { store, view } = mount();
    store.applyOrg(
      withDerived({
        name: "Acme",
        roles: [{ name: "CEO", handle: "ceo", goal: "Set direction" }],
      }),
    );
    store.applySeats([
      { id: "ceo", agent_id: "a-ceo", handle: "ceo", role: "CEO", activity: "working" },
    ]);
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
      // THE SEAT'S OWN ID, which a streamed phase is matched to its seat on —
      // never the role name two seats may share.
      agent_id: "a-ceo",
      model: "claude-sonnet-5",
      total_tokens: 100,
    },
  });

  // A SEAT'S TURNS ARE ITS TRAIL, read here by an admin — once the viewer
  // has said so, which the tab waits for.
  async function seatView() {
    location.hash = "#/company/org/seats/ceo?tab=turns";
    const { store, view, socket } = mount(ADMIN_VIEWER);
    store.applyOrg(withDerived(seat));
    const redraw = () =>
      view.rerender(
        <ClientContext.Provider value={{ store, socket }}>
          <Router>
            <App />
          </Router>
        </ClientContext.Provider>,
      );
    await waitFor(() => expect(screen.queryByLabelText("Checking your access")).toBeNull());
    return { store, view, redraw };
  }

  test("the turn is still there once its review completes", async () => {
    const { store, redraw } = await seatView();

    // Onboarding and execute have landed; the review is live.
    store.applyEvent(phaseEnvelope("p1", "onboarding", "2026-01-01T00:00:01Z") as never);
    store.applyEvent(phaseEnvelope("p2", "execute", "2026-01-01T00:00:05Z") as never);
    store.applySeats([
      {
        id: "ceo",
        agent_id: "a-ceo",
        handle: "ceo",
        role: "CEO",
        activity: "working",
        live_call: {
          versions: ZERO_VERSIONS,
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
    store.applySeats([
      {
        id: "ceo",
        agent_id: "a-ceo",
        handle: "ceo",
        role: "CEO",
        activity: "idle",
        live_call: null,
      },
    ] as never);
    redraw();

    // STILL THERE, all three phases of it, and no longer running.
    expect(screen.queryByText("Running now")).toBeNull();
    expect(screen.getAllByText("onboarding").length).toBeGreaterThan(0);
    expect(screen.getAllByText("execute").length).toBeGreaterThan(0);
    expect(screen.getAllByText("review").length).toBeGreaterThan(0);
    // And the page does not claim the seat has never run.
    expect(screen.queryByText("No phases in the record for this seat")).toBeNull();
  });

  test("it is not held behind a 'new turns' button either", async () => {
    // The reader was watching it: it is not a new row, whichever list it is in.
    // SCROLLED DOWN, which is the whole condition — at the top of the scroller
    // the settled list admits everything anyway, so an assertion taken there
    // passes whether the rule holds or not.
    const { store, redraw } = await seatView();
    const scroller = screenScroller();
    if (!scroller) throw new Error("no scroller to scroll: the shell's layout moved");
    Object.defineProperty(scroller, "scrollTop", { value: 400, configurable: true });
    store.applySeats([
      {
        id: "ceo",
        agent_id: "a-ceo",
        handle: "ceo",
        role: "CEO",
        activity: "working",
        live_call: {
          versions: ZERO_VERSIONS,
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
    store.applySeats([
      {
        id: "ceo",
        agent_id: "a-ceo",
        handle: "ceo",
        role: "CEO",
        activity: "idle",
        live_call: null,
      },
    ] as never);
    redraw();
    expect(screen.queryByText(/new turns?\. Show/)).toBeNull();
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
  const operator = () =>
    mountAs(
      Promise.resolve({
        login: "ops",
        owner: "ops",
        grants: ["admin", "worklet", "doclet"],
      }),
    );
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

// ON A PHONE THE BAR KEEPS ONE ACTION IN VIEW, and "More" holds the rest: the
// frame's star where the page has one, and what the screen folds, such as a
// project's Edit project, which opens the same dialog its inline button does.
// Laid out inline at 390px a project's bar stood three rows tall. A project is
// one of Work's scopes, whose bar carries no star. (What is SHOWN at which
// width is the stylesheet's container query, which this DOM does not
// evaluate; what the menu HOLDS, and that its entries act, is asserted here.)
test("the page bar's More holds the star and what the screen folded, and no Copy link", async () => {
  // A STAR IS KEPT FOR THE TAB'S READER (`lib/starred.ts`).
  noteReader("p-1");
  resetStarsForTest();
  location.hash = "#/worklet/ENG";
  const store = new Store();
  store.applyHealth(healthFrame({ nodes: 1, applied_epoch: 2 }));
  store.applyOrg(withDerived({ name: "Acme", roles: [] }));
  const socket = new LiveSocket(store);
  const project = {
    key: "ENG",
    name: "Core platform",
    unit: { resolved: true },
    lead: { handle: "jane" },
    task_counts: { todo: 1, active: 0, done: 0, closed: 0 },
    statuses: [],
    types: [],
    fields: [],
    tags: [],
    policy_stamp: 1,
    complete: true,
  };
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) => {
    if (what === "viewer") {
      return Promise.resolve({
        login: "U0FOUNDER",
        grants: ["admin", "worklet", "doclet"],
        handle: "jane",
        owner: "jane",
        name: "Jane Founder",
        kind: "human",
        acts: ["write_project", "create_work_item"],
      });
    }
    if (what === "work_project") return Promise.resolve(project);
    return Promise.reject(new QueryError("unauthorized"));
  };
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <App />
      </Router>
    </ClientContext.Provider>,
  );
  for (let i = 0; i < 8; i++) await act(async () => Promise.resolve());
  fireEvent.click(screen.getByRole("button", { name: "More on this page" }));
  const names = screen.getAllByRole("menuitem").map((el) => el.textContent);
  expect(names).toEqual(["Edit project"]);
  fireEvent.click(screen.getByRole("menuitem", { name: "Edit project" }));
  expect(await screen.findByRole("dialog", { name: "Edit Core platform" })).toBeTruthy();
  cleanup();

  // A TASK'S OWN "MORE" JOINS IT rather than standing as a second ellipsis
  // beside the frame's, and its entry still goes where the inline one does.
  location.hash = "#/worklet/ENG-42";
  const task = {
    task: { id: "t-1", key: "ENG-42", project: "ENG", title: "Fix it", status: "todo", version: 3 },
    reassignment_budget: 8,
    complete: true,
  };
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) => {
    if (what === "viewer")
      return Promise.resolve({
        login: "U0FOUNDER",
        owner: "U0FOUNDER",
        grants: ["admin", "worklet", "doclet"],
      });
    if (what === "work_item") return Promise.resolve(task);
    if (what === "work_project") return Promise.resolve(project);
    return Promise.reject(new QueryError("unauthorized"));
  };
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <App />
      </Router>
    </ClientContext.Provider>,
  );
  for (let i = 0; i < 8; i++) await act(async () => Promise.resolve());
  fireEvent.click(screen.getByRole("button", { name: "More on this page" }));
  expect(screen.getAllByRole("menuitem").map((el) => el.textContent)).toEqual([
    "Open on the board",
    "Keep in Starred",
  ]);
  fireEvent.click(screen.getByRole("menuitem", { name: "Open on the board" }));
  expect(location.hash).toContain("#/worklet/ENG");
  expect(location.hash).toContain("peek=");
});

// AN ADDRESS UNDER CHATLET, ON A COMPANY TALKING ON A VENDOR'S, draws where the
// company talks in place of the screen, and the screen asks nothing: no rail,
// no room. Mutation: drop Chatlet's `service` and the rooms are asked.
test("a chat address on a company not running Chatlet says where it talks", async () => {
  location.hash = "#/chatlet";
  const asked: string[] = [];
  const reader = {
    login: "ana",
    owner: "ana",
    handle: "ana",
    grants: ["chatlet"],
    acts: [],
    chat_vendors: ["mattermost"],
  };
  const store = new Store();
  store.applyOrg(
    withDerived({
      name: "Acme",
      roles: [],
      services: { tracker: "worklet", knowledge: "doclet", chat: "vendor" },
    }),
  );
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) => {
    asked.push(what);
    return what === "viewer" ? Promise.resolve(reader) : new Promise(() => {});
  };
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <App />
      </Router>
    </ClientContext.Provider>,
  );
  expect(await screen.findByText("This company talks on Mattermost")).toBeTruthy();
  expect(asked.filter((w) => w.startsWith("chat_"))).toEqual([]);
  expect(screen.queryByRole("navigation", { name: "Your rooms" })).toBeNull();
});

// A COLD LOAD OF A SERVICE'S PAGE WAITS FOR THE PROJECTION: mounted on the
// guess, Doclet asked its spaces, its pages and its activity on a company
// running none, was refused all three, then gave way to the notice. It waits,
// asks nothing, and says what the company runs once it knows. Mutation: mount
// the screen before the org and `containers` is asked.
test("a cold load of a service's page asks nothing until the company says what it runs", async () => {
  location.hash = "#/doclet";
  const asked: string[] = [];
  const reader = { login: "ana", owner: "ana", handle: "ana", grants: ["admin"], acts: [] };
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) => {
    asked.push(what);
    return what === "viewer" ? Promise.resolve(reader) : new Promise(() => {});
  };
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <App />
      </Router>
    </ClientContext.Provider>,
  );
  expect(await screen.findByText("Checking what the company runs")).toBeTruthy();
  expect(asked.filter((w) => w === "containers" || w === "pages")).toEqual([]);
  act(() =>
    store.applyOrg(
      withDerived({
        name: "Acme",
        roles: [],
        services: { tracker: "worklet", knowledge: "none", chat: "chatlet" },
      }),
    ),
  );
  expect(await screen.findByText("This company runs no knowledge base")).toBeTruthy();
  expect(asked.filter((w) => w === "containers" || w === "pages")).toEqual([]);
});
