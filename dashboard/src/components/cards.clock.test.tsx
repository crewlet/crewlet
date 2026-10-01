/**
 * A live phase's stopwatch, and what it costs the card around it.
 *
 * A running phase and a running turn count up once a second, and that is the
 * one thing on either card the clock moves. The cards read it themselves —
 * a reading of the second, at the top of the card — so each tick drew the
 * whole card again: a phase's every round, tool call and prompt document, and
 * a turn's every phase card under it, once a second for as long as anything
 * ran, on the turn page and every seat page showing one. The stopwatch reads
 * the clock in the element that shows it now (`ClockText`), so a tick draws
 * those words and nothing else.
 *
 * WHAT IS COUNTED is a function each card calls once per render of its own —
 * `ledgerOf` for a phase card, `triggerHeadline` for a turn card — wrapped so
 * the count is the card's renders, and nothing about what either returns
 * changes.
 */

import { act, cleanup, render } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { PhaseCard } from "./PhaseCard.tsx";
import { TurnCard } from "./TurnCard.tsx";
import { Router } from "~/app/router.tsx";
import { groupTurns, ledgerOf, triggerHeadline, type PhaseRecord } from "~/lib/phases.ts";

vi.mock("~/lib/phases.ts", async (importOriginal) => {
  const actual = await importOriginal<typeof import("~/lib/phases.ts")>();
  return {
    ...actual,
    ledgerOf: vi.fn(actual.ledgerOf),
    triggerHeadline: vi.fn(actual.triggerHeadline),
  };
});

const T0 = Date.parse("2031-04-16T12:00:00Z");
const iso = (at: number) => new Date(at).toISOString();

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["setInterval", "clearInterval", "Date"] });
  vi.setSystemTime(T0);
  vi.mocked(ledgerOf).mockClear();
  vi.mocked(triggerHeadline).mockClear();
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

/** A phase that started five seconds ago and last reported one second ago. */
function running(over: Partial<PhaseRecord> = {}): PhaseRecord {
  return {
    key: "t1|execute|1",
    turnId: "t1",
    workKey: "wk-1",
    phase: "execute",
    iteration: 1,
    agentId: "a-dev",
    role: "Dev A",
    model: "scripted",
    providerKey: "",
    live: true,
    failed: false,
    error: "",
    errorKind: "",
    systemPrompt: "",
    userPrompt: "",
    response: "",
    tools: [],
    narration: [{ round: 1, reasoning: "the file first", content: "Reading the file." }],
    partial: null,
    inputTokens: 0,
    outputTokens: 0,
    totalTokens: 0,
    roundsUsed: 1,
    exhaustedRounds: false,
    emptyAnswerRounds: 0,
    rescueFired: false,
    decision: "",
    notes: "",
    conversationKey: "",
    toolsAvailable: [],
    toolCatalogue: [],
    worker: "",
    taskId: "",
    hostPhase: "",
    hostIteration: 0,
    backend: "",
    codingAgent: "",
    sandboxId: "",
    costUSD: 0,
    deliveredRefs: [],
    trigger: null,
    at: iso(T0 - 1_000),
    startedAt: iso(T0 - 5_000),
    durationMs: 0,
    eventId: "ev-1",
    ...over,
  };
}

/** Moves the clock one tick at a time, as an open tab sees it. */
function tick(times: number): void {
  for (let i = 0; i < times; i++) {
    act(() => {
      vi.advanceTimersByTime(1_000);
    });
  }
}

// TEN SECONDS OF A RUNNING PHASE are ten new readings of its stopwatch and no
// new drawing of the card: inside the two minutes before a silent phase reads
// as stale, the stopwatch is the only thing on it the clock moves.
test("a running phase's stopwatch moves without drawing the card again", () => {
  const { container } = render(<PhaseCard record={running()} defaultOpen />);
  const time = () => container.querySelector("time.phase-meta")?.textContent;
  expect(time()).toBe("5s");
  const drawn = vi.mocked(ledgerOf).mock.calls.length;

  tick(10);
  expect(time()).toBe("15s");
  expect(vi.mocked(ledgerOf).mock.calls.length).toBe(drawn);
});

// AND A RUNNING TURN: its own count moves, and neither the turn card nor the
// phase card open under it is drawn again for it.
test("a running turn's stopwatch moves without drawing the turn or its phases again", () => {
  const live = groupTurns([running()])[0]!;
  const { container } = render(
    <Router>
      <TurnCard group={live} defaultOpen />
    </Router>,
  );
  const time = () => container.querySelector("header time")?.textContent;
  expect(time()).toBe("5s");
  const turnDrawn = vi.mocked(triggerHeadline).mock.calls.length;
  const phaseDrawn = vi.mocked(ledgerOf).mock.calls.length;
  expect(phaseDrawn).toBeGreaterThan(0);

  tick(10);
  expect(time()).toBe("15s");
  expect(vi.mocked(triggerHeadline).mock.calls.length).toBe(turnDrawn);
  expect(vi.mocked(ledgerOf).mock.calls.length).toBe(phaseDrawn);
});
