/**
 * The one setting each of Crewlet's two helpers has: the model it runs on
 * (`assistant.model` for Ask Crewlet, `advisor.model` for the Advisor), shown
 * in the head of the helper's own page and chosen there by an administrator.
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

import { useCallback, useMemo } from "react";
import { Select, Text } from "@crewlethq/ui";
import { documentLimit } from "~/lib/limits.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useConfigWriteAccess } from "~/lib/useWriteAccess.ts";
import { useSettingWrite, type SettingPlan } from "~/lib/useSettingWrite.ts";
import type { ModelEntriesAnswer } from "~/protocol/models.ts";
import type { CompanyDocument } from "~/protocol/types.ts";
import { SettingWriteStatus } from "./SettingWriteStatus.tsx";

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
}

/** The helper's model: a picker for a reader who may choose, its name for anybody else. */
export function HelperModel({ helper, current, choices, canAssign, onApplied }: HelperModelProps) {
  const { name } = HELPERS[helper];
  const write = useSettingWrite({
    reread: `Reload to see ${name}'s model as it is now.`,
    onApplied,
  });
  if (!canAssign) {
    return (
      <Text as="span" variant="caption" tone="secondary" className="helper-model">
        {current === null ? "No model chosen" : `Runs on ${current}`}
      </Text>
    );
  }
  // A MODEL ASSIGNED BUT NOT LISTED (removed from the company since) is still
  // the value, drawn as what it is rather than as None.
  const listed = choices.some((c) => c.key === current);
  const options = [
    ...choices.map((c) => ({
      value: c.key,
      label: c.key,
      description: c.problem ?? c.model,
      disabled: c.problem !== null && c.key !== current,
    })),
    ...(current !== null && !listed
      ? [{ value: current, label: current, description: "Not one of the company's models" }]
      : []),
    { value: NONE, label: "None", description: HELPERS[helper].without },
  ];
  return (
    <span className="helper-model">
      <Select
        ariaLabel={`${name}'s model`}
        size="sm"
        width="auto"
        value={current ?? NONE}
        options={options}
        disabled={write.busy}
        onChange={(value) =>
          write.submit(helperModelPlan(helper, String(value) === NONE ? null : String(value)))
        }
      />
      <SettingWriteStatus write={write} onReload={onApplied} answersWarning />
    </span>
  );
}

/**
 * Every model of the company as a helper's choice, with the engine's reason
 * where it cannot run one: the administrator's reading, the only one that
 * says it, and the only reader who chooses.
 */
export function helperChoices(answer: ModelEntriesAnswer | null): HelperChoice[] {
  if (answer?.reading !== "admin") return [];
  return answer.entries.map((entry) => ({
    key: entry.key,
    model: entry.model,
    problem: entry.helper_problem,
  }));
}

/**
 * A helper's model as its page draws it: the key assigned, read off the
 * company document an administrator reads, the company's models to choose
 * from, and whether this reader may choose. A reader who may not choose is
 * told the model's name by the page from what it already reads.
 */
export function useHelperModel(helper: Helper): {
  current: string | null;
  choices: HelperChoice[];
  canAssign: boolean;
  reload: () => void;
} {
  const access = useConfigWriteAccess();
  const entries = useQuery("llm_entries", undefined, { enabled: access.can });
  const config = useQuery("config", undefined, { enabled: access.can });
  const doc = (config.data as CompanyDocument | null) ?? null;
  const held = doc ? documentLimit(doc, HELPERS[helper].settingKey) : undefined;
  const choices = useMemo(() => helperChoices(entries.data), [entries.data]);
  const { refetch: refetchEntries } = entries;
  const { refetch: refetchConfig } = config;
  const reload = useCallback(() => {
    refetchEntries();
    refetchConfig();
  }, [refetchEntries, refetchConfig]);
  return {
    current: typeof held === "string" && held !== "" ? held : null,
    choices,
    canAssign: access.can,
    reload,
  };
}
