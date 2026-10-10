/**
 * A parked run as a decision: how long its place is kept, and the board row
 * read as the decision it asks.
 */

import { expect, test } from "vitest";

import { decisionOfRun, holdLine } from "./DecisionRow.tsx";
import type { DecisionRun, SandboxRun } from "~/protocol/index.ts";

const NOW = Date.parse("2031-04-16T10:00:00Z");

const run = (over: Partial<DecisionRun> = {}): DecisionRun => ({
  turn_id: "run-1",
  question: "Keep the v1 parser behind a flag?",
  task: { id: "t-412", key: "ENG-412" },
  agent_handle: "swe",
  asked: ["ana"],
  asked_at: "2031-04-16T09:00:00Z",
  keeps_place_until: "2031-04-16T11:30:00Z",
  answerable_in_chat: false,
  ...over,
});

// THE DEADLINE IS THE ENGINE'S: the line says how far off the instant it was
// sent is, says the box is gone when it was sent null, and says NOTHING about a
// run whose deadline it was not sent — never a guess either way.
test("the hold line reads the engine's deadline and nothing else", () => {
  expect(holdLine(run(), NOW)).toBe("Its box is held for 1h 30m more");
  expect(holdLine(run({ keeps_place_until: "2031-04-16T09:59:00Z" }), NOW)).toBe(
    "Its box is past its pause window and may be reclaimed",
  );
  expect(holdLine(run({ keeps_place_until: null }), NOW)).toBe(
    "Its box is not held: answering restarts the work from the question",
  );
  expect(holdLine(run({ keeps_place_until: undefined }), NOW)).toBeUndefined();
});

// A BOARD ROW IS READ AS ITS DECISION BY MAPPING, NEVER BY ARITHMETIC: the
// row's own deadline is carried as sent, and a row carrying a stamp and a TTL
// but no deadline (a live entry the store has not caught up with) gets none.
test("a board row is the decision it asks, its deadline carried as sent", () => {
  const row = {
    turn_id: "run-1",
    agent_handle: "swe",
    question: "Keep the v1 parser behind a flag?",
    audience_handles: ["ana"],
    audience_fallback: true,
    work_item: { backend: "native", id: "t-412", key: "ENG-412", project: "ENG" },
    paused_at: "2031-04-16T09:00:00Z",
    pause_ttl_seconds: 1800,
    keeps_place_until: "2031-04-16T09:30:00Z",
    asked_at: "2031-04-16T08:59:58Z",
    updated_at: "2031-04-16T09:00:01Z",
    answerable_in_chat: true,
    chat_surface: "slack",
  } as SandboxRun;
  expect(decisionOfRun(row)).toEqual({
    turn_id: "run-1",
    question: "Keep the v1 parser behind a flag?",
    task: { id: "t-412", key: "ENG-412" },
    agent_handle: "swe",
    asked: ["ana"],
    asked_fallback: true,
    asked_at: "2031-04-16T08:59:58Z",
    keeps_place_until: "2031-04-16T09:30:00Z",
    answerable_in_chat: true,
    chat_surface: "slack",
  });
  const live = { ...row, keeps_place_until: undefined } as SandboxRun;
  expect(decisionOfRun(live).keeps_place_until).toBeUndefined();
});
