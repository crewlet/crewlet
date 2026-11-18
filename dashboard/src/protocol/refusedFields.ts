/**
 * Which value a write was refused by, in this dashboard's words: an `invalid`
 * refusal names the field the engine refused (`fields`, ADR-0060), as the
 * request spells it, and never the engine's sentence, which is written for
 * whoever built the request.
 *
 * A field a person fills in is named by what the form calls it; any other
 * (an argument the page itself sends) is named as the value the page sent, so
 * the sentence never tells a person to check what they entered when they
 * entered nothing.
 */

import type { ActionTool } from "./act.ts";

/** What a form calls each field a person fills in, by tool and the field's name. */
const ENTERED: Readonly<Partial<Record<ActionTool, Readonly<Record<string, string>>>>> = {
  code_create_repo: { name: "its name", owner: "its owner", description: "its description" },
  code_open_change: {
    "open.title": "its title",
    "open.body": "its description",
    title: "its title",
  },
  code_update_change: { "edit.title": "its title", "edit.body": "its description" },
  code_close_change: { "close.reason": "the reason" },
  code_comment_on_change: { "comment.body": "the comment" },
  code_review_change: {
    "review.summary": "the review's summary",
    "review.findings": "its findings",
  },
};

/** The form's name for field on tool, an indexed field (`review.findings[2]`) by its list's. */
function labelOf(tool: ActionTool, field: string): string | undefined {
  const named = ENTERED[tool];
  if (!named) return undefined;
  return named[field] ?? named[field.replace(/\[\d+\].*$/, "")];
}

/** The sentence of an `invalid` refusal that names the field it refused. */
export function refusedFieldSentence(tool: ActionTool, fields: readonly string[]): string {
  const field = fields[0]!;
  const label = labelOf(tool, field);
  return label
    ? `That change was refused, and nothing was written: Gitlet did not accept ${label}. Change it, then try again.`
    : `That change was refused, and nothing was written: Gitlet did not accept the value this page sent as ${field}.`;
}
