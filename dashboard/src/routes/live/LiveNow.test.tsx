/**
 * Live › Now running: what each card says, and what it asks the engine.
 *
 * Every case is a way this screen was wrong before: a second "running now"
 * table of the calls the running turns already drew, a finished phase spliced
 * in under a reader mid-row, a strip counted from whatever the tab happened to
 * hold, a seat filter handed over as a role name two seats share, and an empty
 * state promising a record the engine had deleted.
 */

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { Router } from "~/app/router.tsx";
import { FrameReadings } from "~/app/Shell.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { SCREEN_SCROLL_ID } from "~/lib/scroller.ts";
import { TOP_SLACK_PX } from "~/lib/settled.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { EventEnvelope } from "~/protocol/index.ts";
import { LiveNow, stripOf } from "./LiveNow.tsx";
import { PHASE_PAGE } from "./RecentPhases.tsx";

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
  document.body.innerHTML = "";
  location.hash = "";
});

const JANE = {
  login: "U0FOUNDER",
  grants: [
    "config:read",
    "config:write",
    "secrets:write",
    "fleet:operate",
    "people:manage",
    "audit:read",
    "state:read",
    "work:write",
    "knowledge:write",
  ],
  handle: "jane",
  owner: "jane",
  name: "Jane Founder",
  kind: "human",
  acts: ["answer_run"],
};

/** Two unit seats stamped from ONE template: two seats, and the phases they
 *  record carry one role name between them. */
const ORG = {
  name: "Nimbus",
  roles: [
    { name: "Jane Founder", handle: "jane", kind: "human" },
    { name: "Search Engineer", handle: "eng-search", kind: "agent", role: "Engineer" },
    { name: "Payments Engineer", handle: "eng-payments", kind: "agent", role: "Engineer" },
  ],
  units: [],
};

const ago = (ms: number) => new Date(Date.now() - ms).toISOString();

/** A settled phase record, as `phases` and the push both carry it. */
function phaseEvent(id: string, turn: string, agent: string, over: Record<string, unknown> = {}) {
  return {
    id,
    type: "agent_phase_completed",
    timestamp: ago(60_000),
    source: "Engineer",
    actor: "Engineer",
    summary: "",
    category: "llm",
    trace_id: "",
    span_id: "",
    parent_span_id: "",
    topic: "",
    failed: false,
    payload: {
      turn_id: turn,
      phase: "execute",
      iteration: 1,
      role: "Engineer",
      agent_id: agent,
      model: "scripted",
      decision: "delivered",
      ...over,
    },
  } as EventEnvelope;
}

type Answer = unknown | ((params: Record<string, unknown>) => unknown);

function mount({
  hash = "#/live",
  agents = [],
  answers = {},
}: {
  hash?: string;
  agents?: Record<string, unknown>[];
  answers?: Record<string, Answer>;
} = {}) {
  location.hash = hash;
  const store = new Store();
  store.applyHealth({ status: "healthy" } as never);
  store.applyOrg(ORG as never);
  store.applySeats(agents as never);
  const socket = new LiveSocket(store);
  const asked: { kind: string; params: Record<string, unknown> }[] = [];
  const base: Record<string, Answer> = {
    viewer: JANE,
    work_inbox: { handle: "jane", notices: [], primary_reasons: [] },
    sandbox_runs: { runs: [] },
    phases: { phases: [], next: {}, exhausted: true },
    event_series: {
      bucket: "minute",
      since: ago(3_600_000),
      until: ago(0),
      bars: [],
      total: 0,
      by_category: {},
    },
    events: { events: [], next: null, exhausted: true },
  };
  socket.query = ((what: string, params?: Record<string, unknown>) => {
    asked.push({ kind: what, params: params ?? {} });
    const all = { ...base, ...answers };
    const answer = all[what];
    if (answer === undefined) return Promise.resolve({});
    return Promise.resolve(
      typeof answer === "function"
        ? (answer as (p: Record<string, unknown>) => unknown)(params ?? {})
        : answer,
    );
  }) as typeof socket.query;
  render(
    <ToastProvider>
      <LayerHost>
        <ClientContext.Provider value={{ store, socket }}>
          <FrameReadings>
            <Router>
              <LiveNow />
            </Router>
          </FrameReadings>
        </ClientContext.Provider>
      </LayerHost>
    </ToastProvider>,
  );
  return { store, asked };
}

