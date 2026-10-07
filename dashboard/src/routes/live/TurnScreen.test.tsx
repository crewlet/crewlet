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

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { TURN_TABS, TurnPeek, TurnScreen } from "./Turn.tsx";
import { WATCH_TAB, watchHref } from "~/lib/turns.ts";
import { ABSORBED } from "~/contract/turnbands.ts";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
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

/** Stand on one of the screen's tabs, as a reader's URL does. */
function onTab(tab: "timeline" | "transcript" | "context" | "tools") {
  location.hash = `#/live/turns/${TURN}${tab === "timeline" ? "" : `?tab=${tab}`}`;
}

beforeEach(() => onTab("transcript"));

/**
 * Mount the screen over one stubbed answer, on one of its tabs.
 *
 * THE TAB IS THE URL'S, as a reader's is: the phase cards are the Transcript
 * tab's, the brief and the prompt weights the Context tab's, and the header,
 * the page bar and what went wrong are on every tab.
 */
function mount(
  answer: Partial<TurnAnswer>,
  tab: "timeline" | "transcript" | "context" | "tools" = "transcript",
) {
  onTab(tab);
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what: string) =>
    what === "turn"
      ? Promise.resolve({ turn_id: TURN, events: [], truncated: false, nodes: [], ...answer })
      : Promise.resolve({});
  return frame(store, socket);
}

/** Open the page bar's menu: other attempts, traces, and the turn as JSON. */
async function openWaysOut() {
  fireEvent.click(await screen.findByRole("button", { name: /More on this turn/ }));
}

/** The screen inside what the frame mounts around every screen. */
function frame(store: Store, socket: LiveSocket) {
  return render(
    <ClientContext.Provider value={{ store, socket }}>
      <ViewerProvider>
        <Router>
          <TurnScreen turnId={TURN} />
        </Router>
      </ViewerProvider>
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
  // the TURN, and "first to last event" is a claim about
  // a window this page can no longer assume is whole. Both are `Fact.note`
  // now rather than a tile's caption, and the assertion is unchanged by that
  // on purpose — what must not appear is the sentence, wherever it is drawn.
  expect(screen.queryByText("no turn record")).toBeNull();
  expect(screen.queryByText("first to last event")).toBeNull();
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
  expect(screen.getByTitle("delivered the work")).toBeTruthy();
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
  mount(
    {
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
    },
    "context",
  );
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
  mount(
    {
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
    },
    "context",
  );
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
  mount(
    {
      events: [
        phase("2026-09-13T10:01:30Z", 90_000),
        event({
          type: "prefetch_summary",
          timestamp: "2026-09-13T10:00:00Z",
          payload: { turn_id: TURN, onboarding_hint_hit: true, onboarding_hint_bytes: 1016 },
        }),
        promptSize({ approximate_tokens: 7400, system_chars: 24_000, user_chars: 1200 }),
      ],
    },
    "context",
  );
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
  mount(
    {
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
    },
    "context",
  );
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
      ? Promise.resolve({ turn_id: TURN, events: [], truncated: false, nodes: [] })
      : Promise.resolve({});
  // `failed` is optional on a query row and required on a streamed envelope;
  // this phase did not fail.
  store.applyEvent({ ...phase("2026-09-13T10:01:30Z", 90_000), failed: false });

  frame(store, socket);

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
      ? Promise.resolve({ turn_id: TURN, events: [], truncated: false, nodes: [] })
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

  frame(store, socket);

  // ONE trace, not two: the plural form is what a reader is offered when a
  // turn was genuinely resumed elsewhere, and this turn was not.
  await openWaysOut();
  const item = await screen.findByRole("menuitem", { name: /^Trace/ });
  expect(screen.queryByText(/Trace 1 of/)).toBeNull();
  fireEvent.click(item);
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

  let written = "";
  Object.defineProperty(navigator, "clipboard", {
    configurable: true,
    value: { writeText: (t: string) => ((written = t), Promise.resolve()) },
  });
  await openWaysOut();
  fireEvent.click(await screen.findByRole("menuitem", { name: /Copy turn as JSON/ }));

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
  // Two traces, so the menu names them numbered rather than as one.
  await openWaysOut();
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
  // One trace, so the menu offers its single unnumbered entry — which it
  // could only have got from the row.
  await openWaysOut();
  expect(await screen.findByRole("menuitem", { name: /^Trace/ })).toBeTruthy();
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
  mount(woken({ id: "e-9", type: "chat_message", summary: MANY }), "context");
  await screen.findByText("Woken by");
  // The header took the lead, and only the lead.
  expect(document.querySelector(".object-title")?.textContent).toBe(ONE_SENTENCE);
  // And the panel carries the whole thing, that lead included.
  expect(briefProse()).toEqual([MANY]);
});

