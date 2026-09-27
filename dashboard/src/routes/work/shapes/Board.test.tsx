/**
 * The board as a place work is MOVED, and what its cards say is happening.
 *
 * Its failure modes are all ones a screenshot passes. A strip joined on the
 * trigger's `work_key` drew a running turn on whichever task a webhook
 * mentioned; a price where the tokens belong; a drag that moved the card and
 * never told the engine, or told it and drew the card where the engine did not
 * put it; a refusal that left the card in a lane the task is not in; and a
 * read-only reader offered a drag that could only ever be refused.
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { Board, lanesOutOfView } from "./Board.tsx";
import { ItemsView } from "../ItemsView.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { WorkGroup, WorkSummary } from "~/protocol/index.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

const JANE = {
  operator_id: "U0FOUNDER",
  operator: true,
  handle: "jane",
  name: "Jane Founder",
  kind: "human",
  acts: ["place_work_item", "update_work_item"],
};

const ORG = {
  name: "Nimbus",
  timezone: "UTC",
  roles: [
    { name: "Jane Founder", handle: "jane", kind: "human" },
    { name: "SWE", handle: "swe", kind: "agent" },
  ],
  units: [],
};

const NOW = Date.parse("2031-04-16T12:00:00Z");

const row = (key: string, over: Partial<WorkSummary> = {}): WorkSummary => ({
  id: key.toLowerCase(),
  key,
  project: "ENG",
  title: `the task ${key}`,
  type: "task",
  status: "todo",
  updated: "2031-04-16T00:00:00Z",
  version: 7,
  ...over,
});

/** The three open lanes, as the engine answers a status board. */
function lanes(): WorkGroup[] {
  return [
    { key: "todo", count: 2, rows: [row("ENG-1"), row("ENG-2")] },
    { key: "in_progress", count: 1, rows: [row("ENG-3", { status: "in_progress" })] },
    { key: "done", count: 0, rows: [] },
  ];
}

type Reply = { status: number; body: unknown };

let posted: { tool: string; args: Record<string, unknown> }[];
let reply: Reply;

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  posted = [];
  reply = {
    status: 200,
    body: {
      tool: "place_work_item",
      outcome: "applied",
      position: "CREWLET_TRACKER_LOG@1:99",
      receipt: { placed: true },
    },
  };
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      const tool = decodeURIComponent(String(url).split("/operator/act/")[1] ?? "");
      const body = JSON.parse(init.body as string) as { args: Record<string, unknown> };
      posted.push({ tool, args: body.args });
      return new Response(JSON.stringify(reply.body), {
        status: reply.status,
        headers: { "Content-Type": "application/json" },
      });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "";
});

function client(viewer: Record<string, unknown>, agents: Record<string, unknown>[] = []) {
  const store = new Store();
  store.applyHealth({ status: "healthy", nodes: 1 } as never);
  store.applyOrg(ORG as never);
  store.applyAgents(agents as never);
  const socket = new LiveSocket(store);
  const asked: { kind: string; params: Record<string, unknown> }[] = [];
  const answers: Record<string, unknown> = { viewer };
  socket.query = ((what: string, params?: Record<string, unknown>) => {
    asked.push({ kind: what, params: params ?? {} });
    return Promise.resolve(answers[what] ?? {});
  }) as typeof socket.query;
  return { store, socket, asked, answers };
}

/** The board alone, over the given lanes, for the given reader. */
function mountBoard({
  groups = lanes(),
  viewer = JANE,
  agents = [],
  movable = true,
  props = {},
}: {
  groups?: WorkGroup[];
  viewer?: Record<string, unknown>;
  agents?: Record<string, unknown>[];
  movable?: boolean;
  props?: Partial<Parameters<typeof Board>[0]>;
} = {}) {
  const c = client(viewer, agents);
  const draw = (next: WorkGroup[]) => (
    <ToastProvider>
      <LayerHost>
        <ClientContext.Provider value={{ store: c.store, socket: c.socket }}>
          <ViewerProvider>
            <Board
              now={NOW}
              groups={next}
              axis="status"
              chrome={{ seatName: (h) => ORG.roles.find((r) => r.handle === h)?.name ?? h }}
              hrefOf={(r) => `#/work/${r.key}`}
              onOpen={() => {}}
              onOverflow={() => {}}
              overflowHref={() => "#/work"}
              movable={movable}
              {...props}
            />
          </ViewerProvider>
        </ClientContext.Provider>
      </LayerHost>
    </ToastProvider>
  );
  const view = render(draw(groups));
  return { ...c, rerender: (next: WorkGroup[]) => view.rerender(draw(next)), view };
}

