/**
 * The remedy table is keyed exactly as the sentences are.
 *
 * `REFUSAL_SENTENCES` is held to the engine's own reasons by a Go gate
 * (`internal/authz`), so holding this table to it — key for key, and an
 * admission (a null sentence) exactly where the remedy is null — is what makes
 * a reason the engine grows a reason this file must decide a control for,
 * rather than one that silently falls back. Mutation: drop a row and the
 * first assertion fails; give an admission a remedy and the second does.
 */

import { expect, test } from "vitest";
import { REFUSAL_SENTENCES } from "~/contract/authority.ts";
import { AUTHORITY_REMEDIES, remedyFor } from "./authorityRemedy.ts";

test("every reason the sentences name has a remedy, and an admission has none", () => {
  expect(Object.keys(AUTHORITY_REMEDIES).sort()).toEqual(Object.keys(REFUSAL_SENTENCES).sort());
  for (const [reason, sentence] of Object.entries(REFUSAL_SENTENCES)) {
    expect([reason, AUTHORITY_REMEDIES[reason] === null]).toEqual([reason, sentence === null]);
  }
});

// THE GRANTS ARE THE WAY PAST ONLY WHERE A GRANT IS: a stale proof's are what
// admitted the reader, a seat-only verb's are nothing anybody could hold, and
// an artifact's `admin` is the override beside the share its owner makes.
// Mutation: give `role` the relations' row and its assertion fails.
test("a refusal names its access as the remedy only where access is one", () => {
  expect(remedyFor("no_grant", ["worklet"])).toEqual({ action: "sign_in", namesAccess: true });
  expect(remedyFor("not_lead", ["admin"])).toEqual({ action: "sign_in", namesAccess: true });
  expect(remedyFor("unnamed", [])).toEqual({ action: null, namesAccess: false });
  expect(remedyFor("step_up", ["admin"])).toEqual({ action: "confirm", namesAccess: false });
  expect(remedyFor("seat_only", ["admin"])).toEqual({ action: null, namesAccess: false });
  expect(remedyFor("role", ["admin"])).toEqual({ action: "try_again", namesAccess: false });
  expect(remedyFor("ceiling", [])).toEqual({ action: null, namesAccess: false });
  expect(remedyFor("assist_refused", ["admin"])).toEqual({ action: null, namesAccess: false });
  expect(remedyFor("assist_scope", [])).toEqual({ action: null, namesAccess: false });
  expect(remedyFor("from_a_newer_engine", [])).toEqual({ action: null, namesAccess: false });
});
