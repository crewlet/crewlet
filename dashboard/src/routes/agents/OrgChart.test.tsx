/**
 * The live org chart as a reader meets it: the builder's drawing of the
 * applied company in either arrangement, a node per seat saying what it is
 * doing, a node per unit with its project and its lead, the legend's counts,
 * and a node that opens its seat or unit beside the chart — and, for a reader
 * the identity directory answers, what holds each human seat, with the
 * gestures that fill one for a reader holding Admin.
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { OrgChart } from "./OrgChart.tsx";
import { LEGIBLE_ZOOM } from "~/ui/canvasView.ts";
import { findSeats } from "./header.tsx";
import { CONFIG_WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { managedSentence } from "~/protocol/configAnswer.ts";
import { Router } from "~/app/router.tsx";
import { PeekNeighbours } from "~/app/frame/PeekHost.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { indexOrg } from "~/lib/seats.ts";
import { stateCounts } from "~/lib/orgchart.ts";
import { CHART_ORG } from "~/test/orgchart.ts";
import {
  CARD_WIDTH,
  LayoutObserver,
  VIEWPORT,
  canvasWorld,
  chartCard,
  isCanvasViewport,
} from "~/routes/org/builder/viewTestkit.tsx";
import { installWindow } from "~/testing.tsx";
import { LiveSocket, Store, type AgentRow } from "~/protocol/index.ts";
import { healthFrame } from "~/test/health.ts";
import { ZERO_VERSIONS } from "~/test/liveCall.ts";
import { ENGINE_HEADERS } from "~/test/engineAnswer.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

const AGENTS = [
  { id: "ceo", agent_id: "ceo", handle: "ceo", role: "CEO", activity: "idle", last_turn: null },
  {
    id: "cto",
    agent_id: "cto",
    handle: "cto",
    role: "CTO",
    activity: "working",
    live_call: { phase: "review", work_item: { key: "ENG-409" }, versions: ZERO_VERSIONS },
  },
  {
    id: "swe",
    agent_id: "swe",
    handle: "swe",
    role: "SWE",
    activity: "working",
    live_call: { phase: "execute", work_item: { key: "ENG-412" }, versions: ZERO_VERSIONS },
  },
  {
    id: "fe",
    agent_id: "fe",
    handle: "fe",
    role: "FE",
    activity: "needs",
    turn: { turn_id: "t", started_at: "", stage: "parked" },
  },
  {
    id: "pm",
    agent_id: "pm",
    handle: "pm",
    role: "PM",
    activity: "stopped",
    stopped_reason: "budget",
  },
  { id: "devrel", agent_id: "devrel", handle: "devrel", role: "DevRel", activity: "idle" },
] as unknown as AgentRow[];

let restore: () => void;
beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  location.hash = "#/company/org";
  // jsdom has no layout: the chart's cards are measured through this.
  restore = LayoutObserver.install();
});

afterEach(() => {
  cleanup();
  restore();
  location.hash = "";
  vi.unstubAllGlobals();
});

/** What `GET /iam/seats` says of jane's seat: vacant unless a case says otherwise. */
let jane: Record<string, unknown> = {};
/** Every path the chart sent to the engine's REST surface. */
let fetched: string[] = [];
/** What `GET /iam/seats` answers instead of the listing: a refusal. */
let seatsRefused: Response | null = null;

beforeEach(() => {
  jane = {};
  fetched = [];
  seatsRefused = null;
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const path = new URL(String(input), "http://engine.test").pathname;
      fetched.push(path);
      if (path === "/iam/seats" && seatsRefused) return seatsRefused.clone();
      return new Response(
        JSON.stringify(
          path === "/iam/seats"
            ? { seats: [{ handle: "jane", name: "Jane Founder", ...jane }] }
            : { error: "no_route" },
        ),
        {
          status: path === "/iam/seats" ? 200 : 404,
          headers: { "Content-Type": "application/json" },
        },
      );
    }),
  );
});

/** Every grant an operator holds. */
const OPERATOR = {
  login: "ops",
  grants: ["admin", "worklet", "doclet"],
  handle: "jane",
  owner: "jane",
  acts: [],
};

async function mount(viewer: Record<string, unknown> = { login: "", owner: "", acts: [] }) {
  const store = new Store();
  store.applyHealth(healthFrame());
  store.applyOrg(CHART_ORG);
  store.applySeats(AGENTS);
  const socket = new LiveSocket(store);
  const asked: string[] = [];
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) => {
    asked.push(what);
    if (what === "viewer") return Promise.resolve(viewer);
    if (what === "work_projects") {
      return Promise.resolve({
        projects: [{ key: "ENG", unit: { key: "core", name: "Core", resolved: true } }],
        total: 1,
        census: { active: 1, archived: 0 },
        complete: true,
      });
    }
    return Promise.resolve({});
  };
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <ViewerProvider>
        <Router>
          {/* THE SHELL'S OWN PROVIDER, so the order the chart publishes is
              the one `[` and `]` step through. */}
          <PeekNeighbours>
            <OrgChart />
          </PeekNeighbours>
        </Router>
      </ViewerProvider>
    </ClientContext.Provider>,
  );
  await act(async () => {
    for (let i = 0; i < 5; i++) await Promise.resolve();
  });
  act(() => LayoutObserver.settle());
  return { asked, store };
}

