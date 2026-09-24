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
  decisionTone,
  attempts,
  groupTurns,
  mergePhases,
  phaseKey,
  ledgerOf,
  narrations,
  phaseDuration,
  phaseStart,
  rounds,
  splitThinking,
  streamedPhases,
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

  test("between two DURABLE records on one key the NEWER wins", () => {
    // THE SHAPE THAT HID A WHOLE RETRY. A turn id used to be the work key,
    // which a redelivery reproduces by design — so when a turn failed on auth
    // and the broker redelivered it, the retry's `execute` record carried the
    // identity the failed attempt already had. Both were real, different
    // phases. This merge kept whichever the mount-time query applied last,
    // which is the OLDEST, so the screen showed the dead attempt's `auth`
    // failure for the whole time the retry was running.
    //
    // Run ids are unique per execution now (adr/0017), so this collision
    // should not recur. The rule stays because a merge that silently prefers
    // stale data on a key it cannot prove unique is what made it invisible.
    const older = fromPhaseEvent(
      phaseEvent({ failed: true, error_kind: "auth", response: "" }, "2026-01-01T00:00:09Z"),
    )!;
    const newer = fromPhaseEvent(
      phaseEvent({ response: "done at last", total_tokens: 327000 }, "2026-01-01T00:02:00Z"),
    )!;
    // Both orders, because the caller's order is exactly what used to decide it.
    for (const pair of [
      [newer, older],
      [older, newer],
    ]) {
      const merged = mergePhases(pair, []);
      expect(merged).toHaveLength(1);
      expect(merged[0]?.response).toBe("done at last");
    }
  });

  test("a phase record carries the unit of work beside the run", () => {
    const done = fromPhaseEvent(phaseEvent({ work_key: "wk-7" }))!;
    expect(done.turnId).toBe("t1");
    expect(done.workKey).toBe("wk-7");
    // Absent on a record an engine from before the split wrote, and EMPTY
    // rather than undefined so nothing downstream has to test for two
    // absences.
    expect(fromPhaseEvent(phaseEvent())!.workKey).toBe("");
  });
});

describe("attempts at one trigger", () => {
  function group(turnId: string, workKey: string, at: string) {
    return groupTurns([
      fromPhaseEvent(phaseEvent({ turn_id: turnId, work_key: workKey }, at))!,
    ])[0]!;
  }

  test("a turn group carries the work key its phases name", () => {
    expect(group("run-1", "wk-1", "2026-01-01T00:00:09Z").workKey).toBe("wk-1");
  });

  test("two runs of one trigger are numbered oldest first", () => {
    const first = group("run-1", "wk-1", "2026-01-01T00:00:09Z");
    const second = group("run-2", "wk-1", "2026-01-01T00:05:00Z");
    // Newest first, which is the order the screens hold them in — the
    // numbering must not follow it.
    const got = attempts([second, first]);
    expect(got.get("run-1")).toEqual({ index: 1, total: 2 });
    expect(got.get("run-2")).toEqual({ index: 2, total: 2 });
  });

  test("a turn that ran once is not an attempt, and an empty key is not a group", () => {
    // A LONE RUN HAS NOTHING TO DISAMBIGUATE, so tagging it "attempt 1/1"
    // would put a re-run marker on every ordinary turn on the screen.
    expect(attempts([group("run-1", "wk-1", "2026-01-01T00:00:09Z")]).size).toBe(0);
    // And an empty work key is the ABSENCE of an identity — a trigger with
    // nothing to collapse on. Grouping on it would report every unledgered
    // turn as an attempt at every other.
    const a = group("run-1", "", "2026-01-01T00:00:09Z");
    const b = group("run-2", "", "2026-01-01T00:05:00Z");
    expect(attempts([a, b]).size).toBe(0);
  });
});

