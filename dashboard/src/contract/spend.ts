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
