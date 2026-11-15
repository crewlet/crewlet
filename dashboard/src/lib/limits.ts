/**
 * Changing one of the engine's caps, as values: what a person types for a
 * cap, the merge patch that writes it, and what the revision is recorded as.
 *
 * PURE, so every rule the Limits page's Save turns on is pinned without a
 * network: `lib/useSettingWrite.ts` is the half that sends it.
 *
 * # The engine decides what a cap may be, and says so before the save
 *
 * A typed value is read for its SHAPE (a whole number for a count or a rate,
 * a number of seconds, a percentage for a share) and then held to the RANGE
 * the row carries: the engine's own declaration of the bound its validator
 * holds the cap to (`config.Range`), never a copy kept here. So 0, -2 or
 * 101 is said beside the field ("At least 1.") before anything is sent,
 * rather than sent for the engine to refuse. What only the whole document
 * can say (a task's time limit above its call's, a first turn's rounds past
 * the most an extension may grant) is the dry run's, every save makes it
 * first, and its refusal is said in a person's words ([limitRefusal]).
 *
 * # The default is the key's absence
 *
 * A cap the document does not write is the engine's default, so going back to
 * the default REMOVES the key (a merge patch's `null`) rather than writing
 * the default's number: a document that writes today's default keeps it when
 * a later build changes the default, which is not what "use the default"
 * asks for.
 */

import type { LimitsAnswer, SettingRange } from "~/contract/limits.ts";
import { fmtSetting, plural } from "~/lib/format.ts";
import type { CompanyDocument, ConfigProblem } from "~/protocol/types.ts";
import type { SettingChange, SettingPlan } from "./useSettingWrite.ts";

export type LimitRow = LimitsAnswer["limits"][number];

/** A cap as typed: a value to write, the default (the key removed), or why it is neither. */
export type TypedLimit =
  | { readonly ok: true; readonly value: number | null }
  | { readonly ok: false; readonly error: string };

/** What the field of a cap of this unit takes, as the line under it. */
export function limitTyping(row: LimitRow): string {
  switch (row.unit) {
    case "seconds":
      return "A number of seconds.";
    case "fraction":
      return "A percentage, from 1 to 100.";
    case "per_second":
      return "A whole number per second.";
    case "count":
      return "A whole number.";
    case "days":
      return "A whole number of days.";
  }
}

/** A cap's value as its field shows it: the number a person would type. */
export function limitText(row: LimitRow, value: number): string {
  // A SHARE IS TYPED AS A PERCENTAGE, rounded to what a float keeps of it:
  // 0.35 times 100 is 35.00000000000001.
  if (row.unit === "fraction") return String(Math.round(value * 10_000) / 100);
  return String(value);
}

/**
 * What a typed value that is not one of the cap's unit is told: what it is
 * not, which the line under the field (what it takes, and the default) does
 * not already say.
 */
function notOfUnit(row: LimitRow, typed: string): string {
  switch (row.unit) {
    case "seconds":
      return `“${typed}” is not a number of seconds.`;
    case "fraction":
      return `“${typed}” is not a percentage.`;
    case "per_second":
    case "count":
      return `“${typed}” is not a whole number.`;
    case "days":
      return `“${typed}” is not a whole number of days.`;
  }
}

/** A bound in the cap's own unit, as a sentence says it. */
function boundWords(row: LimitRow, n: number): string {
  switch (row.unit) {
    case "fraction":
      return `${Math.round(n * 10_000) / 100}%`;
    case "seconds":
      // AS A SETTING'S DURATION READS everywhere else ("5 minutes"), never a
      // count of seconds a person converts in their head; a zero bound is
      // said as one, since a duration of nothing has no larger unit.
      return n === 0 ? "0 seconds" : fmtSetting(n);
    case "per_second":
      return `${n.toLocaleString()} per second`;
    case "count":
      return n.toLocaleString();
    case "days":
      return plural(n, "day");
  }
}

/** Whether `range` admits `value`, as the engine's `config.Range.Admits` does. */
export function rangeAdmits(range: SettingRange, value: number): boolean {
  if (value < range.min || (range.min_exclusive === true && value === range.min)) return false;
  return range.max === undefined || value <= range.max;
}

