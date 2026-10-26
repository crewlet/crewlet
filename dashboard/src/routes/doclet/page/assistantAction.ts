/**
 * The rule a new body for the page editor's open draft is held to, when the
 * assistant proposes one (`draft.set` on its `body`, `Editor.tsx`).
 *
 * INTO THE OPEN DRAFT, NEVER THE PAGE. The body replaces what the editor
 * holds, exactly as typing would, while the editor is open and only then:
 * the save, its `base_version` and its note stay the person's, and the
 * editor's own undo takes the replacement back. While a merge waits on the
 * person, or a save is in flight, the draft is not theirs to replace either.
 */

import { PAGE_BODY_MAX_BYTES } from "~/contract/pages.ts";

const encoder = new TextEncoder();

/** Why a proposed body cannot be the draft's, or null where it can. */
export function pageBodyProblem(body: string): string | null {
  if (body.includes("\u0000")) return "A page's body is Markdown text.";
  if (encoder.encode(body).length > PAGE_BODY_MAX_BYTES) {
    return "That body is longer than a page can hold.";
  }
  return null;
}
