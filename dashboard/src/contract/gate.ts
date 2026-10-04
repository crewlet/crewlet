/**
 * The node gate — evicting a node and readmitting one — as the engine bounds
 * it: the remedy vocabulary a gate answer speaks, which of those remedies keep
 * the gesture's own operation id, and how long a gesture may take to answer.
 *
 * Each is a COPY of something the engine owns, held to it in both directions
 * by `internal/api`'s `gate_client_test.go`. What is done with them — whether
 * an action keeps the id, the operation id's grammar — is behaviour, and lives
 * in `protocol/gate.ts`.
 */

/**
 * What an operator does about a log a gesture did not finish, or a gesture
 * refused before anything was written — `statelog.GateActions`, held by
 * `internal/api.TestTheDashboardKnowsEveryGateAction`.
 *
 * AN ACTION, NEVER A FLAG. The engine used to send one sentence in the command
 * line's words ("evict it with -force", "without -op-id"), and this dashboard
 * rendered it beside a dialog that has none of those flags. The engine now
 * says WHAT to do and each surface says HOW: here, as its own controls.
 */
export const GATE_ACTIONS = [
  "retry_same_op",
  "new_gesture",
  "force",
  "other_node",
  "reanchor",
  "set_capacity",
  "restore",
  "wait",
] as const;

/**
 * The actions after which the gesture is finished under ITS OWN operation id —
 * `statelog.GateActionsKeepingOperation`, held by
 * `internal/api.TestTheDashboardKeepsTheGesturesOwnIDWhereTheEngineDoes`.
 *
 * A log refused `log_full` cannot be finished by sending the gesture again at
 * once, and a surface that let go of the id then left the operator nothing
 * but a FRESH gesture once the ceiling was raised — which writes every log
 * that already held the first record again, re-dating each eviction and
 * restarting its fence window. So the id is kept for all of these, and only
 * `new_gesture` lets it go.
 */
export const GATE_ACTIONS_KEEPING_OPERATION = [
  "retry_same_op",
  "other_node",
  "reanchor",
  "set_capacity",
] as const;

/**
 * How long one gesture's request may take before the dialog gives up on it,
 * held above `engine.GateAnswerBudget` by
 * `internal/api.TestTheDashboardWaitsPastTheGateBudget`.
 *
 * TWO MINUTES, the command line's own `gateRequestTimeout` and for its
 * reason: the engine answers one gesture within `engine.GateAnswerBudget` — a
 * minute and a half: half a minute to judge it and a minute to write every
 * log — and the rest is the request's round trip. Waiting past the node's own bound is what makes its
 * answer — every log's outcome — reach the operator rather than a client
 * timeout that knows none of it. The default thirty seconds gave up on a
 * gesture the node went on to finish.
 */
export const GATE_REQUEST_TIMEOUT_MS = 120_000;
