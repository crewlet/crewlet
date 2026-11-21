/**
 * What the badge counts and how the lists split are the ENGINE's facts read
 * once: what `decisions` says waits on the reader is counted, what they may
 * answer that waits on somebody else is listed apart and counted by nothing.
 */
import { expect, test } from "vitest";
import type { DecisionItem, DecisionsAnswer, ExecutorRow } from "~/protocol/index.ts";
import { decisionSubjects, needsYouCount, splitDecisions } from "./decisions.ts";

const run = (turn: string, needs: boolean): ExecutorRow =>
  ({
    turn_id: turn,
    agent_handle: "pm",
    status: "awaiting_clarification",
    put_to_you: needs,
    may_answer: true,
    needs_you: needs,
  }) as ExecutorRow;

const item = (turn: string, at: string, needs: boolean): DecisionItem => ({
  kind: "run",
  at,
  run: run(turn, needs),
  put_to_you: needs,
  may_answer: true,
  needs_you: needs,
});

const answer: DecisionsAnswer = {
  handle: "founder",
  items: [
    item("waits-on-me", "2026-11-14T10:00:00Z", true),
    item("with-its-delegate", "2026-11-14T11:00:00Z", false),
  ],
  total: 1,
  answerable: 1,
  more: false,
  capped: false,
};

test("what waits on the reader leads, and what they may answer for somebody else is apart", () => {
  const { yours, others } = splitDecisions(decisionSubjects(answer, []));
  expect(yours.map((s) => (s.kind === "run" ? s.run.turn_id : ""))).toEqual(["waits-on-me"]);
  expect(others.map((s) => (s.kind === "run" ? s.run.turn_id : ""))).toEqual(["with-its-delegate"]);
});

test("the badge is the engine's total with the reader's seats and conditions, never the list's length", () => {
  expect(needsYouCount(answer, 2, 3)).toEqual({ waiting: 6, capped: false });
  expect(needsYouCount({ ...answer, capped: true }, 0, 0)).toEqual({ waiting: 1, capped: true });
  expect(needsYouCount(null, 2, 3)).toEqual({ waiting: null, capped: false });
});

test("a fold-in put to the reader waits on them, and one they may answer for somebody else is apart", () => {
  const foldIn = (id: string, at: string, needs: boolean): DecisionItem => ({
    kind: "fold_in",
    at,
    fold_in: {
      id,
      turn_id: "t-1",
      launch_id: "job-1",
      seat: "pm",
      requester: "founder",
      asker: "ceo",
      asker_kind: "human",
      conversation: "chat:C1",
      words: "also redo the logo",
      cut: 0,
      put_to: ["founder"],
      asked_at: at,
      decision: { question: "ceo asked pm for work of their own", options: [], role: "approver" },
    },
    put_to_you: needs,
    may_answer: true,
    needs_you: needs,
  });
  const folded: DecisionsAnswer = {
    ...answer,
    items: [
      foldIn("put-to-me", "2026-11-14T12:00:00Z", true),
      foldIn("a-lead-may", "2026-11-14T13:00:00Z", false),
    ],
  };
  const { yours, others } = splitDecisions(decisionSubjects(folded, []));
  expect(yours.map((s) => (s.kind === "fold_in" ? s.foldIn.id : ""))).toEqual(["put-to-me"]);
  expect(others.map((s) => (s.kind === "fold_in" ? s.foldIn.id : ""))).toEqual(["a-lead-may"]);
});