const cardOf = (title: string) =>
  screen.getByRole("heading", { name: title }).closest(".crewlet-card") as HTMLElement;

// A TURN THAT HAS ONLY STARTED IS ON ITS CONTEXT STEP, AND IS TIMED FROM ITS
// START. `agent_turn_started` stamps the turn before any phase runs, so a seat
// gathering its context has a turn and no live call — and the row draws it
// rather than waiting for a first model call to exist.
//
// Mutation: read the current step off `live_call` alone and the row has none.
test("a turn that has only started is on its Context step, timed from its start", async () => {
  mount({
    agents: [
      {
        agent_id: "a1",
        id: "a1",
        role: "Search Engineer",
        handle: "eng-search",
        activity: "working",
        turn: { turn_id: "t-start", started_at: ago(125_000), stage: "context" },
      },
    ],
  });
  const card = cardOf("Running turns");
  const row = within(card).getByRole("link", { name: /Search Engineer/ });
  expect(row.getAttribute("href")).toBe("#/live/turns/t-start");
  expect(row.querySelector("[aria-current='step']")?.textContent).toContain("Context");
  expect(row.textContent).toMatch(/2m/);
});

// A ROUND THAT HAS STOPPED MOVING SAYS SO ON ITS OWN ROW, AND A PARKED TURN
// NEVER DOES. The watched-conditions card that used to restate it is gone;
// the mark is beside what it is about.
test("a quiet round is marked on its row, and a parked turn is not", async () => {
  const call = (turn: string) => ({
    turn_id: turn,
    phase: "execute",
    iteration: 1,
    model: "scripted",
    trigger: null,
    prompt: "",
    prompt_messages: null,
    response: "",
    input_tokens: 0,
    output_tokens: 0,
    total_tokens: 0,
    tool_executions: null,
    round_num: 2,
    rounds_used: 2,
    max_rounds: 25,
    in_progress: true,
    started_at: ago(900_000),
    updated_at: ago(700_000),
  });
  mount({
    agents: [
      {
        agent_id: "a1",
        id: "a1",
        role: "Search Engineer",
        handle: "eng-search",
        activity: "working",
        turn: { turn_id: "t-quiet", started_at: ago(900_000), stage: "execute" },
        live_call: call("t-quiet"),
      },
      {
        agent_id: "a2",
        id: "a2",
        role: "Payments Engineer",
        handle: "eng-payments",
        activity: "working",
        turn: { turn_id: "t-parked", started_at: ago(900_000), stage: "parked" },
        live_call: call("t-parked"),
      },
    ],
  });
  const card = cardOf("Running turns");
  const quiet = within(card).getByRole("link", { name: /Search Engineer/ });
  expect(quiet.textContent).toMatch(/stalled/);
  const parked = within(card).getByRole("link", { name: /Payments Engineer/ });
  expect(parked.textContent).not.toMatch(/stalled|no update/);
});

