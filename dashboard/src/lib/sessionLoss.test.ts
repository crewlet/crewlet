import { expect, test } from "vitest";
import { lossWords, notContinuedOf, notContinuedSentence, turnVerdicts } from "./sessionLoss.ts";
import { SESSION_UNNAMED_LOSS } from "~/contract/sessions.ts";
import type { EventRecord } from "~/protocol/index.ts";

// A VERDICT IS SAID AS THE ENGINE SAYS IT (`executorkeep.NotContinued.Sentence`):
// the lost turn, the loss's clause and what the run did instead, and never the
// detail, which is somebody else's words.
//
// Mutation: drop a branch, reword a clause, or put the detail in the sentence,
// and a case goes red.
test("a verdict is said in the engine's sentence", () => {
  expect(notContinuedSentence({ loss: "too_large", instead: "fresh", detail: "ghp_secret" })).toBe(
    "The run did not continue its session: its session was larger than one is kept at: 94 MiB in " +
      "all, at most 48 MiB of it text. It started a fresh session.",
  );
  expect(
    notContinuedSentence({
      loss: "too_large",
      instead: "older",
      lost_turn: "t-n",
      older_turn: "t-m",
    }),
  ).toBe(
    "The run did not continue turn t-n's session: its session was larger than one is kept at: " +
      "94 MiB in all, at most 48 MiB of it text. It continued turn t-m's older session instead, " +
      "which ends before that work.",
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

/** A launch event of the turn, at second `at`. */
function launchEvent(
  type: "executor_launched" | "executor_collected",
  id: string,
  at: number,
  payload: Record<string, unknown>,
): EventRecord {
  return {
    id,
    type,
    timestamp: new Date(Date.UTC(2026, 10, 24, 12, 0, at)).toISOString(),
    source: "SWE",
    actor: "swe",
    summary: "",
    category: "executor",
    trace_id: "",
    span_id: "",
    parent_span_id: "",
    topic: "",
    payload,
  };
}

// A TURN SAYS EACH LAUNCH'S NEWEST WORD ON ITS SESSION: a launch still working
// says what it decided as it started, so a long run that started fresh says so
// while it runs; a collected launch says its collection's final verdict (a box
// that fell back is `restore_failed` only there), and one whose collection lost
// nothing says nothing, whatever it started with. In the order they started.
//
// Mutation: read the collected record alone, let a launch's start outvote its
// collection, or order by the newest event, and a case goes red.
test("a turn says each launch's newest verdict on its session", () => {
  const fresh = { loss: "harness_changed", instead: "fresh", lost_turn: "t0" };
  const events = [
    launchEvent("executor_collected", "e4", 40, {
      launch_id: "l2",
      session_not_continued: { loss: "restore_failed", instead: "fresh", detail: "import failed" },
    }),
    launchEvent("executor_launched", "e1", 10, { launch_id: "l1", session_not_continued: fresh }),
    launchEvent("executor_collected", "e2", 20, { launch_id: "l1" }),
    launchEvent("executor_launched", "e3", 30, { launch_id: "l2" }),
    launchEvent("executor_launched", "e5", 50, { launch_id: "l3", session_not_continued: fresh }),
  ];
  const said = turnVerdicts(events);
  expect(said.map((v) => [v.launchId, v.verdict.loss, v.collected])).toEqual([
    ["l2", "restore_failed", true],
    ["l3", "harness_changed", false],
  ]);
  expect(turnVerdicts(undefined)).toEqual([]);
});

// A LAUNCH IS SAID ONCE: an event naming no launch is not filed under an id of
// its own beside the launch its collection names, which would say it twice.
//
// Mutation: file an event that names no launch under its event id, and the
// launch is said twice.
test("an event naming no launch does not say a launch twice", () => {
  const fresh = { loss: "harness_changed", instead: "fresh" };
  const said = turnVerdicts([
    launchEvent("executor_launched", "e1", 10, { session_not_continued: fresh }),
    launchEvent("executor_collected", "e2", 20, {
      launch_id: "l1",
      session_not_continued: { loss: "restore_failed", instead: "fresh" },
    }),
  ]);
  expect(said.map((v) => [v.launchId, v.verdict.loss])).toEqual([["l1", "restore_failed"]]);
});
