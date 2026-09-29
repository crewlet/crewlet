/**
 * The facts a node wears, and the one that had no reader at all.
 *
 * `internal/api/queries/fleet.go` has written `projections_ready` /
 * `projections_total` on every node row since they were added, with a comment
 * saying where the fact belongs: "the fleet view", because it is the answer to
 * "why is the new node holding nothing". The client type never declared them
 * and no screen read them, so the answer carried the fact and nobody could see
 * it — the same shape `config_revision_id` was in until it was fixed in this
 * same file.
 *
 * Asserted over the facts function rather than through a render because that
 * is where page and rail agree: `ObjectHeader` takes this list on both.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";

import { Fleet, HeldSince, NodePeek, nodeFacts } from "./Fleet.tsx";
import { Shell } from "~/app/Shell.tsx";
import { Router } from "~/app/router.tsx";
import { PAGE_ACTIONS_SLOT } from "~/app/frame/PageActions.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, QueryError, Store } from "~/protocol/index.ts";
import type { FleetNode } from "~/protocol/types.ts";
import { fmtDateTime, relTime } from "~/lib/format.ts";

function node(over: Partial<FleetNode> = {}): FleetNode {
  return { id: "n1", roles: [], seats: 0, config_epoch: 3, ...over };
}

function fact(n: FleetNode, label: string) {
  return nodeFacts({ node: n, target: 3 }).find((f) => f.label === label);
}

describe("nodeFacts", () => {
  test("a node that is still replaying says how far it has come", () => {
    // Two integers, not a bool: "3 of 5" and "0 of 2" are the two readings an
    // operator has to tell apart, which is why the engine sends a pair.
    expect(fact(node({ projections_ready: 3, projections_total: 5 }), "Copies")?.value).toBe(
      "3 of 5 ready",
    );
    expect(fact(node({ projections_ready: 0, projections_total: 2 }), "Copies")?.value).toBe(
      "0 of 2 ready",
    );
  });

  // A BUILD STRING IS ONE TOKEN. In one fact track the clamp broke a
  // pseudo-version as "v0.0.0-" over "20260929172955-…"; a token fact takes
  // two tracks on one line and carries the whole string in its title.
  test("the build version is one token, titled with the whole string", () => {
    const version = "v0.0.0-20260929172955-0123456789ab";
    const f = nodeFacts({ node: node(), target: 3, version }).find((x) => x.label === "Version");
    expect(f?.token).toBe(true);
    render(<>{f?.value}</>);
    expect(screen.getByText(version).getAttribute("title")).toBe(version);
    cleanup();
    // Unknown stays a worded empty, never a token-styled blank.
    expect(nodeFacts({ node: node(), target: 3 }).find((x) => x.label === "Version")?.token).toBe(
      undefined,
    );
  });

  test("a node that published nothing states no reading at all", () => {
    // ABSENT IS NOT ZERO. The node publishes the pair only once the total is
    // non-zero, so a dash here would claim a reading the engine does not keep
    // — and `FactLine` drops an empty value, which is the honest rendering.
    expect(fact(node(), "Copies")?.value).toBe("");
    expect(fact(node({ projections_total: 0 }), "Copies")?.value).toBe("");
  });

  // THE ROLES ARE A FACT, AND A SET: the Nodes table gives its Roles column
  // way beside a peek, so the peek has to say them, on one line of chips.
  test("a node's roles are a set fact, drawn whole on its page and in its peek", () => {
    const roles = fact(node({ roles: ["ingress", "seats", "workers"] }), "Roles");
    expect(roles?.set).toBe(true);
    render(<>{roles?.value}</>);
    for (const role of ["ingress", "seats", "workers"])
      expect(screen.getByText(role)).toBeDefined();
    cleanup();
  });

  test("the reading sits beside the epoch, because it is the other question", () => {
    // A node can hold the current revision and still be replaying the log its
    // state is derived from; only one of those makes its seats servable.
    const labels = nodeFacts({
      node: node({ projections_ready: 1, projections_total: 4 }),
      target: 3,
    }).map((f) => f.label);
    expect(labels.indexOf("Copies")).toBe(labels.indexOf("Epoch") + 1);
  });
});

// A REFUSED READ IS UNKNOWN, NOT ZERO. With no reading ever taken the screen
// drew "0 nodes" in its header, four tiles of 0, "No nodes are reporting" and
// a banner calling the refusal "the last reading that succeeded" — while the
// Settings column beside it said 1 node. The frame now answers a reader it
// knows holds no credential (see App.test.tsx); this is the screen's own half,
// for the reader nobody has answered for yet, whose read is refused anyway.
describe("the fleet, refused before any reading", () => {
  class InertWebSocket {
    static CONNECTING = 0;
    static OPEN = 1;
    static CLOSED = 3;
    readyState = InertWebSocket.CONNECTING;
    send(): void {}
    close(): void {}
  }

  afterEach(() => {
    cleanup();
    location.hash = "#/";
  });

  test("draws the refusal alone: no count, no tiles, no empty fleet", async () => {
    Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
    location.hash = "#/settings/nodes";
    const store = new Store();
    const socket = new LiveSocket(store);
    (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
      what === "viewer" ? new Promise(() => {}) : Promise.reject(new QueryError("unauthorized"));
    const view = render(
      <ClientContext.Provider value={{ store, socket }}>
        <Router>
          <Shell>
            <Fleet />
          </Shell>
        </Router>
      </ClientContext.Provider>,
    );
    expect(await screen.findByText(/This answer is auth-gated/)).toBeDefined();
    expect(document.getElementById(PAGE_ACTIONS_SLOT)?.textContent).toBe("");
    expect(view.container.querySelector(".crewlet-stat-card")).toBeNull();
    expect(screen.queryByText("No nodes are reporting")).toBeNull();
    expect(screen.queryByText(/last reading that succeeded/)).toBeNull();
  });
});

// SINCE IS THE TENURE'S START, and unknown is said as unknown. The lease's
// `acquired_at` is stamped when the epoch is minted and carried through every
// renewal, so it is the answer to "when did this seat last move". A lease an
// older build wrote carries none — and the grid's own date cell says "Never"
// for an absent date, which would claim a seat was never acquired.
describe("the seat's held-since column", () => {
  afterEach(cleanup);
  const now = Date.parse("2031-05-01T10:02:00Z");

  test("a stamped tenure reads as how long ago it began, with the instant in its title", () => {
    // The lease also expires in an hour, so a cell reading the lease's END
    // rather than its START would say "in 1h" and title 11:02, not 08:02.
    const acquired = "2031-05-01T08:02:00Z";
    render(
      <HeldSince
        lease={{ handle: "swe", node: "n1", acquired_at: acquired, expires_in: 3600 }}
        now={now}
      />,
    );
    const cell = screen.getByText(relTime(acquired, now));
    expect(cell.textContent).toMatch(/2h/);
    expect(cell.getAttribute("title")).toBe(fmtDateTime(acquired));
    expect(cell.getAttribute("title")).not.toBe(
      fmtDateTime(new Date(now + 3600_000).toISOString()),
    );
  });

  test("an unstamped lease is not recorded, never 'Never'", () => {
    const view = render(<HeldSince lease={{ handle: "swe", node: "n1" }} now={now} />);
    expect(view.container.textContent).not.toContain("Never");
    expect(screen.getByText(/Not recorded/)).toBeDefined();
  });
});

// THE NAME IS THE ONE FLEXIBLE TRACK. Each lease table exists to say which
// seat (or which duty) sits where, so that is the value that must keep its
// width: as a second `1fr`, Held by took half of what the fixed columns left —
// about 155px for a nine-character node id — and "Agent Frontend SWE" was the
// value cut to "Agent Frontend …" at a 1440 frame. Every other column sizes to
// its content, so the name absorbs whatever width the panel has.
describe("the lease tables", () => {
  class InertWebSocket {
    static CONNECTING = 0;
    static OPEN = 1;
    static CLOSED = 3;
    readyState = InertWebSocket.CONNECTING;
    send(): void {}
    close(): void {}
  }

  afterEach(() => {
    cleanup();
    location.hash = "#/";
  });

  test("give the seat and the duty the only flexible track, first", async () => {
    Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
    location.hash = "#/settings/nodes";
    const answer = {
      nodes: [node({ id: "harness-0", seats: 1 })],
      seats: [
        {
          handle: "agent-frontend-swe",
          node: "harness-0",
          acquired_at: "2031-05-01T08:02:00Z",
          expires_in: 33,
        },
      ],
      duties: [{ duty: "integration-reconciler", node: "harness-0", expires_in: 60 }],
      unplaceable: [],
      unmanned_roles: [],
      this_node: "harness-0",
      target_epoch: 3,
    };
    const store = new Store();
    const socket = new LiveSocket(store);
    (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
      what === "fleet" ? Promise.resolve(answer) : new Promise(() => {});
    const view = render(
      <ClientContext.Provider value={{ store, socket }}>
        <Router>
          <Fleet />
        </Router>
      </ClientContext.Provider>,
    );
    expect(await screen.findByText("integration-reconciler")).toBeDefined();
    const leaseGrids = [...view.container.querySelectorAll<HTMLElement>(".grid-wrap")].filter((g) =>
      /Held by/i.test(g.textContent ?? ""),
    );
    expect(leaseGrids.length).toBe(2);
    for (const grid of leaseGrids) {
      const tracks =
        grid.style.gridTemplateColumns.match(/minmax\([^)]*\)|fit-content\([^)]*\)|\S+/g) ?? [];
      const flexible = tracks.flatMap((t, i) => (t.includes("1fr") ? [i] : []));
      expect(flexible, grid.style.gridTemplateColumns).toEqual([0]);
    }
  });

  // A PAST STAMP, so the relative reading is stable for the life of the test.
  const ACQUIRED = "2020-05-01T08:02:00Z";
  const placed = {
    nodes: [node({ id: "harness-0", seats: 1 })],
    seats: [
      { handle: "agent-frontend-swe", node: "harness-0", acquired_at: ACQUIRED, expires_in: 33 },
    ],
    duties: [{ duty: "integration-reconciler", node: "harness-0", expires_in: 60 }],
    unplaceable: [],
    unmanned_roles: [],
    this_node: "harness-0",
    target_epoch: 3,
  };

  function mountFleet(id?: string) {
    Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
    location.hash = id ? `#/settings/nodes/${id}` : "#/settings/nodes";
    const store = new Store();
    const socket = new LiveSocket(store);
    (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
      what === "fleet" ? Promise.resolve(placed) : new Promise(() => {});
    return render(
      <ClientContext.Provider value={{ store, socket }}>
        <Router>
          <Fleet node={id} />
        </Router>
      </ClientContext.Provider>,
    );
  }

  /** Each grid on screen, by its column heads, with the text of each body row. */
  function grids(container: HTMLElement) {
    return [...container.querySelectorAll<HTMLElement>(".grid-wrap")].map((g) => ({
      heads: [...g.querySelectorAll(".grid-head > .grid-th")].map((h) => h.textContent?.trim()),
      rows: [...g.querySelectorAll<HTMLElement>(".grid-row")],
    }));
  }

  // WHERE A SEAT IS, AND SINCE WHEN (S-39). The since cell is tested on its
  // own above; this holds the COLUMN in both tables that carry a seat's
  // lease, which is what a reader actually sees — deleting it from either
  // table left every other case here green.
  test("Seat placement says since when each seat has been where it is", async () => {
    const view = mountFleet();
    expect(await screen.findByText("integration-reconciler")).toBeDefined();
    const seats = grids(view.container).find((g) => g.heads.includes("Seat"));
    expect(seats?.heads).toEqual(["Seat", "Held by", "Since", "Lease"]);
    const since = relTime(ACQUIRED, Date.now());
    expect(seats?.rows).toHaveLength(1);
    expect(seats?.rows[0]?.textContent).toContain(since);
  });

  test("a node's own Leases table says since when it has held each seat", async () => {
    const view = mountFleet("harness-0");
    expect(await screen.findByText("integration-reconciler")).toBeDefined();
    const seats = grids(view.container).find((g) => g.heads.includes("Seat"));
    expect(seats?.heads).toEqual(["Seat", "Since", "Lease"]);
    expect(seats?.rows[0]?.textContent).toContain(relTime(ACQUIRED, Date.now()));
  });

  // ONE NODE ID, ONE TREATMENT. The seat table drew it as unlinked primary
  // ink, heavier than the seat's own name, and the duty table beside it drew
  // the same id as a link: two looks for one value on one page. Both are the
  // same link to the node's page now — the seat row's own link is an overlay,
  // so a cell may link inside it.
  test("Held by is the same link to the node in both lease tables", async () => {
    const view = mountFleet();
    expect(await screen.findByText("integration-reconciler")).toBeDefined();
    const held = [...view.container.querySelectorAll(".grid-wrap .held-by")];
    expect(held).toHaveLength(2);
    for (const cell of held) {
      expect(cell.tagName).toBe("A");
      expect(cell.textContent).toBe("harness-0");
      expect(cell.getAttribute("href")).toBe("#/settings/nodes/harness-0");
    }
  });
});

