/**
 * What became of an answer a person gave a parked executor by its turn,
 * read off the executor's own record.
 *
 * HERE, BESIDE THE ONE PANE THAT ASKS, rather than in lib/runs.ts, which the
 * shell's attention queue loads on every page: the initial load carries only
 * what the first screen draws.
 */

import type { AskAnswer } from "~/components/writes.tsx";
import type { ExecutorRow } from "~/protocol/types.ts";

/**
 * An answer a person gave a parked executor by its turn (`answer_executor`): the option
 * they chose or what they wrote, the question it answers (`launch_id`) and the
 * answer's own id (`answer_id`), as the engine answered them.
 */
export interface GivenAnswer {
  answer: AskAnswer;
  launchId: string;
  answerId: string;
}

/**
 * What became of a given answer, read off the executor's own record.
 *
 * `answer_executor` answers `pending`: the answer is on its way to the node holding
 * the executor's seat, which records it on the executor's row when it takes it.
 * So "sent" is all the press itself can say, and the RECORD says the rest:
 *
 *  - `recorded`: the executor is answered with this answer, and its resume
 *    waits for the seat;
 *  - `taken`: the executor was launched again with it, or its record is gone
 *    after it was seen recorded or taken (it took the answer and finished);
 *  - `ended`: the record is gone and was never seen holding this answer: the
 *    executor ended (finished, was cancelled or was reaped) and whether it
 *    took the answer first is not something the record can say any more;
 *  - `superseded`: another answer was recorded first, so this one was not used;
 *  - `declined`: the question let this very answer go, and the executor waits
 *    on it again, so the person must answer again;
 *  - `overtaken`: the executor moved on to another question first, so this
 *    answer answers nothing it still asks;
 *  - `sent`: the executor still waits on the question with nothing recorded yet.
 *
 * A record that was never read (`undefined` with `loaded` false) is `sent`:
 * nothing is known yet, and nothing is claimed.
 *
 * NO `unoffered` FATE. The engine announces an answer by turn whose choice
 * the question does not offer as `unoffered`, but `answer_executor` checks the
 * choice against the very question it names (`launch_id`) before it sends
 * anything, and refuses it there: a press this pane made is refused at once,
 * and never becomes an answer that fate could be read for.
 */
export type AnswerFate =
  "sent" | "recorded" | "taken" | "ended" | "superseded" | "declined" | "overtaken";

export function answerFate(
  given: GivenAnswer,
  run: Pick<ExecutorRow, "status" | "launch_id" | "declined_answers" | "answer_ids"> | undefined,
  loaded: boolean,
  /** Whether this answer was already seen recorded on the executor, or taken. */
  seenTaken = false,
): AnswerFate {
  if (!run) {
    if (!loaded) return "sent";
    return seenTaken ? "taken" : "ended";
  }
  if (given.answerId && run.declined_answers?.includes(given.answerId)) return "declined";
  const recorded = run.answer_ids ?? [];
  const ours = given.answerId !== "" && recorded.includes(given.answerId);
  if (recorded.length > 0 && !ours && run.launch_id === given.launchId) return "superseded";
  if (run.launch_id && given.launchId && run.launch_id !== given.launchId) return "overtaken";
  if (run.status === "answered") return "recorded";
  if (run.status === "awaiting_clarification" || run.status === "reseed") return "sent";
  // RESUMED, OR LAUNCHING OR RUNNING on the same question: it was resumed
  // with the answer, and its turn may have launched it again since.
  return "taken";
}
