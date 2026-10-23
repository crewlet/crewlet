/**
 * A sentence the engine wrote about the company document, as the builder
 * draws it.
 *
 * The engine's problems and warnings are written for `crewlet validate` too,
 * where a line reads "roles: this company has no `kind: human` seats": lower
 * case after its field, with a document key in backticks. Drawn verbatim in
 * the builder it began lower case and printed the backticks beside the
 * sentence-case callouts around it. So the first letter of an opening word is
 * raised and a backticked span is drawn as the kit's inline code; the words
 * are the engine's own, unchanged.
 */

import { Fragment, type ReactNode } from "react";
import { InlineCode } from "@crewlethq/ui";

/** The engine's sentence in sentence case, its backticked keys as code. */
export function engineSentence(text: string): ReactNode {
  // RAISED ONLY WHERE IT OPENS ON A WORD: a message that opens on a field
  // path ("units[0].roles[0].goal: missing") names a key, and a key's case is
  // its own.
  const raised = /^[a-z][a-z']*\s/.test(text) ? text.charAt(0).toUpperCase() + text.slice(1) : text;
  // AN UNMATCHED BACKTICK IS A CHARACTER, not code that never ends: only a
  // closed pair is drawn as code.
  const parts = raised.split("`");
  if (parts.length % 2 === 0) return raised;
  return parts.map((part, i) =>
    i % 2 === 1 ? <InlineCode key={i}>{part}</InlineCode> : <Fragment key={i}>{part}</Fragment>,
  );
}
