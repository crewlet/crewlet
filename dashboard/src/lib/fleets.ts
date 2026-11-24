/**
 * An LLM fleet as every model picker names it: the `fleet:<key>` reference a
 * seat job or helper writes, its label, how many members have room, and the
 * engine's rule that a fleet stands alone in a chain.
 */

import type { Fleet, FleetMember } from "~/protocol/fleets.ts";

const FLEET_REF = "fleet:";

export const fleetRef = (key: string) => `${FLEET_REF}${key}`;

/** The fleet a written value names, or null for a model's key. */
export const fleetOfRef = (value: string): string | null =>
  value.startsWith(FLEET_REF) ? value.slice(FLEET_REF.length) : null;

/** A fleet in a picker or a sentence: "Fleet · Claude Opus". */
export const fleetLabel = (f: Pick<Fleet, "name">) => `Fleet · ${f.name}`;

/** Whether a member takes work now or finishes what it runs: the fleet's "ready". */
export const isReady = (m: Pick<FleetMember, "state">) =>
  m.state === "available" || m.state === "near";

/** A fleet option's second line: "Default · 2 of 3 members have room", or "No members yet". */
export function fleetRoom(f: Fleet): string {
  const room =
    f.members.length === 0
      ? "No members yet"
      : `${f.members.filter(isReady).length} of ${f.members.length} members have room`;
  return `${f.is_default ? "Default · " : ""}${room}`;
}

/**
 * A chain after a pick: a fleet stands alone, so picking one makes it the
 * whole chain, and a model picked while a fleet is held replaces the fleet.
 * A removal or a move is kept as it is.
 */
export function standAlone(held: readonly string[], next: readonly string[]): string[] {
  const added = next.filter((v) => !held.includes(v));
  const fleet = added.filter((v) => fleetOfRef(v) !== null).at(-1);
  if (fleet !== undefined) return [fleet];
  if (added.length > 0) return next.filter((v) => fleetOfRef(v) === null);
  return [...next];
}

/**
 * Why the engine refuses a chain, in its words: a fleet inside a list, or a
 * reference to a fleet that does not exist (checked once `fleets` is read).
 */
export function fleetChainProblem(
  chain: readonly string[],
  fleets: readonly Pick<Fleet, "key">[] | null,
): string | undefined {
  const named = chain.map(fleetOfRef).filter((k): k is string => k !== null);
  if (named.length > 0 && chain.length > 1)
    return "A fleet stands alone: choose the fleet or a list of models";
  const missing = fleets ? named.find((k) => !fleets.some((f) => f.key === k)) : undefined;
  return missing !== undefined ? `${missing} is not a fleet` : undefined;
}
