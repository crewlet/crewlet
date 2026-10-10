/**
 * Who files into a knowledge space — the units whose `space:` names it, and
 * the tracker project each of them works in.
 *
 * # The engine's answer, on the container itself
 *
 * A unit's `space:` is kept out of the anonymous org projection, and this read
 * it from the company document — an admin's (ADR-0031) — so a member's
 * knowledge page said "who files here is for admins to read" about which team
 * a space belongs to. The `containers` answer now carries each container's
 * `owners`, resolved against the running chart the way `work_projects`
 * resolves a project's unit, so every reader of the container reads who files
 * into it, and there is nothing left to be in flight, refused or forbidden: a
 * container row on screen is one the engine answered, owners included.
 *
 * ONE MODULE for the tree's space rows and the container rail, because both
 * state the same fact about the same space and two derivations of it had
 * already disagreed.
 */

import type { PageContainer } from "~/protocol/index.ts";

/** One unit that files into a space, as a person reads it. */
export interface SpaceOwner {
  name: string;
  project?: string;
}

/** Who files into this container, by the engine's answer. */
export function spaceOwners(container: PageContainer): SpaceOwner[] {
  return container.owners.map((o) => ({
    name: o.unit.name || o.unit.key || "",
    project: o.project,
  }));
}

/** The one sentence that states who files here, for a tooltip or a screen reader. */
export function ownerSentence(owners: readonly SpaceOwner[]): string {
  if (owners.length === 0) return "No unit names this space in its space:";
  return owners
    .map((u) => `Filed by ${u.name}${u.project ? `, whose work is in ${u.project}` : ""}`)
    .join("; ");
}

/**
 * The tracker projects of the units filing here that the space's own key does
 * not already name — the ones worth DRAWING beside it. The Nimbus shape keys a
 * space the same as its unit's project, and a second `ENG` beside `ENG` says
 * nothing; a space filed by a unit working in another project is exactly the
 * case a reader cannot guess.
 */
export function otherProjects(owners: readonly SpaceOwner[], key: string): string[] {
  const out: string[] = [];
  for (const u of owners) {
    const p = u.project?.trim();
    if (p && p.toUpperCase() !== key.toUpperCase() && !out.includes(p)) out.push(p);
  }
  return out;
}
