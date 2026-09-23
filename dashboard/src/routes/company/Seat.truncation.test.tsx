/**
 * The four per-seat panels whose count is a PAGE SIZE, not a total.
 *
 * `agent_memory` answers the newest `MemoryPageLimit` rows of a seat's diary,
 * its episodes and its counterparty profiles, and `conversations` answers one
 * page of its thread roster. Every one of those reads takes ONE ROW PAST the
 * page as evidence and states `..._truncated` on the answer — and every one of
 * those flags was read by nothing, so a seat that had written four thousand
 * diary notes and a seat that had written fifty drew the same chip, under a
 * heading whose whole question is what this seat remembers.
 *
 * ASSERTED IN BOTH DIRECTIONS, which is the half a marker usually loses: a
 * `+` that always drew, or a note that always drew, is the same defect facing
 * the other way — it would put "there are more" on every complete answer in
 * the product, which is how the one case that matters becomes a word nobody
 * reads.
 *
 * AND A PAGE IS NOT THE ONLY AXIS A READ IS CUT ON. The Turns table is bounded
 * twice — by its page and by a TIME WINDOW — and the second one has no chip
 * and no note to carry it, because the probe row is evidence about the page
 * alone. The only honest place to put it is the ASK, so the window is asserted
 * there.
 */

import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { SeatPeek, SeatScreen } from "./Seat.tsx";
import { TURN_MAX_DAYS, TURN_MAX_RANGE } from "~/routes/activity/Turns.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, MAX_PHASES, Store } from "~/protocol/index.ts";
import type { EventEnvelope } from "~/protocol/index.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

/** The engine's page, as `queries.MemoryPageLimit` sets it. */
const PAGE = 50;

const rows = <T,>(make: (i: number) => T) => Array.from({ length: PAGE }, (_, i) => make(i));

const at = "2026-03-01T09:00:00Z";

const memory = (truncated: boolean) => ({
  id: "ceo",
  diary: rows((i) => ({ id: `d-${i}`, content: `note ${i}`, created_at: at, scope: "note" })),
  episodes: rows((i) => ({
    id: `e-${i}`,
    turn_id: `t-${i}`,
    created_at: at,
    task_summary: `did ${i}`,
    outcome: "delivered",
  })),
  skills: [],
  skills_total: 0,
  counterparties: rows((i) => ({
    subject: { handle: `mate-${i}`, name: `Mate ${i}` },
    resolved: true,
    traits: {},
    interactions: 1,
    first_seen_at: at,
    last_updated_at: at,
    last_corroborated_at: at,
  })),
  onboarded_at: "",
  diary_truncated: truncated,
  episodes_truncated: truncated,
  counterparties_truncated: truncated,
});

const conversations = (truncated: boolean) => ({
  handle: "ceo",
  available: true,
  conversations: rows((i) => ({ key: `slack:C0/${i}`, turns: 1, last_at: at })),
  entries: [],
  truncated,
});

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

function mount(tab: string, truncated: boolean) {
  const store = new Store();
  store.applyOrg({ name: "Acme", roles: [{ name: "CEO", handle: "ceo" }] });
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    Promise.resolve(
      what === "agent_memory"
        ? memory(truncated)
        : what === "conversations"
          ? conversations(truncated)
          : { llm_history: [], next: "" },
    );
  location.hash = `#/company/people/ceo?tab=${tab}`;
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <SeatScreen handle="ceo" />
      </Router>
    </ClientContext.Provider>,
  );
}

/**
 * The chip on the header row a card title sits on.
 *
 * FOUND THROUGH THE TITLE ELEMENT rather than through the text, which the
 * knowledge screen's own suite already does for the same reason: this screen's
 * tab strip carries a "Turns" control, so a text search matches the tab as well
 * as the card it opens and the query fails with two matches rather than
 * reading the wrong one — which is the better failure and still not the test.
 */
