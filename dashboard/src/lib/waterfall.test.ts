import { describe, expect, test } from "vitest";

import type { EventRecord } from "~/protocol/index.ts";
import { phaseRecord, toolCall } from "~/test/phaseRecord.ts";
import { fmtDuration } from "./format.ts";
import { buildWaterfall, tickLabel, ticks, type Span } from "./waterfall.ts";

const T0 = Date.parse("2026-09-28T10:00:00Z");
const iso = (ms: number) => new Date(T0 + ms).toISOString();

function event(type: string, ms: number, payload: Record<string, unknown> = {}): EventRecord {
  return {
    id: `${type}-${ms}`,
    type,
    timestamp: iso(ms),
    source: "SWE",
    actor: "SWE",
    summary: "",
    category: "lifecycle",
    trace_id: "",
    span_id: "",
    parent_span_id: "",
    topic: "",
    payload: { turn_id: "turn-1", ...payload },
  };
}

const byId = (spans: Span[], id: string) => {
  const found = spans.find((s) => s.id === id);
  if (!found) throw new Error(`no span ${id} in ${spans.map((s) => s.id).join(", ")}`);
  return found;
};

/** An executor with two timed rounds and a reviewer after it. */
function settledTurn() {
  const execute = phaseRecord({
    key: "turn-1|execute|1",
    clockStart: iso(1_000),
    durationMs: 9_000,
    at: iso(10_000),
    timedRounds: [
      {
        round: 1,
        startedAt: iso(1_000),
        durationMs: 2_000,
        model: "m",
        inputTokens: 0,
        outputTokens: 0,
        cacheReadTokens: 0,
        toolCalls: 2,
      },
      {
        round: 2,
        startedAt: iso(6_000),
        durationMs: 3_000,
        model: "m",
        inputTokens: 0,
        outputTokens: 0,
        cacheReadTokens: 0,
        toolCalls: 0,
      },
    ],
    tools: [
      // One stamped by the engine, one only timed: the second runs after the
      // first, from the model's answer.
      toolCall({ name: "search", round: 1, startedAt: iso(3_000), durationMs: 1_000 }),
      toolCall({ name: "read", round: 1, durationMs: 500 }),
    ],
  });
  const review = phaseRecord({
    key: "turn-1|review|1",
    phase: "review",
    clockStart: iso(10_000),
    durationMs: 1_000,
    at: iso(11_000),
  });
  const events = [
    event("agent_turn_started", 0, { started_at: iso(0) }),
    event("prefetch_summary", 100, { started_at: iso(100), duration_ms: 400 }),
    event("turn_completed", 11_100, { started_at: iso(0), duration_ms: 11_100 }),
  ];
  return { events, phases: [execute, review] };
}

