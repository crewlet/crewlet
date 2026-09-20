/**
 * The two things a collapsed turn card says about a turn, and how it got both
 * wrong.
 *
 * HOW LONG IT RAN was `last.at - first.at` — two LANDING instants — which drops
 * the first phase's own length: an execute-then-review turn reported its
 * review's duration as the whole turn's, printed above a phase card showing
 * three minutes. And WHAT WOKE IT wore `.truncate`, the CELL rule, on a card
 * that grows to its content, so the sentence was cut at the first line with
 * empty card underneath and the only way to learn what the turn was about was to
 * open it.
 *
 * Both are read against the ENGINE'S OWN ROW where the screen holds one: the
 * turns table above the cards prints the same turn, and the two reported three
 * mutually impossible figures for it.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";

import { TurnCard } from "./TurnCard.tsx";
import { Router } from "~/app/router.tsx";
import { groupTurns, type PhaseRecord } from "~/lib/phases.ts";
import type { TurnRow } from "~/protocol/index.ts";

afterEach(cleanup);

function phase(over: Partial<PhaseRecord> = {}): PhaseRecord {
  return {
    key: "t1|execute|1",
    turnId: "t1",
    workKey: "wk-1",
    phase: "execute",
    iteration: 1,
    role: "Dev A",
    model: "scripted",
    providerKey: "",
    live: false,
    failed: false,
    error: "",
    errorKind: "",
    systemPrompt: "",
    userPrompt: "",
    response: "",
    tools: [],
    narration: [],
    partial: null,
    inputTokens: 0,
    outputTokens: 0,
    totalTokens: 0,
    roundsUsed: 0,
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
    at: "2026-09-13T10:03:00Z",
    startedAt: "2026-09-13T10:00:00Z",
    durationMs: 180_000,
    eventId: "ev-1",
    ...over,
  };
}

/** An execute of three minutes, then a review of one — four in all. */
function turn(over: Partial<PhaseRecord> = {}) {
  return groupTurns([
    phase(over),
    phase({
      key: "t1|review|1",
      phase: "review",
      at: "2026-09-13T10:04:00Z",
      startedAt: "2026-09-13T10:03:00Z",
      durationMs: 60_000,
      ...over,
      // The trigger belongs to the execute record; a review carries none.
      trigger: null,
    }),
  ])[0]!;
}

const draw = (node: React.ReactElement) => render(<Router>{node}</Router>);

test("a turn's length spans its first phase, not the gap between landings", () => {
  draw(<TurnCard group={turn()} />);
  expect(screen.getByText("4m 0s")).toBeTruthy();
  // The review's own duration, which is what two landing instants gave.
  expect(screen.queryByText("1m 0s")).toBeNull();
});

// AND THE ENGINE'S OWN MEASUREMENT WINS over the window this card derived, so
// the card and the table above it cannot state two lengths for one turn.
test("a settled row's measurement wins over the phase window", () => {
  const row = {
    turn_id: "t1",
    started_at: "2026-09-13T10:00:00Z",
    ended_at: "2026-09-13T10:04:35Z",
    duration_ms: 275_000,
    complete: true,
    phases: 2,
    iterations: 1,
    failed: false,
    input_tokens: 0,
    output_tokens: 0,
    total_tokens: 0,
  } satisfies TurnRow;
  draw(<TurnCard group={turn()} row={row} />);
  expect(screen.getByText("4m 35s")).toBeTruthy();
  expect(screen.queryByText("4m 0s")).toBeNull();
});

// A TURN THE ENGINE KILLED BETWEEN PHASES IS STILL A FAILED TURN ON THE CARD.
//
// `group.failed` is `phases.some(p => p.failed)`, which cannot see a panic
// recovered outside the turn loop or a detached sandbox run that failed —
// neither leaves a failed phase record behind. The store's aggregate reads
// those as failures by their TYPE, so the Turns grid one panel up marked the
// turn failed while this card, on the same screen and the same data, did not.
test("the engine's own failure mark shows on a turn with no failed phase", () => {
  const row = {
    turn_id: "t1",
    started_at: "2026-09-13T10:00:00Z",
    ended_at: "2026-09-13T10:04:35Z",
    duration_ms: 275_000,
    complete: false,
    phases: 2,
    iterations: 1,
    failed: true,
    input_tokens: 0,
    output_tokens: 0,
    total_tokens: 0,
  } satisfies TurnRow;
  // Every phase in the group is clean — the failure is the row's alone.
  draw(<TurnCard group={turn()} row={row} />);
  expect(screen.getByText("failed")).toBeTruthy();
});

// AND THE PHASES STILL SPEAK WHEN THE ROW HAS NOT CAUGHT UP. The row is polled
// and the phases are pushed, so a phase that failed a moment ago must be red
// here before the next poll carries it — the union is what makes both true.
test("a failed phase marks the card before the polled row agrees", () => {
  const failedPhase = groupTurns([phase({ failed: true })])[0]!;
  const row = {
    turn_id: "t1",
    started_at: "2026-09-13T10:00:00Z",
    ended_at: "2026-09-13T10:04:35Z",
    duration_ms: 275_000,
    complete: false,
    phases: 1,
    iterations: 1,
    failed: false,
    input_tokens: 0,
    output_tokens: 0,
    total_tokens: 0,
  } satisfies TurnRow;
  draw(<TurnCard group={failedPhase} row={row} />);
  expect(screen.getByText("failed")).toBeTruthy();
});

// A LIVE TURN COUNTS FROM THE INSTANT THE TURNS TABLE CALLS STARTED. `started_at`
// is the turn's first recorded EVENT — its prefetch lands before the first model
// call — so the table read "started 6m ago" beside a card counting 3m 15s from
// the phase that is running.
test("a live turn counts from the start the turns table prints", () => {
  const live = groupTurns([
    phase({ live: true, startedAt: "2026-09-13T10:03:00Z", at: "2026-09-13T10:03:30Z" }),
  ])[0]!;
  const row = {
    turn_id: "t1",
    started_at: "2026-09-13T10:00:00Z",
    ended_at: "",
    duration_ms: 0,
    complete: false,
    phases: 1,
    iterations: 1,
    failed: false,
    input_tokens: 0,
    output_tokens: 0,
    total_tokens: 0,
  } satisfies TurnRow;
  const { container } = draw(<TurnCard group={live} row={row} />);
  expect(container.querySelector("time")?.getAttribute("datetime")).toBe("2026-09-13T10:00:00Z");
});

const SUMMARY =
  "Message from founder: a task created: Write the quarterly plan for the platform team";

// THE TRIGGER LINE CLAMPS RATHER THAN BEING CUT AT ONE LINE. jsdom computes no
// layout, so what is asserted is the CLAIM: the element is on the clamp rule and
// not on the cell rule, and the tail the clamp still cuts is reachable. That
// `.clamp` clamps at all is `styles/text.test.ts`.
test("the trigger line clamps rather than being cut at one line", () => {
  draw(<TurnCard group={turn({ trigger: { type: "notification", summary: SUMMARY } })} />);
  const line = screen.getByText(SUMMARY);
  expect(line.className).toContain("clamp");
  expect(
    line.className,
    ".truncate is the cell rule — on a card it cuts the sentence with room underneath",
  ).not.toContain("truncate");
  expect(line.getAttribute("title")).toBe(SUMMARY);
});