function count(title: string): string | undefined {
  const header = [...document.querySelectorAll(".crewlet-card__header")].find(
    (h) => h.querySelector(".crewlet-card__title")?.textContent === title,
  );
  expect(header, `no card header carries the title ${title}`).toBeTruthy();
  return (header as HTMLElement).querySelector(".crewlet-count")?.textContent ?? undefined;
}

test("a capped memory page says the count is a floor, and what is behind it", async () => {
  mount("memory", true);
  await waitFor(() => expect(count("Private diary")).toBe(`${PAGE}+`));
  expect(count("Past turns")).toBe(`${PAGE}+`);
  expect(count("Who it has worked with")).toBe(`${PAGE}+`);
  // AND THE SENTENCE UNDER EACH, naming the slice and where the rest is. The
  // three reads come back newest-first, so "the newest" is the engine's own
  // ordering rather than a guess about it.
  expect(screen.getByText(/The newest 50 notes; there are more\./)).toBeTruthy();
  expect(screen.getByText(/The newest 50 turns; there are more\./)).toBeTruthy();
  expect(screen.getByText(/The newest 50 colleagues; there are more\./)).toBeTruthy();
  expect(screen.getByText(/agent_diary table/)).toBeTruthy();
});

test("a seat holding exactly the page reports itself whole", async () => {
  // THE CONTROL, and the reason the flag is read rather than inferred from
  // `diary.length >= 50`: this seat holds fifty notes and fifty is all of
  // them, so a `+` here would be a caution about missing data stated over a
  // complete answer.
  mount("memory", false);
  await waitFor(() => expect(count("Private diary")).toBe(`${PAGE}`));
  expect(count("Past turns")).toBe(`${PAGE}`);
  expect(count("Who it has worked with")).toBe(`${PAGE}`);
  expect(screen.queryByText(/there are more/)).toBeNull();
});

test("a capped thread roster says so too", async () => {
  mount("threads", true);
  await waitFor(() => expect(count("Threads")).toBe(`${PAGE}+`));
  expect(screen.getByText(/The newest 50 threads; there are more\./)).toBeTruthy();
});

test("a whole thread roster draws no caution", async () => {
  mount("threads", false);
  await waitFor(() => expect(count("Threads")).toBe(`${PAGE}`));
  expect(screen.queryByText(/there are more/)).toBeNull();
});

// ---------------------------------------------------------------------------
// The two panels whose page was NOT marked, and the one that stated the
// opposite in prose
// ---------------------------------------------------------------------------

/**
 * The same seat, with the answers named per question and every ask recorded.
 *
 * THE PARAMETERS ARE PART OF WHAT IS UNDER TEST here, which the roster above
 * does not need: two of these panels were fixed by CHANGING WHAT IS ASKED FOR —
 * one row past the page on `turns`, the read's whole ceiling on `conversations`
 * — and a screen that draws the right marker over the wrong ask is the defect
 * with a marker painted on it.
 */
function mountAsking(tab: string, answers: Record<string, unknown>) {
  const store = new Store();
  store.applyOrg({ name: "Acme", roles: [{ name: "CEO", handle: "ceo" }] });
  const socket = new LiveSocket(store);
  const asked: { what: string; params?: Record<string, unknown> }[] = [];
  (
    socket as unknown as {
      query: (what: string, params?: Record<string, unknown>) => Promise<unknown>;
    }
  ).query = (what, params) => {
    asked.push({ what, params });
    return Promise.resolve(answers[what] ?? { llm_history: [], next: "" });
  };
  location.hash = `#/company/people/ceo?tab=${tab}`;
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <SeatScreen handle="ceo" />
      </Router>
    </ClientContext.Provider>,
  );
  return asked;
}

const task = (i: number) => ({
  id: `task-${i}`,
  key: `ENG-${i}`,
  title: `Task ${i}`,
  type: "task",
  status: "in_progress",
  status_group: "active",
  priority: "normal",
  assignee: "ceo",
  project: "ENG",
  updated_at: at,
  version: 1,
});

