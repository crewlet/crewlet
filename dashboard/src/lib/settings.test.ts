/**
 * A non-number setting's change as values: what each kind of field saves, that
 * the default is the key removed and an Off of a setting's own is a value
 * written, and that a change is worked out against the document as it is.
 */

import { expect, test } from "vitest";
import type { SettingRow } from "~/contract/settings.ts";
import type { CompanyDocument } from "~/protocol/types.ts";
import {
  draftTarget,
  openDraft,
  settingPatch,
  settingPlan,
  settingServed,
  settingUnchanged,
} from "./settings.ts";

function row(over: Partial<SettingRow> & Pick<SettingRow, "key" | "kind">): SettingRow {
  return {
    group: "knowledge",
    value: "",
    default: "",
    set: false,
    off: false,
    has_off: false,
    ...over,
  };
}

const ROOT = row({
  key: "knowledge.root_space",
  kind: "container",
  value: "HOME",
  default: "HOME",
  has_off: true,
});

test("a field opens on what the document writes: the default, the setting's Off, or a value", () => {
  expect(openDraft(ROOT)).toEqual({ mode: "default" });
  expect(openDraft({ ...ROOT, value: "", off: true, set: true })).toEqual({ mode: "off" });
  expect(openDraft({ ...ROOT, value: "ORG", set: true })).toEqual({ mode: "value", value: "ORG" });
});

test("what a field saves: trimmed text and lists, an empty one the default, Off the empty value", () => {
  expect(draftTarget(ROOT, { mode: "default" })).toBeNull();
  expect(draftTarget(ROOT, { mode: "off" })).toBe("");
  expect(draftTarget(ROOT, { mode: "value", value: " ORG " })).toBe("ORG");
  expect(draftTarget(ROOT, { mode: "value", value: "  " })).toBeNull();
  const scope = row({ key: "knowledge.scope", kind: "containers", value: [], default: [] });
  expect(draftTarget(scope, { mode: "value", value: [" ENG ", "", "PROD"] })).toEqual([
    "ENG",
    "PROD",
  ]);
  expect(draftTarget(scope, { mode: "value", value: [] })).toBeNull();
  const vectors = row({ key: "knowledge.vectors", kind: "switch", value: true, default: true });
  expect(draftTarget(vectors, { mode: "value", value: false })).toBe(false);
});

test("unchanged is what the document writes; served is what the engine applies", () => {
  expect(settingUnchanged(ROOT, null)).toBe(true);
  expect(settingUnchanged(ROOT, "HOME")).toBe(false);
  const written = { ...ROOT, value: "ORG", set: true };
  expect(settingUnchanged(written, "ORG")).toBe(true);
  expect(settingServed(written, "ORG")).toBe(true);
  expect(settingServed(written, null)).toBe(false);
  expect(settingServed(ROOT, null)).toBe(true);
});

test("a patch nests by the key's path, and null removes the key", () => {
  expect(settingPatch("knowledge.scope", ["ENG"])).toEqual({ knowledge: { scope: ["ENG"] } });
  expect(settingPatch("timezone", null)).toEqual({ timezone: null });
});

test("a change is worked out against the document as it is now", () => {
  const plan = settingPlan(ROOT, "Company pages", "ORG", "Doclet", String);
  expect(plan({ knowledge: { root_space: "ORG" } } as unknown as CompanyDocument)).toBeNull();
  expect(plan({} as CompanyDocument)).toEqual({
    patch: { knowledge: { root_space: "ORG" } },
    summary: "Doclet: Company pages set to ORG (knowledge.root_space)",
  });
  const back = settingPlan(ROOT, "Company pages", null, "Doclet", String);
  expect(back({} as CompanyDocument)).toBeNull();
  expect(back({ knowledge: { root_space: "ORG" } } as unknown as CompanyDocument)).toEqual({
    patch: { knowledge: { root_space: null } },
    summary: "Doclet: Company pages back to the default (knowledge.root_space)",
  });
  // AN OFF ALREADY WRITTEN is the empty value, and a write of it is nothing.
  const off = settingPlan(ROOT, "Company pages", "", "Doclet", String);
  expect(off({ knowledge: { root_space: "" } } as unknown as CompanyDocument)).toBeNull();
});
