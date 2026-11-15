/**
 * A schedule changed from the dashboard: added, edited, paused, resumed or
 * removed, as a write of the seat or the team that declares it.
 *
 * # A schedule is a line of the company document
 *
 * The engine keeps no schedule anywhere else: a seat's are its `roles[]`
 * entry's `schedules`, a team's its `units[]` entry's. So every change here is
 * a replacement of that ONE entity through `PUT /config/{roles|units}/{id}`
 * (`protocol/configWrite.ts`): read with the tag of its revision, its
 * `schedules` list changed and nothing else, checked by the engine's dry run,
 * and stored under that tag with a summary. A unit's entity carries its seats
 * and its child units inside it; it goes back whole, so a write of its
 * schedules cannot drop or rewrite anything it did not mean to.
 *
 * # Which id the route addresses
 *
 * NOT the `scope_id` a `/schedules` row carries. That is the scope's
 * IDENTITY in the dispatch ledger, a seat's derived agent id, which the
 * configuration routes know nothing of; a seat is addressed there by its
 * HANDLE, the row's `scope_name`. A unit is its key either way ([ownerOf]).
 *
 * # Pause is the document's own switch
 *
 * Pausing a schedule is `enabled: false` on it, resuming `enabled: true`:
 * what a person reads as paused is the schedule switched off in the company's
 * configuration, which every node's next tick reads. Nothing else holds it.
 *
 * # The name is the schedule's identity
 *
 * A fire's claim, the schedule's history and its conversation on each seat
 * are all keyed on its name, so a rename starts its conversation afresh and
 * leaves its history under the old name (docs/concepts/scheduling.md). An edit
 * here never renames: the name is fixed once the schedule exists, and a
 * schedule wanted under another name is a new one.
 */

import type { ConfigRefusal } from "~/protocol/configAnswer.ts";
import { managedSentence, unsettledSentence } from "~/protocol/configAnswer.ts";
import type { ConfigProblem, ScheduleRow } from "~/protocol/types.ts";
import { configGuardedReason } from "~/lib/useWriteAccess.ts";
import { handleLabel } from "~/lib/seats.ts";
import { cronProblem } from "~/lib/cron.ts";
import { knownZone } from "~/lib/formatters.ts";

/** What declares a schedule: a seat (`role`) or a team (`unit`). */
export type ScheduleScope = "role" | "unit";

/** Who runs a team's schedule: every agent in it, or its lead. */
export type ScheduleTarget = "each" | "lead";

/**
 * The seat or team a schedule is written on, as the configuration routes
 * address it.
 */
export interface ScheduleOwner {
  readonly scope: ScheduleScope;
  /** The entity's id at `/config/{roles|units}/{id}`: a seat's handle, a unit's key. */
  readonly id: string;
}

/** The owner a `/schedules` row's schedule is written on ([ScheduleOwner.id]). */
export function ownerOf(
  row: Pick<ScheduleRow, "scope_type" | "scope_id" | "scope_name">,
): ScheduleOwner {
  return row.scope_type === "role"
    ? { scope: "role", id: row.scope_name }
    : { scope: "unit", id: row.scope_id };
}

/** The collection the configuration routes keep an owner's entity in. */
export function entityPath(owner: ScheduleOwner): "roles" | "units" {
  return owner.scope === "role" ? "roles" : "units";
}

/** An owner as a summary names it: `@handle` for a seat, the unit by its key. */
export function ownerWords(owner: ScheduleOwner): string {
  return owner.scope === "role" ? handleLabel(owner.id) : `the unit ${owner.id}`;
}

/**
 * What the form holds, every field as typed. `timezone` is empty for the
 * company's own clock, which the schedule then follows if the company's
 * changes; `timeoutMinutes` empty for the engine's default cap.
 */
export interface ScheduleDraft {
  readonly name: string;
  readonly cron: string;
  readonly timezone: string;
  readonly task: string;
  readonly target: ScheduleTarget;
  readonly timeoutMinutes: string;
  readonly catchup: boolean;
  readonly enabled: boolean;
}

/** A new schedule's form: fires on the company's clock, catches up, switched on. */
export const NEW_DRAFT: ScheduleDraft = {
  name: "",
  cron: "",
  timezone: "",
  task: "",
  target: "each",
  timeoutMinutes: "",
  catchup: true,
  enabled: true,
};

