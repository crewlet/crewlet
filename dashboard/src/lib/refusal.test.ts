// @vitest-environment node
/**
 * What a screen says when the engine refused it.
 *
 * The sentence a refusal ON AUTHORITY is drawn with — the access it names —
 * is `lib/access.ts`'s (`needsWords`, held by `access.test.ts`); what is held
 * here is the rest: the engine's own words for a code, and the grants read
 * off the envelope.
 */

import { expect, test } from "vitest";

import { authorityWords, refusalText } from "./refusal.ts";
import { RestError, refusedGrants } from "~/protocol/rest.ts";

// THE SIGN-IN SURFACE'S WORDS ARE THE ENGINE'S. A failed sign-in is one
// refusal on purpose, so the only sentence a person may be shown for it is
// the one the engine wrote for its code.
test("a refusal with no detail is the engine's own sentence, and nothing else", () => {
  const err = new RestError(401, {
    error: "sign_in_refused",
    message: "Those sign-in details were not accepted. Check them and try again.",
  });
  expect(refusalText(err)).toBe(
    "Those sign-in details were not accepted. Check them and try again.",
  );
});

test("a refusal that names what to change says it, with its hint, as sentences", () => {
  const err = new RestError(400, {
    error: "invalid_body",
    message:
      "The request body is not one this endpoint accepts — its shape, or a value in it — and nothing was changed.",
    detail: "That code doesn’t match.",
    hint: "Check that your authenticator app shows the right account and your device’s clock is correct.",
  });
  expect(refusalText(err)).toBe(
    "That code doesn’t match. Check that your authenticator app shows the right account and your device’s clock is correct.",
  );
});

// AN ADDRESS LEADING THE ENGINE'S DETAIL IS A VALUE (`internal/api/auth`'s
// cross-site refusal names the origin first), so it is not capitalised.
test("a refusal whose detail opens with an address keeps the address as written", () => {
  const err = new RestError(403, {
    error: "csrf_origin",
    message: "This request came from a page this deployment does not serve.",
    detail: "http://localhost:5199 isn't an address for this deployment.",
    hint: "Set this deployment's address in `api.external_url`, and any other hostname in `api.auth.allowed_origins`.",
  });
  expect(refusalText(err)).toBe(
    "http://localhost:5199 isn't an address for this deployment. Set this deployment's address in `api.external_url`, and any other hostname in `api.auth.allowed_origins`.",
  );
});

test("a throttled attempt says how long, from its Retry-After", () => {
  const err = new RestError(429, { error: "throttled", message: "Too many failed attempts." }, 1);
  expect(refusalText(err)).toBe("Too many failed attempts. Try again in 1s.");
  // A LONG WAIT IS A TIME TO PLAN BY, never a count of seconds to divide.
  const long = new RestError(
    429,
    { error: "throttled", message: "Too many failed attempts." },
    81155,
  );
  expect(refusalText(long)).toBe("Too many failed attempts. Try again in 22h 33m.");
});

// NOBODY ANSWERED, so nothing here may say anything was refused.
test("a request the engine never answered is not called a refusal", () => {
  const err = new RestError(0, { error: "unreachable", detail: "Failed to fetch" });
  expect(refusalText(err)).toBe("No answer came back from the engine. Failed to fetch.");
  expect(refusalText(err)).not.toMatch(/refused|not accepted/);
});

test("the grants are read off the envelope, and nothing else is taken for one", () => {
  expect(refusedGrants({ error: "unauthorized", grants: ["admin", 3, null] })).toEqual(["admin"]);
  expect(refusedGrants({ error: "unauthorized" })).toEqual([]);
  expect(refusedGrants(null)).toEqual([]);
  expect(refusedGrants("not a body")).toEqual([]);
});

// A REFUSAL ON AUTHORITY IS TOLD BY WHAT REFUSED, never by whether it named
// access: only a 401 asks for a sign-in. A 403 naming no access went to the
// sign-in sentence too, so on Audit, Secrets, the backups dialog and
// Integrations a signed-in person a rule refused was told to sign in, while
// every screen drawing the same refusal through QueryState said no access.
//
// Mutation: answer a 403 naming no access with the sign-in sentence, or tell
// a socket refusal apart from the same refusal over REST, and a row goes red.
test("only a 401 asks for a sign-in; a rule's refusal is the rule's own words", () => {
  const signIn = "Sign in to see this.";
  const rows: [string, RestError | Parameters<typeof authorityWords>[1], string][] = [
    ["a 401", new RestError(401, { error: "unauthorized" }), signIn],
    [
      "a rule naming access",
      new RestError(403, { error: "unauthorized", reason: "no_grant", grants: ["admin"] }),
      "Taking a backup needs Admin access. An admin can give it to you.",
    ],
    [
      "the same rule on the socket",
      { reason: "no_grant", grants: ["admin"] },
      "Taking a backup needs Admin access. An admin can give it to you.",
    ],
    [
      "a rule naming none",
      new RestError(403, { error: "unauthorized", reason: "token_refused", grants: [] }),
      "Access tokens can’t do this. Sign in as yourself.",
    ],
    [
      "a refusal saying nothing more",
      new RestError(403, { error: "unauthorized" }),
      "You don’t have access to this.",
    ],
    ["a socket refusal saying nothing more", null, "You don’t have access to this."],
  ];
  for (const [what, refused, said] of rows) {
    expect(authorityWords("Taking a backup", refused), what).toBe(said);
  }
  for (const [what, refused] of rows.slice(1)) {
    expect(authorityWords("Taking a backup", refused), what).not.toBe(signIn);
  }
});

// A PROOF GONE STALE NAMES THE ACCESS THAT ADMITTED THE READER, so a step-up
// they did not confirm is the engine's sentence for it, never "needs Admin
// access" to an admin. Mutation: name the access any 403 carries and this
// fails.
test("a 403 whose rule access does not clear is the engine's sentence, whatever it names", () => {
  const stale = new RestError(403, {
    error: "step_up_required",
    message: "Confirm it's you to do this, then try again.",
    reason: "step_up",
    window: "step_up",
    grants: ["admin"],
  });
  expect(authorityWords("Taking a backup", stale)).toBe(
    "Confirm it's you to do this, then try again.",
  );
});
