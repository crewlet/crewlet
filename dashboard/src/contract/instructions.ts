/**
 * Ask Crewlet's instructions, one layer of them: the company's
 * (`assistant.instructions`) or a person's own (`save_assistant_instructions`),
 * each member optional. Held to `config.AssistantInstructions` by
 * `internal/config`'s `TestTheDashboardHoldsInstructionsToTheEnginesRules`.
 */

/** How long answers run, shortest first. */
export type AnswerLength = "brief" | "balanced" | "detailed";

/** How answers are laid out. */
export type AnswerFormat = "lead" | "lists" | "tldr";

/** The most characters (code points) a layer's text may hold. */
export const ASSISTANT_INSTRUCTIONS_MAX_RUNES = 1000;

/** One layer, as the company document and the person's own record keep it. */
export interface AssistantInstructions {
  length?: AnswerLength;
  format?: AnswerFormat;
  text?: string;
}