const turn = (i: number) => ({
  turn_id: `turn-${i}`,
  role: "CEO",
  started_at: at,
  summary: `did ${i}`,
  iterations: 1,
  total_tokens: 10,
  duration_ms: 1000,
  complete: true,
  failed: false,
});

test("the work card's chip is a floor when the tracker minted a cursor", async () => {
  mountAsking("work", {
    work_items: {
      items: rows(task),
      // ASKED, NOT INFERRED — `readTasksJoined` reads one row past the limit
      // and mints this only when the extra one came back.
      next_cursor: "after-50",
      total_hint: 213,
      complete: true,
    },
  });
  await waitFor(() => expect(count("Assigned and open")).toBe(`${PAGE}+`));
  // THE ENGINE'S OWN COUNT OF THE WHOLE SET, which is the question this card
  // asks and the one no page length can answer — in the tracker's own wording,
  // which `#/work` already spells over the same read.
  expect(screen.getByText(/50 tasks of 213 matching/)).toBeTruthy();
  // AND THE SLICE IS THE READ'S OWN ORDER — `-priority,updated` — which is
  // neither "newest" nor "in title order".
  expect(screen.getByText(/The first 50 tasks by priority; there are more\./)).toBeTruthy();
  const link = screen.getByRole("link", { name: /Their work on the tracker/ });
  expect(link.getAttribute("href")).toContain("assignee=ceo");
  expect(link.getAttribute("href")).toContain("scope=open");
});

test("a seat whose open work all fits states one number and no caution", async () => {
  // THE CONTROL. The tracker minted no cursor, so this is the whole set — and
  // `totalHint` stays silent when the hint equals what is drawn, rather than
  // printing "12 of 12 matching".
  mountAsking("work", {
    work_items: { items: [task(1), task(2)], total_hint: 2, complete: true },
  });
  await waitFor(() => expect(count("Assigned and open")).toBe("2"));
  expect(screen.queryByText(/there are more/)).toBeNull();
  // NO SUBTITLE AT ALL, rather than one whose second half went quiet: "2 tasks"
  // under a chip already reading 2 is the row count said twice, which is the
  // shape `totalHint` returns "" for.
  expect(screen.queryByText(/matching/)).toBeNull();
  expect(screen.queryByText("2 tasks")).toBeNull();
  expect(screen.queryByRole("link", { name: /Their work on the tracker/ })).toBeNull();
});

test("the turns table asks for one row past its page and marks the page", async () => {
  const asked = mountAsking("turns", {
    // FIFTY-ONE ROWS: the fifty this table draws and the probe that says a
    // fifty-first turn exists. The answer's own `next` is deliberately absent
    // from this fixture — `queries.turns` mints it whenever any row came back,
    // so a screen reading it would mark every complete list.
    turns: { turns: Array.from({ length: PAGE + 1 }, (_, i) => turn(i)), next: null },
  });
  await waitFor(() => expect(count("Turns")).toBe(`${PAGE}+`));
  const ask = asked.find((a) => a.what === "turns")?.params;
  expect(ask?.limit).toBe(PAGE + 1);
  // THE WINDOW IS NAMED TOO, and it is the half no marker on this card can
  // state. Sending no `days` is not "everything the store holds": `store.Turns`
  // falls back to `store.DefaultTurnDays`, a week. A seat with twenty turns in
  // the last week and five hundred more in the three weeks the event store
  // still answers for therefore drew a plain `20`, a subtitle about the event
  // store, and no note and no link at all — the probe row is evidence about
  // the PAGE and cannot see a cut on the time axis.
  expect(ask?.days).toBe(TURN_MAX_DAYS);
  // AND THE VALUE IS THE ENGINE'S OWN CEILING: `store.MaxTurnDays`, which is
  // `store.EventHistory` in days, so the read reaches the point past which the
  // event log holds nothing and the time axis cuts nothing.
  expect(TURN_MAX_DAYS).toBe(30);
  // THE PROBE IS EVIDENCE, NOT A ROW: fifty are drawn, not fifty-one.
  expect(screen.queryByText("did 50")).toBeNull();
  expect(screen.getByText(/The newest 50 turns; there are more\./)).toBeTruthy();
  // AND THE PROSE NO LONGER CLAIMS COMPLETENESS OVER A CAPPED READ.
  expect(screen.queryByText(/Every turn the event store holds/)).toBeNull();
  const link = screen.getByRole("link", { name: /Older turns, by window/ });
  expect(link.getAttribute("href")).toContain("role=CEO");
  // AND IT OPENS WHERE THIS PAGE ENDS. `#/activity/turns` falls back to `7d`,
  // so a link offered under a thirty-day table would otherwise answer "older
  // turns" with a shorter horizon than the reader already had.
  expect(link.getAttribute("href")).toContain(`window=${TURN_MAX_RANGE}`);
});

