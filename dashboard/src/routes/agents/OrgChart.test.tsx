/**
 * The live org chart as a reader meets it: a card per seat off the applied
 * projection, a unit box with its project, the legend's counts, and a card
 * that opens the seat beside the chart.
 */

import { act, cleanup, fireEvent, render, screen, within } from "~/test/inCase.ts";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { OrgChart } from "./OrgChart.tsx";
import { LEGIBLE_ZOOM } from "~/ui/canvasView.ts";
import { findSeats } from "./header.tsx";
import { CONFIG_WRITE_REASONS } from "~/lib/useWriteAccess.ts";
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

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

// EACH ROW CARRIES THE HANDLE IT IS PAIRED BY, and the agent id the store
// keeps it under: a card wears the row of the seat its handle names, never one
// that merely shares the seat's name.
const AGENTS = [
  { role: "CEO", handle: "ceo", agent_id: "a-ceo", activity: "idle", last_turn: null },
  {
    role: "CTO",
    handle: "cto",
    agent_id: "a-cto",
    activity: "working",
    live_call: { phase: "review", work_item: { key: "ENG-409" } },
  },
  {
    role: "SWE",
    handle: "swe",
    agent_id: "a-swe",
    activity: "working",
    live_call: { phase: "execute", work_item: { key: "ENG-412" } },
  },
  {
    role: "FE",
    handle: "fe",
    agent_id: "a-fe",
    activity: "needs",
    turn: { turn_id: "t", started_at: "", stage: "parked" },
  },
  { role: "PM", handle: "pm", agent_id: "a-pm", activity: "stopped", stopped_reason: "budget" },
  { role: "DevRel", handle: "devrel", agent_id: "a-devrel", activity: "idle" },
] as unknown as AgentRow[];

let restore: () => void;
beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  location.hash = "#/agents";
  // jsdom has no layout: the chart's cards are measured through this.
  restore = LayoutObserver.install();
});

afterEach(() => {
  cleanup();
  restore();
  location.hash = "";
});

/** A reader signed in without `config:write`: they may read the chart, not change it. */
const READER = { login: "ada.lovelace", grants: ["state:read"], handle: "", acts: [] };

