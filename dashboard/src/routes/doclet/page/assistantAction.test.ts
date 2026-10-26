// @vitest-environment node

/** A new body for the open draft is Markdown within what a page holds. */

import { describe, expect, test } from "vitest";
import { PAGE_BODY_MAX_BYTES } from "~/contract/pages.ts";
import { pageBodyProblem } from "./assistantAction.ts";

describe("a proposed page body", () => {
  test("is taken when it is text a page can hold", () => {
    expect(pageBodyProblem("# Runbook\n\nStep one.")).toBeNull();
  });

  test("is refused past what a page holds, or carrying a NUL", () => {
    expect(pageBodyProblem("x".repeat(PAGE_BODY_MAX_BYTES + 1))).toMatch(/longer/);
    expect(pageBodyProblem("a\u0000b")).toMatch(/Markdown/);
  });
});
