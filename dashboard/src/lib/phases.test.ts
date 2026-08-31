/**
 * The rules that stop the transcript moving under the reader.
 *
 * Every case here is one of the ten mechanisms behind "the LLM calls are
 * jumping and are not easy to follow". They are all ordering or identity
 * rules, which is exactly the kind of thing that looks right in a browser
 * until a turn runs for four minutes.
 */

import { describe, expect, test } from "vitest";
import {
  decisionLabel,
  fromLiveCall,
  fromPhaseEvent,
  groupTurns,
  mergePhases,
  phaseKey,
  ledgerOf,
  narrations,
  rounds,
  splitThinking,
  toolCalls,
  type PhaseRecord,
} from "./phases.ts";
import type { EventRecord, LiveCall } from "~/protocol/index.ts";

function liveCall(over: Partial<LiveCall> = {}): LiveCall {
  return {
    turn_id: "t1",
    phase: "execute",
    iteration: 1,
    model: "claude-sonnet-5",
    trigger: null,
    prompt: "",
    prompt_messages: null,
    response: "",
    input_tokens: 0,
    output_tokens: 0,
    total_tokens: 0,
    tool_executions: null,
    round_num: 0,
    rounds: 0,
    in_progress: true,
    updated_at: "2026-01-01T00:00:05Z",
    ...over,
  };
}

function phaseEvent(over: Record<string, unknown> = {}, ts = "2026-01-01T00:00:09Z"): EventRecord {
  return {
    id: "ev1",
    type: "agent_phase_completed",
    timestamp: ts,
    source: "engine",
    actor: "PM",
    summary: "",
    category: "lifecycle",
    trace_id: "",
    span_id: "",
    parent_span_id: "",
    topic: "",
    payload: {
      turn_id: "t1",
      phase: "execute",
      iteration: 1,
      model: "claude-sonnet-5",
      total_tokens: 120,
      ...over,
    },
  };
}

describe("identity", () => {
  test("a live phase and its finished record share ONE key", () => {
    // THE fix for the row that jumped. They used to differ — the live row was
    // keyed `live|turn|phase|iteration` and the stored row carried a
    // timestamp — so the instant a phase completed its row was removed and a
    // different one inserted: the entrance animation replayed, the row
    // relocated from the end of the list into its chronological slot, and its
    // expanded state was lost with the key it was filed under.
    const live = fromLiveCall(liveCall(), "PM");
    const done = fromPhaseEvent(phaseEvent());
    expect(done).not.toBeNull();
    expect(live.key).toBe(done!.key);
    expect(live.key).toBe(phaseKey("t1", "execute", 1));
  });

  test("the DURABLE record wins a key collision", () => {
    // It is the complete one. A live call lingering in the projection after
    // its event has landed would otherwise re-blank the fields only the event
    // carries — the decision, the notes, the verbatim system prompt.
    const live = fromLiveCall(liveCall({ response: "partial" }), "PM");
    const done = fromPhaseEvent(phaseEvent({ decision: "done", response: "final" }))!;
    const merged = mergePhases([done], [live]);
    expect(merged).toHaveLength(1);
    expect(merged[0]?.decision).toBe("done");
    expect(merged[0]?.response).toBe("final");
    expect(merged[0]?.live).toBe(false);
  });
});

describe("ordering", () => {
  test("timestamps are compared as INSTANTS, never as strings", () => {
    // Go trims trailing zeros from RFC3339Nano, so `…:07Z` sorts before
    // `…:07.42Z` on a raw string compare — it compares 'Z' (0x5A) against '.'
    // (0x2E) and orders the LATER instant first. Store rows are additionally
    // microsecond-truncated while live rows keep nanoseconds.
    const earlier = fromPhaseEvent(phaseEvent({ phase: "plan" }, "2026-01-01T00:00:07Z"))!;
    const later = fromPhaseEvent(phaseEvent({ phase: "review" }, "2026-01-01T00:00:07.42Z"))!;
    const merged = mergePhases([earlier, later], []);
    expect(merged.map((r) => r.phase)).toEqual(["review", "plan"]);
  });

  test("the comparator is transitive and returns 0 for equal rows", () => {
    // `(a, b) => a.lastTs < b.lastTs ? 1 : -1` returns -1 for equal operands,
    // so cmp(a,b) === cmp(b,a) === -1 and equal rows genuinely trade places
    // between renders under V8's TimSort.
    const a = fromPhaseEvent(phaseEvent({ phase: "plan", iteration: 1 }, "2026-01-01T00:00:07Z"))!;
    const b = fromPhaseEvent(phaseEvent({ phase: "plan", iteration: 1 }, "2026-01-01T00:00:07Z"))!;
    const once = mergePhases([a, b], []).map((r) => r.key);
    const twice = mergePhases([b, a], []).map((r) => r.key);
    expect(once).toEqual(twice);
  });

  test("a turn is read FORWARDS while the list of turns is newest first", () => {
    const plan = fromPhaseEvent(phaseEvent({ phase: "plan" }, "2026-01-01T00:00:01Z"))!;
    const exec = fromPhaseEvent(phaseEvent({ phase: "execute" }, "2026-01-01T00:00:02Z"))!;
    const review = fromPhaseEvent(phaseEvent({ phase: "review" }, "2026-01-01T00:00:03Z"))!;
    const older = fromPhaseEvent(
      phaseEvent({ turn_id: "t0", phase: "plan" }, "2025-12-31T00:00:00Z"),
    )!;

    const groups = groupTurns([review, plan, exec, older]);
    expect(groups.map((g) => g.turnId)).toEqual(["t1", "t0"]);
    expect(groups[0]?.phases.map((p) => p.phase)).toEqual(["plan", "execute", "review"]);
  });

  test("a turn group reports live and failed from its phases", () => {
    const live = fromLiveCall(liveCall({ phase: "review", iteration: 2 }), "PM");
    const failed = fromPhaseEvent(phaseEvent({ phase: "execute", failed: true }))!;
    const [group] = groupTurns([live, failed]);
    expect(group?.live).toBe(true);
    expect(group?.failed).toBe(true);
  });
});