/**
 * Every control the chart draws beside a card, by its accessible name — the
 * label an icon-only control carries.
 */
function controls(name: string): (string | null)[] {
  return [...chartCard(card(name)).querySelectorAll("button")].map(
    (b) => b.getAttribute("aria-label") ?? b.textContent,
  );
}

function card(name: string): HTMLElement {
  // BY ROLE ATTRIBUTE rather than the accessibility tree: the canvas holds
  // its world out of it until a layout has been measured, which jsdom never
  // reports as done. The name is the kit's node label's.
  const found = [...document.querySelectorAll<HTMLElement>("[role=treeitem]")].find(
    (el) => el.querySelector(".crewlet-org-label__name")?.textContent === name,
  );
  if (!found) throw new Error(`no card for ${name}`);
  return found;
}

/** What a node writes under its name. */
function caption(name: string): string {
  return (card(name).querySelector(".crewlet-org-label__caption")?.textContent ?? "").trim();
}

/** The lead pill along a unit node's bottom edge. */
function leadOf(name: string): string {
  return chartCard(card(name)).querySelector(".crewlet-org-node-lead")?.textContent ?? "";
}

// A NODE PER SEAT, SAYING WHAT THE ENGINE SAYS IT IS DOING: the engine's word
// under the name, and its whole state line in the node's title.
test("every seat of the applied chart is a node with its state word and state line", async () => {
  await mount();
  for (const name of ["Jane Founder", "CEO", "CTO", "SWE", "FE", "PM", "DevRel"]) {
    expect(card(name)).toBeTruthy();
  }
  expect(caption("SWE")).toBe("Working");
  expect(card("SWE").getAttribute("title")).toBe("Executing ENG-412 · @swe");
  expect(card("SWE").getAttribute("data-state")).toBe("working");
  expect(caption("FE")).toBe("Needs you");
  expect(caption("PM")).toBe("Stopped");
  expect(card("PM").getAttribute("title")).toBe(
    "Stopped: its token budget is spent until the window resets or the ceiling is raised · @pm",
  );
  expect(caption("Jane Founder")).toBe("Human seat");
  expect(card("Jane Founder").getAttribute("title")).toBe("Human · reviews releases · @jane");
});

// A HUMAN NODE SAYS WHAT HOLDS ITS SEAT, for every signed-in reader, under its
// name in the node's neutral ink — who holds a seat is not something it is
// doing. The CONTROL is the browser nobody signed in on above, who is asked
// nothing and reads "Human seat". Mutation: drop the holding from the node's caption
// and the three captions read "Human seat".
test.each([
  [
    "held",
    { holder: { person: "p-jane", kind: "person", login: "jane.doe", stage: "active" } },
    "Held by jane.doe",
  ],
  [
    "invited",
    { invitation: { id: "inv-3", email: "jane@example.com", expires_at: "2031-05-08T09:00:00Z" } },
    "Invited · jane@example.com",
  ],
  ["vacant", {}, "Open seat"],
])("a %s human seat's card says what holds it", async (_, holding, line) => {
  jane = holding;
  await mount({ ...OPERATOR, grants: ["admin"] });
  await act(async () => {});
  const founder = card("Jane Founder");
  expect(caption("Jane Founder")).toBe(line);
  expect(founder.getAttribute("title")).toBe(`${line} · @jane`);
  // NO STATE HUE: a human node carries no engine state for its line to paint.
  expect(founder.hasAttribute("data-state")).toBe(false);
});

// WHO HOLDS A SEAT IS PART OF THE OVERVIEW: every signed-in reader is asked
// the directory's listing — the engine masks it per reader — and reads what
// holds each human seat, while only an admin is offered a gesture on one. The
// CONTROL is a browser nobody has signed in on, who is asked nothing. Mutation:
// gate the listing on Admin again and the tracker reader reads "Human seat".
test("every signed-in reader reads who holds a human seat, and only an admin acts on it", async () => {
  await mount({ ...OPERATOR, grants: ["worklet"] });
  await act(async () => {});
  expect(fetched).toContain("/iam/seats");
  expect(caption("Jane Founder")).toBe("Open seat");
  expect(controls("Jane Founder")).toEqual([]);
  cleanup();
  fetched = [];

  await mount();
  expect(fetched).not.toContain("/iam/seats");
  expect(caption("Jane Founder")).toBe("Human seat");
  expect(card("Jane Founder").getAttribute("title")).toBe("Human · reviews releases · @jane");
});

// A DIRECTORY READ THAT FAILED IS SAID over the chart: drawn as nothing, an
// administrator saw the chart a browser nobody signed in on sees — "Human
// seat" and no menu — with no word why. The CONTROL is the same
// administrator with the listing answered, who is shown no such banner.
// Mutation: drop the banner and the sentence is gone.
test("a directory that did not answer an administrator is said over the chart", async () => {
  seatsRefused = new Response(
    JSON.stringify({ error: "unavailable", message: "This node cannot answer yet." }),
    {
      status: 503,
      headers: { ...ENGINE_HEADERS, "Content-Type": "application/json", "Retry-After": "2" },
    },
  );
  await mount(OPERATOR);
  await act(async () => {});
  expect(screen.getByText("Couldn’t load who holds each seat.")).toBeTruthy();
  expect(screen.getByText(/Still catching up\. This screen retries on its own\./)).toBeTruthy();
  expect(caption("Jane Founder")).toBe("Human seat");
  expect(controls("Jane Founder")).toEqual([]);
  cleanup();

  seatsRefused = null;
  await mount(OPERATOR);
  await act(async () => {});
  expect(caption("Jane Founder")).toBe("Open seat");
  expect(screen.queryByText("Couldn’t load who holds each seat.")).toBeNull();
});

