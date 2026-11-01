/**
 * A harness's mark, wherever an entry's harness is named: the harness choice, the
 * entries table, the entry's header and the add review. Always beside the
 * harness's name, which carries the meaning; the mark is decoration.
 *
 * A built-in harness whose vendor's mark ships in @crewlethq/icons draws it
 * (`VendorMark`, which records each one's source): Claude Code the Claude
 * spark, OpenCode its own mark. Codex draws the kit's terminal glyph, as a
 * custom harness does: no mark of OpenAI's ships until an official file is
 * supplied.
 *
 * NAMED, the mark stands in for the harness's name: an entry's row line says the
 * model alone, so the mark carries the harness as its accessible name. Beside a
 * harness's written name it stays decoration. With `tooltip` it also shows the
 * name on hover, where nothing lies over it. NEVER A CONTROL: it is an image,
 * and inside a linked table row the row's link lies over it and takes the
 * press, which is what a press there means.
 */

import { Tooltip } from "@crewlethq/ui";
import { VendorMark, type Vendor } from "@crewlethq/icons";
import { SquareTerminalGlyph } from "@crewlethq/icons/glyphs";
import { product } from "./words.ts";

/** The vendor mark each harness draws, where @crewlethq/icons ships one. */
const MARKS: Partial<Record<string, Vendor>> = {
  "claude-code": "claude",
  opencode: "opencode",
};

/**
 * ONE SIZE WHEREVER A HARNESS IS NAMED: the harness cards, the table rows, the
 * entry's header and the review drew it at 28, 20, 28 and 32px, so one mark
 * read as four. 24px is the kit's medium icon step and sits on a two-line
 * table cell's centre without growing the row.
 */
export const MARK_SIZE = 24;

export function HarnessMark({
  harness,
  size = MARK_SIZE,
  named = false,
  tooltip = false,
}: {
  harness: string;
  size?: number;
  named?: boolean;
  tooltip?: boolean;
}) {
  const vendor = MARKS[harness];
  const mark = vendor ? (
    <VendorMark vendor={vendor} size={size} />
  ) : (
    <SquareTerminalGlyph size={size} />
  );
  if (!named) return mark;
  const name = product(harness);
  const labelled = (
    <span role="img" aria-label={name} className="models-cli-mark">
      {mark}
    </span>
  );
  return tooltip ? (
    <Tooltip content={name} placement="top">
      {labelled}
    </Tooltip>
  ) : (
    labelled
  );
}
