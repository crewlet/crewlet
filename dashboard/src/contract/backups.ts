/**
 * `backups`: what this fleet has backed up — each owner's newest point, as the
 * fleet's register holds it and marked by the policy the trim takes — and
 * every backup a person asked a node for, from the runtime audit each node
 * keeps, failures included.
 *
 * EXACTLY WHAT `internal/api/queries` SENDS — held there by
 * `TestTheBackupsScreenReadsWhatTheAnswerSends` in both directions, the two
 * unions included.
 */

import type { Coverage } from "./coverage.ts";

/**
 * Who wrote a point. `node`: a node's own copy, announced when its manifest was
 * written — the owner is the node id. `operator`: `crewlet retention ack`, a
 * person's assertion that a copy has left the host, which the engine cannot see
 * for itself.
 */
export type BackupOwnerKind = "node" | "operator";

/**
 * What a requested backup came to. `applied`: its manifest was written.
 * `failed`: it was not — the directory holds debris, not a backup.
 */
export type BackupOutcome = "applied" | "failed";

/**
 * Whose word the trim takes for what is backed up
 * (`stream.tracker_retention.backup_floor`): every node's verified copy
 * (`engine`), or the operator's acknowledgement alone (`operator`).
 */
export type BackupPolicy = "engine" | "operator";

/** One stream's reach in a backup. */
export interface BackupCover {
  stream: string;
  generation: number;
  seq: number;
}

/** One owner's newest backup — its manifest's claims. */
export interface BackupPointRow {
  owner: string;
  kind: BackupOwnerKind;
  /** When the copy STARTED: nothing in it is older. */
  taken_at: string;
  /** Where it was written, on the owner's host. */
  dir?: string;
  /** The taker checked the copy it wrote; an unverified point counts for no
   *  policy. */
  verified: boolean;
  /** The whole artefact. ABSENT on an acknowledgement, which asserts a copy the
   *  engine never saw — never zero. */
  bytes?: number;
  covers: BackupCover[];
  /** The trim may count it: the policy takes this owner's word and it was
   *  verified. */
  counted: boolean;
  /** The point the trim's backup term reads — the newest counted one. */
  newest: boolean;
}

/** One backup a person asked a node for. */
export interface BackupRunRow {
  id: string;
  at: string;
  /** The node whose disk holds the copy. */
  node?: string;
  /**
   * Who asked, as every record names its author (`iam.ActorFor`): the seat a
   * person bound to one asks as (kind `human`), or the whole login of a
   * credential bound to none (kind `operator`).
   */
  actor: string;
  /** Which of the engine's author kinds `actor` is — `human` or `operator`. */
  actor_kind?: string;
  /**
   * The credential the request came through, where it names something the
   * actor does not — a person's machine token or browser session.
   */
  operator_id?: string;
  dir: string;
  outcome: BackupOutcome;
  summary: string;
}

export interface BackupsAnswer {
  policy: BackupPolicy;
  /** Newest first. */
  points: BackupPointRow[];
  /** Newest first, over the event-retention window. */
  history: BackupRunRow[];
  /** The history filled its page, so older backups exist than those listed. */
  more: boolean;
  /** Which nodes the history was read from. */
  coverage: Coverage;
}