// THE NODE'S MENU FILLS A VACANT SEAT, for Admin alone, through the
// tree's own pattern: a pointer strip beside the node and the same items
// under the ContextMenu key on the node itself, since a tree's items hold
// nothing focusable. The CONTROLS: a reader without Admin reads "Open seat" with no menu,
// and an agent's node has none. Mutation: give every human card the menu, or
// drop the grant check, and a line here goes red.
test("a vacant human card offers Invite and Create in its menu to an administrator alone", async () => {
  await mount(OPERATOR);
  await act(async () => {});
  // THE POINTER'S STRIP, out of the tab order, says which seat each control
  // is about; an agent's card draws none.
  expect(controls("Jane Founder")).toEqual([
    "Invite someone as Jane Founder",
    "Add someone as Jane Founder",
    "Actions for Jane Founder",
  ]);
  expect(controls("SWE")).toEqual([]);
  // AN AGENT'S CARD HAS NO SUCH MENU.
  const swe = card("SWE");
  swe.focus();
  fireEvent.keyDown(swe, { key: "ContextMenu" });
  expect(screen.queryByRole("menu", { name: "Actions for SWE" })).toBeNull();
  const founder = card("Jane Founder");
  founder.focus();
  fireEvent.keyDown(founder, { key: "ContextMenu" });
  const menu = screen.getByRole("menu", { name: "Actions for Jane Founder" });
  expect(
    within(menu)
      .getAllByRole("menuitem")
      .map((i) => i.textContent),
  ).toEqual(["Invite someone…", "Add a person…"]);
  fireEvent.click(within(menu).getByRole("menuitem", { name: "Add a person…" }));
  expect(screen.getByRole("dialog", { name: "Add someone as Jane Founder" })).toBeTruthy();
  cleanup();

  await mount({ ...OPERATOR, grants: ["worklet"] });
  await act(async () => {});
  const read = card("Jane Founder");
  expect(caption("Jane Founder")).toBe("Open seat");
  read.focus();
  fireEvent.keyDown(read, { key: "ContextMenu" });
  expect(screen.queryByRole("menu")).toBeNull();
  expect(controls("Jane Founder")).toEqual([]);
});

// A POINTER PRESS ON THE STRIP opens the same dialog the menu does, for the
// card's own seat — and never opens the seat's peek, which a press on the
// card itself does.
test("the strip's Invite opens the invitation for that card's seat", async () => {
  await mount(OPERATOR);
  await act(async () => {});
  const invite = [...chartCard(card("Jane Founder")).querySelectorAll("button")].find(
    (b) => b.getAttribute("aria-label") === "Invite someone as Jane Founder",
  )!;
  fireEvent.click(invite);
  expect(screen.getByRole("dialog", { name: "Invite someone as Jane Founder" })).toBeTruthy();
  expect(location.hash).not.toContain("peek=");
});

// AN INVITED SEAT'S MENU CANCELS ITS INVITATION, and a held one has none.
test("an invited human card's menu cancels its invitation, and a held one has no menu", async () => {
  jane = {
    invitation: { id: "inv-3", email: "jane@example.com", expires_at: "2031-05-08T09:00:00Z" },
  };
  await mount(OPERATOR);
  await act(async () => {});
  const founder = card("Jane Founder");
  founder.focus();
  fireEvent.keyDown(founder, { key: "ContextMenu" });
  const menu = screen.getByRole("menu", { name: "Actions for Jane Founder" });
  expect(
    within(menu)
      .getAllByRole("menuitem")
      .map((i) => i.textContent),
  ).toEqual(["Cancel invitation"]);
  fireEvent.click(within(menu).getByRole("menuitem", { name: "Cancel invitation" }));
  expect(screen.getByRole("dialog", { name: "Cancel this invitation?" })).toBeTruthy();
  cleanup();

  jane = { holder: { person: "p-jane", kind: "person", login: "jane.doe", stage: "active" } };
  await mount(OPERATOR);
  await act(async () => {});
  const held = card("Jane Founder");
  held.focus();
  fireEvent.keyDown(held, { key: "ContextMenu" });
  expect(screen.queryByRole("menu")).toBeNull();
});

