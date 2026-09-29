/**
 * What the turns list says a turn IS.
 *
 * A turn that launched a detached coding run PARKS: it completes a segment and
 * will complete again when the run is collected. The engine used to list it as
 * finished the moment it parked, and before that distinction existed this
 * screen had only two words for a row — finished, or `running`. A parked turn
 * is neither, and drawing it as either sends a reader to the wrong place: a
 * "running" turn with no live phase reads as a wedged loop, and a finished one
 * hides that its real work is still out in a box.
 */

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test } from "vitest";

import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { TurnRow } from "~/protocol/index.ts";
import { Turns } from "./Turns.tsx";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
});

afterEach(() => {
  cleanup();
  location.hash = "";
});

function row(id: string, summary: string, extra: Partial<TurnRow>): TurnRow {
  const at = new Date(Date.now() - 60_000).toISOString();
  return {
    turn_id: id,
    role: "CEO",
    started_at: at,
    ended_at: at,
    duration_ms: 0,
    complete: false,
    parked: false,
    phases: 1,
    iterations: 1,
    failed: false,
    input_tokens: 10,
    output_tokens: 2,
    total_tokens: 12,
    cache_read_tokens: 0,
    cache_write_tokens: 0,
    summary,
    ...extra,
  };
}

/** The engine's axis over the turns that ended: `total` of them, `failed` failed. */
function series(total: number, failed = 0, coverage?: unknown) {
  const at = new Date(Date.now() - 3_600_000).toISOString();
  return {
    bucket: "hour",
    since: at,
    until: new Date().toISOString(),
    bars: [{ at, count: total, failed }],
    total,
    failed,
    by_category: {},
    ...(coverage ? { coverage } : {}),
  };
}

type Asked = { kind: string; params: Record<string, unknown> };

function mountWith(
  answers: Record<string, unknown | ((p: Record<string, unknown>) => unknown)>,
  hash = "#/live/turns",
  org: unknown = { roles: [{ name: "CEO", handle: "ceo" }] },
): { asked: Asked[] } {
  location.hash = hash;
  const store = new Store();
  store.applyHealth({ status: "healthy" });
  store.applyOrg(org as never);
  const socket = new LiveSocket(store);
  const asked: Asked[] = [];
  (
    socket as unknown as {
      query: (what: string, params?: Record<string, unknown>) => Promise<unknown>;
    }
  ).query = (what, params) => {
    asked.push({ kind: what, params: params ?? {} });
    const answer = answers[what];
    return Promise.resolve(
      typeof answer === "function"
        ? (answer as (p: Record<string, unknown>) => unknown)(params ?? {})
        : (answer ?? {}),
    );
  };
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Turns />
      </Router>
    </ClientContext.Provider>,
  );
  return { asked };
}

function mount(turns: TurnRow[], agents: unknown[] = []) {
  location.hash = "#/live/turns";
  const store = new Store();
  store.applyHealth({ status: "healthy" });
  store.applyOrg({ roles: [{ name: "CEO", handle: "ceo" }] });
  if (agents.length) store.applyAgents(agents);
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what: string) =>
    Promise.resolve(
      what === "turns"
        ? { turns, next: null }
        : what === "event_series"
          ? series(turns.length)
          : {},
    );
  return render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Turns />
      </Router>
    </ClientContext.Provider>,
  );
}

const SUMMARIES = ["the finished one", "the parked one", "the running one", "the stopped one"];

/** The widest element around one summary that holds no other row's — its row,
 *  whatever markup the table draws a row with. */
function rowOf(summary: string): HTMLElement {
  let at: HTMLElement = screen.getByText(summary);
  const others = SUMMARIES.filter((s) => s !== summary);
  while (at.parentElement && !others.some((s) => at.parentElement!.textContent?.includes(s))) {
    at = at.parentElement;
  }
  return at;
}

