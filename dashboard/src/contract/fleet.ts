/**
 * The fleet's own maps and broker, as the engine bounds them: the kinds a
 * node's broker can be and the disagreements named between the two records of
 * its membership, the states of the estate map and of a partition's holders,
 * how long a removal may take, and the lengths a hold is offered at.
 *
 * Each is a COPY of something the engine owns, because this is a separate
 * build that cannot import a Go identifier — held to the engine's in both
 * directions by `internal/api`'s `broker_client_test.go`,
 * `estate_client_test.go` and `objects_test.go`. What is done with them is
 * behaviour, and lives in `protocol/broker.ts`, `protocol/estate.ts` and the
 * Settings screens.
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
 * Which of the five things the estate was when the question was asked —
 * `queries.EstateMapStates`. `whole` is layout 0: every data node holds the
 * whole estate and there is no map.
 */
export const ESTATE_MAP_STATES = [
  "unavailable",
  "whole",
  "no_map",
  "unreadable",
  "placed",
] as const;

/** What the MAP says a holder is doing with a partition — `partmap.HolderStates`. */
export const HOLDER_STATES = ["joining", "serving", "leaving"] as const;

/**
 * What a node's own estate lease says it is doing with a partition —
 * `partmap.PartitionStates`. Kept a string on the wire, so a state a newer
 * node reports is shown rather than dropped.
 */
export const PARTITION_STATES = [
  "adopting",
  "catching_up",
  "serving",
  "faulted",
  "draining",
  "released",
] as const;

/**
 * The lengths a hold on either placement map is offered at, the longest being
 * the engine's own ceiling (`membership.MaxHold`): a hold nobody releases must
 * still end, and a longer choice would be refused `invalid_hold`.
 */
export const HOLD_LENGTHS = ["30m", "1h", "2h", "4h", "8h", "24h"] as const;