test("a one-sentence trigger the title says whole is not printed twice", async () => {
  mount(woken({ id: "e-9", type: "chat_message", summary: ONE_SENTENCE }), "context");
  // The panel is still drawn, because it carries the link to the trigger
  // event. What goes is the prose, which is the half the header already is.
  expect(await screen.findByText("Woken by")).toBeTruthy();
  expect(screen.getByRole("link", { name: "the trigger →" })).toBeTruthy();
  expect(document.querySelector(".object-title")?.textContent).toBe(ONE_SENTENCE);
  expect(briefProse()).toEqual([]);
});

test("…and with nothing else to carry, the panel goes with it", async () => {
  mount(woken({ type: "chat_message", summary: ONE_SENTENCE }), "context");
  // The title is what says this screen rendered its answer at all.
  await waitFor(() =>
    expect(document.querySelector(".object-title")?.textContent).toBe(ONE_SENTENCE),
  );
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
  await openWaysOut();
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
  const fact = (await screen.findAllByText("Phase")).find((el) =>
    el.classList.contains("fact-label"),
  );
  expect(fact?.parentElement?.textContent).toContain("Execute");
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

/**
 * A TURN WITH NO PHASE YET IS STILL SOMEBODY'S.
 *
 * The engine publishes `agent_turn_started` before the turn gathers its
 * context, so on a turn deep-linked while it does — or one that died doing it
 * — the opening record is the only row the answer holds. The header read its
 * seat and what woke it off the phases alone, and headed that turn "Turn", run
 * by "the engine". `ABSORBED` files the record into the header, and this is
 * the header keeping that claim.
 */
test("a turn with only its opening record is headed by its seat and its wake", async () => {
  const opening = event({
    type: "agent_turn_started",
    // Not the phases' `actor` fallback: the seat is read off the record.
    actor: "",
    payload: {
      turn_id: TURN,
      role: "CEO",
      agent_handle: "ceo",
      trigger: { type: "external_notification", summary: "Ana asked for the numbers" },
      started_at: "2026-09-13T10:00:00Z",
      resumed: false,
    },
  });
  mount({ events: [opening] });
  await waitFor(() =>
    expect(document.querySelector(".object-title")?.textContent).toBe("Ana asked for the numbers"),
  );
  // THE SEAT IS THE TRAIL'S on the page (see crumbs.test.tsx) and a fact on
  // the rail, which has no trail: read off the record, never "the engine".
  cleanup();
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what: string) =>
    what === "turn"
      ? Promise.resolve({ turn_id: TURN, events: [opening], truncated: false, nodes: [] })
      : Promise.resolve({});
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <ViewerProvider>
        <Router>
          <TurnPeek turnId={TURN} />
        </Router>
      </ViewerProvider>
    </ClientContext.Provider>,
  );
  await waitFor(() => {
    const head = document.querySelector(".object-head")?.textContent ?? "";
    expect(head, "the rail named no seat for a turn whose record names one").toContain("CEO");
    expect(head).not.toContain("the engine");
  });
  // AND THE ROW IS NOT "ALREADY ON THIS PAGE": the opening record is what the
  // turn was GIVEN, which the header, the trail and the Context tab read.
  expect(screen.queryByRole("button", { name: /Already on this page/ })).toBeNull();
});

