/**
 * The one reading of the engine's retry hint, which every retry path in the
 * dashboard takes — the socket's queries and watch, the org builder's check
 * and the REST reads a screen asks again on its own.
 */

import { describe, expect, test } from "vitest";
import { RETRY_AFTER_MAX_MS, UNAVAILABLE_RETRY_MS } from "../contract/retry.ts";
import { REQUEST_TIMEOUT_MS, RestError, restFailure, restRetryMs } from "./rest.ts";
import {
  retryAfterMs,
  UNANSWERED_RETRY_BASE_MS,
  UNANSWERED_RETRY_MAX_MS,
  unansweredRetryMs,
} from "./retry.ts";
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

describe("a request nobody answered", () => {
  // IT BACKS OFF, from a second to the cap: asked at once, an engine that is
  // restarting is hammered with requests that each wait out the deadline.
  test("waits a second, then twice as long each time, up to the cap", () => {
    expect([1, 2, 3, 4, 5, 6, 7].map(unansweredRetryMs)).toEqual([
      1_000, 2_000, 4_000, 8_000, 16_000, 30_000, 30_000,
    ]);
    expect(unansweredRetryMs(1)).toBe(UNANSWERED_RETRY_BASE_MS);
    expect(unansweredRetryMs(1_000)).toBe(UNANSWERED_RETRY_MAX_MS);
  });

  // THE CAP IS ONE ATTEMPT'S OWN DEADLINE, which is the whole of its claim: an
  // engine that recovers is never noticed later than one more attempt would
  // have taken to fail. `retry.ts` imports nothing, so it states the number,
  // and this is what keeps the two from drifting apart.
  test("is never waited longer than one attempt may take", () => {
    expect(UNANSWERED_RETRY_MAX_MS).toBe(REQUEST_TIMEOUT_MS);
  });
});

describe("a failed REST read's wait", () => {
  const engine = (retryAfter: number | null) =>
    new RestError(503, { error: "identity_unavailable" }, retryAfter);
  const polled = { cadence: 60_000, unanswered: 0 };
  const unpolled = { cadence: null, unanswered: 0 };

  test("is the hint on a 503 the engine wrote, bounded like every hint", () => {
    expect(restRetryMs(engine(12), polled)).toBe(12_000);
    expect(restRetryMs(engine(600), polled)).toBe(RETRY_AFTER_MAX_MS);
  });

  // NO `Retry-After` ON A 503 THE ENGINE WROTE is its zero: never on a timer,
  // whatever the screen's own cadence would have been.
  test("is never on a timer for a 503 the engine wrote with no Retry-After", () => {
    expect(restRetryMs(engine(null), polled)).toBeNull();
  });

  // A READ NOBODY ANSWERED BACKS OFF, on a screen with no cadence of its own
  // too: the live socket can be up the whole time it fails — one request past
  // its deadline, one dropped on the way — so its coming back is not an event
  // anybody can wait for, and a read nothing polls was read again only on a
  // reload.
  test.each([
    ["status 0, past its deadline", new RestError(0, { error: "unreachable" })],
    ["a gateway's 504", new RestError(504, { error: "unreadable_body" })],
    ["a proxy's 503", new RestError(503, {}, 5)],
  ])("after %s is the backoff, whatever the cadence", (_, err) => {
    expect(restRetryMs(err, { cadence: null, unanswered: 1 })).toBe(1_000);
    expect(restRetryMs(err, { cadence: 60_000, unanswered: 3 })).toBe(4_000);
    expect(restRetryMs(err, { cadence: 4_000, unanswered: 9 })).toBe(UNANSWERED_RETRY_MAX_MS);
  });

  // EVERY OTHER FAILURE carries no hint, because nobody at the engine decided
  // one: a fault, a refusal on authority. Each waits the screen's own.
  test.each([
    ["a fault", new RestError(500, { error: "internal_error" })],
    ["a refusal on authority", new RestError(403, { error: "unauthorized" })],
    ["something that is not a refusal at all", new TypeError("boom")],
  ])("is the screen's own after %s", (_, err) => {
    expect(restRetryMs(err, polled)).toBe(60_000);
    expect(restRetryMs(err, unpolled)).toBeNull();
  });
});

// AND WHAT ITS BANNER SAYS, which is what says whether the screen asks again:
// a REST screen that mapped its own failures forgot a different case each
// time, and drew nothing, or a fault for a node catching up.
describe("a failed REST read's banner", () => {
  test.each([
    ["a refusal on authority", new RestError(403, { error: "unauthorized" }), "unauthorized"],
    ["nobody signed in", new RestError(401, { error: "invalid_token" }), "unauthorized"],
    // NOT `closed`: that banner says the socket went away and promises a read
    // once it is back, and the socket is routinely up the whole time.
    ["a request that never arrived", new RestError(0, { error: "unreachable" }), "unanswered"],
    ["the engine's 503", new RestError(503, { error: "identity_unavailable" }, 2), "unavailable"],
    ["the engine's 503 with no hint", new RestError(503, { error: "unavailable" }), "unavailable"],
    // NOT A FAULT ON THE NODE: the engine never wrote these.
    ["a proxy's 503", new RestError(503, {}, 5), "unanswered"],
    ["a gateway's page", new RestError(502, { error: "unreadable_body" }), "unanswered"],
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