// NO COMPLETION RECORD IS TWO STATES, told apart by the overlay exactly as
// the turn's own page tells them: a seat on the turn is running it, and none
// means it stopped before it ended. Both read "running" here, beside a turn
// page calling the second "not settled".
test("a parked turn is marked parked, and an open one running or not settled by its seat", async () => {
  mount(
    [
      row("t-done", "the finished one", { complete: true, duration_ms: 4_000 }),
      row("t-parked", "the parked one", { parked: true, duration_ms: 3_000 }),
      row("t-live", "the running one", {}),
      row("t-dead", "the stopped one", {}),
    ],
    [{ role: "CEO", activity: "working", turn: { turn_id: "t-live", stage: "phase" } }],
  );
  await screen.findByText("the parked one");

  await waitFor(() => expect(rowOf("the parked one").textContent).toContain("parked"));
  expect(rowOf("the parked one").textContent).not.toContain("running");
  expect(rowOf("the running one").textContent).toContain("running");
  expect(rowOf("the running one").textContent).not.toMatch(/parked|not settled/);
  expect(rowOf("the stopped one").textContent).toContain("not settled");
  expect(rowOf("the stopped one").textContent).not.toContain("running");
  expect(rowOf("the finished one").textContent).not.toMatch(/parked|running|settled/);
});

/**
 * ONE SEAT'S TURNS ARE ASKED FOR BY ITS HANDLE.
 *
 * The sidebar's seat rows link here with `seat=<handle>`, and the list used to
 * read a `role` parameter instead — so every one of those rows landed on the
 * whole company's turns. The engine resolves the handle to the seat's own id,
 * which a role name shared by two unit seats cannot name.
 */
test("the seat in the address is sent to the engine as its handle", async () => {
  location.hash = "#/live/turns?seat=ceo";
  const store = new Store();
  store.applyHealth({ status: "healthy" });
  store.applyOrg({ roles: [{ name: "CEO", handle: "ceo" }] });
  const socket = new LiveSocket(store);
  const asked: Record<string, unknown>[] = [];
  (
    socket as unknown as {
      query: (what: string, params?: Record<string, unknown>) => Promise<unknown>;
    }
  ).query = (what, params) => {
    if (what === "turns") asked.push(params ?? {});
    return Promise.resolve(what === "turns" ? { turns: [], next: null } : {});
  };
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Turns />
      </Router>
    </ClientContext.Provider>,
  );
  await waitFor(() => expect(asked.length).toBeGreaterThan(0));
  expect(asked[0]).toMatchObject({ seat: "ceo" });
  expect(asked[0]).not.toHaveProperty("role");
});

// A ROW NAMES ITS SEAT BY THE SEAT'S BADGE, AND ITS ITEM BY ONE UNBROKEN KEY.
//
// The seat cell drew a processor glyph where every other surface identifies a
// seat by its badge; the key after the summary broke at its hyphen into
// `ENG-` over `22`, making that row taller than its neighbours. `.item-key`
// is what holds the key on one line (base.css), and the summary truncates
// instead.
test("a row draws the seat's badge and its item key as one token", async () => {
  mount([
    row("t-1", "reviewed the work", {
      complete: true,
      work_item: { backend: "native", id: "x", key: "ENG-22", project: "ENG" },
    }),
  ]);
  await screen.findByText("reviewed the work");
  const r = rowOf("reviewed the work");
  expect(r.querySelector(".cell-seat .crewlet-avatar")).not.toBeNull();
  const key = [...r.querySelectorAll("span")].find((s) => s.textContent === "ENG-22");
  expect(key?.classList.contains("item-key")).toBe(true);
});

// THE CHART'S HINT GIVES WAY BEFORE ITS TITLE. It sat in the header's
// actions, which never shrink, so on a phone the title read "W" beside a
// whole sentence; the subtitle is the part the kit lets give way first.
test("the chart's hint is in the header's subtitle, not its actions", async () => {
  const { container } = mount([row("t-1", "one turn", { complete: true })]);
  await screen.findByText("one turn");
  const subtitle = container.querySelector(".crewlet-card__subtitle");
  expect(subtitle?.textContent).toMatch(/click one to narrow the window/);
  expect(container.querySelector(".crewlet-card__header-actions")?.textContent ?? "").not.toMatch(
    /narrow the window/,
  );
});

// A FAILED TURN WEARS THE DANGER TONE. It wore amber — the one state that asks
// a person for a decision — which a failed turn does not.
test("a turn that carried a failure is marked in the danger tone", async () => {
  mount([row("t-bad", "the failed one", { complete: true, failed: true })]);
  const tag = (await screen.findByText("failure")).closest(".crewlet-tag");
  expect(tag?.classList.contains("crewlet-tag--danger")).toBe(true);
});

