/**
 * The configurable limits a refusal names as where to change it, as the engine
 * names them (`internal/caveat`'s `Setting`, ADR-0060).
 *
 * A refusal a limit caused (a token budget spent) carries a `remedy` naming
 * one of these, and `lib/remedy.ts` resolves each to the screen and the section
 * that edit it. HELD AGAINST `caveat.Settings` by
 * `internal/caveat.TestTheDashboardKnowsEverySettingARemedyNames`: a setting
 * added there and not here would be a refusal with nothing to open.
 */
export const LIMIT_SETTINGS = [
  "company_token_budget",
  "seat_token_budget",
  "assistant_token_budget",
  "model_providers",
] as const;

/** Where a person changes the limit a refusal was caused by. */
export interface Remedy {
  setting: (typeof LIMIT_SETTINGS)[number];
  /** The seat whose own ceilings a `seat_token_budget` remedy names. */
  seat?: string;
}
