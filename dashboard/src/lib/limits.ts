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

import {
  DURATION_UNIT_SECONDS,
  durationValue,
  type DurationUnit,
  type DurationValue,
} from "@crewlethq/ui";
import type { LimitsAnswer, SettingRange } from "~/contract/limits.ts";
import { fmtSetting, plural } from "~/lib/format.ts";
import type { CompanyDocument, ConfigProblem } from "~/protocol/types.ts";
import type { SettingChange, SettingPlan } from "./useSettingWrite.ts";

export type LimitRow = LimitsAnswer["limits"][number];

/** A cap as typed: a value to write, the default (the key removed), or why it is neither. */
export type TypedLimit =
  | { readonly ok: true; readonly value: number | null }
  | { readonly ok: false; readonly error: string };

/** What a cap's field holds: text for a number, an amount and a unit for a duration. Empty is the default. */
export type LimitDraft = string | DurationValue;

/** The units a duration cap's field offers: each its range holds a whole one of, seconds only where needed. */
export function durationUnits(row: LimitRow): DurationUnit[] {
  const range = row.range;
  const wholeMinutes = row.value % 60 === 0 && row.default % 60 === 0;
  return (["days", "hours", "minutes", "seconds"] as const).filter((unit) => {
    const size = DURATION_UNIT_SECONDS[unit];
    if (range?.max !== undefined && range.max < size) return false;
    return unit !== "seconds" || !wholeMinutes || range === undefined || range.min < 60;
  });
}

/** A cap's field as the pencil opens it: empty for the default, else the value in the field's unit. */
export function limitDraft(row: LimitRow): LimitDraft {
  const atDefault = row.value === row.default;
  if (row.unit === "seconds") {
    return durationValue(atDefault ? null : row.value, row.default, durationUnits(row));
  }
  return atDefault ? "" : limitText(row, row.value);
}

/** The least and greatest a cap's field steps to, in the unit it is typed in. */
export function limitBounds(row: LimitRow): { min?: number; max?: number } {
  const range = row.range;
  if (!range) return row.unit === "seconds" || row.unit === "fraction" ? { min: 0 } : {};
  const scale = row.unit === "fraction" ? 100 : 1;
  const step = row.unit === "seconds" ? 1 : 1 / scale;
  const least = range.or_zero ? 0 : range.min_exclusive ? range.min + step : range.min;
  return {
    min: Math.round(least * scale * 100) / 100,
    ...(range.max === undefined ? {} : { max: Math.round(range.max * scale * 100) / 100 }),
  };
}

/** Common durations a duration's chips offer, in seconds. */
const DURATION_LADDER = [
  60, 300, 900, 1_800, 3_600, 7_200, 21_600, 43_200, 86_400, 259_200, 604_800, 1_209_600, 2_592_000,
  7_776_000, 31_536_000,
];

/** Common numbers of days a day count's chips offer. */
const DAYS_LADDER = [1, 7, 14, 30, 90, 180, 365, 730, 1_825, 3_650];

/** Common shares a percentage's chips offer, as fractions of one. */
const FRACTION_LADDER = [0.1, 0.25, 0.5, 0.75, 1];

/**
 * The values a cap's chips offer beside its default, in the cap's own unit:
 * the four of a common ladder nearest the default that the range admits. A
 * count has no common values, so it offers only its default.
 */
export function limitPresets(row: LimitRow): number[] {
  const ladder =
    row.unit === "seconds"
      ? DURATION_LADDER
      : row.unit === "days"
        ? DAYS_LADDER
        : row.unit === "fraction"
          ? FRACTION_LADDER
          : [];
  const around = Math.log(Math.max(row.default, ladder[0] ?? 1));
  return ladder
    .filter((v) => v !== row.default && (!row.range || rangeAdmits(row.range, v)))
    .sort((a, b) => Math.abs(Math.log(a) - around) - Math.abs(Math.log(b) - around))
    .slice(0, 4)
    .sort((a, b) => a - b);
}

/** The field's suffix for a cap of this unit. */
export function limitUnitWords(row: LimitRow): string | undefined {
  switch (row.unit) {
    case "fraction":
      return "%";
    case "per_second":
      return "per second";
    case "days":
      return "days";
    case "count":
    case "seconds":
      return undefined;
  }
}

/** A cap's value as its field shows it: the number a person would type. */
export function limitText(row: LimitRow, value: number): string {
  // A SHARE IS TYPED AS A PERCENTAGE, rounded to what a float keeps of it:
  // 0.35 times 100 is 35.00000000000001.
  if (row.unit === "fraction") return String(Math.round(value * 10_000) / 100);
  if (row.unit === "seconds") return durationText(value);
  return String(value);
}

/** The units a duration is typed in, by their letter, in seconds. */
const DURATION_UNITS: Readonly<Record<string, number>> = { s: 1, m: 60, h: 3_600, d: 86_400 };

/** A duration as typed: whole days, hours or minutes by their letter, else seconds. */
function durationText(seconds: number): string {
  for (const unit of ["d", "h", "m"] as const) {
    const size = DURATION_UNITS[unit]!;
    if (seconds > 0 && seconds % size === 0) return `${seconds / size}${unit}`;
  }
  return String(seconds);
}

/**
 * What a typed value that is not one of the cap's unit is told: what it is
 * not, which the line under the field (what it takes, and the default) does
 * not already say.
 */
