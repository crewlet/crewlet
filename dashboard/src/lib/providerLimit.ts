/**
 * A model at its usage limit, as ONE condition per entry.
 *
 * The engine holds a seat's limit steady for as long as it lasts
 * (`provider_limit`, internal/api/livestate/provider.go): which entry, whether
 * the seat waits for it (`parked`) or runs on a fallback (`running_on`), and
 * when the vendor said it lifts (`resets_at`). What a person needs from that is
 * one fact per entry, not one per seat: "Claude Code subscription limit
 * reached. Agents resume at 4:30 PM." Five seats on one spent subscription are
 * one thing to know, and drawn as five rows, each flipping between "stopped"
 * and "last turn failed" as the engine retried them, they were the Inbox's
 * flicker.
 *
 * Grouped by the limited entry's key, in the order the seats come, so the
 * condition's id and its place in the list are the entry's for as long as the
 * limit lasts.
 */

import type { AgentRow } from "~/protocol/index.ts";
import { CLI_PRODUCTS } from "~/contract/models.ts";
import { fmtClock, plural, tsKey } from "./format.ts";

/** One limited entry and the seats it holds up or hands to a fallback. */
export interface EntryLimit {
  key: string;
  /** The CLI the entry drives, and whether it signs in on a plan. */
  cli: string;
  subscription: boolean;
  /** The seats waiting for it. */
  parked: AgentRow[];
  /** The seats running on a fallback meanwhile, with the key carrying each. */
  carried: { row: AgentRow; on: string }[];
  /** When the vendor said it lifts, where every seat's report named the same
   *  instant; null where any did not say. */
  resetsAt: string | null;
  /** When the condition began: the earliest any seat reported it. */
  since: string;
  /** The vendor's own sentence, for the technical disclosure. */
  said?: string;
}

/** Every entry at its limit, by key, in the order its first seat comes. */
export function entryLimits(agents: readonly AgentRow[]): EntryLimit[] {
  const byKey = new Map<string, EntryLimit>();
  const unnamed = new Set<string>();
  for (const row of agents) {
    const limit = row.provider_limit;
    if (!limit) continue;
    let entry = byKey.get(limit.provider_key);
    if (!entry) {
      entry = {
        key: limit.provider_key,
        cli: limit.cli,
        subscription: limit.subscription,
        parked: [],
        carried: [],
        resetsAt: null,
        since: limit.since,
      };
      byKey.set(limit.provider_key, entry);
    }
    if (limit.parked) entry.parked.push(row);
    else if (limit.running_on) entry.carried.push({ row, on: limit.running_on });
    if (tsKey(limit.since) < tsKey(entry.since)) entry.since = limit.since;
    if (!entry.said && limit.said) entry.said = limit.said;
    // ONE INSTANT OR NONE: seats that disagree about when the limit lifts
    // (one probe read it, another did not) are told no instant rather than
    // whichever seat came first.
    if (!limit.resets_at) unnamed.add(limit.provider_key);
    else if (entry.resetsAt === null) entry.resetsAt = limit.resets_at;
    else if (tsKey(entry.resetsAt) !== tsKey(limit.resets_at)) unnamed.add(limit.provider_key);
  }
  for (const key of unnamed) {
    const entry = byKey.get(key);
    if (entry) entry.resetsAt = null;
  }
  return [...byKey.values()];
}

/** What an entry is called where a person reads it: its CLI's product name,
 *  or its key where the CLI is one of the company's own or was not said. */
export function entryName(limit: Pick<EntryLimit, "key" | "cli">): string {
  return CLI_PRODUCTS[limit.cli] ?? limit.key;
}

/** "Claude Code subscription limit reached": the entry's name, and the
 *  limit's kind by how its CLI signs in. */
export function limitTitle(limit: EntryLimit): string {
  return `${entryName(limit)} ${limit.subscription ? "subscription" : "usage"} limit reached`;
}

/**
 * What the limit costs, in a sentence or two: who waits and until when, and
 * who runs on what meanwhile.
 *
 * "Agents resume at 4:30 PM." "CTO resumes at Wed 2:00 AM." "Agents are
 * running on their fallback model until it lifts at 4:30 PM." Where the
 * vendor named no time: "Agents resume when it lifts. Resume time unknown."
 *
 * THE FALLBACK IS NOT NAMED: which entry carries a seat is the company's
 * configuration, which only an admin reads; whether one does is what the
 * person deciding whether to wait needs.
 */
export function limitDetail(limit: EntryLimit, now: number): string {
  const at = limit.resetsAt ? fmtClock(limit.resetsAt, now) : "";
  const sentences: string[] = [];
  if (limit.parked.length > 0) {
    const who = whoOf(limit.parked, limit.parked.length + limit.carried.length);
    const verb = limit.parked.length === 1 ? "resumes" : "resume";
    sentences.push(at ? `${who} ${verb} at ${at}.` : `${who} ${verb} when it lifts.`);
  }
  if (limit.carried.length > 0) {
    const one = limit.carried.length === 1;
    const who = whoOf(
      limit.carried.map((c) => c.row),
      limit.parked.length + limit.carried.length,
    );
    const until = at ? `until it lifts at ${at}` : "until it lifts";
    sentences.push(
      `${who} ${one ? "is" : "are"} running on ${one ? "its" : "their"} fallback model ${until}.`,
    );
  }
  if (!at) sentences.push("Resume time unknown.");
  return sentences.join(" ");
}

/** The seats named as one subject: "Agents" where they are all of them, the
 *  seat's role where it is one, a count otherwise. */
function whoOf(rows: readonly AgentRow[], all: number): string {
  const only = rows.length === 1 && all === 1 ? rows[0] : undefined;
  if (only) return only.role;
  if (rows.length === all) return "Agents";
  return plural(rows.length, "agent");
}