async function settle() {
  await act(async () => {
    for (let i = 0; i < 8; i++) await Promise.resolve();
  });
}

/** The card for a key, as the anchor the reader focuses. */
const cardOf = (key: string) =>
  screen.getByText(key, { selector: ".work-key" }).closest("a") as HTMLAnchorElement;

/** Which lane a card is drawn in, by the lane's own name. */
const laneOf = (key: string) =>
  cardOf(key).closest("section")?.querySelector(".work-col-name")?.textContent ?? "";

// ---------------------------------------------------------------------------
// What a card says
// ---------------------------------------------------------------------------

describe("the card", () => {
  // TOKENS, NEVER MONEY — the one measure of cost this dashboard draws.
  test("spend renders tokens, compactly, and names the unit for a screen reader", async () => {
    mountBoard({
      groups: [
        { key: "todo", count: 1, rows: [row("ENG-1", { spend: { tokens: 38_200 } as never })] },
      ],
    });
    await settle();
    const tokens = cardOf("ENG-1").querySelector(".work-card-tokens") as HTMLElement;
    expect(tokens.textContent).toBe("38.2k tokens");
    expect(tokens.getAttribute("title")).toBe("38,200 tokens");
    expect(cardOf("ENG-1").textContent).not.toMatch(/\$/);
  });

  // WHAT THE TASK HOLDS UP, and the labels it carries — the facts only a card
  // draws, asked for with `fields=` by the list that draws it.
  test("a card names what it blocks and its labels", async () => {
    mountBoard({
      groups: [
        {
          key: "todo",
          count: 1,
          rows: [row("ENG-1", { dependents_count: 2, tags: ["api", "flaky"] })],
        },
      ],
      props: { detail: { tags: [{ slug: "flaky", label: "Flaky" }] } as never },
    });
    await settle();
    const card = cardOf("ENG-1");
    expect(within(card).getByText("blocks 2")).toBeTruthy();
    expect(within(card).getByText("api")).toBeTruthy();
    expect(within(card).getByText("Flaky")).toBeTruthy();
  });

  // THE BAND IS A STATE AND SAYS SO IN WORDS: the seat and what it is doing on
  // THIS task, and a run parked waiting on the reader — never somebody else's.
  test("a running turn and a run waiting on the reader each say so in words", async () => {
    mountBoard({
      props: {
        facts: {
          live: new Map([
            [
              "ENG-1",
              { handle: "swe", doing: "executing · round 7 of 20", since: "2031-04-16T11:54:00Z" },
            ],
          ]),
          waiting: new Map([["ENG-3", { since: "2031-04-16T11:22:00Z" }]]),
          ring: () => undefined,
        },
      },
    });
    await settle();
    expect(cardOf("ENG-1").querySelector(".work-card-live")?.textContent).toBe(
      "SWE · executing · round 7 of 206m",
    );
    expect(cardOf("ENG-3").querySelector(".work-card-live")?.textContent).toContain(
      "Waiting on you · coding run parked",
    );
    expect(cardOf("ENG-2").querySelector(".work-card-live")).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// A drag is a write, made as the reader
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Lanes past the window
// ---------------------------------------------------------------------------

describe("lanes out of view", () => {
  // WHOLLY IN VIEW OR NAMED: a lane cut at the scroller's edge is a lane whose
  // cards cannot be read, so it counts as out of view on its side.
  test("a lane cut at either edge is out of view on that side", () => {
    const lanes = [0, 290, 580, 870, 1160, 1450].map((left) => ({ left, width: 274 }));
    expect(lanesOutOfView(lanes, 0, 1144)).toEqual({ before: [], after: [4, 5] });
    // Four whole lanes and a fraction of a pixel's rounding is still whole.
    expect(lanesOutOfView(lanes, 0, 1143.5)).toEqual({ before: [], after: [4, 5] });
    // Scrolled by one page: the first two are behind, nothing ahead.
    expect(lanesOutOfView(lanes, 580, 1144)).toEqual({ before: [0, 1], after: [] });
    // Mid-way, a lane straddling each edge is out of view on its own side.
    expect(lanesOutOfView(lanes, 100, 1144)).toEqual({ before: [0], after: [4, 5] });
  });

  // THE BOARD NAMES WHAT IS PAST ITS EDGE, above the lanes where a reader looks
  // — its scrollbar is under the tallest lane — and the button brings them in.
  test("the lanes past the edge are named above the board and one press pages to them", async () => {
    const LANE = 274;
    const GAP = 16;
    const VIEW = 4 * LANE + 3 * GAP;
    const restore = [
      stubGetter("offsetLeft", function (this: HTMLElement) {
        const at = [...(this.parentElement?.children ?? [])].indexOf(this);
        return this.classList.contains("work-col") ? at * (LANE + GAP) : 0;
      }),
      stubGetter("offsetWidth", function (this: HTMLElement) {
        return this.classList.contains("work-col") ? LANE : 0;
      }),
      stubGetter("clientWidth", function (this: HTMLElement) {
        return this.classList.contains("work-board") ? VIEW : 0;
      }),
    ];
    const scrolled: number[] = [];
    HTMLElement.prototype.scrollBy = function (opts?: ScrollToOptions | number) {
      scrolled.push(typeof opts === "object" ? (opts.left ?? 0) : Number(opts));
    } as typeof HTMLElement.prototype.scrollBy;
    try {
      mountBoard({
        groups: [
          ...lanes(),
          { key: "in_review", count: 0, rows: [] },
          { key: "cancelled", count: 1, rows: [row("ENG-7", { status: "cancelled" })] },
          { key: "closed", count: 1, rows: [row("ENG-8", { status: "closed" })] },
        ],
      });
      await settle();
      const pager = screen.getByRole("group", { name: "Lanes out of view" });
      const ahead = within(pager).getByRole("button", {
        name: "Show 2 more lanes: Cancelled, Closed",
      });
      // ONE PHRASING AT EVERY WIDTH: the count, never the bare names, which
      // read as two status words rather than as a control revealing lanes.
      expect(ahead.textContent).toBe("2 more lanes");
      expect(ahead.getAttribute("title")).toBe("Cancelled, Closed");
      expect(document.querySelector(".work-board-frame")?.getAttribute("data-more-end")).toBe(
        "true",
      );
      fireEvent.click(ahead);
      expect(scrolled).toEqual([VIEW]);
    } finally {
      restore.forEach((undo) => undo());
      delete (HTMLElement.prototype as { scrollBy?: unknown }).scrollBy;
    }
  });

  // AND ON THE WORK SCREEN THEY ARE NAMED IN THE BAR, not on a row of their
  // own: the approved board starts its lanes directly under one bar, and a
  // line above them spent a row on every board that was wider than its window.
  test("given the bar's slot, the lanes past the edge are named there and nowhere above the lanes", async () => {
    const LANE = 274;
    const GAP = 16;
    const VIEW = 4 * LANE + 3 * GAP;
    const restore = [
      stubGetter("offsetLeft", function (this: HTMLElement) {
        const at = [...(this.parentElement?.children ?? [])].indexOf(this);
        return this.classList.contains("work-col") ? at * (LANE + GAP) : 0;
      }),
      stubGetter("offsetWidth", function (this: HTMLElement) {
        return this.classList.contains("work-col") ? LANE : 0;
      }),
      stubGetter("clientWidth", function (this: HTMLElement) {
        return this.classList.contains("work-board") ? VIEW : 0;
      }),
    ];
    const slot = document.createElement("span");
    document.body.appendChild(slot);
    try {
      mountBoard({
        groups: [
          ...lanes(),
          { key: "in_review", count: 0, rows: [] },
          { key: "cancelled", count: 1, rows: [row("ENG-7", { status: "cancelled" })] },
          { key: "closed", count: 1, rows: [row("ENG-8", { status: "closed" })] },
        ],
        props: { laneSlot: slot },
      });
      await settle();
      const pager = within(slot).getByRole("group", { name: "Lanes out of view" });
      expect(within(pager).getByRole("button", { name: /Cancelled, Closed/ })).toBeTruthy();
      expect(document.querySelector(".work-board-head")).toBeNull();
    } finally {
      restore.forEach((undo) => undo());
      slot.remove();
    }
  });

  // AND NOTHING IS SAID WHEN EVERY LANE FITS: a pager over a board with nothing
  // past its edge would be a control that does nothing.
  test("a board whose lanes all fit draws no pager", async () => {
    mountBoard();
    await settle();
    expect(screen.queryByRole("group", { name: "Lanes out of view" })).toBeNull();
    expect(document.querySelector(".work-board-frame")?.hasAttribute("data-more-end")).toBe(false);
  });
});

/** Replace one layout getter on every element; answers the undo. */
function stubGetter(name: "offsetLeft" | "offsetWidth" | "clientWidth", get: () => number) {
  const proto = name === "clientWidth" ? Element.prototype : HTMLElement.prototype;
  const before = Object.getOwnPropertyDescriptor(proto, name);
  Object.defineProperty(proto, name, { configurable: true, get });
  return () => {
    if (before) Object.defineProperty(proto, name, before);
  };
}

describe("moving a card", () => {
  // INTO ANOTHER LANE IS A STATUS AND A PLACE, in one call, conditional on the
  // version the card was drawn at — and the neighbour it went above, never an
  // index into a board somebody else may have rearranged.
  test("a drag across lanes sends the status and the neighbour", async () => {
    mountBoard();
    await settle();
    // Alt+→ is the same gesture as a pointer drop: the top of the lane beside.
    fireEvent.keyDown(cardOf("ENG-1"), { key: "ArrowRight", altKey: true });
    await settle();
    expect(posted).toEqual([
      {
        tool: "place_work_item",
        args: { item: "ENG-1", before: "ENG-3", status: "in_progress", if_match: 7 },
      },
    ]);
  });

  // A POINTER DROP at the bottom of an empty lane is a status change alone:
  // there is no neighbour to be placed against.
  test("a pointer drop into an empty lane sends the status alone", async () => {
    mountBoard();
    await settle();
    const data = new Map<string, string>();
    const dataTransfer = {
      setData: (k: string, v: string) => data.set(k, v),
      getData: (k: string) => data.get(k) ?? "",
      effectAllowed: "",
      dropEffect: "",
    };
    fireEvent.dragStart(cardOf("ENG-2"), { dataTransfer });
    const done = screen.getByRole("region", { name: /^Done/ });
    fireEvent.dragOver(done, { dataTransfer });
    fireEvent.drop(done, { dataTransfer });
    await settle();
    expect(posted).toEqual([
      { tool: "place_work_item", args: { item: "ENG-2", status: "done", if_match: 7 } },
    ]);
  });

  // WITHIN A LANE IT IS A PLACE ALONE, and a drop where the card already was
  // sends nothing — the engine would refuse it as a move that moves nothing.
  test("a reorder names its neighbour and a drop in place sends nothing", async () => {
    mountBoard();
    await settle();
    fireEvent.keyDown(cardOf("ENG-2"), { key: "ArrowUp", altKey: true });
    await settle();
    expect(posted.at(-1)).toEqual({
      tool: "place_work_item",
      args: { item: "ENG-2", before: "ENG-1", if_match: 7 },
    });
    posted = [];
    fireEvent.keyDown(cardOf("ENG-3"), { key: "ArrowUp", altKey: true });
    await settle();
    expect(posted).toEqual([]);
  });

  // CONFIRMED, NOT OPTIMISTIC: a refusal puts the card back where the engine
  // has it and says why in the engine's words.
  test("a refused move snaps back with the engine's sentence", async () => {
    reply = { status: 409, body: { error: "stale_version", tool: "place_work_item", detail: "" } };
    mountBoard();
    await settle();
    fireEvent.keyDown(cardOf("ENG-1"), { key: "ArrowRight", altKey: true });
    await settle();
    expect(laneOf("ENG-1")).toBe("To do");
    expect(cardOf("ENG-1").getAttribute("data-pending")).toBeNull();
    expect(document.querySelector(".write-refusal")?.textContent).toContain(
      "Changed by somebody else since you opened it",
    );
  });

  // THE COUNTS TRAVEL WITH THE CARD while the engine decides — a heading still
  // reading the old tally over a card already drawn under it is two claims
  // that disagree — and a refusal puts both back with the card.
  test("a pending move moves the two lane counts and a refusal restores them", async () => {
    let answer!: (r: Response) => void;
    vi.stubGlobal(
      "fetch",
      vi.fn(() => new Promise<Response>((resolve) => (answer = resolve))),
    );
    mountBoard();
    await settle();
    const counts = () =>
      [...document.querySelectorAll(".work-col-count")].map((el) => el.textContent);
    expect(counts()).toEqual(["2", "1", "0"]);
    fireEvent.keyDown(cardOf("ENG-1"), { key: "ArrowRight", altKey: true });
    await settle();
    expect(cardOf("ENG-1").getAttribute("data-pending")).toBe("true");
    expect(counts()).toEqual(["1", "2", "0"]);
    await act(async () => {
      answer(
        new Response(
          JSON.stringify({ error: "stale_version", tool: "place_work_item", detail: "" }),
          { status: 409, headers: { "Content-Type": "application/json" } },
        ),
      );
    });
    await settle();
    expect(laneOf("ENG-1")).toBe("To do");
    expect(counts()).toEqual(["2", "1", "0"]);
  });

  // A LANE CHANGE THAT LANDED STAYS. The move is two records, and when the
  // place is refused after the status landed the card is in its new lane —
  // snapping it back would draw a status that is no longer true.
  test("a placed: false answer keeps the card in its new lane and says the place was not taken", async () => {
    reply = {
      status: 200,
      body: {
        tool: "place_work_item",
        outcome: "applied",
        position: "CREWLET_TRACKER_LOG@1:99",
        receipt: { placed: false, unplaced: "ENG-1 is in progress, but not above ENG-3." },
      },
    };
    const { rerender } = mountBoard();
    await settle();
    fireEvent.keyDown(cardOf("ENG-1"), { key: "ArrowRight", altKey: true });
    await settle();
    expect(laneOf("ENG-1")).toBe("In progress");
    expect(cardOf("ENG-1").getAttribute("data-pending")).toBeNull();
    expect(screen.getByText("ENG-1 is in progress, but not above ENG-3.")).toBeTruthy();
    // AND THE RE-READ IS WHAT DRAWS IT FROM THEN ON, at the place the engine
    // kept: the overlay goes with the first new answer.
    const reread = lanes();
    reread[0]!.rows = [row("ENG-2")];
    reread[1]!.rows = [
      row("ENG-3", { status: "in_progress" }),
      row("ENG-1", { status: "in_progress" }),
    ];
    rerender(reread);
    await settle();
    const order = [
      ...screen.getByRole("region", { name: /^In progress/ }).querySelectorAll(".work-key"),
    ];
    expect(order.map((el) => el.textContent)).toEqual(["ENG-3", "ENG-1"]);
  });

  // DRAG ONLY UNDER THE MANUAL ORDER: in any other order the card would jump
  // back to where its date or priority puts it, which reads as a refusal that
  // was not one — so the board says which order moves cards instead.
  //
  // AND IT SAYS SO WHEN SOMEBODY TRIES, not on a line above every board: the
  // hint cost the lanes a row on each visit to explain a gesture most visits
  // never make.
  test("outside the manual order a card does not lift, and a try says why", async () => {
    mountBoard({ movable: false });
    await settle();
    expect(cardOf("ENG-1").getAttribute("draggable")).toBeNull();
    expect(screen.queryByText(/Cards move in the manual order/)).toBeNull();
    fireEvent.keyDown(cardOf("ENG-1"), { key: "ArrowRight", altKey: true });
    await settle();
    expect(posted).toEqual([]);
    expect(screen.getByText(/Cards move in the manual order/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Dismiss" }));
    expect(screen.queryByText(/Cards move in the manual order/)).toBeNull();
  });

  test("a pointer drag outside the manual order is caught and answered", async () => {
    mountBoard({ movable: false });
    await settle();
    const start = new Event("dragstart", { bubbles: true, cancelable: true });
    act(() => {
      cardOf("ENG-1").dispatchEvent(start);
    });
    expect(start.defaultPrevented).toBe(true);
    expect(screen.getByText(/Cards move in the manual order/)).toBeTruthy();
  });
});

// ---------------------------------------------------------------------------
// Who may move a card
// ---------------------------------------------------------------------------

// A READ-ONLY READER CANNOT DRAG, and is told why in the sentence every other
// write control uses — a card that simply would not lift reads as a broken
// board.
describe.each([
  { who: "an anonymous reader", viewer: { anonymous: true }, reason: WRITE_REASONS.anonymous },
  {
    who: "an unbound token",
    viewer: { operator_id: "ci", operator: true, handle: "", unbound: true },
    reason: WRITE_REASONS.unbound,
  },
  {
    who: "a person the engine does not move cards for",
    viewer: { ...JANE, acts: ["update_work_item"] },
    reason: WRITE_REASONS.not_served,
  },
])("$who", ({ viewer, reason }) => {
  test("cannot drag, and is told why once", async () => {
    mountBoard({ viewer });
    await settle();
    expect(cardOf("ENG-1").getAttribute("draggable")).toBeNull();
    fireEvent.keyDown(cardOf("ENG-1"), { key: "ArrowRight", altKey: true });
    await settle();
    expect(posted).toEqual([]);
    expect(screen.getByText(/Moving cards is off/).textContent).toContain(reason);
  });
});

// ---------------------------------------------------------------------------
// The board inside the list
// ---------------------------------------------------------------------------

describe("the board on the work screen", () => {
  function mountList(agents: Record<string, unknown>[]) {
    location.hash = "#/work?shape=board";
    const c = client(JANE, agents);
    c.answers.work_items = {
      items: [],
      groups: [
        { key: "todo", count: 2, rows: [row("ENG-1"), row("ENG-2")] },
        { key: "in_progress", count: 0, rows: [] },
      ],
      total_hint: 2,
      complete: true,
    };
    render(
      <ToastProvider>
        <LayerHost>
          <ClientContext.Provider value={{ store: c.store, socket: c.socket }}>
            <Router>
              <ViewerProvider>
                <ItemsView />
              </ViewerProvider>
            </Router>
          </ClientContext.Provider>
        </LayerHost>
      </ToastProvider>,
    );
    return c;
  }

  const working = (item: string, key = "ENG-9") => ({
    id: "swe",
    role: "SWE",
    handle: "swe",
    activity: "working",
    live_call: {
      turn_id: "t-1",
      work_key: key,
      phase: "execute",
      round_num: 3,
      max_rounds: 20,
      work_item: { id: item.toLowerCase(), key: item, project: "ENG" },
    },
    turn: { started_at: "2031-04-16T11:54:00Z", stage: "running" },
  });

  // THE STRIP IS ON THE TASK THE TURN IS CHARGED TO, and on no card the
  // trigger's work key happens to name.
  test("a running turn is drawn on the task it is charged to, never on its work key", async () => {
    mountList([working("ENG-2", "ENG-1")]);
    await settle();
    expect(cardOf("ENG-2").querySelector(".work-card-live")?.textContent).toContain(
      "SWE · executing · round 3 of 20",
    );
    expect(cardOf("ENG-1").querySelector(".work-card-live")).toBeNull();
  });

  // A TURN ENDING ON A CARD IS A CARD THAT JUST CHANGED, and nothing pushes the
  // row — so the list asks again then, rather than a poll later.
  test("the board asks again when a turn on a card it draws ends", async () => {
    const c = mountList([working("ENG-2")]);
    await settle();
    const before = c.asked.filter((a) => a.kind === "work_items").length;
    act(() =>
      c.store.applyAgents([{ id: "swe", role: "SWE", handle: "swe", activity: "idle" }] as never),
    );
    await settle();
    expect(c.asked.filter((a) => a.kind === "work_items").length).toBeGreaterThan(before);
  });

  // THE BOARD OPENS ON RECENT: its last lane is Done, and under Open that lane
  // admits nothing. Recent is open work plus what finished since the start of
  // the week, asked as the engine's own token so the week is the company's.
  test("a board opens on Recent, asking for the week's finished work", async () => {
    const c = mountList([]);
    await settle();
    const asked = c.asked.findLast((a) => a.kind === "work_items")!.params;
    expect(asked.closed_since).toBe("sow");
    expect(asked.show_closed).toBeUndefined();
    expect(asked.status_group).toBeUndefined();
    // AND THE CARD'S OWN FACTS, opt-in on the wire.
    expect(asked.fields).toBe("tags,dependents_count,open_asks,spend");
    expect(screen.getByRole("combobox", { name: "Which work" }).textContent).toContain("Recent");
  });
});
