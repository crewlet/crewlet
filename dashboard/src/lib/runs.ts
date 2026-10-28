import type { SandboxRun } from "~/protocol/types.ts";

/**
 * How often the durable coding-run record is asked again.
 *
 * ONE VALUE for every reader of `sandbox_runs` — the runs board, a run's own
 * page and rail, and the attention queue Home and the Inbox draw. It was two:
 * the board polled every 20 s and the queue every 30 s, so a run that parked
 * on a question appeared on the board and then, up to ten seconds later, as
 * the decision the Inbox asks a person to make — two screens disagreeing about
 * whether anybody was being waited on.
 *
 * TWENTY SECONDS, because nothing pushes this record: a run's status moves on
 * its own row in the coordination store and the live projection carries only
 * what a running-runs panel draws. A run lives for minutes and a parked one
 * waits for a person, who is not answering inside twenty seconds either; the
 * live projection reconciles its own panel against the same record every
 * 30 s, so a shorter poll here would be reading a record that has not moved.
 */
export const RUNS_POLL_MS = 20_000;

/**
 * An answer a person gave a parked run by its turn (`answer_run`): what they
 * wrote, the question it answers (`launch_id`) and the answer's own id
 * (`answer_id`), as the engine answered them.
 */
export interface GivenAnswer {
  text: string;
  launchId: string;
  answerId: string;
}

/**
 * What became of a given answer, read off the run's own record.
 *
 * `answer_run` answers `pending`: the answer is on its way to the node holding
 * the run's seat, which records it on the run when it takes it. So "sent" is
 * all the press itself can say, and the RECORD says the rest:
 *
 *  - `taken`: the run is answered or resumed on the question the answer was
 *    given against, or its record is gone (the run took it and ended);
 *  - `declined`: the question let this very answer go, and the run waits on it
 *    again, so the person must answer again;
 *  - `overtaken`: the run moved on to another question first, so this answer
 *    answers nothing the run still asks;
 *  - `sent`: the run still waits on the question with nothing recorded yet.
 *
 * A record that was never read (`undefined` with `loaded` false) is `sent`:
 * nothing is known yet, and nothing is claimed.
 */
export type AnswerFate = "sent" | "taken" | "declined" | "overtaken";

export function answerFate(
  given: GivenAnswer,
  run: Pick<SandboxRun, "status" | "launch_id" | "declined_answers"> | undefined,
  loaded: boolean,
): AnswerFate {
  if (!run) return loaded ? "taken" : "sent";
  if (given.answerId && run.declined_answers?.includes(given.answerId)) return "declined";
  if (run.launch_id && given.launchId && run.launch_id !== given.launchId) return "overtaken";
  if (run.status === "answered" || run.status === "resumed") return "taken";
  if (run.status === "awaiting_clarification" || run.status === "reseed") return "sent";
  // LAUNCHING OR RUNNING on the same question: it was resumed and its turn
  // launched the next job before any new question was asked.
  return "taken";
}