/** What a cap admits, in its own unit, as the sentence beside its field: "At least 1." */
export function rangeWords(row: LimitRow, range: SettingRange): string {
  const least = boundWords(row, range.min);
  if (range.max === undefined) {
    return range.min_exclusive ? `More than ${least}.` : `At least ${least}.`;
  }
  const most = boundWords(row, range.max);
  return range.min_exclusive
    ? `More than ${least}, and at most ${most}.`
    : `From ${least} to ${most}.`;
}

/**
 * Reads what was typed for a cap. Empty is the default, which is always
 * admitted; any other value is held to its unit and then to the range the
 * engine holds the cap to.
 */
export function readLimit(row: LimitRow, text: string): TypedLimit {
  const typed = text.trim().replace(/%$/, "").trim();
  if (typed === "") return { ok: true, value: null };
  // A SIGN IS READ, so -2 is told the cap's range rather than that it is not
  // a number.
  if (!/^-?\d+(\.\d+)?$/.test(typed)) return { ok: false, error: notOfUnit(row, typed) };
  const n = Number(typed);
  if (!Number.isFinite(n)) return { ok: false, error: notOfUnit(row, typed) };
  let value: number;
  switch (row.unit) {
    case "fraction":
      value = Math.round(n * 100) / 10_000;
      break;
    case "seconds":
      value = n;
      break;
    case "count":
    case "per_second":
    case "days":
      if (!Number.isInteger(n)) return { ok: false, error: notOfUnit(row, typed) };
      value = n;
      break;
  }
  if (row.range && !rangeAdmits(row.range, value)) {
    return { ok: false, error: rangeWords(row, row.range) };
  }
  return { ok: true, value };
}

/**
 * Whether what was typed is the value in force already: the default for an
 * empty field. Nothing to save, so Save is held.
 */
export function limitUnchanged(row: LimitRow, value: number | null): boolean {
  return (value ?? row.default) === row.value;
}

/**
 * The engine's refusal of one cap's change, in a person's words.
 *
 * The engine's `message` is its line for an operator, led by the document's
 * key (`turn_engine.max_iterations: value out of range: …`), and was drawn
 * here as it came. A problem at this cap carrying the range it broke is said
 * as the field says it before a save; one the rule wrote a person's words for
 * (`said`) is said in those; anything else is said as what it is to this
 * person, a value the rest of the company's configuration cannot take, and
 * the operator's line stays where an operator reads it.
 */
export function limitRefusal(row: LimitRow, problems: readonly ConfigProblem[]): string {
  const said = new Set<string>();
  let unsaid = false;
  for (const p of problems) {
    if (p.path === row.key && p.range) said.add(rangeWords(row, p.range));
    else if (p.said) said.add(p.said);
    else unsaid = true;
  }
  if (unsaid || said.size === 0) {
    said.add("This value does not fit the rest of the company’s configuration.");
  }
  return [...said].join(" ");
}

/** A cap's key as the document's path: its dotted segments. */
function segmentsOf(key: string): string[] {
  return key.split(".");
}

/** The value the document itself writes at `key`, `undefined` where it writes none. */
export function documentLimit(document: CompanyDocument, key: string): unknown {
  let at: unknown = document;
  for (const segment of segmentsOf(key)) {
    if (typeof at !== "object" || at === null || Array.isArray(at)) return undefined;
    at = (at as Record<string, unknown>)[segment];
  }
  return at ?? undefined;
}

/** The merge patch that writes `value` at `key`, or removes the key for `null`. */
export function limitPatch(key: string, value: number | null): Record<string, unknown> {
  const segments = segmentsOf(key);
  let patch: unknown = value;
  for (let i = segments.length - 1; i >= 0; i--) patch = { [segments[i]!]: patch };
  return patch as Record<string, unknown>;
}

/**
 * One cap's change against the document as it is now: nothing where the
 * document already holds what was typed, else the patch and the summary the
 * revision history shows for it, opened by the page it was made on (`area`).
 */
export function limitPlan(
  row: LimitRow,
  label: string,
  value: number | null,
  area: string,
): SettingPlan {
  return (document): SettingChange | null => {
    const held = documentLimit(document, row.key);
    if (value === null ? held === undefined : held === value) return null;
    return {
      patch: limitPatch(row.key, value),
      summary:
        value === null
          ? `${area}: ${label} back to the default (${row.key})`
          : `${area}: ${label} set to ${limitText(row, value)}${row.unit === "fraction" ? "%" : ""} (${row.key})`,
    };
  };
}
