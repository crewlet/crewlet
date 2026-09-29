/**
 * The three things the Turn header DERIVES, each of which was quietly wrong.
 *
 * None of them is a rendering question, which is why they are pure functions
 * and tested as such: how many problems a turn had, what window it ran in,
 * and what word describes how it ended are decisions about records, and each
 * one was making a claim the rest of the same page contradicted — a badge
 * counting two problems over a panel holding one row, a duration that began
 * at the first worker rather than at the turn, and an em dash captioned "no
 * turn record" printed above the panel rendering that record.
 */

import { describe, expect, test } from "vitest";
import { outcomeOf, problemCount, turnFacts, type TurnView } from "./Turn.tsx";
// THE SPAN RULE LIVES BESIDE THE PHASES IT MEASURES NOW. The turn CARD had a
// second one — two LANDING instants subtracted, which drops the first phase's
// own length — so the two are one function and one set of cases.
import { turnSpan } from "~/lib/phases.ts";
import type { PhaseRecord, Timed } from "~/lib/phases.ts";
import type { EventRecord } from "~/protocol/index.ts";
import { tellStory } from "~/lib/turnstory.ts";
import { phaseRecord } from "~/test/phaseRecord.ts";

function record(payload: Record<string, unknown>): EventRecord {
  return { payload } as unknown as EventRecord;
}

function row(type: string): EventRecord {
  return { type } as unknown as EventRecord;
}

describe("problemCount", () => {
  test("a stop the engine wrote two records about is one problem", () => {
    // internal/engine/telemetry.go closes a failed turn with the summary
    // (`failed: true`) and then a dedicated `turn.guard_breach` — one stop,
    // two events. Summing them put "2 problems" in the header over the one
    // row the panel below it renders.
    expect(problemCount([row("turn.guard_breach")], true)).toBe(1);
    expect(problemCount([row("budget_exhausted")], true)).toBe(1);
    expect(problemCount([row("llm_unavailable")], true)).toBe(1);
  });

  test("a failure with no record of its own still counts", () => {
    // A turn the REVIEWER decided against carries the flag and nothing else:
    // `publishFailure` writes nothing when no guard fired and no error came
    // back. At zero the header would say nothing and `clean` would go on to
    // claim "nothing went wrong" about a turn that failed.
    expect(problemCount([], true)).toBe(1);
  });

  test("the flag is deduped against its own record, never against the rows", () => {
    // The case a plain `||` swallowed: a reviewer-failed turn that ALSO
    // recovered a provider hand-off has two independent problems, and only
    // one of them is a row. `sandbox_run_failed` is a failure the engine
    // publishes elsewhere, so it does not describe this turn's stop either.
    expect(problemCount([row("provider_fallback")], true)).toBe(2);
    expect(problemCount([row("sandbox_run_failed")], true)).toBe(2);
    // …and the dedupe still fires when the stop record is among several.
    expect(problemCount([row("provider_fallback"), row("turn.guard_breach")], true)).toBe(2);
  });

  test("rows without a failure are counted as they are", () => {
    // A provider fallback is in `WENT_WRONG` and is not a failed turn.
    expect(problemCount([row("provider_fallback"), row("notification_skipped")], false)).toBe(2);
  });

  test("a clean turn has nothing to count", () => {
    expect(problemCount([], false)).toBe(0);
  });
});