describe("the round ledger", () => {
  test("tool calls group by their OWN round, which only appends", () => {
    // The previous surface distributed tool badges across inter-paragraph
    // slots with `floor(j * slots / tools.length)`. Both the divisor and the
    // slot count grow every round, so every earlier badge was re-placed each
    // time a new tool ran — a badge physically moved from one paragraph to
    // another while the reader was looking at it. `round` was on the wire the
    // whole time and never read.
    const calls = toolCalls([
      { name: "search", round: 0, arguments: "{}", result: "ok", success: true },
      { name: "read", round: 1, arguments: "{}", result: "ok", success: true },
      { name: "write", round: 1, arguments: "{}", result: "", success: false, error: "boom" },
    ]);
    const ledger = rounds(calls);
    expect(ledger.map((r) => r.round)).toEqual([0, 1]);
    expect(ledger[1]?.tools.map((t) => t.name)).toEqual(["read", "write"]);
    expect(ledger[1]?.tools[1]?.failed).toBe(true);
  });

  test("adding a round never moves an earlier one", () => {
    const before = rounds(
      toolCalls([
        { name: "a", round: 0 },
        { name: "b", round: 1 },
      ]),
    );
    const after = rounds(
      toolCalls([
        { name: "a", round: 0 },
        { name: "b", round: 1 },
        { name: "c", round: 2 },
      ]),
    );
    expect(after.slice(0, 2)).toEqual(before);
  });

  test("a producer that never set a round still gets a stable ledger", () => {
    // The array's own order is the sequence, and it only appends. ONE-BASED,
    // matching the engine's own `round` (which is `roundsUsed`) — numbering a
    // fallback from 0 would put two producers on different scales in one list.
    const ledger = rounds(toolCalls([{ name: "a" }, { name: "b" }]));
    expect(ledger.map((r) => r.round)).toEqual([1, 2]);
  });

  test("a failure is read from any of the three ways the engine spells it", () => {
    expect(toolCalls([{ name: "a", success: false }])[0]?.failed).toBe(true);
    expect(toolCalls([{ name: "a", failed: true }])[0]?.failed).toBe(true);
    expect(toolCalls([{ name: "a", error: "boom" }])[0]?.failed).toBe(true);
    expect(toolCalls([{ name: "a", success: true }])[0]?.failed).toBe(false);
  });
});

describe("presentation rules", () => {
  test("reasoning is split off the front of the answer", () => {
    // The engine keeps a phase's reasoning as a <think> prefix of Response,
    // so this is a documented shape rather than a guess.
    const { thinking, answer } = splitThinking("<think>weighing it up</think>\nShipped it.");
    expect(thinking).toBe("weighing it up");
    expect(answer.trim()).toBe("Shipped it.");
  });

  test("a response with no reasoning is left alone", () => {
    const { thinking, answer } = splitThinking("Shipped it.");
    expect(thinking).toBe("");
    expect(answer).toBe("Shipped it.");
  });

  test("a decision is rendered as what it MEANS", () => {
    // `plan`, `direct`, `skip`, `done` and `self_iterate` were on the wire and
    // rendered nowhere, so the single most useful fact about a phase — what it
    // decided — was invisible.
    expect(decisionLabel("plan", "direct")).toBe("answered directly, no plan needed");
    expect(decisionLabel("review", "self_iterate")).toBe("sent the turn back to Plan");
    expect(decisionLabel("execute", "")).toBe("");
  });

  test("a payload-free event yields no record rather than a blank one", () => {
    const record: PhaseRecord | null = fromPhaseEvent({
      ...phaseEvent(),
      payload: undefined,
    } as never);
    expect(record).toBeNull();
  });
});