/** The fields a draft has, each of which a located problem can be about. */
export type DraftField =
  "name" | "cron" | "timezone" | "task" | "target" | "timeout" | "catchup" | "enabled";

/** A schedule as the document holds it: the entity's own object, every key kept. */
export type WrittenSchedule = Record<string, unknown>;

/** The draft an existing schedule opens the form with. */
export function draftOf(written: WrittenSchedule): ScheduleDraft {
  const seconds = typeof written.timeout_seconds === "number" ? written.timeout_seconds : 0;
  return {
    name: str(written.name),
    cron: str(written.cron),
    timezone: str(written.timezone),
    task: str(written.task),
    target: written.target === "lead" ? "lead" : "each",
    timeoutMinutes: seconds > 0 ? String(seconds / 60) : "",
    catchup: written.catchup !== false,
    enabled: written.enabled !== false,
  };
}

/**
 * The schedule the draft writes: `original`'s object with the form's fields
 * set, so a key this screen does not draw (one a newer build wrote) goes back
 * as it came.
 *
 * A DEFAULT STAYS UNWRITTEN where it was: a schedule that never said
 * `enabled`, `catchup` or `target` does not start saying it because a form
 * showed the default, so the revision's diff is what the person changed. A
 * key the document did state is written as the form now says.
 */
export function writtenOf(
  draft: ScheduleDraft,
  scope: ScheduleScope,
  original: WrittenSchedule = {},
): WrittenSchedule {
  const out: WrittenSchedule = { ...original };
  out.name = draft.name.trim();
  out.cron = draft.cron.trim().replace(/\s+/g, " ");
  out.task = draft.task.trim();
  const zone = draft.timezone.trim();
  if (zone) out.timezone = zone;
  else delete out.timezone;
  // A SEAT'S SCHEDULE HAS NO TARGET: the seat runs it. One the document
  // carried anyway is left as it was, since the engine ignores it.
  if (scope === "unit") keepDefault(out, "target", draft.target, "each", original);
  keepDefault(out, "enabled", draft.enabled, true, original);
  keepDefault(out, "catchup", draft.catchup, true, original);
  // THE CAP AS IT WAS where the minutes the form showed were not touched, so a
  // cap of 90 seconds the form drew as 1.5 minutes is not rewritten by an edit
  // of the task.
  const seconds = timeoutSeconds(draft.timeoutMinutes);
  const untouched =
    typeof original.timeout_seconds === "number" &&
    draftOf(original).timeoutMinutes === draft.timeoutMinutes.trim();
  if (!seconds) delete out.timeout_seconds;
  else if (!untouched) out.timeout_seconds = seconds;
  return out;
}

function keepDefault(
  out: WrittenSchedule,
  key: string,
  value: unknown,
  fallback: unknown,
  original: WrittenSchedule,
): void {
  if (value === fallback && !(key in original)) delete out[key];
  else out[key] = value;
}

/** The cap in whole seconds, null for one that is not a number of minutes. */
export function timeoutSeconds(minutes: string): number | null {
  const typed = minutes.trim();
  if (typed === "") return 0;
  const n = Number(typed);
  if (!Number.isFinite(n) || n < 0) return null;
  return Math.round(n * 60);
}

/**
 * What the form refuses before anything is sent, field by field: the checks
 * this screen can make as the engine will (a name, a task, an expression its
 * cron grammar reads, a zone this browser knows, a cap that is a number). The
 * engine's dry run decides the rest, and its problems are drawn the same way
 * ([locatedProblems]).
 */
export function draftProblems(
  draft: ScheduleDraft,
  taken: readonly string[],
): Partial<Record<DraftField, string>> {
  const out: Partial<Record<DraftField, string>> = {};
  const name = draft.name.trim();
  if (name === "") out.name = "Give the schedule a name.";
  else if (taken.includes(name)) out.name = "Another schedule here already has this name.";
  // THE FIELD THAT IS WRONG, named as the engine's grammar finds it ("The
  // minute must be 0 to 59."), never one sentence for every mistake.
  const cron = draft.cron.trim();
  const cronWrong = cron === "" ? "Say when it runs, as a cron expression." : cronProblem(cron);
  if (cronWrong) out.cron = cronWrong;
  const zone = draft.timezone.trim();
  if (zone !== "" && knownZone(zone) === null) out.timezone = "This is not a time zone.";
  if (draft.task.trim() === "") out.task = "Say what the run should do.";
  if (timeoutSeconds(draft.timeoutMinutes) === null)
    out.timeout = "Give the time limit as a number of minutes, or leave it empty.";
  return out;
}

