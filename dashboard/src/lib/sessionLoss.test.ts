import { expect, test } from "vitest";
import { lossWords, notContinuedOf, notContinuedSentence } from "./sessionLoss.ts";
import { SESSION_UNNAMED_LOSS } from "~/contract/sessions.ts";

// A VERDICT IS SAID AS THE ENGINE SAYS IT (`executorkeep.NotContinued.Sentence`):
// the lost turn, the loss's clause and what the run did instead, and never the
// detail, which is somebody else's words.
//
// Mutation: drop a branch, reword a clause, or put the detail in the sentence,
// and a case goes red.
test("a verdict is said in the engine's sentence", () => {
  expect(notContinuedSentence({ loss: "too_large", instead: "fresh", detail: "ghp_secret" })).toBe(
    "The run did not continue its session: its session was larger than the 48 MiB kept for one " +
      "session. It started a fresh session.",
  );
  expect(
    notContinuedSentence({
      loss: "too_large",
      instead: "older",
      lost_turn: "t-n",
      older_turn: "t-m",
    }),
  ).toBe(
    "The run did not continue turn t-n's session: its session was larger than the 48 MiB kept " +
      "for one session. It continued turn t-m's older session instead, which ends before that work.",
  );
  expect(
    notContinuedSentence({
      loss: "capture_late",
      instead: "older",
      older: { launch_seq: 2, capture_seq: 3 },
    }),
  ).toContain("an older capture of its turn (launch 2, capture 3)");
});

// A LOSS A NEWER ENGINE NAMES IS A VALUE: read, and said in the unnamed words.
test("a loss this build does not name is said as unnamed", () => {
  expect(lossWords("a_newer_loss")).toBe(SESSION_UNNAMED_LOSS);
  const nc = notContinuedOf({ loss: "a_newer_loss", instead: "fresh" });
  expect(nc && notContinuedSentence(nc)).toContain(SESSION_UNNAMED_LOSS);
  expect(notContinuedOf(undefined)).toBeNull();
  expect(notContinuedOf({ instead: "fresh" })).toBeNull();
});
