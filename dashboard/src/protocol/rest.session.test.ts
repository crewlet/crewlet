/**
 * What a REST refusal says about the SESSION, noted where every request
 * passes — and the two facts a refusal carries that a person is shown: the
 * engine's own sentence, and the wait a `429` names.
 *
 * The reason this is the transport's job and not a screen's: a screen that
 * recognised a lost session itself would be one more screen that forgot to,
 * and the one that forgot would sit on a refusal banner a person cannot act
 * on while the rest of the product sent them to sign in.
 */

import { afterEach, describe, expect, test, vi } from "vitest";

import { currentSessionNeed, rest, RestError, sessionRestored } from "./index.ts";

afterEach(() => {
  vi.unstubAllGlobals();
  sessionRestored();
});

function answering(status: number, body: unknown, headers: Record<string, string> = {}) {
  vi.stubGlobal(
    "fetch",
    vi.fn(
      async () =>
        new Response(JSON.stringify(body), {
          status,
          headers: { "Content-Type": "application/json", ...headers },
        }),
    ),
  );
}

async function refusal(): Promise<RestError> {
  const err = await rest.get("/secrets").catch((e: unknown) => e);
  expect(err).toBeInstanceOf(RestError);
  return err as RestError;
}

describe("a refusal and the session", () => {
  test("a 401 on a guarded route is a browser that needs to sign in", async () => {
    answering(401, { error: "invalid_token", message: "This request needs a credential." });
    await refusal();
    expect(currentSessionNeed()).toBe("sign_in");
  });

  // THE CONTROL, and it is the half that keeps a sign-in form usable: the
  // engine answers a wrong password and a code prompt with 401 too, and both
  // are answers about what was TYPED. Read as a lost session, the sign-in
  // screen would be routed to itself on every mistyped password.
  test("a 401 about typed details says nothing about the session", async () => {
    for (const code of ["sign_in_refused", "second_factor_required"]) {
      answering(401, { error: code });
      await refusal();
      expect(currentSessionNeed(), code).toBeNull();
    }
  });

  test("a 403 that says the session may only enrol asks for the enrolment", async () => {
    answering(403, { error: "second_factor_enrolment_required" });
    await refusal();
    expect(currentSessionNeed()).toBe("second_factor");
  });

  // A REFUSAL ON AUTHORITY IS ABOUT THE REQUEST: the reader is signed in and
  // lacks a grant, and sending them to sign in would lose the screen for a
  // credential that is fine.
  test("a 403 on authority leaves the session alone", async () => {
    answering(403, { error: "unauthorized", grants: ["secrets:read"] });
    await refusal();
    expect(currentSessionNeed()).toBeNull();
  });

  test("a sign-in that lands clears the need", async () => {
    answering(401, { error: "invalid_token" });
    await refusal();
    sessionRestored();
    expect(currentSessionNeed()).toBeNull();
  });
});

describe("what a refusal carries for a person", () => {
  test("the engine's own sentence rides beside the code", async () => {
    answering(401, {
      error: "sign_in_refused",
      message: "Those sign-in details were not accepted. Check them and try again.",
    });
    const err = await refusal();
    expect(err.code).toBe("sign_in_refused");
    expect(err.sentence).toBe("Those sign-in details were not accepted. Check them and try again.");
  });

  test("a 429's wait is read off its Retry-After, in seconds", async () => {
    answering(429, { error: "throttled" }, { "Retry-After": "16" });
    expect((await refusal()).retryAfter).toBe(16);
  });

  // NO HEADER IS NOT ZERO. A 503 with no Retry-After is the engine saying
  // waiting will not help, which a screen must not render as "try again now".
  test("an answer with no Retry-After carries none", async () => {
    answering(503, { error: "unavailable" });
    expect((await refusal()).retryAfter).toBeNull();
  });
});
