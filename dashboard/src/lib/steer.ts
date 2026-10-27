/**
 * What a person is told about a note they sent a turn.
 *
 * `steer_turn` answers `pending` with WHEN the turn will read the note
 * (`reads`, one of `STEER_READS`): at its next step, or — for a coding run
 * parked on a question — once the question is answered. The toast the press
 * raises says which, so a person who steered a run waiting on somebody's
 * answer is not told it will be read at the next step.
 */

import { STEER_READS } from "~/contract/steer.ts";

/** When a turn reads a note it took, as `steer_turn` answers it. */
type SteerReads = (typeof STEER_READS)[number];

/**
 * The words for each value — the compiler holds every value of the contract to
 * a sentence.
 */
const READS_SENTENCE: Record<SteerReads, string> = {
  next_step: "It reads it at its next step.",
  after_answer: "Its coding run is waiting on a question; it reads the note once that is answered.",
};

/**
 * The sentence for a `steer_turn` answer's `reads`, or undefined for an answer
 * that names no value this build knows — which is then told the generic way.
 */
export function steerReadsSentence(receipt: unknown): string | undefined {
  const reads = (receipt as { reads?: unknown } | null)?.reads;
  return typeof reads === "string" && (STEER_READS as readonly string[]).includes(reads)
    ? READS_SENTENCE[reads as SteerReads]
    : undefined;
}
