/**
 * A case's flush ends with the case.
 *
 * The pair below is what a timeout leaves behind, without the deadline: the
 * first case ends with a wait of its OWN still out — a promise nothing in the
 * harness can see or wake — and a flush to make once that wait is over. The
 * case after it releases the wait and reads what the late flush came to.
 *
 * Through the module-level `answered()` every suite used to keep, the late
 * flush opened an `act` beside the live case and returned as though nothing
 * were wrong: a function every case shares cannot tell the case calling it now
 * from one still running after its time ran out. The case's own flush can —
 * and refuses before the step a case hands it, since that step is the move
 * (advancing the timers) that would reach the next case's page.
 */

import { expect, test } from "vitest";
import { flushInCase } from "./inCase.ts";

/** What the late flush came to: "returned", or why it refused. */
let late: Promise<string> = Promise.resolve("never started");
let release: () => void = () => {};
/** Whether the late flush's step ran — the move a late case must not make. */
let stepped = false;

test("a case that ends while it waits for something of its own", async () => {
  const answered = flushInCase();
  await answered();
  const own = new Promise<void>((resolve) => {
    release = resolve;
  });
  late = (async () => {
    await own;
    // A STEP, as a case that holds the timers takes one: advancing them fires
    // whatever is armed — by then, the next case's timers.
    await answered(() => {
      stepped = true;
    });
  })().then(
    () => "returned",
    (cause: unknown) => (cause instanceof Error ? cause.message : String(cause)),
  );
});

test("is refused its next flush, rather than acting beside the next case", async () => {
  // THE NEXT CASE HAS A FLUSH OF ITS OWN, which a flush found anywhere but in
  // the case that asked would be.
  const answered = flushInCase();
  release();
  expect(await late).toMatch(/^answered: the case that rendered this page has ended/);
  // REFUSED BEFORE ITS STEP, not only before its flush.
  expect(stepped).toBe(false);
  // And the live case's own flush, step and all, is unaffected.
  let mine = false;
  await expect(
    answered(() => {
      mine = true;
    }),
  ).resolves.toBeUndefined();
  expect(mine).toBe(true);
});
