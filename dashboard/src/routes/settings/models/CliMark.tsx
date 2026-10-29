/**
 * A coding CLI's mark, wherever an entry's CLI is named: the CLI choice, the
 * entries table, the entry's header and the add review. Always beside the
 * CLI's name, which carries the meaning; the mark is decoration.
 *
 * A built-in CLI whose vendor's mark ships in @crewlethq/icons draws it
 * (`VendorMark`, which records each one's source): Claude Code the Claude
 * spark, OpenCode its own mark. Codex draws the kit's terminal glyph, as a
 * custom CLI does: no mark of OpenAI's ships until an official file is
 * supplied.
 *
 * NAMED, the mark stands in for the CLI's name: an entry's row line says the
 * model alone, so the mark carries the CLI as its accessible name. Beside a
 * CLI's written name it stays decoration. With `tooltip` it also shows the
 * name on hover. Inside a linked table row the row's link lies over its
 * cells, so a mark that shows a tooltip there is raised above the link
 * (`onPress`, the grid's `data-above-row`) and opens the row's entry itself:
 * raised and inert, it would be a spot in the row that opened nothing. The
 * keyboard still reaches the entry through the row's own link, whose name
 * starts with the mark's.
 */

import { Tooltip } from "@crewlethq/ui";
import { VendorMark, type Vendor } from "@crewlethq/icons";
import { SquareTerminalGlyph } from "@crewlethq/icons/glyphs";
import { product } from "./words.ts";

/** The vendor mark each CLI draws, where @crewlethq/icons ships one. */
const MARKS: Partial<Record<string, Vendor>> = {
  "claude-code": "claude",
  opencode: "opencode",
};

/**
 * ONE SIZE WHEREVER A CLI IS NAMED: the CLI cards, the table rows, the
 * entry's header and the review drew it at 28, 20, 28 and 32px, so one mark
 * read as four. 24px is the kit's medium icon step and sits on a two-line
 * table cell's centre without growing the row.
 */
export const MARK_SIZE = 24;

export function CliMark({
  agent,
  size = MARK_SIZE,
  named = false,
  tooltip = false,
  onPress,
}: {
  agent: string;
  size?: number;
  named?: boolean;
  tooltip?: boolean;
  /** Raises the mark above a linked row, and does what the row does. */
  onPress?: (() => void) | undefined;
}) {
  const vendor = MARKS[agent];
  const mark = vendor ? (
    <VendorMark vendor={vendor} size={size} />
  ) : (
    <SquareTerminalGlyph size={size} />
  );
  if (!named) return mark;
  const name = product(agent);
  const labelled = (
    <span
      role="img"
      aria-label={name}
      className={onPress ? "models-cli-mark models-cli-mark--pressable" : "models-cli-mark"}
      data-above-row={onPress ? "" : undefined}
      onClick={onPress}
    >
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
