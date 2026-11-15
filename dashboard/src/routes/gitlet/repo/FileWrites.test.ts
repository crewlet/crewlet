/**
 * Where a commit from the Files tab lands is what the engine's read resolved,
 * never how the address spells the revision.
 *
 * The invariants worth breaking a build over: the default branch (an address
 * naming no revision) commits to the branch the read resolved, at the commit
 * it stood at; a tag and a commit take no commit; and nothing is offered
 * before the read has said where the branch stands. Mutation: read the
 * branch off the address and the tag and default cases go red.
 */

import { expect, test } from "vitest";
import type { CodeGitAnswer } from "~/protocol/index.ts";
import { targetHeld, targetOf } from "./FileWrites.tsx";

const HEAD = "0123456789abcdef0123456789abcdef01234567";

const read = (ref: string): CodeGitAnswer =>
  ({ repo: "web", kind: "tree", resolved: { ref: { ref, oid: HEAD } } }) as CodeGitAnswer;

test("a branch's read commits to that branch at the commit it resolved", () => {
  expect(targetOf("", read("refs/heads/main"))).toEqual({ branch: "main", head: HEAD });
  expect(targetOf("feature/x", read("refs/heads/feature/x"))).toEqual({
    branch: "feature/x",
    head: HEAD,
  });
  expect(targetHeld(targetOf("", read("refs/heads/main")))).toBeUndefined();
});

test("a tag and a commit take no commit", () => {
  expect(targetOf("refs/tags/v1", read("refs/tags/v1")).branch).toBeNull();
  // A TAG NAMED BARE is a tag still: the resolution says so.
  expect(targetOf("v1", read("refs/tags/v1")).branch).toBeNull();
  expect(targetOf(HEAD, null)).toEqual({ branch: null, head: HEAD });
  expect(targetHeld(targetOf(HEAD, null))).toMatch(/Choose a branch/);
});

test("nothing is offered before the read says where the branch stands", () => {
  expect(targetHeld(targetOf("main", null))).toMatch(/Still reading/);
});