/**
 * A RESUMED SEGMENT DOES NOT RENAME THE TURN. A parked coding run publishes a
 * start per segment under one id, and a segment's wake is the box's completion
 * rather than what asked for the work — so where both are held, the header is
 * the dispatch's.
 */
test("the dispatch's opening record heads the turn over a resumed segment's", async () => {
  mount({
    events: [
      event({
        type: "agent_turn_started",
        timestamp: "2026-09-13T10:00:00Z",
        payload: {
          turn_id: TURN,
          role: "CEO",
          trigger: { type: "external_notification", summary: "Ana asked for the numbers" },
          resumed: false,
        },
      }),
      event({
        type: "agent_turn_started",
        timestamp: "2026-09-13T10:05:00Z",
        payload: {
          turn_id: TURN,
          role: "CEO",
          trigger: { type: "sandbox_run_completed", summary: "the box finished" },
          resumed: true,
        },
      }),
    ],
  });
  await waitFor(() =>
    expect(document.querySelector(".object-title")?.textContent).toBe("Ana asked for the numbers"),
  );
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

// ---------------------------------------------------------------------------
// Steer
// ---------------------------------------------------------------------------

/** Mount over a seat whose live overlay places it on this turn, at `stage`. */
function onTurn(stage: "phase" | "parked") {
  const store = new Store();
  store.applyAgents([
    {
      role: "CEO",
      handle: "ceo",
      turn: { turn_id: TURN, started_at: "2026-09-13T10:00:00Z", stage },
    },
  ] as never);
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what: string) =>
    what === "turn"
      ? Promise.resolve({
          turn_id: TURN,
          nodes: [],
          events: [phase("2026-09-13T10:01:30Z", 90_000)],
          truncated: false,
        })
      : Promise.resolve({});
  frame(store, socket);
}

// THE STATUS SAYS WHERE THE TURN IS, running or parked, beside its title.
test("a running turn says so beside its title, and a parked one says what it waits on", async () => {
  onTurn("phase");
  expect(await screen.findByText(/^Running/)).toBeTruthy();
  cleanup();
  onTurn("parked");
  expect(await screen.findByText(/^Parked on a coding run/)).toBeTruthy();
});

// NEITHER RUNNING NOR ENDED IS A STATE, and the header says it. A turn whose
// node stopped before it closed has no seat on it and no closing record, and
// the header drew no mark at all beside a turns list reading "Not settled".
//
// AND ONLY WHILE THE OVERLAY CAN SEE THE SEATS: a view with no live
// connection has no seat to ask, and "no seat is on it" is then not an answer.
test("a turn nobody is running and nothing closed says it has not settled", async () => {
  const events = [
    event({
      type: "agent_turn_started",
      payload: { turn_id: TURN, role: "CEO", started_at: "2026-09-13T10:00:00Z" },
    }),
  ];
  onTab("transcript");
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what: string) =>
    what === "turn"
      ? Promise.resolve({ turn_id: TURN, events, truncated: false, nodes: [] })
      : Promise.resolve({});
  frame(store, socket);
  // THE ANSWER IS IN — the Timeline's rows are drawn from it — and still no
  // claim is made about who is running the turn.
  await act(async () => {
    await new Promise((r) => setTimeout(r, 50));
  });
  expect(screen.queryByLabelText("Loading the turn")).toBeNull();
  expect(screen.queryByText("not settled"), "a disconnected view guessed").toBeNull();
  act(() => store.setConnected(true));
  expect(await screen.findByText("not settled")).toBeTruthy();
  cleanup();
  onTurn("phase");
  expect(await screen.findByText(/^Running/)).toBeTruthy();
  expect(screen.queryByText("not settled")).toBeNull();
});

