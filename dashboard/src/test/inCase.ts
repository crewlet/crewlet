/**
 * A flush of the page's pending answers that ENDS WITH THE CASE that asked for
 * it. Imported by tests only.
 *
 * # Why a suite does not just `await act(async () => {})`
 *
 * That line is how a suite lets a stubbed engine's answers land without a
 * deadline: the answers are promises, `act` runs them and every render they
 * cause, and a slow machine makes it slower and never wrong. Five suites each
 * kept their own copy, named `answered()`.
 *
 * But a case that times out is failed, not stopped. Its function goes on
 * running beside the cases after it, and the next `act` it opens interleaves
 * with the live case's — and React keeps ONE act scope count for the process,
 * restoring on exit whatever it found on entry, so two interleaved scopes
 * leave it raised for good and every later render in the file is queued and
 * never flushed. One timeout reads as the rest of the file failing. The org
 * builder's harness closed that for its own waits (`routes/org/builder/
 * testkit.tsx`'s `Lens`); a module-level `answered()` could not be closed the
 * same way, because a function every case shares cannot tell the case calling
 * it now from the case that is still running after its time ran out.
 *
 * # So the flush is the case's own
 *
 * [flushInCase] is called while a case runs — a suite's `mount` calls it and
 * hands the flush back with the page — and it is retired when that case
 * finishes (`onTestFinished`). A late call through it is refused before it
 * opens an `act`, naming why, rather than opening one beside the next case.
 * The case holds the flush, so there is no shared variable a late caller could
 * read the next case's flush from: that variable is exactly what let the
 * builder's waits settle the next case's lens.
 */

import { act } from "@testing-library/react";
import { onTestFinished } from "vitest";

/**
 * Lets every answer the page has out land, and renders what they change.
 *
 * `step`, when given, is taken first in an `act` of its own — the move that
 * makes the page ask again, such as advancing the timers a case holds. It is
 * the flush's rather than the caller's because the refusal has to come BEFORE
 * it: a late case that moved the timers and was refused only at the flush had
 * already fired the next case's.
 */
export type Answered = (step?: () => void) => Promise<void>;

/**
 * A flush for the case running now, refused once that case has finished.
 *
 * Throws if called outside a running case, since a flush that belongs to no
 * case is the module-level one this replaces.
 */
export function flushInCase(): Answered {
  let open = true;
  onTestFinished(() => {
    open = false;
  });
  return async (step) => {
    if (!open) {
      throw new Error(
        "answered: the case that rendered this page has ended — this is that case, still running after its time ran out",
      );
    }
    if (step) act(step);
    await act(async () => {});
  };
}