describe("ordering", () => {
  test("timestamps are compared as INSTANTS, never as strings", () => {
    // Go trims trailing zeros from RFC3339Nano, so `…:07Z` sorts before
    // `…:07.42Z` on a raw string compare — it compares 'Z' (0x5A) against '.'
    // (0x2E) and orders the LATER instant first. Store rows are additionally
    // microsecond-truncated while live rows keep nanoseconds.
    const earlier = fromPhaseEvent(phaseEvent({ phase: "execute" }, "2026-01-01T00:00:07Z"))!;
    const later = fromPhaseEvent(phaseEvent({ phase: "review" }, "2026-01-01T00:00:07.42Z"))!;
    const merged = mergePhases([earlier, later], []);
    expect(merged.map((r) => r.phase)).toEqual(["review", "execute"]);
  });

  test("the comparator is transitive and returns 0 for equal rows", () => {
    // `(a, b) => a.lastTs < b.lastTs ? 1 : -1` returns -1 for equal operands,
    // so cmp(a,b) === cmp(b,a) === -1 and equal rows genuinely trade places
    // between renders under V8's TimSort.
    const a = fromPhaseEvent(
      phaseEvent({ phase: "execute", iteration: 1 }, "2026-01-01T00:00:07Z"),
    )!;
    const b = fromPhaseEvent(
      phaseEvent({ phase: "execute", iteration: 1 }, "2026-01-01T00:00:07Z"),
    )!;
    const once = mergePhases([a, b], []).map((r) => r.key);
    const twice = mergePhases([b, a], []).map((r) => r.key);
    expect(once).toEqual(twice);
  });

  test("a turn is read FORWARDS while the list of turns is newest first", () => {
    const onboarding = fromPhaseEvent(phaseEvent({ phase: "onboarding" }, "2026-01-01T00:00:01Z"))!;
    const exec = fromPhaseEvent(phaseEvent({ phase: "execute" }, "2026-01-01T00:00:02Z"))!;
    const review = fromPhaseEvent(phaseEvent({ phase: "review" }, "2026-01-01T00:00:03Z"))!;
    const older = fromPhaseEvent(
      phaseEvent({ turn_id: "t0", phase: "execute" }, "2025-12-31T00:00:00Z"),
    )!;

    const groups = groupTurns([review, onboarding, exec, older]);
    expect(groups.map((g) => g.turnId)).toEqual(["t1", "t0"]);
    expect(groups[0]?.phases.map((p) => p.phase)).toEqual(["onboarding", "execute", "review"]);
  });

  test("a turn group reports live and failed from its phases", () => {
    const live = fromLiveCall(liveCall({ phase: "review", iteration: 2 }), "PM");
    const failed = fromPhaseEvent(phaseEvent({ phase: "execute", failed: true }))!;
    const [group] = groupTurns([live, failed]);
    expect(group?.live).toBe(true);
    expect(group?.failed).toBe(true);
  });

  // A TURN'S SPAN COVERS ITS FIRST PHASE, not the gap between two LANDINGS.
  // Both `at` values are completion instants, so subtracting them drops the
  // opening phase's own length entirely: the turn card printed its review's
  // duration as the whole turn's, above a phase card showing three minutes.
  test("a turn's span covers its first phase, not the gap between landings", () => {
    const exec = fromPhaseEvent(
      phaseEvent({ phase: "execute", duration_ms: 180_000 }, "2026-01-01T10:03:00Z"),
    )!;
    const review = fromPhaseEvent(
      phaseEvent({ phase: "review", duration_ms: 60_000 }, "2026-01-01T10:04:00Z"),
    )!;
    const [group] = groupTurns([exec, review]);
    expect(group?.span).toBe(240_000);
    // THE INVARIANT THE SCREENSHOT BROKE: a turn is never shorter than the
    // longest phase inside it.
    expect(group!.span!).toBeGreaterThanOrEqual(
      Math.max(...group!.phases.map((p) => p.durationMs)),
    );
  });

  // AND ITS ITERATION COUNT IS ITS OWN PHASES', which is the same quantity
  // `store.Turns` reports as `MAX(iteration)` — so the turns table above the
  // cards and the card itself state one number. A worker's iteration belongs to
  // the delegate call that spawned it, not to this turn.
  test("a turn's iteration count is the highest its OWN phases reached", () => {
    const exec = fromPhaseEvent(phaseEvent({ phase: "execute", iteration: 2 }))!;
    const worker = fromPhaseEvent(
      phaseEvent({ phase: "subagent", iteration: 7, host_phase: "execute", host_iteration: 2 }),
    )!;
    const [group] = groupTurns([exec, worker]);
    expect(group?.iterations).toBe(2);
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

  test("a call the engine timed and attributed is read as it was written", () => {
    // The row the tool loop and the surface now write: when, how long, and
    // who answered.
    const [timed] = toolCalls([
      {
        name: "create_issue",
        round: 2,
        started_at: "2026-09-24T10:00:01.5Z",
        duration_ms: 2300,
        origin: "mcp:github",
        server: "github",
      },
    ]);
    expect(timed).toMatchObject({ durationMs: 2300, origin: "mcp:github", server: "github" });
  });

  test("a row nothing timed reads as not recorded, never as instant", () => {
    const [untimed] = toolCalls([{ name: "run_sandbox", round: 1 }]);
    expect(untimed).toMatchObject({ durationMs: 0, origin: "", server: "" });
  });
});

describe("a round that reached nobody", () => {
  test("empty_answer_rounds survives onto the record", () => {
    // The count is the ONLY surviving signal for a model that answers with
    // nothing: the engine used to fail the provider call on that round, which
    // walked the fallback chain and could end the turn as llm_unavailable —
    // a red event. It now returns an empty answer and re-asks once, so
    // without this number a seat whose model never speaks looks like a seat
    // that merely gets rescued a lot.
    const record = fromPhaseEvent(phaseEvent({ empty_answer_rounds: 2 }))!;
    expect(record.emptyAnswerRounds).toBe(2);
  });

  test("a phase recorded before the field existed reads as zero, not NaN", () => {
    // The envelope evolves additive-only and a rolling upgrade replays rows
    // written by a build that had no such field.
    expect(fromPhaseEvent(phaseEvent())!.emptyAnswerRounds).toBe(0);
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
    // The outcome and the review decision are on the wire and rendered
    // nowhere else, so the single most useful fact about a phase — what it
    // decided — would otherwise be invisible.
    expect(decisionLabel("execute", "delivered")).toBe("delivered the work");
    expect(decisionLabel("review", "self_iterate")).toBe("sent the turn back for another round");
    expect(decisionLabel("execute", "")).toBe("");
  });

  test("an engine-written outcome is not dressed up as the model's own word", () => {
    // `incomplete` is the one outcome the executor never says: the engine
    // writes it when nothing was submitted at all. Rendered as a peer of
    // `delivered`, it would read as a commitment the model never made — and
    // every rescue path in the turn engine turns on telling those apart.
    expect(decisionLabel("execute", "incomplete")).toContain("the engine marked it");
    expect(decisionLabel("execute", "delivered")).not.toContain("engine");
  });

  test("a decision this build does not know still renders as itself", () => {
    // Store rows outlive the bundle that reads them: the retired plan phase's
    // verdicts are in every event log written before the redesign, and a
    // label that dropped them would blank the one column explaining the row.
    expect(decisionLabel("plan", "direct")).toBe("direct");
    expect(decisionLabel("execute", "teleported")).toBe("teleported");
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

describe("delegated workers", () => {
  // EIGHT WORKERS IN ONE ROUND SHARE turn|phase|iteration. Without the task
  // id in the key the map keeps the last one to arrive, and seven workers —
  // their prompts, their tools, their failures — are simply not on the page.
  test("every worker of one call keeps its own row", () => {
    const call = [1, 2, 3].map((n) =>
      fromPhaseEvent(
        phaseEvent({
          phase: "subagent",
          iteration: 1,
          task_id: `task-${n}`,
          worker: "researcher",
          host_phase: "execute",
          host_iteration: 1,
        }),
      ),
    );
    const keys = new Set(call.map((r) => r!.key));
    expect(keys.size).toBe(3);
  });

  // A NESTED CALL BELONGS UNDER THE PHASE THAT MADE IT. host_phase and
  // host_iteration have always been on the wire and nothing read them, so a
  // fan-out rendered as siblings of the turn's own phases and the reader had
  // to work out which round each belonged to.
  test("workers hang off their host phase, not beside it", () => {
    const exec = fromPhaseEvent(
      phaseEvent({ phase: "execute", iteration: 1 }, "2026-01-01T00:00:02Z"),
    )!;
    const review = fromPhaseEvent(
      phaseEvent({ phase: "review", iteration: 1 }, "2026-01-01T00:00:09Z"),
    )!;
    const workers = ["a", "b"].map((id) =>
      fromPhaseEvent(
        phaseEvent(
          {
            phase: "subagent",
            iteration: 1,
            task_id: id,
            host_phase: "execute",
            host_iteration: 1,
          },
          "2026-01-01T00:00:05Z",
        ),
      )!,
    );

    const [group] = groupTurns([exec, review, ...workers]);
    // The turn's OWN phases are the two it ran.
    expect(group?.phases.map((p) => p.phase)).toEqual(["execute", "review"]);
    const nested = group?.nested.get(phaseKey("t1", "execute", 1)) ?? [];
    expect(nested.map((p) => p.taskId)).toEqual(["a", "b"]);
    // And nothing hangs off the phase that made no calls.
    expect(group?.nested.get(phaseKey("t1", "review", 1))).toBeUndefined();
  });

  // A TURN'S OWN PHASES KEEP THE THREE-PART KEY they have always had, so a
  // live call and its completed event still resolve to one row.
  test("a phase with no task id keeps its original identity", () => {
    expect(phaseKey("t1", "execute", 1)).toBe("t1|execute|1");
    expect(phaseKey("t1", "subagent", 1, "gather")).toBe("t1|subagent|1|gather");
    const live = fromLiveCall(liveCall({ phase: "execute", iteration: 1 }), "PM");
    const done = fromPhaseEvent(phaseEvent({ phase: "execute", iteration: 1 }))!;
    expect(live.key).toBe(done.key);
  });
});

describe("the phases that finish while a tab is watching", () => {
  // THE TURN THAT DISAPPEARED. A screen reads two sources: a query answered
  // once at mount, and the seat's live overlay. Neither covers a phase that
  // completes while the reader is looking at it — the projection clears
  // `live_call` the moment it lands — so the phase, and with it the whole
  // turn, simply went away. The durable record is on the wire; this is what
  // reads it.

  test("a streamed record replaces the live call it closes, in place", () => {
    const live = fromLiveCall(liveCall({ phase: "review", iteration: 1 }), "PM");
    const streamed = streamedPhases(
      [phaseEvent({ phase: "review", iteration: 1, decision: "done" })],
      () => true,
    );
    const merged = mergePhases(streamed, [live]);
    // ONE row, not two, and it is the finished one: same key, so the card the
    // reader was watching becomes the finished card rather than being removed
    // and replaced.
    expect(merged).toHaveLength(1);
    expect(merged[0]?.key).toBe(live.key);
    expect(merged[0]?.live).toBe(false);
    expect(merged[0]?.decision).toBe("done");
  });

  test("a turn survives its own last phase completing", () => {
    // The seat page's exact sequence on a FIRST turn, where the mount-time
    // history is empty: onboarding and execute have landed on the stream, the
    // review is live, then the review lands too and the overlay clears.
    const events = [
      phaseEvent({ phase: "onboarding", iteration: 0 }, "2026-01-01T00:00:01Z"),
      phaseEvent({ phase: "execute", iteration: 1 }, "2026-01-01T00:00:05Z"),
    ];
    const liveReview = fromLiveCall(liveCall({ phase: "review", iteration: 1 }), "PM");

    const during = groupTurns(
      mergePhases(
        streamedPhases(events, () => true),
        [liveReview],
      ),
    );
    expect(during).toHaveLength(1);
    expect(during[0]?.live).toBe(true);
    expect(during[0]?.phases.map((p) => p.phase)).toEqual(["onboarding", "execute", "review"]);

    const after = groupTurns(
      mergePhases(
        streamedPhases(
          [...events, phaseEvent({ phase: "review", iteration: 1 }, "2026-01-01T00:00:09Z")],
          () => true,
        ),
        // The overlay has been cleared: this is the render that used to leave
        // the page empty.
        [],
      ),
    );
    // The turn is STILL THERE, with every phase it ran, and is no longer live.
    expect(after).toHaveLength(1);
    expect(after[0]?.live).toBe(false);
    expect(after[0]?.phases.map((p) => p.phase)).toEqual(["onboarding", "execute", "review"]);
  });

  test("the scope filter reads the record, not the envelope", () => {
    // A seat page keeps its own seat's phases. The role is on the PAYLOAD; an
    // envelope's `actor` agrees today and is not the field a phase record is
    // built from.
    const events = [
      phaseEvent({ role: "PM" }),
      phaseEvent({ role: "Engineer" }, "2026-01-01T00:00:10Z"),
    ];
    expect(streamedPhases(events, (r) => r.role === "PM").map((r) => r.role)).toEqual(["PM"]);
  });

  test("a row with no payload is dropped rather than rendered blank", () => {
    const bare = { ...phaseEvent(), payload: undefined };
    expect(streamedPhases([bare], () => true)).toEqual([]);
  });
});

describe("a phase's duration", () => {
  // ON THE RECORD, not reconstructed from a second event. This used to fold
  // `agent_phase_started` onto the finished record and subtract, which needs
  // BOTH events in one reader's hands — and the screen it exists for, a turn
  // deep-linked WHILE IT RUNS, has exactly none of them: its `turn` query was
  // answered before the phase started, and the only envelopes buffered after
  // that are completed ones.

  test("a completed phase reports what the engine measured", () => {
    const done = fromPhaseEvent(phaseEvent({ duration_ms: 100_000 }, "2026-01-01T00:01:40Z"))!;
    expect(phaseDuration(done)).toBe(100_000);
    // …and the instant it BEGAN is the landing less the measurement, which is
    // what opens the Turn screen's window on a turn still running.
    expect(phaseStart(done)).toBe(Date.parse("2026-01-01T00:00:00Z"));
  });

  test("a NESTED call reports one too, which the pairing could never give it", () => {
    // A delegate's worker and the round-cap judge publish no
    // `agent_phase_started` — they nest under a host phase that is already
    // showing one — so no worker of a fan-out of eight had a duration
    // anywhere, and "which one was slow" had no answer on any screen.
    const worker = fromPhaseEvent(
      phaseEvent({
        phase: "subagent",
        host_phase: "execute",
        host_iteration: 1,
        duration_ms: 12_000,
      }),
    )!;
    expect(phaseDuration(worker)).toBe(12_000);
  });

  test("an unmeasured phase reports nothing rather than zero", () => {
    // An agent-mode executor's rounds ran inside a coding CLI's own loop in
    // another process, so the engine measured no loop of its own and the
    // field is 0. Rendering that as "0ms" would put a confident number on a
    // phase that took four minutes.
    const done = fromPhaseEvent(phaseEvent())!;
    expect(done.durationMs).toBe(0);
    expect(phaseDuration(done)).toBeNull();
    // The start then degrades to the landing instant rather than to the
    // epoch: a window opening in 1970 is worse than one opening late.
    expect(phaseStart(done)).toBe(Date.parse("2026-01-01T00:00:09Z"));
  });

  test("a live phase has an elapsed time, not a duration", () => {
    // The overlay's `started_at` is what keeps a running phase's counter
    // correct; the duration is the engine's FINAL measurement and does not
    // exist until the phase lands.
    const live = fromLiveCall(liveCall({ started_at: "2026-01-01T00:00:30Z" }), "PM");
    expect(phaseDuration(live)).toBeNull();
    expect(phaseStart(live)).toBe(Date.parse("2026-01-01T00:00:30Z"));
  });

  test("a negative measurement is refused rather than rendered", () => {
    // Nothing the engine publishes should produce one — it measures with a
    // single monotonic read — but a payload is a wire value and a negative
    // duration would render as a phase that finished before it began.
    const done = fromPhaseEvent(phaseEvent({ duration_ms: -5 }))!;
    expect(phaseDuration(done)).toBeNull();
  });

  test("an unreadable landing instant yields no start at all", () => {
    // `tsKey` answers 0 for what it cannot parse, and 0 is the epoch. The
    // caller filters zeroes; handing it `0 - duration_ms` would hand it a
    // NEGATIVE instant that passes no filter written for a missing one.
    const done = fromPhaseEvent(phaseEvent({ duration_ms: 1000 }, "not a timestamp"))!;
    expect(phaseStart(done)).toBe(0);
  });
});

// A DECISION CARRIES ITS OWN TONE.
//
// The phase card's tone was an inline `=== "self_iterate" ? warning : neutral`
// at its own call site, so the three outcomes a reader has to act on drew the
// same grey as the one that needs nothing.
describe("a decision carries its own tone", () => {
  test("the three outcomes a reader must act on are not the ordinary hue", () => {
    // `blocked` is the executor reporting it could not do the work;
    // `incomplete` is the engine saying nothing was submitted at all; and a
    // reviewer's `failed` is the one nothing else on a phase card draws red,
    // because a review record never sets the phase's own `failed` flag.
    expect(decisionTone("execute", "blocked")).toBe("caution");
    expect(decisionTone("execute", "incomplete")).toBe("caution");
    expect(decisionTone("review", "failed")).toBe("critical");
  });

  test("the ordinary end of a turn is not a caution", () => {
    // Four status hues spent on every row is four hues spent on none: a turn
    // that delivered and a turn nobody asked anything of are what a seat's feed
    // is mostly made of.
    expect(decisionTone("execute", "no_action")).toBe("neutral");
    expect(decisionTone("execute", "delivered")).toBe("positive");
    expect(decisionTone("review", "done")).toBe("positive");
    expect(decisionTone("review", "self_iterate")).toBe("caution");
  });

  test("a decision this build cannot read takes no hue", () => {
    // A row written by a build this bundle predates still renders, and a hue it
    // was never given is not invented for it. `subagent` is deliberately in
    // here: every status but `ok` already sets the record's `failed` flag and
    // draws a danger pill, so a second one beside it reports one stop twice.
    expect(decisionTone("plan", "direct")).toBe("neutral");
    expect(decisionTone("subagent", "timed_out")).toBe("neutral");
    expect(decisionTone("execute", "")).toBe("neutral");
    expect(decisionTone("", "blocked")).toBe("neutral");
  });

  test("a phase reaches the table whatever its case", () => {
    // A phase value is a column in the event store and nothing normalises its
    // case on the way out — the label lookup already lowercases, and a tone that
    // did not would lose the hue on one screen and not the next.
    expect(decisionTone("Execute", "blocked")).toBe("caution");
  });
});

describe("a pre-split phase record's unit of work", () => {
  // schema/0029 backfilled the `work_key` COLUMN from `turn_id` — which is
  // where the work key lived before ADR-0017 split the two — and deliberately
  // left the stored payloads alone: they record what that build published, and
  // it published no such field. So a parser reading `payload.work_key` alone
  // reports no unit of work for every turn older than the split, while the
  // server answers the same question off the column for all of them. One
  // authority, and it is the row's own field.
  test("comes off the row's column, which the payload does not carry", () => {
    const rec: EventRecord = { ...phaseEvent(), work_key: "wk-backfilled" };
    const done = fromPhaseEvent(rec)!;
    expect(done.workKey).toBe("wk-backfilled");
  });

  // AND A LIVE FRAME STILL WORKS: a phase event pushed on the socket carries
  // its work key in the payload and no promoted column, because nothing has
  // stored it yet.
  test("falls back to the payload for a frame nothing has stored", () => {
    const done = fromPhaseEvent(phaseEvent({ work_key: "wk-live" }))!;
    expect(done.workKey).toBe("wk-live");
  });
});
