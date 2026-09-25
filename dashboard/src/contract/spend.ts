/**
 * Which dimension the Spend screen's time axis is split on.
 *
 * The engine's own closed set minus nothing: `token_series` refuses a `group`
 * it does not know, naming what it accepts, so this list and `tokens.Groups`
 * have to agree — and `internal/api/queries`' cost-dimension gate holds them
 * against each other in both directions. There is no `turn`: the series is
 * read from company days, which hold none, and per-turn spend is the turn
 * list sorted by tokens.
 */
export const GROUPS = [
  { value: "phase", label: "Phase" },
  { value: "model", label: "Model" },
  { value: "provider", label: "Provider" },
  { value: "seat", label: "Seat" },
  { value: "unit", label: "Unit" },
  { value: "worker", label: "Worker" },
] as const;

/**
 * The four bands a phase breakdown is drawn in, and the data series each one
 * takes — the engine folds every phase into one of them ONCE
 * (`tokens.PhaseBand`: execute and a coding run are Execute, review is Review,
 * delegated workers are Workers, the learning workers, the judge and
 * onboarding are Auxiliary), so no screen folds a phase itself.
 *
 * `series` is the 1-based data hue, in stacking order; `internal/api/queries`'
 * band gate holds `value` against `tokens.Bands` in both directions.
 */
export const BANDS = [
  { value: "execute", label: "Execute", series: 1 },
  { value: "review", label: "Review", series: 2 },
  { value: "workers", label: "Workers", series: 3 },
  { value: "auxiliary", label: "Auxiliary", series: 4 },
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
