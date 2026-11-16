/**
 * One setting of the company document that is not a number, drawn and changed
 * where it is read: the engine's row for it (`queries.SettingRow`), its value
 * in force with the pencil that changes it, and where it comes from.
 *
 * The number's twin (`LimitSetting`), on the same editor (`SettingEditor`):
 * the field is the kind's (a line of text, a zone, a space key with its Off, a
 * list of keys, or a switch), and the rest is the one write path. No rule of
 * the value is checked here: every save is the engine's to check first, and a
 * refusal is said in the words its rule wrote for a person (`lib/settings.ts`).
 */

import { InlineCode, Input, SegmentedControl, Select, TagsInput } from "@crewlethq/ui";
import type { ReactNode } from "react";
import { SettingEditor } from "~/components/SettingEditor.tsx";
import type { SettingRow } from "~/contract/settings.ts";
import {
  draftTarget,
  openDraft,
  sameValue,
  settingPlan,
  settingServed,
  settingUnchanged,
  type SettingDraft,
  type SettingTarget,
  type SettingValue,
} from "~/lib/settings.ts";

/** A value as a sentence says it: text as written, a list joined, a switch On or Off. */
export function settingWords(value: SettingValue): string {
  if (Array.isArray(value)) return value.join(", ");
  if (typeof value === "boolean") return value ? "On" : "Off";
  return value;
}

/** Every zone this browser knows, with the value in force where it does not. */
function zoneNames(current: string): string[] {
  const known =
    typeof Intl.supportedValuesOf === "function" ? Intl.supportedValuesOf("timeZone") : [];
  return current && !known.includes(current) ? [current, ...known] : known;
}