// THE BAR HOLDS THE TURN'S OWN CONTROLS, and the rest is one menu. "Copy" sat
// beside the frame's "Copy link" meaning something else, and with Download the
// bar was eight controls — scrolled half off a phone's edge, where everything
// but Steer now folds into the frame's "More".
test("the turn as JSON is in the page bar's menu, and only Steer stays on a phone", async () => {
  onTurn("phase");
  const steer = await screen.findByRole("button", { name: /Steer/ });
  expect(steer.closest(".page-action-folds")).toBeNull();
  expect(screen.queryByRole("button", { name: /^Copy$/ })).toBeNull();
  expect(screen.queryByRole("button", { name: /^Download$/ })).toBeNull();
  const more = screen.getByRole("button", { name: /More on this turn/ });
  expect(more.closest(".page-action-folds")).not.toBeNull();
  fireEvent.click(more);
  expect(await screen.findByRole("menuitem", { name: /Download turn as JSON/ })).toBeTruthy();
  expect(screen.getByRole("menuitem", { name: /Copy turn as JSON/ })).toBeTruthy();
});

// ---------------------------------------------------------------------------
// Watching it live
// ---------------------------------------------------------------------------

/** The seat's push while it runs this turn's `phase`, as the projection sends it. */
function runningSeat(phase: string) {
  return {
    role: "CEO",
    handle: "ceo",
    activity: "working",
    turn: { turn_id: TURN, started_at: "2026-09-13T10:00:00Z", stage: "phase" },
    live_call: {
      turn_id: TURN,
      phase,
      iteration: 1,
      model: "claude-sonnet-5",
      in_progress: true,
      round_num: 0,
      rounds_used: 1,
      max_rounds: 25,
      started_at: "2026-09-13T10:01:00Z",
      updated_at: "2026-09-13T10:01:30Z",
      input_tokens: 0,
      output_tokens: 0,
      total_tokens: 0,
    },
  };
}

/** Each phase card on the Transcript: its phase, whether it is live, whether it is open. */
function cards() {
  return [...document.querySelectorAll(".phase-card")].map((card) => ({
    phase: card.querySelector(".phase-head")?.textContent ?? "",
    live: card.classList.contains("live"),
    open: card.querySelector(".phase-body") !== null,
  }));
}

test("a watch link's tab is one the page has", () => {
  expect(TURN_TABS as readonly string[]).toContain(WATCH_TAB);
});

// A WATCH LINK LANDS ON THE PHASE THE TURN IS ON, OPEN. With the first card the
// only open one, it opened on the onboarding pass that ended minutes ago and
// the review the reader came to watch was a closed row two cards under it.
test("a watch link lands on the transcript with the running phase open", async () => {
  location.hash = watchHref(TURN);
  const store = new Store();
  store.applyAgents([runningSeat("review")] as never);
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what: string) =>
    what === "turn"
      ? Promise.resolve({
          turn_id: TURN,
          nodes: [],
          events: [
            phase("2026-09-13T10:00:20Z", 20_000, { phase: "onboarding" }),
            phase("2026-09-13T10:00:40Z", 20_000, { phase: "execute" }),
          ],
          truncated: false,
        })
      : Promise.resolve({});
  frame(store, socket);
  await waitFor(() => expect(cards()).toHaveLength(3));
  expect(screen.getByRole("tab", { selected: true }).textContent).toBe("Transcript");
  // NOT THE FIRST CARD, or "the first is open" would pass this for nothing.
  expect(cards()[0]?.live).toBe(false);
  expect(cards().find((c) => c.live)?.open, "the running phase is open").toBe(true);
  // AND THE FIRST STAYS OPEN, as it always was, and a settled one after it shut.
  expect(
    cards()
      .filter((c) => !c.live)
      .map((c) => c.open),
  ).toEqual([true, false]);
});

/**
 * Mount on a watch link over the seat's push and the turn's stored events,
 * answering `sandbox_tail` with `tail` — the page a reader lands on.
 */
function mountWatching(seat: Record<string, unknown>, events: EventRecord[], tail?: unknown) {
  location.hash = watchHref(TURN);
  const store = new Store();
  store.applyAgents([seat] as never);
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what: string) =>
    what === "turn"
      ? Promise.resolve({ turn_id: TURN, events, truncated: false, nodes: [] })
      : what === "sandbox_tail" && tail
        ? Promise.resolve(tail)
        : Promise.resolve({});
  frame(store, socket);
  return store;
}

