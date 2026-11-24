/**
 * LLM fleets, typed: the client half of `GET /llm/fleets` (the `llm_fleets`
 * query) and of the `llm-fleets` entities `PUT /config/llm-fleets/{key}`
 * writes. A fleet groups model entries of one class; a seat job or helper set
 * to `fleet:<key>` runs on whichever member has room. RELATIVE imports, as in
 * every protocol file.
 */

import type { ModelJob } from "../contract/models.ts";

/** How a fleet spreads new work: by usage share, or the first member until it nears its limit. */
export type FleetStrategy = "balanced" | "sequential";

/** How a member signs in: a plan (`auth.mode: subscription`), an API key, a cloud account or a gateway. */
export type FleetMemberKind = "plan" | "api" | "cloud" | "gateway";

/** Whether a member takes new work: only `available` does; `near` finishes what it runs. */
export type FleetMemberState = "available" | "near" | "limit" | "signedout";

export interface FleetMember {
  entry: string;
  kind: FleetMemberKind;
  state: FleetMemberState;
  /** When a member at its limit has room again. */
  until: string | null;
  /** Why it takes no new work, in a person's words; empty while it does. */
  why: string;
  /** Its usage share in whole points, 0 to 100; null where nothing is known yet. */
  share: number | null;
  /** A key, cloud account or gateway a Balanced fleet keeps for when every plan is out. */
  overflow: boolean;
  last_served_at: string | null;
  /**
   * The seats and helpers with work in flight on it now, by handle (`assistant`,
   * `advisor` for the helpers): never a prediction, so empty while nothing runs.
   */
  serving: string[];
}

export interface Fleet {
  key: string;
  name: string;
  /** `default-fleet`, where new seats and helpers start: never deleted or renamed. */
  is_default: boolean;
  /** The model class every member runs ("Claude Opus"); empty while it has no member. */
  class: string;
  harness: string;
  strategy: FleetStrategy;
  threshold_points: number;
  /** Points between the most and least used members taking new work; null where none balances. */
  spread: number | null;
  /** In list order, which is a Sequential fleet's order. */
  members: FleetMember[];
}

/** What a seat job or helper is set to: a fleet, or one pinned entry (`""` for unset). */
export type FleetTarget = { fleet: string } | { pinned: string };

/** What `now` is: the member its work in flight runs on, or where its next piece of work goes. */
export type FleetNowKind = "running" | "next";

/** A seat job as the engine resolves it (`config.ProviderJob`), helpers excluded. */
export type FleetJob = Exclude<ModelJob, "assistant" | "advisor">;

export interface FleetJobAssignment {
  job: FleetJob;
  target: FleetTarget;
  /** The entry its work runs on now, or where its next piece goes; null while it waits. */
  now: string | null;
  /** Which of the two `now` is; absent while it waits. */
  now_kind?: FleetNowKind;
  waiting_until: string | null;
}

export interface FleetAssignment {
  /** A seat's handle, or `assistant` / `advisor`. */
  subject: string;
  kind: "seat" | "helper";
  name: string;
  /** A helper's target; a seat's executor's, its every job in `jobs`. */
  target: FleetTarget;
  now: string | null;
  now_kind?: FleetNowKind;
  waiting_until: string | null;
  jobs?: FleetJobAssignment[];
}

/** `llm_fleets`: every fleet with its members' readings, and what each seat and helper runs on. */
export interface FleetsAnswer {
  reading: "admin" | "member";
  fleets: Fleet[];
  assignments: FleetAssignment[];
}

/** The `llm-fleets` entity, as `PUT /config/llm-fleets/{key}` takes it. */
export interface LLMFleet {
  name: string;
  strategy: FleetStrategy;
  /** Entry keys; the order is a Sequential fleet's. */
  members: string[];
  threshold_points?: number;
}
