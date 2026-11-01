/**
 * One value of a proposal's change row, drawn as what it is.
 *
 * ONE RULE FOR EVERY ROW a card draws, the engine's (`changeRowsOf`) and a
 * screen's own (`previewRow`), so a value reads the same whichever drew it:
 *
 * - A LIST IS A LIST: its items one a line (`ChangeListItems`), never joined
 *   into one line with commas or semicolons.
 * - A JSON DOCUMENT (a request's body) is code, in a code block.
 * - TEXT ACROSS LINES (a page's body) is a preview of the page: the
 *   assistant's own Markdown (`ASSISTANT_WORDS`: no written address opened
 *   and no picture), so its headings and bullets read as they will.
 * - A COLOUR is its chip beside its hex. The chip is decoration; the hex is
 *   the words a screen reader hears.
 * - Anything else is its text, on the line.
 *
 * A list, a document or a text is a BLOCK value: it takes the card's width
 * under its label (`ChangeList`'s `block`).
 */

import type { ReactNode } from "react";
import { ChangeListItems, CodeBlock, ColorChip, Inline, Prose } from "@crewlethq/ui";
import { ASSISTANT_WORDS, renderMarkdown } from "~/lib/markdown.ts";

/** A value as a row draws it, and whether it is a block. */
export interface DrawnValue {
  node: ReactNode;
  block: boolean;
}

const HEX = /^#[0-9a-f]{6}$/i;

/** Whether text is a JSON object or list, as a request's body is. */
function isDocument(text: string): boolean {
  const trimmed = text.trim();
  if (!(trimmed.startsWith("{") || trimmed.startsWith("["))) return false;
  try {
    const parsed: unknown = JSON.parse(trimmed);
    return parsed !== null && typeof parsed === "object";
  } catch {
    return false;
  }
}

/** One value, by its text and, where it is a list, its items. */
export function drawValue(text: string, items?: readonly string[] | null): DrawnValue {
  if (items && items.length > 0) {
    return { node: <ChangeListItems items={items} />, block: true };
  }
  if (isDocument(text) && text.includes("\n")) {
    return {
      node: <CodeBlock code={text.trim()} language="json" wrap copyable={false} />,
      block: true,
    };
  }
  if (text.includes("\n")) {
    return {
      node: (
        <Prose as="div" measure="narrow" className="prose md">
          {renderMarkdown(text, ASSISTANT_WORDS)}
        </Prose>
      ),
      block: true,
    };
  }
  if (HEX.test(text.trim())) {
    return {
      node: (
        <Inline as="span" gap={1} align="center">
          <ColorChip value={text.trim().toLowerCase()} size="sm" />
          {text.trim()}
        </Inline>
      ),
      block: false,
    };
  }
  return { node: text, block: false };
}