async function mount(viewer: Record<string, unknown> = READER, agents: AgentRow[] = AGENTS) {
  const store = new Store();
  store.applyHealth({ status: "healthy" });
  store.setConnected(true);
  store.applyOrg(CHART_ORG);
  // THE ROSTER, which `applySeats` sets whole; `applyAgents` only patches the
  // live overlay of a seat the roster already holds.
  store.applySeats(agents);
  const socket = new LiveSocket(store);
  const asked: string[] = [];
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) => {
    asked.push(what);
    if (what === "viewer") return Promise.resolve(viewer);
    if (what === "work_projects") {
      return Promise.resolve({
        projects: [{ key: "ENG", unit: { key: "Core", name: "Core", resolved: true } }],
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
  return { asked };
}

function card(name: string): HTMLElement {
  // BY ROLE ATTRIBUTE rather than the accessibility tree: the canvas holds
  // its world out of it until a layout has been measured, which jsdom never
  // reports as done.
  const found = [...document.querySelectorAll<HTMLElement>("[role=treeitem]")].find(
    (el) => el.querySelector(".oc-name")?.textContent === name,
  );
  if (!found) throw new Error(`no card for ${name}`);
  return found;
}

// A CARD WEARS THE ROW ITS HANDLE NAMES, whatever name the row carries: paired
// by name, two seats sharing one wore each other's state, and a row whose name
// had moved wore none.
//
// Mutation: pair the cards by `role` against the seat's name again, and SWE's
// card says nothing about ENG-412.
test("a card is paired with its live row by handle, never by name", async () => {
  await mount(
    READER,
    AGENTS.map((a) => ({ ...a, role: "Somebody else" })),
  );
  expect(within(card("SWE")).getByText("Executing ENG-412")).toBeTruthy();
});

// A CARD PER SEAT, SAYING WHAT THE ENGINE SAYS IT IS DOING.
test("every seat of the applied chart is a card with its place and state line", async () => {
  await mount();
  for (const name of ["Jane Founder", "CEO", "CTO", "SWE", "FE", "PM", "DevRel"]) {
    expect(card(name)).toBeTruthy();
  }
  expect(within(card("SWE")).getByText("Engineering · Core")).toBeTruthy();
  expect(within(card("SWE")).getByText("Executing ENG-412")).toBeTruthy();
  expect(within(card("PM")).getByText("Stopped · budget")).toBeTruthy();
  expect(within(card("Jane Founder")).getByText("Human · reviews releases")).toBeTruthy();
  expect(card("SWE").getAttribute("data-state")).toBe("working");
});

// THE BOX NAMES THE UNIT AND ITS PROJECT.
test("a unit under its lead is boxed with the project its work is filed under", async () => {
  await mount();
  const head = [...document.querySelectorAll(".crewlet-tree-canvas__group-head")].map(
    (el) => el.textContent,
  );
  expect(head).toContain("ENG Engineering · Core");
});

// THE LEGEND IS THE ENGINE'S COUNT, and the same one `stateCounts` takes.
test("the legend's figures are the activity counts", async () => {
  await mount();
  const counts = stateCounts(indexOrg(CHART_ORG), AGENTS);
  const legend = screen.getByRole("group", { name: "Seats by what they are doing" });
  expect(legend.textContent).toBe(
    `Working ${counts.working}Needs you ${counts.needs}Stopped ${counts.stopped}Idle ${counts.idle}`,
  );
  expect(counts).toEqual({ working: 2, needs: 1, stopped: 1, idle: 2 });
});

// A CARD OPENS THE SEAT BESIDE THE CHART, addressed by its handle.
test("pressing a card opens that seat's peek", async () => {
  await mount();
  await act(async () => {
    fireEvent.click(card("SWE"));
  });
  expect(location.hash).toContain("peek=seat%3Aswe");
});

// ADD SEAT IS NEVER HIDDEN: a reader who may not change the org sees it held,
// with the reason — the GRANT a structural chart write is decided by, never
// "an operator token".
test("Add seat is held with its reason for a reader without config:write", async () => {
  await mount();
  const add = screen.getByRole("button", { name: /Add seat/ });
  expect(add.getAttribute("aria-disabled") === "true" || add.hasAttribute("disabled")).toBe(true);
  expect(add.getAttribute("title")).toBe(CONFIG_WRITE_REASONS.no_grant);
});

test("a config:write holder's Add seat goes to the builder, adding an agent", async () => {
  await mount({ login: "jane.founder", grants: ["config:write"], handle: "jane", acts: [] });
  const add = screen.getByRole("link", { name: /Add seat/ });
  expect(add.getAttribute("href")).toBe("#/agents/edit?add=agent");
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

/**
 * Report the canvas — the kit's viewport and the chart's own box — at a width,
 * and the two probes a UNIT BOX is laid out by (its padding and the height of
 * its label), which the builder's harness has no groups to know about. Named
 * by the kit's classes for the reason the box test above names its head.
 */
function standCanvasAt(width: number): void {
  canvasWidth = width;
  const base = LayoutObserver.sizer;
  LayoutObserver.sizer = (el) => {
    if (isCanvasViewport(el) || el.classList.contains("oc-chart")) {
      return { width: canvasWidth, height: VIEWPORT.height };
    }
    if (el.classList.contains("crewlet-tree-canvas__group-pad")) return { width: 12, height: 12 };
    if (el.classList.contains("crewlet-tree-canvas__group-head")) return { width: 160, height: 28 };
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
    location.hash = "#/agents?peek=seat%3Adevrel";
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

// A NEW WIDTH IS FITTED AGAIN. The canvas fits once; the peek then took a
// third of it away, and the least pan that kept the pressed card in view left
// the rest of the company cut at the other edge — and closing the peek left it
// cut there, with the freed third of the canvas empty. The chart is fitted
// again, all of it, in the middle, whenever the canvas's width moves — at 800
// this chart fits at 92%, above the floor.
test("a peek that narrows the canvas fits the whole chart into it, centred", async () => {
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
  location.hash = "#/agents?peek=seat%3Adevrel";
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
  standCanvasAt(VIEWPORT.width);
  await mount();
  const opened = spread(VIEWPORT.width);
  await act(async () => {
    fireEvent.click(card("DevRel"));
  });
  standCanvasAt(700);
  await settleChart();
  await act(async () => {
    location.hash = "#/agents";
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
    location.hash = "#/agents";
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
      location.hash = "#/agents";
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
