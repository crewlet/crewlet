/**
 * Where the question a notice is about stands, in one sentence: open, settled
 * by the facts (ADR-0023's overtaken ask), answered or resolved.
 */

import type { WorkAskView } from "~/protocol/types.ts";

/** The sentence for `ask`; `when` says an instant relative to now. */
export function askStateWords(ask: WorkAskView, when: (at: string) => string): string {
  if (ask.open && ask.overtaken === "superseded") {
    return "The question it is about no longer needs an answer: it was asked again since.";
  }
  if (ask.open && ask.overtaken === "moot") {
    return "The question it is about no longer needs an answer: its item is finished.";
  }
  if (ask.open) return "The question it is about is still open.";
  const how = ask.resolved ? "resolved" : "answered";
  return `The question it is about was ${how}${ask.answered_at ? ` at ${when(ask.answered_at)}` : ""}.`;
}
