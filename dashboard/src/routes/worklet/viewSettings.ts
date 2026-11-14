/**
 * A saved view's parameters in the words the screen uses for them: what its
 * page says it shows.
 *
 * ITS OWN MODULE, loaded with the views' route rather than with every screen:
 * `lib/work.ts` is in the entry every reader downloads before the first
 * screen draws, and only a view's page reads a view's settings.
 */

import {
  NO_FILTERS,
  SCOPE_GROUPS,
  SORTS,
  axisName,
  filterChips,
  type LabelContext,
  type TrackerFilters,
} from "~/lib/work.ts";

/**
 * A saved view's parameters, each in the words the screen uses for it.
 *
 * A VIEW'S PAGE PRINTED THE ENGINE'S GRAMMAR: `show_closed true`,
 * `sort -updated`, `subtasks separate`, as code, which is the wire rather than
 * anything a reader set. Every narrowing is worded as its chip words it
 * ([filterChips]), the order as the Sort picker names it, a grouping as Group
 * by does, and which work is shown as Show does; a key no control names is
 * still said, as itself, rather than hidden, since it is part of what the view
 * asks.
 */
export function viewSettingWords(
  params: Readonly<Record<string, string>>,
  ctx: LabelContext = {},
): string[] {
  const out: string[] = [];
  const said = new Set<string>();
  const say = (key: string, words: string) => {
    said.add(key);
    out.push(words);
  };
  // WHICH WORK IS SHOWN, as Show names it.
  const group = params.status_group ?? "";
  if (params.closed_since) {
    say("closed_since", "Open work, and what finished this week");
    said.add("status_group");
  } else if (group === SCOPE_GROUPS.open) {
    say("status_group", "Open work");
  } else if (group === SCOPE_GROUPS.closed) {
    say("status_group", "Finished and cancelled work");
  }
  const closed = params.show_closed ?? "";
  if (closed === "true")
    say("show_closed", group ? "Finished work included" : "Every task, finished or not");
  else if (closed.startsWith("recent:"))
    say("show_closed", `Work finished in the last ${closed.slice(7)} included`);
  else if (closed === "false") say("show_closed", "Open work only");
  // THE NARROWINGS, as their chips word them.
  const filters: TrackerFilters = { ...NO_FILTERS, fields: {}, scope: "all" };
  for (const [key, value] of Object.entries(params)) {
    if (key.startsWith("f.")) filters.fields[key] = value;
    else if (key === "blocked" || key === "removed") filters[key] = value === "true";
    else if (key in NO_FILTERS && typeof NO_FILTERS[key as keyof TrackerFilters] === "string") {
      (filters as unknown as Record<string, string>)[key] = value;
    }
  }
  if (params.group !== undefined) filters.group = params.group;
  filters.groupBy = params.group_by ?? "";
  for (const chip of filterChips(filters, ctx)) {
    say(chip.param, [chip.label, chip.verb, chip.value].filter(Boolean).join(" "));
  }
  // THE ARRANGEMENT, as Group by, Subgroup by and Sort name it.
  if (params.group_by)
    say("group_by", `Grouped by ${axisName(params.group_by) || params.group_by}`);
  if (params.group_by2)
    say("group_by2", `Subgrouped by ${axisName(params.group_by2) || params.group_by2}`);
  if (params.sort) {
    const named = SORTS.find((o) => o.value === params.sort)?.label;
    say("sort", `Sorted by ${named ?? params.sort}`);
  }
  if (params.subtasks) {
    say(
      "subtasks",
      params.subtasks === "separate"
        ? "Sub-tasks as their own rows"
        : "Sub-tasks under their parent",
    );
  }
  // WHAT NO CONTROL NAMES, said as itself.
  for (const [key, value] of Object.entries(params)) {
    if (said.has(key) || key === "group" || key === "group_limit") continue;
    out.push(`${key} ${value}`);
  }
  return out;
}
