/**
 * The one reading of the engine's retry hint, which every retry path in the
 * dashboard takes — the socket's queries and watch, the shared health read,
 * the org builder's check and the REST reads a screen asks again on its own.
 */

import { describe, expect, test } from "vitest";
import { RestError, restFailure, restRetryMs } from "./rest.ts";
import { RETRY_AFTER_MAX_MS, retryAfterMs, UNAVAILABLE_RETRY_MS } from "./retry.ts";
import { unavailableRetryMs } from "./socket.ts";

describe("a retry hint", () => {
  // EVERY HINT THE ENGINE FIXES IS WAITED OUT EXACTLY: the identity estate's
  // and an undecidable authority's two seconds, a busy surface's three, an
  // election's four, the health tick's five, the reconcile poll's fifteen and
  // a drain's thirty — the list internal/api's gate holds under the bound.
  // Anything shorter would ask before the node said it could answer; anything
  // longer would hold a recovered node.
  test.each([2, 3, 4, 5, 15, 30])("of %i seconds is waited out exactly", (seconds) => {
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

describe("a failed REST read's wait", () => {
  const engine = (retryAfter: number | null) =>
    new RestError(503, { error: "identity_unavailable" }, retryAfter);

  test("is the hint on a 503 the engine wrote, bounded like every hint", () => {
    expect(restRetryMs(engine(12), 60_000)).toBe(12_000);
    expect(restRetryMs(engine(600), 60_000)).toBe(RETRY_AFTER_MAX_MS);
  });

  // NO `Retry-After` ON A 503 THE ENGINE WROTE is its zero: never on a timer,
  // whatever the screen's own cadence would have been.
  test("is never on a timer for a 503 the engine wrote with no Retry-After", () => {
    expect(restRetryMs(engine(null), 60_000)).toBeNull();
  });

  // EVERY OTHER FAILURE carries no hint, because nobody at the engine decided
  // one: a 503 a proxy wrote (no engine code), a fault, a refusal on
  // authority, a request that never arrived. Each waits the screen's own.
  test.each([
    ["a proxy's 503", new RestError(503, {}, 5)],
    ["a fault", new RestError(500, { error: "internal_error" })],
    ["a refusal on authority", new RestError(403, { error: "unauthorized" })],
    ["no answer", new RestError(0, { error: "unreachable" })],
    ["something that is not a refusal at all", new TypeError("boom")],
  ])("is the screen's own after %s", (_, err) => {
    expect(restRetryMs(err, 60_000)).toBe(60_000);
    expect(restRetryMs(err, null)).toBeNull();
  });
});

// AND WHAT ITS BANNER SAYS, which is what says whether the screen asks again:
// a REST screen that mapped its own failures forgot a different case each
// time, and drew nothing, or a fault for a node catching up.
describe("a failed REST read's banner", () => {
  test.each([
    ["a refusal on authority", new RestError(403, { error: "unauthorized" }), "unauthorized"],
    ["nobody signed in", new RestError(401, { error: "invalid_token" }), "unauthorized"],
    ["a request that never arrived", new RestError(0, { error: "unreachable" }), "closed"],
    ["the engine's 503", new RestError(503, { error: "identity_unavailable" }, 2), "unavailable"],
    ["the engine's 503 with no hint", new RestError(503, { error: "unavailable" }), "unavailable"],
    ["a proxy's 503", new RestError(503, {}, 5), "query_failed"],
    ["a fault", new RestError(500, { error: "internal_error" }), "query_failed"],
    ["something that is not a refusal at all", new TypeError("boom"), "query_failed"],
  ])("after %s is the matching code", (_, err, code) => {
    expect(restFailure(err).error).toBe(code);
  });

  test("carries what lets the banner say what would change it", () => {
    expect(restFailure(new RestError(503, { error: "unavailable" }, 12)).refusal).toEqual({
      code: null,
      detail: null,
      retryAfter: 12,
    });
    expect(
      restFailure(new RestError(403, { error: "unauthorized", grants: ["secrets:read"] })).refusal,
    ).toEqual({ reason: "unauthorized", grants: ["secrets:read"] });
  });
});