describe("turnSpan", () => {
  // A FINISHED phase, which is what the query answers with: its landing
  // instant plus what the engine measured. There is no second timestamp to
  // read a start off — see `phaseDuration` in ~/lib/phases.ts.
  const phase = (at: string, durationMs: number): Timed => ({
    live: false,
    startedAt: at,
    at,
    durationMs,
  });
  const running = (startedAt: string, at: string): Timed => ({
    live: true,
    startedAt,
    at,
    durationMs: 0,
  });
  const event = (timestamp: string) => ({ timestamp });

  test("opens at the earliest START, not at the earliest phase to land", () => {
    // `phases` is ordered by when each phase LANDED, so a worker a delegate
    // spawned at T+20s and that finished at T+30s sorts ahead of the execute
    // round running T+0 → T+90 that spawned it. Reading `phases[0].startedAt`
    // opened the window twenty seconds late and "Took" lost the whole stretch
    // before the fan-out.
    const span = turnSpan(
      [],
      [
        phase("2026-09-13T10:00:30Z", 10_000), // the worker: T+20 → T+30
        phase("2026-09-13T10:01:30Z", 90_000), // its host round: T+0 → T+90
      ],
    );
    expect(span.to - span.from).toBe(90_000);
  });

  test("a finished phase contributes when it BEGAN, not when it landed", () => {
    // The regression the derivation exists to stop. `startedAt` on a
    // completed record equals `at`, so reading it put the phase's own END
    // into the minimum — and on a turn whose opening rounds have landed and
    // whose newest one is live, that reported the turn as beginning where its
    // first phase finished.
    const span = turnSpan([], [phase("2026-09-13T10:01:00Z", 60_000)]);
    expect(span.from).toBe(Date.parse("2026-09-13T10:00:00Z"));
    expect(span.to).toBe(Date.parse("2026-09-13T10:01:00Z"));
  });

  test("spans the query answer and the stream together", () => {
    // The case it exists for: a turn deep-linked while it runs answers the
    // query with its opening rows and streams the rest.
    const span = turnSpan(
      [event("2026-09-13T10:00:00Z"), event("2026-09-13T10:00:05Z")],
      [running("2026-09-13T10:00:10Z", "2026-09-13T10:02:00Z")],
    );
    expect(span.from).toBe(Date.parse("2026-09-13T10:00:00Z"));
    expect(span.to).toBe(Date.parse("2026-09-13T10:02:00Z"));
  });

  test("an unreadable instant is dropped, never taken as the start", () => {
    // `tsKey` answers 0 for what it cannot parse, and 0 is the epoch — one
    // unreadable timestamp would report a turn that has been running since
    // 1970.
    const span = turnSpan(
      [],
      [
        running("not a timestamp", "2026-09-13T10:00:30Z"),
        running("2026-09-13T10:00:00Z", "also not"),
      ],
    );
    expect(span.from).toBe(Date.parse("2026-09-13T10:00:00Z"));
    expect(span.to).toBe(Date.parse("2026-09-13T10:00:30Z"));
  });

  test("reads every event, not the first and last of a list it did not sort", () => {
    // The query answer arrives oldest first, but that is the CALLER's
    // property: indexing made the sort an invisible precondition, which is
    // exactly the precondition the phase list had already broken.
    const span = turnSpan([event("2026-09-13T10:02:00Z"), event("2026-09-13T10:00:00Z")], []);
    expect(span.from).toBe(Date.parse("2026-09-13T10:00:00Z"));
    expect(span.to).toBe(Date.parse("2026-09-13T10:02:00Z"));
  });

  test("a page holding nothing reports no window rather than a false one", () => {
    expect(turnSpan([], [])).toEqual({ from: 0, to: 0 });
    // Ends but no starts: a duration measured against nothing is worse than
    // the em dash the caller falls back to.
    expect(turnSpan([], [running("", "2026-09-13T10:00:30Z")])).toEqual({ from: 0, to: 0 });
  });
});

