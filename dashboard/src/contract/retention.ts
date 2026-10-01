/**
 * The retention report's closed sets — the states a report row names where a
 * number alone would read as a position. Each is `statelog`'s, held to it in
 * both directions by
 * `internal/statelog.TestTheDashboardKnowsEveryRetentionReportState`: a state
 * the engine sends and this union does not name is drawn as nothing, and one
 * named here that the engine never sends is a branch nothing can reach.
 */

/**
 * What a domain's floor is — `statelog.TrimFloorStates`. `none_at_generation`
 * is the stretch after a reanchor before the trim's first tick on the adopted
 * stream, when there is no floor to print and a zero would read as one.
 */
export type RetentionTrimFloorState = "published" | "none_at_generation" | "unreadable";

/**
 * Which finding is behind a `wrong_stream` — `statelog.IdentityCauses`. One
 * word refuses for four facts, each with its own remedy.
 */
export type RetentionIdentityCause =
  "recreated" | "ahead_of_log" | "log_diverged" | "generation_passed";

/**
 * A node's position generation against its domain's —
 * `statelog.GenerationStates`. Only `current` compares with the log's
 * sequences: a position from a generation the log has left is a number in a
 * space that no longer exists.
 */
export type RetentionGenerationState = "current" | "left" | "ahead" | "unknown";