// THE STRUCTURE IS THE BUILDER'S: the company at the root, each unit with its
// type and the project its work is filed under, and its lead in a pill.
test("the structure draws the company, and each unit with its project and its lead", async () => {
  await mount({ login: "jane.doe", grants: ["worklet"], owner: "jane", acts: [] });
  expect(caption("Acme")).toBe("Company");
  expect(caption("Core")).toBe("Unit · ENG");
  expect(card("Core").getAttribute("title")).toBe("2 seats. Projects: ENG");
  expect(leadOf("Core")).toBe("Lead: CTO");
  expect(leadOf("Developer Relations")).toBe("Lead: PM (inherited)");
  expect(leadOf("Leadership")).toBe("Lead");
  cleanup();

  // THE PROJECT IS WORKLET'S: a reader without Worklet is not asked a
  // question the engine would refuse, and a unit's node carries its type alone.
  const { asked } = await mount({ login: "sam", grants: ["doclet"], owner: "sam", acts: [] });
  expect(asked).not.toContain("work_projects");
  expect(caption("Core")).toBe("Unit");
  expect(leadOf("Core")).toBe("Lead: CTO");
});

// AN AGENT SEAT IS TONED, as the builder tones one; a person, a unit and the
// company keep the chart's neutral surface.
test("an agent seat is toned and nothing else is", async () => {
  await mount();
  expect(chartCard(card("SWE")).getAttribute("data-tone")).toBe("purple");
  for (const name of ["Jane Founder", "Core", "Acme"]) {
    expect(chartCard(card(name)).getAttribute("data-tone"), name).toBeNull();
  }
});

// THE SWITCH UNDER THE ZOOM BAR draws who reports to whom instead: the same
// seats, under their primary managers, and no company or unit.
test("the switch draws the reporting chart, and the address names it", async () => {
  await mount();
  await act(async () => {
    fireEvent.click(screen.getByRole("tab", { name: "Reporting" }));
  });
  act(() => LayoutObserver.settle());
  expect(location.hash).toContain("chart=reporting");
  for (const name of ["Jane Founder", "CEO", "SWE", "DevRel"]) expect(card(name)).toBeTruthy();
  expect(() => card("Acme")).toThrow();
  expect(() => card("Core")).toThrow();
  expect(screen.getByRole("tab", { name: "Reporting" }).getAttribute("aria-selected")).toBe("true");
});

// THE LEGEND IS THE ENGINE'S COUNT, and the same one `stateCounts` takes.
test("the legend's figures are the activity counts", async () => {
  await mount();
  const counts = stateCounts(indexOrg(CHART_ORG), AGENTS);
  const legend = screen.getByRole("group", { name: "Seats by what they are doing" });
  expect(legend.textContent).toBe(
    // A GROUP keyed on the engine's `stopped` holds the paused seats as well
    // as the ones a fault stopped, and its word says both.
    `Working ${counts.working}Needs you ${counts.needs}Paused or stopped ${counts.stopped}Idle ${counts.idle}`,
  );
  expect(counts).toEqual({ working: 2, needs: 1, stopped: 1, idle: 2 });
});

// A NODE OPENS ITS SEAT BESIDE THE CHART, addressed by its handle.
test("pressing a seat opens that seat's peek", async () => {
  await mount();
  await act(async () => {
    fireEvent.click(card("SWE"));
  });
  expect(location.hash).toContain("peek=seat%3Aswe");
});

// AND A UNIT OPENS THE UNIT, addressed by its KEY, as Teams addresses one —
// never by its name, which two units may share.
test("pressing a unit opens that unit's peek", async () => {
  await mount();
  await act(async () => {
    fireEvent.click(card("Core"));
  });
  expect(location.hash).toContain("peek=unit%3Acore");
});

// ADD SEAT IS NEVER HIDDEN: a reader who may not change the org sees it held,
// with the reason.
test("Add seat is held with its reason for a reader nobody has signed in", async () => {
  await mount();
  const add = screen.getByRole("button", { name: /Add seat/ });
  expect(add.getAttribute("aria-disabled") === "true" || add.hasAttribute("disabled")).toBe(true);
  expect(add.getAttribute("title")).toBe(CONFIG_WRITE_REASONS.anonymous);
});

test("an operator's Add seat goes to the builder, adding an agent", async () => {
  await mount({
    login: "ops",
    grants: ["admin", "worklet", "doclet"],
    handle: "jane",
    owner: "jane",
    acts: [],
  });
  const add = screen.getByRole("link", { name: /Add seat/ });
  expect(add.getAttribute("href")).toBe("#/company/org/edit?add=agent");
});

// A LEAD ADDS TO THE UNIT THEY LEAD: nothing at the company's top level is
// theirs, so their Add seat opens the builder on their unit.
test("a lead's Add seat goes to the builder, adding to the unit they lead", async () => {
  await mount({
    login: "cto.person",
    grants: ["worklet"],
    handle: "cto",
    owner: "cto",
    acts: [],
  });
  const add = screen.getByRole("link", { name: /Add seat/ });
  expect(add.getAttribute("href")).toBe("#/company/org/edit?unit=core&add=agent");
});