/** The phase cards' headers, in the order the Transcript draws them. */
const heads = () => [...document.querySelectorAll<HTMLElement>(".phase-card .phase-head")];

// A PHASE THAT GOES LIVE WHILE THE PAGE IS OPEN is a new card, and it mounts
// open; a settled card the reader CLOSED stays closed through the push that
// brought it, because a card's open state is latched at mount and is the
// reader's after it.
test("a phase that starts while the transcript is open arrives open, and a card the reader closed stays closed", async () => {
  const store = mountWatching({ ...runningSeat("execute"), live_call: null }, [
    phase("2026-09-13T10:00:20Z", 20_000, { phase: "onboarding" }),
    phase("2026-09-13T10:00:40Z", 20_000, { phase: "execute" }),
  ]);
  await waitFor(() => expect(cards()).toHaveLength(2));
  expect(cards().map((c) => c.open)).toEqual([true, false]);
  fireEvent.click(heads()[0]!);
  expect(
    cards().map((c) => c.open),
    "the reader closed the first",
  ).toEqual([false, false]);
  act(() => store.applyAgents([runningSeat("review")] as never));
  await waitFor(() => expect(cards()).toHaveLength(3));
  expect(cards().find((c) => c.live)?.open, "the phase that started is open").toBe(true);
  expect(
    cards()
      .filter((c) => !c.live)
      .map((c) => c.open),
    "the card the reader closed is still closed",
  ).toEqual([false, false]);
});

// THE LIVE CARD IS THE READER'S ONCE IT IS DRAWN. Two pushes land every tool
// round, and a card re-seeded from "it is live" on each would reopen what the
// reader had just closed, twice a round, for the rest of the phase.
test("a live card the reader closed stays closed when the next round's push lands", async () => {
  const store = mountWatching(runningSeat("execute"), [
    phase("2026-09-13T10:00:20Z", 20_000, { phase: "onboarding" }),
  ]);
  await waitFor(() => expect(cards()).toHaveLength(2));
  const live = () => document.querySelector<HTMLElement>(".phase-card.live");
  expect(live()?.querySelector(".phase-body"), "it lands open").not.toBeNull();
  fireEvent.click(live()!.querySelector<HTMLElement>(".phase-head")!);
  expect(live()?.querySelector(".phase-body")).toBeNull();
  const next = runningSeat("execute");
  act(() =>
    store.applyAgents([
      {
        ...next,
        live_call: { ...next.live_call, rounds_used: 2, updated_at: "2026-09-13T10:01:40Z" },
      },
    ] as never),
  );
  await waitFor(() => expect(document.body.textContent).toContain("round"));
  expect(live(), "still the live phase").not.toBeNull();
  expect(live()?.querySelector(".phase-body"), "and still closed").toBeNull();
});

// A LIVE PHASE THAT COMPLETES KEEPS ITS CARD, AND THE CARD STAYS OPEN. The
// durable record replaces the live one under the same `turn|phase|iteration`
// key, so the card is not remounted; and it latched open at mount, so the
// transcript the reader was following does not shut the moment it ends.
test("a live phase that completes keeps its card, open", async () => {
  const store = mountWatching(runningSeat("execute"), [
    phase("2026-09-13T10:00:20Z", 20_000, { phase: "onboarding" }),
  ]);
  await waitFor(() => expect(cards()).toHaveLength(2));
  const card = document.querySelector(".phase-card.live");
  expect(card?.querySelector(".phase-body")).not.toBeNull();
  // THE ORDER THE ENGINE SENDS THEM IN: the record first, then the push that
  // clears the call while the turn is between phases.
  act(() => {
    store.applyEvent({
      ...phase("2026-09-13T10:01:50Z", 50_000, { phase: "execute" }),
      failed: false,
    } as never);
    store.applyAgents([{ ...runningSeat("review"), live_call: null }] as never);
  });
  await waitFor(() => expect(document.querySelector(".phase-card.live")).toBeNull());
  expect(cards()).toHaveLength(2);
  const after = document.querySelectorAll(".phase-card")[1];
  expect(after, "the same card, not a remount").toBe(card);
  expect(after?.querySelector(".phase-body"), "and still open").not.toBeNull();
});

