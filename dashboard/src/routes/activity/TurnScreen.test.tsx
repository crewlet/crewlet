/**
 * The Turn screen as a screen, rather than as three pure functions.
 *
 * Its helpers had tests and its rendering did not, which is exactly backwards
 * for a page whose failures are all claims: this screen states how long a turn
 * took, how it ended and whether anything went wrong, and every one of those
 * is a sentence beside a number rather than a number alone. A caption that
 * says the wrong thing about a right number is the defect this page keeps
 * having, and no test over `turnSpan` or `outcomeOf` can see one.
 *
 * Each case below is a claim the page was making, or refusing to make, that
 * the rows it holds do not support.
 */

import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { TurnScreen } from "./Turn.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { EventRecord, TurnAnswer } from "~/protocol/index.ts";

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
  vi.unstubAllGlobals();
});

const TURN = "t-77";

function event(over: Partial<EventRecord> & { type: string }): EventRecord {
  return {
    id: over.type + "-" + (over.timestamp ?? ""),
    source: "engine",
    actor: "CEO",
    summary: "",
    category: "lifecycle",
    trace_id: "",
    span_id: "",
    parent_span_id: "",
    topic: "",
    timestamp: "2026-09-13T10:00:00Z",
    ...over,
  } as EventRecord;
}

/** A finished execute phase that took `durationMs`, as the engine publishes it. */
function phase(at: string, durationMs: number, over: Record<string, unknown> = {}): EventRecord {
  return event({
    type: "agent_phase_completed",
    timestamp: at,
    payload: {
      turn_id: TURN,
      phase: "execute",
      iteration: 1,
      role: "CEO",
      model: "claude-sonnet-5",
      total_tokens: 1200,
      rounds_used: 2,
      duration_ms: durationMs,
      ...over,
    },
  });
}

/** Mount the screen over one stubbed answer. */
function mount(answer: Partial<TurnAnswer>) {
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what: string) =>
    what === "turn"
      ? Promise.resolve({ turn_id: TURN, events: [], truncated: false, ...answer })
      : Promise.resolve({});
  return render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <TurnScreen turnId={TURN} />
      </Router>
    </ClientContext.Provider>,
  );
}

// THE DURATION THE ENGINE MEASURED REACHES THE CARD.
//
// It used to be reconstructed by pairing the completed record with the
// `agent_phase_started` that shares its key — and on this screen's own reason
// for existing, a turn deep-linked while it runs, neither half of that pairing
// is in the browser. Asserted through the render because the plumbing is four
// hops long (payload → fromPhaseEvent → phaseDuration → PhaseCard) and each
// hop was capable of dropping it silently.
test("a phase card shows the duration off the record itself", async () => {
  mount({ events: [phase("2026-09-13T10:01:30Z", 90_000)] });
  expect(await screen.findByTitle("how long this phase took")).toHaveProperty(
    "textContent",
    "1m 30s",
  );
});

// A CUT VIEW NAMES THE GAP, IN THE HEADER AND ABOVE THE PANELS.
//
// `EventLog.Turn` orders oldest first and stops at the store's per-turn cap,
// so a head-only read lost the turn's ENDING — the two records this header
// reads its outcome, its wall clock and its plan summary off — and a cut turn
// was indistinguishable from one that never finished. The answer recovers the
// ending beside the opening now, so what is missing is the MIDDLE, and the
// page says that rather than warning it cannot answer its own question.
test("a cut view names the middle as the gap, not the ending", async () => {
  mount({ events: [phase("2026-09-13T10:01:30Z", 90_000)], truncated: true });
  expect(await screen.findByText(/middle not shown/)).toBeTruthy();
  expect(screen.getByText(/what is missing is the middle/)).toBeTruthy();
  // Neither note may state what it used to: "no turn record" is a claim about
  // the TURN, and "spanning the turn's first and last event" is a claim about
  // a window this page can no longer assume is whole. Both are `Fact.note`
  // now rather than a tile's caption, and the assertion is unchanged by that
  // on purpose — what must not appear is the sentence, wherever it is drawn.
  expect(screen.queryByText("no turn record")).toBeNull();
  expect(screen.queryByText("spanning the turn's first and last event")).toBeNull();
});