// A MANAGED DOCUMENT (ADR-0030) holds Add seat for a reader whose credential
// is not one of its writers — the builder it opens would be read-only — with
// the sentence that names who manages it, whether the reader holds the grant
// or leads a unit; a writer still adds. Mutation: drop the managed block from
// the org write gate and the first two mounts offer the link.
test("Add seat is held with the managed sentence for a reader who is not a writer", async () => {
  await mount({ ...OPERATOR, config_writer: false, config_managed_by: ["gitops"] });
  const add = screen.getByRole("button", { name: /Add seat/ });
  expect(add.getAttribute("aria-disabled") === "true" || add.hasAttribute("disabled")).toBe(true);
  expect(add.getAttribute("title")).toBe(managedSentence(["gitops"]));
  cleanup();

  await mount({
    login: "cto.person",
    grants: ["worklet"],
    handle: "cto",
    owner: "cto",
    acts: [],
    config_writer: false,
    config_managed_by: ["gitops"],
  });
  const lead = screen.getByRole("button", { name: /Add seat/ });
  expect(lead.getAttribute("title")).toBe(managedSentence(["gitops"]));
  cleanup();

  await mount({ ...OPERATOR, config_writer: true, config_managed_by: ["gitops"] });
  expect(screen.getByRole("link", { name: /Add seat/ }).getAttribute("href")).toBe(
    "#/company/org/edit?add=agent",
  );
});

// FIND A SEAT matches what a reader remembers somebody by — name, handle,
// unit — and a name that starts with the term comes before one that only
// contains it.
test("find a seat ranks a prefix before a substring, and reads the unit", () => {
  const index = indexOrg(CHART_ORG);
  expect(findSeats(index, "f").map((s) => s.name)).toEqual(["FE", "Jane Founder"]);
  expect(findSeats(index, "core").map((s) => s.name)).toEqual(["SWE", "FE"]);
  expect(findSeats(index, "  ")).toEqual([]);
});

/** The canvas's width as the harness reports it, which a suite may narrow. */
let canvasWidth = VIEWPORT.width;

/** Report the canvas (the kit's viewport and the chart's own box) at a width. */
function standCanvasAt(width: number): void {
  canvasWidth = width;
  const base = LayoutObserver.sizer;
  LayoutObserver.sizer = (el) => {
    if (isCanvasViewport(el) || el.classList.contains("oc-chart")) {
      return { width: canvasWidth, height: VIEWPORT.height };
    }
    return base(el);
  };
}

/**
 * Lay the chart out and let it answer: the canvas says it is ready through an
 * attribute, which a MutationObserver hears as a microtask, and what the chart
 * does about it eases over frames the harness runs.
 */
async function settleChart(): Promise<void> {
  LayoutObserver.settle();
  await act(async () => {
    for (let i = 0; i < 3; i++) await Promise.resolve();
  });
  LayoutObserver.settle();
}

/** Where a seat's card is drawn on screen, in the canvas's own coordinates. */
function onScreen(name: string): { left: number; right: number; scale: number } {
  const world = canvasWorld(document.body).style.transform;
  const w = /translate\((-?[\d.]+)px,\s*(-?[\d.]+)px\)(?:\s*scale\(([\d.]+)\))?/.exec(world);
  const c = /translate\((-?[\d.]+)px,\s*(-?[\d.]+)px\)/.exec(chartCard(card(name)).style.transform);
  if (!w || !c) throw new Error(`not placed: world ${world}`);
  const k = Number(w[3] ?? 1);
  const left = Number(w[1]) + Number(c[1]) * k;
  return { left, right: left + CARD_WIDTH * k, scale: k };
}

// THE PEEK TAKES THE CANVAS'S WIDTH, AND THE CARD IT IS ABOUT STAYS IN VIEW.
// The chart is fitted once, at the width it had before the peek; opening the
// peek narrowed the canvas under a card the reader had just pressed and left
// it past the edge, ring and all.
test("the seat the peek opens on is inside the canvas after the peek narrows it", async () => {
  standCanvasAt(VIEWPORT.width);
  await mount();
  await act(async () => {
    fireEvent.click(card("DevRel"));
  });
  expect(location.hash).toContain("peek=seat%3Adevrel");
  standCanvasAt(420);
  await settleChart();
  const drawn = onScreen("DevRel");
  expect(drawn.left).toBeGreaterThanOrEqual(0);
  expect(drawn.right).toBeLessThanOrEqual(420);
});

// A SEAT THAT APPEARS IS REVEALED. Held at its legible zoom in a narrow canvas
// (beside the assistant) the chart is wider than the canvas, and a seat the
// reader just asked the assistant to add landed past the edge, unseen.
test("a seat added while the chart is open is brought into view", async () => {
  standCanvasAt(420);
  const { store } = await mount();
  await settleChart();
  // The same company with a Tech Writer added beside DevRel, as the engine
  // answers once the write lands.
  type Seatful = { id: string; roles?: { name: string; handle: string }[]; children?: Seatful[] };
  const added = structuredClone(CHART_ORG) as unknown as {
    units: Seatful[];
    derived: { seats: Record<string, unknown>[]; units: { id: string; seats: string[] }[] };
  };
  added.units[2]!.children![1]!.roles!.push({ name: "Tech Writer", handle: "tw" });
  added.derived.seats.push({ ...added.derived.seats[6]!, handle: "tw", name: "Tech Writer" });
  added.derived.units.find((u) => u.id === "developer-relations")!.seats.push("tw");
  await act(async () => {
    store.applyOrg(added as never);
  });
  await settleChart();
  const drawn = onScreen("Tech Writer");
  expect(drawn.left).toBeGreaterThanOrEqual(0);
  expect(drawn.right).toBeLessThanOrEqual(420);
});