test("a seat whose turns all fit is counted plainly", async () => {
  // THE CONTROL, and the exact boundary the probe row exists for: the engine
  // holds fifty turns, the read asked for fifty-one, and fifty came back — so
  // this is every turn it has and the chip must not carry a `+`.
  mountAsking("turns", {
    turns: { turns: Array.from({ length: PAGE }, (_, i) => turn(i)), next: null },
  });
  await waitFor(() => expect(count("Turns")).toBe(`${PAGE}`));
  expect(screen.queryByText(/there are more/)).toBeNull();
  expect(screen.queryByRole("link", { name: /Older turns, by window/ })).toBeNull();
});

test("the thread roster asks for the read's ceiling rather than its default", async () => {
  // `queries.MaxConversationPage`. The note under this list names a SQL table
  // as where the rest is, and that is only honest once the parameter that
  // reaches three quarters of it has actually been sent.
  const asked = mountAsking("threads", { conversations: conversations(true) });
  await waitFor(() => expect(count("Threads")).toBe(`${PAGE}+`));
  expect(asked.some((a) => a.what === "conversations" && a.params?.limit === 200)).toBe(true);
});

// ---------------------------------------------------------------------------
// The rail's last turn, which no query stands behind
// ---------------------------------------------------------------------------

/** A completed phase as the socket streams it, payload and all. */
const streamedPhase = (id: string, role: string): EventEnvelope =>
  ({
    id,
    type: "agent_phase_completed",
    category: "llm",
    source: "engine",
    actor: role,
    summary: "",
    trace_id: "",
    span_id: "",
    parent_span_id: "",
    topic: "",
    timestamp: at,
    failed: false,
    payload: { turn_id: `turn-${id}`, phase: "execute", iteration: 1, role },
  }) as EventEnvelope;

function mountPeek(store: Store) {
  store.applyOrg({ name: "Acme", roles: [{ name: "CEO", handle: "ceo" }] });
  const socket = new LiveSocket(store);
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <SeatPeek handle="ceo" />
      </Router>
    </ClientContext.Provider>,
  );
}

test("a rail whose seat's phases the tab dropped does not say nothing streamed", () => {
  // The seat's phase streamed in and was then pushed out by other seats'
  // work. "Nothing has streamed to this tab yet" over that is false.
  const store = new Store();
  store.applyEvent(streamedPhase("mine", "CEO"));
  for (let i = 0; i < MAX_PHASES; i++) store.applyEvent(streamedPhase(`other-${i}`, "CFO"));
  mountPeek(store);
  expect(screen.queryByText(/Nothing has streamed to this tab yet/)).toBeNull();
  expect(screen.getByText(/Nothing this tab still holds/)).toBeTruthy();
  // AND WHERE THE RECORD IS, as a link to the tab that draws it.
  expect(screen.getByRole("link", { name: "Turns tab" }).getAttribute("href")).toContain(
    "tab=turns",
  );
});

