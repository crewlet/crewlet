/**
 * A phase record's tool calls (`tool_executions[]`), as the engine records
 * them.
 */

/**
 * Why no record says what a call was made with: `tool_executions[].arguments_unknown`
 * on a phase record, the engine's `toolloop.ArgsUnknown`. Its values are `unread`, a call
 * made with arguments in text that could not be read (what the model wrote did
 * not parse and the call was answered unrun, or an executor run's record of it
 * does not read back), and `unkept`, an executor run's call still in flight as
 * its round was read, whose entry kept its identity alone.
 *
 * HELD AGAINST `toolloop.ArgsUnknowns` by
 * `internal/agent/toolloop.TestTheDashboardKnowsEveryReasonArgumentsAreNotKnown`,
 * in both directions, and the words a reader is told for them
 * (`UNKNOWN_ARGUMENTS` in lib/withheld.ts) are typed over this union, so a
 * reason the engine starts recording fails the gate until it is named here and
 * fails the typecheck until it has words. A value the wire carries that this
 * build does not know is still arguments not known (`unknownArguments`),
 * because an older dashboard meets a newer engine.
 */
export type ArgumentsUnknown = "unread" | "unkept";
