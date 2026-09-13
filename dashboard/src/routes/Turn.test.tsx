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
import { outcomeOf, problemCount, turnSpan } from "./Turn.tsx";
import type { EventRecord } from "~/protocol/index.ts";

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
  const phase = (startedAt: string, at: string) => ({ startedAt, at });
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
        phase("2026-09-13T10:00:20Z", "2026-09-13T10:00:30Z"),
        phase("2026-09-13T10:00:00Z", "2026-09-13T10:01:30Z"),
      ],
    );
    expect(span.to - span.from).toBe(90_000);
  });

  test("spans the query answer and the stream together", () => {
    // The case it exists for: a turn deep-linked while it runs answers the
    // query with its opening rows and streams the rest.
    const span = turnSpan(
      [event("2026-09-13T10:00:00Z"), event("2026-09-13T10:00:05Z")],
      [phase("2026-09-13T10:00:10Z", "2026-09-13T10:02:00Z")],
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
      [phase("not a timestamp", "2026-09-13T10:00:30Z"), phase("2026-09-13T10:00:00Z", "also not")],
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
    expect(turnSpan([], [phase("", "2026-09-13T10:00:30Z")])).toEqual({ from: 0, to: 0 });
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

  test("no record at all is still an em dash, not a failure", () => {
    // The absence the early return exists for, which reading the flag first
    // must not swallow.
    expect(outcomeOf({ summary: undefined, learning: undefined })).toEqual({
      word: "—",
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
