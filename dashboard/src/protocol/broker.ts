/**
 * The fleet broker's membership as the wire knows it: the kinds a node's broker
 * can be, the disagreements the engine names between what the nodes advertise
 * and what the metadata group counts, and how long a removal may take.
 *
 * The closed sets and the removal's wait are declared in `../contract/fleet.ts`,
 * the one home of every value an engine gate holds; this module re-exports
 * them beside the wire shapes that use them.
 */

import { BROKER_FINDING_KINDS, BROKER_KINDS, BROKER_REMOVE_TIMEOUT_MS } from "../contract/fleet.ts";

export { BROKER_FINDING_KINDS, BROKER_KINDS, BROKER_REMOVE_TIMEOUT_MS };

/** One of [BROKER_KINDS]. Kept a string on the wire, so a kind a newer node
 *  sends is shown rather than dropped. */
export type BrokerKind = (typeof BROKER_KINDS)[number];

/** One of [BROKER_FINDING_KINDS]. */
export type BrokerFindingKind = (typeof BROKER_FINDING_KINDS)[number];

/** One live node's broker, as its presence advertises it. */
export interface BrokerNode {
  node: string;
  /**
   * The raft peer id the metadata group counts this node by when it is a
   * voter — what a voter is matched to its node by, since a voter's name is
   * only what the answering member has heard.
   */
  peer: string;
  kind: BrokerKind | string;
  roles: string[] | null;
}

/** One voter of the metadata group, as the member that answered sees it. */
export interface MetaPeer {
  /**
   * The server's name — the node id — and EMPTY for a voter the answering
   * member has not heard from since it started, which `peer` still names.
   */
  name: string;
  /** The raft peer id the group counts the voter by. */
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
  /** The node id, empty for a voter nobody can name. */
  node: string;
  /** The voter's peer id, on a finding about a voter. */
  peer?: string;
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

/** `POST /fleet/broker/remove/{node}`'s and `/remove-peer/{peer}`'s answer. */
export interface BrokerRemoved {
  /** The removed voter's node id, empty where only its peer id was known. */
  node: string;
  peer: string;
  /** The member whose system account carried it. */
  by: string;
  group?: MetaGroup;
}
