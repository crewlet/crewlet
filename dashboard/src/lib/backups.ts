/**
 * Taking a backup from the dashboard, and the words the Backups section draws
 * the fleet's record in.
 *
 * # What a backup from here is
 *
 * `POST /backup?dir=` on the node serving this page, which copies that node's
 * whole durable state — both databases and every stream, the company's sealed
 * credentials included — into a directory ON THAT NODE'S HOST. Nothing is
 * downloaded; `crewlet backup` is a client of the same route. The engine audits
 * it (`backup_requested`), so the history below the button is the record of
 * every backup a person asked for, this one included.
 *
 * Pure values over the wire types, so the rules are tested without a screen.
 */

import type {
  BackupCover,
  BackupOutcome,
  BackupOwnerKind,
  BackupPointRow,
  BackupPolicy,
} from "~/contract/backups.ts";
import { RestError } from "~/protocol/rest.ts";
import { sentence } from "./format.ts";
import { authorityWords } from "./refusal.ts";

/**
 * How long the dashboard waits for a copy, in milliseconds.
 *
 * TWELVE HOURS, the `crewlet backup` CLI's own wait and for its reason: the
 * route is synchronous and a copy is bounded by the size of the store, the
 * stream estate and — on `s3` — the company's files, and a FIRST copy of a
 * large company's files is hours over a modest link. The cost of too short a
 * wait is the confusing one, a screen calling a backup failed while the engine
 * finishes writing a perfectly good one. A ceiling, not an expectation.
 */
export const BACKUP_WAIT_MS = 12 * 60 * 60_000;

/** Who wrote a point, as the owner column says it. */
export const OWNER_KIND_WORDS: Record<BackupOwnerKind, string> = {
  node: "Node copy",
  operator: "Operator acknowledgement",
};

/** What a requested backup came to. */
export const OUTCOME_WORDS: Record<BackupOutcome, string> = {
  applied: "Written",
  failed: "Failed",
};

/** Whose word the trim takes, in one sentence. */
export const POLICY_WORDS: Record<BackupPolicy, string> = {
  engine: "Each log's trim counts the newest verified copy of it any node wrote.",
  operator: "Each log counts only copies of it an operator confirmed off the host.",
};

/**
 * The oldest of the points some log's trim reads, and how many there are — or
 * undefined while no log's trim reads any.
 *
 * THE OLDEST, because each log is trimmed against the newest counted backup that
 * covers it: under `operator` there is one such point per log acknowledged, and
 * the one that matters is the stalest — the log the backup-age alarm names
 * first. Picking the freshest hid a log days past its acknowledgement behind
 * another acknowledged a minute ago.
 */
export function oldestRead(
  points: BackupPointRow[],
): { point: BackupPointRow; of: number } | undefined {
  const read = points.filter((p) => p.newest);
  if (read.length === 0) return undefined;
  // BY INSTANT, never by the string: the engine writes fractional seconds only
  // where there are some, and "…:00.5Z" sorts before "…:00Z" as text.
  const point = read.reduce((a, p) => (Date.parse(p.taken_at) < Date.parse(a.taken_at) ? p : a));
  return { point, of: read.length };
}

/** One stream's reach, as a line: `CREWLET_TRACKER_LOG @900 (generation 2)`. */
export function coverWords(cover: BackupCover): string {
  return `${cover.stream} @${cover.seq} (generation ${cover.generation})`;
}

/**
 * Whether a directory can be asked for at all: absolute, on the engine host's
 * POSIX rules — the engine ships for linux and darwin only, and a relative path
 * would be resolved against a working directory nobody at this browser can
 * see. The engine refuses one too; this says so before the round trip.
 */
export function isAbsoluteDir(dir: string): boolean {
  return dir.trim().startsWith("/");
}

/**
 * A directory to offer, beside the newest copy THIS node wrote: its parent,
 * with a fresh UTC stamp as the leaf — so the second backup lands next to the
 * first. Empty when this node has never backed up, because inventing a
 * location on somebody's host would be choosing where their credentials go —
 * and empty too when that copy sat directly under `/`, for the same reason: a
 * new directory at the filesystem root is not "beside" anything an operator
 * chose.
 *
 * THE ENGINE REFUSES A DIRECTORY THAT IS NOT EMPTY, so the offer must never be
 * one a backup already went to. The stamp is to the SECOND, and a leaf that
 * still names a directory this node was asked for (`used` — its register
 * point and every history row, a failed request's debris included) takes a
 * `-2`, `-3` … suffix: stamped to the minute, reopening the dialog in the
 * minute of the last copy offered that copy's own directory.
 */
