// @vitest-environment node

import { expect, test } from "vitest";

import { isWholeReference, wholeReferenceName } from "./references.ts";

// A WHOLE REFERENCE IS ONE `${NAME}` AND NOTHING ELSE, trimmed as the engine
// trims it; what the shell would expand and the engine leaves alone is none.
test("a whole reference is one name between braces and nothing else", () => {
  expect(wholeReferenceName("${GITLAB_TOKEN}")).toBe("GITLAB_TOKEN");
  expect(wholeReferenceName("  ${_X1}\n")).toBe("_X1");
  for (const value of ["Bearer ${TOKEN}", "${1}", "${line#host=}", "$TOKEN", "${A}${B}", ""]) {
    expect(isWholeReference(value)).toBe(false);
  }
  expect(isWholeReference(42)).toBe(false);
});
