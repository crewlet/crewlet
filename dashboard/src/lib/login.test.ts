// @vitest-environment node

/**
 * A login is held to its holder's grammar before a form posts it. The table is
 * the dashboard's half; whether the grammar IS the engine's is
 * `internal/api/iamapi.TestTheDashboardChecksALoginByTheEnginesGrammar`'s.
 */

import { expect, test } from "vitest";
import { loginProblem, type LoginKind } from "./login.ts";

test.each<[LoginKind, string, RegExp | null]>([
  // What a person types for their own name, which the engine refuses.
  ["person", "Frank", /joined by dots/],
  ["person", "erin", /joined by dots/],
  ["person", "Erin NG", /joined by dots/],
  ["person", "ci:erin", /joined by dots/],
  // The controls: a person's login and a machine's, as the engine takes them.
  ["person", "erin.ng", null],
  ["person", "dana-sre.ops", null],
  ["machine", "ci:release", null],
  ["machine", "deploybot", /joined by a colon/],
  ["machine", "jane.doe", /joined by a colon/],
  ["machine", "pat:release", /names a credential/],
  ["person", `${"a".repeat(30)}.${"b".repeat(33)}`, null],
  ["person", `${"a".repeat(30)}.${"b".repeat(34)}`, /At most 64 characters/],
  // Empty is the form's to say: the field is required.
  ["person", "", null],
])("a %s login %j", (kind, login, want) => {
  const got = loginProblem(kind, login);
  if (want === null) expect(got).toBeNull();
  else expect(got).toMatch(want);
});