// AND THE RECOVERED ENDING IS READ, so a cut turn reports how it ended.
//
// This is the whole reason the answer does a second seek: the outcome is the
// headline of this page, and a head-only read put an em dash where it goes on
// exactly the long turns worth opening.
test("a cut view still reports the outcome, off the recovered record", async () => {
  mount({
    truncated: true,
    events: [
      phase("2026-09-13T10:01:30Z", 90_000),
      event({
        type: "turn_completed",
        timestamp: "2026-09-13T10:40:00Z",
        payload: { turn_id: TURN, review_outcome: "done", outcome: "delivered" },
      }),
    ],
  });
  // BOTH FRAMES REPORT IT, because both render `turnFacts`: the word is the
  // fact's value and WHOSE word it is — the reviewer's verdict or the
  // executor's own — is its note. This case is about the note: a cut view
  // recovers the turn's ending from the record rather than from the rows it
  // holds, and `delivered the work` is that recovery being read. It used to
  // reach only the page, through a tile the rail had no room for.
  expect((await screen.findAllByText("done")).length).toBeGreaterThan(0);
  expect(screen.getByText("delivered the work")).toBeTruthy();
});

// AND IT DOES NOT CLAIM THE TURN WAS CLEAN. "Nothing went wrong" is a claim
// over every row of the turn, and a cut view has rows it never saw — any of
// which could be the guard breach that ended it.
test("a cut view withholds the clean badge, and a complete one gives it", async () => {
  const complete = [
    phase("2026-09-13T10:01:30Z", 90_000),
    event({
      type: "agent_turn_completed",
      timestamp: "2026-09-13T10:01:31Z",
      payload: { turn_id: TURN, failed: false, decision: "done" },
    }),
  ];
  // THE CONTROL FIRST, or the absence below passes on a page that never
  // renders the badge at all.
  mount({ events: complete });
  expect(await screen.findByText("nothing went wrong")).toBeTruthy();

  cleanup();
  mount({ events: complete, truncated: true });
  await screen.findByText(/middle not shown/);
  expect(screen.queryByText("nothing went wrong")).toBeNull();
});

// prompt.size REACHES THE PAGE.
//
// Six small integers per phase were banded into `given` and read by nobody:
// the screen took `prefetch_summary` out of that band and dropped the rest,
// so the only route to a phase's prompt size was the raw payload of a row in
// the residual list.
test("each phase's prompt size is rendered rather than banded and dropped", async () => {
  mount({
    events: [
      phase("2026-09-13T10:01:30Z", 90_000),
      event({
        type: "prompt.size",
        timestamp: "2026-09-13T10:00:01Z",
        payload: {
          turn_id: TURN,
          phase: "execute",
          iteration: 1,
          approximate_tokens: 7400,
          system_chars: 24000,
          user_chars: 1200,
        },
      }),
    ],
  });
  expect(await screen.findByText("Prompt sent")).toBeTruthy();
  expect(screen.getByTitle("the engine's own approximation").textContent).toBe("7,400");
  expect(screen.getByTitle("characters in the system prompt")).toBeTruthy();
});

// A TURN THAT ANSWERED WITH NOTHING IS AN EMPTY STATE, not a blank page — and
// the empty state must not fire on a turn whose phases are arriving on the
// stream instead, which is the deep-link-while-running case.
test("a turn with no events at all says so", async () => {
  mount({ events: [] });
  expect(await screen.findByText("No events for this turn")).toBeTruthy();
});

// …AND DOES NOT FIRE WHEN THE PHASES ARE ARRIVING ON THE STREAM.
//
// The other half of the same condition, and the half the case above cannot
// see: the empty state is guarded on the store's rows as well as the query's,
// so a turn deep-linked WHILE IT RUNS — the one this screen exists for — must
// show the phase it is on rather than "no events for this turn". Without this
// case, deleting the store term from the guard leaves the whole route suite
// green.
test("a running turn's streamed phases keep the empty state away", async () => {
  const store = new Store();
  const socket = new LiveSocket(store);
  // The QUERY comes back empty — the store's write has not reached the event
  // log yet, which is exactly the deep-link-while-running race.
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what: string) =>
    what === "turn"
      ? Promise.resolve({ turn_id: TURN, events: [], truncated: false })
      : Promise.resolve({});
  // `failed` is optional on a query row and required on a streamed envelope;
  // this phase did not fail.
  store.applyEvent({ ...phase("2026-09-13T10:01:30Z", 90_000), failed: false });

  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <TurnScreen turnId={TURN} />
      </Router>
    </ClientContext.Provider>,
  );

  expect(await screen.findByTitle("how long this phase took")).toBeTruthy();
  expect(screen.queryByText("No events for this turn")).toBeNull();
});