test("a rail whose tab has dropped nothing says so plainly", () => {
  // THE CONTROL: no drop, no caution.
  mountPeek(new Store());
  expect(screen.getByText(/Nothing has streamed to this tab yet/)).toBeTruthy();
  expect(screen.queryByText(/dropped older ones/)).toBeNull();
});

// ---------------------------------------------------------------------------
// The four blocks of their own queue
// ---------------------------------------------------------------------------

/** One `work_my_work` answer, each block at the engine's `tracker.MyWorkRows`. */
const MY_WORK_ROWS = 20;
const myWork = (truncated: boolean) => {
  const cut = { priorities: truncated, collaborating: truncated, asked_of_me: truncated };
  const block = (prefix: string) =>
    Array.from({ length: MY_WORK_ROWS }, (_, i) => ({ ...task(i), id: `${prefix}-${i}` }));
  return {
    handle: "ceo",
    priorities: block("p"),
    assigned: [],
    asked_of_me: Array.from({ length: MY_WORK_ROWS }, (_, i) => ({
      ...task(i),
      comment: `c-${i}`,
      asked_by: "cfo",
      asked_at: at,
      body: `question ${i}`,
    })),
    checklist_items: [],
    collaborating: block("c"),
    watching_recent: [],
    unblocked_recent: [],
    truncated: cut,
    complete: true,
  };
};

test("their own queue's blocks read the answer's truncation flags", async () => {
  mountAsking("work", {
    viewer: { operator_id: "op", operator: true, handle: "", name: "", kind: "" },
    work_items: { items: [], complete: true },
    work_my_work: myWork(true),
  });
  // A FLOOR, NOT A TOTAL: the engine cut each block at twenty and said so.
  await waitFor(() => expect(count("What they mean to do first")).toBe(`${MY_WORK_ROWS}+`));
  expect(count("Collaborating")).toBe(`${MY_WORK_ROWS}+`);
  expect(count("Asked of them")).toBe(`${MY_WORK_ROWS}+`);
  // AND EACH NAMES ITS OWN ORDER — a priority list is not "the newest" of
  // anything — and where the rest of it is.
  expect(
    screen.getByText(/The first 20 tasks in the list's own order; there are more\./),
  ).toBeTruthy();
  expect(screen.getByText(/The 20 tasks updated most recently; there are more\./)).toBeTruthy();
  expect(screen.getByText(/The newest 20 questions; there are more\./)).toBeTruthy();
  expect(screen.getByText("GET /work?container=workspace&priorities=ceo")).toBeTruthy();
  expect(screen.getByText("GET /work?container=workspace&collaborator=ceo")).toBeTruthy();
});

test("their own queue's blocks, whole, draw plain counts and no caution", async () => {
  mountAsking("work", {
    viewer: { operator_id: "op", operator: true, handle: "", name: "", kind: "" },
    work_items: { items: [], complete: true },
    work_my_work: myWork(false),
  });
  await waitFor(() => expect(count("What they mean to do first")).toBe(`${MY_WORK_ROWS}`));
  expect(count("Collaborating")).toBe(`${MY_WORK_ROWS}`);
  expect(screen.queryByText(/there are more/)).toBeNull();
});

// "ITS EVENTS" IS THE EVENT LOG, filtered to the seat. It navigated to the bare
// `#/activity`, which is Live now and reads no `actor` at all — so the button
// dropped the reader on a screen about the whole company.
test("a seat's events open the event log narrowed to it", async () => {
  mountAsking("overview", {});
  const button = await screen.findByRole("button", { name: "Its events" });
  button.click();
  await waitFor(() => expect(location.hash).toContain("#/activity/events?"));
  expect(location.hash).toContain("actor=CEO");
});