// THE RUNNING AND SETTLED LISTS NEVER SPLICE. A reader scrolled into the
// recent phases is not shoved down by a phase somebody else's turn finished;
// it waits behind a "new phases" button. The phase the reader WATCHED running
// above is the exception: it lands in place the moment it completes.
//
// Mutation: drop `runningKeys` from `RecentPhases`, and the watched phase is
// held behind the button with the other one.
test("a finished phase is held back under a reader, unless it was running above", async () => {
  const scroller = document.createElement("div");
  scroller.id = SCREEN_SCROLL_ID;
  // A READER WHO STAYS SCROLLED: the router restores a remembered position
  // on mount, which would put a plain writable offset back at the top.
  Object.defineProperty(scroller, "scrollTop", { get: () => TOP_SLACK_PX + 40, set: () => {} });
  scroller.scrollTo = () => {};
  document.body.append(scroller);
  const running = {
    id: "a1",
    agent_id: "id-search",
    role: "Search Engineer",
    handle: "eng-search",
    activity: "working",
    turn: { turn_id: "t-watched", started_at: ago(60_000), stage: "execute" },
    live_call: {
      turn_id: "t-watched",
      phase: "execute",
      iteration: 1,
      model: "scripted",
      trigger: null,
      prompt: "",
      prompt_messages: null,
      response: "",
      input_tokens: 0,
      output_tokens: 0,
      total_tokens: 0,
      tool_executions: null,
      round_num: 0,
      rounds_used: 1,
      in_progress: true,
      started_at: ago(60_000),
      updated_at: ago(1_000),
    },
  };
  const { store } = mount({
    agents: [running],
    answers: {
      phases: { phases: [phaseEvent("p-old", "t-old", "id-search")], next: {}, exhausted: true },
    },
  });
  const card = cardOf("Recent phases");
  await within(card).findByText("delivered");
  expect(within(card).getAllByRole("row").length).toBeGreaterThan(0);
  const rowsBefore = card.querySelectorAll("[data-row-index]").length;

  act(() => {
    store.applyEvent(phaseEvent("p-watched", "t-watched", "id-search"));
    store.applyEvent(phaseEvent("p-other", "t-other", "id-payments"));
  });
  // THE WATCHED ONE IS IN; THE OTHER WAITS.
  await waitFor(() =>
    expect(card.querySelectorAll("[data-row-index]").length).toBe(rowsBefore + 1),
  );
  expect(within(card).getByRole("button", { name: /1 new phase finished/ })).toBeTruthy();
});

// ONE SEAT'S PHASES ARE ASKED FOR BY ITS HANDLE, and the ones finishing on
// the push are matched on its id — never the role name the two engineers share.
//
// Mutation: filter the streamed phases on `role`, and the payments engineer's
// phase is listed under the search engineer.
test("the seat filter asks by handle and matches the push by id", async () => {
  const { store, asked } = mount({
    hash: "#/live?seat=eng-search",
    agents: [
      {
        id: "a1",
        agent_id: "id-search",
        role: "Search Engineer",
        handle: "eng-search",
        activity: "idle",
      },
      {
        id: "a2",
        agent_id: "id-payments",
        role: "Payments Engineer",
        handle: "eng-payments",
        activity: "idle",
      },
    ],
  });
  await waitFor(() => expect(asked.some((q) => q.kind === "phases")).toBe(true));
  const phases = asked.find((q) => q.kind === "phases")!.params;
  expect(phases).toMatchObject({ seat: "eng-search", limit: PHASE_PAGE });
  expect(phases).not.toHaveProperty("role");
  expect(asked.find((q) => q.kind === "event_series")?.params).toMatchObject({
    seat: "eng-search",
    bucket: "minute",
  });

  act(() => {
    store.applyEvent(phaseEvent("p-mine", "t-mine", "id-search", { decision: "mine" }));
    store.applyEvent(phaseEvent("p-twin", "t-twin", "id-payments", { decision: "twin" }));
  });
  const card = cardOf("Recent phases");
  await within(card).findByText("mine");
  expect(within(card).queryByText("twin")).toBeNull();
});