// THE NODES TABLE HAS ONE FLEXIBLE TRACK, AND IT IS THE NODE. Roles, Seats and
// In flight were three `1fr` columns, so they split whatever the fixed columns
// left: at 1440 a single digit sat in 150px of air while the role chips
// wrapped onto three lines, and under a peek each column fell to a letter —
// "R.", "S.", "I." — and the chips to empty pills. Now every fact sizes to its
// content, and where the row cannot hold them all, whole columns give way in
// a declared order, each one a fact the node's peek already carries.
describe("the Nodes table", () => {
  class InertWebSocket {
    static CONNECTING = 0;
    static OPEN = 1;
    static CLOSED = 3;
    readyState = InertWebSocket.CONNECTING;
    send(): void {}
    close(): void {}
  }

  const answer = {
    nodes: [
      node({
        id: "harness-0",
        roles: ["ingress", "seats", "workers"],
        seats: 7,
        in_flight: 0,
        posture: "serve",
        config_status: "ok",
        expires_in: 31,
        started_at: "2020-05-01T08:00:00Z",
      }),
    ],
    seats: [],
    duties: [],
    unplaceable: [],
    unmanned_roles: [],
    this_node: "harness-0",
    target_epoch: 3,
  };

  function mount(ui: React.ReactNode, hash = "#/settings/nodes") {
    Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
    location.hash = hash;
    const store = new Store();
    const socket = new LiveSocket(store);
    (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
      what === "fleet" ? Promise.resolve(answer) : new Promise(() => {});
    return render(
      <ClientContext.Provider value={{ store, socket }}>
        <Router>{ui}</Router>
      </ClientContext.Provider>,
    );
  }

  /** The Nodes grid: the one whose heads begin with Node. */
  function nodesGrid(container: HTMLElement) {
    const grid = [...container.querySelectorAll<HTMLElement>(".grid-wrap")].find(
      (g) => g.querySelector(".grid-th")?.textContent?.trim().startsWith("Node") ?? false,
    );
    expect(grid, "the Nodes grid is drawn").toBeDefined();
    return grid!;
  }

  const heads = (grid: HTMLElement) =>
    [...grid.querySelectorAll(".grid-head > .grid-th")].map((h) => h.textContent?.trim());

  afterEach(() => {
    cleanup();
    location.hash = "#/";
  });

  test("the node is the one flexible track, and it is never narrower than its name", async () => {
    const view = mount(<Fleet />);
    expect(await screen.findByText("workers")).toBeDefined();
    const tracks =
      nodesGrid(view.container).style.gridTemplateColumns.match(
        /minmax\([^)]*\)|fit-content\([^)]*\)|\S+/g,
      ) ?? [];
    expect(tracks).toHaveLength(8);
    // The node: flexible, floored at its content, so an id never wraps or cuts.
    expect(tracks[0]).toBe("minmax(max-content, 1fr)");
    // Roles: the chips' own width, so a chip is never a pill without its word.
    expect(tracks[1]).toBe("max-content");
    // Every other fact: content-sized. No second `1fr` shares the spare width.
    for (const track of tracks.slice(2)) expect(track).toMatch(/^fit-content\(/);
    expect(tracks.filter((t) => t.includes("1fr"))).toHaveLength(1);
  });

  // THE ROW THE RAIL IS OPEN ON IS MARKED, as the work list marks its peeked
  // item, so the table says which node the rail beside it is about.
  test("the node the peek is open on is the marked row", async () => {
    const view = mount(<Fleet />, "#/settings/nodes?peek=node:harness-0");
    expect(await screen.findByText("workers")).toBeDefined();
    const rows = [...nodesGrid(view.container).querySelectorAll(".grid-row")];
    expect(rows).toHaveLength(1);
    expect(rows[0]!.classList.contains("selected")).toBe(true);
    cleanup();
    const unpeeked = mount(<Fleet />);
    expect(await screen.findByText("workers")).toBeDefined();
    expect(nodesGrid(unpeeked.container).querySelector(".grid-row.selected")).toBeNull();
  });

  // LAID OUT, as the browser measured each column at its content: the node
  // with its "this one" tag, the three role chips on one line, and every
  // other column at its head's width. A peek at 1440 leaves the table 486px,
  // a 1280 window 746px and a 1440 one 906px.
  describe("laid out", () => {
    const NATURAL: Record<string, number> = {
      Node: 165,
      Roles: 181,
      Seats: 60,
      "In flight": 79,
      Posture: 77,
      Config: 68,
      Lease: 60,
      "Up since": 78,
    };
    let box = 486;
    const real = globalThis.ResizeObserver;
    beforeEach(() => {
      globalThis.ResizeObserver = class {
        observe(): void {}
        unobserve(): void {}
        disconnect(): void {}
      } as unknown as typeof ResizeObserver;
      vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockImplementation(function (
        this: HTMLElement,
      ) {
        return this.classList.contains("grid-wrap") ? box : 0;
      });
      // Each head is its column's natural width, laid end to end.
      vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
        this: HTMLElement,
      ) {
        const width = (el: Element) => {
          const text = el.textContent?.trim() ?? "";
          const name = Object.keys(NATURAL).find((k) => text.startsWith(k));
          return name ? NATURAL[name]! : 60;
        };
        if (this.classList.contains("grid-th")) {
          const siblings = [...this.parentElement!.children];
          const left = siblings.slice(0, siblings.indexOf(this)).reduce((n, h) => n + width(h), 0);
          const right = left + width(this);
          return { left, right, width: width(this), top: 0, bottom: 0, height: 0 } as DOMRect;
        }
        const right = this.classList.contains("grid-wrap") ? box : 0;
        return { left: 0, right, width: right, top: 0, bottom: 0, height: 0 } as DOMRect;
      });
    });
    afterEach(() => {
      vi.restoreAllMocks();
      globalThis.ResizeObserver = real;
      box = 486;
    });

    async function drawn(width: number) {
      box = width;
      const view = mount(<Fleet />);
      expect(await screen.findByText("harness-0")).toBeDefined();
      const grid = nodesGrid(view.container);
      return {
        heads: heads(grid),
        hidden: grid.parentElement?.querySelector(".grid-foot")?.textContent ?? "",
      };
    }

    test("beside a peek it gives way whole columns, and never cuts one to a letter", async () => {
      const at = await drawn(486);
      expect(at.heads).toEqual(["Node", "Seats", "In flight", "Posture", "Config"]);
      expect(at.hidden).toContain("Hidden to fit: Lease, Up since and Roles");
    });

    test("at a 1280 window it keeps the roles on their line and gives up the lease alone", async () => {
      const at = await drawn(746);
      expect(at.heads).toEqual([
        "Node",
        "Roles",
        "Seats",
        "In flight",
        "Posture",
        "Config",
        "Up since",
      ]);
      expect(at.hidden).toContain("Hidden to fit: Lease");
    });

    test("at a 1440 window it draws every column", async () => {
      const at = await drawn(906);
      expect(at.heads).toEqual(Object.keys(NATURAL));
      expect(at.hidden).not.toContain("Hidden to fit");
    });

    // A COLUMN HIDDEN IN FAVOUR OF THE PEEK HAS TO BE IN IT. Squeezed to
    // nothing, every column that can give way does — and each one it names is
    // a fact the node's own peek draws, so closing nothing and opening the
    // node still answers what the row stopped saying.
    test("every column it can give way is one the node's peek carries", async () => {
      const at = await drawn(160);
      const hidden = at.hidden.replace(/^.*Hidden to fit:\s*/, "").split(/,\s*|\s+and\s+/);
      expect(hidden.sort()).toEqual(["In flight", "Lease", "Roles", "Up since"]);
      cleanup();
      mount(<NodePeek id="harness-0" />);
      expect(await screen.findByText("Its own lease")).toBeDefined();
      const IN_THE_PEEK: Record<string, string> = {
        Lease: "Its own lease",
        "Up since": "Up since",
        "In flight": "In flight",
        Roles: "Roles",
      };
      for (const column of hidden) {
        expect(screen.getAllByText(IN_THE_PEEK[column]!).length, column).toBeGreaterThan(0);
      }
      expect(screen.getByText("workers")).toBeDefined();
    });
  });
});
