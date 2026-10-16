// @vitest-environment node

/**
 * A login is held to its holder's grammar before a form posts it. The table is
 * the dashboard's half; whether the grammar IS the engine's is
 * `internal/api/iamapi.TestTheDashboardChecksALoginByTheEnginesGrammar`'s.
 */

import { expect, test } from "vitest";
import { loginKind, loginProblem, type LoginKind } from "./login.ts";

test.each<[LoginKind, string, RegExp | null]>([
  // What a person types for their own name, which the engine refuses.
  ["person", "Frank", /with dots, like jane\.doe/],
  ["person", "erin", /with dots, like jane\.doe/],
  ["person", "Erin NG", /with dots, like jane\.doe/],
  ["person", "ci:erin", /with dots, like jane\.doe/],
  // The controls: a person's login and a machine's, as the engine takes them.
  ["person", "erin.ng", null],
  ["person", "dana-sre.ops", null],
  ["machine", "ci:release", null],
  ["machine", "deploybot", /with colons, like ci:release/],
  ["machine", "jane.doe", /with colons, like ci:release/],
  ["machine", "pat:release", /can’t start with pat:/],
  ["person", `${"a".repeat(30)}.${"b".repeat(33)}`, null],
  ["person", `${"a".repeat(30)}.${"b".repeat(34)}`, /Use 64 characters or fewer \(this has 65\)/],
  // Empty is the form's to say: the field is required.
  ["person", "", null],
])("a %s login %j", (kind, login, want) => {
  const got = loginProblem(kind, login);
  if (want === null) expect(got).toBeNull();
  else expect(got).toMatch(want);
});

// WHOSE LOGIN IT IS, told by its shape alone: an unbound reader's remedy turns
// on it — a person with no seat is a fault an administrator mends, and a
// service account or a Tier A token's session is told how to bind — and the
// two grammars are disjoint, so no login is both. Mutation: test the machine
// grammar first with a looser pattern and a person's login reads as a
// machine's.
test.each<[string, LoginKind | null]>([
  ["jane.doe", "person"],
  ["dana-sre.ops", "person"],
  ["ci:release", "machine"],
  ["token:ops-7", "machine"],
  ["jane", null],
  ["Jane.Doe", null],
  ["", null],
])("the login %j is a %s login", (login, kind) => {
  expect(loginKind(login)).toBe(kind);
});
