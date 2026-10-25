/**
 * Why a model stopped writing one round's response — `rounds[].stop_reason` on
 * a phase record, the engine's `llm.StopReason`.
 *
 * HELD AGAINST `llm.StopReasons` by
 * `internal/providers/llm.TestTheDashboardKnowsEveryStopReason`, in both
 * directions.
 *
 * THREE REASONS, the ones a coding CLI can honestly name. `end` and `tool_use`
 * are a round that FINISHED — the model asked for an answer or for a set of
 * calls — so the phase card says nothing about either. `cap` is the one that
 * ends a round unfinished: the CLI's own turn cap stopped its run at this
 * request with the model still working, and the phase card's round says so
 * (`CapNote` in components/PhaseCard.tsx). A reason the engine starts
 * recording needs the same decision — a sentence where it ends a round
 * unfinished — which is why the gate fails until this union names it. A run
 * the ENGINE ended is no model's reason and is not here: it is the run's own
 * outcome (`invocations[].outcome`). An ABSENT `stop_reason` is a backend that
 * reported none — not reported, never "ended normally" as a fact.
 */
export type StopReason = "end" | "tool_use" | "cap";
