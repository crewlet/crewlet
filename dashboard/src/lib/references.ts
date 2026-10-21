/**
 * Whether a value is, or carries, a `${NAME}` reference — by the engine's own
 * grammar (`contract/references.ts`, held to `internal/envref`), and the ONE
 * place a screen asks.
 *
 * PRESENTATION ONLY: the engine decides what a reference is, resolves it and
 * refuses what is not one. A screen asks to decide what to draw, and asking a
 * private copy of the grammar is how five of them came to spell it five times
 * with nothing holding any of them to the engine's.
 */

import { REFERENCE_NAME } from "~/contract/references.ts";

const WHOLE = new RegExp(`^\\$\\{(${REFERENCE_NAME})\\}$`);

/**
 * The variable a value names when it is EXACTLY one reference, or undefined.
 * Trimmed first, as `envref.Whole` trims, so what this calls a whole reference
 * is what the engine resolves as one.
 */
export function wholeReferenceName(value: unknown): string | undefined {
  if (typeof value !== "string") return undefined;
  return WHOLE.exec(value.trim())?.[1];
}

/** Whether a value is exactly one reference ([wholeReferenceName]). */
export function isWholeReference(value: unknown): boolean {
  return wholeReferenceName(value) !== undefined;
}