describe("outcomeOf", () => {
  test("a turn stopped before anything decided still reads as failed", () => {
    // The engine can end a turn with `failed: true` and no word at all —
    // `decision` and `review_outcome` are both the zero `phase.Decision`.
    // Read after the words, the flag was unreachable and this rendered an em
    // dash with no tone, captioned "no turn record".
    const out = outcomeOf({
      summary: record({ failed: true, error_kind: "guard_breach" }),
      learning: undefined,
    });
    expect(out.word).toBe("failed");
    expect(out.tone).toBe("critical");
    expect(out.sub).toBe("the engine stopped it: guard_breach");
  });

  test("no record at all is still an absence, not a failure", () => {
    // The absence the early return exists for, which reading the flag first
    // must not swallow. THE EMPTY STRING rather than the mark an absence is
    // drawn as: the two call sites that read this compared against the glyph,
    // so the day the product settled on one absent mark the comparison would
    // have gone false and a running turn would have grown an Outcome fact.
    expect(outcomeOf({ summary: undefined, learning: undefined })).toEqual({
      word: "",
      tone: undefined,
      sub: "",
    });
  });

  test("the reviewer's own `failed` is read even with no summary at all", () => {
    // The other half of the branch: a turn whose summary fell out of the
    // store's window but whose learning record survived. Nothing else in the
    // function can produce the critical tone from this input.
    const out = outcomeOf({ summary: undefined, learning: record({ review_outcome: "failed" }) });
    expect(out.word).toBe("failed");
    expect(out.tone).toBe("critical");
    expect(out.sub).toBe("the turn will not retry");
  });

  test("a turn that delivered reads as the executor's own word", () => {
    const out = outcomeOf({
      summary: record({ failed: false, decision: "done" }),
      learning: record({ review_outcome: "done", outcome: "delivered" }),
    });
    expect(out.word).toBe("done");
    expect(out.tone).toBe("positive");
    expect(out.sub).toBe("delivered the work");
  });
});

/**
 * The fact line is what the page and the rail BOTH wear, so what it says about
 * a number is said once. A note is a claim about where a value came from —
 * the kind of claim that goes stale silently, because nothing on screen
 * contradicts it — so these pin the branches rather than the prose: a wall
 * clock is captioned only when it was NOT measured, a token figure only when
 * workers are outside it, and the cache only when a phase reported one.
 */