// THE TRACE BUTTONS NAME THIS TURN'S TRACES, AND NOBODY ELSE'S.
//
// `usePhaseEvents` is the store's GLOBAL phase slice — every seat's completed
// phase, every turn's, two hundred deep — and the header folded it whole, so a
// tab left open on a busy company offered "Trace 2 of 4" buttons leading into
// other turns. This case is the worst shape of it: the deep-link-while-running
// turn, where the query answers with nothing at all and the ONE trace button
// therefore came entirely from whichever foreign phase landed most recently.
test("the trace buttons name this turn's traces, not the tab's", async () => {
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what: string) =>
    what === "turn"
      ? Promise.resolve({ turn_id: TURN, events: [], truncated: false })
      : Promise.resolve({});
  // Another seat's turn, landing in the same tab a moment before this one's.
  store.applyEvent({
    ...phase("2026-09-13T10:00:00Z", 1_000, { turn_id: "t-elsewhere", role: "CFO" }),
    failed: false,
    trace_id: "trace-elsewhere",
  });
  store.applyEvent({
    ...phase("2026-09-13T10:01:30Z", 90_000),
    failed: false,
    trace_id: "trace-mine",
  });

  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <TurnScreen turnId={TURN} />
      </Router>
    </ClientContext.Provider>,
  );

  // ONE trace, not two: the plural form is what a reader is offered when a
  // turn was genuinely resumed elsewhere, and this turn was not.
  const button = await screen.findByRole("button", { name: "Trace" });
  expect(screen.queryByText(/Trace 1 of/)).toBeNull();
  button.click();
  expect(location.hash).toContain("trace-mine");
  expect(location.hash).not.toContain("trace-elsewhere");
});

// THE DOWNLOADED FILE SAYS WHAT THE SCREEN SAYS.
//
// The page marks a capped turn with a badge and a banner because its opening
// and ending without its middle is indistinguishable from a turn that died
// early. The export carried the same rows with no such marker, so a reader who
// attached `turn-<id>.json` to a bug report handed on the exact ambiguity this
// read exists to remove — and whoever opened it could not tell an incomplete
// turn from a complete one.
test("the exported turn carries the truncation flag", async () => {
  mount({ events: [phase("2026-09-13T10:01:30Z", 90_000)], truncated: true });
  const copy = await screen.findByTitle(
    "the whole turn as JSON — its record, its phases and everything else it published",
  );

  let written = "";
  Object.defineProperty(navigator, "clipboard", {
    configurable: true,
    value: { writeText: (t: string) => ((written = t), Promise.resolve()) },
  });
  copy.click();

  await waitFor(() => expect(written).not.toBe(""));
  expect(JSON.parse(written)).toHaveProperty("truncated", true);
});

// THE STORE'S OWN TRACE LIST IS NOT A DERIVATION, and the page prefers it.
//
// A list folded from the rows this page holds can only name the traces of rows
// it HOLDS: a cut view is missing its middle and a turn past the retention
// window is missing most of itself, so a trace that lived only in the gap is
// one the fold can never reach. `internal/api/queries/insight.go` seeks them
// separately for exactly that reason.
test("a trace the rows never carried still reaches the header", async () => {
  mount({
    trace_ids: ["only-in-the-gap", "also-on-a-row"],
    events: [
      phase("2026-09-13T10:01:30Z", 90_000, {}),
      event({
        type: "turn_completed",
        timestamp: "2026-09-13T10:40:00Z",
        trace_id: "also-on-a-row",
      }),
    ],
  });
  // Two traces, so the header draws the numbered list rather than one button.
  expect(await screen.findByText("Trace 1 of 2")).toBeTruthy();
  expect(screen.getByText("Trace 2 of 2")).toBeTruthy();
});

// AND AN EMPTY ANSWER IS NOT "THIS TURN TOUCHED NONE". The seek degrades to an
// empty list rather than failing the read, so an absent value has to fall
// through to the rows rather than blanking a trace the page can see.
test("an empty trace list falls through to the rows", async () => {
  mount({
    trace_ids: [],
    events: [
      event({ type: "turn_completed", timestamp: "2026-09-13T10:40:00Z", trace_id: "from-a-row" }),
    ],
  });
  // One trace, so the header draws its single unnumbered button — which it
  // could only have got from the row.
  expect(await screen.findByRole("button", { name: "Trace" })).toBeTruthy();
});

