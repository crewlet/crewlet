/**
 * The one setting each of Crewlet's two helpers has: the fleet or model it
 * runs on (`assistant.model` for Ask Crewlet, `advisor.model` for the
 * Advisor), chosen by an administrator under Who runs on which model.
 *
 * THE OPTIONS ARE THE PAGE'S (`pickerOptions`): the fleets, then the models,
 * each model the engine says cannot run a helper listed disabled with its
 * reason. A pick asks once, then is a setting write (`lib/useSettingWrite.ts`):
 * read at the save, checked first, saved under the revision it was read at.
 * None removes the key, which turns Ask Crewlet off and the Advisor's model
 * work with it.
 */

import { useEffect, useState } from "react";
import { Select, VisuallyHidden, type SelectOption } from "@crewlethq/ui";
import { ConfirmModal } from "~/components/Modal.tsx";
import { documentLimit } from "~/lib/limits.ts";
import { useSettingWrite, type SettingPlan } from "~/lib/useSettingWrite.ts";
import { SettingWriteStatus, standingWords } from "./SettingWriteStatus.tsx";

/** The two helpers, by the block of the company document each is set in. */
export type Helper = "assistant" | "advisor";

/** What each helper is called, where its model is written, and what having none means. */
export const HELPERS: Readonly<
  Record<Helper, { name: string; settingKey: string; without: string; off: string }>
> = {
  assistant: {
    name: "Ask Crewlet",
    settingKey: "assistant.model",
    without: "Ask Crewlet is off without a model",
    off: "Turn Ask Crewlet off",
  },
  advisor: {
    name: "Advisor",
    settingKey: "advisor.model",
    without: "Its checks still run, with no explanations or report summary",
    off: "Run the Advisor with no model",
  },
};

/** The change that sets `helper` to `value` (a model key or `fleet:<key>`), or takes it away for null. */
export function helperModelPlan(helper: Helper, value: string | null): SettingPlan {
  const { name, settingKey } = HELPERS[helper];
  return (document) => {
    const held = documentLimit(document, settingKey);
    if ((held ?? null) === value) return null;
    return {
      patch: { [helper]: { model: value } },
      summary:
        value === null
          ? `${name}: model removed (${settingKey})`
          : `${name}: model set to ${value} (${settingKey})`,
    };
  };
}

/** The value Select holds for "no model". */
const NONE = "";

export interface HelperModelProps {
  helper: Helper;
  /** What it runs on now: a model key or `fleet:<key>`; null where none is set. */
  current: string | null;
  /** The page's picker options (`pickerOptions`). */
  options: readonly SelectOption[];
  /** A value in words, for the confirm: "Fleet · Claude Opus", or the model's key. */
  wordOf: (value: string) => string;
  /** Whether the reader may choose: an administrator, who writes the company document. */
  canAssign: boolean;
  /** Read again what the page draws, once the change is applied or a reload is the remedy. */
  onApplied: () => void;
  /** A pick waiting for its confirm (null for None), or undefined once it is answered. */
  onPending?: (value: string | null | undefined) => void;
}

/** The helper's picker: quiet and full width, as every picker in Who runs is. */
export function HelperModel({
  helper,
  current,
  options,
  wordOf,
  canAssign,
  onApplied,
  onPending,
}: HelperModelProps) {
  const { name } = HELPERS[helper];
  const write = useSettingWrite({
    reread: `Reload to see ${name}'s model as it is now.`,
    onApplied,
  });
  // THE CHOICE SHOWS AT ONCE: the stored value is read back only once the write is applied.
  const [chosen, setChosen] = useState<string | null | undefined>(undefined);
  const settled = write.state.kind;
  useEffect(() => {
    if (settled === "refused" || settled === "unchanged" || settled === "idle") {
      setChosen(undefined);
    }
  }, [settled]);
  useEffect(() => {
    if (chosen !== undefined && chosen === current) setChosen(undefined);
  }, [chosen, current]);
  const [asked, setAsked] = useState<{ value: string | null } | null>(null);
  const held = chosen !== undefined ? chosen : current;
  const shown = asked ? asked.value : held;
  const ask = (next: { value: string | null } | null) => {
    setAsked(next);
    onPending?.(next ? next.value : undefined);
  };
  // A VALUE NOT LISTED (a model removed since) is still the value, said as what it is.
  const listed = options.some((o) => o.value === shown);
  const choices: SelectOption[] = [
    ...options.map((o) => (o.value === shown ? { ...o, disabled: false } : o)),
    ...(shown && !listed
      ? [{ value: shown, label: wordOf(shown), description: "Not one of the company's models" }]
      : []),
    { value: NONE, label: "None", description: HELPERS[helper].without },
  ];
  return (
    <span className="helper-model">
      <Select
        ariaLabel={`${name}'s model`}
        size="sm"
        width="full"
        appearance="quiet"
        inset="flush"
        hintColumn="fit"
        value={shown ?? NONE}
        options={choices}
        disabled={!canAssign || write.busy}
        onChange={(picked) => {
          const value = String(picked) === NONE ? null : String(picked);
          if (value !== held) ask({ value });
        }}
      />
      {asked ? (
        <ConfirmModal
          open
          onClose={() => ask(null)}
          onConfirm={() => {
            setChosen(asked.value);
            write.submit(helperModelPlan(helper, asked.value));
            ask(null);
          }}
          {...(asked.value === null
            ? {
                title: `${HELPERS[helper].off}?`,
                message: `${HELPERS[helper].without}.`,
                confirmLabel: "Turn off",
                destructive: true,
              }
            : {
                title: `Move ${name} to ${wordOf(asked.value)}?`,
                message: "It moves at its next turn; a running turn finishes where it is.",
                confirmLabel: "Move",
                changes: [
                  {
                    key: helper,
                    subject: name,
                    subjectText: name,
                    rows: [
                      {
                        kind: "changed" as const,
                        label: "Model",
                        from: held ? wordOf(held) : "Not set",
                        to: wordOf(asked.value),
                      },
                    ],
                  },
                ],
              })}
        />
      ) : null}
      <SettingWriteStatus write={write} onReload={onApplied} answersWarning />
      {/* THE PICKER SHOWS THE NEW VALUE; a screen reader is told it was saved. */}
      {write.state.kind === "saved" ? (
        <VisuallyHidden>
          <span role="status">
            {held === null ? `${name} has no model now` : `${name} now runs on ${wordOf(held)}`}
            {write.state.standing === "applied" || write.state.standing === "passed"
              ? "."
              : `, ${standingWords(write.state.standing)}`}
          </span>
        </VisuallyHidden>
      ) : null}
    </span>
  );
}
