/**
 * Which part of the company's work a limit bounds. COPIES of
 * `queries.LimitGroups`, in the engine's drawing order, held by
 * `internal/api`'s `TestTheDashboardReadsExactlyTheLimitsTheEngineSends`.
 */
type LimitGroup = "turn" | "delegation" | "notifications" | "schedules";

/** What a limit's number counts. COPIES of `queries.LimitUnits`. */
type LimitUnit = "count" | "seconds" | "fraction" | "per_second";

/**
 * One cap the engine holds the company's work to: `queries.LimitRow` in Go.
 * `value` is what the engine ENFORCES, defaults applied, so a screen never
 * keeps its own copy of a default. The applied company does not remember
 * whether a value was written, so there is no "set" flag: what is true to
 * say is whether `value` differs from `default`.
 */
interface LimitRow {
  /** The setting's path in the company document, e.g. `turn_engine.max_iterations`. */
  key: string;
  group: LimitGroup;
  unit: LimitUnit;
  value: number;
  default: number;
  /**
   * The value switches the limit off rather than bounding anything:
   * `notification_rate_limit` 0 is no rate limit, and
   * `turn_engine.onboarding_max_tool_rounds` 0 is no onboarding pass.
   */
  off: boolean;
  /**
   * The same question asked of `default`: a screen that says which default
   * a set value replaced says Off where the default is no limit, without its
   * own copy of which zeros switch a limit off.
   */
  default_off: boolean;
}

/**
 * The `limits` answer, an administrator's: every cap in the engine's drawing
 * order. Empty on a node running no company, where no turn runs.
 */
export interface LimitsAnswer {
  limits: LimitRow[];
}
