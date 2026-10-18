/**
 * A keyed write's refusal: the access to ask an admin for where that access is
 * the way past the rule, and the engine's own words everywhere else.
 */

import { afterEach, expect, test, vi } from "vitest";
import { keyedWrite } from "./keyedWrite.ts";

function answer(status: number, body: unknown) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => new Response(JSON.stringify(body), { status })),
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
});

test("a refusal some access clears names that access, once", async () => {
  answer(403, {
    error: "unauthorized",
    message: "You don't have the access this needs (Worklet). Ask an admin for it.",
    reason: "no_grant",
    grants: ["worklet"],
  });
  const got = await keyedWrite({ method: "PATCH", path: "/artifacts/a", key: "k" });
  expect(got.kind === "refused" && got.text).toBe(
    "You need Worklet access for this. Ask an admin.",
  );
});

// AN ARTIFACT'S ROLE NAMES THE ADMIN OVERRIDE beside the share its owner
// makes, and that override is no remedy to send a reader to. Mutation: decide
// by the grants alone and this tells them they need Admin access.
test("a role on an artifact names no access, and is the engine's own sentence", async () => {
  const said =
    "Your role on this doesn't allow that. Whoever owns it can share it with you at a higher one.";
  answer(403, {
    error: "unauthorized",
    message: said,
    reason: "role",
    role: "editor",
    grants: ["admin"],
  });
  const got = await keyedWrite({ method: "PATCH", path: "/artifacts/a", key: "k" });
  expect(got.kind === "refused" && got.text).toBe(said);
});