// A NODE MISSING FROM A SEAT'S LATEST EVENTS IS NAMED TOO. The Activity card
// reads two things of the fleet once a seat is chosen — the count and the
// seat's own events — and only the count's coverage was handed to its note,
// so a short list of a seat's events read as a quiet seat.
//
// Mutation: pass only `series.data?.coverage` to the note, and node-b is not
// named.
test("the Activity card names a node missing from the seat's events", async () => {
  mount({
    hash: "#/live?seat=eng-search",
    answers: {
      events: {
        events: [],
        next: null,
        exhausted: true,
        coverage: {
          complete: false,
          nodes: [
            { id: "node-a", answered: true, error: "" },
            { id: "node-b", answered: false, error: "no answer inside the fleet read budget" },
          ],
        },
      },
    },
  });
  const card = cardOf("Activity");
  const note = await within(card).findByText(/is missing one node/);
  expect(note.closest(".coverage-note")?.textContent).toContain("node-b");
});

// OLDER IS THE ENGINE'S CURSOR. It paged the event LIST and read every row
// back one `event` at a time — sixty-one round trips for sixty rows.
test("Load older asks phases again with the cursor the page carried", async () => {
  const { asked } = mount({
    answers: {
      phases: (p: Record<string, unknown>) =>
        p.before_id
          ? { phases: [phaseEvent("p-2", "t-2", "id")], next: {}, exhausted: true }
          : {
              phases: [phaseEvent("p-1", "t-1", "id")],
              next: { before_time: "2026-09-28T10:00:00Z", before_id: "p-1" },
              exhausted: false,
            },
    },
  });
  const card = cardOf("Recent phases");
  const more = await within(card).findByRole("button", { name: `Load ${PHASE_PAGE} older` });
  act(() => more.click());
  await within(card).findByText("That is the beginning of the retained record.");
  const pages = asked.filter((q) => q.kind === "phases");
  expect(pages.at(-1)?.params).toMatchObject({
    before_time: "2026-09-28T10:00:00Z",
    before_id: "p-1",
    limit: PHASE_PAGE,
  });
  expect(asked.some((q) => q.kind === "event")).toBe(false);
});

// THE EVENT LOG IS A LINK TO THE LOG, carrying the seat. It was a button
// navigating to the path this screen was already on for one release, which
// reloaded Now running.
test("Event log links to the log, with the seat when one is chosen", async () => {
  mount({ hash: "#/live?seat=eng-search" });
  const link = within(cardOf("Activity")).getByRole("link", { name: "Event log" });
  expect(link.getAttribute("href")).toBe("#/live/events?seat=eng-search");
});

// THE EMPTY BOX SAYS WHAT IS TRUE. It promised every finished run was "still
// in the record under Runs" — a settled run's row is deleted; its record is
// its turn's trace.
test("an empty In a box says where a finished run's record is", async () => {
  mount();
  const card = cardOf("In a box");
  expect(card.textContent).toContain(
    "No coding run is in flight. A finished run's record is its turn's trace.",
  );
  expect(card.textContent).not.toMatch(/still in the record/);
});

// A RUN PARKED ON A QUESTION IS ANSWERED HERE, and says who it asks.
test("a parked run is a row of Waiting on a person with its audience and Answer", async () => {
  mount({
    answers: {
      sandbox_runs: {
        runs: [
          {
            turn_id: "t-parked",
            agent_handle: "eng-search",
            role: "Engineer",
            status: "awaiting_clarification",
            question: "Drop the polling fallback?",
            audience: "jane",
            audience_handles: ["jane"],
            started_at: ago(3_000_000),
            updated_at: ago(2_000_000),
            paused_at: ago(2_280_000),
            pause_ttl_seconds: 7_200,
            task_description: "fleet map",
            placement: "remote",
            coding_agent: "claude",
            box_exists: true,
          },
        ],
      },
    },
  });
  const card = cardOf("Waiting on a person");
  await within(card).findByText(/parked on a question/);
  expect(card.textContent).toContain("asks Jane Founder");
  expect(card.textContent).toMatch(/held for 1h \d+m more/);
  expect(within(card).getByRole("button", { name: /Answer/ })).toBeTruthy();
  // AND IT IS NOT DRAWN AGAIN AS A BOX: one run, one row.
  expect(cardOf("In a box").textContent).toContain("Nothing is running in a box");
});