// ---------------------------------------------------------------------------
// A parked turn, watched
// ---------------------------------------------------------------------------

/** The seat's push while its turn is parked on a coding run: working, no call. */
function parkedSeat() {
  return {
    role: "CEO",
    handle: "ceo",
    activity: "working",
    turn: { turn_id: TURN, started_at: "2026-09-13T10:00:00Z", stage: "parked" },
    live_call: null,
  };
}

/** The run the executor launched before it parked, as the engine announces it. */
function launched(at: string, launchId = "L1") {
  return event({
    type: "sandbox_run_started",
    timestamp: at,
    payload: {
      turn_id: TURN,
      launch_id: launchId,
      started_at: at,
      coding_agent: "claude-code",
    },
  });
}

const TAIL = {
  outcome: "tail",
  turn_id: TURN,
  launch_id: "L1",
  node: "node-b",
  output: {
    text: "running go test ./provisioner/...",
    source: "transcript",
    cut: false,
    as_of: "2026-09-13T10:05:00Z",
    finished: false,
  },
};

// A PARKED TURN'S LIVE WORK IS ITS CODING RUN, and a watch link — every one of
// them lands on this tab — finds it here: the run's live output, the way to
// the run's own page, and the executor's card it parked in. The page had no
// phase live, so it was a Transcript with nothing moving on it and no way to
// the thing that was.
test("a parked turn's transcript draws its coding run live, with a way to the run", async () => {
  mountWatching(
    parkedSeat(),
    [phase("2026-09-13T10:01:30Z", 90_000), launched("2026-09-13T10:01:20Z")],
    TAIL,
  );
  expect(await screen.findByText("running go test ./provisioner/...")).toBeTruthy();
  const run = screen.getByText("Coding run").closest(".crewlet-card") as HTMLElement;
  expect(run.textContent).toContain("claude-code");
  expect(within(run).getByRole("link", { name: "Open the run" }).getAttribute("href")).toBe(
    "#/live/runs/t-77",
  );
});

// THE PHASE IT PARKED IN IS OPEN, which is not the first card on a later
// iteration: the executor that launched the run is the newest execute.
test("a parked turn on its second iteration opens the executor it parked in", async () => {
  mountWatching(
    parkedSeat(),
    [
      phase("2026-09-13T10:01:00Z", 60_000),
      phase("2026-09-13T10:01:30Z", 30_000, { phase: "review", decision: "self_iterate" }),
      phase("2026-09-13T10:03:00Z", 60_000, { iteration: 2 }),
      launched("2026-09-13T10:02:50Z"),
    ],
    TAIL,
  );
  await waitFor(() => expect(cards()).toHaveLength(3));
  expect(cards().map((c) => c.open)).toEqual([true, false, true]);
});

// A RUN WHOSE ANNOUNCEMENT HAS NOT BEEN READ YET is still said to be out: the
// stage moves on the push, and the answer naming the run is a refetch behind.
test("a parked turn whose run is not announced yet says it is reading it", async () => {
  mountWatching(parkedSeat(), [phase("2026-09-13T10:01:30Z", 90_000)]);
  expect(await screen.findByText(/Reading which run it launched/)).toBeTruthy();
});

// ---------------------------------------------------------------------------
// Where a watch link lands
// ---------------------------------------------------------------------------

/**
 * The shell's scroller, standing in for the frame: `top`/`bottom` are its
 * box, and its `scrollTop` is a plain value this test can read back.
 */
function scroller(scrollTop: number) {
  const el = document.createElement("div");
  el.id = "screen-scroll";
  Object.defineProperty(el, "scrollTop", { value: scrollTop, writable: true });
  document.body.appendChild(el);
  return el;
}

/** Place every element: the scroller's box, and the tailing ledger's. */
function layout(view: { top: number; bottom: number }, ledger: { top: number; bottom: number }) {
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
    this: HTMLElement,
  ) {
    const box = this.id === "screen-scroll" ? view : this.matches(".tail-scroll") ? ledger : null;
    const { top, bottom } = box ?? { top: 0, bottom: 0 };
    return {
      top,
      bottom,
      left: 0,
      right: 0,
      width: 0,
      height: bottom - top,
      x: 0,
      y: top,
      toJSON() {},
    };
  });
}

