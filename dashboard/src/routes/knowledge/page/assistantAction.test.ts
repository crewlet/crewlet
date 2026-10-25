// @vitest-environment node

/** A new draft of a page's body is plain Markdown, within what a page holds, and nothing else. */

import { describe, expect, test } from "vitest";
import { PAGE_BODY_MAX_BYTES } from "~/contract/pages.ts";
import { parsePageDraft } from "./assistantAction.ts";

describe("page.draft.replace", () => {
  test("takes a body", () => {
    expect(parsePageDraft({ body: "# Runbook\n\nStep one." })).toEqual({
      ok: true,
      value: { body: "# Runbook\n\nStep one." },
    });
  });

  test("refuses a body past what a page holds, a save smuggled beside it, and what is not text", () => {
    expect(parsePageDraft({ body: "x".repeat(PAGE_BODY_MAX_BYTES + 1) }).ok).toBe(false);
    expect(parsePageDraft({ body: "x", base_version: 3 }).ok).toBe(false);
    expect(parsePageDraft({ body: 3 }).ok).toBe(false);
    expect(parsePageDraft("body").ok).toBe(false);
  });
});
