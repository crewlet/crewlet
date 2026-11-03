import { decisionTexts } from "./work.ts";
import type { ExecutorRow } from "~/protocol/index.ts";

/**
 * How often the executors' durable record is asked again.
 *
 * ONE VALUE for every reader of `executors` — the Executors board, an
 * executor's own page and rail, and the attention queue Home and the Inbox
 * draw. It was two: the board polled every 20 s and the queue every 30 s, so
 * an executor that parked on a question appeared on the board and then, up to
 * ten seconds later, as
 * the decision the Inbox asks a person to make — two screens disagreeing about
 * whether anybody was being waited on.
 *
 * TWENTY SECONDS, because nothing pushes this record: an executor's status
 * moves on its own row in the coordination store and the live projection
 * carries only what a panel of executors in flight draws. A launch lives for
 * minutes and a parked executor waits for a person, who is not answering
 * inside twenty seconds either; the
 * live projection reconciles its own panel against the same record every
 * 30 s, so a shorter poll here would be reading a record that has not moved.
 */
export const RUNS_POLL_MS = 20_000;

/**
 * Every text of an executor's question a reader reads as one message, as
 * `askTexts` is an ask's: a bare `#11` in any of them is placed by what the
 * others name (`TokenScope`).
 *
 * AN EXECUTOR'S QUESTION IS ITS TITLE AND ITS CONTEXT, each its own, on every
 * reader of it: the durable row, the live entry and the record of its answer.
 * Nothing is split out of the question (`components/RunQuestion.tsx`).
 */
export function runAskTexts(
  run: Pick<ExecutorRow, "question" | "question_context" | "decision">,
): string[] {
  return [run.question ?? "", run.question_context, ...decisionTexts(run.decision)].filter(
    (text) => text.trim() !== "",
  );
}

/**
 * Why an executor's question cannot be answered from what a screen holds, or
 * undefined where it can.
 *
 * AN ANSWER NAMES THE QUESTION IT ANSWERS (`answer_executor{launch_id}`), the one
 * the reader saw, so an executor that moved on to another question refuses it
 * rather than taking it as an answer to that one. A live entry the screen
 * read before the executor's durable row carries no launch yet, and an answer
 * pressed then would name nothing.
 */
export function unanswerable(run: Pick<ExecutorRow, "launch_id">): string | undefined {
  return run.launch_id ? undefined : "Reading the executor’s question first.";
}
