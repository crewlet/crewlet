/**
 * A domain's trim in words: what each of its six terms is called on a screen,
 * and which of them decides where the trim stops.
 *
 * # Words beside the name, never instead of it
 *
 * A term's own name (`backup_floor`) is what the engine logs, the CLI prints and
 * the retention guide is indexed by, so a screen shows it beside these words
 * rather than replacing it: a reader who meets the name anywhere else finds it
 * here too. `RetentionTermName` is held to the engine's list in both
 * directions, so a term the engine adds cannot reach a screen with no words.
 */

import type { RetentionTermName } from "~/contract/retention.ts";
import type { RetentionDomain, RetentionTerm } from "~/protocol/index.ts";

/** A term's name in words, and what holds a trim when the term binds. */
export interface TermWords {
  name: string;
  holds: string;
}

/** Every term in words. */
export const TERM_WORDS: Record<RetentionTermName, TermWords> = {
  applied: { name: "Slowest node", holds: "the slowest node" },
  min_hold: { name: "Holds", holds: "a live hold" },
  backup_floor: { name: "Backup", holds: "the backup" },
  snapshot_floor: { name: "Snapshots", holds: "the snapshots" },
  feed_ack_floor: { name: "Wake feed", holds: "the wake feed" },
  age_floor: { name: "Minimum age", holds: "the minimum age" },
};

/**
 * The term that decides where a domain's trim stops: the one holding it while
 * it is blocked, and otherwise the lowest term that binds, which on a healthy
 * fleet is the minimum age. Undefined where the trim has concluded nothing.
 */
export function bindingTerm(d: RetentionDomain): RetentionTermName | undefined {
  if (d.trim_floor_state !== "published") return undefined;
  if (d.blocked_by) return d.blocked_by;
  let lowest: RetentionTerm | undefined;
  for (const t of d.terms) {
    if (t.state !== "ok" || t.seq === undefined) continue;
    if (lowest?.seq === undefined || t.seq < lowest.seq) lowest = t;
  }
  return lowest?.name;
}
