/**
 * Ask Crewlet's instructions as values: a layer's draft, its presets in a
 * reader's words, whether two layers are the same, and the company layer's
 * configuration change. PURE, so every rule the settings screen and the drawer
 * keep is pinned by a test over values.
 *
 * A text is compared and counted TRIMMED, in code points as the engine counts
 * it (`config.AssistantInstructions`), so a draft is marked over the cap where
 * it is typed rather than refused after Save.
 */

import { countCharacters } from "@crewlethq/ui";
import {
  ASSISTANT_INSTRUCTIONS_MAX_RUNES,
  type AnswerFormat,
  type AnswerLength,
  type AssistantInstructions,
} from "~/contract/instructions.ts";
import type { CompanyDocument } from "~/protocol/types.ts";
import { documentLimit } from "./limits.ts";
import type { SettingPlan } from "./useSettingWrite.ts";

/** One layer as its form holds it: "" is the engine's default. */
export interface InstructionsDraft {
  readonly length: AnswerLength | "";
  readonly format: AnswerFormat | "";
  readonly text: string;
}

/** Each preset as a reader picks it, the default first. */
export const LENGTH_CHOICES: readonly { value: AnswerLength | ""; label: string }[] = [
  { value: "", label: "Default" },
  { value: "brief", label: "Brief" },
  { value: "balanced", label: "Balanced" },
  { value: "detailed", label: "Detailed" },
];
export const FORMAT_CHOICES: readonly { value: AnswerFormat | ""; label: string }[] = [
  { value: "", label: "Default" },
  { value: "lead", label: "Answer first" },
  { value: "lists", label: "Bullets and tables" },
  { value: "tldr", label: "TL;DR first" },
];

/** A layer's draft; none is the default everywhere. */
export function draftOf(layer: AssistantInstructions | null | undefined): InstructionsDraft {
  return { length: layer?.length ?? "", format: layer?.format ?? "", text: layer?.text ?? "" };
}

/** The layer a draft saves, with every default left out and its text trimmed. */
export function layerOf(draft: InstructionsDraft): AssistantInstructions {
  const text = draft.text.trim();
  return {
    ...(draft.length ? { length: draft.length } : {}),
    ...(draft.format ? { format: draft.format } : {}),
    ...(text ? { text } : {}),
  };
}

/** Whether a layer sets nothing. */
export function isEmptyLayer(layer: AssistantInstructions | null | undefined): boolean {
  const l = layerOf(draftOf(layer));
  return l.length === undefined && l.format === undefined && l.text === undefined;
}

/** Whether two layers say the same, as the engine would store them. */
export function sameLayer(
  a: AssistantInstructions | null | undefined,
  b: AssistantInstructions | null | undefined,
): boolean {
  const x = layerOf(draftOf(a));
  const y = layerOf(draftOf(b));
  return x.length === y.length && x.format === y.format && x.text === y.text;
}

/** How many characters a text holds as it is saved. */
export function textLength(text: string): number {
  return countCharacters(text.trim());
}

/** Whether a draft's text is past the cap, which Save refuses before sending. */
export function overCap(draft: InstructionsDraft): boolean {
  return textLength(draft.text) > ASSISTANT_INSTRUCTIONS_MAX_RUNES;
}

/** Which layers shape the reader's answers, for the drawer's quiet note. */
export function layersInUse(
  answer: { company: AssistantInstructions; mine: AssistantInstructions } | null | undefined,
): { company: boolean; mine: boolean } {
  return { company: !isEmptyLayer(answer?.company), mine: !isEmptyLayer(answer?.mine) };
}

/** The document key the company layer is written at. */
export const COMPANY_INSTRUCTIONS_KEY = "assistant.instructions";

/**
 * The company layer's change from `base`, the layer the form was filled from,
 * to `next`: whole, every member it leaves out removed. A layer changed since
 * the form was filled is a conflict to reload, never written over.
 */
export function companyInstructionsPlan(
  base: AssistantInstructions,
  next: AssistantInstructions,
): SettingPlan {
  return (document: CompanyDocument) => {
    const held = documentLimit(document, COMPANY_INSTRUCTIONS_KEY) as
      AssistantInstructions | undefined;
    if (sameLayer(held, next)) return null;
    if (!sameLayer(held, base)) return "conflict";
    const layer = layerOf(draftOf(next));
    return {
      patch: {
        assistant: {
          instructions: isEmptyLayer(layer)
            ? null
            : {
                length: layer.length ?? null,
                format: layer.format ?? null,
                text: layer.text ?? null,
              },
        },
      },
      summary: isEmptyLayer(layer)
        ? `Ask Crewlet: company instructions removed (${COMPANY_INSTRUCTIONS_KEY})`
        : `Ask Crewlet: company instructions changed (${COMPANY_INSTRUCTIONS_KEY})`,
    };
  };
}
