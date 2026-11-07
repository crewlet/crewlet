/**
 * The example key a task finder offers is the company's own.
 *
 * THE INVARIANT: an example a reader can try. A key from another company's
 * project finds nothing here, so the example takes the shape of this
 * company's first project, and there is none where it has no project.
 */

import { expect, test } from "vitest";

import { taskKeyHint } from "./projectKeys.ts";

// Mutation: write a fixed key into the hint and this goes red.
test("the example key is the company's first project's", () => {
  expect(taskKeyHint(new Set(["OPS", "ENG"]))).toBe("A key like OPS-1, or words from its title.");
  expect(taskKeyHint(new Set(["ENG"]))).toBe("A key like ENG-1, or words from its title.");
});

test("a company with no project gets no example key", () => {
  expect(taskKeyHint(new Set())).toBe("A task key, or words from its title.");
});
