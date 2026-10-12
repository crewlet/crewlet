// @vitest-environment node

/**
 * A page's body as a reader reads it: the title is said once.
 *
 * A seat opens a page with `# {title}` more often than not, so the page drew
 * its title twice and every list of changes named a new page and then named
 * it again as its note. One rule decides what "the same words" means for both.
 */

import { describe, expect, test } from "vitest";
import { repeatsTitle, withoutRepeatedTitle } from "./body.ts";

describe("what repeats a page's title", () => {
  test.each([
    ["the title itself", "Split modes", true],
    ["another case", "split MODES", true],
    ["other spacing", "  Split   modes ", true],
    ["a heading's closing hashes", "Split modes ##", true],
    ["a line that starts with it", "Split modes: equal, exact and percent", false],
    ["a different line", "Equal, exact amounts and percentages.", false],
    ["nothing at all", "", false],
  ])("%s", (_name, line, repeats) => {
    expect(repeatsTitle(line, "Split modes")).toBe(repeats);
  });

  // AN EMPTY TITLE IS NEVER REPEATED: a purged page's change has no title,
  // and an empty note is not its title said again.
  test("an empty title repeats nothing", () => {
    expect(repeatsTitle("", "")).toBe(false);
    expect(repeatsTitle("Anything", "")).toBe(false);
  });
});

describe("the body without its repeated title", () => {
  test("drops a first heading that only repeats the title, and the blank line after it", () => {
    expect(withoutRepeatedTitle("# Split modes\n\nEqual, exact and percent.", "Split modes")).toBe(
      "Equal, exact and percent.",
    );
  });

  test("keeps any other opening heading, which is content", () => {
    const body = "## Overview\n\nEqual, exact and percent.";
    expect(withoutRepeatedTitle(body, "Split modes")).toBe(body);
  });

  test("keeps a title that is not on the first line", () => {
    const body = "Intro.\n\n# Split modes";
    expect(withoutRepeatedTitle(body, "Split modes")).toBe(body);
  });
});
