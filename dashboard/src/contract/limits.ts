/**
 * Which part of the company's work a limit bounds. COPIES of
 * `queries.LimitGroups`, in the engine's drawing order, held by
 * `internal/api`'s `TestTheDashboardReadsExactlyTheLimitsTheEngineSends`.
 */
type LimitGroup = "turn" | "delegation" | "notifications" | "schedules";

/** What a limit's number counts. COPIES of `queries.LimitUnits`. */
type LimitUnit = "count" | "seconds" | "fraction" | "per_second";

/**
 * The values a cap admits, as the engine's validator holds it: `config.Range`
 * in Go, the ONE declaration of the bound both read. In the cap's own unit (a
 * share as a fraction of one, a time limit in seconds).
 */
export interface SettingRange {
  /** The least value admitted, or, with `min_exclusive`, the value every one is above. */
  min: number;
  /** `min` itself is refused: a time limit of zero expires before the work starts. */
  min_exclusive?: boolean;
  /** The greatest value admitted; absent where nothing bounds the cap from above. */
  max?: number;
}

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
  /**
   * `key` is a setting of the company document, so a write of it changes
   * `value`. False for a cap the engine holds as a constant (the schedule
   * timeout, which each schedule sets for itself): a screen offers a change
   * exactly where this is true.
   */
  settable: boolean;
  /**
   * What the validator admits for `key`, so the editor says a value outside it
   * beside the field before anything is sent. On every settable row; absent
   * on a constant.
   */
  range?: SettingRange;
}

/**
 * The `limits` answer, an administrator's: every cap in the engine's drawing
 * order. Empty on a node running no company, where no turn runs.
 */
export interface LimitsAnswer {
  limits: LimitRow[];
}