/** The schedule named `name` as the owner's entity writes it, or null. */
export function writtenScheduleOf(
  entity: Record<string, unknown>,
  name: string,
): WrittenSchedule | null {
  const list = Array.isArray(entity.schedules) ? (entity.schedules as unknown[]) : [];
  return list.filter(isRecord).find((s) => s.name === name) ?? null;
}

/** A change to one owner's schedules. */
export type ScheduleChange =
  | { readonly kind: "create"; readonly draft: ScheduleDraft }
  | { readonly kind: "edit"; readonly name: string; readonly draft: ScheduleDraft }
  | { readonly kind: "enable"; readonly name: string; readonly enabled: boolean }
  | { readonly kind: "remove"; readonly name: string };

/** What a change does to the entity as read, or why it cannot be made there. */
export type Applied =
  | {
      readonly kind: "changed";
      readonly entity: Record<string, unknown>;
      /** What the revision is stored under: the change, said as an instruction. */
      readonly summary: string;
      /** What a person is told once it is saved: the change, said as done. */
      readonly done: string;
      /** Where the changed schedule sits in the list, for a problem that names it. */
      readonly index: number;
    }
  /** The change is already what the document says: nothing to save. */
  | { readonly kind: "unchanged" }
  | { readonly kind: "refused"; readonly message: string };

/**
 * The owner's entity with one change made to its `schedules` and nothing
 * else, and the summary its revision is stored under. Pure, over the entity
 * the route answered.
 */
export function applyChange(
  entity: Record<string, unknown>,
  owner: ScheduleOwner,
  change: ScheduleChange,
): Applied {
  const list: WrittenSchedule[] = Array.isArray(entity.schedules)
    ? (entity.schedules as unknown[]).filter(isRecord)
    : [];
  const where = ownerWords(owner);
  const withSchedules = (
    schedules: WrittenSchedule[],
    summary: string,
    done: string,
    index: number,
  ): Applied => {
    const next: Record<string, unknown> = { ...entity };
    if (schedules.length > 0) next.schedules = schedules;
    else delete next.schedules;
    return { kind: "changed", entity: next, summary, done, index };
  };
  if (change.kind === "create") {
    const written = writtenOf(change.draft, owner.scope);
    const name = str(written.name);
    if (list.some((s) => s.name === name)) {
      return {
        kind: "refused",
        message: `${capital(where)} already has a schedule called “${name}”.`,
      };
    }
    return withSchedules(
      [...list, written],
      `Add the schedule “${name}” to ${where}`,
      `Added “${name}” to ${where}`,
      list.length,
    );
  }
  const index = list.findIndex((s) => s.name === change.name);
  if (index < 0) {
    return {
      kind: "refused",
      message: `The schedule “${change.name}” is no longer on ${where} in the company’s configuration. Reload to see it as it is now.`,
    };
  }
  const original = list[index]!;
  const replace = (next: WrittenSchedule) => list.map((s, i) => (i === index ? next : s));
  switch (change.kind) {
    case "edit": {
      // THE NAME IS FIXED: see the module doc.
      const written = writtenOf({ ...change.draft, name: change.name }, owner.scope, original);
      const fields = changedFields(original, written);
      if (fields.length === 0) return { kind: "unchanged" };
      return withSchedules(
        replace(written),
        `Change the schedule “${change.name}” on ${where}: ${fields.join(", ")}`,
        `Saved “${change.name}”`,
        index,
      );
    }
    case "enable": {
      if ((original.enabled !== false) === change.enabled) return { kind: "unchanged" };
      const next = { ...original, enabled: change.enabled };
      return withSchedules(
        replace(next),
        `${change.enabled ? "Resume" : "Pause"} the schedule “${change.name}” on ${where}`,
        `${change.enabled ? "Resumed" : "Paused"} “${change.name}”`,
        index,
      );
    }
    case "remove":
      return withSchedules(
        list.filter((_, i) => i !== index),
        `Remove the schedule “${change.name}” from ${where}`,
        `Deleted “${change.name}”`,
        index,
      );
  }
}

