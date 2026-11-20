/**
 * The one setting each of Crewlet's two helpers has: the model it runs on
 * (`assistant.model` for Ask Crewlet, `advisor.model` for the Advisor), chosen
 * by an administrator in Settings › Models alone, on the helpers' line under its list;
 * neither helper's own page names it.
 *
 * WHICH MODELS CAN RUN A HELPER IS THE ENGINE'S ANSWER (`llm_entries`'
 * `helper_problem`): a model whose harness keeps tools of its own that Crewlet
 * cannot turn off is listed disabled, with the engine's reason beside it,
 * never left out, so the person reads why their model is not offered.
 *
 * THE CHOICE IS A SETTING WRITE like any other (`lib/useSettingWrite.ts`):
 * read at the moment of the save, checked first, saved under the revision it
 * was read at, and drawn as applied once this node says so. None removes the
 * key, which turns Ask Crewlet off and the Advisor's model work with it.
 */

import { useEffect, useState, type ReactNode } from "react";
import { Select, Text } from "@crewlethq/ui";
import { documentLimit } from "~/lib/limits.ts";
import { useSettingWrite, type SettingPlan } from "~/lib/useSettingWrite.ts";
import { SettingWriteStatus, standingWords } from "./SettingWriteStatus.tsx";

/** The two helpers, by the block of the company document each is set in. */
export type Helper = "assistant" | "advisor";

/** What each helper is called, where its model is written, and what having none means. */
export const HELPERS: Readonly<
  Record<Helper, { name: string; settingKey: string; without: string }>
> = {
  assistant: {
    name: "Ask Crewlet",
    settingKey: "assistant.model",
    without: "Ask Crewlet is off without a model",
  },
  advisor: {
    name: "Advisor",
    settingKey: "advisor.model",
    without: "Its checks still run, with no explanations or report summary",
  },
};

/** One model a helper may be given, or why it cannot be. */
export interface HelperChoice {
  key: string;
  /** The vendor's model it calls, said under the key. */
  model: string;
  /** Why it cannot run a helper, in the engine's words; null where it can. */
  problem: string | null;
  /** Drawn before its key on the option and the trigger: its harness's mark. */
  leading?: ReactNode;
  /** Its words on the option and the trigger, where they say more than its key (the model it runs, by name). */
  label?: ReactNode;
}

/** The change that gives `helper` the model `key`, or takes its model away for null. */
export function helperModelPlan(helper: Helper, key: string | null): SettingPlan {
  const { name, settingKey } = HELPERS[helper];
  return (document) => {
    const held = documentLimit(document, settingKey);
    if ((held ?? null) === key) return null;
    return {
      patch: { [helper]: { model: key } },
      summary:
        key === null
          ? `${name}: model removed (${settingKey})`
          : `${name}: model set to ${key} (${settingKey})`,
    };
  };
}

/** The value Select holds for "no model". */
const NONE = "";

export interface HelperModelProps {
  helper: Helper;
  /** The model assigned now, by its key; null where none is. */
  current: string | null;
  /** The company's models, each with why it cannot run a helper. */
  choices: readonly HelperChoice[];
  /** Whether the reader may choose: an administrator, who writes the company document. */
  canAssign: boolean;
  /** Read again what the page draws, once the change is applied or a reload is the remedy. */
  onApplied: () => void;
  /** How wide the picker is: its own words' width, or its place's whole width in a column of them. */
  width?: "auto" | "full";
}

/** The helper's model: a picker for a reader who may choose, its name for anybody else. */
export function HelperModel({
  helper,
  current,
  choices,
  canAssign,
  onApplied,
  width = "auto",
}: HelperModelProps) {
  const { name } = HELPERS[helper];
  const write = useSettingWrite({
    reread: `Reload to see ${name}'s model as it is now.`,
    onApplied,
  });
  // THE CHOICE SHOWS AT ONCE: the stored model is read back only once the
  // write is applied, seconds later, and the old one standing until then read
  // as a change that was lost. The chosen model is drawn while it is checked
  // and saved, and the stored one again where nothing was saved.
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
  const shown = chosen !== undefined ? chosen : current;
  if (!canAssign) {
    return (
      <Text as="span" variant="caption" tone="secondary" className="helper-model">
        {current === null ? "No model chosen" : `Runs on ${current}`}
      </Text>
    );
  }
  // A MODEL ASSIGNED BUT NOT LISTED (removed from the company since) is still
  // the value, drawn as what it is rather than as None.
  const listed = choices.some((c) => c.key === shown);
  const options = [
    ...choices.map((c) => ({
      value: c.key,
      label: c.label ?? c.key,
      text: c.key,
      ...(c.leading ? { leading: c.leading } : {}),
      // THE MODEL IS IN THE LABEL where one is given; the line under it is then the reason alone.
      ...(c.problem !== null || !c.label ? { description: c.problem ?? c.model } : {}),
      disabled: c.problem !== null && c.key !== shown,
    })),
    ...(shown !== null && !listed
      ? [{ value: shown, label: shown, description: "Not one of the company's models" }]
      : []),
    { value: NONE, label: "None", description: HELPERS[helper].without },
  ];
  return (
    <span className="helper-model">
      <Select
        ariaLabel={`${name}'s model`}
        size="sm"
        width={width}
        value={shown ?? NONE}
        options={options}
        disabled={write.busy}
        onChange={(value) => {
          const key = String(value) === NONE ? null : String(value);
          setChosen(key);
          write.submit(helperModelPlan(helper, key));
        }}
      />
      <SettingWriteStatus write={write} onReload={onApplied} answersWarning />
      {write.state.kind === "saved" ? (
        <Text as="span" variant="caption" tone="secondary" role="status">
          {chosen === null || (chosen === undefined && current === null)
            ? `${name} has no model now`
            : `${name} now runs on ${chosen ?? current}`}
          {write.state.standing === "applied" || write.state.standing === "passed"
            ? "."
            : `, ${standingWords(write.state.standing)}`}
        </Text>
      ) : null}
    </span>
  );
}