// THE AXIS IS THE ENGINE'S COUNT OF THE TURNS THAT ENDED, over the whole
// window. It was folded from the page of rows this screen held, so a busy
// week drawn from the newest two hundred turns read as a quiet one — and a
// turn that parked on a coding run and resumed wrote two completions, so a
// count of the type alone drew it twice.
//
// Mutation: fold the bars from the rows again, and the total is the page's 1
// rather than the engine's 57.
test("the axis states the engine's total of the turns that ended", async () => {
  const { asked } = mountWith(
    {
      turns: { turns: [row("t-1", "one turn", { complete: true })], next: null },
      event_series: series(57, 4),
    },
    "#/live/turns?seat=ceo",
  );
  await screen.findByText("one turn");
  await screen.findByText(/57 turns ended/);
  expect(screen.getByText(/57 turns ended/).textContent).toMatch(/4 failed/);
  expect(screen.getByText(/57 turns in this window/)).toBeTruthy();
  const axis = asked.find((q) => q.kind === "event_series")!.params;
  expect(axis).toMatchObject({ type: "agent_turn_completed", suspended: "false", seat: "ceo" });
});

// THE TOKEN SORT IS THE ENGINE'S. Spend's "recent turns by tokens" lands
// here with `sort=-tokens`, and the list is ordered where the whole set is.
test("sort=-tokens asks the engine for the costliest turns first", async () => {
  const { asked } = mountWith(
    {
      turns: { turns: [row("t-1", "the costly one", { complete: true })], next: null },
      event_series: series(1),
    },
    "#/live/turns?sort=-tokens",
  );
  await screen.findByText("the costly one");
  expect(asked.find((q) => q.kind === "turns")?.params).toMatchObject({ sort: "-tokens" });
  expect(screen.getByRole("radio", { name: "Most tokens" }).getAttribute("aria-checked")).toBe(
    "true",
  );
});

// A NODE THAT DID NOT ANSWER IS NAMED. The fleet's turns are read from every
// node at query time, and a short list with no note reads as a quiet company.
test("a node missing from the answer is named above the list", async () => {
  mountWith({
    turns: {
      turns: [row("t-1", "one turn", { complete: true })],
      next: null,
      coverage: {
        complete: false,
        nodes: [
          { id: "node-a", answered: true, error: "" },
          { id: "node-b", answered: false, error: "no answer inside the fleet read budget" },
        ],
      },
    },
    event_series: series(1),
  });
  await screen.findByText("one turn");
  const note = await screen.findByText(/is missing one node/);
  expect(note.closest(".coverage-note")?.textContent).toContain(
    "node-b — no answer inside the fleet read budget",
  );
  expect(note.closest(".coverage-note")?.textContent).not.toContain("node-a");
});

// OLDER IS THE ENGINE'S CURSOR, and the older page is appended.
test("Load older asks for the page past the cursor and lists it", async () => {
  const { asked } = mountWith({
    turns: (p: Record<string, unknown>) =>
      p.before
        ? { turns: [row("t-2", "the older one", { complete: true })], next: null }
        : { turns: [row("t-1", "the newer one", { complete: true })], next: "cursor-1" },
    event_series: series(2),
  });
  const more = await screen.findByRole("button", { name: "Load older" });
  more.click();
  await screen.findByText("the older one");
  expect(screen.getByText("the newer one")).toBeTruthy();
  expect(asked.filter((q) => q.kind === "turns").at(-1)?.params).toMatchObject({
    before: "cursor-1",
  });
});

// A BAR PICKED IN THE PAST IS ASKED FOR AS ITSELF.
//
// The axis is a control — clicking a bar narrows the window to it — and the
// list under it asked the engine for "the last N days", N being the window's
// LENGTH: a one-hour bar three days ago was asked as the last day, every row
// that came back was newer than the bar, a client-side filter dropped them
// all, and the screen said "No turns in this window" under an axis counting
// twelve. The window's two instants are the question now, and the engine's
// answer is the list.
//
// Mutation: ask by `days` again (and filter the rows here), and the request
// carries no bounds and the bar's turn is never listed.
test("a window in the past asks the engine for that window and lists its turns", async () => {
  const from = new Date(Date.now() - 72 * 3_600_000);
  from.setUTCMinutes(0, 0, 0);
  const to = new Date(from.getTime() + 3_600_000);
  const started = new Date(from.getTime() + 600_000).toISOString();
  const { asked } = mountWith(
    {
      turns: (p: Record<string, unknown>) =>
        p.since === from.toISOString() && p.until === to.toISOString()
          ? {
              turns: [
                row("t-past", "the bar's own turn", {
                  complete: true,
                  started_at: started,
                  ended_at: started,
                }),
              ],
              next: null,
            }
          : { turns: [row("t-now", "a turn from today", { complete: true })], next: null },
      event_series: series(12),
    },
    `#/live/turns?window=${from.toISOString()}/${to.toISOString()}`,
  );
  await screen.findByText("the bar's own turn");
  const params = asked.find((q) => q.kind === "turns")!.params;
  expect(params).toMatchObject({ since: from.toISOString(), until: to.toISOString() });
  expect(params).not.toHaveProperty("days");
  expect(screen.queryByText("No turns in this window")).toBeNull();
});

