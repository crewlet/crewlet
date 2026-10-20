/**
 * How the demo engine says no, in one place every module of it shares.
 *
 * Kept apart from `engine.ts` so the query table, the act handlers and the
 * REST router can refuse without importing the engine they are part of.
 */

import type { QueryErrorCode } from "~/contract/errors.ts";

/** The prefix every line the harness writes to the console carries. */
export const LOG = "[prototype]";

/** A refusal the engine sends as a query's error frame. */
export class DemoRefusal extends Error {
  readonly code: QueryErrorCode;
  readonly detail: string | null;
  constructor(code: QueryErrorCode, detail: string | null = null) {
    super(detail ?? code);
    this.name = "DemoRefusal";
    this.code = code;
    this.detail = detail;
  }
}

/** The sentence every unmodelled write is refused with. */
export function notModelled(what: string): string {
  return `${what} is not modelled in this prototype, so nothing was changed.`;
}

/** Go's `%q`, for the values an engine refusal quotes. */
export function goQuote(s: string): string {
  return JSON.stringify(s);
}
