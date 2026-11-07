/**
 * What the org's pages say about a unit beside the seats in it: its path as a
 * reader names it, and the projects its work is filed under.
 */

import type { WorkProjectRow } from "~/protocol/types.ts";
import type { Unit } from "./seats.ts";

/** A unit's path as a reader names it: "Engineering · Core". */
export function unitPath(unit: Unit | null | undefined): string {
  return unit ? unit.chain.map((u) => u.name).join(" · ") : "";
}

/**
 * Every project filed to each unit, by unit KEY, what a project's `unit:`
 * names and what [Unit.id] is (`lib/seats.ts`). Never the unit's NAME, which is
 * prose two units may share: keyed on it, two teams called Platform drew each
 * other's projects.
 */
export function projectsByUnit(
  projects: readonly Pick<WorkProjectRow, "key" | "unit" | "archived">[],
): Map<string, string[]> {
  const out = new Map<string, string[]>();
  for (const p of projects) {
    // AN ARCHIVED PROJECT IS NOT WHERE A UNIT'S WORK GOES: its key would send
    // a reader to a board nobody files to.
    if (p.archived || !p.unit?.key) continue;
    out.set(p.unit.key, [...(out.get(p.unit.key) ?? []), p.key]);
  }
  for (const keys of out.values()) keys.sort();
  return out;
}