/** The words a summary names each field by. */
const FIELD_WORDS: Record<string, string> = {
  cron: "when it runs",
  timezone: "its time zone",
  task: "its task",
  target: "who runs it",
  timeout_seconds: "its time limit",
  catchup: "catching up",
  enabled: "whether it runs",
};

/** The fields a write changes, in words, in the form's order. */
function changedFields(before: WrittenSchedule, after: WrittenSchedule): string[] {
  return Object.keys(FIELD_WORDS)
    .filter((key) => JSON.stringify(before[key]) !== JSON.stringify(after[key]))
    .map((key) => FIELD_WORDS[key]!);
}

/** A document key of a schedule, as the form names its field. */
const FIELD_OF: Record<string, DraftField> = {
  name: "name",
  cron: "cron",
  timezone: "timezone",
  task: "task",
  target: "target",
  timeout_seconds: "timeout",
  catchup: "catchup",
  enabled: "enabled",
};

/**
 * The engine's problems with a write, each beside the field it is about where
 * it is about the changed schedule, and the rest as they came.
 *
 * BY THE PROBLEM'S SEGMENTS, never its path string (`protocol/types.ts`): the
 * last `schedules` in them, the index after it and the key after that, and
 * what holds that list, a seat (`roles`) or a unit. A seat's problem names
 * the seat by handle; a unit's names the unit by its NAME (the engine's
 * `config.Problem`), which `unitName` is, so a seat's schedule inside the unit
 * at the same index is never taken for the unit's own.
 */
export function locatedProblems(
  problems: readonly ConfigProblem[],
  owner: ScheduleOwner,
  index: number,
  unitName = "",
): { fields: Partial<Record<DraftField, string>>; general: string[] } {
  const fields: Partial<Record<DraftField, string>> = {};
  const general: string[] = [];
  for (const p of problems) {
    const segments = p.segments ?? [];
    const at = segments.lastIndexOf("schedules");
    const onSeat = at >= 2 && segments[at - 2] === "roles";
    const ours =
      at >= 0 &&
      (owner.scope === "role"
        ? onSeat && p.seat === owner.id
        : !onSeat && (p.unit === undefined || p.unit === unitName || p.unit === owner.id));
    const key = segments[at + 2];
    const field = typeof key === "string" ? FIELD_OF[key] : undefined;
    if (ours && segments[at + 1] === index && field && !fields[field]) {
      fields[field] = p.message;
    } else {
      general.push(p.message);
    }
  }
  return { fields, general };
}

/**
 * A refused read or write of the owner's entity in one sentence a person can
 * act on, and whether reading again is the remedy: the reading every writer
 * of the company document gives (`lib/ceilings.ts`'s), in a schedule's words.
 */
export function scheduleRefusalWords(
  refusal: ConfigRefusal | { readonly kind: "missing" },
  owner: ScheduleOwner,
): { message: string; reload: boolean } {
  switch (refusal.kind) {
    case "guarded":
      return { message: configGuardedReason(refusal.code), reload: false };
    case "managed":
      return { message: managedSentence(refusal.managedBy), reload: false };
    case "conflict":
      return {
        message:
          "The company’s configuration changed since this was read, so nothing was saved. Reload to see its schedules as they are now.",
        reload: true,
      };
    case "draining":
      return {
        message: `This engine is draining and saved nothing${refusal.detail ? `: ${refusal.detail}` : "."}`,
        reload: false,
      };
    case "starting":
      return {
        message: `This engine is still starting and saved nothing${refusal.detail ? `: ${refusal.detail}` : "."}`,
        reload: false,
      };
    case "unavailable":
      return { message: refusal.detail, reload: false };
    case "unsettled":
      return {
        message: `${unsettledSentence(refusal)} The change may or may not have landed. Reload before trying again.`,
        reload: true,
      };
    case "missing":
      return {
        message: `${capital(ownerWords(owner))} is no longer in the company’s configuration.`,
        reload: true,
      };
    case "problems":
      return {
        message: refusal.problems.map((p) => p.message).join(" ") || "The engine refused it.",
        reload: false,
      };
  }
}

function str(value: unknown): string {
  return typeof value === "string" ? value : "";
}

function capital(text: string): string {
  return text.charAt(0).toUpperCase() + text.slice(1);
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