describe("buildWaterfall", () => {
  test("nests rounds under their phase and tools under their round, in reading order", () => {
    const { events, phases } = settledTurn();
    const w = buildWaterfall({ events, phases, now: T0 + 60_000, running: false, parked: false });
    expect(w.spans.map((s) => `${s.depth}:${s.kind}:${s.label}`)).toEqual([
      "0:turn:Turn",
      "1:context:Context",
      "1:phase:Execute",
      "2:model:Round 1",
      "3:tool:search",
      "3:tool:read",
      "2:model:Round 2",
      "1:phase:Review",
    ]);
    expect(w.from).toBe(T0);
    // The turn's own measurement closes it, past its last phase.
    expect(w.to).toBe(T0 + 11_100);
    expect(w.open).toBe(false);
  });

  test("a tool the engine stamped sits at its stamp; one it only timed runs after the one before it", () => {
    const { events, phases } = settledTurn();
    const w = buildWaterfall({ events, phases, now: T0, running: false, parked: false });
    const search = byId(w.spans, "turn-1|execute|1.r1.t0");
    const read = byId(w.spans, "turn-1|execute|1.r1.t1");
    expect([search.start - T0, search.end - T0, search.placed]).toEqual([3_000, 4_000, false]);
    expect([read.start - T0, read.end - T0, read.placed]).toEqual([4_000, 4_500, true]);
  });

  test("with no stamp at all, a round's calls run serially from the model's answer", () => {
    const phase = phaseRecord({
      clockStart: iso(0),
      durationMs: 5_000,
      timedRounds: [
        {
          round: 1,
          startedAt: iso(0),
          durationMs: 1_000,
          model: "m",
          inputTokens: 0,
          outputTokens: 0,
          cacheReadTokens: 0,
          toolCalls: 2,
        },
      ],
      tools: [toolCall({ name: "a", durationMs: 200 }), toolCall({ name: "b", durationMs: 300 })],
    });
    const w = buildWaterfall({
      events: [],
      phases: [phase],
      now: T0,
      running: false,
      parked: false,
    });
    const [a, b] = [
      byId(w.spans, "turn-1|execute|1.r1.t0"),
      byId(w.spans, "turn-1|execute|1.r1.t1"),
    ];
    expect([a.start - T0, a.end - T0]).toEqual([1_000, 1_200]);
    expect([b.start - T0, b.end - T0]).toEqual([1_200, 1_500]);
  });

  test("a delegate's workers nest under the round that spawned them", () => {
    const { events, phases } = settledTurn();
    const worker = phaseRecord({
      key: "turn-1|subagent|1|task-a",
      phase: "subagent",
      worker: "researcher",
      hostPhase: "execute",
      hostIteration: 1,
      hostRound: 2,
      clockStart: iso(6_500),
      durationMs: 1_000,
    });
    const orphan = phaseRecord({
      key: "turn-1|subagent|1|task-b",
      phase: "subagent",
      worker: "writer",
      hostPhase: "execute",
      hostIteration: 1,
      clockStart: iso(7_000),
      durationMs: 1_000,
    });
    const w = buildWaterfall({
      events,
      phases: [...phases, worker, orphan],
      now: T0,
      running: false,
      parked: false,
    });
    const order = w.spans.map((s) => `${s.depth}:${s.label}`);
    const round2 = order.indexOf("2:Round 2");
    expect(order[round2 + 1]).toBe("3:researcher");
    // A worker whose record names no round sits under its phase, not a round.
    expect(order).toContain("2:writer");
  });

  test("a running phase, its round in flight and the call running now end at now", () => {
    const now = T0 + 20_000;
    const live = phaseRecord({
      live: true,
      clockStart: iso(1_000),
      timedRounds: [
        {
          round: 1,
          startedAt: iso(1_000),
          durationMs: 1_000,
          model: "m",
          inputTokens: 0,
          outputTokens: 0,
          cacheReadTokens: 0,
          toolCalls: 1,
        },
      ],
      roundStartedAt: iso(15_000),
      runningCall: { round: 1, name: "run_tests", arguments: "{}", startedAt: iso(2_500) },
    });
    const w = buildWaterfall({
      events: [event("agent_turn_started", 0, { started_at: iso(0) })],
      phases: [live],
      now,
      running: true,
      parked: false,
    });
    const phase = byId(w.spans, "turn-1|execute|1");
    const inFlight = byId(w.spans, "turn-1|execute|1.r2");
    const call = byId(w.spans, "turn-1|execute|1.r1.running");
    for (const s of [phase, inFlight, call, byId(w.spans, "turn")]) {
      expect([s.id, s.open, s.end]).toEqual([s.id, true, now]);
    }
    expect(w.open).toBe(true);
    expect(w.to).toBe(now);
  });

  // THE LATEST ROUND'S START IS NOT A ROUND IN FLIGHT while that round's
  // tools run: numbered "the next round" it drew round 3 from round 2's start,
  // minutes early, while round 2 was still the round on screen.
  test("a live start that belongs to a recorded round draws no round in flight", () => {
    const round = (n: number, startMs: number, durationMs: number) => ({
      round: n,
      startedAt: iso(startMs),
      durationMs,
      model: "m",
      inputTokens: 0,
      outputTokens: 0,
      cacheReadTokens: 0,
      toolCalls: 1,
    });
    const now = T0 + 237_000;
    const tools = phaseRecord({
      live: true,
      clockStart: iso(0),
      timedRounds: [round(1, 0, 90_000), round(2, 90_000, 85_000)],
      roundStartedAt: iso(90_000),
    });
    const input = { events: [], now, running: true, parked: false };
    const stale = buildWaterfall({ ...input, phases: [tools] });
    expect(stale.spans.filter((s) => s.kind === "model").map((s) => s.label)).toEqual([
      "Round 1",
      "Round 2",
    ]);
    expect(stale.spans.some((s) => s.kind === "model" && s.open)).toBe(false);

    // Once the engine announces round 3, it is drawn from ITS start.
    const opened = buildWaterfall({
      ...input,
      phases: [{ ...tools, roundStartedAt: iso(180_000) }],
    });
    const r3 = byId(opened.spans, "turn-1|execute|1.r3");
    expect([r3.label, r3.start, r3.end, r3.open]).toEqual(["Round 3", T0 + 180_000, now, true]);
  });

  test("the first round is drawn open from its own start before anything has come back", () => {
    const now = T0 + 8_000;
    const first = phaseRecord({ live: true, clockStart: iso(0), roundStartedAt: iso(40) });
    const w = buildWaterfall({ events: [], phases: [first], now, running: true, parked: false });
    const r1 = byId(w.spans, "turn-1|execute|1.r1");
    expect([r1.label, r1.start, r1.end, r1.open]).toEqual(["Round 1", T0 + 40, now, true]);
  });

  // ONE LENGTH FOR "THE TURN": the header's wall clock is the engine's own
  // measurement, and the reflection pass runs after it.
  test("the turn row ends at the turn's own measurement, with reflection trailing it", () => {
    const { events, phases } = settledTurn();
    const w = buildWaterfall({
      events: [
        ...events,
        event("agent_turn_completed", 11_100),
        event("reflection_completed", 11_500),
      ],
      phases,
      now: T0 + 60_000,
      running: false,
      parked: false,
    });
    const turn = byId(w.spans, "turn");
    const reflection = byId(w.spans, "reflection");
    expect(turn.end).toBe(T0 + 11_100);
    expect(reflection.end).toBe(T0 + 11_500);
    expect(w.to).toBe(T0 + 11_500);
  });

  test("a record with no instants is listed as untimed, never drawn at an invented place", () => {
    const legacy = phaseRecord({ key: "turn-1|execute|1", at: "", startedAt: "", durationMs: 0 });
    const w = buildWaterfall({
      events: [],
      phases: [legacy],
      now: T0,
      running: false,
      parked: false,
    });
    expect(w.spans).toEqual([]);
    expect(w.untimed.map((u) => `${u.kind}:${u.label}`)).toEqual(["phase:Execute"]);
  });

  test("a turn that parked on a coding run is one turn: its phase spans the run it waited on", () => {
    // The executor's ONE record, published after the resume, carries its
    // rounds from before the park and after it.
    const execute = phaseRecord({
      clockStart: iso(1_000),
      durationMs: 4_000,
      at: iso(62_000),
      launchId: "L1",
      timedRounds: [
        {
          round: 1,
          startedAt: iso(1_000),
          durationMs: 1_000,
          model: "m",
          inputTokens: 0,
          outputTokens: 0,
          cacheReadTokens: 0,
          toolCalls: 1,
        },
        {
          round: 2,
          startedAt: iso(60_000),
          durationMs: 2_000,
          model: "m",
          inputTokens: 0,
          outputTokens: 0,
          cacheReadTokens: 0,
          toolCalls: 0,
        },
      ],
    });
    const run = phaseRecord({
      key: "turn-1|sandbox|1|L1",
      phase: "sandbox",
      launchId: "L1",
      codingAgent: "claude-code",
      clockStart: iso(3_000),
      durationMs: 55_000,
    });
    const events = [
      event("agent_turn_started", 0, { started_at: iso(0) }),
      event("sandbox_run_started", 3_000, {
        launch_id: "L1",
        started_at: iso(3_000),
        coding_agent: "claude-code",
      }),
      event("agent_turn_started", 58_500, { started_at: iso(0), resumed: true }),
    ];
    const w = buildWaterfall({
      events,
      phases: [execute, run],
      now: T0 + 90_000,
      running: false,
      parked: false,
    });
    const phase = byId(w.spans, "turn-1|execute|1");
    const box = byId(w.spans, "run:L1");
    expect(w.spans.filter((s) => s.kind === "turn")).toHaveLength(1);
    expect([phase.start - T0, phase.end - T0]).toEqual([1_000, 62_000]);
    expect([box.start - T0, box.end - T0, box.depth, box.launchId]).toEqual([
      3_000,
      58_000,
      1,
      "L1",
    ]);
    expect(w.from).toBe(T0);
  });

  test("two launches are two spans, and only the one still out is open", () => {
    const first = phaseRecord({
      key: "turn-1|sandbox|1|L1",
      phase: "sandbox",
      launchId: "L1",
      clockStart: iso(1_000),
      durationMs: 4_000,
    });
    const events = [
      event("sandbox_run_started", 1_000, { launch_id: "L1", started_at: iso(1_000) }),
      event("sandbox_run_started", 8_000, { launch_id: "L2", started_at: iso(8_000) }),
    ];
    const now = T0 + 30_000;
    const w = buildWaterfall({ events, phases: [first], now, running: true, parked: true });
    const runs = w.spans.filter((s) => s.kind === "run");
    expect(runs.map((r) => [r.launchId, r.open, r.end - T0])).toEqual([
      ["L1", false, 5_000],
      ["L2", true, 30_000],
    ]);
  });

  test("a run that failed ends at its failure", () => {
    const events = [
      event("sandbox_run_started", 1_000, { launch_id: "L1", started_at: iso(1_000) }),
      event("sandbox_run_failed", 4_000, {}),
    ];
    const w = buildWaterfall({
      events,
      phases: [],
      now: T0 + 9_000,
      running: false,
      parked: false,
    });
    const run = byId(w.spans, "run:L1");
    expect([run.failed, run.open, run.end - T0]).toEqual([true, false, 4_000]);
  });

  test("review pending follows a finished executor while the turn runs, and not while it is parked", () => {
    const execute = phaseRecord({ clockStart: iso(0), durationMs: 2_000 });
    const input = { events: [], phases: [execute], now: T0 + 5_000, running: true };
    const pending = buildWaterfall({ ...input, parked: false }).spans.find(
      (s) => s.kind === "pending",
    );
    expect(pending && [pending.start - T0, pending.end - T0, pending.open]).toEqual([
      2_000,
      5_000,
      true,
    ]);
    expect(buildWaterfall({ ...input, parked: true }).spans.some((s) => s.kind === "pending")).toBe(
      false,
    );
  });
});

