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
 * Which of those bands each PHASE value is drawn in — `tokens.PhaseBand`,
 * written out by phase, for the charts that list phases rather than bands (the
 * live window's by-phase answer keeps every phase it saw).
 *
 * A PHASE IS A CATEGORY, AND A CATEGORY HAS NO COLOUR OF ITS OWN: the design
 * system removed its phase family, and inside a figure a phase is a series.
 * Taking the series of the band the engine already folds it into is what keeps
 * one phase one colour on every chart — a coding run is Execute on the Spend
 * chart and in the live list alike. A phase this build has not met (a newer
 * peer's) is Auxiliary, which is exactly where the engine puts it.
 *
 * `internal/api/queries`' phase-band gate holds every entry against
 * `tokens.PhaseBand` and requires every phase in `phase.All`.
 */
export const PHASE_BANDS = {
  execute: "execute",
  sandbox: "execute",
  plan: "execute",
  review: "review",
  subagent: "workers",
  auxiliary: "auxiliary",
  judge: "auxiliary",
  onboarding: "auxiliary",
} as const;

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
