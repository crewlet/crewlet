/**
 * Why a model stopped writing one round's response — `rounds[].stop_reason` on
 * a phase record, the engine's `llm.StopReason`, normalised across vendors.
 *
 * HELD AGAINST `llm.StopReasons` by
 * `internal/providers/llm.TestTheDashboardKnowsEveryStopReason`, in both
 * directions: the phase card's sentence for a round that did not finish is
 * keyed on this union, so a reason the engine records and this does not name
 * would reach the screen as a round with nothing said about why it ended.
 *
 * `end` and `tool_use` are a round that finished; the other four end the
 * phase. An ABSENT `stop_reason` is a backend that reported none — not
 * reported, never "ended normally" as a fact.
 */
export type StopReason =
  "end" | "tool_use" | "max_tokens" | "refusal" | "context_exceeded" | "paused";
