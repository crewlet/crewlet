/**
 * The estate map as the wire knows it: which data nodes hold each partition of
 * the replicated estate — `queries.FleetEstate`, the `estate` question and
 * `GET /estate` — and what a gesture on it answers, `api.EstateAnswer`.
 *
 * THE ENGINE'S OWN RENDERING IS THE ESTATE SCREEN'S FIXTURE: its suite reads
 * `internal/api/testdata/estate_answer.json`, which the Go renderers write, so
 * a field renamed on either side fails a test until the other follows it. And
 * every closed set below is a COPY of the engine's, held to it in both
 * directions by `internal/api`'s `estate_client_test.go`.
 */

import type { MapHold, MapMember, MapRemoval } from "./types.ts";

/**
 * Which of the five things the estate was when the question was asked —
 * `queries.EstateMapStates`. `whole` is layout 0, every fleet on this build:
 * every data node holds the whole estate and there is no map.
 */
export const ESTATE_MAP_STATES = [
  "unavailable",
  "whole",
  "no_map",
  "unreadable",
  "placed",
] as const;
export type EstateMapState = (typeof ESTATE_MAP_STATES)[number];

/** What the MAP says a holder is doing with a partition — `partmap.HolderStates`. */
export const HOLDER_STATES = ["joining", "serving", "leaving"] as const;
export type HolderState = (typeof HOLDER_STATES)[number];

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
export type PartitionState = (typeof PARTITION_STATES)[number];

/** What one data node's estate lease says of itself. */
export interface EstateLease {
  weight: number;
  /** The layout it runs, absent when its lease does not say. */
  layout?: number;
  /**
   * Whether its store can hold partitions — ABSENT when the lease does not
   * say, which the map counts exactly as a failed store, never as healthy.
   */
  healthy?: boolean;
  detail?: string;
  /** Whether the map counts it present and healthy. */
  able: boolean;
  /** The map epoch it last acted on, absent for none of this map's lineage. */
  map_epoch?: number;
  free_bytes: number;
  /** Held partitions whose lexical index is still building there. */
  building?: string[];
}

/** One data node holding the whole estate, at layout 0. */
export interface WholeHolder {
  node: string;
  /** Absent for a node running a build from before the estate lease. */
  lease?: EstateLease;
  /** What that lease says of the one partition. */
  reports?: PartitionState | string;
}

/** How a layout divides the estate: one space. */
export interface EstateSpace {
  space: string;
  partitions: number;
  domains: string[];
}

/** The map's last balance. `converged: false` is a measurement, never a fault. */
export interface EstateBalance {
  deviation_percent: number;
  tolerance_percent: number;
  converged: boolean;
  rounds: number;
}

/** One member as the estate question renders it. */
export interface EstateMember extends MapMember {
  share_percent: number;
  serving: number;
  joining: number;
  leaving: number;
  /** Partitions an operator moved off it. */
  moved_off?: string[];
  live: boolean;
  lease?: EstateLease;
}

/** One node's place in a partition's holder table. */
export interface EstateHolder {
  node: string;
  state: HolderState | string;
  /** The map epoch it entered that state at. */
  since: number;
  /** What its own lease says of the partition, absent where it says nothing. */
  reports?: PartitionState | string;
  /** Whether the map counts its node present and healthy. */
  able: boolean;
}

/** An operator's move of a partition off a node. */
export interface EstateMove {
  node: string;
  by: string;
  reason?: string;
  at: string;
  /**
   * On the map but NOT IN EFFECT: without the node, the members left could not
   * hold the partition's copies, so its target names the node again until a
   * member returns — a move moves a copy and never drops one.
   */
  waiting?: boolean;
}

/** One partition. */
export interface EstatePartition {
  id: string;
  space: string;
  /** Where it should be held, primary first. */
  target: string[];
  /** Copies that can answer for it now, against the copies its target has. */
  serving: number;
  wanted: number;
  holders: EstateHolder[];
  moves?: EstateMove[];
}

/** Layout 0: the one partition, and the data nodes that each hold it whole. */
export interface WholeEstate {
  state: "whole";
  layout: number;
  /** The sentence every surface gives layout 0. */
  detail: string;
  partition: string;
  holders: WholeHolder[];
}

/** A placed map, every field present. */
export interface PlacedEstate {
  state: "placed";
  layout: number;
  /** The map's lineage — what a hold and a release are confirmed by. */
  generation: string;
  epoch: number;
  spaces: EstateSpace[];
  replicas: number;
  copies: number;
  failure_domain?: string;
  distinct_domains: number;
  domain_limited: boolean;
  hold?: MapHold;
  balance?: EstateBalance;
  unserved: number;
  short: number;
  joining: number;
  leaving: number;
  moves: number;
  members: EstateMember[];
  removed: MapRemoval[];
  partitions: EstatePartition[];
}

/** The three states with nothing to show but the sentence that says them. */
export interface UnplacedEstate {
  state: "unavailable" | "no_map" | "unreadable";
  layout: number;
  detail: string;
}

/** The estate question's answer. */
export type FleetEstate = WholeEstate | PlacedEstate | UnplacedEstate;

/** What a gesture on the estate map answered. */
export interface EstateGestureAnswer {
  /** False only when every attempt lost its race: nothing asked is in the map. */
  landed: boolean;
  /** No gesture moves it: the epoch counts the holder table. */
  epoch: number;
  generation: string;
  node?: string;
  member?: MapMember;
  partition?: string;
  /** The move of `partition` off `node` in force now, absent after a cancel. */
  move?: EstateMove;
  /** Where `partition`'s copies should now be. */
  target?: string[];
  hold?: MapHold;
  hint?: string;
}
