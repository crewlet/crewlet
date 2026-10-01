/**
 * The search vocabulary's one decision that is not a label: which mode a
 * search runs in when nobody chose one.
 *
 *  - THE DEFAULT IS NEVER A MODE THE ENGINE SAID IT CANNOT SERVE AS ASKED.
 *    With no embeddings provider that is Keyword, not a Hybrid the control
 *    draws disabled and the engine then degrades.
 *  - "NOT KNOWN YET" MOVES NOTHING: until an answer lists its modes the
 *    default is the engine's own, Hybrid.
 *  - A MODE THE READER NAMED IS THEIRS, served or not.
 */

import { expect, test } from "vitest";

import { defaultSearchMode, resolveSearchMode } from "./search.ts";

test("the default is Hybrid where Hybrid is served", () => {
  expect(defaultSearchMode({ modes: ["hybrid", "keyword", "semantic"] })).toBe("hybrid");
});

test("with no embeddings provider the default is the first mode served", () => {
  expect(defaultSearchMode({ modes: ["keyword"] })).toBe("keyword");
});

test("before anything says which modes exist, the default is the engine's", () => {
  expect(defaultSearchMode(null)).toBe("hybrid");
  expect(defaultSearchMode({ modes: [] })).toBe("hybrid");
});

test("a named mode is kept, and an unknown one is the engine's default", () => {
  expect(resolveSearchMode("hybrid", { modes: ["keyword"] })).toBe("hybrid");
  expect(resolveSearchMode("semantic", { modes: ["keyword"] })).toBe("semantic");
  expect(resolveSearchMode("vibes", { modes: ["keyword"] })).toBe("hybrid");
  expect(resolveSearchMode(null, { modes: ["keyword"] })).toBe("keyword");
});