describe("turnFacts", () => {
  function view(over: Partial<TurnView>): TurnView {
    return {
      turnId: "t",
      loading: false,
      error: null,
      attempt: null,
      events: [],
      cut: false,
      phases: [],
      own: [phaseRecord()],
      nested: new Map(),
      rec: {} as TurnView["rec"],
      role: "",
      trigger: null,
      outcome: outcomeOf({} as TurnView["rec"]),
      running: false,
      durationMs: null,
      span: { from: 0, to: 0, timed: false },
      tokens: 0,
      workerTokens: 0,
      workerCount: 0,
      iterations: 0,
      traceIds: [],
      story: tellStory([]),
      trouble: 0,
      clean: false,
      coverage: null,
      nodes: [],
      workItem: null,
      stage: "",
      parked: false,
      overlay: true,
      startedAt: 0,
      handle: "",
      paused: false,
      liveCall: null,
      ...over,
    };
  }

  function fact(v: TurnView, label: string, now = 0) {
    return turnFacts(v, now).find((f) => f.label === label);
  }

  test("the engine's own milliseconds carry no note", () => {
    // A caption under every duration is a caption nobody reads, and then the
    // one time it says something else it is missed. Measured is the silent
    // case precisely so the derived one is loud.
    const measured = fact(
      view({ durationMs: 121, span: { from: 10, to: 900, timed: true } }),
      "Wall clock",
    );
    expect(measured?.value).toBe("121ms");
    expect(measured?.note).toBe(undefined);
  });

  test("a duration this page derived says so, and says which window", () => {
    const spanned = fact(view({ span: { from: 1_000, to: 4_000, timed: false } }), "Wall clock");
    expect(spanned?.note).toBe("first to last event");
    // THE WATERFALL'S WINDOW, where anything was timed: the same length the
    // Turn row beneath the header is drawn at, and a note that says so.
    const timed = fact(view({ span: { from: 1_000, to: 1_416, timed: true } }), "Wall clock");
    expect([timed?.value, timed?.note]).toEqual(["416ms", "from its spans; no closing record"]);
    // A CUT VIEW HOLDS BOTH ENDS — the span is the turn's real window — but
    // the record carrying the engine's own measurement is missing from a turn
    // this page has both ends of, and those are different sentences.
    const cut = fact(
      view({ cut: true, span: { from: 1_000, to: 4_000, timed: false } }),
      "Wall clock",
    );
    expect(cut?.note).toBe("from its ends; record not shown");
  });

  test("a running turn's wall clock is how long it has run, off its own start", () => {
    const running = fact(view({ running: true, startedAt: 1_000 }), "Wall clock", 373_000);
    expect(running?.value).toBe("6m 12s");
    expect(running?.note).toBe(undefined);
  });

  test("a turn with neither a measurement nor a window states no duration", () => {
    const f = fact(view({ span: { from: 0, to: 0, timed: false } }), "Wall clock");
    expect(f?.value).toBe("");
    expect(f?.note).toBe(undefined);
  });

  test("tokens are in and out, and name the workers outside them", () => {
    // `subagent_tokens` is deliberately outside `total_tokens` at the engine,
    // so the note is the only thing on either surface that answers "how much
    // of this turn was fan-out".
    const own = [phaseRecord({ inputTokens: 1_200, outputTokens: 300 })];
    expect(fact(view({ own }), "Tokens")?.value).toBe("1,200 in · 300 out");
    expect(fact(view({ own, workerTokens: 400, workerCount: 1 }), "Tokens")?.note).toBe(
      "+400 in 1 worker",
    );
    expect(fact(view({ own, workerTokens: 400, workerCount: 3 }), "Tokens")?.note).toBe(
      "+400 in 3 workers",
    );
    expect(fact(view({ own }), "Tokens")?.note).toBe(undefined);
  });

  test("the cache is a share of the input, and absent — never 0% — when none was reported", () => {
    const cached = [phaseRecord({ inputTokens: 1_000, cacheReadTokens: 250 })];
    expect(fact(view({ own: cached }), "Cache")?.value).toBe("25%");
    expect(fact(view({ own: cached }), "Cache")?.note).toBe("250 of 1,000 input read from cache");
    const unreported = [phaseRecord({ inputTokens: 1_000, cacheReadTokens: 0 })];
    expect(fact(view({ own: unreported }), "Cache")?.value).toBe("");
  });

  test("a running phase reads as its round of its granted cap", () => {
    const liveCall = {
      phase: "execute",
      iteration: 1,
      rounds_used: 2,
      round_num: 2,
      max_rounds: 25,
    } as TurnView["liveCall"];
    // THE ROUND IS THE NOTE: as a clause of the value, "Execute · round 3 of
    // 24" wrapped to two lines in its track at 1440.
    const running = fact(view({ running: true, stage: "phase", liveCall }), "Phase");
    expect([running?.value, running?.note]).toEqual(["Execute", "round 3 of 25"]);
    const parked = fact(view({ running: true, stage: "parked", parked: true }), "Phase");
    expect([parked?.value, parked?.note]).toEqual(["Execute", "waiting on a coding run"]);
  });

  test("a settled turn's phases hold one line, with its iterations as the note", () => {
    const own = [
      phaseRecord({ key: "a", phase: "execute", iteration: 1 }),
      phaseRecord({ key: "b", phase: "review", iteration: 1 }),
      phaseRecord({ key: "c", phase: "execute", iteration: 2 }),
    ];
    const phase = fact(view({ own, iterations: 2 }), "Phase");
    expect([phase?.value, phase?.note]).toEqual(["Execute → Review", "2 iterations"]);
    expect(fact(view({ own: own.slice(0, 2), iterations: 1 }), "Phase")?.note).toBe(undefined);
  });

  test("the node is the one the answer says ran it", () => {
    expect(fact(view({ nodes: ["node-a", "node-b"] }), "Node")?.value).toBe("node-a, node-b");
    expect(fact(view({}), "Node")?.value).toBe("");
  });

  test("a turn with no phase record in hand counts nothing, and notes nothing", () => {
    // "0 tokens · 0 tool calls" is a claim the turn did nothing. A turn still
    // loading has not been shown to have done nothing — and a note hanging
    // under an absent value would outlive the value it qualifies.
    const empty = view({ own: [], workerTokens: 400, workerCount: 2 });
    expect(fact(empty, "Tokens")?.value).toBe("");
    expect(fact(empty, "Tokens")?.note).toBe(undefined);
    expect(fact(empty, "Tool calls")?.value).toBe("");
    expect(fact(empty, "Cache")?.value).toBe("");
  });
});
