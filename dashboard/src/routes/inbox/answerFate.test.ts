import { expect, test } from "vitest";

import { answerFate, type GivenAnswer } from "./answerFate.ts";

/**
 * WHAT BECAME OF AN ANSWER IS READ OFF THE RUN'S RECORD, never assumed from
 * the press. `answer_run` answers `pending`, and the Inbox used to show that
 * as "Answered" and drop the question: a person whose answer never reached the
 * run was told it had, and the question kept coming back.
 *
 * Mutation: answer `taken` for a run still waiting, and the first case fails;
 * ignore `declined_answers`, and the person is never told to answer again.
 */
test("an answer's fate is the run record's, not the press's", () => {
  const given: GivenAnswer = { text: "use main", launchId: "q1", answerId: "a1" };
  const run = (over: Record<string, unknown>) => ({
    status: "awaiting_clarification" as const,
    launch_id: "q1",
    declined_answers: [] as string[],
    ...over,
  });
  expect(answerFate(given, run({}), true)).toBe("sent");
  expect(answerFate(given, run({ status: "reseed" }), true)).toBe("sent");
  expect(answerFate(given, run({ status: "answered", answer_ids: ["a1"] }), true)).toBe("recorded");
  expect(answerFate(given, run({ status: "answered" }), true)).toBe("recorded");
  // THE FIRST RECORDED ANSWER WINS: another one on the same question was used.
  expect(answerFate(given, run({ status: "answered", answer_ids: ["a9"] }), true)).toBe(
    "superseded",
  );
  expect(answerFate(given, run({ status: "resumed" }), true)).toBe("taken");
  expect(answerFate(given, run({ status: "running" }), true)).toBe("taken");
  // GONE, never seen holding the answer: the run ended, and the record cannot
  // say whether it took the answer first.
  expect(answerFate(given, undefined, true)).toBe("ended");
  expect(answerFate(given, undefined, true, true)).toBe("taken");
  expect(answerFate(given, undefined, false)).toBe("sent");
  expect(answerFate(given, run({ declined_answers: ["a1"] }), true)).toBe("declined");
  expect(answerFate(given, run({ launch_id: "q2" }), true)).toBe("overtaken");
  // ANOTHER PERSON'S ANSWER LET GO is not this one's.
  expect(answerFate(given, run({ declined_answers: ["a0"] }), true)).toBe("sent");
});