// A READER STEPPING IN THE PEEK KEEPS THEIR FOCUS. The kit reveals a card by
// focusing it; a reveal that left focus on the card would hand the reader's
// next `]` to the tree's type-ahead.
test("revealing the peeked seat leaves focus where the reader had it", async () => {
  standCanvasAt(VIEWPORT.width);
  await mount();
  const outside = document.createElement("button");
  document.body.append(outside);
  outside.focus();
  await act(async () => {
    location.hash = "#/company/org?peek=seat%3Adevrel";
    window.dispatchEvent(new HashChangeEvent("hashchange"));
  });
  standCanvasAt(420);
  await settleChart();
  expect(document.activeElement).toBe(outside);
  const drawn = onScreen("DevRel");
  expect(drawn.left).toBeGreaterThanOrEqual(0);
  expect(drawn.right).toBeLessThanOrEqual(420);
  outside.remove();
});

// ONE PRESS, ONE NAVIGATION. Selection follows the focus a press gives a card,
// and the card's own click opens it: both reached the address, so every press
// on a card with the peek open moved it twice.
test("pressing a card while the peek is open moves the peek once", async () => {
  standCanvasAt(VIEWPORT.width);
  await mount();
  await act(async () => {
    fireEvent.click(card("SWE"));
  });
  const moves = vi.spyOn(history, "replaceState");
  const pushes = vi.spyOn(history, "pushState");
  await act(async () => {
    card("FE").focus();
    fireEvent.click(card("FE"));
  });
  const navigations = [...moves.mock.calls, ...pushes.mock.calls].filter(
    (call) => typeof call[2] === "string" && call[2].includes("peek="),
  );
  expect(location.hash).toContain("peek=seat%3Afe");
  expect(navigations).toHaveLength(1);
  moves.mockRestore();
  pushes.mockRestore();
});

/** Every card's left and right edge on screen, and the canvas's own margins. */
function spread(width: number): { left: number; right: number; margins: [number, number] } {
  const names = ["Jane Founder", "CEO", "CTO", "SWE", "FE", "PM", "DevRel"];
  const drawn = names.map(onScreen);
  const left = Math.min(...drawn.map((d) => d.left));
  const right = Math.max(...drawn.map((d) => d.right));
  return { left, right, margins: [left, width - right] };
}

/**
 * The reporting chart's address. THE TWO FITS BELOW ARE RECKONED ON IT: its
 * seven seats are the chart those widths were chosen for, where the structure
 * of the same company (the company, seven units and their seats) is a drawing
 * no width in this suite fits whole above the floor.
 */
const REPORTING = "#/company/org?chart=reporting";

// A NEW WIDTH IS FITTED AGAIN. The canvas fits once; the peek then took a
// third of it away, and the least pan that kept the pressed card in view left
// the rest of the company cut at the other edge — and closing the peek left it
// cut there, with the freed third of the canvas empty. The chart is fitted
// again, all of it, in the middle, whenever the canvas's width moves, and at
// 800 the reporting chart fits above the floor.
test("a peek that narrows the canvas fits the whole chart into it, centred", async () => {
  location.hash = REPORTING;
  standCanvasAt(VIEWPORT.width);
  await mount();
  await act(async () => {
    fireEvent.click(card("DevRel"));
  });
  standCanvasAt(800);
  await settleChart();
  const narrowed = spread(800);
  expect(onScreen("CEO").scale).toBeGreaterThanOrEqual(LEGIBLE_ZOOM);
  expect(narrowed.left).toBeGreaterThanOrEqual(0);
  expect(narrowed.right).toBeLessThanOrEqual(800);
  expect(Math.abs(narrowed.margins[0] - narrowed.margins[1])).toBeLessThanOrEqual(2);
});

// BUT NEVER SHRUNK PAST READING. Fitted into what a peek left, the chart was
// drawn at 54% at 1280, its state lines seven pixels. A fit that falls below
// the floor is drawn AT it — exactly, not at whichever zoom step came next —
// with the seat the peek is on centred across the canvas; the rest of the
// company is a pan away. At 700 this chart would fit at 80%.
test("a chart the peek would shrink past reading is drawn at the floor, the seat centred across it", async () => {
  standCanvasAt(VIEWPORT.width);
  await mount();
  await act(async () => {
    fireEvent.click(card("DevRel"));
  });
  standCanvasAt(700);
  await settleChart();
  const drawn = onScreen("DevRel");
  expect(drawn.scale).toBeCloseTo(LEGIBLE_ZOOM, 6);
  expect((drawn.left + drawn.right) / 2).toBeCloseTo(350, 3);
});

// AND THE FIRST VIEW IS HELD TOO. A chart opened straight onto a peek (a
// pasted link) is fitted by the canvas itself, at the narrow width, before any
// refit — and that fit was the 54% chart as surely as a refit was.
test("a chart first drawn beside a peek is held at the floor, the seat in view", async () => {
  location.hash = "#/company/org?peek=seat%3Adevrel";
  standCanvasAt(700);
  await mount();
  await settleChart();
  const drawn = onScreen("DevRel");
  expect(drawn.scale).toBeCloseTo(LEGIBLE_ZOOM, 6);
  expect((drawn.left + drawn.right) / 2).toBeCloseTo(350, 3);
});