export function TextSetting({
  row,
  label,
  area,
  reread,
  onApplied,
  words = settingWords,
  offWords = "Off",
  valueWords = "Another space",
  helper,
  placeholder,
  choices,
  switchWords = { on: "On", off: "Off" },
}: {
  row: SettingRow;
  /** What the setting is called on its page; its key is drawn beside it. */
  label: string;
  /** The page it is changed on, which opens the revision's summary: "Doclet". */
  area: string;
  /** How a conflict ends, in the page's words. */
  reread: string;
  /** Called once this node applies the saved revision, to read the page's answer again. */
  onApplied: () => void;
  /** A value as the page says it: "Every space" for an empty list. */
  words?: (value: SettingValue) => string;
  /** What the setting's own Off reads as. */
  offWords?: string;
  /** The choice beside Default and Off that takes a value of its own. */
  valueWords?: string;
  /** The line under the field. */
  helper: string;
  /** What an empty text field shows. */
  placeholder?: string;
  /** Values a list offers to pick from, beside what is typed: the spaces there are. */
  choices?: readonly string[];
  /** What a switch's two choices are called, where On and Off say it badly: Required, Optional. */
  switchWords?: { readonly on: string; readonly off: string };
}) {
  const defaultWords = words(row.default);
  // A KEY OR A DOMAIN IS A MACHINE VALUE, drawn as one.
  const drawValue = (value: SettingValue, off: boolean): ReactNode => {
    if (off) return offWords;
    if (row.kind === "text" || row.kind === "container") {
      return <InlineCode>{words(value)}</InlineCode>;
    }
    return words(value);
  };
  return (
    <SettingEditor<SettingDraft, SettingTarget>
      label={label}
      settingKey={row.key}
      answer={row}
      settable
      reread={reread}
      onApplied={onApplied}
      draw={(saved) => {
        const target = saved ? saved.value : undefined;
        // THE SAVED VALUE while it applies: the default for a removed key,
        // the Off for an empty one.
        const value = target === undefined ? row.value : target === null ? row.default : target;
        const set = target === undefined ? row.set : target !== null;
        const off = row.has_off && value === "";
        return {
          value: drawValue(value, off),
          // Provenance only: a text default can be long; the editor names it.
          source: !set ? "Default" : sameValue(value, row.default) ? "Set" : "Changed",
          changed: set,
        };
      }}
      edit={{
        open: () => {
          const draft = openDraft(row);
          // A FIELD WITH NO CHOICE OF MODES opens on its text, empty for the
          // default.
          if (draft.mode === "default" && !row.has_off && row.kind !== "switch") {
            return { mode: "value", value: row.kind === "containers" ? [] : "" };
          }
          return draft;
        },
        read: (draft) => {
          if (draft.mode === "value" && draft.value === "" && row.has_off) {
            return { ok: false, error: "Type a space key, or choose Default or Off." };
          }
          return { ok: true, value: draftTarget(row, draft) };
        },
        same: (target) => settingUnchanged(row, target),
        sameWords: `This is the ${label.toLowerCase()} already.`,
        unchangedWords: "Already set to that.",
        // THE OFF IS SAID AS THE PAGE CALLS IT in the revision's summary.
        plan: (target) =>
          settingPlan(row, label, target, area, (v) =>
            row.has_off && v === "" ? offWords : words(v),
          ),
        served: (target) => settingServed(row, target),
        helper,
        field: ({ id, state, set, error, busy }) => {
          switch (row.kind) {
            case "zone":
              return (
                <Select
                  id={id}
                  ariaLabel={label}
                  searchable
                  disabled={busy}
                  error={error}
                  value={state.mode === "value" ? String(state.value) : ""}
                  options={[
                    { value: "", label: `Default (${defaultWords})` },
                    ...zoneNames(row.set ? String(row.value) : "").map((z) => ({
                      value: z,
                      label: z,
                    })),
                  ]}
                  onChange={(next) =>
                    set(next === "" ? { mode: "default" } : { mode: "value", value: String(next) })
                  }
                />
              );
            case "containers":
              return (
                <TagsInput
                  id={id}
                  label={label}
                  allowCustom
                  monospace
                  disabled={busy}
                  aria-invalid={error}
                  focusOnMount
                  value={state.mode === "value" && Array.isArray(state.value) ? state.value : []}
                  options={(choices ?? []).map((c) => ({ value: c }))}
                  placeholder="Add a space key"
                  onChange={(next) => set({ mode: "value", value: next })}
                />
              );
            case "switch":
              return (
                <SegmentedControl
                  label={label}
                  semantics="radio"
                  value={state.mode === "value" ? (state.value ? "on" : "off") : "default"}
                  options={[
                    {
                      value: "default",
                      label: `Default (${row.default === true ? switchWords.on : switchWords.off})`,
                    },
                    { value: "on", label: switchWords.on },
                    { value: "off", label: switchWords.off },
                  ]}
                  onValueChange={(next) =>
                    set(
                      next === "default"
                        ? { mode: "default" }
                        : { mode: "value", value: next === "on" },
                    )
                  }
                />
              );
            case "container":
              if (row.has_off) {
                return (
                  <SegmentedControl
                    label={label}
                    semantics="radio"
                    layout="chips"
                    value={state.mode}
                    options={[
                      { value: "default", label: `Default (${defaultWords})` },
                      { value: "off", label: offWords },
                      { value: "value", label: valueWords },
                    ]}
                    onValueChange={(next) =>
                      set(
                        next === "value"
                          ? { mode: "value", value: row.set && !row.off ? String(row.value) : "" }
                          : { mode: next },
                      )
                    }
                    trailing={
                      state.mode === "value" ? (
                        <Input
                          id={id}
                          aria-label={`${label}: space key`}
                          autoFocus
                          inputSize="sm"
                          value={String(state.value)}
                          placeholder="ENG"
                          autoComplete="off"
                          spellCheck={false}
                          error={error}
                          disabled={busy}
                          onChange={(event) => set({ mode: "value", value: event.target.value })}
                        />
                      ) : undefined
                    }
                  />
                );
              }
              break;
            case "text":
              break;
          }
          return (
            <Input
              id={id}
              value={state.mode === "value" ? String(state.value) : ""}
              autoFocus
              placeholder={placeholder ?? `Default: ${defaultWords}`}
              autoComplete="off"
              spellCheck={false}
              error={error}
              disabled={busy}
              onChange={(event) => set({ mode: "value", value: event.target.value })}
            />
          );
        },
      }}
    />
  );
}
