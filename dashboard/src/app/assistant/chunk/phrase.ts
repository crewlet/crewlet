/**
 * A phrase the engine sends (`AssistantPhrase`) as words, for every reader
 * that does not draw its records: a copy, a diff, a comparison.
 */

import type { AssistantPart } from "~/contract/assistant.ts";

/** Words that name records, run by run (`wire.Phrase`). */
export type AssistantPhrase = AssistantPart[];

/** A phrase as words, each record by its name: what a copy and a plain reader read. */
export function phraseText(phrase: AssistantPhrase | undefined): string {
  return (phrase ?? []).map((part) => part.ref?.name ?? part.text ?? "").join("");
}

/** Whether a phrase names a record. */
export function namesRecord(phrase: AssistantPhrase | undefined): boolean {
  return (phrase ?? []).some((part) => part.ref !== undefined);
}

/** Words as a phrase that names nothing. */
export function plainPhrase(text: string): AssistantPhrase {
  return text === "" ? [] : [{ text }];
}
