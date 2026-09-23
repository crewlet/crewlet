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

import { needsSentence } from "./refusal.ts";
import { refusedGrants } from "~/protocol/rest.ts";

test("a refusal naming grants says which, and that the credential lacks them", () => {
  expect(needsSentence("Editing the organization", ["config:read", "fleet:operate"])).toBe(
    "Editing the organization needs config:read or fleet:operate, which the credential you presented does not carry.",
  );
});

test("a refusal naming none asks for a credential, not an operator token", () => {
  const said = needsSentence("Reading one pass", []);
  expect(said).toBe(
    "Reading one pass needs a credential the engine accepts. Sign in, or set a token.",
  );
  expect(said).not.toMatch(/operator/);
});

test("the grants are read off the envelope, and nothing else is taken for one", () => {
  expect(refusedGrants({ error: "unauthorized", grants: ["config:read", 3, null] })).toEqual([
    "config:read",
  ]);
  expect(refusedGrants({ error: "unauthorized" })).toEqual([]);
  expect(refusedGrants(null)).toEqual([]);
  expect(refusedGrants("not a body")).toEqual([]);
});
