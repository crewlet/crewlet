/**
 * The fleet's broker and object store, as the engine bounds them: the kinds
 * a node's broker can be and the disagreements named between the two records
 * of its membership, how long a removal may take, and the states of the object
 * store's report.
 *
 * Each is a COPY of something the engine owns, because this is a separate
 * build that cannot import a Go identifier — held to the engine's in both
 * directions by `internal/api`'s `broker_client_test.go` and
 * `objects_test.go`. What is done with them is behaviour, and lives in
 * `protocol/broker.ts` and the Settings screens.
 */

/**
 * How a node's broker takes part in the fleet's — `placement.BrokerKind`, as
 * `String()` renders it.
 *
 * `unknown` IS A VALUE, not an absence: a node running a build older than the
 * field says nothing, and the engine renders that as `unknown` so the cell is
 * never empty — an empty cell reads as nothing to look at, and this one is
 * counted as a member wherever that is the safe reading.
 */
export const BROKER_KINDS = ["member", "leaf", "client", "unknown"] as const;

/**
 * The disagreements between the two records of the broker's membership —
 * `engine.BrokerFindingKinds`.
 */
export const BROKER_FINDING_KINDS = ["dead_member", "not_in_group", "unknown_kind"] as const;

/**
 * How long a removal's request may take before the dialog gives up on it.
 *
 * THREE MINUTES AND FIVE SECONDS, the command line's own wait and for its
 * reason: the node bounds the whole removal by one deadline
 * (`engine.BrokerRemoveWait`, two minutes and five seconds) — the carrying
 * member's own commit budget and one round trip for its answer — and a minute
 * more covers the node listing the fleet and reaching the member. A dialog
 * that gave up first would report a removal the group went on to commit as
 * failed.
 */
export const BROKER_REMOVE_TIMEOUT_MS = 185_000;

/**
 * Which of the three things the object collector's record was when the fleet
 * view read it — `queries.ObjectsStates`. `not_yet` is a fleet whose collector
 * has not finished a pass; `unavailable` a record the coordination store
 * would not give up.
 */
export const OBJECTS_STATES = ["unavailable", "not_yet", "reported"] as const;
