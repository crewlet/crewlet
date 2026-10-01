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
 * from one still running after its time ran out. The case's own flush can.
 */

import { expect, test } from "vitest";
import { flushInCase } from "./inCase.ts";

/** What the late flush came to: "returned", or why it refused. */
let late: Promise<string> = Promise.resolve("never started");
let release: () => void = () => {};

test("a case that ends while it waits for something of its own", async () => {
  const answered = flushInCase();
  await answered();
  const own = new Promise<void>((resolve) => {
    release = resolve;
  });
  late = (async () => {
    await own;
    await answered();
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
  // And the live case's own flush is unaffected.
  await expect(answered()).resolves.toBeUndefined();
});
