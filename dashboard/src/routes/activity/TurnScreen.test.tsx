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

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { TurnScreen } from "./Turn.tsx";
import { ABSORBED } from "~/contract/turnbands.ts";
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

/**
 * One `prompt.size` measurement for this turn's executor, at iteration 1.
 *
 * Each call gets its own instant, because a re-run turn publishes several of
 * these under ONE phase key and two events sharing an id is a shape the store
 * never produces.
 */
let measured = 0;
function promptSize(payload: Record<string, unknown>): EventRecord {
  measured++;
  return event({
    type: "prompt.size",
    timestamp: `2026-09-13T10:00:${String(measured).padStart(2, "0")}Z`,
    payload: { turn_id: TURN, phase: "execute", iteration: 1, ...payload },
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

// prompt.size REACHES THE PAGE, EVERY TERM OF IT.
//
// A whole row of integers per phase was banded into `given` and read by
// nobody: the screen took `prefetch_summary` out of that band and dropped the
// rest, so the only route to a phase's prompt size was the raw payload of a
// row in the residual list. The tool-definition array is the term the ENGINE
// then turned out not to be measuring either — the largest one — so a column
// that renders a permanent 0 is the failure this case exists to catch, which
// is why the figures asserted are ones only a measuring engine produces.
test("each phase's prompt size is rendered rather than banded and dropped", async () => {
  mount({
    events: [
      phase("2026-09-13T10:01:30Z", 90_000),
      promptSize({
        approximate_tokens: 7400,
        system_chars: 24000,
        user_chars: 1200,
        message_chars: 0,
        tool_chars: 3800,
        tool_count: 11,
      }),
    ],
  });
  expect(await screen.findByText("Prompt sent")).toBeTruthy();
  expect(screen.getByTitle("the engine's own approximation").textContent).toBe("7,400");
  expect(screen.getByTitle("bytes in the system prompt")).toBeTruthy();
  // THE TOOL ARRAY IS ON THE PAGE, which is the term this panel was blind to
  // and the dominant one: a measured turn read ~6,900 tokens here against the
  // provider's 205,000.
  expect(screen.getByTitle("11 tool definitions, as compact JSON").textContent).toBe("3.7 KB");
  expect(screen.getByTitle("bytes of conversation a resumed phase re-entered")).toBeTruthy();
});

// A SUSPENDED PHASE DRAWS BOTH OF ITS PROMPTS.
//
// The panel used to collapse a repeated `phase|iteration` into one row with an
// `×N` chip, reading a repeat as the turn's dispatch having been re-delivered.
// A redelivery mints a new run id (`adr/0017`) and the turn query is
// `WHERE turn_id = ?`, so the attempts never share a page — the repeat is a
// SUSPEND. `internal/agent/runner/resume.go` re-enters the parked phase at the
// parked iteration, and the two measurements are two prompts: the opening
// system and user text, then the conversation the resume sends instead. Merged
// into one, the row asserted System 0 B over a 24,000-byte system prompt and
// called two different prompts a token range.
test("a phase that suspended draws its opening and its re-entry, not one merged row", async () => {
  mount({
    events: [
      phase("2026-09-13T10:01:30Z", 90_000),
      promptSize({
        approximate_tokens: 1753,
        system_chars: 24000,
        user_chars: 2800,
        message_chars: 0,
      }),
      promptSize({
        approximate_tokens: 1814,
        system_chars: 0,
        user_chars: 0,
        message_chars: 3329,
      }),
    ],
  });
  await screen.findByText("Prompt sent");
  const tokens = screen.getAllByTitle("the engine's own approximation");
  expect(tokens.map((t) => t.textContent)).toEqual(["1,753", "1,814"]);
  // THE OPENING FRAME IS STILL ON THE PAGE. This is the figure the merge
  // destroyed — the re-entry's 0 replaced it, and the panel reported a phase
  // that opened with no prompt at all.
  const systems = screen.getAllByTitle("bytes in the system prompt");
  expect(systems.map((s) => s.textContent)).toEqual(["23 KB", "0 B"]);
  // And exactly the second row says why its opening is empty.
  expect(screen.getAllByText("resumed")).toHaveLength(1);
});

// EVERY FIGURE COLUMN SHARES ONE BOX WITH THE ROWS AROUND IT.
//
// `.num-col` is a fixed width and `.num-block` scrolls sideways when the port
// is narrower than the table. What makes the two work together is that all the
// rows are sized as ONE box (`.num-rows`): sized per row instead, each row's
// width is its own tag and its own chips, so the moment the block is clamped
// every row falls back to a different width and the columns splay — a phone
// drew 12 KB, 23 KB and 5.3 KB at three x positions under one heading.
//
// ASSERTED AS ANCESTRY, because jsdom computes no layout: the splay has no DOM
// signature, but the structure that prevents it does, and a figure column
// added outside the shared box is exactly how it comes back.
test("every figure column sits inside the box the rows are sized as", async () => {
  mount({
    events: [
      phase("2026-09-13T10:01:30Z", 90_000),
      event({
        type: "prefetch_summary",
        timestamp: "2026-09-13T10:00:00Z",
        payload: { turn_id: TURN, onboarding_hint_hit: true, onboarding_hint_bytes: 1016 },
      }),
      promptSize({ approximate_tokens: 7400, system_chars: 24_000, user_chars: 1200 }),
    ],
  });
  // BOTH TABLES, asserted by their own headings first: the prefetch half's
  // single figure is a column too — it was a bare trailing span — and without
  // this the loop below passes on a page that renders only the other half.
  await screen.findByText("Prompt sent");
  await screen.findByText("Reached the prompt");
  const columns = document.querySelectorAll(".num-col");
  expect([...columns].map((c) => c.textContent)).toContain("1016 B");
  for (const col of columns) {
    expect(col.closest(".num-rows"), col.textContent ?? "").toBeTruthy();
    expect(col.closest(".num-block"), col.textContent ?? "").toBeTruthy();
  }
});

// A LEDGER CELL THAT CAN BE CUT SAYS ITS WHOLE TEXT SOMEWHERE.
//
// `.truncate` cuts at a PIXEL, and until `.num-rows .truncate` was given a
// measure it never fired in these rows at all: inside a `max-content` box every
// item is drawn at its full width, so the class was decoration and the BLOCK
// grew in its place — which is how one 455px note came to push a figure column
// off a 1570px screen. The block holds its columns now and the prose is what
// gives, so the title is the only remaining copy of the rest of the sentence.
//
// THE THREAD NOTE IS THE ONE THAT REACHES IT: 455px against a 320px measure,
// and it grows with the message count's digits, where the other three the
// blocks can say all arrive whole under 250px.
//
// jsdom computes no layout, so the CUT cannot be asserted here. The `title`
// can, and it is exactly what goes missing when the next truncating cell is
// added beside these two.
test("a ledger cell that can be cut carries its whole text in a title", async () => {
  mount({
    events: [
      event({
        type: "prefetch_summary",
        timestamp: "2026-09-13T10:00:00Z",
        payload: {
          turn_id: TURN,
          thread_context_hit: true,
          thread_context_bytes: 4096,
          thread_context_read: true,
          thread_context_posts: 12,
          thread_context_stopped_short: true,
        },
      }),
    ],
  });
  const note = await screen.findByText(/but not the newest/);
  expect(note.className).toContain("truncate");
  expect(note.getAttribute("title")).toBe(note.textContent);
  const label = screen.getByText("The thread so far");
  expect(label.className).toContain("truncate");
  expect(label.getAttribute("title")).toBe("The thread so far");
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
  // THE BARRIER IS THE FACT, not a chip beside it. This waited on the page
  // bar's "1 phases" tag, which was the `PHASES 1` fact repeated 40px higher
  // up the screen and went when the header took the turn's state back — so
  // the wait is on the fact itself, which is where the count was always
  // stated.
  // …and the fact rather than the Phases CARD, which is a second element
  // with the same word in it.
  const phases = (await screen.findAllByText("Phases")).find((el) =>
    el.classList.contains("fact-label"),
  );
  expect(phases?.parentElement?.textContent).toContain("1");
  expect(screen.queryByText(/attempt \d+\/\d+/)).toBeNull();
});

/**
 * A ROW IS STYLED BY THE RULE THAT FILED IT.
 *
 * `bandOf` puts a row in "What went wrong" through `isFailed`, which reads the
 * payload's own flag as well as the promoted `failed` column — the first is
 * what a LIVE event carries and the second is all that survives into history,
 * which is why `internal/events` declares the pair once in Go. The row used to
 * style on `event.failed` alone, so a live failure appeared in the panel with
 * no red edge and no glyph: the same turn drawn two ways on one screen.
 */
test("a failure carried only on the payload is still drawn as one", async () => {
  mount({
    events: [
      phase("2026-09-13T10:00:02Z", 1000),
      event({
        type: "provider_fallback",
        timestamp: "2026-09-13T10:00:03Z",
        summary: "default failed (auth) — no provider left in the chain",
        // AS A LIVE EVENT DOES: the flag is on the payload and the promoted
        // column is still false.
        failed: false,
        payload: { turn_id: TURN, failed: true },
      }),
    ],
  });
  const row = await screen.findByText(/no provider left in the chain/);
  const link = row.closest("a");
  expect(link, "the row is not rendered as a turn row").toBeTruthy();
  expect(
    link!.className,
    "the panel filed it as a failure and the row drew it as ordinary work",
  ).toContain("failed");
});

/**
 * THE PAGE ACCOUNTS FOR ITS WHOLE ANSWER.
 *
 * Every panel on this screen draws a subset of the turn's rows, and on an
 * ordinary turn most of the answer is absorbed: a start and a record per
 * phase, plus both halves of the turn's own record. `ABSORBED` has named a
 * destination per type all along and `Story.absorbed` said it was counted "so
 * the screen can say where they went" — and no screen said, so the rows
 * stopped at the band. A row the query returned and the page silently dropped
 * is indistinguishable from a row the store never held, which is the one
 * shape this page was rebuilt to stop repeating.
 */
test("the rows it does not list are accounted for, by where each went", async () => {
  // A SELF-ITERATING TURN, so the rows and the kinds of row are different
  // numbers: five absorbed rows of three types. A note counting its own lines
  // would read "3" here and be wrong about the only thing it is for.
  mount({
    events: [
      event({
        type: "agent_phase_started",
        timestamp: "2026-09-13T10:00:01Z",
        payload: { turn_id: TURN, phase: "execute", iteration: 1 },
      }),
      phase("2026-09-13T10:00:02Z", 1000),
      event({
        type: "agent_phase_started",
        timestamp: "2026-09-13T10:00:03Z",
        payload: { turn_id: TURN, phase: "execute", iteration: 2 },
      }),
      phase("2026-09-13T10:00:04Z", 1000, { iteration: 2 }),
      event({
        type: "turn_completed",
        timestamp: "2026-09-13T10:00:05Z",
        payload: { turn_id: TURN, duration_ms: 2000 },
      }),
    ],
  });
  // THE COUNT IS THE ANSWER TO "is that everything?", and it is on the
  // trigger, so a reader gets it without opening anything.
  const trigger = await screen.findByRole("button", { name: /Already on this page/ });
  expect(trigger.textContent, "the trigger does not count the rows it stands for").toContain("5");
  // WHERE EACH WENT is the answer to the next question, and it is the map's
  // own words rather than a paraphrase of them beside it.
  fireEvent.click(trigger);
  const row = screen.getByText("agent_phase_started").parentElement!;
  expect(row.textContent, "a repeated type does not say how many it stands for").toContain("2");
  expect(screen.getByText(ABSORBED.agent_phase_started!)).toBeTruthy();
  expect(screen.getByText(ABSORBED.agent_phase_completed!)).toBeTruthy();
  expect(screen.getByText(ABSORBED.turn_completed!)).toBeTruthy();
});

/** A turn whose every row is a row of its own has nothing to account for, and
 *  a panel that is always present is a panel nobody reads — the same reason
 *  "What went wrong" is absent on a healthy turn. */
test("a turn with nothing absorbed draws no such note", async () => {
  mount({
    events: [
      event({
        type: "provider_fallback",
        timestamp: "2026-09-13T10:00:03Z",
        summary: "default failed (auth) — no provider left in the chain",
        payload: { turn_id: TURN, failed: true },
      }),
    ],
  });
  await screen.findByText(/no provider left in the chain/);
  expect(screen.queryByRole("button", { name: /Already on this page/ })).toBeNull();
});
