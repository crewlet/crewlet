/**
 * What the page editor takes from the assistant: a new draft of the body
 * being edited (`page.draft.replace`).
 *
 * INTO THE OPEN DRAFT, NEVER THE PAGE. The body replaces what the editor
 * holds, exactly as typing would, while the editor is open and only then:
 * the save, its `base_version` and its note stay the person's, and the
 * editor's own undo takes the replacement back. While a merge waits on the
 * person, or a save is in flight, the draft is not theirs to replace either.
 */

import type { PageActionParse } from "~/app/assistant/context.ts";
import { PAGE_BODY_MAX_BYTES } from "~/contract/pages.ts";

/** A new body for the open draft. */
export interface PageDraft {
  readonly body: string;
}

const encoder = new TextEncoder();

/** Parse a new body, held to the bound the engine stores a page's body at. */
export function parsePageDraft(raw: unknown): PageActionParse<PageDraft> {
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) {
    return { ok: false, reason: "That is not a new draft of the page." };
  }
  const { body, ...rest } = raw as Record<string, unknown>;
  if (Object.keys(rest).length > 0) return { ok: false, reason: "A draft is the page's body." };
  if (typeof body !== "string" || body.includes("\u0000")) {
    return { ok: false, reason: "A page's body is Markdown text." };
  }
  if (encoder.encode(body).length > PAGE_BODY_MAX_BYTES) {
    return { ok: false, reason: "That body is longer than a page can hold." };
  }
  return { ok: true, value: { body } };
}