// ONLY THE READER GOES BELOW IT. Fit pressed by the reader is a request for
// the whole company, legible or not, and it is drawn.
test("the reader's own Fit draws the whole chart, below the floor if it must", async () => {
  standCanvasAt(VIEWPORT.width);
  await mount();
  await act(async () => {
    fireEvent.click(card("DevRel"));
  });
  standCanvasAt(700);
  await settleChart();
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Fit to view" }));
  });
  await settleChart();
  const whole = spread(700);
  expect(onScreen("CEO").scale).toBeLessThan(LEGIBLE_ZOOM);
  expect(whole.left).toBeGreaterThanOrEqual(0);
  expect(whole.right).toBeLessThanOrEqual(700);
});

// `[` AND `]` ON A CARD STEP THE PEEK. The tree reads every printable key a
// card is sent as type-ahead, so pressing a card and then `]` — the way a
// reader walks the company — reached neither the tree's search nor the rail.
// A step REPLACES the address, as the rail's own buttons do.
test("] and [ pressed on a card step the peek through the chart's order", async () => {
  standCanvasAt(VIEWPORT.width);
  await mount();
  await act(async () => {
    fireEvent.click(card("SWE"));
  });
  expect(location.hash).toContain("peek=seat%3Aswe");
  const pushes = vi.spyOn(history, "pushState");
  await act(async () => {
    card("SWE").focus();
    fireEvent.keyDown(card("SWE"), { key: "]" });
  });
  await settleChart();
  expect(location.hash).toContain("peek=seat%3Afe");
  await act(async () => {
    fireEvent.keyDown(document.activeElement ?? card("FE"), { key: "[" });
  });
  expect(location.hash).toContain("peek=seat%3Aswe");
  expect(pushes).not.toHaveBeenCalled();
  pushes.mockRestore();
});

test("closing the peek fits the chart to the width it gets back, centred", async () => {
  location.hash = REPORTING;
  standCanvasAt(VIEWPORT.width);
  await mount();
  const opened = spread(VIEWPORT.width);
  await act(async () => {
    fireEvent.click(card("DevRel"));
  });
  standCanvasAt(700);
  await settleChart();
  await act(async () => {
    location.hash = REPORTING;
    window.dispatchEvent(new HashChangeEvent("hashchange"));
  });
  standCanvasAt(VIEWPORT.width);
  await settleChart();
  const back = spread(VIEWPORT.width);
  // THE VIEW THE CHART OPENED ON, and nothing cut at either edge.
  expect(back.left).toBeCloseTo(opened.left, 0);
  expect(back.right).toBeCloseTo(opened.right, 0);
  expect(Math.abs(back.margins[0] - back.margins[1])).toBeLessThanOrEqual(2);
});

// A READER'S OWN VIEW IS THEIRS. Somebody who zoomed in to read the cards and
// then pressed one was thrown back out to the whole company by the peek the
// press opened; a view the reader moved stays at their zoom, with the pressed
// card revealed in it.
test("a reader who zoomed keeps their zoom when the peek narrows the canvas", async () => {
  standCanvasAt(VIEWPORT.width);
  await mount();
  // The canvas says it is ready, and the chart hears it, before a reader can
  // press anything.
  await settleChart();
  const fitted = onScreen("CEO").scale;
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Zoom in" }));
  });
  await settleChart();
  const zoomed = onScreen("CEO").scale;
  expect(zoomed).toBeGreaterThan(fitted);
  await act(async () => {
    fireEvent.click(card("DevRel"));
  });
  standCanvasAt(420);
  await settleChart();
  const drawn = onScreen("DevRel");
  expect(drawn.scale).toBeCloseTo(zoomed, 5);
  expect(drawn.left).toBeGreaterThanOrEqual(0);
  expect(drawn.right).toBeLessThanOrEqual(420);
});

// AND PRESSING FIT HANDS THE CHART BACK. The reader's own Fit is remembered
// as a fit, so the next width change fits the chart again rather than keeping
// a view the reader already gave up.
test("after the reader presses Fit, a new width is fitted again", async () => {
  standCanvasAt(VIEWPORT.width);
  await mount();
  await settleChart();
  const opened = spread(VIEWPORT.width);
  // Zoomed in, the reader's view is theirs: the peek keeps it.
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Zoom in" }));
  });
  await settleChart();
  await act(async () => {
    fireEvent.click(card("DevRel"));
  });
  standCanvasAt(700);
  await settleChart();
  // Fit, pressed beside the peek, is a fit the chart has never been drawn at…
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Fit to view" }));
  });
  await settleChart();
  // …and remembered as one, so closing the peek fits the chart again.
  await act(async () => {
    location.hash = "#/company/org";
    window.dispatchEvent(new HashChangeEvent("hashchange"));
  });
  standCanvasAt(VIEWPORT.width);
  await settleChart();
  const back = spread(VIEWPORT.width);
  expect(back.left).toBeCloseTo(opened.left, 0);
  expect(back.right).toBeCloseTo(opened.right, 0);
});