// THE TITLE IS A LEAD, SO THE PANEL STILL PRINTS THE WHOLE TRIGGER.
//
// `TurnBrief` drops its prose where the title already said it, and that
// comparison is exact: `woke === omit`. While the title was the WHOLE string
// it matched whenever the two came from the same place, and the paragraph
// went. Now that `turnTitle` takes only the LEAD sentence, it matches only a
// trigger that IS one sentence — so a longer one prints here in full, with the
// header's own sentence at the front of it.
//
// That is the answer rather than an oversight, and these three cases are what
// pin it. Printing only the tail would open the paragraph mid-thought; dropping
// it would lose everything the lead did not take, which on a turn with no plan
// summary is the only account of itself this screen has. What is still dropped
// is the case the rule was written for — a trigger the title says ALL of.
const ONE_SENTENCE = "Founder asked for the quarterly numbers.";
const MANY = `${ONE_SENTENCE} They want revenue by unit and the headcount behind it.`;

/** The prose paragraphs of the "Woken by" panel — the only ones on this page. */
function briefProse(): string[] {
  return [...document.querySelectorAll("p.t-body.measure")].map((p) => p.textContent ?? "");
}

/** One phase carrying a trigger, and no turn record — so the title is the trigger's. */
function woken(trigger: Record<string, unknown>) {
  return { events: [phase("2026-09-13T10:01:30Z", 90_000, { trigger })] };
}

test("a multi-sentence trigger prints whole, under the title's lead", async () => {
  mount(woken({ id: "e-9", type: "chat_message", summary: MANY }));
  await screen.findByText("Woken by");
  // The header took the lead, and only the lead.
  expect(document.querySelector(".object-title")?.textContent).toBe(ONE_SENTENCE);
  // And the panel carries the whole thing, that lead included.
  expect(briefProse()).toEqual([MANY]);
});

test("a one-sentence trigger the title says whole is not printed twice", async () => {
  mount(woken({ id: "e-9", type: "chat_message", summary: ONE_SENTENCE }));
  // The panel is still drawn, because it carries the link to the trigger
  // event. What goes is the prose, which is the half the header already is.
  expect(await screen.findByText("Woken by")).toBeTruthy();
  expect(screen.getByRole("link", { name: "the trigger →" })).toBeTruthy();
  expect(document.querySelector(".object-title")?.textContent).toBe(ONE_SENTENCE);
  expect(briefProse()).toEqual([]);
});

test("…and with nothing else to carry, the panel goes with it", async () => {
  mount(woken({ type: "chat_message", summary: ONE_SENTENCE }));
  // The phase card is what says this screen rendered at all.
  await screen.findByTitle("how long this phase took");
  expect(screen.queryByText("Woken by")).toBeNull();
});

/**
 * THE SCREEN SAYS WHICH ATTEMPT IT IS SHOWING, and links the others.
 *
 * A turn id names one RUN (`adr/0017`), so a trigger that failed without
 * reaching outside the engine and was redelivered is several turns — and this
 * page is where every deep link in the product lands. Arriving on the failed
 * attempt with nothing saying a later one succeeded is how a reader concludes
 * the work never happened; arriving on the second with nothing saying it is a
 * retry is how they conclude the company did it twice.
 */
test("a re-run says which attempt it is and links the one before it", async () => {
  mount({
    turn_id: TURN,
    work_key: "wk-1",
    attempts: [
      { turn_id: "run-1", failed: true } as TurnAnswer["attempts"] extends (infer R)[] ? R : never,
      { turn_id: TURN, failed: false } as TurnAnswer["attempts"] extends (infer R)[] ? R : never,
    ],
    events: [phase("2026-09-13T10:00:02Z", 1000)],
  });
  expect(await screen.findByText("attempt 2/2")).toBeTruthy();
  // The one before it is REACHABLE, not merely announced — and says it
  // failed, so the pair reads as the story it is.
  expect(await screen.findByText(/Attempt 1 \(failed\)/)).toBeTruthy();
});

/** A turn that ran once claims nothing: tagging it "attempt 1/1" would put a
 *  re-run marker on every ordinary turn in the company. */
test("a turn that ran once carries no attempt badge", async () => {
  mount({
    turn_id: TURN,
    work_key: "wk-1",
    attempts: [
      { turn_id: TURN, failed: false } as TurnAnswer["attempts"] extends (infer R)[] ? R : never,
    ],
    events: [phase("2026-09-13T10:00:02Z", 1000)],
  });
  await screen.findByText("1 phases");
  expect(screen.queryByText(/attempt \d+\/\d+/)).toBeNull();
});