describe("narration is kept beside the round that produced it", () => {
  test("a round's thinking, speech and calls land in one block", () => {
    // The bug this replaces: `response` is the JOIN of every round's turn,
    // and a join cannot be undone — the parts are separated by a blank line
    // and prose contains blank lines. Splitting it on the leading <think>
    // tag showed round 1's thinking as "the reasoning" and every later
    // round's thinking as "the model output", tags and all.
    const ledger = rounds(
      toolCalls([
        { name: "search", round: 1 },
        { name: "read", round: 2 },
      ]),
      narrations([
        { round: 1, reasoning: "which tool?", content: "looking it up" },
        { round: 2, reasoning: "now I know", content: "done" },
      ]),
    );
    expect(ledger.map((r) => r.round)).toEqual([1, 2]);
    expect(ledger[0]).toMatchObject({ reasoning: "which tool?", content: "looking it up" });
    expect(ledger[0]!.tools.map((t) => t.name)).toEqual(["search"]);
    expect(ledger[1]).toMatchObject({ reasoning: "now I know", content: "done" });
  });

  test("the final round has no tools and still gets a block", () => {
    // It is the round holding the answer. Grouping on tool calls alone drops
    // it entirely, which is how the answer went missing from the ledger.
    const ledger = rounds(
      toolCalls([{ name: "search", round: 1 }]),
      narrations([{ round: 2, content: "here is what I found" }]),
    );
    expect(ledger.map((r) => r.round)).toEqual([1, 2]);
    expect(ledger[1]!.content).toBe("here is what I found");
    expect(ledger[1]!.tools).toEqual([]);
  });

  test("a round that only called tools narrates nothing", () => {
    const ledger = rounds(
      toolCalls([{ name: "search", round: 1 }]),
      narrations([{ round: 1, reasoning: "   ", content: "" }]),
    );
    expect(ledger[0]!.reasoning).toBe("");
    expect(ledger[0]!.content).toBe("");
  });

  test("rounds only ever append, so nothing above an insertion moves", () => {
    const before = rounds(
      toolCalls([{ name: "a", round: 1 }]),
      narrations([{ round: 1, content: "first" }]),
    );
    const after = rounds(
      toolCalls([
        { name: "a", round: 1 },
        { name: "b", round: 2 },
      ]),
      narrations([
        { round: 1, content: "first" },
        { round: 2, content: "second" },
      ]),
    );
    expect(after[0]).toEqual(before[0]);
  });
});

describe("a phase recorded before narration existed still renders", () => {
  // Those events are already in the store, and an applied write is history
  // rather than source: they have to keep rendering.
  const legacyRecord = {
    tools: toolCalls([{ name: "search", round: 1 }]),
    narration: [],
    response: "<think>pondering</think>\nthe answer",
  };

  test("the joined response is shown whole rather than guessed apart", () => {
    const { ledger, legacy } = ledgerOf(legacyRecord);
    expect(ledger.map((r) => r.round)).toEqual([1]);
    expect(legacy).toEqual({ thinking: "pondering", answer: "the answer" });
  });

  test("narration, when present, wins outright", () => {
    const { legacy } = ledgerOf({
      ...legacyRecord,
      narration: narrations([{ round: 1, content: "proper" }]),
    });
    expect(legacy).toBeNull();
  });

  test("a phase with neither offers no empty transcript block", () => {
    expect(ledgerOf({ tools: [], narration: [], response: "" }).legacy).toBeNull();
  });
});

describe("a round being written is not a round that is finished", () => {
  test("the partial becomes the newest round, marked streaming", () => {
    const ledger = rounds(
      toolCalls([{ name: "search", round: 1 }]),
      narrations([{ round: 1, content: "looked it up" }]),
      { round: 2, reasoning: "still think", content: "half a sen" },
    );
    expect(ledger.map((r) => r.round)).toEqual([1, 2]);
    expect(ledger[0]!.streaming).toBe(false);
    expect(ledger[1]).toMatchObject({ streaming: true, content: "half a sen" });
  });

  test("an abandoned attempt is kept beside the retry, not erased", () => {
    // A reader has already seen that text; making it vanish reads as a
    // glitch, and "this model wrote some of an answer then died" is the
    // useful fact about a flaky provider.
    const ledger = rounds([], [], {
      round: 1,
      content: "second try",
      abandoned: [{ round: 1, content: "first try died here" }],
    });
    expect(ledger[0]!.content).toBe("second try");
    expect(ledger[0]!.abandoned.map((a) => a.content)).toEqual(["first try died here"]);
  });

  test("a live phase with only a partial is not treated as a legacy record", () => {
    // Otherwise the joined `response` fallback would render alongside it and
    // the same words would appear twice.
    const { ledger, legacy } = ledgerOf({
      tools: [],
      narration: [],
      partial: { round: 1, content: "writing" },
      response: "writing",
    });
    expect(legacy).toBeNull();
    expect(ledger).toHaveLength(1);
  });

  test("a finished phase has no partial at all", () => {
    const { ledger } = ledgerOf({
      tools: toolCalls([{ name: "a", round: 1 }]),
      narration: narrations([{ round: 1, content: "done" }]),
      partial: null,
      response: "done",
    });
    expect(ledger.every((r) => !r.streaming)).toBe(true);
  });
});