describe("ticks", () => {
  test("steps along 1-2-5 and never exceeds the requested count", () => {
    expect(ticks(11_000, 6)).toEqual({
      step: 2_000,
      marks: [0, 2_000, 4_000, 6_000, 8_000, 10_000],
    });
    expect(ticks(900, 6).step).toBe(200);
    expect(ticks(0).marks).toEqual([0]);
  });
});

describe("tickLabel", () => {
  test("names a mark in whole units of its step", () => {
    expect([0, 5_000, 10_000].map((t) => tickLabel(t, 5_000))).toEqual(["0", "5s", "10s"]);
    expect(tickLabel(200, 200)).toBe("200ms");
    expect(tickLabel(90_000, 30_000)).toBe("1m 30s");
    expect(tickLabel(120_000, 60_000)).toBe("2m");
  });

  // ONE FORMAT ACROSS A RULER: "10 s" beside "1m 40s", and "4000 ms" beside
  // "1 s", were two conventions in one row of labels.
  test("writes every mark of one ruler in one format, the duration column's", () => {
    const { step, marks } = ticks(100_000, 10);
    const labels = marks.map((t) => tickLabel(t, step));
    expect(labels).toEqual([
      "0",
      "10s",
      "20s",
      "30s",
      "40s",
      "50s",
      "1m",
      "1m 10s",
      "1m 20s",
      "1m 30s",
      "1m 40s",
    ]);
    for (const label of labels) expect(label).not.toMatch(/\d \S/);
    // A sub-second step past one second counts seconds, not thousands of ms.
    expect([500, 1_000, 1_500, 4_000].map((t) => tickLabel(t, 500))).toEqual([
      "500ms",
      "1s",
      "1.5s",
      "4s",
    ]);
    expect(tickLabel(1_050, 50)).toBe("1.05s");
    expect(tickLabel(40_000, 10_000)).toBe(fmtDuration(40_000));
  });
});
