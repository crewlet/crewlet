/**
 * Gitlet › Changes' query: the words each filter is said in.
 */

import { READINESS_BUCKETS } from "~/contract/code.ts";

/** Where an open change's merge stands, as a listing narrows to it (`contract/code.ts`). */
export type ReadinessBucket = (typeof READINESS_BUCKETS)[number];

/** Each bucket in the Readiness picker's words, keyed on the contract's set. */
export const READINESS_WORDS: Readonly<Record<ReadinessBucket, string>> = {
  ready: "Ready to merge",
  approval: "Waits on review",
  checks: "Waits on checks",
  blockers: "Blocked by findings",
};

/** Whether a string is a bucket the engine narrows a listing to. */
export function isReadinessBucket(value: string): value is ReadinessBucket {
  return (READINESS_BUCKETS as readonly string[]).includes(value);
}
