/**
 * Why a model stopped writing one round's response — `rounds[].stop_reason` on
 * a phase record, the engine's `llm.StopReason`.
 *
 * HELD AGAINST `llm.StopReasons` by
 * `internal/providers/llm.TestTheDashboardKnowsEveryStopReason`, in both
 * directions.
 *
 * TWO REASONS, because a coding CLI reports two: it is asked for an answer or
 * a set of calls, and both are a round that FINISHED, so the phase card says
 * nothing about either. A CLI that hit its own cap or declined the request
 * returns that as prose, or as a failure the phase's error carries. A reason
 * the engine starts recording that ends a round unfinished needs a sentence
 * on the phase card's round, which is why the gate fails until this union
 * names it. An ABSENT `stop_reason` is a backend that reported none — not
 * reported, never "ended normally" as a fact.
 */
export type StopReason = "end" | "tool_use";
