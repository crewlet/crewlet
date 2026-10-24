/**
 * Which project a new task is filed in when the person has not chosen one,
 * and the projects a Project field offers. The New task sheet opens on it, and
 * a create page's "who builds it" block follows it as the agent changes.
 */

import type { NewTaskPreset } from "~/app/newTask.ts";
import { projectsByUnit } from "~/lib/orgchart.ts";
import type { OrgIndex } from "~/lib/seats.ts";
import type { SelectOption } from "@crewlethq/ui";
import type { WorkProjectRow } from "~/protocol/index.ts";

/**
 * Which project the sheet opens on: the door's, else the ASSIGNEE's (the
 * project filed under the preset seat's own unit, or the nearest unit above it
 * that has one), else the person's own (where their create lands when it names
 * none — the engine's answer, `viewer`), else the first active project. ""
 * while none is known, and the create refuses naming the field rather than
 * guessing.
 *
 * THE ASSIGNEE'S UNIT OUTRANKS THE PERSON'S OWN because a door that names a
 * seat and no project — a seat's Message, a board lane grouped by assignee
 * across every project — is filing work FOR that seat: the Agent CEO's ask
 * belongs on the board its unit works from, not on whichever project the asker
 * happens to sit in or the first one in the list.
 */
export function startingProject(
  preset: NewTaskPreset,
  own: string,
  projects: readonly WorkProjectRow[],
  index?: OrgIndex,
): string {
  const known = (key: string | undefined) => !!key && projects.some((p) => p.key === key);
  // BEFORE THE LIST HAS ANSWERED, a named project is trusted as given: a
  // project page's door knows its key, and blanking it until a second read
  // lands would draw the sheet empty and then fill it.
  if (projects.length === 0) return preset.project || own;
  if (known(preset.project)) return preset.project!;
  const chain = preset.assignee ? (index?.byHandle.get(preset.assignee)?.unit?.chain ?? []) : [];
  if (chain.length) {
    const byUnit = projectsByUnit(projects);
    for (const unit of [...chain].reverse()) {
      const key = byUnit.get(unit.id)?.[0];
      if (key) return key;
    }
  }
  if (known(own)) return own;
  return projects[0]!.key;
}

/** The projects as the Project field offers them: key and name, archived ones out. */
export function projectOptions(rows: readonly WorkProjectRow[], chosen: string): SelectOption[] {
  const options: SelectOption[] = rows.map((p) => ({
    value: p.key,
    // A NAME THAT IS ITS KEY is said once: "ENG · ENG" said nothing twice.
    label: p.name && p.name !== p.key ? `${p.key} · ${p.name}` : p.key,
    text: `${p.key} ${p.name}`,
  }));
  // A PROJECT THE DOOR NAMED BEFORE THE LIST ANSWERED is still a value the
  // field holds, and a select whose value is not among its options draws a
  // blank — so it is offered as itself until the list says otherwise.
  if (chosen && !rows.some((p) => p.key === chosen))
    options.unshift({ value: chosen, label: chosen });
  return options;
}