/** The seat running execute with one round written, so its ledger is drawn. */
function oneRoundIn() {
  const seat = runningSeat("execute");
  return {
    ...seat,
    live_call: {
      ...seat.live_call,
      tool_executions: [
        {
          name: "knowledge.search",
          round: 1,
          arguments: { q: "retry backoff" },
          result: "",
          success: true,
          started_at: "2026-09-13T10:01:10Z",
          duration_ms: 300,
        },
      ],
    },
  };
}

const frameDone = () =>
  act(async () => {
    await new Promise((r) => requestAnimationFrame(() => r(null)));
    await new Promise((r) => requestAnimationFrame(() => r(null)));
  });

// THE ROUND A READER CAME TO WATCH IS ON SCREEN WHEN THEY ARRIVE. The live
// ledger tails its newest round at the bottom of a box as tall as the view,
// and under the header and the cards above it that bottom was below the fold.
test("a watch link brings the live ledger's newest round into view once, as it lands", async () => {
  const el = scroller(0);
  layout({ top: 70, bottom: 900 }, { top: 465, bottom: 1262 });
  const store = mountWatching(oneRoundIn(), [
    phase("2026-09-13T10:00:20Z", 20_000, { phase: "onboarding" }),
  ]);
  await waitFor(() => expect(cards()).toHaveLength(2));
  expect(document.querySelector(".phase-card.live .tail-scroll.tailing")).not.toBeNull();
  await frameDone();
  expect(el.scrollTop, "the ledger's bottom is at the view's").toBe(362);
  // ONCE: back at the top, a later push moves nothing.
  el.scrollTop = 0;
  const next = oneRoundIn();
  act(() =>
    store.applyAgents([
      { ...next, live_call: { ...next.live_call, updated_at: "2026-09-13T10:01:45Z" } },
    ] as never),
  );
  await frameDone();
  expect(el.scrollTop, "a push is not an arrival").toBe(0);
  el.remove();
});

// AND NEVER UNDER A READER WHO HAS ALREADY MOVED: the page moves only when the
// reader is not reading.
test("a watch link moves nothing once the reader has scrolled", async () => {
  const el = scroller(0);
  layout({ top: 70, bottom: 900 }, { top: 465, bottom: 1262 });
  mountWatching(oneRoundIn(), [phase("2026-09-13T10:00:20Z", 20_000, { phase: "onboarding" })]);
  // THE READER MOVES while the turn is still being read.
  el.scrollTop = 40;
  await waitFor(() => expect(cards()).toHaveLength(2));
  expect(document.querySelector(".phase-card.live .tail-scroll.tailing")).not.toBeNull();
  await frameDone();
  expect(el.scrollTop).toBe(40);
  el.remove();
});

/**
 * THE RAIL'S PHASE STRIP READS THE RESCUE TOO. A reviewer the engine decided
 * for writes `self_iterate`, the same word a reviewer chooses on purpose — so
 * a strip that glossed the word alone said the reviewer sent the turn back,
 * about a round nothing judged.
 */
test("the rail's phase strip says when the engine decided for the reviewer", async () => {
  const store = new Store();
  const socket = new LiveSocket(store);
  const review = phase("2026-09-13T10:02:00Z", 900, {
    phase: "review",
    decision: "self_iterate",
    rescue_fired: true,
  });
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what: string) =>
    what === "turn"
      ? Promise.resolve({ turn_id: TURN, events: [review], truncated: false, nodes: [] })
      : Promise.resolve({});
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <ViewerProvider>
        <Router>
          <TurnPeek turnId={TURN} />
        </Router>
      </ViewerProvider>
    </ClientContext.Provider>,
  );
  expect(
    await screen.findByText("never decided — the engine sent the turn back for another round"),
  ).toBeTruthy();
  expect(screen.queryByText("sent the turn back for another round")).toBeNull();
});
