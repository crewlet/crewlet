/**
 * A parked run's row says what the ENGINE decided about it for the reader,
 * never a rule of its own: "Needs you" only where the engine says it waits on
 * them, whom it waits on otherwise, and the answer offered only where the
 * engine says they may give it.
 */
import { expect, test } from "vitest";
import type { ExecutorRow } from "~/protocol/index.ts";
import { runStanding } from "./runStanding.ts";

const names: Record<string, string> = { "release-manager": "Release Manager", ana: "Ana", bo: "Bo" };
const nameOf = (h: string) => names[h] ?? h;

function run(over: Partial<ExecutorRow>): ExecutorRow {
  return {
    turn_id: "t1",
    agent_handle: "pm",
    role: "Product Manager",
    status: "awaiting_clarification",
    harness: "claude-code",
    placement: "direct",
    question_context: "",
    audience: "manager",
    audience_handles: ["ana"],
    branch: "",
    trace_id: "",
    owner: "",
    box_exists: true,
    paused_at: "",
    pause_ttl_seconds: 0,
    started_at: "",
    updated_at: "",
    answerable_in_chat: false,
    ...over,
  };
}

test("a run put to the reader needs them, and offers the answer the engine allows", () => {
  expect(
    runStanding(run({ put_to: ["ana"], put_to_you: true, may_answer: true, needs_you: true }), nameOf),
  ).toEqual({ needs: true, answers: true, word: "Needs you" });
});

test("a run with its delegate says whom it waits on, and the reader who may answer it may", () => {
  expect(
    runStanding(
      run({ put_to: ["release-manager"], put_to_you: false, may_answer: true, needs_you: false }),
      nameOf,
    ),
  ).toEqual({ needs: false, answers: true, word: "Waiting on Release Manager" });
});

test("a run the reader may not answer says whom it waits on and offers nothing", () => {
  expect(
    runStanding(
      run({ put_to: ["ana", "bo"], put_to_you: false, may_answer: false, needs_you: false }),
      nameOf,
    ),
  ).toEqual({ needs: false, answers: false, word: "Waiting on Ana and Bo" });
});

test("a live entry the engine has not stated is nobody's to answer, named by its audience", () => {
  expect(runStanding(run({}), nameOf)).toEqual({
    needs: false,
    answers: false,
    word: "Waiting on Ana",
  });
});
