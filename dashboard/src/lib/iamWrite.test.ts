/**
 * What an `/iam` write came to, read off the engine's answer.
 *
 * Four answers a person acts on differently: done (and whether this node has
 * applied it), unknown (retried under the key the engine handed back, or —
 * on a route that reads none — issued again), and refused, in the engine's
 * words with the grants that would admit and the steps of an edit that had
 * already landed.
 */

import { afterEach, expect, test, vi } from "vitest";
import { iamWrite } from "./iamWrite.ts";

function answer(status: number, body: unknown) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => new Response(JSON.stringify(body), { status })),
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
});

test("a 202 is done and pending, a 200 done and applied", async () => {
  answer(202, { outcome: "pending", position: "p" });
  expect(await iamWrite({ method: "PATCH", path: "/iam/people/p", key: "k" })).toEqual({
    kind: "done",
    pending: true,
    body: { outcome: "pending", position: "p" },
  });
  answer(200, { outcome: "applied" });
  const done = await iamWrite({ method: "PATCH", path: "/iam/people/p", key: "k" });
  expect(done.kind === "done" && done.pending).toBe(false);
});

test("an unknown answer keeps the key the engine handed back", async () => {
  answer(503, { error: "unavailable", outcome: "unknown", op_id: "scoped-key" });
  expect(await iamWrite({ method: "DELETE", path: "/iam/people/p", key: "sent" })).toMatchObject({
    kind: "unknown",
    key: "scoped-key",
  });
});

// NO ANSWER AT ALL is unknown too, and the retry is the key that was sent.
test("a request nobody answered is unknown under the key that was sent", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => {
      throw new TypeError("network down");
    }),
  );
  expect(await iamWrite({ method: "DELETE", path: "/iam/people/p", key: "sent" })).toMatchObject({
    kind: "unknown",
    key: "sent",
  });
});

// A ROUTE THAT READS NO KEY says what the engine said to do: issue again.
test("an unknown on an unkeyed route carries no key and the engine's own remedy", async () => {
  answer(503, {
    error: "unavailable",
    outcome: "unknown",
    op_id: "k",
    detail: "no link was issued: issue again",
  });
  const got = await iamWrite({ method: "POST", path: "/iam/people/p/password-reset" });
  expect(got).toMatchObject({ kind: "unknown", key: "" });
  expect(got.kind === "unknown" && got.text).toMatch(/No link was issued: issue again/);
});

// A REFUSAL PART WAY THROUGH AN EDIT says what changed before it. Mutation:
// drop `landed` and the half-landed edit reads as one that changed nothing.
test("a refusal is the engine's words, the grants that admit, and what had landed", async () => {
  answer(403, {
    error: "unauthorized",
    message: "The credential you presented does not carry the grant this request needs.",
    reason: "conferral",
    grants: ["people:manage"],
    landed: ["identity"],
  });
  const got = await iamWrite({ method: "PATCH", path: "/iam/people/p", key: "k" });
  expect(got.kind).toBe("refused");
  const text = got.kind === "refused" ? got.text : "";
  expect(text).toMatch(/does not carry the grant/);
  expect(text).toMatch(/people:manage would admit you/);
  expect(text).toMatch(/What did change: the login and seat\./);
});

// A STEP-UP REFUSAL NAMES NO GRANT AS MISSING: the grants it carries are the
// ones that admitted the caller. Mutation: name grants on every refusal and a
// holder of people:manage is told people:manage would admit them.
test("a step-up refusal is about confirming, never a grant", async () => {
  answer(403, {
    error: "step_up_required",
    message: "This action needs you to have confirmed who you are recently.",
    reason: "step_up",
    window: "step_up",
    grants: ["people:manage"],
  });
  const got = await iamWrite({ method: "POST", path: "/iam/credentials" });
  const text = got.kind === "refused" ? got.text : "";
  expect(text).toMatch(/confirmed who you are/);
  expect(text).not.toMatch(/would admit you/);
});