// A PHONE READS THE CHART AS ROWS. Fitted into 360 pixels a company of cards
// is drawn at a third of its size; eased onto its root it sat mid-canvas under
// an empty band at half size. The kit's own narrow-screen view of a hierarchy
// is its tree grid, and the rows say what the cards say, at full size.
test("on a phone the chart is an outline of rows, with the legend at its head", async () => {
  const phone = installWindow(390);
  try {
    await mount();
    expect(document.querySelector(".crewlet-canvas")).toBeNull();
    const outline = screen.getByRole("treegrid", { name: "Org chart" });
    // THE SEATS' ROWS, past the grid's header row.
    const rows = within(outline)
      .getAllByRole("row")
      .filter((r) => r.querySelector(".oc-name"));
    // Every seat, indented under its manager, in the chart's order.
    const names = rows.map((r) => r.querySelector(".oc-name")?.textContent);
    expect(names).toEqual(["Jane Founder", "CEO", "CTO", "SWE", "FE", "PM", "DevRel"]);
    const swe = rows[3]!;
    expect(swe.getAttribute("aria-level")).toBe("4");
    // A HUMAN ROW SAYS WHAT ITS CARD SAYS, for this reader the availability.
    expect(rows[0]!.textContent).toContain("Human · reviews releases");
    expect(swe.textContent).toContain("Engineering · Core");
    expect(swe.textContent).toContain("Executing ENG-412");
    // THE LEGEND HEADS IT, as a line of the page.
    const legend = screen.getByRole("group", { name: "Seats by what they are doing" });
    expect(legend.hasAttribute("data-inline")).toBe(true);
    expect(legend.compareDocumentPosition(outline) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    // A ROW'S NAME OPENS THE SEAT beside the chart, and Enter on a row does too.
    await act(async () => {
      fireEvent.click(within(swe).getByRole("link", { name: "SWE" }));
    });
    expect(location.hash).toContain("peek=seat%3Aswe");
    await act(async () => {
      location.hash = "#/company/org";
      window.dispatchEvent(new HashChangeEvent("hashchange"));
    });
    const pm = rows[5]!;
    await act(async () => {
      pm.focus();
      fireEvent.keyDown(pm, { key: "Enter" });
    });
    expect(location.hash).toContain("peek=seat%3Apm");
  } finally {
    phone.restore();
  }
});

// THE CHEVRON STANDS BESIDE THE NAME. A phone's row is three lines, and the
// kit centres its chevron on the whole row — beside the unit line, under the
// badge, pointing at nothing. jsdom lays nothing out, so the cascade is read:
// the chevron is set on the row's first line, at the badge's own inset.
test("on a phone a row's chevron stands on the name's line", async () => {
  const read = (path: string) => readFileSync(join(process.cwd(), path), "utf8");
  const sheets = [
    read("node_modules/@crewlethq/ui/dist/styles.css"),
    read("src/styles/screens.css"),
  ].map((text) => {
    const style = document.createElement("style");
    style.textContent = text;
    document.head.append(style);
    return style;
  });
  const phone = installWindow(390);
  try {
    await mount();
    const outline = screen.getByRole("treegrid", { name: "Org chart" });
    const toggle = outline.querySelector<HTMLElement>(".crewlet-tree-grid__toggle")!;
    expect(getComputedStyle(toggle).alignSelf).toBe("flex-start");
    const row = outline.querySelector<HTMLElement>(".oc-row")!;
    // THE BADGE'S OWN INSET: the row pads its first line by this, and the
    // chevron and the badge are both 26px, so one inset puts them on one line.
    expect(getComputedStyle(toggle).getPropertyValue("margin-block-start")).toBe(
      getComputedStyle(row).getPropertyValue("padding-block"),
    );
  } finally {
    phone.restore();
    for (const s of sheets) s.remove();
  }
});

// THE NAME IS WHAT A FOUND SEAT IS READ BY. The kit's hint never shrinks and
// its label does, so at a search field's width every option read
// "@agent-ceo · Leadership · Exec…" with the seat's NAME squeezed to nothing.
// jsdom lays nothing out, so the cascade over the options this finder draws
// is what is read: the name may not shrink, the hint gives way, and the list
// may grow past the field.
test("a found seat's option keeps its name and lets the hint give way", async () => {
  const read = (path: string) => readFileSync(join(process.cwd(), path), "utf8");
  const sheets = [
    read("node_modules/@crewlethq/ui/dist/styles.css"),
    read("src/styles/screens.css"),
  ].map((text) => {
    const style = document.createElement("style");
    style.textContent = text;
    document.head.append(style);
    return style;
  });
  try {
    await mount();
    const field = screen.getByPlaceholderText("Find a seat");
    await act(async () => {
      fireEvent.change(field, { target: { value: "c" } });
    });
    const option = screen.getByRole("option", { name: /^CEO/ });
    const label = option.querySelector<HTMLElement>(".crewlet-combobox__label")!;
    const hint = option.querySelector<HTMLElement>(".crewlet-listbox__hint")!;
    expect(label.textContent).toBe("CEO");
    expect(getComputedStyle(label).flexShrink).toBe("0");
    expect(getComputedStyle(hint).minWidth).toBe("0px");
    expect(getComputedStyle(hint).textOverflow).toBe("ellipsis");
    const panel = option.closest<HTMLElement>(".crewlet-combobox__panel")!;
    expect(getComputedStyle(panel).width).toBe("max-content");
  } finally {
    for (const s of sheets) s.remove();
  }
});
