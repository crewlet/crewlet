/**
 * The fleet broker's membership as the wire knows it: the kinds a node's broker
 * can be, the disagreements the engine names between what the nodes advertise
 * and what the metadata group counts, and how long a removal may take.
 *
 * Every constant here is a COPY of something the engine owns, because this is
 * a separate build that cannot import a Go identifier — and each copy is held
 * to the engine's by a gate on the engine side (`internal/api`'s
 * `broker_client_test.go`), in both directions, so it cannot drift silently.
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

/** One of [BROKER_KINDS]. Kept a string on the wire, so a kind a newer node
 *  sends is shown rather than dropped. */
export type BrokerKind = (typeof BROKER_KINDS)[number];

/**
 * The disagreements between the two records of the broker's membership —
 * `engine.BrokerFindingKinds`.
 */
export const BROKER_FINDING_KINDS = ["dead_member", "not_in_group", "unknown_kind"] as const;

/** One of [BROKER_FINDING_KINDS]. */
export type BrokerFindingKind = (typeof BROKER_FINDING_KINDS)[number];

/**
 * How long a removal's request may take before the dialog gives up on it.
 *
 * THREE MINUTES AND TEN SECONDS, the command line's own wait and for its
 * reason: the member carrying a removal waits for the metadata group to commit
 * it within the budget one clustered metadata change gets, the node that
 * received the request waits that and a read's round trip on top
 * (`engine.BrokerRemoveWait`, two minutes and ten seconds), and a minute more
 * covers the node listing the fleet and reaching the member. A dialog that
 * gave up first would report a removal the group went on to commit as failed.
 */
export const BROKER_REMOVE_TIMEOUT_MS = 190_000;

/** One live node's broker, as its presence advertises it. */
export interface BrokerNode {
  node: string;
  kind: BrokerKind | string;
  roles: string[] | null;
}

/** One voter of the metadata group, as the member that answered sees it. */
export interface MetaPeer {
  name: string;
  peer: string;
  self?: boolean;
  leader?: boolean;
  current: boolean;
  offline?: boolean;
  /** Nanoseconds since the answering member last heard from it. */
  active: number;
}

/** The metadata group as one member reports it. */
export interface MetaGroup {
  cluster: string;
  /** Absent while the group has no leader. */
  leader?: string;
  peers: MetaPeer[];
}

/** One disagreement, about one node. */
export interface BrokerFinding {
  kind: BrokerFindingKind | string;
  node: string;
  detail: string;
}

/**
 * `GET /fleet/broker` — the `fleet_broker` question.
 *
 * AN UNREAD GROUP IS NEVER AN EMPTY ONE: `group` is absent when no member could
 * report it, and `group_error` says why.
 */
export interface FleetBrokerAnswer {
  node: string;
  kind: BrokerKind | string;
  /** A fleet on an external cluster, whose membership is its operator's. */
  external: boolean;
  nodes: BrokerNode[] | null;
  group?: MetaGroup;
  group_from?: string;
  group_error?: string;
  findings: BrokerFinding[];
}

/** `POST /fleet/broker/remove/{node}`'s answer. */
export interface BrokerRemoved {
  node: string;
  /** The member whose system account carried it. */
  by: string;
  group?: MetaGroup;
}