function notOfUnit(row: LimitRow, typed: string): string {
  switch (row.unit) {
    case "seconds":
      return `“${typed}” is not a duration.`;
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
  if (range.or_zero === true && value === 0) return true;
  if (value < range.min || (range.min_exclusive === true && value === range.min)) return false;
  return range.max === undefined || value <= range.max;
}

/**
 * What a cap admits, in its own unit, as the sentence beside its field: "At
 * least 1." A range that also admits 0 says it last, in `zeroWords` where the
 * page knows what 0 means there ("0 keeps messages for ever").
 */
export function rangeWords(row: LimitRow, range: SettingRange, zeroWords?: string): string {
  if (range.or_zero === true) {
    const scale = rangeWords(row, { ...range, or_zero: false }).replace(/\.$/, "");
    return `${scale}, or ${zeroWords ?? "0"}.`;
  }
  const least = boundWords(row, range.min);
  // A BOUND THIS NODE'S OPERATOR SET is named as theirs, so a person refused
  // past it knows a company setting cannot move it.
  const operator =
    range.operator_max === true
      ? " This node’s operator allows no more."
      : range.operator_min === true
        ? " This node’s operator allows no less."
        : "";
  if (range.max === undefined) {
    return (range.min_exclusive ? `More than ${least}.` : `At least ${least}.`) + operator;
  }
  const most = boundWords(row, range.max);
  return (
    (range.min_exclusive
      ? `More than ${least}, and at most ${most}.`
      : `From ${least} to ${most}.`) + operator
  );
}

/**
 * What a cap admits, short, as the line under its field: "1 to 100",
 * "5 minutes to 30 days", "30 to 3,650 days, or 0 to keep them for ever".
 */
export function limitHint(row: LimitRow, zeroWords?: string, unit?: string): string | undefined {
  return row.range ? rangeHint(row, row.range, zeroWords, unit) : undefined;
}

function rangeHint(row: LimitRow, range: SettingRange, zeroWords?: string, unit?: string): string {
  if (range.or_zero === true) {
    return `${rangeHint(row, { ...range, or_zero: false }, undefined, unit)}, or ${zeroWords ?? "0"}`;
  }
  // A COUNT OF A THING THE PAGE NAMES says it once, after both ends: "12 to 256 characters".
  if (unit !== undefined && row.unit === "count" && !range.min_exclusive) {
    const most = range.max === undefined ? "" : ` to ${range.max.toLocaleString()}`;
    return `${range.max === undefined ? "At least " : ""}${range.min.toLocaleString()}${most} ${unit}`;
  }
  const node =
    range.operator_max === true || range.operator_min === true ? " (this node’s limit)" : "";
  const least = boundWords(row, range.min);
  if (range.max === undefined) {
    return `${range.min_exclusive ? "More than" : "At least"} ${least}${node}`;
  }
  const most = boundWords(row, range.max);
  if (range.min_exclusive) return `More than ${least}, up to ${most}${node}`;
  // ONE UNIT WORD FOR BOTH ENDS where they share it: "30 to 3,650 days".
  const shared = row.unit === "days" || row.unit === "per_second";
  return `${shared ? range.min.toLocaleString() : least} to ${most}${node}`;
}

/**
 * Reads what was typed for a cap. Empty is the default, which is always
 * admitted; any other value is held to its unit and then to the range the
 * engine holds the cap to.
 */
export function readLimit(row: LimitRow, draft: LimitDraft, zeroWords?: string): TypedLimit {
  if (typeof draft !== "string") return readDurationDraft(row, draft, zeroWords);
  const typed = draft.trim().replace(/%$/, "").trim();
  if (typed === "") return { ok: true, value: null };
  if (row.unit === "seconds") return readDuration(row, typed, zeroWords);
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
    case "count":
    case "per_second":
    case "days":
      if (!Number.isInteger(n)) return { ok: false, error: notOfUnit(row, typed) };
      value = n;
      break;
  }
  if (row.range && !rangeAdmits(row.range, value)) {
    return { ok: false, error: rangeWords(row, row.range, zeroWords) };
  }
  return { ok: true, value };
}

/** A duration's amount, in the unit picked beside it. */
function readDurationDraft(row: LimitRow, draft: DurationValue, zeroWords?: string): TypedLimit {
  const typed = draft.amount.trim();
  if (typed === "") return { ok: true, value: null };
  if (!/^-?\d+(\.\d+)?$/.test(typed)) return { ok: false, error: `“${typed}” is not a number.` };
  const value = Math.round(Number(typed) * DURATION_UNIT_SECONDS[draft.unit]);
  if (row.range && !rangeAdmits(row.range, value)) {
    return { ok: false, error: rangeWords(row, row.range, zeroWords) };
  }
  return { ok: true, value };
}

/** A duration as typed: a number of seconds, or of minutes, hours or days by their letter. */
function readDuration(row: LimitRow, typed: string, zeroWords?: string): TypedLimit {
  const match = /^(-?\d+(?:\.\d+)?)\s*([smhd])?$/i.exec(typed);
  if (!match) return { ok: false, error: notOfUnit(row, typed) };
  const value = Number(match[1]) * DURATION_UNITS[(match[2] ?? "s").toLowerCase()]!;
  if (!Number.isFinite(value)) return { ok: false, error: notOfUnit(row, typed) };
  if (row.range && !rangeAdmits(row.range, value)) {
    return { ok: false, error: rangeWords(row, row.range, zeroWords) };
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
export function limitRefusal(
  row: LimitRow,
  problems: readonly ConfigProblem[],
  zeroWords?: string,
): string {
  const said = new Set<string>();
  let unsaid = false;
  for (const p of problems) {
    if (p.path === row.key && p.range) said.add(rangeWords(row, p.range, zeroWords));
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
          : `${area}: ${label} set to ${row.unit === "seconds" ? fmtSetting(value) : limitText(row, value)}${row.unit === "fraction" ? "%" : ""} (${row.key})`,
    };
  };
}
