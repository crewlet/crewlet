/**
 * Changing one of the company's settings that is not a number, as values: what
 * the field holds for each kind, the merge patch that writes it, and what the
 * revision is recorded as. `lib/limits.ts` is the numbers' twin.
 *
 * PURE, so every rule a Save turns on is pinned without a network:
 * `lib/useSettingWrite.ts` is the half that sends it.
 *
 * # The engine decides what a value may be
 *
 * No rule of these values is copied here: a domain's grammar, a space key's
 * shape, whether a zone exists and whether search by meaning has a provider
 * are each the validator's, and every save is checked by the engine first
 * (the dry run), whose refusal says it in a person's words. What a field
 * reads is only what it holds: text, a zone, a key, a list, or a switch.
 *
 * # The default is the key's absence
 *
 * Going back to the default REMOVES the key (a merge patch's `null`), as a
 * cap's does, so a later build's default is the company's; an Off of a
 * setting's own (`skills_container: ""`) is a value written, never the
 * default.
 */

import type { SettingRow } from "~/contract/settings.ts";
import { documentLimit } from "./limits.ts";
import type { SettingChange, SettingPlan } from "./useSettingWrite.ts";

/** A setting's value, by kind. */
export type SettingValue = SettingRow["value"];

/** What a save writes: a value, or `null` for the default (the key removed). */
export type SettingTarget = SettingValue | null;

/** Whether two values of a setting are the same value. */
export function sameValue(a: SettingTarget | undefined, b: SettingTarget | undefined): boolean {
  if (Array.isArray(a) && Array.isArray(b)) {
    return a.length === b.length && a.every((v, i) => v === b[i]);
  }
  return a === b;
}

/**
 * What a field of a setting holds: the default, the setting's own Off, or a
 * value. One shape for every kind, so the editor's state is one type.
 */
export type SettingDraft =
  | { readonly mode: "default" }
  | { readonly mode: "off" }
  | { readonly mode: "value"; readonly value: SettingValue };

/** The field as the pencil opens it: what the document writes, or the default. */
export function openDraft(row: SettingRow): SettingDraft {
  if (!row.set) return { mode: "default" };
  if (row.has_off && row.off) return { mode: "off" };
  return { mode: "value", value: row.value };
}

/**
 * What a draft saves. An empty line or list is the default; a value is
 * trimmed, as the engine reads it, and an entry of a list too.
 */
export function draftTarget(row: SettingRow, draft: SettingDraft): SettingTarget {
  switch (draft.mode) {
    case "default":
      return null;
    case "off":
      return "";
    case "value": {
      const v = draft.value;
      if (typeof v === "string") return v.trim() === "" ? null : v.trim();
      if (Array.isArray(v)) {
        const list = v.map((s) => s.trim()).filter(Boolean);
        return list.length === 0 ? null : list;
      }
      return row.kind === "switch" ? v : null;
    }
  }
}

/** Whether a save of `target` leaves the setting as the document writes it now. */
export function settingUnchanged(row: SettingRow, target: SettingTarget): boolean {
  if (target === null) return !row.set;
  return row.set && sameValue(row.value, target);
}

/** Whether the answer the page draws already says `target` is in force. */
export function settingServed(row: SettingRow, target: SettingTarget): boolean {
  return sameValue(row.value, target === null ? row.default : target);
}

/** The merge patch that writes `target` at `key`, or removes the key for `null`. */
export function settingPatch(key: string, target: SettingTarget): Record<string, unknown> {
  let patch: unknown = target;
  const segments = key.split(".");
  for (let i = segments.length - 1; i >= 0; i--) patch = { [segments[i]!]: patch };
  return patch as Record<string, unknown>;
}

/**
 * One setting's change against the document as it is now: nothing where the
 * document already holds it, else the patch and the summary the revision
 * history shows, opened by the page it was made on (`area`) and saying the
 * value as the page draws it (`words`).
 */
export function settingPlan(
  row: SettingRow,
  label: string,
  target: SettingTarget,
  area: string,
  words: (value: SettingValue) => string,
): SettingPlan {
  return (document): SettingChange | null => {
    const held = documentLimit(document, row.key) as SettingTarget | undefined;
    if (target === null ? held === undefined : sameValue(held, target)) return null;
    return {
      patch: settingPatch(row.key, target),
      summary:
        target === null
          ? `${area}: ${label} back to the default (${row.key})`
          : `${area}: ${label} set to ${words(target)} (${row.key})`,
    };
  };
}
