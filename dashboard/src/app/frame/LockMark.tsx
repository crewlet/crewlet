/**
 * The lock a navigation entry draws beside its name when the reader lacks the
 * access it takes: the kit's key glyph, and the access named for a screen
 * reader after the label. One mark for the sidebar's rows, the Settings
 * column and the header's section tabs, so a locked place reads the same
 * wherever it is offered (a tab that drew none looked open, then refused).
 *
 * Nothing at all where nothing is missing.
 */

import { KeyGlyph } from "@crewlethq/icons/glyphs";
import { accessNames, type Grant } from "~/lib/access.ts";

export function LockMark({ needs }: { needs: readonly Grant[] }) {
  if (needs.length === 0) return null;
  return (
    <>
      <KeyGlyph size="xs" aria-hidden="true" />
      <span className="sr-only">, needs {accessNames(needs)} access</span>
    </>
  );
}