// THE STRIP IS THE ENGINE'S COUNT, summed into its cells exactly: a cell's
// value is the sum of the minute bars that start inside it.
test("the strip sums the engine's minute bars into its cells", () => {
  const now = Date.UTC(2026, 8, 28, 12, 0, 30);
  const bar = (min: number, count: number) => ({
    at: new Date(Date.UTC(2026, 8, 28, 11, min)).toISOString(),
    count,
  });
  // SIX-MINUTE CELLS, the width a six-hour window cuts into sixty.
  const cells = stripOf(
    {
      bucket: "minute",
      since: "",
      until: "",
      bars: [bar(54, 2), bar(55, 3), bar(59, 4)],
      total: 9,
      by_category: {},
    },
    now,
    6 * 60_000,
    60,
  );
  expect(cells.reduce((n, c) => n + c.v, 0)).toBe(9);
  // The 11:54 cell holds all three bars; the newest cell is 12:00, the one
  // the clock is in, and holds none.
  expect(cells.at(-2)).toEqual({ t: Date.UTC(2026, 8, 28, 11, 54), v: 9 });
  expect(cells.at(-1)).toEqual({ t: Date.UTC(2026, 8, 28, 12, 0), v: 0 });
});

// A TURN ON NO ITEM SAYS WHAT WOKE IT. The row read "Search Engineer
// Executing" and stopped, which says the seat is busy and nothing about with
// what; the trigger rides every phase frame, so it is always there to say.
//
// Mutation: draw `stateLine` alone and the subject is gone.
test("a running turn on no item names what woke it after the verb", async () => {
  mount({
    agents: [
      {
        agent_id: "a1",
        id: "a1",
        role: "Search Engineer",
        handle: "eng-search",
        activity: "working",
        turn: { turn_id: "t-free", started_at: ago(5_000), stage: "phase" },
        live_call: {
          turn_id: "t-free",
          phase: "execute",
          iteration: 1,
          trigger: { type: "a2a_request", summary: "Drafting the 2.4 launch brief" },
          round_num: -1,
          rounds_used: 0,
          max_rounds: 24,
          in_progress: true,
          started_at: ago(5_000),
          updated_at: ago(1_000),
        },
      },
    ],
  });
  const row = within(cardOf("Running turns")).getByRole("link", { name: /Search Engineer/ });
  expect(row.textContent).toContain("Drafting the 2.4 launch brief");
  // AND THE ROUND IS ONE FROM THE OPENING FRAME, before the model answers.
  expect(row.textContent).toContain("round 1 of 24");
});

// ONE WORKING COUNT, the shell's. The page drew its own "N working" beside the
// header's chip, the same number twice, and on a phone it pushed the chip off
// the page bar.
test("the page draws no working count of its own", async () => {
  mount({
    agents: [
      {
        agent_id: "a1",
        id: "a1",
        role: "Search Engineer",
        handle: "eng-search",
        activity: "working",
        turn: { turn_id: "t-1", started_at: ago(5_000), stage: "context" },
      },
    ],
  });
  await screen.findByRole("heading", { name: "Running turns" });
  expect(screen.queryByText(/^\d+ working$|^Nobody working$/)).toBeNull();
});

// `failed=true`, THE SPELLING TURNS USES: an address carried between the two
// Live screens means the same thing on both, and `failed=1` meant nothing on
// Turns.
test("the failures chip writes and reads failed=true", async () => {
  mount({ hash: "#/live?failed=true" });
  const chip = await screen.findByRole("radio", { name: "failed phases" });
  expect(chip.getAttribute("aria-checked")).toBe("true");
  fireEvent.click(chip);
  await waitFor(() => expect(location.hash).not.toContain("failed="));
  fireEvent.click(chip);
  await waitFor(() => expect(location.hash).toContain("failed=true"));
});
