/**
 * How a model call failed: the classifier's kind on a seat's `last_error`, on a
 * phase's `error_kind` and on a live call's `error.kind`, the engine's
 * `llm.ErrorKind` as its `String()` spells it.
 *
 * HELD AGAINST `llm.ErrorKind` by
 * `internal/providers/llm.TestTheDashboardKnowsEveryErrorKind`, in both
 * directions, and the plain sentences a screen says for each
 * (`FAILURE_WORDS` in lib/seats.ts) are typed over this union, so a kind the
 * engine starts recording fails the gate until it is named here and fails the
 * typecheck until it has words. The wire field stays an open string: a failure
 * recorded by something other than the classifier (a guard breach, a budget)
 * carries its own word, and a screen says a generic sentence for it rather
 * than the machine code.
 */
export type ProviderErrorKind = "fatal" | "rate_limit" | "auth" | "timeout" | "server" | "canceled";
