/**
 * The one reading of the engine's retry hint, which every retry path in the
 * dashboard takes — the socket's queries and watch, the shared health read and
 * the org builder's check.
 */

import { describe, expect, test } from "vitest";
import { RETRY_AFTER_MAX_MS, retryAfterMs, UNAVAILABLE_RETRY_MS } from "./retry.ts";
import { unavailableRetryMs } from "./socket.ts";

describe("a retry hint", () => {
  // EVERY HINT THE ENGINE FIXES IS WAITED OUT EXACTLY: the identity estate's
  // two seconds, an election's four, the health tick's five, the reconcile
  // poll's fifteen and a drain's thirty. Anything shorter would ask before the
  // node said it could answer; anything longer would hold a recovered node.
  test.each([2, 4, 5, 15, 30])("of %i seconds is waited out exactly", (seconds) => {
    expect(retryAfterMs(seconds)).toBe(seconds * 1_000);
  });

  // THE ONE HINT WITH NO BOUND OF ITS OWN is a backlog divided by a drain rate,
  // which runs to minutes; past the bound the screen asks again anyway.
  test("past the bound is asked again at the bound", () => {
    expect(retryAfterMs(31)).toBe(RETRY_AFTER_MAX_MS);
    expect(retryAfterMs(600)).toBe(RETRY_AFTER_MAX_MS);
  });

  // ZERO IS THE ANSWER: waiting will not change it, so nothing asks again on a
  // timer. Read as "soon", it was a five-second loop against a full log for as
  // long as the tab stayed open.
  test("of zero is never on a timer", () => {
    expect(retryAfterMs(0)).toBeNull();
  });
});

describe("an unavailable answer's wait", () => {
  test("is its hint, when the engine sent one", () => {
    expect(unavailableRetryMs({ code: "behind", detail: null, retryAfter: 12 })).toBe(12_000);
    expect(unavailableRetryMs({ code: "log_full", detail: null, retryAfter: 0 })).toBeNull();
  });

  // AN ANSWER WITH NO HINT waits what the engine says when it has nothing
  // better, rather than stopping: a frame from a node older than the field is
  // not a statement that waiting changes nothing.
  test("is the engine's own default when the answer carried none", () => {
    expect(unavailableRetryMs(null)).toBe(UNAVAILABLE_RETRY_MS);
  });
});
