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
import { moment } from "./model.ts";

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
 *  - `taken`: the executor was launched again with it, or it ended — its
 *    record gone, or its ending decided — after it was seen recorded or taken
 *    (it took the answer and finished);
 *  - `ended`: the executor ended (finished, was cancelled or was reaped) —
 *    its record gone, or its ending decided, which no answer reaches — and
 *    was never seen holding this answer, so whether it took the answer first
 *    is not something the record can say;
 *  - `superseded`: another answer was recorded first, so this one was not used;
 *  - `refused`: a reply in the conversation reached the question first and
 *    was too long for the executor to take: it closed the question with none
 *    of it reaching the executor (`answer_refused`), so this answer was not
 *    used either, and the executor asks again if it still needs one;
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
  "sent" | "recorded" | "taken" | "ended" | "superseded" | "refused" | "declined" | "overtaken";

export function answerFate(
  given: GivenAnswer,
  run:
    | Pick<
        ExecutorRow,
        "status" | "launch_id" | "declined_answers" | "answer_ids" | "answer_refused" | "ending"
      >
    | undefined,
  loaded: boolean,
  /** Whether this answer was already seen recorded on the executor, or taken. */
  seenTaken = false,
): AnswerFate {
  if (!run) {
    if (!loaded) return "sent";
    return seenTaken ? "taken" : "ended";
  }
  // AN ENDING DECIDED IS AN EXECUTOR THAT IS OVER, as a record that is gone
  // is: it takes no answer, and its record waits only for the ending's last
  // steps, whatever status it was ended in.
  if (run.ending) return seenTaken ? "taken" : "ended";
  if (given.answerId && run.declined_answers?.includes(given.answerId)) return "declined";
  const recorded = run.answer_ids ?? [];
  const ours = given.answerId !== "" && recorded.includes(given.answerId);
  // A REFUSAL CLOSED THE QUESTION, no answer: what the record names is a reply
  // too long for the executor to take, and drawn as somebody's answer it would
  // tell the person the executor carries on with it.
  if (recorded.length > 0 && !ours && run.launch_id === given.launchId) {
    return run.answer_refused ? "refused" : "superseded";
  }
  if (run.launch_id && given.launchId && run.launch_id !== given.launchId) return "overtaken";
  if (run.status === "answered") return "recorded";
  if (run.status === "awaiting_clarification" || run.status === "reseed") return "sent";
  // RESUMED, OR LAUNCHING OR RUNNING on the same question: it was resumed
  // with the answer, and its turn may have launched it again since.
  return "taken";
}

/**
 * When what closed the executor's question was recorded, on the READER's clock
 * ([moment]), and by whom where `who`: a value beside the sentence drawn above
 * it, never a second wording of it. Who it was is left to that sentence where
 * it names them, as the engine's sentence for a question already answered does
 * (`caveat.AnsweredAlready`, which leaves the time to the screen), so a person
 * is told it once.
 *
 * A REFUSAL IS NO ANSWER. Where the record is a reply too long for the executor
 * to take (`answer_refused`), it closed the question with none of it reaching
 * the executor, and "Answered" beside it told the person the executor carries
 * on with somebody's answer it was never given.
 */
export function closedAt(
  record: Pick<ExecutorRow, "answered_by" | "answered_at" | "answer_refused">,
  now: number,
  who: boolean,
): string {
  const by = who && record.answered_by ? ` by @${record.answered_by}` : "";
  const at = moment(record.answered_at ?? "", now);
  if (record.answer_refused) {
    return `A reply${by} too long for the executor to take closed the question at ${at}.`;
  }
  return `Answered${by} at ${at}.`;
}
