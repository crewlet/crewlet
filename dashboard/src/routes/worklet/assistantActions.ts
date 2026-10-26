/**
 * What the work list takes from the assistant: its filters
 * (`work.filter.apply`), their Clear (`work.filters.clear`) and its
 * arrangement (`work.arrange.set`), written to the address exactly as its own
 * controls write them.
 *
 * THE KEYS ARE `URL_HOMES`', never a second list: every key the grammar
 * calls a chip (a narrowing) or an arrangement (how the same answer is
 * drawn) may be written, and nothing else. The scope switch, a saved view's
 * tab and the calendar's month are the screen's own controls and stay out,
 * for the reasons `URL_HOMES` gives each of them.
 *
 * THE HOST'S LOCK IS NOT A FILTER. On `#/me` the list is one person's work,
 * and the locked key is what the page IS: a patch that names it is refused
 * with the reason, rather than applied and silently overridden on the way to
 * the wire (`buildItemsParams`), so the assistant can never widen `#/me` past
 * the person it is about.
 */

import type { UrlActionKey, UrlPatch } from "~/app/assistant/urlAction.ts";
import { GROUP_AXES } from "~/contract/work.ts";
import {
  NO_FILTERS,
  SORTS,
  URL_HOMES,
  axisName,
  filterChips,
  isShape,
  type LabelContext,
  type TrackerFilters,
} from "~/lib/work.ts";

/** A narrowing's value, as its chip words it: the chips' own words, never new copy. */
function chipWords(name: string, value: string, labels: LabelContext): string {
  const f: TrackerFilters = { ...NO_FILTERS, fields: {} };
  if (name.startsWith("f.")) f.fields = { [name]: value };
  else if (name === "blocked" || name === "removed") f[name] = value === "true";
  else if (name === "group") f.group = value;
  else (f as unknown as Record<string, unknown>)[name] = value;
  const chip = filterChips(f, labels).find((c) => c.param === name);
  return chip?.value ?? (chip ? "On" : value);
}

/** A narrowing's field name, as its chip names it. */
function chipLabel(name: string, labels: LabelContext): string {
  if (name.startsWith("f.")) {
    const slug = name.slice("f.".length);
    return labels.fields?.find((d) => d.slug === slug || d.id === slug)?.name || slug;
  }
  const f: TrackerFilters = { ...NO_FILTERS, fields: {} };
  if (name === "blocked" || name === "removed") f[name] = true;
  else if (name === "group") f.group = "";
  else (f as unknown as Record<string, unknown>)[name] = "x";
  return filterChips(f, labels).find((c) => c.param === name)?.label ?? name;
}

/** A switch's one value: on. Taking it off is a null. */
const onlyTrue = (label: string) => (value: string) =>
  value === "true" ? null : `${label} is a switch: it is on, or it is cleared.`;

/** An axis the list groups by, as Group by offers it. */
function axisCheck(value: string): string | null {
  return value === "none" || GROUP_AXES.some((a) => a.value === value)
    ? null
    : `This list cannot be grouped by "${value}".`;
}

/**
 * The keys a page action over the work list may write, by `URL_HOMES`' home:
 * the chips (`work.filter.apply`) or the arrangement (`work.arrange.set`),
 * each with the label and the words its own control uses.
 */
export function workKeys(
  home: "chip" | "arrangement",
  labels: LabelContext,
): Record<string, UrlActionKey> {
  const keys: Record<string, UrlActionKey> = {};
  for (const [name, at] of Object.entries(URL_HOMES)) {
    if (at !== home) continue;
    if (home === "chip") {
      keys[name] = {
        label: name === "f." ? "Field" : chipLabel(name, labels),
        history: "filter",
        words: (value) => chipWords(name, value, labels),
        ...(name === "blocked" || name === "removed"
          ? { check: onlyTrue(chipLabel(name, labels)) }
          : {}),
      };
      continue;
    }
    switch (name) {
      case "shape":
        // A SHAPE IS A PLACE THE READER WENT, so it pushes, as the shape row does.
        keys[name] = {
          label: "Shape",
          history: "section",
          check: (v) => (isShape(v) ? null : `There is no "${v}" shape.`),
        };
        break;
      case "group_by":
      case "group_by2":
        keys[name] = {
          label: name === "group_by" ? "Group by" : "Then group by",
          history: "filter",
          check: axisCheck,
          words: (v) => (v === "none" ? "No grouping" : axisName(v) || v),
        };
        break;
      case "sort":
        keys[name] = {
          label: "Sort",
          history: "filter",
          check: (v) =>
            SORTS.some((s) => s.value === v) ? null : `This list cannot be sorted by "${v}".`,
          words: (v) => SORTS.find((s) => s.value === v)?.label ?? v,
        };
        break;
      default:
        keys[name] = { label: name.endsWith(".") ? "Columns" : "Display", history: "filter" };
    }
  }
  return keys;
}

/**
 * The keys Clear writes (`clearFiltersPatch`): every chip and the segment,
 * which is the one key it touches that is not a chip.
 */
export function workClearKeys(labels: LabelContext): Record<string, UrlActionKey> {
  return {
    ...workKeys("chip", labels),
    scope: {
      label: "Segment",
      history: "filter",
      words: (v) => v.charAt(0).toUpperCase() + v.slice(1),
    },
  };
}

/** A patch without the keys a host locks, which are what the page is and never the assistant's to clear. */
export function withoutLocked(
  patch: Readonly<Record<string, string | null>>,
  lock: Readonly<Record<string, string>> | undefined,
): Record<string, string | null> {
  const out: Record<string, string | null> = {};
  for (const [k, v] of Object.entries(patch)) if (!lock || !Object.hasOwn(lock, k)) out[k] = v;
  return out;
}

/**
 * The host's lock as the page action's gate: a patch naming a locked key is
 * refused, since that key is what the page is.
 */
export function lockGate(lock: Readonly<Record<string, string>> | undefined) {
  return (patch: UrlPatch): string | null =>
    lock && Object.keys(patch).some((k) => Object.hasOwn(lock, k))
      ? "This list shows one person's work, so that filter stays as it is. Use All work to filter by someone else."
      : null;
}
