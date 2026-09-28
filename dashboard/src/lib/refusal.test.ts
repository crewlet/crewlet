// @vitest-environment node
/**
 * What a screen says when the engine refused it on authority.
 *
 * Every screen this replaced said "needs an operator token", which sent a
 * signed-in reader to find a credential they have no use for. The sentence
 * names the grants the REFUSAL named, and says "a credential" only where
 * nothing the engine accepted was presented.
 */

import { expect, test } from "vitest";

import { needsSentence, refusalText } from "./refusal.ts";
import { RestError, refusedGrants } from "~/protocol/rest.ts";

test("a refusal naming grants says which, and that the credential lacks them", () => {
  expect(needsSentence("Editing the organization", ["config:read", "fleet:operate"])).toBe(
    "Editing the organization needs config:read or fleet:operate, which the credential you presented does not carry.",
  );
});

test("a refusal naming none asks for a credential, not an operator token", () => {
  const said = needsSentence("Reading one pass", []);
  expect(said).toBe("Reading one pass needs a credential the engine accepts. Sign in.");
  expect(said).not.toMatch(/operator|token/);
});

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
    message: "The request body is not in the shape this endpoint accepts.",
    detail: "that code does not match the secret",
    hint: "check the authenticator app has the right account",
  });
  expect(refusalText(err)).toBe(
    "That code does not match the secret. Check the authenticator app has the right account.",
  );
});

test("a throttled attempt says how long, from its Retry-After", () => {
  const err = new RestError(429, { error: "throttled", message: "Too many failed attempts." }, 1);
  expect(refusalText(err)).toBe("Too many failed attempts. Try again in 1 second.");
});

// NOBODY ANSWERED, so nothing here may say anything was refused.
test("a request the engine never answered is not called a refusal", () => {
  const err = new RestError(0, { error: "unreachable", detail: "Failed to fetch" });
  expect(refusalText(err)).toBe("No answer came back from the engine. Failed to fetch.");
  expect(refusalText(err)).not.toMatch(/refused|not accepted/);
});

test("the grants are read off the envelope, and nothing else is taken for one", () => {
  expect(refusedGrants({ error: "unauthorized", grants: ["config:read", 3, null] })).toEqual([
    "config:read",
  ]);
  expect(refusedGrants({ error: "unauthorized" })).toEqual([]);
  expect(refusedGrants(null)).toEqual([]);
  expect(refusedGrants("not a body")).toEqual([]);
});
