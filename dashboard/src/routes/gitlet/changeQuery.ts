/**
 * Gitlet › Changes' query: a run `name:value` with a known name is a qualifier, written to its
 * address key; every other run is a word, sent verbatim as the engine's `text`, which judges it.
 */

import { CHANGE_NEEDS, CHANGE_STATES, CHANGE_WINDOWS, READINESS_BUCKETS } from "~/contract/code.ts";
import { EVERY_STATE, isChangeNeed, isChangeWindow, isRepoName } from "~/lib/code.ts";

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

/** The filters the address names, as Gitlet › Changes reads them; `text` is the words (`q=`). */
export interface Filters {
  state: string;
  needs: string;
  repo: string;
  owner: string;
  reviewer: string;
  work: string;
  window: string;
  readiness: string;
  text: string;
}

/** Each qualifier a search of changes reads, and the address key it writes, in the chips' order. */
export const QUALIFIERS = {
  is: "state",
  author: "owner",
  reviewer: "reviewer",
  repo: "repo",
  "waiting-on": "needs",
  readiness: "readiness",
  updated: "window",
  work: "work",
} as const;

export type Qualifier = keyof typeof QUALIFIERS;
export type FilterKey = (typeof QUALIFIERS)[Qualifier];

const NAMES = Object.keys(QUALIFIERS) as Qualifier[];
const isQualifier = (name: string): name is Qualifier => (NAMES as string[]).includes(name);

/** What the box held: the filters it names, its words, and what it could not read. */
export interface Parsed {
  filters: Partial<Record<FilterKey, string>>;
  words: string;
  /** A known qualifier's value it does not take, or one named twice: nothing is submitted. */
  problem?: string;
  /** The names of `name:value` runs searched as words. */
  unknown: string[];
}

/** "a, b or c" (or "and"). */
const listed = (values: readonly string[], last = "or") =>
  values.length < 2
    ? values.join("")
    : `${values.slice(0, -1).join(", ")} ${last} ${values.at(-1)}`;

/** A run that reads as `name:value` with a name of letters: a qualifier, known or not. */
const QUALIFIER_RUN = /^([\p{L}][\p{L}\p{N}_-]*):(.+)$/u;

/** Why a known qualifier does not take this value, or undefined where it does. */
function valueProblem(name: Qualifier, v: string): string | undefined {
  switch (name) {
    case "is":
      return (CHANGE_STATES as readonly string[]).includes(v)
        ? undefined
        : `“${v}” is not a state: is: takes ${listed(CHANGE_STATES)}.`;
    case "waiting-on":
      return isChangeNeed(v)
        ? undefined
        : `“${v}” is not what a change waits on: waiting-on: takes ${listed(CHANGE_NEEDS)}.`;
    case "readiness":
      return isReadinessBucket(v)
        ? undefined
        : `“${v}” is not where a merge stands: readiness: takes ${listed(READINESS_BUCKETS)}.`;
    case "updated":
      return isChangeWindow(v)
        ? undefined
        : `“${v}” is not a window: updated: takes ${listed(CHANGE_WINDOWS)}.`;
    case "repo":
      return isRepoName(v) ? undefined : `“${v}” is not a name a repository can have.`;
    case "work":
      return /^[^:]+:.+$/.test(v)
        ? undefined
        : `“${v}” is not a work item: work: takes <backend>:<id>, as a task's link writes it.`;
    case "author":
    case "reviewer":
      return undefined;
  }
}

/** Parse what the box holds; `me` is the reader's handle, "" where they are bound to no seat. */
export function parseChangeQuery(text: string, me: string): Parsed {
  const filters: Partial<Record<FilterKey, string>> = {};
  const words: string[] = [];
  const unknown: string[] = [];
  let problem: string | undefined;
  for (const run of text.split(/\s+/).filter(Boolean)) {
    const match = QUALIFIER_RUN.exec(run);
    if (!match) {
      words.push(run);
      continue;
    }
    const [, name, raw] = match as unknown as [string, string, string];
    if (!isQualifier(name)) {
      words.push(run);
      if (!unknown.includes(name)) unknown.push(name);
      continue;
    }
    const key = QUALIFIERS[name];
    let value = raw;
    if ((name === "author" || name === "reviewer") && value === "@me") {
      if (me === "") problem ??= "@me names nobody: you are bound to no seat.";
      value = me;
    }
    if (key in filters) problem ??= `${name}: is named twice: a search takes one of each.`;
    problem ??= valueProblem(name, value);
    filters[key] = value;
  }
  return { filters, words: words.join(" "), unknown, ...(problem ? { problem } : {}) };
}

/** A filter drawn as its qualifier: `is:open`, `repo:web`. */
export interface FilterChip {
  id: FilterKey;
  label: string;
}

/** The chips for the address's filters, in QUALIFIERS' order; no `is:` chip where every state is listed. */
export function filterChips(f: Filters): FilterChip[] {
  const chips: FilterChip[] = [];
  for (const name of NAMES) {
    const key = QUALIFIERS[name];
    const value = f[key];
    if (value === "" || (key === "state" && value === EVERY_STATE)) continue;
    chips.push({ id: key, label: `${name}:${value}` });
  }
  return chips;
}

/** The state a listing names where the address names none. */
export const DEFAULT_STATE = "open";

/**
 * The address patch a submit writes: the box's qualifiers over the address's filters, its words as
 * `q`; a bucket named without a state lists the open changes, as the Readiness picker does.
 */
export function submitPatch(current: Filters, parsed: Parsed): Record<string, string | null> {
  const patch: Record<string, string | null> = {};
  for (const key of Object.values(QUALIFIERS)) {
    patch[key] = parsed.filters[key] ?? (current[key] === "" ? null : current[key]);
  }
  if (parsed.filters.readiness !== undefined && parsed.filters.state === undefined) {
    patch.state = null;
  }
  if (patch.state === DEFAULT_STATE) patch.state = null;
  patch.q = parsed.words === "" ? null : parsed.words;
  return patch;
}

/** Whether any filter or word differs from the default listing (the open changes, unnarrowed). */
export function narrowed(f: Filters): boolean {
  return (
    f.state !== DEFAULT_STATE ||
    f.text !== "" ||
    Object.values(QUALIFIERS).some((key) => key !== "state" && f[key] !== "")
  );
}

/** Every key the bar writes, cleared: the default listing. */
export function clearedPatch(): Record<string, null> {
  return Object.fromEntries([...Object.values(QUALIFIERS), "q"].map((key) => [key, null]));
}

/** The note for `name:value` runs searched as words, or undefined where there are none. */
export function unknownNote(names: readonly string[]): string | undefined {
  if (names.length === 0) return undefined;
  const said = listed(
    names.map((n) => `“${n}:”`),
    "and",
  );
  return names.length === 1
    ? `${said} is not a qualifier, so it was searched as words.`
    : `${said} are not qualifiers, so they were searched as words.`;
}
