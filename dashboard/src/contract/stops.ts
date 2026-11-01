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

/**
 * How one CLI run of a phase ended — `invocations[].outcome` on a phase record,
 * the engine's `toolloop.InvocationOutcome`.
 *
 * HELD AGAINST `toolloop.InvocationOutcomes` by
 * `internal/agent/toolloop.TestTheDashboardKnowsEveryInvocationOutcome`, in both
 * directions, and the phase card's words for them (`OUTCOME_WORDS` in
 * lib/phases.ts) are typed over this union, so an outcome the engine starts
 * recording fails the gate until it is named here and fails the typecheck
 * until it has words. A value the wire carries that this build does not know
 * is still read as itself (`outcomeWords`), because an older dashboard meets a
 * newer engine.
 */
export type InvocationOutcome = "end" | "cap" | "stopped" | "failed";
