/**
 * Which dimension the Spend screen's time axis is split on.
 *
 * The engine's own closed set minus nothing: `token_series` refuses a `group`
 * it does not know, naming what it accepts, so this list and `tokens.Groups`
 * have to agree — and `internal/api/queries`' cost-dimension gate holds them
 * against each other in both directions.
 */
export const GROUPS = [
  { value: "phase", label: "Phase" },
  { value: "model", label: "Model" },
  { value: "seat", label: "Seat" },
  { value: "unit", label: "Unit" },
  { value: "worker", label: "Worker" },
  { value: "turn", label: "Turn" },
] as const;

/**
 * What a capped budget window is doing, as the ENGINE judges it — the
 * `state` on every window of the `budget` push and the `budgets` answer.
 *
 * THE ENGINE'S THREE, held against `types.BudgetState`'s constants by
 * `internal/events/types`' budget-state gate. There is no threshold on this
 * side: a screen that wants to say "nearly spent" reads `near` rather than
 * dividing, and the one fraction it may draw as a mark is the answer's own
 * `near_fraction`. The screens held three of their own before this (75%, 90%
 * and the kit's), so one window read as healthy, near and full at once.
 */
export type BudgetState = "ok" | "near" | "refusing";