// AN EMPTY PAGE WITH A CURSOR STILL OFFERS THE NEXT ONE. The fleet's cursor
// can be present on a page with no row — a node stopped before any turn
// above it could be shown — and "Load older" was drawn only beside rows, so
// the reader was left on an empty list with no way past it.
test("Load older is offered on an empty page that has more", async () => {
  mountWith({
    turns: (p: Record<string, unknown>) =>
      p.before
        ? { turns: [row("t-2", "found past the gap", { complete: true })], next: null }
        : { turns: [], next: "cursor-1" },
    event_series: series(1),
  });
  (await screen.findByRole("button", { name: "Load older" })).click();
  await screen.findByText("found past the gap");
});

// THE AXIS SAYS IT IS NOT NARROWED BY FAILURE. "Carried a failure" is a fact
// about a whole turn and the axis counts completion records, so the axis
// stays every turn — and says so, rather than drawing a total the list under
// it does not match.
test("with a failure filter the axis says it counts every turn", async () => {
  const { asked } = mountWith(
    {
      turns: { turns: [row("t-1", "a clean one", { complete: true })], next: null },
      event_series: series(9, 2),
    },
    "#/live/turns?failed=false",
  );
  await screen.findByText("a clean one");
  expect((await screen.findByText(/9 turns ended/)).textContent).toContain(
    "every turn, not only the filtered ones",
  );
  expect(asked.find((q) => q.kind === "turns")?.params).toMatchObject({ failed: "false" });
});

/**
 * THE SEAT MENU OFFERS EVERY AGENT, wherever the chart puts it.
 *
 * The org as `/org` answers it: the seats ABOVE every unit are the founders —
 * humans, named without a handle — and every agent sits in a unit. The menu
 * was built from the top-level roles alone, so on a real company it offered
 * "Every seat" and nothing else, and a `?seat=` from a link drew the bare
 * handle because no option matched it.
 *
 * Mutation: build the options from `org.roles` again and the agents vanish.
 */
test("the seat menu offers every agent in every unit, by name", async () => {
  const org = {
    name: "Nimbus",
    roles: [
      { name: "Jane Founder", kind: "human" },
      { name: "Maya Ops", kind: "human" },
    ],
    units: [
      {
        name: "Executives",
        roles: [
          { name: "Agent CEO", handle: "agent-ceo", kind: "agent" },
          { name: "Agent CTO", handle: "agent-cto", kind: "agent" },
        ],
        children: [
          { name: "Core", roles: [{ name: "Agent SWE", handle: "agent-swe", kind: "agent" }] },
        ],
      },
    ],
  };
  mountWith(
    { turns: { turns: [], next: null }, event_series: series(0) },
    "#/live/turns?seat=agent-cto",
    org,
  );
  const menu = await screen.findByRole("combobox", { name: "Seat" });
  // A SEAT FROM THE ADDRESS IS NAMED, not echoed as its handle.
  expect(menu.textContent).toContain("Agent CTO");
  fireEvent.click(menu);
  const options = await waitFor(() => {
    const rows = screen.getAllByRole("option");
    if (rows.length < 2) throw new Error("the listbox has not opened");
    return rows.map((r) => r.textContent?.trim());
  });
  expect(options).toEqual(["Every seat", "Agent CEO", "Agent CTO", "Agent SWE"]);
});

// THE STATE COLUMN IS HEADED. It sat under an empty heading between What it did
// and Iterations, which read as a gap in the table with badges floating in it.
test("the state badges sit under a State heading", async () => {
  mount([row("t-live", "the running one", {})]);
  await screen.findByText("the running one");
  const heads = screen.getAllByRole("columnheader").map((h) => h.textContent?.trim());
  expect(heads).toContain("State");
});