export function suggestDir(
  points: readonly BackupPointRow[],
  node: string | undefined,
  now: Date,
  used: Iterable<string> = [],
): string {
  const mine = points.find((p) => p.kind === "node" && p.owner === node && p.dir);
  const dir = trimSlashes(mine?.dir ?? "");
  const cut = dir.lastIndexOf("/");
  if (cut <= 0) return "";
  const parent = dir.slice(0, cut);
  const taken = new Set<string>([dir]);
  for (const d of used) taken.add(trimSlashes(d));
  const base = `${parent}/crewlet-${stamp(now)}`;
  let offer = base;
  for (let n = 2; taken.has(offer); n++) offer = `${base}-${n}`;
  return offer;
}

function trimSlashes(dir: string): string {
  return dir.replace(/\/+$/, "");
}

/** `20260930-120405`: sortable, and legal in every filesystem the engine runs on. */
function stamp(now: Date): string {
  const two = (n: number) => String(n).padStart(2, "0");
  return (
    `${now.getUTCFullYear()}${two(now.getUTCMonth() + 1)}${two(now.getUTCDate())}` +
    `-${two(now.getUTCHours())}${two(now.getUTCMinutes())}${two(now.getUTCSeconds())}`
  );
}

/**
 * What `POST /backup` answers when it wrote a backup — the fields this page
 * reports. A shape of the dashboard's own rather than the engine's manifest,
 * for the reason the CLI decodes into its own: a newer node's manifest must not
 * fail an older page over a field it does not print.
 */
export interface TakenBackup {
  taken_at: string;
  finished_at: string;
  node_id: string;
  stores?: { bytes: number }[];
  streams?: { bytes: number }[];
}

/** The whole artefact's size: every database copy and every stream snapshot. */
export function takenBytes(manifest: TakenBackup): number {
  const sum = (rows: { bytes: number }[] | undefined) =>
    (rows ?? []).reduce((total, row) => total + (row.bytes ?? 0), 0);
  return sum(manifest.stores) + sum(manifest.streams);
}

/**
 * The refusals `POST /backup` makes over the fleet's own state, each with the
 * sentence said when an engine sends no hint — its hint, as a sentence of its
 * own, is said when it does.
 */
const FLEET_REFUSALS = new Map<string, string>([
  ["objects_unreachable", "The object store did not answer for the company's files."],
  [
    "record_unreadable",
    "A log this backup replays holds a record a newer build wrote; take the backup from a node running that build.",
  ],
  [
    "record_unverified",
    "A log this backup replays holds a record signed under a key this node does not hold; add the key to its secrets.keys and restart it, or take the backup from a node that holds it.",
  ],
  [
    "log_recreated",
    "This node's copy is keyed to a log that has since been recreated; take the backup once the node has re-keyed to it, or from a node that has.",
  ],
  [
    "log_gap",
    "A log a restore would replay no longer holds every record the copy needs; take the backup again.",
  ],
]);

/**
 * Why a backup was not taken, and whether it is the directory's fault.
 *
 * THE ENGINE NAMES ONLY THE CALLER'S OWN MISTAKE (a 400 carries the detail —
 * a relative path, a directory that is not empty) and keeps every other reason
 * in its log, so this says where to look rather than inventing one.
 */
export function backupRefusal(err: unknown): { field: boolean; message: string } {
  if (!(err instanceof RestError)) {
    return { field: false, message: err instanceof Error ? err.message : String(err) };
  }
  if (err.status === 400) {
    return { field: true, message: err.detail || err.code || "The engine refused this directory." };
  }
  if (err.unauthorized) {
    // THE ACCESS THE REFUSAL NAMED — Admin — or, for nobody signed in, that
    // somebody has to be.
    return { field: false, message: authorityWords("Taking a backup", err) };
  }
  if (err.status === 0) {
    return {
      field: false,
      message: "No answer in time. The copy may still finish; check the history.",
    };
  }
  // ANOTHER BACKUP IS RUNNING on this node: nothing failed and nothing was
  // written, so the copy-failed sentence below would send the reader to a log
  // with nothing wrong in it.
  if (err.code === "backup_running") {
    return {
      field: false,
      message:
        "A backup is already running on this node, and two at once would share its one trim hold. Nothing was written; take this one once it finishes. The history on this page records it.",
    };
  }
  // THE FLEET'S STATE RATHER THAN THIS NODE'S: the object store did not
  // answer, a log the backup replays holds a newer build's record or one signed
  // under a key this node does not hold, the copy is keyed to a log since
  // recreated, or a log is missing records the copy needs.
  // The engine says what to do in its hint; the copy-failed sentence below
  // would send the reader to a node with nothing wrong on it. The hint is a
  // CLAUSE — lower-case and unstopped — so it is made a sentence before the
  // one that follows it, or the two run together.
  const fleet = FLEET_REFUSALS.get(err.code);
  if (fleet !== undefined) {
    return {
      field: false,
      message: `${err.hint ? sentence(err.hint) : fleet} Whatever was written there has no manifest, so it is not a backup.`,
    };
  }
  return {
    field: false,
    message: "The copy failed. The engine log has the reason (api_backup_failed).",
  };
}
